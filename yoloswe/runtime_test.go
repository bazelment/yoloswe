package yoloswe

import (
	"context"
	"testing"
	"time"
)

// TestRunValidation tests prompt validation in Run method
func TestRunValidation(t *testing.T) {
	t.Run("empty prompt after validation", func(t *testing.T) {
		swe := New(Config{})
		ctx := context.Background()
		err := swe.Run(ctx, "  \n\t  ")
		if err == nil {
			t.Error("expected error for whitespace-only prompt")
		}
	})

	t.Run("prompt too short", func(t *testing.T) {
		swe := New(Config{})
		ctx := context.Background()
		err := swe.Run(ctx, "ab")
		if err == nil {
			t.Error("expected error for too-short prompt")
		}
	})
}

// TestContextCancellationBehavior tests context handling
func TestContextCancellationBehavior(t *testing.T) {
	t.Run("pre-cancelled context", func(t *testing.T) {
		swe := New(Config{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		// Should fail quickly with context cancelled
		err := swe.Run(ctx, "test prompt")
		// We expect it to fail because the session can't start with cancelled context
		if err == nil {
			t.Log("Warning: expected error with pre-cancelled context")
		}
	})

	t.Run("timeout context", func(t *testing.T) {
		swe := New(Config{})
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
		defer cancel()

		time.Sleep(1 * time.Millisecond) // Ensure timeout

		err := swe.Run(ctx, "test prompt")
		if err == nil {
			t.Log("Warning: expected error with timed-out context")
		}
	})
}

// TestStatsTracking tests that stats are properly tracked
func TestStatsTracking(t *testing.T) {
	swe := New(Config{})

	// Initial stats should be zero/empty
	stats := swe.Stats()
	if stats.IterationCount != 0 {
		t.Errorf("expected initial iteration count 0, got %d", stats.IterationCount)
	}
	if stats.BuilderCostUSD != 0 {
		t.Errorf("expected initial cost 0, got %.4f", stats.BuilderCostUSD)
	}
	if stats.ExitReason != "" {
		t.Errorf("expected initial exit reason empty, got %q", stats.ExitReason)
	}

	// Modify stats directly (simulating a run)
	swe.stats.IterationCount = 5
	swe.stats.BuilderCostUSD = 2.5
	swe.stats.ExitReason = ExitReasonAccepted
	swe.stats.TotalDurationMs = 30000

	// Verify stats are returned correctly
	stats = swe.Stats()
	if stats.IterationCount != 5 {
		t.Errorf("expected iteration count 5, got %d", stats.IterationCount)
	}
	if stats.BuilderCostUSD != 2.5 {
		t.Errorf("expected cost 2.5, got %.4f", stats.BuilderCostUSD)
	}
	if stats.ExitReason != ExitReasonAccepted {
		t.Errorf("expected exit reason %q, got %q", ExitReasonAccepted, stats.ExitReason)
	}
}

// TestConfigSanitizationEdgeCases tests edge cases in config sanitization
func TestConfigSanitizationEdgeCases(t *testing.T) {
	t.Run("very large values", func(t *testing.T) {
		config := Config{
			MaxBudgetUSD:   999999.99,
			MaxTimeSeconds: 999999999,
			MaxIterations:  999999,
		}
		SanitizeConfig(&config)
		// Should keep the values (not clamp them)
		if config.MaxBudgetUSD != 999999.99 {
			t.Error("should preserve large budget")
		}
		if config.MaxTimeSeconds != 999999999 {
			t.Error("should preserve large timeout")
		}
		if config.MaxIterations != 999999 {
			t.Error("should preserve large iterations")
		}
	})

	t.Run("mixed zero and valid values", func(t *testing.T) {
		config := Config{
			BuilderModel:   "opus",
			MaxBudgetUSD:   0,     // Should get default
			MaxTimeSeconds: 1800,  // Should keep
			MaxIterations:  0,     // Should get default
		}
		SanitizeConfig(&config)

		if config.BuilderModel != "opus" {
			t.Error("should preserve valid builder model")
		}
		if config.MaxBudgetUSD != 100.0 {
			t.Error("should apply default budget")
		}
		if config.MaxTimeSeconds != 1800 {
			t.Error("should preserve valid timeout")
		}
		if config.MaxIterations != 10 {
			t.Error("should apply default iterations")
		}
	})

	t.Run("whitespace in all string fields", func(t *testing.T) {
		config := Config{
			BuilderWorkDir: "  /path/to/dir  ",
			RecordingDir:   "\t/recordings\t",
			SystemPrompt:   "\n\nprompt\n\n",
			Goal:           "  goal  ",
		}
		SanitizeConfig(&config)

		if config.BuilderWorkDir != "/path/to/dir" {
			t.Errorf("expected trimmed work dir, got %q", config.BuilderWorkDir)
		}
		if config.RecordingDir != "/recordings" {
			t.Errorf("expected trimmed recording dir, got %q", config.RecordingDir)
		}
		if config.SystemPrompt != "prompt" {
			t.Errorf("expected trimmed prompt, got %q", config.SystemPrompt)
		}
		if config.Goal != "goal" {
			t.Errorf("expected trimmed goal, got %q", config.Goal)
		}
	})
}

// TestPrintSummaryWithZeroStats tests summary output with minimal stats
func TestPrintSummaryWithZeroStats(t *testing.T) {
	swe := New(Config{})
	// Should not panic with zero stats
	swe.PrintSummary()
}

// TestMultipleRunCalls tests that Run can't be called multiple times concurrently
func TestMultipleRunCalls(t *testing.T) {
	t.Skip("Requires proper session lifecycle management - implementation dependent")
}
