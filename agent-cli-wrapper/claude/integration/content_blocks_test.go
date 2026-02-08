//go:build integration && manual && local
// +build integration,manual,local

// Integration tests for structured content blocks.
//
// Run with: bazel test //agent-cli-wrapper/claude/integration:integration_test --test_tag_filters=manual,local

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

func TestSession_ContentBlocks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("failed to start session: %v", err)
	}
	defer session.Stop()

	// Send a prompt that will trigger tool use
	result, err := session.Ask(ctx, "List files in current directory using ls command")
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}

	if !result.Success {
		t.Fatalf("expected successful turn, got error: %v", result.Error)
	}

	// Verify content blocks are populated
	if len(result.ContentBlocks) == 0 {
		t.Fatal("expected at least one content block")
	}

	// Check for different block types
	var hasText bool
	var hasToolUse bool
	var hasToolResult bool

	for _, block := range result.ContentBlocks {
		switch block.Type {
		case claude.ContentBlockTypeText:
			hasText = true
			if block.Text == "" {
				t.Error("text block has empty text")
			}
			t.Logf("Text block: %q", block.Text[:min(50, len(block.Text))])

		case claude.ContentBlockTypeThinking:
			t.Logf("Thinking block: %q", block.Thinking[:min(50, len(block.Thinking))])

		case claude.ContentBlockTypeToolUse:
			hasToolUse = true
			if block.ToolUseID == "" {
				t.Error("tool_use block has empty tool_use_id")
			}
			if block.ToolName == "" {
				t.Error("tool_use block has empty tool_name")
			}
			t.Logf("Tool use block: name=%s, id=%s", block.ToolName, block.ToolUseID)

		case claude.ContentBlockTypeToolResult:
			hasToolResult = true
			if block.ToolUseID == "" {
				t.Error("tool_result block has empty tool_use_id")
			}
			t.Logf("Tool result block: id=%s, is_error=%v", block.ToolUseID, block.IsError)
		}
	}

	// For a tool-using prompt, we expect all three types
	if !hasText {
		t.Error("expected at least one text block")
	}
	if !hasToolUse {
		t.Error("expected at least one tool_use block")
	}
	if !hasToolResult {
		t.Error("expected at least one tool_result block")
	}

	t.Logf("Content blocks validated: %d total blocks", len(result.ContentBlocks))
}

func TestCollectResponse_ContentBlocks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("failed to start session: %v", err)
	}
	defer session.Stop()

	// Send message
	_, err := session.SendMessage(ctx, "What is 2+2?")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	// Use CollectResponse to get result and events
	result, events, err := session.CollectResponse(ctx)
	if err != nil {
		t.Fatalf("CollectResponse failed: %v", err)
	}

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	if !result.Success {
		t.Fatalf("expected successful turn, got error: %v", result.Error)
	}

	// Verify content blocks are in result
	if len(result.ContentBlocks) == 0 {
		t.Error("expected content blocks in result")
	}

	// Verify events were collected
	if len(events) == 0 {
		t.Error("expected events to be collected")
	}

	var foundTurnComplete bool
	for _, evt := range events {
		if _, ok := evt.(claude.TurnCompleteEvent); ok {
			foundTurnComplete = true
			break
		}
	}
	if !foundTurnComplete {
		t.Error("expected TurnCompleteEvent in collected events")
	}

	t.Logf("CollectResponse validated: %d content blocks, %d events",
		len(result.ContentBlocks), len(events))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
