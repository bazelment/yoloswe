package agent

import "context"

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
	SendMessage(ctx context.Context, message string) (*AgentResult, error)

	// TurnCount returns the number of turns completed.
	TurnCount() int
}

// EphemeralAgent is implemented by agents that create fresh sessions per task
// (Designer, Builder, Reviewer).
type EphemeralAgent interface {
	Agent

	// Execute runs a single task with a fresh session.
	// Returns the result, task ID (for logging), and any error.
	Execute(ctx context.Context, prompt string) (*AgentResult, string, error)

	// TaskCount returns the number of tasks executed.
	TaskCount() int
}
