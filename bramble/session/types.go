// Package session provides session management for the TUI application.
package session

import (
	"context"
	"sync"
	"time"
)

// SessionType distinguishes between planner and builder sessions.
type SessionType string

const (
	SessionTypePlanner SessionType = "planner"
	SessionTypeBuilder SessionType = "builder"
)

// SessionStatus represents the lifecycle state of a session.
type SessionStatus string

const (
	StatusPending   SessionStatus = "pending"
	StatusRunning   SessionStatus = "running"
	StatusIdle      SessionStatus = "idle"      // Waiting for follow-up input
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

// Session represents a single plan or builder session.
type Session struct {
	ID           SessionID
	Type         SessionType
	Status       SessionStatus
	WorktreePath string
	WorktreeName string
	Prompt       string
	Title        string // Short title derived from prompt
	Model        string // Model name (e.g. "sonnet")
	Progress     *SessionProgress
	CreatedAt    time.Time
	StartedAt    *time.Time
	CompletedAt  *time.Time
	Error        error

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.RWMutex
}

// SessionProgress tracks real-time progress.
type SessionProgress struct {
	CurrentPhase string  // "thinking", "tool_execution", "idle"
	CurrentTool  string  // Currently executing tool name
	TurnCount    int
	TotalCostUSD float64
	InputTokens  int
	OutputTokens int
	LastActivity time.Time
	StatusLine   string // Short status for display

	mu sync.RWMutex
}

// Update updates progress safely.
func (p *SessionProgress) Update(fn func(*SessionProgress)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(p)
}

// Clone returns a copy of the progress for safe reading.
func (p *SessionProgress) Clone() SessionProgress {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return SessionProgress{
		CurrentPhase: p.CurrentPhase,
		CurrentTool:  p.CurrentTool,
		TurnCount:    p.TurnCount,
		TotalCostUSD: p.TotalCostUSD,
		InputTokens:  p.InputTokens,
		OutputTokens: p.OutputTokens,
		LastActivity: p.LastActivity,
		StatusLine:   p.StatusLine,
	}
}

// SessionEvent wraps a claude event with session context.
type SessionEvent struct {
	SessionID SessionID
	Event     interface{} // claude.Event or custom events
}

// SessionInfo provides a snapshot of session state for display.
type SessionInfo struct {
	ID           SessionID
	Type         SessionType
	Status       SessionStatus
	WorktreePath string
	WorktreeName string
	Prompt       string
	Title        string
	Model        string
	Progress     SessionProgress
	CreatedAt    time.Time
	StartedAt    *time.Time
	CompletedAt  *time.Time
	ErrorMsg     string
}

// ToInfo converts a Session to SessionInfo for safe display.
func (s *Session) ToInfo() SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	info := SessionInfo{
		ID:           s.ID,
		Type:         s.Type,
		Status:       s.Status,
		WorktreePath: s.WorktreePath,
		WorktreeName: s.WorktreeName,
		Prompt:       s.Prompt,
		Title:        s.Title,
		Model:        s.Model,
		CreatedAt:    s.CreatedAt,
		StartedAt:    s.StartedAt,
		CompletedAt:  s.CompletedAt,
	}

	if s.Progress != nil {
		info.Progress = s.Progress.Clone()
	}

	if s.Error != nil {
		info.ErrorMsg = s.Error.Error()
	}

	return info
}

// ToolState represents the execution state of a tool.
type ToolState string

const (
	ToolStateRunning  ToolState = "running"
	ToolStateComplete ToolState = "complete"
	ToolStateError    ToolState = "error"
)

// OutputLine represents a line of session output for display.
type OutputLine struct {
	Timestamp time.Time
	Type      OutputLineType
	Content   string

	// Optional metadata for rich types
	ToolName   string                 `json:"tool_name,omitempty"`
	ToolID     string                 `json:"tool_id,omitempty"`
	ToolInput  map[string]interface{} `json:"tool_input,omitempty"`
	ToolResult interface{}            `json:"tool_result,omitempty"`
	IsError    bool                   `json:"is_error,omitempty"`
	TurnNumber int                    `json:"turn_number,omitempty"`
	CostUSD    float64                `json:"cost_usd,omitempty"`
	DurationMs int64                  `json:"duration_ms,omitempty"`

	// Tool execution state for real-time updates
	ToolState ToolState `json:"tool_state,omitempty"`
	StartTime time.Time `json:"start_time,omitempty"`
}

// OutputLineType categorizes output lines.
type OutputLineType string

const (
	OutputTypeText       OutputLineType = "text"
	OutputTypeThinking   OutputLineType = "thinking"
	OutputTypeTool       OutputLineType = "tool"       // Legacy - kept for backward compat
	OutputTypeToolStart  OutputLineType = "tool_start" // Tool invocation beginning
	OutputTypeToolResult OutputLineType = "tool_result"
	OutputTypeError      OutputLineType = "error"
	OutputTypeStatus     OutputLineType = "status"
	OutputTypeTurnEnd    OutputLineType = "turn_end"
)

// SessionOutputEvent is sent when session produces output.
type SessionOutputEvent struct {
	SessionID SessionID
	Line      OutputLine
}

// SessionStateChangeEvent is sent when session state changes.
type SessionStateChangeEvent struct {
	SessionID SessionID
	OldStatus SessionStatus
	NewStatus SessionStatus
}
