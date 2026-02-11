// Package issue provides types and persistence for tracking CI failure issues.
package issue

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// FailureCategory classifies the type of CI failure.
type FailureCategory string

const (
	CategoryLintGo      FailureCategory = "lint/go"
	CategoryLintBazel   FailureCategory = "lint/bazel"
	CategoryLintTS      FailureCategory = "lint/ts"
	CategoryLintPython  FailureCategory = "lint/python"
	CategoryBuild       FailureCategory = "build"
	CategoryBuildDocker FailureCategory = "build/docker"
	CategoryTest        FailureCategory = "test"
	CategoryInfraDepbot FailureCategory = "infra/dependabot"
	CategoryInfraCI     FailureCategory = "infra/ci"
	CategoryUnknown     FailureCategory = "unknown"
)

// ValidCategories is the set of valid failure categories for LLM triage.
var ValidCategories = map[FailureCategory]bool{
	CategoryLintGo:      true,
	CategoryLintBazel:   true,
	CategoryLintTS:      true,
	CategoryLintPython:  true,
	CategoryBuild:       true,
	CategoryBuildDocker: true,
	CategoryTest:        true,
	CategoryInfraDepbot: true,
	CategoryInfraCI:     true,
	CategoryUnknown:     true,
}

// CIFailure represents a single categorized CI failure.
// This is the raw data structure returned by CI scanning/triage before being
// reconciled into tracked Issues.
type CIFailure struct {
	Timestamp time.Time
	RunURL    string
	HeadSHA   string
	Branch    string
	JobName   string
	Category  FailureCategory
	Signature string
	Summary   string
	Details   string
	File      string
	ErrorCode string
	RunID     int64
	Line      int
}

// ComputeSignature generates a stable dedup key for a failure.
// Format: {normalized-message-hash}:{file}
// When summary is empty, falls back to details, then job name.
// Job name is only used in the hash when it's the sole identifier
// (empty summary + empty details), so the same error across different
// jobs deduplicates correctly.
func ComputeSignature(category FailureCategory, file, summary, jobName, details string) string {
	msg := summary
	if msg == "" {
		msg = details
	}
	if msg == "" {
		// Only include job name when there's no other content to hash.
		msg = jobName
	}
	normalized := normalizeMessage(msg)
	h := sha256.Sum256([]byte(normalized))
	shortHash := fmt.Sprintf("%x", h[:8])
	canonFile := canonicalizePath(file)
	return fmt.Sprintf("%s:%s", shortHash, canonFile)
}

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
	FirstSeen     time.Time       `json:"first_seen"`
	LastSeen      time.Time       `json:"last_seen"`
	ResolvedAt    *time.Time      `json:"resolved_at,omitempty"`
	Category      FailureCategory `json:"category"`
	Summary       string          `json:"summary"`
	Details       string          `json:"details"`
	File          string          `json:"file"`
	Status        Status          `json:"status"`
	ID            string          `json:"id"`
	Signature     string          `json:"signature"`
	DismissReason string          `json:"dismiss_reason,omitempty"`
	ErrorCode     string          `json:"error_code,omitempty"`
	RunURL        string          `json:"run_url,omitempty"`
	JobName       string          `json:"job_name,omitempty"`
	FixAttempts   []FixAttempt    `json:"fix_attempts,omitempty"`
	Line          int             `json:"line,omitempty"`
	SeenCount     int             `json:"seen_count"`
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
