// Package session provides session management for the TUI application.
package session

import (
	"context"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/bramble/sessionmodel"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// SessionType distinguishes between planner and builder sessions.
type SessionType string

const (
	SessionTypePlanner SessionType = "planner"
	SessionTypeBuilder SessionType = "builder"
)

// Provider name aliases — canonical definitions are in multiagent/agent.
const (
	ProviderClaude = agent.ProviderClaude
	ProviderCodex  = agent.ProviderCodex
	ProviderGemini = agent.ProviderGemini
)

// AgentModel is an alias for agent.AgentModel.
type AgentModel = agent.AgentModel

// AvailableModels is the full unfiltered model list (alias).
var AvailableModels = agent.AllModels

// ModelByID looks up a model by ID from the full list (alias).
func ModelByID(id string) (AgentModel, bool) {
	return agent.ModelByID(id)
}

// NextModel cycles through the full unfiltered model list (alias).
// Prefer ModelRegistry.NextModel() for availability-filtered cycling.
func NextModel(currentID string) AgentModel {
	for i, m := range agent.AllModels {
		if m.ID == currentID {
			return agent.AllModels[(i+1)%len(agent.AllModels)]
		}
	}
	return agent.AllModels[0]
}

// --- Type aliases from sessionmodel (canonical definitions live there) ------

// SessionStatus represents the lifecycle state of a session.
type SessionStatus = sessionmodel.SessionStatus

// SessionID is a unique identifier for a session.
type SessionID = sessionmodel.SessionID

// ToolState represents the execution state of a tool.
type ToolState = sessionmodel.ToolState

// OutputLine represents a line of session output for display.
type OutputLine = sessionmodel.OutputLine

// OutputLineType categorises output lines.
type OutputLineType = sessionmodel.OutputLineType

// Re-export constants so existing callers don't need to change imports.
const (
	StatusPending   = sessionmodel.StatusPending
	StatusRunning   = sessionmodel.StatusRunning
	StatusIdle      = sessionmodel.StatusIdle
	StatusCompleted = sessionmodel.StatusCompleted
	StatusFailed    = sessionmodel.StatusFailed
	StatusStopped   = sessionmodel.StatusStopped

	ToolStateRunning  = sessionmodel.ToolStateRunning
	ToolStateComplete = sessionmodel.ToolStateComplete
	ToolStateError    = sessionmodel.ToolStateError

	OutputTypeText       = sessionmodel.OutputTypeText
	OutputTypeThinking   = sessionmodel.OutputTypeThinking
	OutputTypeTool       = sessionmodel.OutputTypeTool
	OutputTypeToolStart  = sessionmodel.OutputTypeToolStart
	OutputTypeToolResult = sessionmodel.OutputTypeToolResult
	OutputTypeError      = sessionmodel.OutputTypeError
	OutputTypeStatus     = sessionmodel.OutputTypeStatus
	OutputTypeTurnEnd    = sessionmodel.OutputTypeTurnEnd
	OutputTypePlanReady  = sessionmodel.OutputTypePlanReady
)

// DeepCopyOutputLine returns a deep copy of an OutputLine.
var DeepCopyOutputLine = sessionmodel.DeepCopyOutputLine

// Session represents a single plan or builder session.
type Session struct {
	CreatedAt      time.Time
	ctx            context.Context
	Error          error
	Progress       *SessionProgress
	StartedAt      *time.Time
	CompletedAt    *time.Time
	cancel         context.CancelFunc
	WorktreeName   string
	Prompt         string
	Title          string
	Model          string
	PlanFilePath   string // Path to plan file (planner sessions only)
	TmuxWindowName string // tmux window name (empty for TUI mode)
	TmuxWindowID   string // tmux window ID like @1, @2 (empty for TUI mode)
	RunnerType     string // "tui" or "tmux"
	ID             SessionID
	WorktreePath   string
	Status         SessionStatus
	Type           SessionType
	mu             sync.RWMutex
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
	CreatedAt      time.Time
	CompletedAt    *time.Time
	StartedAt      *time.Time
	WorktreePath   string
	WorktreeName   string
	Prompt         string
	Title          string
	Model          string
	PlanFilePath   string
	TmuxWindowName string // tmux window name (empty for TUI mode)
	TmuxWindowID   string // tmux window ID like @1, @2 (empty for TUI mode)
	RunnerType     string // "tui" or "tmux"
	ID             SessionID
	Status         SessionStatus
	Type           SessionType
	ErrorMsg       string
	Progress       SessionProgressSnapshot
}

// ToInfo converts a Session to SessionInfo for safe display.
func (s *Session) ToInfo() SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	info := SessionInfo{
		ID:             s.ID,
		Type:           s.Type,
		Status:         s.Status,
		WorktreePath:   s.WorktreePath,
		WorktreeName:   s.WorktreeName,
		Prompt:         s.Prompt,
		Title:          s.Title,
		Model:          s.Model,
		PlanFilePath:   s.PlanFilePath,
		TmuxWindowName: s.TmuxWindowName,
		TmuxWindowID:   s.TmuxWindowID,
		RunnerType:     s.RunnerType,
		CreatedAt:      s.CreatedAt,
		StartedAt:      s.StartedAt,
		CompletedAt:    s.CompletedAt,
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

// AppendStreamingDelta appends a new streaming delta while removing duplicated
// overlap between the end of the existing text and the start of the delta.
// This is used to accumulate streaming text/thinking deltas into a single
// OutputLine.Content without producing duplicate text at chunk boundaries.
func AppendStreamingDelta(existing, delta string) string {
	if existing == "" || delta == "" {
		return existing + delta
	}

	maxOverlap := len(existing)
	if len(delta) < maxOverlap {
		maxOverlap = len(delta)
	}

	for overlap := maxOverlap; overlap > 0; overlap-- {
		if existing[len(existing)-overlap:] == delta[:overlap] {
			return existing + delta[overlap:]
		}
	}

	return existing + delta
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


