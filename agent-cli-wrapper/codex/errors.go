package codex

import (
	"errors"
	"fmt"
)

// Sentinel errors for common error conditions.
var (
	// ErrAlreadyStarted is returned when Start() is called on an already started client.
	ErrAlreadyStarted = errors.New("client already started")

	// ErrNotStarted is returned when an operation requires a started client.
	ErrNotStarted = errors.New("client not started")

	// ErrStopping is returned when an operation is attempted while the client is stopping.
	ErrStopping = errors.New("client is stopping")

	// ErrClientClosed is returned when an operation is attempted on a closed client.
	ErrClientClosed = errors.New("client is closed")

	// ErrThreadNotFound is returned when a thread ID is not found.
	ErrThreadNotFound = errors.New("thread not found")

	// ErrThreadNotReady is returned when a thread is not ready for turns.
	ErrThreadNotReady = errors.New("thread not ready for turns")

	// ErrTurnInProgress is returned when a turn is already in progress.
	ErrTurnInProgress = errors.New("turn already in progress")

	// ErrNoTurnInProgress is returned when waiting for a turn but none is active.
	ErrNoTurnInProgress = errors.New("no turn in progress")

	// ErrTimeout is returned when an operation times out.
	ErrTimeout = errors.New("operation timed out")

	// ErrInvalidState is returned for invalid state transitions.
	ErrInvalidState = errors.New("invalid state transition")
)

// RPCError represents a JSON-RPC error from the app-server.
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// ProcessError represents an error with the app-server subprocess.
type ProcessError struct {
	Message  string
	ExitCode int
	Stderr   string
	Cause    error
}

func (e *ProcessError) Error() string {
	if e.ExitCode != 0 {
		return fmt.Sprintf("%s (exit code %d)", e.Message, e.ExitCode)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *ProcessError) Unwrap() error {
	return e.Cause
}

// ProtocolError represents a protocol-level error (e.g., malformed JSON).
type ProtocolError struct {
	Message string
	Line    string
	Cause   error
}

func (e *ProtocolError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *ProtocolError) Unwrap() error {
	return e.Cause
}

// TurnError represents an error that occurred during a turn.
type TurnError struct {
	ThreadID string
	TurnID   string
	Message  string
	Cause    error
}

func (e *TurnError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("turn error (thread=%s, turn=%s): %s: %v", e.ThreadID, e.TurnID, e.Message, e.Cause)
	}
	return fmt.Sprintf("turn error (thread=%s, turn=%s): %s", e.ThreadID, e.TurnID, e.Message)
}

func (e *TurnError) Unwrap() error {
	return e.Cause
}
