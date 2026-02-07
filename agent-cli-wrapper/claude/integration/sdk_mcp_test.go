//go:build integration
// +build integration

// Integration test for SDK MCP tool support.
//
// Tests that an SDKToolHandler wired into a Claude session receives
// tool calls from the CLI through the stdin/stdout control protocol.
//
// Run with: bazel test //agent-cli-wrapper/claude/integration:integration_test --test_timeout=120
//
// Requires:
// - The claude CLI installed and available in PATH
// - A valid API key configured

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// addNumbersHandler is a simple SDK tool handler for testing.
type addNumbersHandler struct {
	mu        sync.Mutex
	callCount int
	lastArgs  json.RawMessage
}

func (h *addNumbersHandler) Tools() []protocol.MCPToolDefinition {
	return []protocol.MCPToolDefinition{
		{
			Name:        "add_numbers",
			Description: "Add two numbers together and return the sum",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"a": {"type": "number", "description": "First number"},
					"b": {"type": "number", "description": "Second number"}
				},
				"required": ["a", "b"]
			}`),
		},
	}
}

func (h *addNumbersHandler) HandleToolCall(_ context.Context, name string, args json.RawMessage) (*protocol.MCPToolCallResult, error) {
	h.mu.Lock()
	h.callCount++
	h.lastArgs = args
	h.mu.Unlock()

	if name != "add_numbers" {
		return &protocol.MCPToolCallResult{
			Content: []protocol.MCPContentItem{
				{Type: "text", Text: fmt.Sprintf("unknown tool: %s", name)},
			},
			IsError: true,
		}, nil
	}

	var params struct {
		A float64 `json:"a"`
		B float64 `json:"b"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	sum := params.A + params.B
	return &protocol.MCPToolCallResult{
		Content: []protocol.MCPContentItem{
			{Type: "text", Text: fmt.Sprintf("%g", sum)},
		},
	}, nil
}

func (h *addNumbersHandler) CallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.callCount
}

func TestSession_Integration_SDKMCPTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	handler := &addNumbersHandler{}

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithSDKTools("test-tools", handler),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer session.Stop()

	// Send a prompt that should trigger the add_numbers tool
	t.Log("Sending prompt to trigger SDK MCP tool call...")
	_, err := session.SendMessage(ctx, "Use the add_numbers tool to add 17 and 25. Reply with just the number, nothing else.")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	events, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents failed: %v", err)
	}

	if events.Ready != nil {
		t.Logf("Session ready: id=%s, model=%s", events.Ready.Info.SessionID, events.Ready.Info.Model)
	}

	// Check that the tool was called
	if handler.CallCount() == 0 {
		t.Error("Expected add_numbers handler to be called at least once")
	}
	t.Logf("Handler called %d time(s)", handler.CallCount())

	// Check for tool start events with the SDK MCP tool name pattern
	hasSDKTool := false
	for _, ts := range events.ToolStarts {
		t.Logf("Tool started: %s", ts.Name)
		if strings.Contains(ts.Name, "add_numbers") {
			hasSDKTool = true
		}
	}
	if !hasSDKTool {
		t.Error("Expected ToolStartEvent with add_numbers tool")
	}

	// Turn should complete successfully
	if events.TurnComplete == nil {
		t.Fatal("Expected TurnCompleteEvent")
	}
	if !events.TurnComplete.Success {
		t.Errorf("Turn should have succeeded, got error: %v", events.TurnComplete.Error)
	}

	t.Logf("Turn completed: success=%v, cost=$%.6f", events.TurnComplete.Success, events.TurnComplete.Usage.CostUSD)

	// Check response mentions 42
	for _, te := range events.TextEvents {
		if strings.Contains(te.FullText, "42") {
			t.Logf("Response contains expected sum '42'")
		}
	}
}
