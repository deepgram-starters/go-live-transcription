/**
 * Go Live Transcription Starter - Backend Server
 *
 * Simple WebSocket proxy to Deepgram's Live Transcription API.
 * Forwards all messages (JSON and binary) bidirectionally between client and Deepgram.
 *
 * Routes:
 *   GET  /api/session              - Issue JWT session token
 *   GET  /api/metadata             - Project metadata from deepgram.toml
 *   WS   /api/live-transcription   - WebSocket proxy to Deepgram STT (auth required)
 *   GET  /health                   - Health check
 */

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	msginterfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	dginterfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	listen "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/listen"

	"github.com/BurntSushi/toml"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

// ============================================================================
// CONFIGURATION
// ============================================================================

type config struct {
	DeepgramAPIKey string
	DeepgramSttURL string
	Port           string
	Host           string
	SessionSecret  []byte
}

type liveControlClient interface {
	Finalize() error
	KeepAlive() error
}

type liveControlMessage struct {
	Type string `json:"type"`
}

func forwardLiveControl(data []byte, client liveControlClient) error {
	var control liveControlMessage
	if err := json.Unmarshal(data, &control); err != nil {
		return fmt.Errorf("invalid control message: %w", err)
	}

	switch control.Type {
	case "CloseStream":
		return client.Finalize()
	case "KeepAlive":
		return client.KeepAlive()
	default:
		return nil
	}
}

func loadConfig() config {
	_ = godotenv.Load()

	apiKey := os.Getenv("DEEPGRAM_API_KEY")
	if apiKey == "" {
		log.Fatal("ERROR: DEEPGRAM_API_KEY environment variable is required\n" +
			"Please copy sample.env to .env and add your API key")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	host := os.Getenv("HOST")
	if host == "" {
		host = "0.0.0.0"
	}

	secret := os.Getenv("SESSION_SECRET")
	var secretBytes []byte
	if secret != "" {
		secretBytes = []byte(secret)
	} else {
		secretBytes = make([]byte, 32)
		if _, err := rand.Read(secretBytes); err != nil {
			log.Fatal("Failed to generate session secret:", err)
		}
	}

	return config{
		DeepgramAPIKey: apiKey,
		DeepgramSttURL: "wss://api.deepgram.com/v1/listen",
		Port:           port,
		Host:           host,
		SessionSecret:  secretBytes,
	}
}

// ============================================================================
// SESSION AUTH - JWT tokens for production security
// ============================================================================

const jwtExpiry = time.Hour

// issueToken creates a signed JWT with a 1-hour expiry.
func issueToken(secret []byte) (string, error) {
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(jwtExpiry)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(secret)
}

// validateToken verifies a JWT token string and returns an error if invalid.
func validateToken(tokenStr string, secret []byte) error {
	_, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return secret, nil
	})
	return err
}

// validateWsToken extracts and validates a JWT from the access_token.<jwt> subprotocol.
// Returns the full subprotocol string if valid, empty string if invalid.
func validateWsToken(protocols []string, secret []byte) string {
	for _, proto := range protocols {
		if strings.HasPrefix(proto, "access_token.") {
			tokenStr := strings.TrimPrefix(proto, "access_token.")
			if err := validateToken(tokenStr, secret); err == nil {
				return proto
			}
		}
	}
	return ""
}

// ============================================================================
// METADATA - deepgram.toml parsing
// ============================================================================

type tomlConfig struct {
	Meta map[string]interface{} `toml:"meta"`
}

// loadMetadata reads and parses the [meta] section from deepgram.toml.
func loadMetadata() (map[string]interface{}, error) {
	var cfg tomlConfig
	if _, err := toml.DecodeFile("deepgram.toml", &cfg); err != nil {
		return nil, fmt.Errorf("failed to read deepgram.toml: %w", err)
	}
	if cfg.Meta == nil {
		return nil, fmt.Errorf("missing [meta] section in deepgram.toml")
	}
	return cfg.Meta, nil
}

// ============================================================================
// CORS MIDDLEWARE
// ============================================================================

// corsMiddleware adds CORS headers to HTTP responses.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ============================================================================
// WEBSOCKET PROXY
// ============================================================================

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// activeConnections tracks all open client WebSocket connections for graceful shutdown.
var activeConnections sync.Map

// queryOr returns the query value for key, or def when empty.
func queryOr(r *http.Request, key, def string) string {
	if v := r.URL.Query().Get(key); v != "" {
		return v
	}
	return def
}

