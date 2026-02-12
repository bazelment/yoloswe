package acp

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

	// ErrSessionNotFound is returned when a session ID is not found.
	ErrSessionNotFound = errors.New("session not found")

	// ErrNoTurnInProgress is returned when waiting for a turn but none is active.
	ErrNoTurnInProgress = errors.New("no turn in progress")

	// ErrInvalidState is returned for invalid state transitions.
	ErrInvalidState = errors.New("invalid state transition")
)

// RPCError represents a JSON-RPC error from the agent.
type RPCError struct {
	Message string
	Code    int
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// ProcessError represents an error with the agent subprocess.
type ProcessError struct {
	Cause    error
	Message  string
	ExitCode int
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
	Cause   error
	Message string
	Line    string
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
	SessionID string
	Message   string
}

func (e *TurnError) Error() string {
	return fmt.Sprintf("turn error (session=%s): %s", e.SessionID, e.Message)
}
