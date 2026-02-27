package protocol

import "encoding/json"

// TraceEntry represents a single entry in a trace file.
// Trace files wrap protocol messages with metadata for debugging and fixtures.
type TraceEntry struct {
	ID         string          `json:"id"`
	Timestamp  string          `json:"timestamp"`
	Direction  string          `json:"direction"` // "sent" or "received"
	Message    json.RawMessage `json:"message"`
	TurnNumber int             `json:"turnNumber,omitempty"`
}

// ParseTraceEntry parses a trace entry and extracts the inner protocol message.
// Falls back to parsing as a raw protocol message if the entry format doesn't match.
func ParseTraceEntry(line []byte) (Message, error) {
	var entry TraceEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		// Try parsing as raw message (in case it's not wrapped)
		return ParseMessage(line)
	}
	return ParseMessage(entry.Message)
}
