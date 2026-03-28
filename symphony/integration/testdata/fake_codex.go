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
	"os"
	"time"
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
}

func writeMsg(msg rpcMessage) {
	data, _ := json.Marshal(msg)
	fmt.Fprintf(os.Stdout, "%s\n", data)
}

func writeResponse(id any, result any) {
	writeMsg(rpcMessage{JSONRPC: "2.0", ID: id, Result: result})
}

func writeNotification(method string, params any) {
	raw, _ := json.Marshal(params)
	writeMsg(rpcMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

func main() {
	slow := os.Getenv("FAKE_CODEX_SLOW") == "true"
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	turnCount := 0

	for scanner.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}

		switch {
		case msg.ID != nil && msg.Method == "":
			// This is a response to a request we sent (e.g., approval response).
			// Ignore.
			continue

		case msg.Method == "initialize":
			writeResponse(msg.ID, map[string]any{
				"capabilities": map[string]any{},
			})

		case msg.Method == "initialized":
			// Notification, no response.

		case msg.Method == "thread/start":
			writeResponse(msg.ID, map[string]any{
				"thread": map[string]any{
					"id": "thread-fake-001",
				},
			})

		case msg.Method == "turn/start":
			turnCount++
			turnID := fmt.Sprintf("turn-fake-%03d", turnCount)
			writeResponse(msg.ID, map[string]any{
				"turn": map[string]any{
					"id": turnID,
				},
			})

			if slow {
				time.Sleep(2 * time.Second)
			} else {
				time.Sleep(100 * time.Millisecond)
			}

			// Emit token usage.
			inputToks := int64(100 * turnCount)
			outputToks := int64(50 * turnCount)
			writeNotification("thread/tokenUsage/updated", map[string]any{
				"input_tokens":  inputToks,
				"output_tokens": outputToks,
				"total_tokens":  inputToks + outputToks,
			})

			if slow {
				time.Sleep(500 * time.Millisecond)
			}

			// Complete the turn.
			writeNotification("turn/completed", map[string]any{
				"usage": map[string]any{
					"total_token_usage": map[string]any{
						"input_tokens":  inputToks,
						"output_tokens": outputToks,
						"total_tokens":  inputToks + outputToks,
					},
				},
			})

		default:
			// Unknown method, respond with error if it has an ID.
			if msg.ID != nil {
				data, _ := json.Marshal(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"error": map[string]any{
						"code":    -32601,
						"message": "method not found: " + msg.Method,
					},
				})
				fmt.Fprintf(os.Stdout, "%s\n", data)
			}
		}
	}
}
