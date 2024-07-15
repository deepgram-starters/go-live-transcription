package tests

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/pkg/api/live/v1/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/pkg/client/live"
)

type MyCallback struct {
	sb                 *strings.Builder
	transcriptReceived chan struct{}
	t                  *testing.T
}

func (c MyCallback) Message(mr *api.MessageResponse) error {
	// handle the message
	if len(mr.Channel.Alternatives) == 0 {
		c.t.Error("No alternatives received")
		return nil
	}

	sentence := strings.TrimSpace(mr.Channel.Alternatives[0].Transcript)
	if len(sentence) == 0 {
		c.t.Error("No transcript received")
		return nil
	}

	if mr.IsFinal {
		c.sb.WriteString(sentence)
		c.sb.WriteString(" ")

		if mr.SpeechFinal {
			fmt.Printf("[------- Is Final]: %s\n", c.sb.String())
			c.sb.Reset()
			c.transcriptReceived <- struct{}{} // Signal that a transcript is received
		}
	} else {
		fmt.Printf("[Interm Result]: %s\n", sentence)
	}

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

func (c MyCallback) Error(er *api.ErrorResponse) error {
	// handle the error
	fmt.Printf("\n[Error] Received\n")
	fmt.Printf("Error.Type: %s\n", er.Type)
	fmt.Printf("Error.ErrCode: %s\n", er.Message)
	fmt.Printf("Error.Description: %s\n\n", er.Description)
	return nil
}

func (c MyCallback) UtteranceEnd(ur *api.UtteranceEndResponse) error {
	utterance := strings.TrimSpace(c.sb.String())
	if len(utterance) > 0 {
		fmt.Printf("[------- UtteranceEnd]: %s\n", utterance)
		c.sb.Reset()
	} else {
		fmt.Printf("\n[UtteranceEnd] Received\n")
	}

	return nil
}

const (
	STREAM_URL = "http://stream.live.vc.bbcmedia.co.uk/bbc_world_service"
)

func TestDeepgramLiveTranscription(t *testing.T) {
	// Init library
	client.InitWithDefault()

	// Go context
	ctx := context.Background()

	// Set the Transcription options
	transcriptOptions := interfaces.LiveTranscriptionOptions{
		Language:  "en-US",
		Punctuate: true,
	}

	transcriptReceived := make(chan struct{})

	callback := MyCallback{
		sb:                 &strings.Builder{},
		transcriptReceived: transcriptReceived,
	}

	// Create a Deepgram client
	dgClient, err := client.NewWithDefaults(ctx, transcriptOptions, callback)
	if err != nil {
		t.Fatalf("ERROR creating LiveTranscription connection: %v", err)
	}

	// Get the HTTP stream
	httpClient := new(http.Client)
	res, err := httpClient.Get(STREAM_URL)
	if err != nil {
		t.Fatalf("httpClient.Get failed. Err: %v", err)
	}
	defer res.Body.Close()

	// Connect the websocket to Deepgram
	bConnected := dgClient.Connect()
	if bConnected == nil {
		t.Fatal("Client.Connect failed")
	}

	go func() {
		dgClient.Stream(bufio.NewReader(res.Body))
	}()

	// Wait for a transcript or timeout after 30 seconds
	select {
	case <-transcriptReceived:
		fmt.Println("Transcript received")
	case <-time.After(30 * time.Second):
		t.Fatal("Timeout waiting for transcript")
	}

	// Close client
	dgClient.Stop()

	fmt.Printf("Test finished\n")
}
