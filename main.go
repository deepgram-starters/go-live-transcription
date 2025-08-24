package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	api "github.com/deepgram/deepgram-go-sdk/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/pkg/client/listen"
	"github.com/joho/godotenv"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Allow all origins
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type WebSocketMessage struct {
	Type string `json:"type"`
}

// Implement the api.Callback interface
type MyCallback struct {
	socket *websocket.Conn
}

// Deepgram will call these methods when it receives a response: NewMyCallback, Message, Metadata, UtteranceEnd, Error
func NewMyCallback(conn *websocket.Conn) *MyCallback {
	return &MyCallback{
		socket: conn,
	}
}

func (c MyCallback) Open(ocr *api.OpenResponse) error {
	// handle the open
	fmt.Printf("\n[Open] Received\n")
	return nil
}

func (c MyCallback) SpeechStarted(ssr *api.SpeechStartedResponse) error {
	fmt.Printf("\n[SpeechStarted] Received\n")
	return nil
}

func (c *MyCallback) Message(mr *api.MessageResponse) error {
	sentence := strings.TrimSpace(mr.Channel.Alternatives[0].Transcript)
	if len(mr.Channel.Alternatives) == 0 || len(sentence) == 0 {
		return nil
	}
	fmt.Printf("\nDeepgram: %s\n\n", sentence)

	c.socket.WriteJSON(sentence)
	return nil
}

func (c MyCallback) Metadata(md *api.MetadataResponse) error {
	fmt.Printf("\n[Metadata] Received\n")
	fmt.Printf("Metadata.RequestID: %s\n", strings.TrimSpace(md.RequestID))
	fmt.Printf("Metadata.Channels: %d\n", md.Channels)
	fmt.Printf("Metadata.Created: %s\n\n", strings.TrimSpace(md.Created))
	return nil
}

func (c MyCallback) UtteranceEnd(ur *api.UtteranceEndResponse) error {
	fmt.Printf("\n[UtteranceEnd] Received\n")
	return nil
}

func (c MyCallback) Error(er *api.ErrorResponse) error {
	fmt.Printf("\n[Error] Received\n")
	fmt.Printf("Error.Type: %s\n", er.Type)
	fmt.Printf("Error.Message: %s\n", er.ErrMsg)
	fmt.Printf("Error.Description: %s\n\n", er.Description)
	return nil
}

func (c MyCallback) Close(ocr *api.CloseResponse) error {
	// handle the close
	fmt.Printf("\n[Close] Received\n")
	return nil
}

func (c MyCallback) UnhandledEvent(byData []byte) error {
	// handle the unhandled event
	fmt.Printf("\n[UnhandledEvent] Received\n")
	fmt.Printf("UnhandledEvent: %s\n\n", string(byData))
	return nil
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("WebSocket upgrade failed:", err)
		return
	}

	fmt.Println("WebSocket: connection established")

	// Configuration for the Deepgram client
	ctx := context.Background()
	apiKey := os.Getenv("DEEPGRAM_API_KEY")
	fmt.Println("Using API key:", apiKey)
	clientOptions := interfaces.ClientOptions{
		// EnableKeepAlive: true,
	}
	transcriptOptions := interfaces.LiveTranscriptionOptions{
		Language:    "en-US",
		Model:       "nova-3",
		SmartFormat: true,
	}

	// Callback used to handle responses from Deepgram
	callback := NewMyCallback(conn)

	// Create a new Deepgram LiveTranscription client with config options
	dgClient, err := client.NewWSUsingCallback(ctx, apiKey, &clientOptions, &transcriptOptions, callback)
	if err != nil {
		fmt.Println("ERROR creating LiveTranscription connection:", err)
		return
	}

	// Connect the websocket to Deepgram
	bConnected := dgClient.Connect()
	if !bConnected {
		fmt.Println("Client.Connect failed")
		os.Exit(1)
	}

	var clientMsg WebSocketMessage

	// Set up a loop to continuously read messages from the WebSocket
	for {
		messageType, p, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseGoingAway) {
				fmt.Println("Client closed connection (going away)")
				return
			}
			fmt.Println("Error reading WebSocket message:", err)
			return
		}
		if messageType == websocket.BinaryMessage {
			// Send the audio data to Deepgram
			n, err := dgClient.Write(p)
			if err != nil {
				fmt.Println("Error sending data to Deepgram:", err)
			} else {
				fmt.Println("WebSocket: data sent to Deepgram")
			}
			fmt.Printf("WebSocket: %d bytes from client \n", n)
		} else if messageType == websocket.TextMessage {
			err := json.Unmarshal(p, &clientMsg)
			if err != nil {
				fmt.Println("Error decoding JSON:", err)
				continue
			}
			fmt.Printf("WebSocket: %s\n", clientMsg.Type)

			if clientMsg.Type == "closeMicrophone" {
				// Close the connection to Deepgram
				dgClient.Stop()
				fmt.Println("WebSocket: closed connection to Deepgram")
				return
			}
		}

	}
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	serverAddress := "0.0.0.0:8080"

	client.InitWithDefault()
	http.Handle("/", http.StripPrefix("/", http.FileServer(http.Dir("./public"))))
	http.HandleFunc("/ws", handleWebSocket)
	fmt.Println("Server started on ", serverAddress)
	http.ListenAndServe(serverAddress, nil)
}
