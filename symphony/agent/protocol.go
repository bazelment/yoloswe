package agent

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// Message represents a JSON-RPC message (request, response, or notification).
type Message struct {
	ID     any             `json:"id,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// RPCError represents a JSON-RPC error.
type RPCError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// Protocol manages JSON-RPC request/response tracking.
type Protocol struct {
	process *Process
	nextID  atomic.Int64
}

// NewProtocol wraps a process with JSON-RPC protocol handling.
func NewProtocol(p *Process) *Protocol {
	return &Protocol{process: p}
}

// Send sends a JSON-RPC request and returns the response.
func (pr *Protocol) Send(method string, params any) (*Message, error) {
	id := pr.nextID.Add(1)

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}

	if err := pr.process.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	// Read lines until we get a response matching our request ID.
	for {
		line, err := pr.process.ReadLine()
		if err != nil {
			return nil, fmt.Errorf("read response for %s: %w", method, err)
		}

		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			// Non-JSON line from stdout, skip (spec says parse attempts on complete lines).
			continue
		}

		// Check if this is our response.
		if msgID, ok := toFloat64(msg.ID); ok {
			if msgID == float64(id) {
				if msg.Error != nil {
					return nil, fmt.Errorf("%s error: code=%d message=%s", method, msg.Error.Code, msg.Error.Message)
				}
				return &msg, nil
			}
		}

		// Not our response — it's a notification or event. Skip for now.
		// In the full session, events would be dispatched to handlers.
	}
}

// Notify sends a JSON-RPC notification (no response expected).
func (pr *Protocol) Notify(method string, params any) error {
	req := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	return pr.process.WriteJSON(req)
}

// ReadMessage reads and parses one JSON-RPC message from stdout.
func (pr *Protocol) ReadMessage() (*Message, error) {
	line, err := pr.process.ReadLine()
	if err != nil {
		return nil, err
	}

	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return &msg, nil
}

// Respond sends a JSON-RPC response to a request.
func (pr *Protocol) Respond(id any, result any) error {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	return pr.process.WriteJSON(resp)
}

// RespondError sends a JSON-RPC error response.
func (pr *Protocol) RespondError(id any, code int, message string) error {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	return pr.process.WriteJSON(resp)
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
