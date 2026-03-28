// Package linear implements the Linear GraphQL issue-tracker adapter.
package linear

import "fmt"

// ErrorCategory classifies tracker errors per Spec Section 11.4.
type ErrorCategory string

const (
	ErrUnsupportedTrackerKind ErrorCategory = "unsupported_tracker_kind"
	ErrMissingTrackerAPIKey   ErrorCategory = "missing_tracker_api_key"
	ErrMissingTrackerProjSlug ErrorCategory = "missing_tracker_project_slug"
	ErrLinearAPIRequest       ErrorCategory = "linear_api_request"
	ErrLinearAPIStatus        ErrorCategory = "linear_api_status"
	ErrLinearGraphQLErrors    ErrorCategory = "linear_graphql_errors"
	ErrLinearUnknownPayload   ErrorCategory = "linear_unknown_payload"
	ErrLinearMissingEndCursor ErrorCategory = "linear_missing_end_cursor"
)

// Error is a typed tracker error with a category for programmatic handling.
type Error struct {
	Cause    error
	Category ErrorCategory
	Message  string
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Category, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Category, e.Message)
}

func (e *Error) Unwrap() error {
	return e.Cause
}
