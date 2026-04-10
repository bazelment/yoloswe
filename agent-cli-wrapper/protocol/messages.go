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

	// MessageTypeKeepAlive is a heartbeat with no payload, emitted by the CLI
	// to keep the stdio connection alive when otherwise idle.
	MessageTypeKeepAlive MessageType = "keep_alive"
	// MessageTypeToolProgress is emitted periodically by the CLI while a tool
	// is still executing so consumers can render elapsed time.
	MessageTypeToolProgress MessageType = "tool_progress"
	// MessageTypeToolUseSummary is a summarized representation of one or more
	// tool invocations, used by streamlined/compact output modes.
	MessageTypeToolUseSummary MessageType = "tool_use_summary"
	// MessageTypeAuthStatus reports progress of an interactive OAuth flow.
	MessageTypeAuthStatus MessageType = "auth_status"
	// MessageTypeRateLimitEvent fires when the server-side rate limit state
	// changes (entering/leaving warning or rejected states).
	MessageTypeRateLimitEvent MessageType = "rate_limit_event"
	// MessageTypePromptSuggestion carries a predicted next user prompt and is
	// emitted after each turn when prompt suggestions are enabled. Internal.
	MessageTypePromptSuggestion MessageType = "prompt_suggestion"
	// MessageTypeStreamlinedText is a lightweight text-only replacement for an
	// assistant message in streamlined output. Internal.
	MessageTypeStreamlinedText MessageType = "streamlined_text"
	// MessageTypeStreamlinedToolUseSummary is a lightweight cumulative tool
	// summary that replaces tool_use blocks in streamlined output. Internal.
	MessageTypeStreamlinedToolUseSummary MessageType = "streamlined_tool_use_summary"
	// MessageTypeControlCancelRequest cancels a currently open control request
	// by request ID. Can flow in either direction.
	MessageTypeControlCancelRequest MessageType = "control_cancel_request"
	// MessageTypeUpdateEnvironmentVariables is an SDK→CLI stdin-only message
	// that updates process environment variables at runtime.
	MessageTypeUpdateEnvironmentVariables MessageType = "update_environment_variables"
)

// ResultSubtype discriminates the outcome of a completed turn.
type ResultSubtype string

const (
	// ResultSubtypeSuccess means the turn finished normally and the Result
	// field on ResultMessage contains the final assistant text.
	ResultSubtypeSuccess ResultSubtype = "success"
	// ResultSubtypeErrorDuringExecution means a fatal error occurred mid-turn.
	ResultSubtypeErrorDuringExecution ResultSubtype = "error_during_execution"
	// ResultSubtypeErrorMaxTurns means the turn cap was exceeded.
	ResultSubtypeErrorMaxTurns ResultSubtype = "error_max_turns"
	// ResultSubtypeErrorMaxBudgetUSD means the cost budget was exceeded.
	ResultSubtypeErrorMaxBudgetUSD ResultSubtype = "error_max_budget_usd"
	// ResultSubtypeErrorMaxStructuredOutputRetries means structured-output
	// parsing kept failing past the retry cap.
	ResultSubtypeErrorMaxStructuredOutputRetries ResultSubtype = "error_max_structured_output_retries"
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
	// raw preserves the on-wire JSON so DecodePayload() can decode
	// subtype-specific fields not declared on this flat envelope.
	raw json.RawMessage `json:"-"`
}

// UnmarshalJSON captures the raw bytes alongside the normal field decoding so
// that SystemMessage.DecodePayload() can re-decode into typed payload structs
// without losing subtype-specific fields that this flat envelope does not
// declare.
func (m *SystemMessage) UnmarshalJSON(data []byte) error {
	type systemMessageAlias SystemMessage
	var tmp systemMessageAlias
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}
	*m = SystemMessage(tmp)
	m.raw = append(json.RawMessage(nil), data...)
	return nil
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
//
// Result is populated only when Subtype == ResultSubtypeSuccess. For any of
// the error subtypes the Errors slice is populated instead with one or more
// human-readable error strings. Callers should prefer Outcome() to get a
// sealed variant that forces handling of the error case.
type ResultMessage struct {
	ModelUsage        map[string]ModelUsage `json:"modelUsage,omitempty"`
	SessionID         string                `json:"session_id"`
	Subtype           string                `json:"subtype"`
	UUID              string                `json:"uuid"`
	Type              MessageType           `json:"type"`
	Result            string                `json:"result"`
	Errors            []string              `json:"errors,omitempty"`
	PermissionDenials []interface{}         `json:"permission_denials,omitempty"`
	Usage             UsageDetails          `json:"usage"`
	TotalCostUSD      float64               `json:"total_cost_usd"`
	NumTurns          int                   `json:"num_turns"`
	DurationAPIMs     int64                 `json:"duration_api_ms"`
	DurationMs        int64                 `json:"duration_ms"`
	IsError           bool                  `json:"is_error"`
}

