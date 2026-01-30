package protocol

import "encoding/json"

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
	Type              MessageType `json:"type"`
	Subtype           string      `json:"subtype"`
	SessionID         string      `json:"session_id"`
	UUID              string      `json:"uuid"`
	CWD               string      `json:"cwd,omitempty"`
	Tools             []string    `json:"tools,omitempty"`
	MCPServers        []MCPServer `json:"mcp_servers,omitempty"`
	Model             string      `json:"model,omitempty"`
	PermissionMode    string      `json:"permissionMode,omitempty"`
	SlashCommands     []string    `json:"slash_commands,omitempty"`
	Agents            []string    `json:"agents,omitempty"`
	Skills            []string    `json:"skills,omitempty"`
	Plugins           []Plugin    `json:"plugins,omitempty"`
	APIKeySource      string      `json:"apiKeySource,omitempty"`
	OutputStyle       string      `json:"output_style,omitempty"`
	ClaudeCodeVersion string      `json:"claude_code_version,omitempty"`

	// Hook response fields (when subtype === 'hook_response')
	HookName  string `json:"hook_name,omitempty"`
	HookEvent string `json:"hook_event,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

// MsgType returns the message type.
func (m SystemMessage) MsgType() MessageType { return MessageTypeSystem }

// Usage tracks token usage.
type Usage struct {
	InputTokens              int           `json:"input_tokens"`
	CacheCreationInputTokens int           `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int           `json:"cache_read_input_tokens"`
	OutputTokens             int           `json:"output_tokens"`
	ServiceTier              string        `json:"service_tier,omitempty"`
	CacheCreation            CacheCreation `json:"cache_creation,omitempty"`
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
	Type            MessageType    `json:"type"`
	Message         MessageContent `json:"message"`
	ParentToolUseID *string        `json:"parent_tool_use_id"`
	SessionID       string         `json:"session_id"`
	UUID            string         `json:"uuid"`
}

// MsgType returns the message type.
func (m AssistantMessage) MsgType() MessageType { return MessageTypeAssistant }

// UserMessage represents tool results echoed back.
type UserMessage struct {
	Type            MessageType    `json:"type"`
	Message         MessageContent `json:"message"`
	SessionID       string         `json:"session_id"`
	UUID            string         `json:"uuid"`
	ParentToolUseID *string        `json:"parent_tool_use_id"`
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
	InputTokens              int                `json:"input_tokens"`
	CacheCreationInputTokens int                `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                `json:"cache_read_input_tokens"`
	OutputTokens             int                `json:"output_tokens"`
	ServiceTier              string             `json:"service_tier,omitempty"`
	ServerToolUse            ServerToolUseStats `json:"server_tool_use,omitempty"`
	CacheCreation            CacheCreation      `json:"cache_creation,omitempty"`
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
	Type              MessageType           `json:"type"`
	Subtype           string                `json:"subtype"`
	IsError           bool                  `json:"is_error"`
	DurationMs        int64                 `json:"duration_ms"`
	DurationAPIMs     int64                 `json:"duration_api_ms"`
	NumTurns          int                   `json:"num_turns"`
	Result            string                `json:"result"`
	SessionID         string                `json:"session_id"`
	TotalCostUSD      float64               `json:"total_cost_usd"`
	Usage             UsageDetails          `json:"usage"`
	ModelUsage        map[string]ModelUsage `json:"modelUsage,omitempty"`
	PermissionDenials []interface{}         `json:"permission_denials,omitempty"`
	UUID              string                `json:"uuid"`
}

// MsgType returns the message type.
func (m ResultMessage) MsgType() MessageType { return MessageTypeResult }

// UserMessageToSend is what we send to the CLI.
type UserMessageToSend struct {
	Type    string                 `json:"type"`
	Message UserMessageToSendInner `json:"message"`
}

// UserMessageToSendInner is the inner part of messages we send.
type UserMessageToSendInner struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []ContentBlock
}

// RawMessage is used for initial type discrimination.
type RawMessage struct {
	Type MessageType     `json:"type"`
	Raw  json.RawMessage `json:"-"`
}
