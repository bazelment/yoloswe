// Package model defines the core domain entities for Symphony.
// All types here correspond to the spec's Section 4: Core Domain Model.
package model

import "time"

// Issue is the normalized issue record used by orchestration, prompt rendering,
// and observability. Spec Section 4.1.1.
type Issue struct {
	Description *string
	Priority    *int
	BranchName  *string
	URL         *string
	CreatedAt   *time.Time
	UpdatedAt   *time.Time
	ID          string
	Identifier  string
	Title       string
	State       string
	Labels      []string
	BlockedBy   []BlockerRef
}

// BlockerRef is a reference to a blocking issue. Spec Section 4.1.1.
type BlockerRef struct {
	ID         *string
	Identifier *string
	State      *string
}

// WorkflowDefinition is the parsed WORKFLOW.md payload. Spec Section 4.1.2.
type WorkflowDefinition struct {
	Config         map[string]any
	PromptTemplate string
}

// Workspace represents a filesystem workspace assigned to one issue. Spec Section 4.1.4.
type Workspace struct {
	Path         string
	WorkspaceKey string
	CreatedNow   bool
}

// RunAttempt tracks one execution attempt for one issue. Spec Section 4.1.5.
type RunAttempt struct {
	IssueID         string
	IssueIdentifier string
	Attempt         *int
	WorkspacePath   string
	StartedAt       time.Time
	Status          RunStatus
	Error           string
}

// RunStatus represents the status of a run attempt. Spec Section 7.2.
type RunStatus string

const (
	RunStatusPreparingWorkspace  RunStatus = "preparing_workspace"
	RunStatusBuildingPrompt      RunStatus = "building_prompt"
	RunStatusLaunchingAgent      RunStatus = "launching_agent"
	RunStatusInitializingSession RunStatus = "initializing_session"
	RunStatusStreamingTurn       RunStatus = "streaming_turn"
	RunStatusFinishing           RunStatus = "finishing"
	RunStatusSucceeded           RunStatus = "succeeded"
	RunStatusFailed              RunStatus = "failed"
	RunStatusTimedOut            RunStatus = "timed_out"
	RunStatusStalled             RunStatus = "stalled"
	RunStatusCanceledByReconcile RunStatus = "canceled_by_reconciliation"
)

// LiveSession tracks state while a coding-agent subprocess is running. Spec Section 4.1.6.
type LiveSession struct {
	SessionID              string
	ThreadID               string
	TurnID                 string
	AgentPID               *string
	LastAgentEvent         *string
	LastAgentTimestamp     *time.Time
	LastAgentMessage       string
	InputTokens            int64
	OutputTokens           int64
	TotalTokens            int64
	LastReportedInputToks  int64
	LastReportedOutputToks int64
	LastReportedTotalToks  int64
	TurnCount              int
}

// RetryEntry is the scheduled retry state for an issue. Spec Section 4.1.7.
type RetryEntry struct {
	IssueID    string
	Identifier string
	Error      string
	Attempt    int
	DueAtMs    int64
	Generation uint64
}

// AgentTotals holds aggregate token and runtime totals. Spec Section 13.5.
type AgentTotals struct {
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	SecondsRunning float64
}

// RunningEntry tracks a single running issue in the orchestrator. Spec Section 16.4.
type RunningEntry struct {
	Issue        Issue
	StartedAt    time.Time
	Identifier   string
	Session      LiveSession
	RetryAttempt int
}

// ExitReason describes why a worker exited.
type ExitReason string

const (
	ExitReasonNormal   ExitReason = "normal"
	ExitReasonFailed   ExitReason = "failed"
	ExitReasonTimedOut ExitReason = "timed_out"
	ExitReasonStalled  ExitReason = "stalled"
	ExitReasonCanceled ExitReason = "canceled"
	// ExitReasonInactive means the issue left active states while the worker was
	// running (e.g. moved to Done/Cancelled). The orchestrator should release
	// the claim without scheduling a continuation.
	ExitReasonInactive ExitReason = "inactive"
)
