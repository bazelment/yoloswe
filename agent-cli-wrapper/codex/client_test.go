package codex

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	client := NewClient()

	if client == nil {
		t.Fatal("NewClient should return a client")
	}
	if client.state == nil {
		t.Error("state manager should be initialized")
	}
	if client.threads == nil {
		t.Error("threads map should be initialized")
	}
	if client.pending == nil {
		t.Error("pending map should be initialized")
	}
	if client.events == nil {
		t.Error("events channel should be initialized")
	}
	if client.accumulator == nil {
		t.Error("accumulator should be initialized")
	}
}

func TestClient_State_Initial(t *testing.T) {
	client := NewClient()

	if client.State() != ClientStateUninitialized {
		t.Errorf("expected uninitialized state, got %v", client.State())
	}
}

func TestClient_Events(t *testing.T) {
	client := NewClient(WithEventBufferSize(10))

	events := client.Events()
	if events == nil {
		t.Error("Events() should return a channel")
	}

	// Verify it's the same channel
	if events != client.events {
		t.Error("Events() should return the client's event channel")
	}
}

func TestClient_Info_BeforeStart(t *testing.T) {
	client := NewClient()

	info := client.Info()
	if info != nil {
		t.Error("Info() should return nil before Start()")
	}
}

func TestClient_GetThread_NotFound(t *testing.T) {
	client := NewClient()

	thread, ok := client.GetThread("nonexistent")
	if ok {
		t.Error("GetThread should return false for nonexistent thread")
	}
	if thread != nil {
		t.Error("GetThread should return nil for nonexistent thread")
	}
}

func TestClient_ListThreads_Empty(t *testing.T) {
	client := NewClient()

	threads := client.ListThreads()
	if len(threads) != 0 {
		t.Errorf("expected 0 threads, got %d", len(threads))
	}
}

func TestClient_CloseThread_NotFound(t *testing.T) {
	client := NewClient()

	err := client.CloseThread("nonexistent")
	if err != ErrThreadNotFound {
		t.Errorf("expected ErrThreadNotFound, got %v", err)
	}
}

func TestClient_CreateThread_NotStarted(t *testing.T) {
	client := NewClient()
	ctx := context.Background()

	_, err := client.CreateThread(ctx)
	if err != ErrNotStarted {
		t.Errorf("expected ErrNotStarted, got %v", err)
	}
}

func TestClient_Ask_NotStarted(t *testing.T) {
	client := NewClient()
	ctx := context.Background()

	_, err := client.Ask(ctx, "test")
	if err != ErrNotStarted {
		t.Errorf("expected ErrNotStarted, got %v", err)
	}
}

func TestClient_Stop_NotStarted(t *testing.T) {
	client := NewClient()

	// Should not error when not started
	err := client.Stop()
	if err != nil {
		t.Errorf("Stop() should not error when not started: %v", err)
	}
}

