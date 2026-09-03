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

// ToolDeniedError reports a turn that agy's own JSON result marks SUCCESS
// (exit 0, empty response) while its stderr carries the jetski headless-mode
// auto-denial marker. See isToolDeniedEmptyResult for why this is the only
// condition that promotes such a payload to an error, and why it cannot
// false-positive on a turn that legitimately produced no text.
type ToolDeniedError struct {
	// Stderr is the raw stderr captured for the denied turn, kept verbatim so
	// callers/logs can see which permission (read_file, command, ...) was
	// denied without re-parsing agy's message themselves.
	Stderr string
}

func (e *ToolDeniedError) Error() string {
	return fmt.Sprintf("agy: turn reported SUCCESS with an empty response after a tool permission was auto-denied in headless mode: %s", e.Stderr)
}
