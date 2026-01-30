package protocol

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TraceEntry represents a single entry in a trace file.
// The trace files wrap protocol messages with metadata.
type TraceEntry struct {
	ID         string          `json:"id"`
	Timestamp  string          `json:"timestamp"`
	Direction  string          `json:"direction"`
	Message    json.RawMessage `json:"message"`
	TurnNumber int             `json:"turnNumber"`
}

// parseTraceEntry parses a trace entry and extracts the inner message.
func parseTraceEntry(line []byte) (Message, error) {
	var entry TraceEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		// Try parsing as raw message (in case it's not wrapped)
		return ParseMessage(line)
	}

	// Parse the inner message
	return ParseMessage(entry.Message)
}

// TestParseTraceFromCLI parses all messages from a real CLI trace file.
// This validates that our protocol types can handle real-world data.
func TestParseTraceFromCLI(t *testing.T) {
	tracePath := filepath.Join("..", "testdata", "traces", "from_cli.jsonl")

	file, err := os.Open(tracePath)
	if err != nil {
		t.Skipf("Trace file not found (run from sdk root or ensure testdata exists): %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Increase buffer size for large messages
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	lineNum := 0
	var (
		systemMsgs    int
		assistantMsgs int
		userMsgs      int
		resultMsgs    int
		streamEvents  int
		controlReqs   int
		controlResps  int
		unknownTypes  int
		parseErrors   int
	)

	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()

		msg, err := parseTraceEntry(line)
		if err != nil {
			parseErrors++
			t.Logf("Line %d: parse error: %v", lineNum, err)
			t.Logf("Line content (first 200 chars): %s", truncateString(string(line), 200))
			continue
		}

		switch m := msg.(type) {
		case SystemMessage:
			systemMsgs++
			if m.Subtype == "init" {
				t.Logf("Session init: model=%s, session_id=%s", m.Model, m.SessionID)
			}
		case AssistantMessage:
			assistantMsgs++
		case UserMessage:
			userMsgs++
		case ResultMessage:
			resultMsgs++
			t.Logf("Result: success=%v, turns=%d, cost=$%.6f", !m.IsError, m.NumTurns, m.TotalCostUSD)
		case StreamEvent:
			streamEvents++
		case ControlRequest:
			controlReqs++
		case ControlResponse:
			controlResps++
		default:
			unknownTypes++
			t.Logf("Line %d: unknown message type: %T", lineNum, msg)
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("Scanner error: %v", err)
	}

	t.Logf("\nTrace file parsed successfully!")
	t.Logf("Total lines: %d", lineNum)
	t.Logf("System messages: %d", systemMsgs)
	t.Logf("Assistant messages: %d", assistantMsgs)
	t.Logf("User messages: %d", userMsgs)
	t.Logf("Result messages: %d", resultMsgs)
	t.Logf("Stream events: %d", streamEvents)
	t.Logf("Control requests: %d", controlReqs)
	t.Logf("Control responses: %d", controlResps)
	t.Logf("Unknown types: %d", unknownTypes)
	t.Logf("Parse errors: %d", parseErrors)

	// Assertions
	if parseErrors > 0 {
		t.Errorf("Expected 0 parse errors, got %d", parseErrors)
	}

	// We should have at least some messages of each common type
	if systemMsgs == 0 {
		t.Error("Expected at least one system message")
	}
	if assistantMsgs == 0 {
		t.Error("Expected at least one assistant message")
	}
	if resultMsgs == 0 {
		t.Error("Expected at least one result message")
	}
	if streamEvents == 0 {
		t.Error("Expected at least one stream event")
	}
}

// TestParseTraceToCliMessages parses user messages sent to the CLI.
func TestParseTraceToCliMessages(t *testing.T) {
	tracePath := filepath.Join("..", "testdata", "traces", "to_cli.jsonl")

	file, err := os.Open(tracePath)
	if err != nil {
		t.Skipf("Trace file not found: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()

		msg, err := parseTraceEntry(line)
		if err != nil {
			t.Errorf("Line %d: failed to parse: %v", lineNum, err)
			continue
		}

		// Messages to CLI are user messages
		userMsg, ok := msg.(UserMessage)
		if !ok {
			t.Errorf("Line %d: expected UserMessage, got %T", lineNum, msg)
			continue
		}

		t.Logf("Line %d: user message role=%s", lineNum, userMsg.Message.Role)
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("Scanner error: %v", err)
	}

	t.Logf("Parsed %d messages to CLI", lineNum)
}

// TestParseStreamEventsFromTrace validates all stream event types.
func TestParseStreamEventsFromTrace(t *testing.T) {
	tracePath := filepath.Join("..", "testdata", "traces", "from_cli.jsonl")

	file, err := os.Open(tracePath)
	if err != nil {
		t.Skipf("Trace file not found: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	eventCounts := make(map[string]int)
	var parseErrors int

	for scanner.Scan() {
		line := scanner.Bytes()

		msg, err := parseTraceEntry(line)
		if err != nil {
			continue
		}

		streamEvent, ok := msg.(StreamEvent)
		if !ok {
			continue
		}

		// Parse the inner event
		innerEvent, err := ParseStreamEvent(streamEvent.Event)
		if err != nil {
			parseErrors++
			t.Logf("Failed to parse inner stream event: %v", err)
			continue
		}

		if innerEvent == nil {
			continue // Unknown event type
		}

		// Count by type
		switch e := innerEvent.(type) {
		case MessageStartEvent:
			eventCounts["message_start"]++
		case ContentBlockStartEvent:
			eventCounts["content_block_start"]++
		case ContentBlockDeltaEvent:
			eventCounts["content_block_delta"]++
			// Parse delta type
			var delta struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(e.Delta, &delta); err == nil {
				eventCounts["delta:"+delta.Type]++
			}
		case ContentBlockStopEvent:
			eventCounts["content_block_stop"]++
		case MessageDeltaEvent:
			eventCounts["message_delta"]++
		case MessageStopEvent:
			eventCounts["message_stop"]++
		default:
			eventCounts["unknown"]++
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("Scanner error: %v", err)
	}

	t.Logf("\nStream event types:")
	for eventType, count := range eventCounts {
		t.Logf("  %s: %d", eventType, count)
	}

	if parseErrors > 0 {
		t.Errorf("Expected 0 parse errors, got %d", parseErrors)
	}

	// We should have the basic event types
	if eventCounts["message_start"] == 0 {
		t.Error("Expected message_start events")
	}
	if eventCounts["content_block_start"] == 0 {
		t.Error("Expected content_block_start events")
	}
	if eventCounts["message_stop"] == 0 {
		t.Error("Expected message_stop events")
	}
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
