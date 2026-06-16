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
	SessionID string                `json:"session_id"`
	Message   AssistantMessageInner `json:"message"`
}

// ToolCallMessage represents a tool call event (started or completed).
//
// The tool_call field carries a single tool call but the cursor-agent CLI emits
// it in more than one JSON shape, so it is held as a raw message and decoded by
// ParseToolCallDetail rather than typed directly (a typed field made the whole
// frame — and therefore the whole session — fail when the shape drifted).
//
// Observed shapes:
//   - object (documented): {"readToolCall":{"args":{...},"result":...}}
//   - array:               [{"readToolCall":{"args":{...}}}]
//
// Example: {"type":"tool_call","subtype":"started","call_id":"...","tool_call":{"readToolCall":{"args":{"path":"..."}}},"session_id":"..."}
// Example: {"type":"tool_call","subtype":"completed","call_id":"...","tool_call":{"readToolCall":{"args":{"path":"..."},"result":"..."}},"session_id":"..."}
type ToolCallMessage struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	CallID    string          `json:"call_id"`
	SessionID string          `json:"session_id"`
	ToolCall  json.RawMessage `json:"tool_call"`
}

// ToolCallDetail holds the extracted name, args, and optional result from a tool call.
type ToolCallDetail struct {
	Args   map[string]interface{}
	Result interface{}
	Name   string
}

// toolCallEntry is the {tool_name → {args, result?}} mapping that appears either
// directly as the tool_call object or as the elements of the tool_call array.
type toolCallEntry map[string]map[string]interface{}

// ParseToolCallDetail extracts the tool call detail from a ToolCallMessage.
// The tool_call field is a single-key map (tool name) → {args, result?}, which
// the CLI emits either bare (object shape) or wrapped in a one-element array.
func ParseToolCallDetail(msg *ToolCallMessage) (*ToolCallDetail, error) {
	if msg == nil || len(msg.ToolCall) == 0 {
		return nil, fmt.Errorf("empty tool_call field")
	}

	entry, err := decodeToolCallEntry(msg.ToolCall)
	if err != nil {
		return nil, err
	}

	for name, detail := range entry {
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

// decodeToolCallEntry decodes the raw tool_call field into a single
// {tool_name → detail} entry, tolerating both the object and array shapes the
// cursor-agent CLI emits.
func decodeToolCallEntry(raw json.RawMessage) (toolCallEntry, error) {
	trimmed := skipJSONSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty tool_call field")
	}

	switch trimmed[0] {
	case '{':
		var entry toolCallEntry
		if err := json.Unmarshal(trimmed, &entry); err != nil {
			return nil, fmt.Errorf("decode tool_call object: %w", err)
		}
		return entry, nil
	case '[':
		var entries []toolCallEntry
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("decode tool_call array: %w", err)
		}
		for _, entry := range entries {
			if len(entry) > 0 {
				return entry, nil
			}
		}
		return nil, fmt.Errorf("no tool call entries found")
	default:
		return nil, fmt.Errorf("unexpected tool_call shape: %s", string(trimmed[:1]))
	}
}

// skipJSONSpace trims leading JSON whitespace so the first meaningful byte can
// be inspected to discriminate object vs array shape.
func skipJSONSpace(b []byte) []byte {
	for len(b) > 0 {
		switch b[0] {
		case ' ', '\t', '\r', '\n':
			b = b[1:]
		default:
			return b
		}
	}
	return b
}

// ResultMessage represents the final result of a session.
// Example: {"type":"result","subtype":"success","duration_ms":1234,"duration_api_ms":1000,"is_error":false,"result":"...","session_id":"..."}
type ResultMessage struct {
	Type          string `json:"type"`
	Subtype       string `json:"subtype"`
	Result        string `json:"result"`
	SessionID     string `json:"session_id"`
	DurationMs    int64  `json:"duration_ms"`
	DurationAPIMs int64  `json:"duration_api_ms"`
	IsError       bool   `json:"is_error"`
}

// IsFailure reports whether the result frame represents a failed turn. A turn
// is failed when is_error is true OR the subtype is anything other than
// "success" — error-subtyped frames sometimes leave is_error=false while
// signalling failure via the subtype, mirroring the Claude CLI contract.
func (m ResultMessage) IsFailure() bool {
	return m.IsError || (m.Subtype != "" && m.Subtype != "success")
}

// Message is the union type returned by ParseMessage.
type Message interface {
	messageType() string
}

func (m *SystemInitMessage) messageType() string { return "system" }
func (m *AssistantMessage) messageType() string  { return "assistant" }
func (m *ToolCallMessage) messageType() string   { return "tool_call" }
func (m *ResultMessage) messageType() string     { return "result" }

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
		// Unknown system subtypes are silently skipped (forward-compatible).
		return nil, nil

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
