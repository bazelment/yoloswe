//go:build integration && manual && local
// +build integration,manual,local

// Integration tests for Query and QueryStream functions.
//
// Run with: bazel test //agent-cli-wrapper/claude/integration:integration_test --test_tag_filters=manual,local
//
// These tests require:
// - The claude CLI to be installed and available in PATH
// - A valid API key configured

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

func TestQuery_BasicPrompt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Test basic query with a simple math question
	result, err := claude.Query(ctx, "What is 2+2? Answer with just the number.")
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	// Verify result structure
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	if !result.Success {
		t.Errorf("expected successful result, got error: %v", result.Error)
	}

	if result.Text == "" {
		t.Error("expected non-empty text response")
	}

	if result.SessionID == "" {
		t.Error("expected non-empty session ID")
	}

	if result.Usage.CostUSD <= 0 {
		t.Error("expected positive cost")
	}

	t.Logf("Query succeeded: text=%q, cost=$%.6f, sessionID=%s",
		result.Text, result.Usage.CostUSD, result.SessionID)
}

func TestQuery_WithModel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Test query with explicit model
	result, err := claude.Query(ctx, "Say 'hello' in one word.",
		claude.WithModel("haiku"))
	if err != nil {
		t.Fatalf("Query with model failed: %v", err)
	}

	if !result.Success {
		t.Errorf("expected successful result, got error: %v", result.Error)
	}

	if result.Text == "" {
		t.Error("expected non-empty text response")
	}

	t.Logf("Query with model succeeded: text=%q", result.Text)
}

func TestQuery_DefaultsBypassPermissions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Query should default to bypass permissions (no explicit permission mode)
	// Use a prompt that would trigger tool use
	result, err := claude.Query(ctx, "List files in the current directory using ls command")
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	if !result.Success {
		t.Errorf("expected successful result (should auto-approve tools), got error: %v", result.Error)
	}

	t.Logf("Query with tool use succeeded (bypass permissions): text length=%d", len(result.Text))
}

func TestQueryStream_BasicPrompt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Test streaming query
	events, err := claude.QueryStream(ctx, "Count from 1 to 3")
	if err != nil {
		t.Fatalf("QueryStream failed: %v", err)
	}

	if events == nil {
		t.Fatal("expected non-nil event channel")
	}

	// Collect all events
	var textEvents []claude.TextEvent
	var turnComplete *claude.TurnCompleteEvent
	for evt := range events {
		switch e := evt.(type) {
		case claude.TextEvent:
			textEvents = append(textEvents, e)
		case claude.TurnCompleteEvent:
			turnComplete = &e
		}
	}

	// Verify we received events
	if len(textEvents) == 0 {
		t.Error("expected at least one text event")
	}

	if turnComplete == nil {
		t.Fatal("expected turn complete event")
	}

	if !turnComplete.Success {
		t.Errorf("expected successful turn, got error: %v", turnComplete.Error)
	}

	t.Logf("QueryStream succeeded: received %d text events, cost=$%.6f",
		len(textEvents), turnComplete.Usage.CostUSD)
}
