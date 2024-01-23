package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	api "github.com/deepgram/deepgram-go-sdk/pkg/api/live/v1/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/pkg/client/live"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type WebSocketMessage struct {
	Type string `json:"type"`
}

// Implement your own callback
type MyCallback struct {
	Response *api.MessageResponse
	SendChan chan *api.MessageResponse // New channel for communication
}

// Initialize the channel in the MyCallback struct
func NewMyCallback() *MyCallback {
	return &MyCallback{
		SendChan: make(chan *api.MessageResponse),
	}
}

func (c *MyCallback) Message(mr *api.MessageResponse) error {
	sentence := strings.TrimSpace(mr.Channel.Alternatives[0].Transcript)

	if len(mr.Channel.Alternatives) == 0 || len(sentence) == 0 {
		return nil
	}
	fmt.Printf("\n%s\n", sentence)

	c.Response = mr

	// Send the response through the channel
	c.SendChan <- mr

	return nil
}

func (c MyCallback) Metadata(md *api.MetadataResponse) error {
	// handle the metadata
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
	// handle the error
	fmt.Printf("\n[Error] Received\n")
	fmt.Printf("Error.Type: %s\n", er.Type)
	fmt.Printf("Error.Message: %s\n", er.Message)
	fmt.Printf("Error.Description: %s\n\n", er.Description)
	return nil
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("WebSocket upgrade failed:", err)
		return
	}

	ctx := context.Background()
	// set the Transcription options
	transcriptOptions := interfaces.LiveTranscriptionOptions{
		Language:    "en-US",
		Model:       "nova-2",
		SmartFormat: true,
	}

	clientOptions := interfaces.ClientOptions{
		EnableKeepAlive: true,
	}

	apiKey := os.Getenv("DEEPGRAM_API_KEY")
	// implement your own callback
	callback := NewMyCallback()
	// create a Deepgram client
	dgClient, err := client.New(ctx, apiKey, &clientOptions, transcriptOptions, callback)
	if err != nil {
		fmt.Println("ERROR creating LiveTranscription connection:", err)
		return
	}

	var microphoneOpen bool

	// connect the websocket to Deepgram
	wsconn := dgClient.Connect()
	if wsconn == nil {
		fmt.Println("Client.Connect failed")
		os.Exit(1)
	}

	if err != nil {
		// Handle error
		return
	}
	// defer conn.Close()
	var clientMsg WebSocketMessage
	// Set up a loop to continuously read messages from the WebSocket
	for {
		select {
		case response := <-callback.SendChan:
			// Send the sentence to the client
			err := conn.WriteJSON(response)
			if err != nil {
				fmt.Println("Error sending message:", err)
				return
			}
		default:
			// Handle other WebSocket messages or events
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
				microphoneOpen = true
				// Send the received message to Deepgram
				n, err := dgClient.Write(p)
				if err != nil {
					fmt.Println("Error sending data to Deepgram:", err)
					// Handle the error as needed
				}
				fmt.Printf("WebSocket: sent %d bytes to Deepgram\n", n)
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
					if err != nil {
						fmt.Println("Error closing connection to Deepgram:", err)
						// Handle the error as needed
					}
					fmt.Println("WebSocket: closed connection to Deepgram")
					microphoneOpen = false
					return
				}
			}

			// else if clientMsg.Type == "openMicrophone" {
			// 	// Reopen the connection to Deepgram
			// 	wsconn = dgClient.AttemptReconnect(5)
			// 	if wsconn == nil {
			// 		fmt.Println("Client.Connect failed")
			// 		os.Exit(1)
			// 	}
			// 	fmt.Println("WebSocket: reopened connection to Deepgram")
			// 	// Update the microphone state
			// 	microphoneOpen = true
			// }
			_ = microphoneOpen
		}
	}
}

func main() {
	// init library to set up logging
	client.InitWithDefault()
	// Serve static files (HTML, CSS, JS, etc.)
	http.Handle("/", http.StripPrefix("/", http.FileServer(http.Dir("./public"))))

	// Handle WebSocket connections
	http.HandleFunc("/ws", handleWebSocket)

	// Start the server
	http.ListenAndServe(":8080", nil)
}
