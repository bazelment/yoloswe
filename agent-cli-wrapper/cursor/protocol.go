package cursor

import (
	"encoding/json"
	"fmt"
)

// RawMessage is used for initial type discrimination of NDJSON lines.
type RawMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
}

// SystemInitMessage represents a system init message.
// Example: {"type":"system","subtype":"init","session_id":"...","model":"...","cwd":"...","permissionMode":"...","apiKeySource":"..."}
type SystemInitMessage struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	SessionID      string `json:"session_id"`
	Model          string `json:"model"`
	CWD            string `json:"cwd"`
	PermissionMode string `json:"permissionMode"`
	APIKeySource   string `json:"apiKeySource"`
}

// AssistantMessageContent is a content block within an assistant message.
type AssistantMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// AssistantMessageInner is the inner message object of an assistant message.
type AssistantMessageInner struct {
	Role    string                    `json:"role"`
	Content []AssistantMessageContent `json:"content"`
}

// AssistantMessage represents an assistant text message.
// Example: {"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"..."}]},"session_id":"..."}
type AssistantMessage struct {
	Type      string                `json:"type"`
	Message   AssistantMessageInner `json:"message"`
	SessionID string                `json:"session_id"`
}

// ToolCallMessage represents a tool call event (started or completed).
// The tool_call field is a map with a single key (the tool name) mapping to the tool call detail.
// Example: {"type":"tool_call","subtype":"started","call_id":"...","tool_call":{"Read":{"args":{"file_path":"..."}}},"session_id":"..."}
// Example: {"type":"tool_call","subtype":"completed","call_id":"...","tool_call":{"Read":{"args":{"file_path":"..."},"result":"..."}},"session_id":"..."}
type ToolCallMessage struct {
	Type      string                            `json:"type"`
	Subtype   string                            `json:"subtype"`
	CallID    string                            `json:"call_id"`
	ToolCall  map[string]map[string]interface{} `json:"tool_call"`
	SessionID string                            `json:"session_id"`
}

// ToolCallDetail holds the extracted name, args, and optional result from a tool call.
type ToolCallDetail struct {
	Name   string
	Args   map[string]interface{}
	Result interface{}
}

// ParseToolCallDetail extracts the tool call detail from a ToolCallMessage.
// The tool_call field is a map with a single key (tool name) → {args, result?}.
func ParseToolCallDetail(msg *ToolCallMessage) (*ToolCallDetail, error) {
	if msg == nil || len(msg.ToolCall) == 0 {
		return nil, fmt.Errorf("empty tool_call field")
	}

	for name, detail := range msg.ToolCall {
		d := &ToolCallDetail{Name: name}

		if args, ok := detail["args"]; ok {
			if argsMap, ok := args.(map[string]interface{}); ok {
				d.Args = argsMap
			}
		}

		if result, ok := detail["result"]; ok {
			d.Result = result
		}

		return d, nil
	}

	return nil, fmt.Errorf("no tool call entries found")
}

// ResultMessage represents the final result of a session.
// Example: {"type":"result","subtype":"success","duration_ms":1234,"duration_api_ms":1000,"is_error":false,"result":"...","session_id":"..."}
type ResultMessage struct {
	Type          string `json:"type"`
	Subtype       string `json:"subtype"`
	DurationMs    int64  `json:"duration_ms"`
	DurationAPIMs int64  `json:"duration_api_ms"`
	IsError       bool   `json:"is_error"`
	Result        string `json:"result"`
	SessionID     string `json:"session_id"`
}

// Message is the union type returned by ParseMessage.
type Message interface {
	messageType() string
}

func (m *SystemInitMessage) messageType() string { return "system" }
func (m *AssistantMessage) messageType() string   { return "assistant" }
func (m *ToolCallMessage) messageType() string    { return "tool_call" }
func (m *ResultMessage) messageType() string      { return "result" }

// ParseMessage parses a raw NDJSON line into a typed message.
func ParseMessage(line []byte) (Message, error) {
	var raw RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse message type: %w", err)
	}

	switch raw.Type {
	case "system":
		if raw.Subtype == "init" {
			var msg SystemInitMessage
			if err := json.Unmarshal(line, &msg); err != nil {
				return nil, fmt.Errorf("failed to parse system init message: %w", err)
			}
			return &msg, nil
		}
		return nil, fmt.Errorf("unknown system subtype: %s", raw.Subtype)

	case "assistant":
		var msg AssistantMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse assistant message: %w", err)
		}
		return &msg, nil

	case "tool_call":
		var msg ToolCallMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse tool_call message: %w", err)
		}
		return &msg, nil

	case "result":
		var msg ResultMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse result message: %w", err)
		}
		return &msg, nil

	default:
		// Unknown message types (e.g. "user", "thinking") are silently skipped.
		return nil, nil
	}
}
