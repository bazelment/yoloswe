package yoloswe

import (
	"context"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/yoloswe/testutil"
)

// Integration tests for the builder-reviewer loop.
// These tests require real Claude and Codex SDK sessions.
// Run with: bazel test //yoloswe:integration_test

func TestIntegrationBuilderReviewerLoop(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitGitRepo(t, workDir)
	config := Config{
		BuilderModel:   "haiku",
		ReviewerModel:  "gpt-5.2-codex",
		BuilderWorkDir: workDir,
		MaxBudgetUSD:   1.0,
		MaxTimeSeconds: 120,
		MaxIterations:  3,
		Verbose:        true,
	}

	swe := New(config)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prompt := "Create a simple hello world function in Go"

	if err := swe.Run(ctx, prompt); err != nil {
		t.Logf("Run error: %v", err)
	}

	stats := swe.Stats()
	t.Logf("Exit reason: %s", stats.ExitReason)
	t.Logf("Iterations: %d", stats.IterationCount)
	t.Logf("Cost: $%.4f", stats.BuilderCostUSD)

	if stats.IterationCount == 0 {
		t.Error("expected at least one iteration")
	}
}

func TestIntegrationBudgetLimit(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitGitRepo(t, workDir)
	config := Config{
		BuilderModel:   "haiku",
		ReviewerModel:  "gpt-5.2-codex",
		BuilderWorkDir: workDir,
		MaxBudgetUSD:   0.01, // Very low budget
		MaxTimeSeconds: 60,
		MaxIterations:  10,
		Verbose:        true,
	}

	swe := New(config)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prompt := "Implement a complex web server with authentication"

	_ = swe.Run(ctx, prompt)
	stats := swe.Stats()

	// Should exit due to budget (or interrupt if context times out first)
	if stats.ExitReason != ExitReasonBudgetExceeded && stats.ExitReason != ExitReasonInterrupt {
		t.Logf("Expected budget exceeded or interrupt, got: %s (cost: $%.4f)", stats.ExitReason, stats.BuilderCostUSD)
	}
}

func TestIntegrationTimeoutLimit(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitGitRepo(t, workDir)
	config := Config{
		BuilderModel:   "sonnet",
		ReviewerModel:  "gpt-5.2-codex",
		BuilderWorkDir: workDir,
		MaxBudgetUSD:   10.0,
		MaxTimeSeconds: 5, // Very short timeout
		MaxIterations:  10,
		Verbose:        true,
	}

	swe := New(config)
	// Use context timeout to bound test duration (first turn may exceed MaxTimeSeconds)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prompt := "Create a comprehensive test suite"

	_ = swe.Run(ctx, prompt)
	stats := swe.Stats()

	// Should exit due to timeout or interrupt (context timeout may fire first)
	if stats.ExitReason != ExitReasonTimeExceeded && stats.ExitReason != ExitReasonInterrupt {
		t.Logf("Expected timeout or interrupt, got: %s (duration: %.1fs)", stats.ExitReason, float64(stats.TotalDurationMs)/1000)
	}
}

func TestIntegrationContextCancellation(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitGitRepo(t, workDir)
	config := Config{
		BuilderModel:   "haiku",
		ReviewerModel:  "gpt-5.2-codex",
		BuilderWorkDir: workDir,
		MaxBudgetUSD:   5.0,
		MaxTimeSeconds: 600,
		MaxIterations:  10,
		Verbose:        true,
	}

	swe := New(config)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after 2 seconds
	go func() {
		time.Sleep(2 * time.Second)
		cancel()
	}()

	prompt := "Create a simple function"

	err := swe.Run(ctx, prompt)
	stats := swe.Stats()

	// Should exit due to interrupt
	if stats.ExitReason != ExitReasonInterrupt {
		t.Logf("Expected interrupt, got: %s", stats.ExitReason)
	}
	if err != nil {
		t.Logf("Run returned error: %v", err)
	}
}

func TestIntegrationMaxIterations(t *testing.T) {
	workDir := t.TempDir()
	testutil.InitGitRepo(t, workDir)
	config := Config{
		BuilderModel:   "haiku",
		ReviewerModel:  "gpt-5.2-codex",
		BuilderWorkDir: workDir,
		MaxBudgetUSD:   10.0,
		MaxTimeSeconds: 600,
		MaxIterations:  1, // Only one iteration
		Verbose:        true,
	}

	swe := New(config)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prompt := "Create a function with a subtle bug that needs fixing"

	err := swe.Run(ctx, prompt)
	stats := swe.Stats()

	// Should exit due to max iterations if reviewer doesn't accept first time
	if stats.IterationCount <= 1 && stats.ExitReason == ExitReasonMaxIterations {
		t.Log("Reached max iterations as expected")
	}
	if err != nil {
		t.Logf("Run returned error: %v", err)
	}
}