func TestClient_emit(t *testing.T) {
	client := NewClient(WithEventBufferSize(10))

	event := ClientReadyEvent{UserAgent: "test"}
	client.emit(event)

	select {
	case received := <-client.events:
		if e, ok := received.(ClientReadyEvent); ok {
			if e.UserAgent != "test" {
				t.Errorf("unexpected UserAgent: %q", e.UserAgent)
			}
		} else {
			t.Errorf("unexpected event type: %T", received)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("event not received")
	}
}

func TestClient_emit_ChannelFull(t *testing.T) {
	client := NewClient(WithEventBufferSize(1))

	// Fill the channel
	client.emit(ClientReadyEvent{UserAgent: "first"})

	// This should not block (drops the event)
	done := make(chan bool)
	go func() {
		client.emit(ClientReadyEvent{UserAgent: "second"})
		done <- true
	}()

	select {
	case <-done:
		// Good - emit didn't block
	case <-time.After(100 * time.Millisecond):
		t.Error("emit blocked when channel was full")
	}
}

func TestClient_emitError(t *testing.T) {
	client := NewClient(WithEventBufferSize(10))

	testErr := &ProtocolError{Message: "test error"}
	client.emitError("thread-123", "turn-456", testErr, "test_context")

	select {
	case received := <-client.events:
		if e, ok := received.(ErrorEvent); ok {
			if e.ThreadID != "thread-123" {
				t.Errorf("unexpected ThreadID: %q", e.ThreadID)
			}
			if e.TurnID != "turn-456" {
				t.Errorf("unexpected TurnID: %q", e.TurnID)
			}
			if e.Context != "test_context" {
				t.Errorf("unexpected Context: %q", e.Context)
			}
		} else {
			t.Errorf("unexpected event type: %T", received)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("error event not received")
	}
}

func TestUnmarshalRaw(t *testing.T) {
	raw := json.RawMessage(`{"name": "test", "value": 42}`)

	var result struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	err := unmarshalRaw(raw, &result)
	if err != nil {
		t.Fatalf("unmarshalRaw failed: %v", err)
	}

	if result.Name != "test" {
		t.Errorf("unexpected name: %q", result.Name)
	}
	if result.Value != 42 {
		t.Errorf("unexpected value: %d", result.Value)
	}
}

func TestUnmarshalRaw_Invalid(t *testing.T) {
	raw := json.RawMessage(`invalid json`)

	var result struct{}

	err := unmarshalRaw(raw, &result)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// Test thread management
func TestClient_ThreadManagement(t *testing.T) {
	client := NewClient()

	// Manually add a thread (simulating what CreateThread does internally)
	thread := newThread(client, "thread-123", ThreadConfig{})
	client.threads["thread-123"] = thread

	// GetThread should find it
	found, ok := client.GetThread("thread-123")
	if !ok {
		t.Error("GetThread should find the thread")
	}
	if found != thread {
		t.Error("GetThread should return the correct thread")
	}

	// ListThreads should include it
	threads := client.ListThreads()
	if len(threads) != 1 {
		t.Errorf("expected 1 thread, got %d", len(threads))
	}

	// CloseThread should remove it
	err := client.CloseThread("thread-123")
	if err != nil {
		t.Errorf("CloseThread failed: %v", err)
	}

	// GetThread should not find it anymore
	_, ok = client.GetThread("thread-123")
	if ok {
		t.Error("GetThread should not find closed thread")
	}
}

// Test ID generator
func TestIdGenerator(t *testing.T) {
	gen := &idGenerator{}

	id1 := gen.Next()
	id2 := gen.Next()
	id3 := gen.Next()

	if id1 != 1 {
		t.Errorf("expected id 1, got %d", id1)
	}
	if id2 != 2 {
		t.Errorf("expected id 2, got %d", id2)
	}
	if id3 != 3 {
		t.Errorf("expected id 3, got %d", id3)
	}
}

// Test RPC result handling
func TestRpcResult(t *testing.T) {
	result := &rpcResult{
		Response: &JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      1,
			Result:  json.RawMessage(`{"test": "value"}`),
		},
	}

	if result.Response == nil {
		t.Error("Response should be set")
	}
	if result.Error != nil {
		t.Error("Error should be nil")
	}
}

func TestRpcResult_WithError(t *testing.T) {
	result := &rpcResult{
		Error: &RPCError{Code: -32600, Message: "Invalid Request"},
	}

	if result.Error == nil {
		t.Error("Error should be set")
	}
	if result.Response != nil {
		t.Error("Response should be nil when error is set")
	}
}

// Test ConnectionInfo
func TestConnectionInfo(t *testing.T) {
	info := &ConnectionInfo{
		UserAgent: "codex/1.0.0",
	}

	if info.UserAgent != "codex/1.0.0" {
		t.Errorf("unexpected UserAgent: %q", info.UserAgent)
	}
}

// Test handleExecCommandOutput base64 decoding
func TestHandleExecCommandOutput_Base64Decoding(t *testing.T) {
	tests := []struct {
		name          string
		chunk         string
		expectedChunk string
	}{
		{
			name:          "base64 encoded text",
			chunk:         "SGVsbG8sIFdvcmxkIQ==", // "Hello, World!"
			expectedChunk: "Hello, World!",
		},
		{
			name:          "base64 encoded short text",
			chunk:         "bHM=", // "ls"
			expectedChunk: "ls",
		},
		{
			name:          "base64 encoded JSON",
			chunk:         "eyJuYW1lIjogInRlc3QifQ==", // {"name": "test"}
			expectedChunk: `{"name": "test"}`,
		},
		{
			name:          "base64 encoded multiline",
			chunk:         "bGluZTEKbGluZTIKbGluZTM=", // "line1\nline2\nline3"
			expectedChunk: "line1\nline2\nline3",
		},
		{
			name:          "invalid base64 falls back to original",
			chunk:         "not-valid-base64!!!",
			expectedChunk: "not-valid-base64!!!",
		},
		{
			name:          "empty string",
			chunk:         "",
			expectedChunk: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(WithEventBufferSize(10))

			// Build the nested JSON structure that handleExecCommandOutput expects
			msg := ExecCommandOutputMsg{
				Type:   "exec_command_output_delta",
				CallID: "call123",
				Stream: "stdout",
				Chunk:  tt.chunk,
			}
			msgJSON, _ := json.Marshal(msg)

			notif := CodexEventNotification{
				ConversationID: "thread123",
				Msg:            msgJSON,
			}
			notifJSON, _ := json.Marshal(notif)

			// Call the handler
			client.handleExecCommandOutput(notifJSON)

			// Check the emitted event
			select {
			case event := <-client.events:
				if e, ok := event.(CommandOutputEvent); ok {
					if e.Chunk != tt.expectedChunk {
						t.Errorf("Chunk = %q, want %q", e.Chunk, tt.expectedChunk)
					}
					if e.CallID != "call123" {
						t.Errorf("CallID = %q, want %q", e.CallID, "call123")
					}
					if e.ThreadID != "thread123" {
						t.Errorf("ThreadID = %q, want %q", e.ThreadID, "thread123")
					}
				} else {
					t.Errorf("unexpected event type: %T", event)
				}
			case <-time.After(100 * time.Millisecond):
				t.Error("event not received")
			}
		})
	}
}