// ResultOutcome is a sealed interface describing the outcome of a turn.
// Implementations are ResultSuccess and ResultError.
type ResultOutcome interface {
	isResultOutcome()
}

// ResultSuccess is returned by ResultMessage.Outcome when the turn succeeded.
// Text holds the final assistant result string.
type ResultSuccess struct {
	Text string
}

func (ResultSuccess) isResultOutcome() {}

// ResultError is returned by ResultMessage.Outcome when the turn failed.
// Subtype identifies the specific failure mode; Errors lists any human
// readable diagnostics the CLI attached. Text holds the raw `result` field
// when present — some upstream error frames put the diagnostic there
// instead of in Errors, so callers should consider both.
type ResultError struct {
	Subtype ResultSubtype
	Text    string
	Errors  []string
}

func (ResultError) isResultOutcome() {}

// Outcome returns a sealed variant describing whether the turn succeeded or
// failed, so callers can switch on the concrete type instead of inspecting
// Subtype strings directly.
func (m ResultMessage) Outcome() ResultOutcome {
	if !m.IsError && ResultSubtype(m.Subtype) == ResultSubtypeSuccess {
		return ResultSuccess{Text: m.Result}
	}
	return ResultError{
		Subtype: ResultSubtype(m.Subtype),
		Errors:  m.Errors,
		Text:    m.Result,
	}
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

// KeepAliveMessage is a heartbeat with no payload. The CLI emits it on an
// idle timer so the stdio pipe does not appear dead to downstream consumers.
type KeepAliveMessage struct {
	Type MessageType `json:"type"`
}

// MsgType returns the message type.
func (m KeepAliveMessage) MsgType() MessageType { return MessageTypeKeepAlive }

// ToolProgressMessage is emitted periodically while a tool is executing so
// consumers can surface a live "running for Ns" indicator. ParentToolUseID is
// set when the tool runs under a delegated sub-agent; TaskID is set when it
// belongs to a background task.
type ToolProgressMessage struct {
	ParentToolUseID    *string     `json:"parent_tool_use_id"`
	TaskID             *string     `json:"task_id,omitempty"`
	Type               MessageType `json:"type"`
	ToolUseID          string      `json:"tool_use_id"`
	ToolName           string      `json:"tool_name"`
	UUID               string      `json:"uuid"`
	SessionID          string      `json:"session_id"`
	ElapsedTimeSeconds float64     `json:"elapsed_time_seconds"`
}

// MsgType returns the message type.
func (m ToolProgressMessage) MsgType() MessageType { return MessageTypeToolProgress }

// ToolUseSummaryMessage is a textual summary covering one or more previously
// emitted tool_use blocks. PrecedingToolUseIDs lists the tool-use IDs this
// summary replaces/summarizes.
type ToolUseSummaryMessage struct {
	Type                MessageType `json:"type"`
	Summary             string      `json:"summary"`
	UUID                string      `json:"uuid"`
	SessionID           string      `json:"session_id"`
	PrecedingToolUseIDs []string    `json:"preceding_tool_use_ids"`
}

// MsgType returns the message type.
func (m ToolUseSummaryMessage) MsgType() MessageType { return MessageTypeToolUseSummary }

// AuthStatusMessage reports progress of an interactive OAuth login flow.
// Output accumulates the CLI-facing status lines; Error is set once the
// flow fails terminally.
type AuthStatusMessage struct {
	Error            *string     `json:"error,omitempty"`
	Type             MessageType `json:"type"`
	UUID             string      `json:"uuid"`
	SessionID        string      `json:"session_id"`
	Output           []string    `json:"output"`
	IsAuthenticating bool        `json:"isAuthenticating"`
}

// MsgType returns the message type.
func (m AuthStatusMessage) MsgType() MessageType { return MessageTypeAuthStatus }

// RateLimitInfo mirrors the upstream SDKRateLimitInfo shape. Field names use
// camelCase to match the wire format. Timestamps (ResetsAt, OverageResetsAt)
// are unix seconds and SurpassedThreshold is a numeric ratio — upstream
// encodes them as numbers, not strings.
type RateLimitInfo struct {
	ResetsAt              *float64 `json:"resetsAt,omitempty"`
	Utilization           *float64 `json:"utilization,omitempty"`
	OverageResetsAt       *float64 `json:"overageResetsAt,omitempty"`
	OverageDisabledReason *string  `json:"overageDisabledReason,omitempty"`
	SurpassedThreshold    *float64 `json:"surpassedThreshold,omitempty"`
	Status                string   `json:"status"`
	RateLimitType         string   `json:"rateLimitType,omitempty"`
	OverageStatus         string   `json:"overageStatus,omitempty"`
	IsUsingOverage        bool     `json:"isUsingOverage,omitempty"`
}

// RateLimitEventMessage fires whenever the server's rate-limit state for the
// current subscription changes (e.g. crossing into allowed_warning or
// rejected).
type RateLimitEventMessage struct {
	Type          MessageType   `json:"type"`
	UUID          string        `json:"uuid"`
	SessionID     string        `json:"session_id"`
	RateLimitInfo RateLimitInfo `json:"rate_limit_info"`
}

// MsgType returns the message type.
func (m RateLimitEventMessage) MsgType() MessageType { return MessageTypeRateLimitEvent }

// PromptSuggestionMessage carries a predicted next user prompt, emitted at
// the end of a turn when prompt suggestions are enabled. Internal feature.
type PromptSuggestionMessage struct {
	Type       MessageType `json:"type"`
	Suggestion string      `json:"suggestion"`
	UUID       string      `json:"uuid"`
	SessionID  string      `json:"session_id"`
}

// MsgType returns the message type.
func (m PromptSuggestionMessage) MsgType() MessageType { return MessageTypePromptSuggestion }

// StreamlinedTextMessage is a lightweight text-only assistant message used in
// streamlined output mode, with thinking and tool_use blocks stripped.
type StreamlinedTextMessage struct {
	Type      MessageType `json:"type"`
	Text      string      `json:"text"`
	SessionID string      `json:"session_id"`
	UUID      string      `json:"uuid"`
}

// MsgType returns the message type.
func (m StreamlinedTextMessage) MsgType() MessageType { return MessageTypeStreamlinedText }

// StreamlinedToolUseSummaryMessage replaces tool_use blocks in streamlined
// output with a cumulative human-readable summary string.
type StreamlinedToolUseSummaryMessage struct {
	Type        MessageType `json:"type"`
	ToolSummary string      `json:"tool_summary"`
	SessionID   string      `json:"session_id"`
	UUID        string      `json:"uuid"`
}

// MsgType returns the message type.
func (m StreamlinedToolUseSummaryMessage) MsgType() MessageType {
	return MessageTypeStreamlinedToolUseSummary
}

// ControlCancelRequest cancels a currently open control request by ID. It
// can be sent in either direction on the stdio stream.
type ControlCancelRequest struct {
	Type      MessageType `json:"type"`
	RequestID string      `json:"request_id"`
}

// MsgType returns the message type.
func (m ControlCancelRequest) MsgType() MessageType { return MessageTypeControlCancelRequest }

// UpdateEnvironmentVariablesMessage is an SDK→CLI stdin-only message that
// updates the CLI process environment variables at runtime.
type UpdateEnvironmentVariablesMessage struct {
	Variables map[string]string `json:"variables"`
	Type      MessageType       `json:"type"`
}

// MsgType returns the message type.
func (m UpdateEnvironmentVariablesMessage) MsgType() MessageType {
	return MessageTypeUpdateEnvironmentVariables
}
