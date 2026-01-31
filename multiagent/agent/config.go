// Package agent provides core types and interfaces for the multi-agent system.
package agent

import "time"

// AgentRole identifies the type of agent.
type AgentRole string

const (
	RoleOrchestrator AgentRole = "orchestrator"
	RolePlanner      AgentRole = "planner"
	RoleDesigner     AgentRole = "designer"
	RoleBuilder      AgentRole = "builder"
	RoleReviewer     AgentRole = "reviewer"
)

// String returns the string representation of the role.
func (r AgentRole) String() string {
	return string(r)
}

// IsLongRunning returns true if this role uses a long-running session.
func (r AgentRole) IsLongRunning() bool {
	return r == RoleOrchestrator || r == RolePlanner
}

// AgentConfig configures an agent instance.
type AgentConfig struct {
	// Role identifies the agent type.
	Role AgentRole

	// Model specifies the Claude model to use (e.g., "haiku", "sonnet", "opus").
	Model string

	// SystemPrompt is the system prompt for this agent.
	SystemPrompt string

	// WorkDir is the working directory for file operations.
	WorkDir string

	// SessionDir is the directory for storing session recordings.
	// For long-running agents: SessionDir/role/
	// For ephemeral agents: SessionDir/role/task-xxx/
	SessionDir string

	// MaxTurnsPerTask limits turns for ephemeral agents (prevents runaway).
	MaxTurnsPerTask int

	// TurnTimeout is the maximum time to wait for a turn to complete.
	TurnTimeout time.Duration

	// BudgetUSD is the cost budget for this agent (0 = unlimited).
	BudgetUSD float64

	// TaskTimeout is the maximum time for a single task execution (0 = unlimited).
	// Only applies to ephemeral agents (Designer, Builder, Reviewer).
	TaskTimeout time.Duration
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig(role AgentRole) AgentConfig {
	return AgentConfig{
		Role:            role,
		Model:           "sonnet",
		WorkDir:         ".",
		SessionDir:      ".claude-swarm/sessions",
		MaxTurnsPerTask: 10,
		TurnTimeout:     5 * time.Minute,
		BudgetUSD:       0, // unlimited
		TaskTimeout:     0, // unlimited
	}
}

// ProgressReporter is an interface for progress reporting.
// This is duplicated here to avoid import cycles with the progress package.
type ProgressReporter interface {
	Event(event interface{})
	Close()
}

// SwarmConfig configures the entire agent swarm.
type SwarmConfig struct {
	// Progress receives progress events during execution.
	Progress ProgressReporter

	// SessionID uniquely identifies this swarm session.
	SessionID string

	// WorkDir is the root working directory.
	WorkDir string

	// SessionDir is where all session recordings are stored.
	SessionDir string

	// Models for each agent role.
	OrchestratorModel string
	PlannerModel      string
	DesignerModel     string
	BuilderModel      string
	ReviewerModel     string

	// TotalBudgetUSD is the total cost budget across all agents.
	TotalBudgetUSD float64

	// MaxIterations prevents infinite loops.
	MaxIterations int

	// EnableCheckpointing enables session state persistence for error recovery.
	EnableCheckpointing bool

	// MissionTimeout is the maximum time for the entire mission (0 = unlimited).
	MissionTimeout time.Duration

	// HeartbeatInterval is how often to emit heartbeat events during long operations (0 = no heartbeats).
	HeartbeatInterval time.Duration

	// StallTimeout is how long without progress before considering the mission stalled (0 = no stall detection).
	StallTimeout time.Duration
}

// DefaultSwarmConfig returns a swarm config with sensible defaults.
func DefaultSwarmConfig() SwarmConfig {
	return SwarmConfig{
		WorkDir:             ".",
		SessionDir:          ".claude-swarm/sessions",
		OrchestratorModel:   "sonnet",
		PlannerModel:        "sonnet",
		DesignerModel:       "sonnet",
		BuilderModel:        "sonnet",
		ReviewerModel:       "haiku",
		TotalBudgetUSD:      1.0,
		MaxIterations:       50,
		EnableCheckpointing: true,
		MissionTimeout:      0,                // unlimited
		HeartbeatInterval:   30 * time.Second, // emit heartbeat every 30s
		StallTimeout:        10 * time.Minute, // warn if no progress for 10 minutes
	}
}
