// Package tracker defines the pluggable issue-tracker interface for jiradozer.
// Linear is the first implementation; GitHub Issues, Jira, etc. can follow.
package tracker

import (
	"context"
	"time"
)

// IssueTracker is the read+write interface for issue tracking systems.
type IssueTracker interface {
	// FetchIssue returns an issue by its human-readable identifier (e.g. "ENG-123").
	FetchIssue(ctx context.Context, identifier string) (*Issue, error)

	// FetchComments returns comments on an issue created after the given time.
	// Comments are returned in chronological order.
	FetchComments(ctx context.Context, issueID string, since time.Time) ([]Comment, error)

	// FetchWorkflowStates returns available workflow states for the given team.
	FetchWorkflowStates(ctx context.Context, teamID string) ([]WorkflowState, error)

	// PostComment creates a new comment on an issue.
	PostComment(ctx context.Context, issueID string, body string) error

	// UpdateIssueState transitions an issue to the given workflow state.
	UpdateIssueState(ctx context.Context, issueID string, stateID string) error
}
