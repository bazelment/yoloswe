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
