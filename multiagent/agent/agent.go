package agent

import (
	"context"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// Agent is the base interface for all agents.
type Agent interface {
	// Role returns the agent's role.
	Role() AgentRole

	// SessionDir returns the directory where session recordings are stored.
	SessionDir() string

	// TotalCost returns the accumulated cost in USD.
	TotalCost() float64
}

// LongRunningAgent is implemented by agents that maintain persistent sessions
// (Orchestrator and Planner).
type LongRunningAgent interface {
	Agent

	// Start initializes and starts the agent's session.
	Start(ctx context.Context) error

	// Stop gracefully shuts down the agent's session.
	Stop() error

	// SendMessage sends a message to the agent and waits for completion.
	SendMessage(ctx context.Context, message string) (*claude.TurnResult, error)

	// TurnCount returns the number of turns completed.
	TurnCount() int
}

// EphemeralAgent is implemented by agents that create fresh sessions per task
// (Designer, Builder, Reviewer).
type EphemeralAgent interface {
	Agent

	// Execute runs a single task with a fresh session.
	// Returns the result, task ID (for logging), and any error.
	Execute(ctx context.Context, prompt string) (*claude.TurnResult, string, error)

	// TaskCount returns the number of tasks executed.
	TaskCount() int
}

// TurnResult wraps claude.TurnResult with additional context.
type TurnResult struct {
	*claude.TurnResult

	// Text is the accumulated text response.
	Text string

	// AgentRole identifies which agent produced this result.
	AgentRole AgentRole

	// TaskID is set for ephemeral agents to identify the task.
	TaskID string
}

// ExtractText extracts the text content from a TurnResult.
// This is a helper since the SDK's TurnResult doesn't directly expose accumulated text.
func ExtractText(result *claude.TurnResult) string {
	// The SDK accumulates text in the turn, but we need to access it via recording
	// For now, we'll rely on the caller to collect text from events
	// This is a placeholder - actual implementation depends on SDK internals
	return ""
}
