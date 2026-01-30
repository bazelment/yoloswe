package codex

import (
	"encoding/json"
	"time"
)

// SessionFormat identifies the session log format.
const SessionFormatCodex = "codex"

// SessionLogHeader is written as the first line of a session log.
type SessionLogHeader struct {
	Format    string `json:"format"`
	Version   string `json:"version"`
	Client    string `json:"client"`
	Timestamp string `json:"timestamp"`
}

// SessionLogEntry represents a single session log entry.
type SessionLogEntry struct {
	Timestamp string          `json:"timestamp"`
	Direction string          `json:"direction"` // "sent" or "received"
	Message   json.RawMessage `json:"message"`
}

// NewSessionLogHeader creates a header for a new session log.
func NewSessionLogHeader(clientName string) SessionLogHeader {
	return SessionLogHeader{
		Format:    SessionFormatCodex,
		Version:   "1.0",
		Client:    clientName,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// NewSessionLogEntry creates a new session log entry.
func NewSessionLogEntry(direction string, data []byte) SessionLogEntry {
	return SessionLogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Direction: direction,
		Message:   json.RawMessage(data),
	}
}
