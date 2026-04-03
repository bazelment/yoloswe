// Package agent defines the Agent interface and shared types for symphony's
// pluggable agent system. Concrete implementations live in subpackages
// (e.g. agent/codex).
package agent

import "context"

// Agent is the interface that all agent session types must implement.
// The orchestrator depends only on this interface, never on concrete sessions.
type Agent interface {
	// RunTurn starts a turn with the given prompt and streams events via onEvent
	// until the turn completes, fails, or times out.
	RunTurn(ctx context.Context, prompt string, onEvent func(Event)) (TurnResult, error)

	// Stop gracefully stops the agent session.
	Stop() error

	// ThreadID returns the current thread/conversation ID.
	ThreadID() string

	// SessionID returns the composed session ID.
	SessionID() string

	// PID returns the process ID of the agent subprocess, or nil if not applicable.
	PID() *int
}

// SessionConfig holds configuration for creating an agent session.
type SessionConfig struct {
	Type              string // Agent type (e.g. "codex"). Empty defaults to "codex".
	Command           string
	WorkDir           string
	ApprovalPolicy    string
	ThreadSandbox     string
	TurnSandboxPolicy string
	IssueIdentifier   string
	IssueTitle        string
	TurnTimeoutMs     int
	ReadTimeoutMs     int
}

// TurnStatus represents the outcome of a turn.
type TurnStatus string

const (
	TurnCompleted TurnStatus = "completed"
	TurnFailed    TurnStatus = "failed"
	TurnTimedOut  TurnStatus = "timed_out"
	TurnCancelled TurnStatus = "cancelled"
)

// TurnResult holds the outcome of a turn.
type TurnResult struct {
	Error  error
	Status TurnStatus
}
