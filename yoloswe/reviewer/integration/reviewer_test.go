//go:build integration

package integration

import (
	"context"
	"flag"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

var (
	agyModel    = flag.String("agy-model", reviewer.DefaultAgyModel, "agy model ID to use in integration tests")
	claudeModel = flag.String("claude-model", reviewer.DefaultClaudeModel, "Claude model ID to use in integration tests")
)

// TestReviewWithResult_Codex tests that a simple review round-trip completes
// within a reasonable time using the codex backend.
func TestReviewWithResult_Codex(t *testing.T) {
	config := reviewer.Config{
		BackendType: reviewer.BackendCodex,
		Model:       "gpt-5.4-mini",
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

// TestReviewWithResult_Agy tests that a simple review round-trip completes
// within a reasonable time using the agy backend.
// Requires the "agy" CLI to be installed and authenticated.
func TestReviewWithResult_Agy(t *testing.T) {
	config := reviewer.Config{
		BackendType: reviewer.BackendAgy,
		Model:       *agyModel,
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

// TestReviewWithResult_Claude tests that a simple review round-trip completes
// using the claude backend. Requires the "claude" CLI to be installed and
// authenticated.
//
// Unlike the other three backends, claude reports token usage on its turn
// event, so this test also asserts the counts survive the bridge — a silent
// regression there would leave /pr-polish unable to budget claude rounds.
func TestReviewWithResult_Claude(t *testing.T) {
	config := reviewer.Config{
		BackendType: reviewer.BackendClaude,
		Model:       *claudeModel,
		WorkDir:     t.TempDir(),
		ReadOnly:    true,
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

	// The ready-event adapter is the only path that populates these; a nil
	// adapter would still produce text but leave resume unusable.
	if r.LastSessionID() == "" {
		t.Error("Expected a session id from the claude ReadyEvent")
	}
	if result.OutputTokens == 0 {
		t.Errorf("Expected non-zero output tokens, got in=%d out=%d", result.InputTokens, result.OutputTokens)
	}

	t.Logf("Review completed in %v (response length: %d chars, session %s, tokens in=%d out=%d)",
		elapsed, len(result.ResponseText), r.LastSessionID(), result.InputTokens, result.OutputTokens)
}

// TestReviewWithResult_ClaudeResumeFallback verifies that a stale resume id
// degrades to a fresh session tagged resume_status=fallback rather than
// failing the round. This is the live check on isResumeUnavailableMessage's
// claude phrasing — if the CLI's wording changes, this test catches it.
func TestReviewWithResult_ClaudeResumeFallback(t *testing.T) {
	config := reviewer.Config{
		BackendType:     reviewer.BackendClaude,
		Model:           *claudeModel,
		WorkDir:         t.TempDir(),
		ReadOnly:        true,
		ResumeSessionID: "00000000-0000-4000-8000-000000000000",
	}

	r := reviewer.New(config)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Failed to start reviewer: %v", err)
	}
	defer r.Stop()

	result, err := r.ReviewWithResult(ctx, "Say 'hello' and nothing else.")
	if err != nil {
		t.Fatalf("Expected fallback to a fresh session, got error: %v", err)
	}
	if result.ResumeStatus != reviewer.ResumeStatusFallback {
		t.Errorf("resume status = %q, want %q", result.ResumeStatus, reviewer.ResumeStatusFallback)
	}
}
