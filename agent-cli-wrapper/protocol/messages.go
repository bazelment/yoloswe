package protocol

import (
	"encoding/json"
	"fmt"
)

// MessageType discriminates between message kinds.
type MessageType string

const (
	MessageTypeSystem          MessageType = "system"
	MessageTypeAssistant       MessageType = "assistant"
	MessageTypeUser            MessageType = "user"
	MessageTypeResult          MessageType = "result"
	MessageTypeStreamEvent     MessageType = "stream_event"
	MessageTypeControlRequest  MessageType = "control_request"
	MessageTypeControlResponse MessageType = "control_response"
)

// Message is the interface for all protocol messages.
type Message interface {
	MsgType() MessageType
}

// MCPServer represents an MCP server connection.
type MCPServer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Plugin represents a loaded plugin.
type Plugin struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// SystemMessage represents session initialization and system events.
type SystemMessage struct {
	ExitCode          *int        `json:"exit_code,omitempty"`
	UUID              string      `json:"uuid"`
	PermissionMode    string      `json:"permissionMode,omitempty"`
	ClaudeCodeVersion string      `json:"claude_code_version,omitempty"`
	CWD               string      `json:"cwd,omitempty"`
	Type              MessageType `json:"type"`
	Subtype           string      `json:"subtype"`
	Model             string      `json:"model,omitempty"`
	SessionID         string      `json:"session_id"`
	Stderr            string      `json:"stderr,omitempty"`
	Stdout            string      `json:"stdout,omitempty"`
	HookEvent         string      `json:"hook_event,omitempty"`
	HookName          string      `json:"hook_name,omitempty"`
	APIKeySource      string      `json:"apiKeySource,omitempty"`
	OutputStyle       string      `json:"output_style,omitempty"`
	Tools             []string    `json:"tools,omitempty"`
	Plugins           []Plugin    `json:"plugins,omitempty"`
	Skills            []string    `json:"skills,omitempty"`
	Agents            []string    `json:"agents,omitempty"`
	SlashCommands     []string    `json:"slash_commands,omitempty"`
	MCPServers        []MCPServer `json:"mcp_servers,omitempty"`
}

// MsgType returns the message type.
func (m SystemMessage) MsgType() MessageType { return MessageTypeSystem }

// Usage tracks token usage.
type Usage struct {
	ServiceTier              string        `json:"service_tier,omitempty"`
	CacheCreation            CacheCreation `json:"cache_creation,omitempty"`
	InputTokens              int           `json:"input_tokens"`
	CacheCreationInputTokens int           `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int           `json:"cache_read_input_tokens"`
	OutputTokens             int           `json:"output_tokens"`
}

// CacheCreation contains cache creation timing details.
type CacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens,omitempty"`
}

// FlexibleContent can be either a string or an array of content blocks.
type FlexibleContent struct {
	raw json.RawMessage
}

// UnmarshalJSON implements json.Unmarshaler.
func (fc *FlexibleContent) UnmarshalJSON(data []byte) error {
	fc.raw = data
	return nil
}

// MarshalJSON implements json.Marshaler.
func (fc FlexibleContent) MarshalJSON() ([]byte, error) {
	if fc.raw == nil {
		return []byte("null"), nil
	}
	return fc.raw, nil
}

// IsString returns true if the content is a string.
func (fc FlexibleContent) IsString() bool {
	if len(fc.raw) == 0 {
		return false
	}
	return fc.raw[0] == '"'
}

