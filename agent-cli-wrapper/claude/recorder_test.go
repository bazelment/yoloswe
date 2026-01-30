package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecorder_SingleFile(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "recorder-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create recorder
	recorder := newSessionRecorder(tmpDir)

	// Record messages before initialization
	recorder.RecordSent(map[string]string{"type": "user", "content": "hello"})
	recorder.RecordReceived(map[string]string{"type": "system", "subtype": "init"})

	// Initialize
	err = recorder.Initialize(RecordingMetadata{
		SessionID:      "test-session",
		Model:          "claude-3",
		WorkDir:        "/tmp",
		Tools:          []string{"bash"},
		PermissionMode: "ask",
	})
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Record more messages after initialization
	recorder.RecordSent(map[string]string{"type": "user", "content": "follow up"})
	recorder.RecordReceived(map[string]string{"type": "assistant", "content": "response"})

	// Close recorder
	recorder.Close()

	// Verify single messages.jsonl file exists
	messagesPath := filepath.Join(recorder.Path(), "messages.jsonl")
	if _, err := os.Stat(messagesPath); os.IsNotExist(err) {
		t.Fatalf("messages.jsonl not created")
	}

	// Verify old files don't exist
	if _, err := os.Stat(filepath.Join(recorder.Path(), "to_cli.jsonl")); !os.IsNotExist(err) {
		t.Error("to_cli.jsonl should not exist")
	}
	if _, err := os.Stat(filepath.Join(recorder.Path(), "from_cli.jsonl")); !os.IsNotExist(err) {
		t.Error("from_cli.jsonl should not exist")
	}

	// Load messages and verify
	messages, err := LoadMessages(recorder.Path())
	if err != nil {
		t.Fatalf("LoadMessages failed: %v", err)
	}

	if len(messages) != 4 {
		t.Errorf("expected 4 messages, got %d", len(messages))
	}
}

func TestRecorder_ContainerFormat(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "recorder-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	recorder := newSessionRecorder(tmpDir)

	err = recorder.Initialize(RecordingMetadata{
		SessionID: "test-session",
	})
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	recorder.RecordSent(map[string]string{"type": "user"})
	recorder.RecordReceived(map[string]string{"type": "assistant"})
	recorder.Close()

	messages, err := LoadMessages(recorder.Path())
	if err != nil {
		t.Fatalf("LoadMessages failed: %v", err)
	}

	// Verify container format
	for i, msg := range messages {
		if msg.Timestamp.IsZero() {
			t.Errorf("message %d has zero timestamp", i)
		}
		if msg.Direction != "sent" && msg.Direction != "received" {
			t.Errorf("message %d has invalid direction: %s", i, msg.Direction)
		}
		if msg.Message == nil {
			t.Errorf("message %d has nil message", i)
		}
	}

	// Verify directions
	if messages[0].Direction != "sent" {
		t.Errorf("expected first message direction 'sent', got '%s'", messages[0].Direction)
	}
	if messages[1].Direction != "received" {
		t.Errorf("expected second message direction 'received', got '%s'", messages[1].Direction)
	}
}

func TestRecorder_BuffersReceivedMessages(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "recorder-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	recorder := newSessionRecorder(tmpDir)

	// Record received message BEFORE initialization
	recorder.RecordReceived(map[string]string{"type": "system", "data": "pre-init"})

	err = recorder.Initialize(RecordingMetadata{
		SessionID: "test-session",
	})
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	recorder.Close()

	messages, err := LoadMessages(recorder.Path())
	if err != nil {
		t.Fatalf("LoadMessages failed: %v", err)
	}

	// Should have the pre-init message
	if len(messages) != 1 {
		t.Errorf("expected 1 message (buffered received), got %d", len(messages))
	}

	if messages[0].Direction != "received" {
		t.Errorf("expected buffered message to be 'received', got '%s'", messages[0].Direction)
	}
}

func TestRecorder_TimestampOrder(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "recorder-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	recorder := newSessionRecorder(tmpDir)

	// Record messages before initialization with slight delays
	recorder.RecordSent(map[string]int{"seq": 1})
	time.Sleep(time.Millisecond)
	recorder.RecordReceived(map[string]int{"seq": 2})
	time.Sleep(time.Millisecond)
	recorder.RecordSent(map[string]int{"seq": 3})

	err = recorder.Initialize(RecordingMetadata{
		SessionID: "test-session",
	})
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	recorder.Close()

	messages, err := LoadMessages(recorder.Path())
	if err != nil {
		t.Fatalf("LoadMessages failed: %v", err)
	}

	// Verify messages are in timestamp order
	for i := 1; i < len(messages); i++ {
		if messages[i].Timestamp.Before(messages[i-1].Timestamp) {
			t.Errorf("messages not in order: %v before %v",
				messages[i].Timestamp, messages[i-1].Timestamp)
		}
	}
}

func TestRecordedMessage_MarshalJSON(t *testing.T) {
	msg := RecordedMessage{
		Timestamp: time.Date(2024, 1, 27, 10, 30, 45, 123000000, time.UTC),
		Direction: "sent",
		Message:   map[string]string{"type": "user"},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Verify by unmarshaling back and comparing values
	var unmarshaled RecordedMessage
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if !unmarshaled.Timestamp.Equal(msg.Timestamp) {
		t.Errorf("timestamp mismatch: got %v, want %v", unmarshaled.Timestamp, msg.Timestamp)
	}
	if unmarshaled.Direction != msg.Direction {
		t.Errorf("direction mismatch: got %v, want %v", unmarshaled.Direction, msg.Direction)
	}

	// Verify millisecond precision in timestamp string
	if !strings.Contains(string(data), "2024-01-27T10:30:45.123Z") {
		t.Errorf("timestamp format wrong in JSON: %s", string(data))
	}
}

func TestRecordedMessage_UnmarshalJSON(t *testing.T) {
	data := `{"timestamp":"2024-01-27T10:30:45.123Z","direction":"received","message":{"type":"assistant"}}`

	var msg RecordedMessage
	err := json.Unmarshal([]byte(data), &msg)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if msg.Direction != "received" {
		t.Errorf("expected direction 'received', got '%s'", msg.Direction)
	}

	expectedTime := time.Date(2024, 1, 27, 10, 30, 45, 123000000, time.UTC)
	if !msg.Timestamp.Equal(expectedTime) {
		t.Errorf("expected timestamp %v, got %v", expectedTime, msg.Timestamp)
	}
}

func TestLoadMessages_EmptyFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "recorder-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create empty messages.jsonl
	err = os.WriteFile(filepath.Join(tmpDir, "messages.jsonl"), []byte{}, 0644)
	if err != nil {
		t.Fatalf("failed to create empty file: %v", err)
	}

	messages, err := LoadMessages(tmpDir)
	if err != nil {
		t.Fatalf("LoadMessages failed: %v", err)
	}

	if len(messages) != 0 {
		t.Errorf("expected 0 messages, got %d", len(messages))
	}
}

func TestLoadMessages_MissingFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "recorder-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	_, err = LoadMessages(tmpDir)
	if err == nil {
		t.Error("expected error for missing file")
	}
}
