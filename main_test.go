package main

import "testing"

type fakeLiveControlClient struct {
	finalized bool
	keptAlive bool
}

func (c *fakeLiveControlClient) Finalize() error {
	c.finalized = true
	return nil
}

func (c *fakeLiveControlClient) KeepAlive() error {
	c.keptAlive = true
	return nil
}

func TestForwardLiveControl(t *testing.T) {
	tests := []struct {
		name      string
		message   string
		finalized bool
		keptAlive bool
		wantErr   bool
	}{
		{name: "finalizes CloseStream", message: `{"type":"CloseStream"}`, finalized: true},
		{name: "forwards KeepAlive", message: `{"type":"KeepAlive"}`, keptAlive: true},
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
			if client.finalized != tt.finalized {
				t.Errorf("Finalize() called = %t, want %t", client.finalized, tt.finalized)
			}
			if client.keptAlive != tt.keptAlive {
				t.Errorf("KeepAlive() called = %t, want %t", client.keptAlive, tt.keptAlive)
			}
		})
	}
}
