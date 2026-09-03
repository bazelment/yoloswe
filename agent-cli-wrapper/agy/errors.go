package agy

import "fmt"

var (
	ErrAlreadyStarted = fmt.Errorf("agy session already started")
	ErrNotStarted     = fmt.Errorf("agy session not started")
)

// CLINotFoundError is returned when the agy binary cannot be found.
type CLINotFoundError struct {
	Cause error
	Path  string
}

func (e *CLINotFoundError) Error() string {
	return fmt.Sprintf("agy CLI not found at %q: %v", e.Path, e.Cause)
}

func (e *CLINotFoundError) Unwrap() error { return e.Cause }

// ProcessError wraps a process startup or execution failure.
type ProcessError struct {
	Cause   error
	Message string
}

func (e *ProcessError) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Cause)
}

func (e *ProcessError) Unwrap() error { return e.Cause }

// ToolDeniedError reports an empty result after agy auto-denied a headless
// tool permission request.
type ToolDeniedError struct {
	Status string
	// AgyError is the result payload's own error field, preserved so a turn
	// that failed for an unrelated reason and also tripped a denial does not
	// lose agy's explanation of the failure.
	AgyError string
	Stderr   string
}

func (e *ToolDeniedError) Error() string {
	msg := fmt.Sprintf("agy: turn reported %q with an empty response after a tool permission was auto-denied in headless mode: %s", e.Status, e.Stderr)
	if e.AgyError != "" {
		msg += fmt.Sprintf(" (agy error: %s)", e.AgyError)
	}
	return msg
}