// AsString returns the content as a string (if it is one).
func (fc FlexibleContent) AsString() (string, bool) {
	if !fc.IsString() {
		return "", false
	}
	var s string
	if err := json.Unmarshal(fc.raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// AsBlocks returns the content as content blocks (if it is an array).
func (fc FlexibleContent) AsBlocks() (ContentBlocks, bool) {
	if fc.IsString() || len(fc.raw) == 0 {
		return nil, false
	}
	var blocks ContentBlocks
	if err := json.Unmarshal(fc.raw, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

// MessageContent is the inner content of assistant/user messages.
type MessageContent struct {
	Model        string          `json:"model,omitempty"`
	ID           string          `json:"id,omitempty"`
	Type         string          `json:"type,omitempty"`
	Role         string          `json:"role"`
	Content      FlexibleContent `json:"content"`
	StopReason   *string         `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        Usage           `json:"usage,omitempty"`
}

// AssistantMessage is a complete message from Claude.
type AssistantMessage struct {
	ParentToolUseID *string        `json:"parent_tool_use_id"`
	Type            MessageType    `json:"type"`
	SessionID       string         `json:"session_id"`
	UUID            string         `json:"uuid"`
	Message         MessageContent `json:"message"`
}

// MsgType returns the message type.
func (m AssistantMessage) MsgType() MessageType { return MessageTypeAssistant }

// UserMessage represents tool results echoed back.
type UserMessage struct {
	ParentToolUseID *string        `json:"parent_tool_use_id"`
	Type            MessageType    `json:"type"`
	SessionID       string         `json:"session_id"`
	UUID            string         `json:"uuid"`
	Message         MessageContent `json:"message"`
}

// MsgType returns the message type.
func (m UserMessage) MsgType() MessageType { return MessageTypeUser }

// ServerToolUseStats tracks server-side tool usage.
type ServerToolUseStats struct {
	WebSearchRequests int `json:"web_search_requests,omitempty"`
	WebFetchRequests  int `json:"web_fetch_requests,omitempty"`
}

// UsageDetails is the extended usage in ResultMessage.
type UsageDetails struct {
	ServiceTier              string             `json:"service_tier,omitempty"`
	ServerToolUse            ServerToolUseStats `json:"server_tool_use,omitempty"`
	CacheCreation            CacheCreation      `json:"cache_creation,omitempty"`
	InputTokens              int                `json:"input_tokens"`
	CacheCreationInputTokens int                `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                `json:"cache_read_input_tokens"`
	OutputTokens             int                `json:"output_tokens"`
}

// ModelUsage tracks usage per model.
type ModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	WebSearchRequests        int     `json:"webSearchRequests,omitempty"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int     `json:"contextWindow,omitempty"`
	MaxOutputTokens          int     `json:"maxOutputTokens,omitempty"`
}

// ResultMessage contains turn completion metrics.
type ResultMessage struct {
	ModelUsage        map[string]ModelUsage `json:"modelUsage,omitempty"`
	SessionID         string                `json:"session_id"`
	Subtype           string                `json:"subtype"`
	UUID              string                `json:"uuid"`
	Type              MessageType           `json:"type"`
	Result            string                `json:"result"`
	PermissionDenials []interface{}         `json:"permission_denials,omitempty"`
	Usage             UsageDetails          `json:"usage"`
	TotalCostUSD      float64               `json:"total_cost_usd"`
	NumTurns          int                   `json:"num_turns"`
	DurationAPIMs     int64                 `json:"duration_api_ms"`
	DurationMs        int64                 `json:"duration_ms"`
	IsError           bool                  `json:"is_error"`
}

// MsgType returns the message type.
func (m ResultMessage) MsgType() MessageType { return MessageTypeResult }

// UserMessageToSend is what we send to the CLI.
type UserMessageToSend struct {
	Message UserMessageToSendInner `json:"message"`
	Type    string                 `json:"type"`
}

// UserMessageToSendInner is the inner part of messages we send.
type UserMessageToSendInner struct {
	Content interface{} `json:"content"`
	Role    string      `json:"role"`
}

// Marshal serializes the message to a JSON line ready to write to the CLI.
func (m UserMessageToSend) Marshal() ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal UserMessageToSend: %w", err)
	}
	return b, nil
}

// RawMessage is used for initial type discrimination.
type RawMessage struct {
	Type MessageType     `json:"type"`
	Raw  json.RawMessage `json:"-"`
}
