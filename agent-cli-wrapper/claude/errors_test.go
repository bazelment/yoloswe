package claude

import (
	"errors"
	"os/exec"
	"testing"
)

func TestCLINotFoundError(t *testing.T) {
	cause := exec.ErrNotFound
	err := &CLINotFoundError{
		Path:  "/usr/bin/claude",
		Cause: cause,
	}

	// Test Error() method
	errMsg := err.Error()
	if errMsg == "" {
		t.Error("expected non-empty error message")
	}
	if len(errMsg) < 10 {
		t.Errorf("error message too short: %q", errMsg)
	}

	// Test Unwrap()
	if !errors.Is(err, cause) {
		t.Error("expected errors.Is to find cause")
	}

	// Test errors.As
	var cliErr *CLINotFoundError
	if !errors.As(err, &cliErr) {
		t.Error("expected errors.As to match CLINotFoundError")
	}
	if cliErr.Path != "/usr/bin/claude" {
		t.Errorf("expected path '/usr/bin/claude', got %q", cliErr.Path)
	}
}

func TestSentinelErrors(t *testing.T) {
	// Test that sentinel errors are defined
	sentinels := []error{
		ErrBudgetExceeded,
		ErrMaxTurnsExceeded,
	}

	for _, err := range sentinels {
		if err == nil {
			t.Error("sentinel error is nil")
		}
		if err.Error() == "" {
			t.Error("sentinel error has empty message")
		}
	}

	// Test that they can be compared with errors.Is
	testErr := ErrBudgetExceeded
	if !errors.Is(testErr, ErrBudgetExceeded) {
		t.Error("errors.Is failed for ErrBudgetExceeded")
	}

	testErr = ErrMaxTurnsExceeded
	if !errors.Is(testErr, ErrMaxTurnsExceeded) {
		t.Error("errors.Is failed for ErrMaxTurnsExceeded")
	}
}

func TestTransientError(t *testing.T) {
	cause := errors.New("error_during_execution")
	err := &TransientError{
		Cause:     cause,
		Message:   "Stream idle timeout - partial response received",
		RequestID: "req_abc123",
	}

	if got := err.Error(); got != "transient CLI error (request req_abc123): Stream idle timeout - partial response received" {
		t.Errorf("Error() = %q", got)
	}
	if !errors.Is(err, cause) {
		t.Error("expected errors.Is to find cause")
	}
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Error("expected errors.As to match TransientError")
	}
	if !IsTransient(err) {
		t.Error("expected IsTransient true")
	}
	if IsTransient(errors.New("unrelated")) {
		t.Error("expected IsTransient false for unrelated error")
	}
	if !IsRecoverable(err) {
		t.Error("TransientError should be recoverable")
	}
}

func TestTransientError_WithoutRequestID(t *testing.T) {
	err := &TransientError{Message: "temporary failure"}
	if got := err.Error(); got != "transient CLI error: temporary failure" {
		t.Errorf("Error() = %q", got)
	}
}

func TestIsRecoverable_CLINotFound(t *testing.T) {
	err := &CLINotFoundError{
		Path:  "/usr/bin/claude",
		Cause: exec.ErrNotFound,
	}

	if IsRecoverable(err) {
		t.Error("CLINotFoundError should not be recoverable")
	}
}

func TestIsRecoverable_BudgetErrors(t *testing.T) {
	// Budget and turn limit errors are recoverable (caller can decide what to do)
	if !IsRecoverable(ErrBudgetExceeded) {
		t.Error("ErrBudgetExceeded should be recoverable")
	}
	if !IsRecoverable(ErrMaxTurnsExceeded) {
		t.Error("ErrMaxTurnsExceeded should be recoverable")
	}
}
