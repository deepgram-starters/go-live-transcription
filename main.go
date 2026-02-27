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
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

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

// buildDeepgramURL constructs the Deepgram WebSocket URL with query parameters
// forwarded from the client request.
func buildDeepgramURL(baseURL string, r *http.Request) string {
	dgURL, _ := url.Parse(baseURL)
	q := dgURL.Query()

	// Map of parameter names to defaults
	params := map[string]string{
		"model":        "nova-3",
		"language":     "en",
		"smart_format": "true",
		"punctuate":    "true",
		"diarize":      "false",
		"filler_words": "false",
		"encoding":     "linear16",
		"sample_rate":  "16000",
		"channels":     "1",
	}

	for name, defaultVal := range params {
		val := r.URL.Query().Get(name)
		if val == "" {
			val = defaultVal
		}
		q.Set(name, val)
	}

	dgURL.RawQuery = q.Encode()
	return dgURL.String()
}

// forwardMessages reads messages from src and writes them to dst.
// It signals completion on the done channel and logs the direction label.
func forwardMessages(src, dst *websocket.Conn, label string, done chan<- struct{}, counter *int64) {
	defer func() { done <- struct{}{} }()

	for {
		msgType, data, err := src.ReadMessage()
		if err != nil {
			// Connection closed or errored -- signal and exit
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[%s] read error: %v", label, err)
			}
			return
		}

		*counter++
		count := *counter

		// Log periodically for binary, always for text
		if msgType == websocket.TextMessage || count%10 == 0 {
			log.Printf("[%s] message #%d (binary: %v, size: %d)", label, count, msgType == websocket.BinaryMessage, len(data))
		}

		if err := dst.WriteMessage(msgType, data); err != nil {
			log.Printf("[%s] write error: %v", label, err)
			return
		}
	}
}

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

		// Build Deepgram URL with forwarded query parameters
		deepgramURL := buildDeepgramURL(cfg.DeepgramSttURL, r)

		model := r.URL.Query().Get("model")
		if model == "" {
			model = "nova-3"
		}
		language := r.URL.Query().Get("language")
		if language == "" {
			language = "en"
		}
		encoding := r.URL.Query().Get("encoding")
		if encoding == "" {
			encoding = "linear16"
		}
		sampleRate := r.URL.Query().Get("sample_rate")
		if sampleRate == "" {
			sampleRate = "16000"
		}
		channels := r.URL.Query().Get("channels")
		if channels == "" {
			channels = "1"
		}

		log.Printf("Connecting to Deepgram STT: model=%s, language=%s, encoding=%s, sample_rate=%s, channels=%s",
			model, language, encoding, sampleRate, channels)

		// Connect to Deepgram with API key auth
		dialer := websocket.DefaultDialer
		header := http.Header{
			"Authorization": []string{"Token " + cfg.DeepgramAPIKey},
		}

		deepgramConn, _, err := dialer.Dial(deepgramURL, header)
		if err != nil {
			log.Printf("Failed to connect to Deepgram: %v", err)
			clientConn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "Failed to connect to Deepgram"))
			return
		}
		defer deepgramConn.Close()

		log.Println("Connected to Deepgram STT API")

		// Bidirectional message forwarding using goroutines
		done := make(chan struct{}, 2)
		var dgToClientCount, clientToDgCount int64

		go forwardMessages(deepgramConn, clientConn, "deepgram->client", done, &dgToClientCount)
		go forwardMessages(clientConn, deepgramConn, "client->deepgram", done, &clientToDgCount)

		// Wait for either direction to finish (indicates one side closed)
		<-done

		// Clean up: close both connections
		log.Println("Proxy session ending, closing connections")
		clientConn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		deepgramConn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Client disconnected"))
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
