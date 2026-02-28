// Package sessionmodel provides the core data model for bramble TUI sessions.
// It implements a clean MVC layering: envelope strippers normalize any wire
// format into the common Claude protocol vocabulary, a single MessageParser
// converts vocabulary messages into model mutations, and SessionModel is the
// single source of truth read by the View.
package sessionmodel

import (
	"encoding/json"
	"time"
)

// --- Output types (canonical definitions) ------------------------------------

// OutputLineType categorises output lines.
type OutputLineType string

const (
	OutputTypeText       OutputLineType = "text"
	OutputTypeThinking   OutputLineType = "thinking"
	OutputTypeTool       OutputLineType = "tool"       // Legacy
	OutputTypeToolStart  OutputLineType = "tool_start"
	OutputTypeToolResult OutputLineType = "tool_result"
	OutputTypeError      OutputLineType = "error"
	OutputTypeStatus     OutputLineType = "status"
	OutputTypeTurnEnd    OutputLineType = "turn_end"
	OutputTypePlanReady  OutputLineType = "plan_ready"
)

// ToolState represents the execution state of a tool.
type ToolState string

const (
	ToolStateRunning  ToolState = "running"
	ToolStateComplete ToolState = "complete"
	ToolStateError    ToolState = "error"
)

// OutputLine represents a line of session output for display.
type OutputLine struct {
	StartTime  time.Time              `json:"start_time,omitempty"`
	Timestamp  time.Time
	ToolResult interface{}            `json:"tool_result,omitempty"`
	ToolInput  map[string]interface{} `json:"tool_input,omitempty"`
	ToolName   string                 `json:"tool_name,omitempty"`
	ToolID     string                 `json:"tool_id,omitempty"`
	Content    string
	ToolState  ToolState `json:"tool_state,omitempty"`
	Type       OutputLineType
	TurnNumber int     `json:"turn_number,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	DurationMs int64   `json:"duration_ms,omitempty"`
	IsError    bool    `json:"is_error,omitempty"`
}

// DeepCopyOutputLine returns a deep copy of an OutputLine, cloning mutable
// fields (ToolInput map, ToolResult) so that the caller's copy is independent.
func DeepCopyOutputLine(line OutputLine) OutputLine {
	if line.ToolInput != nil {
		newInput := make(map[string]interface{}, len(line.ToolInput))
		for k, v := range line.ToolInput {
			newInput[k] = v
		}
		line.ToolInput = newInput
	}
	line.ToolResult = deepCopyInterface(line.ToolResult)
	return line
}

// deepCopyInterface shallow-clones the mutable container types that JSON
// unmarshalling produces (map[string]interface{}, []interface{}).
// Strings, numbers, bools, and nil are immutable and returned as-is.
func deepCopyInterface(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		cp := make(map[string]interface{}, len(val))
		for k, v := range val {
			cp[k] = deepCopyInterface(v)
		}
		return cp
	case []interface{}:
		cp := make([]interface{}, len(val))
		for i, v := range val {
			cp[i] = deepCopyInterface(v)
		}
		return cp
	default:
		return v
	}
}

// --- Session lifecycle types ------------------------------------------------

// SessionStatus represents the lifecycle state of a session.
type SessionStatus string

const (
	StatusPending   SessionStatus = "pending"
	StatusRunning   SessionStatus = "running"
	StatusIdle      SessionStatus = "idle"
	StatusCompleted SessionStatus = "completed"
	StatusFailed    SessionStatus = "failed"
	StatusStopped   SessionStatus = "stopped"
)

// IsTerminal returns true if the status is a terminal state.
func (s SessionStatus) IsTerminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusStopped
}

// CanAcceptInput returns true if the session can accept user input.
func (s SessionStatus) CanAcceptInput() bool {
	return s == StatusIdle
}

// SessionID is a unique identifier for a session.
type SessionID string

// --- Session metadata -------------------------------------------------------

// SessionMeta holds metadata extracted from system{init} and envelope fields.
type SessionMeta struct {
	SessionID         string
	Model             string
	CWD               string
	ClaudeCodeVersion string
	PermissionMode    string
	GitBranch         string
	Tools             []string
	Agents            []string
	Skills            []string
	Status            SessionStatus
}

// ToolUseResultMeta captures tool execution details from raw JSONL envelopes.
type ToolUseResultMeta struct {
	Stdout      string `json:"stdout,omitempty"`
	Stderr      string `json:"stderr,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
}

// TurnUsage tracks token/cost usage for a single turn.
type TurnUsage struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// RawEnvelopeMeta holds metadata from raw JSONL (~/.claude/projects/) envelopes.
type RawEnvelopeMeta struct {
	Timestamp     time.Time
	ToolUseResult json.RawMessage
	Data          json.RawMessage // progress data payload
	ErrorJSON     json.RawMessage // system error payload
	ParentUUID    string
	GitBranch     string
	Version       string
	UUID          string
	SessionID     string // outer envelope sessionId (used as fallback when inner message lacks it)
	Type          string // envelope type: system, progress, pr-link, etc.
	Subtype       string // system subtype: api_error, compact_boundary, etc.
	Content       string // text content from system/queue-operation envelopes
	Operation     string // queue-operation operation type
	PRURL         string // pr-link URL
	PRRepository  string // pr-link repository
	PRNumber      int    // pr-link PR number
	DurationMs    int64  // system/turn_duration milliseconds
	IsSidechain   bool
}

// --- Progress ---------------------------------------------------------------

// ProgressSnapshot is a mutex-free snapshot of session progress metrics.
// It mirrors session.SessionProgress but without the embedded mutex, making it
// safe to copy and use as a value.
type ProgressSnapshot struct {
	LastActivity time.Time
	CurrentPhase string
	CurrentTool  string
	StatusLine   string
	TurnCount    int
	TotalCostUSD float64
	InputTokens  int
	OutputTokens int
}

// --- Observer / ModelEvent --------------------------------------------------

// Observer receives notifications when the model mutates.
type Observer interface {
	OnModelEvent(event ModelEvent)
}

// ModelEvent is the interface for model mutation notifications.
type ModelEvent interface {
	modelEvent() // sealed marker
}

// OutputAppended fires when a new output line is added or an existing line is
// updated (e.g. streaming text append, tool state change).
type OutputAppended struct{}

func (OutputAppended) modelEvent() {}

// StatusChanged fires when the session status changes.
type StatusChanged struct {
	Old, New SessionStatus
}

func (StatusChanged) modelEvent() {}

// ProgressUpdated fires when session progress is updated.
type ProgressUpdated struct{}

func (ProgressUpdated) modelEvent() {}

// MetaUpdated fires when session metadata is set or changed.
type MetaUpdated struct{}

func (MetaUpdated) modelEvent() {}
