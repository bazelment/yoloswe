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
	StatusIdle      SessionStatus = "idle" // Waiting for follow-up input
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
	CreatedAt    time.Time
	ctx          context.Context
	Error        error
	Progress     *SessionProgress
	StartedAt    *time.Time
	CompletedAt  *time.Time
	cancel       context.CancelFunc
	WorktreeName string
	Prompt       string
	Title        string
	Model        string
	PlanFilePath string // Path to plan file (planner sessions only)
	ID           SessionID
	WorktreePath string
	Status       SessionStatus
	Type         SessionType
	mu           sync.RWMutex
}

// SessionProgress tracks real-time progress.
type SessionProgress struct {
	LastActivity time.Time
	CurrentPhase string
	CurrentTool  string
	StatusLine   string
	TurnCount    int
	TotalCostUSD float64
	InputTokens  int
	OutputTokens int
	mu           sync.RWMutex
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
	Event     interface{}
	SessionID SessionID
}

// SessionProgressSnapshot is a mutex-free copy of SessionProgress for display.
type SessionProgressSnapshot struct {
	LastActivity time.Time
	CurrentPhase string
	CurrentTool  string
	StatusLine   string
	TurnCount    int
	TotalCostUSD float64
	InputTokens  int
	OutputTokens int
}

// SessionInfo provides a snapshot of session state for display.
type SessionInfo struct {
	CreatedAt    time.Time
	CompletedAt  *time.Time
	StartedAt    *time.Time
	WorktreePath string
	WorktreeName string
	Prompt       string
	Title        string
	Model        string
	PlanFilePath string
	ID           SessionID
	Status       SessionStatus
	Type         SessionType
	ErrorMsg     string
	Progress     SessionProgressSnapshot
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
		PlanFilePath: s.PlanFilePath,
		CreatedAt:    s.CreatedAt,
		StartedAt:    s.StartedAt,
		CompletedAt:  s.CompletedAt,
	}

	if s.Progress != nil {
		p := s.Progress.Clone()
		info.Progress = SessionProgressSnapshot{
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
	StartTime  time.Time `json:"start_time,omitempty"`
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
	OutputTypePlanReady  OutputLineType = "plan_ready" // Plan file content for rendering
)

// deepCopyOutputLine returns a deep copy of an OutputLine, cloning mutable
// fields (ToolInput map, ToolResult) so that the caller's copy is independent.
func deepCopyOutputLine(line OutputLine) OutputLine {
	if line.ToolInput != nil {
		newInput := make(map[string]interface{}, len(line.ToolInput))
		for k, v := range line.ToolInput {
			newInput[k] = v
		}
		line.ToolInput = newInput
	}
	// ToolResult is an interface{} — typically a string or JSON-decoded value.
	// A shallow copy is sufficient for immutable types; map values are not
	// mutated after construction so this is safe in practice.
	return line
}

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
