// Fake Codex app-server for integration testing.
// Speaks the JSON-RPC 2.0 protocol over stdin/stdout.
//
// Behavior:
// - Responds to initialize, thread/start, turn/start handshake
// - Emits token usage events during turns
// - Completes turns immediately (or after a delay if FAKE_CODEX_SLOW=true)
// - Exits cleanly when stdin closes
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

type fakeCodexServer struct {
	out       io.Writer
	sleep     func(time.Duration)
	slow      bool
	turnCount int
}

func writeMsg(w io.Writer, msg rpcMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}

func writeResponse(w io.Writer, id any, result any) error {
	return writeMsg(w, rpcMessage{JSONRPC: "2.0", ID: id, Result: result})
}

func writeNotification(w io.Writer, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return writeMsg(w, rpcMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

func serveFakeCodex(in io.Reader, out io.Writer, slow bool, sleep func(time.Duration)) error {
	if sleep == nil {
		sleep = time.Sleep
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	server := &fakeCodexServer{
		out:   out,
		sleep: sleep,
		slow:  slow,
	}

	for scanner.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if err := server.handle(msg); err != nil {
			return err
		}
	}

	return scanner.Err()
}

func (s *fakeCodexServer) handle(msg rpcMessage) error {
	switch {
	case msg.ID != nil && msg.Method == "":
		// This is a response to a request we sent (e.g., approval response).
		return nil

	case msg.Method == "initialize":
		return writeResponse(s.out, msg.ID, map[string]any{
			"capabilities": map[string]any{},
		})

	case msg.Method == "initialized":
		// Notification, no response.
		return nil

	case msg.Method == "thread/start":
		return writeResponse(s.out, msg.ID, map[string]any{
			"thread": map[string]any{
				"id": "thread-fake-001",
			},
		})

	case msg.Method == "turn/start":
		return s.handleTurnStart(msg)

	default:
		if msg.ID == nil {
			return nil
		}
		return writeMsg(s.out, rpcMessage{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Error: map[string]any{
				"code":    -32601,
				"message": "method not found: " + msg.Method,
			},
		})
	}
}

func (s *fakeCodexServer) handleTurnStart(msg rpcMessage) error {
	s.turnCount++
	turnID := fmt.Sprintf("turn-fake-%03d", s.turnCount)
	if err := writeResponse(s.out, msg.ID, map[string]any{
		"turn": map[string]any{
			"id": turnID,
		},
	}); err != nil {
		return err
	}

	if s.slow {
		s.sleep(2 * time.Second)
	} else {
		s.sleep(100 * time.Millisecond)
	}

	inputToks := int64(100 * s.turnCount)
	outputToks := int64(50 * s.turnCount)
	if err := writeNotification(s.out, "thread/tokenUsage/updated", map[string]any{
		"input_tokens":  inputToks,
		"output_tokens": outputToks,
		"total_tokens":  inputToks + outputToks,
	}); err != nil {
		return err
	}

	if s.slow {
		s.sleep(500 * time.Millisecond)
	}

	return writeNotification(s.out, "turn/completed", map[string]any{
		"usage": map[string]any{
			"total_token_usage": map[string]any{
				"input_tokens":  inputToks,
				"output_tokens": outputToks,
				"total_tokens":  inputToks + outputToks,
			},
		},
	})
}

func main() {
	slow := os.Getenv("FAKE_CODEX_SLOW") == "true"
	if err := serveFakeCodex(os.Stdin, os.Stdout, slow, time.Sleep); err != nil {
		fmt.Fprintf(os.Stderr, "fake_codex: %v\n", err)
		os.Exit(1)
	}
}
