package cursor

import (
	"errors"
	"fmt"
)

// Sentinel errors for common error conditions.
var (
	ErrAlreadyStarted = errors.New("session already started")
	ErrNotStarted     = errors.New("session not started")
	ErrSessionClosed  = errors.New("session is closed")
)

// ProtocolError represents a protocol-level error.
type ProtocolError struct {
	Cause   error
	Message string
	Line    string
}

func (e *ProtocolError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("protocol error: %s: %v", e.Message, e.Cause)
	}
	return fmt.Sprintf("protocol error: %s", e.Message)
}

func (e *ProtocolError) Unwrap() error {
	return e.Cause
}

// ProcessError represents a process-level error.
type ProcessError struct {
	Cause    error
	Message  string
	Stderr   string
	ExitCode int
}

func (e *ProcessError) Error() string {
	if e.ExitCode != 0 {
		return fmt.Sprintf("process error: %s (exit code %d)", e.Message, e.ExitCode)
	}
	return fmt.Sprintf("process error: %s", e.Message)
}

func (e *ProcessError) Unwrap() error {
	return e.Cause
}

// CLINotFoundError indicates the Cursor Agent CLI binary was not found.
type CLINotFoundError struct {
	Cause error
	Path  string
}

func (e *CLINotFoundError) Error() string {
	return fmt.Sprintf("CLI binary not found at %q: %v", e.Path, e.Cause)
}

func (e *CLINotFoundError) Unwrap() error {
	return e.Cause
}

// IsRecoverable returns true if the error is recoverable.
func IsRecoverable(err error) bool {
	if err == nil {
		return true
	}

	var procErr *ProcessError
	if errors.As(err, &procErr) {
		return false
	}

	var cliErr *CLINotFoundError
	if errors.As(err, &cliErr) {
		return false
	}

	if errors.Is(err, ErrSessionClosed) {
		return false
	}

	return true
}
