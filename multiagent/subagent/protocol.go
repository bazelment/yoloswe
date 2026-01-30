// Package subagent provides a streaming protocol for sub-agent communication.
//
// The protocol enables real-time progress streaming, automatic file tracking,
// cost aggregation, and graceful cancellation between the Planner and its
// sub-agents (Designer, Builder, Reviewer).
package subagent

import (
	"encoding/json"
	"time"

	"github.com/bazelment/yoloswe/multiagent/protocol"
)

// MessageType discriminates between protocol message kinds.
type MessageType string

const (
	// Request messages (Planner -> Sub-agent)
	MessageTypeRequest MessageType = "request"
	MessageTypeCancel  MessageType = "cancel"

	// Response messages (Sub-agent -> Planner)
	MessageTypeProgress   MessageType = "progress"
	MessageTypeFileEvent  MessageType = "file_event"
	MessageTypeCostUpdate MessageType = "cost_update"
	MessageTypeResult     MessageType = "result"
	MessageTypeError      MessageType = "error"
)

// AgentType identifies the type of sub-agent.
type AgentType string

const (
	AgentTypeDesigner AgentType = "designer"
	AgentTypeBuilder  AgentType = "builder"
	AgentTypeReviewer AgentType = "reviewer"
)

// Phase indicates the current execution phase of a sub-agent.
type Phase string

const (
	PhaseThinking  Phase = "thinking"
	PhaseToolCall  Phase = "tool_call"
	PhaseStreaming Phase = "streaming"
)

// FileAction indicates the type of file operation.
type FileAction string

const (
	FileActionCreate FileAction = "create"
	FileActionModify FileAction = "modify"
	FileActionDelete FileAction = "delete"
	FileActionRead   FileAction = "read"
)

// Request is the envelope for sub-agent task requests.
type Request struct {
	Type      MessageType     `json:"type"`
	ID        string          `json:"id"`
	Agent     AgentType       `json:"agent"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// Cancel requests termination of an in-progress task.
type Cancel struct {
	Type      MessageType `json:"type"`
	RequestID string      `json:"request_id"`
	Reason    string      `json:"reason,omitempty"`
}

// Progress streams incremental updates during task execution.
type Progress struct {
	Type      MessageType `json:"type"`
	RequestID string      `json:"request_id"`
	Agent     AgentType   `json:"agent"`
	Timestamp time.Time   `json:"timestamp"`

	// Progress details
	Phase   Phase  `json:"phase"`
	Message string `json:"message,omitempty"`

	// Streaming text (when Phase == PhaseStreaming)
	TextDelta string `json:"text_delta,omitempty"`
	FullText  string `json:"full_text,omitempty"`

	// Tool activity (when Phase == PhaseToolCall)
	ToolName    string                 `json:"tool_name,omitempty"`
	ToolID      string                 `json:"tool_id,omitempty"`
	ToolInput   map[string]interface{} `json:"tool_input,omitempty"`
	ToolStarted bool                   `json:"tool_started,omitempty"` // true=start, false=complete
}

// FileEvent reports file operations detected from tool calls.
type FileEvent struct {
	Type      MessageType `json:"type"`
	RequestID string      `json:"request_id"`
	Agent     AgentType   `json:"agent"`
	Timestamp time.Time   `json:"timestamp"`

	Path     string     `json:"path"`
	Action   FileAction `json:"action"`
	ToolName string     `json:"tool_name"`
}

// CostUpdate reports accumulated cost during execution.
type CostUpdate struct {
	Type      MessageType `json:"type"`
	RequestID string      `json:"request_id"`
	Agent     AgentType   `json:"agent"`
	Timestamp time.Time   `json:"timestamp"`

	TurnCostUSD  float64 `json:"turn_cost_usd"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
}

// Result is the final response from a sub-agent.
type Result struct {
	Type      MessageType `json:"type"`
	RequestID string      `json:"request_id"`
	Agent     AgentType   `json:"agent"`
	Timestamp time.Time   `json:"timestamp"`

	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`

	// Full response text for parsing
	Text string `json:"text,omitempty"`

	// Parsed response (one of these, based on agent type)
	Design *protocol.DesignResponse `json:"design,omitempty"`
	Build  *protocol.BuildResponse  `json:"build,omitempty"`
	Review *protocol.ReviewResponse `json:"review,omitempty"`

	// Aggregated data
	FilesCreated  []string `json:"files_created"`
	FilesModified []string `json:"files_modified"`
	TotalCostUSD  float64  `json:"total_cost_usd"`
	DurationMs    int64    `json:"duration_ms"`
}

// Error reports an error during task execution.
type Error struct {
	Type      MessageType `json:"type"`
	RequestID string      `json:"request_id"`
	Agent     AgentType   `json:"agent"`
	Timestamp time.Time   `json:"timestamp"`

	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// NewProgress creates a new Progress event.
func NewProgress(requestID string, agent AgentType, phase Phase) *Progress {
	return &Progress{
		Type:      MessageTypeProgress,
		RequestID: requestID,
		Agent:     agent,
		Timestamp: time.Now(),
		Phase:     phase,
	}
}

// NewFileEvent creates a new FileEvent.
func NewFileEvent(requestID string, agent AgentType, path string, action FileAction, toolName string) *FileEvent {
	return &FileEvent{
		Type:      MessageTypeFileEvent,
		RequestID: requestID,
		Agent:     agent,
		Timestamp: time.Now(),
		Path:      path,
		Action:    action,
		ToolName:  toolName,
	}
}

// NewCostUpdate creates a new CostUpdate event.
func NewCostUpdate(requestID string, agent AgentType, turnCost, totalCost float64, inputTokens, outputTokens int) *CostUpdate {
	return &CostUpdate{
		Type:         MessageTypeCostUpdate,
		RequestID:    requestID,
		Agent:        agent,
		Timestamp:    time.Now(),
		TurnCostUSD:  turnCost,
		TotalCostUSD: totalCost,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}
}

// NewResult creates a new Result.
func NewResult(requestID string, agent AgentType, success bool, durationMs int64) *Result {
	return &Result{
		Type:          MessageTypeResult,
		RequestID:     requestID,
		Agent:         agent,
		Timestamp:     time.Now(),
		Success:       success,
		DurationMs:    durationMs,
		FilesCreated:  make([]string, 0),
		FilesModified: make([]string, 0),
	}
}

// NewError creates a new Error event.
func NewError(requestID string, agent AgentType, code, message string, retryable bool) *Error {
	return &Error{
		Type:      MessageTypeError,
		RequestID: requestID,
		Agent:     agent,
		Timestamp: time.Now(),
		Code:      code,
		Message:   message,
		Retryable: retryable,
	}
}
