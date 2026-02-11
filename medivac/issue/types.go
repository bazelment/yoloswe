// Package issue provides types and persistence for tracking CI failure issues.
package issue

import (
	"time"

	"github.com/bazelment/yoloswe/medivac/github"
)

// Status represents the lifecycle state of an issue.
type Status string

const (
	StatusNew         Status = "new"
	StatusInProgress  Status = "in_progress"
	StatusFixPending  Status = "fix_pending"
	StatusFixApproved Status = "fix_approved"
	StatusFixMerged   Status = "fix_merged"
	StatusVerified    Status = "verified"
	StatusRecurred    Status = "recurred"
	StatusWontFix     Status = "wont_fix"
)

// Issue represents a tracked CI failure.
type Issue struct {
	FirstSeen     time.Time              `json:"first_seen"`
	LastSeen      time.Time              `json:"last_seen"`
	ResolvedAt    *time.Time             `json:"resolved_at,omitempty"`
	Category      github.FailureCategory `json:"category"`
	Summary       string                 `json:"summary"`
	Details       string                 `json:"details"`
	File          string                 `json:"file"`
	Status        Status                 `json:"status"`
	ID            string                 `json:"id"`
	Signature     string                 `json:"signature"`
	DismissReason string                 `json:"dismiss_reason,omitempty"`
	ErrorCode     string                 `json:"error_code,omitempty"`
	FixAttempts   []FixAttempt           `json:"fix_attempts,omitempty"`
	Line          int                    `json:"line,omitempty"`
	SeenCount     int                    `json:"seen_count"`
}

// FixOption describes one possible approach to fix an issue.
type FixOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// FixAttempt records a single attempt to fix an issue.
type FixAttempt struct {
	StartedAt   time.Time   `json:"started_at"`
	CompletedAt *time.Time  `json:"completed_at,omitempty"`
	Branch      string      `json:"branch"`
	PRURL       string      `json:"pr_url,omitempty"`
	PRState     string      `json:"pr_state,omitempty"`
	PRReview    string      `json:"pr_review,omitempty"`
	Outcome     string      `json:"outcome,omitempty"`
	Error       string      `json:"error,omitempty"`
	Reasoning   string      `json:"reasoning,omitempty"`
	RootCause   string      `json:"root_cause,omitempty"`
	LogFile     string      `json:"log_file,omitempty"`
	FixOptions  []FixOption `json:"fix_options,omitempty"`
	AgentCost   float64     `json:"agent_cost,omitempty"`
	PRNumber    int         `json:"pr_number,omitempty"`
}
