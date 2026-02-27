package integration

import (
	"context"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

// TestReviewWithResult_Codex tests that a simple review round-trip completes
// within a reasonable time using the codex backend.
func TestReviewWithResult_Codex(t *testing.T) {
	config := reviewer.Config{
		BackendType: reviewer.BackendCodex,
		Model:       "gpt-5.2-codex",
		WorkDir:     t.TempDir(),
		Verbose:     true,
	}

	r := reviewer.New(config)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Failed to start reviewer: %v", err)
	}
	defer r.Stop()

	start := time.Now()
	result, err := r.ReviewWithResult(ctx, "Say 'hello' and nothing else.")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("ReviewWithResult failed after %v: %v", elapsed, err)
	}

	if result.ResponseText == "" {
		t.Fatal("Expected non-empty response text")
	}

	t.Logf("Review completed in %v (response length: %d chars)", elapsed, len(result.ResponseText))
}

// TestReviewWithResult_Cursor tests that a simple review round-trip completes
// within a reasonable time using the cursor backend.
func TestReviewWithResult_Cursor(t *testing.T) {
	config := reviewer.Config{
		BackendType: reviewer.BackendCursor,
		WorkDir:     t.TempDir(),
		Verbose:     true,
	}

	r := reviewer.New(config)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Failed to start reviewer: %v", err)
	}
	defer r.Stop()

	start := time.Now()
	result, err := r.ReviewWithResult(ctx, "Say 'hello' and nothing else.")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("ReviewWithResult failed after %v: %v", elapsed, err)
	}

	if result.ResponseText == "" {
		t.Fatal("Expected non-empty response text")
	}

	t.Logf("Review completed in %v (response length: %d chars)", elapsed, len(result.ResponseText))
}
