package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProcessManager_SessionLogging(t *testing.T) {
	// Create temp directory for session log
	tmpDir, err := os.MkdirTemp("", "codex-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logPath := filepath.Join(tmpDir, "session.jsonl")

	// Create process manager with session logging
	pm := newProcessManager(ClientConfig{
		SessionLogPath: logPath,
		ClientName:     "test-client",
	})

	// Simulate session log file creation (without starting actual process)
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("failed to create session log: %v", err)
	}
	pm.sessionLog = f

	// Write the header first (as Start() would do)
	header := NewSessionLogHeader(pm.config.ClientName)
	enc := json.NewEncoder(pm.sessionLog)
	enc.Encode(header)

	// Test logging sent message
	testMsg := map[string]string{"type": "test", "data": "hello"}
	msgData, _ := json.Marshal(testMsg)
	pm.logMessageLocked("sent", msgData)

	// Test logging received message
	recvMsg := map[string]string{"type": "response", "data": "world"}
	recvData, _ := json.Marshal(recvMsg)
	pm.logMessageLocked("received", recvData)

	// Close the file
	f.Close()
	pm.sessionLog = nil

	// Verify the log file contents
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}

	// Parse each line
	lines := splitLines(data)
	if len(lines) != 3 {
		t.Errorf("expected 3 log entries (header + 2 messages), got %d", len(lines))
	}

	// Verify header
	var hdr SessionLogHeader
	if err := json.Unmarshal(lines[0], &hdr); err != nil {
		t.Fatalf("failed to parse header: %v", err)
	}
	if hdr.Format != SessionFormatCodex {
		t.Errorf("expected format '%s', got '%s'", SessionFormatCodex, hdr.Format)
	}
	if hdr.Version != "1.0" {
		t.Errorf("expected version '1.0', got '%s'", hdr.Version)
	}
	if hdr.Client != "test-client" {
		t.Errorf("expected client 'test-client', got '%s'", hdr.Client)
	}
	if hdr.Timestamp == "" {
		t.Error("timestamp should not be empty")
	}

	// Verify first message (sent)
	var msg1 SessionLogEntry
	if err := json.Unmarshal(lines[1], &msg1); err != nil {
		t.Fatalf("failed to parse first message: %v", err)
	}
	if msg1.Direction != "sent" {
		t.Errorf("expected direction 'sent', got '%s'", msg1.Direction)
	}
	if msg1.Timestamp == "" {
		t.Error("timestamp should not be empty")
	}

	// Verify second message (received)
	var msg2 SessionLogEntry
	if err := json.Unmarshal(lines[2], &msg2); err != nil {
		t.Fatalf("failed to parse second message: %v", err)
	}
	if msg2.Direction != "received" {
		t.Errorf("expected direction 'received', got '%s'", msg2.Direction)
	}
}

func TestSessionLogHeader(t *testing.T) {
	header := NewSessionLogHeader("my-client")

	if header.Format != SessionFormatCodex {
		t.Errorf("expected format '%s', got '%s'", SessionFormatCodex, header.Format)
	}
	if header.Version != "1.0" {
		t.Errorf("expected version '1.0', got '%s'", header.Version)
	}
	if header.Client != "my-client" {
		t.Errorf("expected client 'my-client', got '%s'", header.Client)
	}
	if header.Timestamp == "" {
		t.Error("timestamp should not be empty")
	}
}

func TestSessionLogEntry(t *testing.T) {
	data := []byte(`{"key": "value"}`)
	entry := NewSessionLogEntry("sent", data)

	if entry.Direction != "sent" {
		t.Errorf("expected direction 'sent', got '%s'", entry.Direction)
	}
	if entry.Timestamp == "" {
		t.Error("timestamp should not be empty")
	}
	if string(entry.Message) != string(data) {
		t.Errorf("expected message '%s', got '%s'", string(data), string(entry.Message))
	}
}

func TestProcessManager_NoSessionLog(t *testing.T) {
	// Create process manager without session logging
	pm := newProcessManager(ClientConfig{})

	// This should not panic
	pm.logMessageLocked("sent", []byte(`{"test": "data"}`))
}

// splitLines splits data by newlines, ignoring empty lines
func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}
