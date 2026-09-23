package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

type fakeLiveControlClient struct {
	lastJSON  string
	keptAlive bool
	finalized bool
}

func TestQueryBoolInterimResultsDefaultsOff(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/live-transcription", nil)
	if queryBool(request, "interim_results", false) {
		t.Fatal("interim_results defaults to true, want false")
	}

	request = httptest.NewRequest("GET", "/api/live-transcription?interim_results=true", nil)
	if !queryBool(request, "interim_results", false) {
		t.Fatal("interim_results=true was not honored")
	}
}

func (c *fakeLiveControlClient) WriteJSON(payload interface{}) error {
	data, err := json.Marshal(payload)
	if err == nil {
		c.lastJSON = string(data)
	}
	return err
}

func (c *fakeLiveControlClient) KeepAlive() error {
	c.keptAlive = true
	return nil
}

func (c *fakeLiveControlClient) Finalize() error {
	c.finalized = true
	return nil
}

func TestForwardLiveControl(t *testing.T) {
	tests := []struct {
		name      string
		message   string
		lastJSON  string
		keptAlive bool
		finalized bool
		wantErr   bool
	}{
		{name: "forwards CloseStream", message: `{"type":"CloseStream"}`, lastJSON: `{"type":"CloseStream"}`},
		{name: "forwards KeepAlive", message: `{"type":"KeepAlive"}`, keptAlive: true},
		{name: "forwards Finalize", message: `{"type":"Finalize"}`, finalized: true},
		{name: "ignores unrelated JSON", message: `{"message":"CloseStream"}`},
		{name: "rejects malformed JSON", message: `{`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeLiveControlClient{}
			err := forwardLiveControl([]byte(tt.message), client)
			if (err != nil) != tt.wantErr {
				t.Fatalf("forwardLiveControl() error = %v, wantErr %t", err, tt.wantErr)
			}
			if client.lastJSON != tt.lastJSON {
				t.Errorf("WriteJSON() payload = %q, want %q", client.lastJSON, tt.lastJSON)
			}
			if client.keptAlive != tt.keptAlive {
				t.Errorf("KeepAlive() called = %t, want %t", client.keptAlive, tt.keptAlive)
			}
			if client.finalized != tt.finalized {
				t.Errorf("Finalize() called = %t, want %t", client.finalized, tt.finalized)
			}
		})
	}
}