// queryBool parses a boolean query value, falling back to def when empty.
func queryBool(r *http.Request, key string, def bool) bool {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	return v == "true" || v == "1"
}

// liveTranscriptionCallback implements the Deepgram SDK LiveMessageCallback
// interface and relays Deepgram events to the browser WebSocket as JSON text
// frames, preserving the wire format the frontend already expects.
type liveTranscriptionCallback struct {
	conn *websocket.Conn
	mu   *sync.Mutex
}

// send marshals a Deepgram response and writes it to the browser connection.
func (c *liveTranscriptionCallback) send(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("Failed to marshal Deepgram event: %v", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("Failed to forward Deepgram event to client: %v", err)
	}
}

func (c *liveTranscriptionCallback) Open(or *msginterfaces.OpenResponse) error { return nil }
func (c *liveTranscriptionCallback) Message(mr *msginterfaces.MessageResponse) error {
	c.send(mr)
	return nil
}
func (c *liveTranscriptionCallback) Metadata(md *msginterfaces.MetadataResponse) error {
	c.send(md)
	return nil
}
func (c *liveTranscriptionCallback) SpeechStarted(ssr *msginterfaces.SpeechStartedResponse) error {
	c.send(ssr)
	return nil
}
func (c *liveTranscriptionCallback) UtteranceEnd(ur *msginterfaces.UtteranceEndResponse) error {
	c.send(ur)
	return nil
}
func (c *liveTranscriptionCallback) Close(cr *msginterfaces.CloseResponse) error { return nil }
func (c *liveTranscriptionCallback) Error(er *msginterfaces.ErrorResponse) error {
	c.send(er)
	return nil
}
func (c *liveTranscriptionCallback) UnhandledEvent(byData []byte) error { return nil }

// handleLiveTranscription is the WebSocket handler for /api/live-transcription.
// It authenticates via JWT subprotocol, then creates a bidirectional proxy to Deepgram.
func handleLiveTranscription(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Println("WebSocket upgrade request for: /api/live-transcription")

		// Validate JWT from access_token.<jwt> subprotocol
		protocols := websocket.Subprotocols(r)
		validProto := validateWsToken(protocols, cfg.SessionSecret)
		if validProto == "" {
			log.Println("WebSocket auth failed: invalid or missing token")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		log.Println("Backend handling /api/live-transcription WebSocket (authenticated)")

		// Upgrade client connection, echoing back the validated subprotocol
		responseHeader := http.Header{}
		responseHeader.Set("Sec-WebSocket-Protocol", validProto)
		clientConn, err := upgrader.Upgrade(w, r, responseHeader)
		if err != nil {
			log.Printf("WebSocket upgrade failed: %v", err)
			return
		}

		// Track the connection for graceful shutdown
		activeConnections.Store(clientConn, true)
		defer func() {
			activeConnections.Delete(clientConn)
			clientConn.Close()
		}()

		log.Println("Client connected to /api/live-transcription")

		// Build Deepgram live transcription options from forwarded query params.
		sampleRate, _ := strconv.Atoi(queryOr(r, "sample_rate", "16000"))
		channels, _ := strconv.Atoi(queryOr(r, "channels", "1"))
		tOptions := &dginterfaces.LiveTranscriptionOptions{
			Model:          queryOr(r, "model", "nova-3"),
			Language:       queryOr(r, "language", "en"),
			Encoding:       queryOr(r, "encoding", "linear16"),
			SampleRate:     sampleRate,
			Channels:       channels,
			SmartFormat:    queryBool(r, "smart_format", true),
			Punctuate:      queryBool(r, "punctuate", true),
			Diarize:        queryBool(r, "diarize", false),
			FillerWords:    queryBool(r, "filler_words", false),
			InterimResults: true,
		}

		log.Printf("Connecting to Deepgram STT: model=%s, language=%s, encoding=%s, sample_rate=%d, channels=%d",
			tOptions.Model, tOptions.Language, tOptions.Encoding, tOptions.SampleRate, tOptions.Channels)

		// Serialize all writes to the browser connection (callback + close frames).
		writeMu := &sync.Mutex{}
		closeToClient := func(code int, msg string) {
			writeMu.Lock()
			defer writeMu.Unlock()
			clientConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, msg))
		}

		// Connect to Deepgram using the official Go SDK (listen v1 WebSocket).
		cOptions := &dginterfaces.ClientOptions{EnableKeepAlive: true}
		callback := &liveTranscriptionCallback{conn: clientConn, mu: writeMu}

		dgClient, err := listen.NewWSUsingCallback(context.Background(), cfg.DeepgramAPIKey, cOptions, tOptions, callback)
		if err != nil {
			log.Printf("Failed to create Deepgram client: %v", err)
			closeToClient(websocket.CloseInternalServerErr, "Failed to connect to Deepgram")
			return
		}

		if !dgClient.Connect() {
			log.Printf("Failed to connect to Deepgram")
			closeToClient(websocket.CloseInternalServerErr, "Failed to connect to Deepgram")
			return
		}
		defer dgClient.Stop()

		log.Println("Connected to Deepgram STT API")

		// Pump audio (binary) and control (text) messages from the browser to Deepgram.
		for {
			msgType, data, err := clientConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					log.Printf("client read error: %v", err)
				}
				break
			}

			switch msgType {
			case websocket.BinaryMessage:
				if _, werr := dgClient.Write(data); werr != nil {
					log.Printf("Failed to write audio to Deepgram: %v", werr)
					closeToClient(websocket.CloseInternalServerErr, "Deepgram write failed")
					return
				}
			case websocket.TextMessage:
				if err := forwardLiveControl(data, dgClient); err != nil {
					log.Printf("Failed to forward control message to Deepgram: %v", err)
				}
			}
		}

		log.Println("Proxy session ending, closing connections")
		closeToClient(websocket.CloseNormalClosure, "")
	}
}

