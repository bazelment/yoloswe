// Package tracker defines the issue-tracker adapter interface and factory.
// Spec Section 11.1.
package tracker

import (
	"context"

	"github.com/bazelment/yoloswe/symphony/model"
	"github.com/bazelment/yoloswe/symphony/tracker/linear"
)

// Tracker is the read-only issue-tracker adapter contract.
// Symphony is a scheduler/runner and tracker reader; ticket writes
// are handled by the coding agent (Spec Section 11.5).
type Tracker interface {
	// FetchCandidateIssues returns issues in the given active states
	// for the specified project slug. Used for dispatch.
	FetchCandidateIssues(ctx context.Context, activeStates []string, projectSlug string) ([]model.Issue, error)

	// FetchIssueStatesByIDs returns the current state of the given
	// issue IDs. Used for active-run reconciliation.
	FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]model.Issue, error)

	// FetchIssuesByStates returns issues in the given state names.
	// Used for startup terminal cleanup.
	FetchIssuesByStates(ctx context.Context, states []string) ([]model.Issue, error)
}

// New creates a Tracker for the given kind.
// Supported kinds: "linear".
func New(kind, endpoint, apiKey string) (Tracker, error) {
	switch kind {
	case "linear":
		if apiKey == "" {
			return nil, &linear.Error{
				Category: linear.ErrMissingTrackerAPIKey,
				Message:  "api key is required for Linear tracker",
			}
		}
		return linear.NewClient(endpoint, apiKey), nil
	default:
		return nil, &linear.Error{
			Category: linear.ErrUnsupportedTrackerKind,
			Message:  "unsupported tracker kind: " + kind,
		}
	}
}
