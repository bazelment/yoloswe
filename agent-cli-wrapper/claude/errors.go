package claude

import (
	"errors"
	"fmt"
)

// Sentinel errors for common error conditions.
var (
	ErrAlreadyStarted     = errors.New("session already started")
	ErrNotStarted         = errors.New("session not started")
	ErrStopping           = errors.New("session is stopping")
	ErrSessionClosed      = errors.New("session is closed")
	ErrTimeout            = errors.New("operation timed out")
	ErrProcessExited      = errors.New("CLI process exited unexpectedly")
	ErrInvalidState       = errors.New("invalid state transition")
	ErrPermissionDenied   = errors.New("permission denied")
	ErrBudgetExceeded     = errors.New("budget limit exceeded")
	ErrMaxTurnsExceeded   = errors.New("max turns exceeded")
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

// TurnError represents an error during turn execution.
type TurnError struct {
	Cause      error
	Message    string
	TurnNumber int
}

func (e *TurnError) Error() string {
	return fmt.Sprintf("turn %d error: %s", e.TurnNumber, e.Message)
}

func (e *TurnError) Unwrap() error {
	return e.Cause
}

// CLINotFoundError indicates the Claude CLI binary was not found.
type CLINotFoundError struct {
	Path  string
	Cause error
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

	// Process exit errors are not recoverable
	var procErr *ProcessError
	if errors.As(err, &procErr) {
		return false
	}

	// CLI not found is not recoverable
	var cliErr *CLINotFoundError
	if errors.As(err, &cliErr) {
		return false
	}

	// Session closed is not recoverable
	if errors.Is(err, ErrSessionClosed) {
		return false
	}

	// Most other errors are recoverable
	return true
}
