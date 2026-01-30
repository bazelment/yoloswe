package codex

import (
	"errors"
	"testing"
)

func TestRPCError(t *testing.T) {
	err := &RPCError{
		Code:    -32600,
		Message: "Invalid Request",
	}

	expected := "rpc error -32600: Invalid Request"
	if err.Error() != expected {
		t.Errorf("expected %q, got %q", expected, err.Error())
	}
}

func TestProcessError(t *testing.T) {
	tests := []struct {
		name     string
		err      *ProcessError
		expected string
	}{
		{
			name: "with exit code",
			err: &ProcessError{
				Message:  "process failed",
				ExitCode: 1,
			},
			expected: "process failed (exit code 1)",
		},
		{
			name: "with cause",
			err: &ProcessError{
				Message: "process failed",
				Cause:   errors.New("underlying error"),
			},
			expected: "process failed: underlying error",
		},
		{
			name: "message only",
			err: &ProcessError{
				Message: "process failed",
			},
			expected: "process failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Error() != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, tt.err.Error())
			}
		})
	}
}

func TestProcessError_Unwrap(t *testing.T) {
	cause := errors.New("underlying error")
	err := &ProcessError{
		Message: "process failed",
		Cause:   cause,
	}

	if err.Unwrap() != cause {
		t.Error("Unwrap should return the cause")
	}

	// Test errors.Is works
	if !errors.Is(err, cause) {
		t.Error("errors.Is should match the cause")
	}
}

func TestProtocolError(t *testing.T) {
	tests := []struct {
		name     string
		err      *ProtocolError
		expected string
	}{
		{
			name: "with cause",
			err: &ProtocolError{
				Message: "parse failed",
				Line:    "invalid json",
				Cause:   errors.New("syntax error"),
			},
			expected: "parse failed: syntax error",
		},
		{
			name: "message only",
			err: &ProtocolError{
				Message: "parse failed",
			},
			expected: "parse failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Error() != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, tt.err.Error())
			}
		})
	}
}

func TestProtocolError_Unwrap(t *testing.T) {
	cause := errors.New("syntax error")
	err := &ProtocolError{
		Message: "parse failed",
		Cause:   cause,
	}

	if err.Unwrap() != cause {
		t.Error("Unwrap should return the cause")
	}
}

func TestTurnError(t *testing.T) {
	tests := []struct {
		name     string
		err      *TurnError
		expected string
	}{
		{
			name: "with cause",
			err: &TurnError{
				ThreadID: "thread-123",
				TurnID:   "turn-456",
				Message:  "turn failed",
				Cause:    errors.New("timeout"),
			},
			expected: "turn error (thread=thread-123, turn=turn-456): turn failed: timeout",
		},
		{
			name: "message only",
			err: &TurnError{
				ThreadID: "thread-123",
				TurnID:   "turn-456",
				Message:  "turn failed",
			},
			expected: "turn error (thread=thread-123, turn=turn-456): turn failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Error() != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, tt.err.Error())
			}
		})
	}
}

func TestTurnError_Unwrap(t *testing.T) {
	cause := errors.New("timeout")
	err := &TurnError{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
		Message:  "turn failed",
		Cause:    cause,
	}

	if err.Unwrap() != cause {
		t.Error("Unwrap should return the cause")
	}
}

func TestSentinelErrors(t *testing.T) {
	// Verify sentinel errors are unique
	errs := []error{
		ErrAlreadyStarted,
		ErrNotStarted,
		ErrStopping,
		ErrClientClosed,
		ErrThreadNotFound,
		ErrThreadNotReady,
		ErrTurnInProgress,
		ErrNoTurnInProgress,
		ErrTimeout,
		ErrInvalidState,
	}

	for i, err1 := range errs {
		for j, err2 := range errs {
			if i != j && err1 == err2 {
				t.Errorf("sentinel errors should be unique: %v == %v", err1, err2)
			}
		}
	}
}

func TestSentinelErrors_Messages(t *testing.T) {
	tests := []struct {
		err      error
		contains string
	}{
		{ErrAlreadyStarted, "already started"},
		{ErrNotStarted, "not started"},
		{ErrStopping, "stopping"},
		{ErrClientClosed, "closed"},
		{ErrThreadNotFound, "not found"},
		{ErrThreadNotReady, "not ready"},
		{ErrTurnInProgress, "in progress"},
		{ErrNoTurnInProgress, "no turn"},
		{ErrTimeout, "timed out"},
		{ErrInvalidState, "invalid state"},
	}

	for _, tt := range tests {
		if tt.err.Error() == "" {
			t.Errorf("%v should have a message", tt.err)
		}
	}
}