// ============================================================================
// HTTP HANDLERS
// ============================================================================

// handleSession issues a JWT session token.
func handleSession(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, err := issueToken(cfg.SessionSecret)
		if err != nil {
			log.Printf("Failed to issue token: %v", err)
			http.Error(w, `{"error":"INTERNAL_SERVER_ERROR","message":"Failed to issue session token"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}
}

// handleMetadata returns the [meta] section from deepgram.toml.
func handleMetadata() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		meta, err := loadMetadata()
		if err != nil {
			log.Printf("Error reading metadata: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "INTERNAL_SERVER_ERROR",
				"message": fmt.Sprintf("Failed to read metadata from deepgram.toml: %v", err),
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(meta)
	}
}

// handleHealth returns a simple health check response.
func handleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// ============================================================================
// GRACEFUL SHUTDOWN
// ============================================================================

// gracefulShutdown closes all active WebSocket connections and shuts down the HTTP server.
func gracefulShutdown(server *http.Server) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	sig := <-quit
	log.Printf("\n%s signal received: starting graceful shutdown...", sig)

	// Close all active WebSocket connections
	connCount := 0
	activeConnections.Range(func(key, value interface{}) bool {
		connCount++
		return true
	})
	log.Printf("Closing %d active WebSocket connection(s)...", connCount)

	activeConnections.Range(func(key, value interface{}) bool {
		conn := key.(*websocket.Conn)
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "Server shutting down"))
		conn.Close()
		return true
	})

	// Shut down HTTP server with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	log.Println("Shutdown complete")
}

// ============================================================================
// MAIN
// ============================================================================

func main() {
	cfg := loadConfig()

	// Initialize the Deepgram Go SDK.
	listen.InitWithDefault()

	mux := http.NewServeMux()

	// HTTP endpoints
	mux.Handle("/api/session", handleSession(cfg))
	mux.Handle("/api/metadata", handleMetadata())
	mux.Handle("/health", handleHealth())

	// WebSocket endpoint
	mux.Handle("/api/live-transcription", handleLiveTranscription(cfg))

	// Wrap all routes in CORS middleware
	handler := corsMiddleware(mux)

	addr := cfg.Host + ":" + cfg.Port
	server := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	// Start graceful shutdown listener in background
	go gracefulShutdown(server)

	// Generate a random hex string for log display (session secret indicator)
	secretHex := hex.EncodeToString(cfg.SessionSecret[:8])

	log.Println("")
	log.Println(strings.Repeat("=", 70))
	log.Printf("Backend API Server running at http://localhost:%s", cfg.Port)
	log.Println("")
	log.Printf("  GET  /api/session")
	log.Printf("  WS   /api/live-transcription (auth required)")
	log.Printf("  GET  /api/metadata")
	log.Printf("  GET  /health")
	log.Println("")
	log.Printf("Session secret: %s... (first 8 bytes)", secretHex)
	log.Println(strings.Repeat("=", 70))
	log.Println("")

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server failed: %v", err)
	}
}
