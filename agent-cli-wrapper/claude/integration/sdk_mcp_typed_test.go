//go:build integration
// +build integration

// Integration test for TypedToolRegistry with SDK MCP tool support.
//
// Tests that typed tools registered with TypedToolRegistry work end-to-end
// through the Claude CLI control protocol.
//
// Run with: bazel test //agent-cli-wrapper/claude/integration:integration_test --test_timeout=300
//
// Requires:
// - The claude CLI installed and available in PATH
// - A valid API key configured

package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// Test parameter types for the integration test tools
type AddParams struct {
	A float64 `json:"a" jsonschema:"required,description=First number to add"`
	B float64 `json:"b" jsonschema:"required,description=Second number to add"`
}

type EchoParams struct {
	Text string `json:"text" jsonschema:"required,description=Text to echo back"`
}

type SearchParams struct {
	Query      string   `json:"query" jsonschema:"required,description=Search query string"`
	MaxResults int      `json:"max_results,omitempty" jsonschema:"description=Maximum number of results to return,default=10"`
	Filters    []string `json:"filters,omitempty" jsonschema:"description=Filter criteria to apply"`
}

// typedToolsHandler wraps TypedToolRegistry and tracks tool calls for testing
type typedToolsHandler struct {
	*claude.TypedToolRegistry
	mu        sync.Mutex
	callCount map[string]int
}

func newTypedToolsHandler() *typedToolsHandler {
	h := &typedToolsHandler{
		TypedToolRegistry: claude.NewTypedToolRegistry(),
		callCount:         make(map[string]int),
	}

	// Register add_numbers tool
	claude.AddTool(h.TypedToolRegistry, "add_numbers", "Add two numbers together and return the sum",
		func(ctx context.Context, p AddParams) (string, error) {
			h.mu.Lock()
			h.callCount["add_numbers"]++
			h.mu.Unlock()

			sum := p.A + p.B
			return fmt.Sprintf("%g", sum), nil
		})

	// Register echo tool
	claude.AddTool(h.TypedToolRegistry, "echo", "Echo back the input text with a prefix",
		func(ctx context.Context, p EchoParams) (string, error) {
			h.mu.Lock()
			h.callCount["echo"]++
			h.mu.Unlock()

			return fmt.Sprintf("Echo: %s", p.Text), nil
		})

	// Register search tool
	claude.AddTool(h.TypedToolRegistry, "search", "Search with optional filters and result limits",
		func(ctx context.Context, p SearchParams) (string, error) {
			h.mu.Lock()
			h.callCount["search"]++
			h.mu.Unlock()

			maxResults := p.MaxResults
			if maxResults == 0 {
				maxResults = 10
			}

			result := fmt.Sprintf("Searching for '%s' (max %d results)", p.Query, maxResults)
			if len(p.Filters) > 0 {
				result += fmt.Sprintf(", filters: %v", p.Filters)
			}

			return result, nil
		})

	return h
}

func (h *typedToolsHandler) GetCallCount(toolName string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.callCount[toolName]
}

func TestSession_Integration_TypedTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	handler := newTypedToolsHandler()

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithSDKTools("typed-tools", handler),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer session.Stop()

	t.Log("Sending prompt to trigger typed SDK MCP tools...")
	_, err := session.SendMessage(ctx,
		"Please use the tools to: 1) add 17 and 25, 2) echo the text 'hello world', 3) search for 'golang' with max 5 results. Reply with just the results, nothing else.")
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

	// Check that tools were called
	addCount := handler.GetCallCount("add_numbers")
	echoCount := handler.GetCallCount("echo")
	searchCount := handler.GetCallCount("search")

	t.Logf("Tool call counts: add_numbers=%d, echo=%d, search=%d", addCount, echoCount, searchCount)

	// At least one tool should have been called
	totalCalls := addCount + echoCount + searchCount
	if totalCalls == 0 {
		t.Error("Expected at least one tool to be called")
	}

	// Check for tool start events
	toolNames := make(map[string]bool)
	for _, ts := range events.ToolStarts {
		t.Logf("Tool started: %s (id=%s)", ts.Name, ts.ID)
		if strings.Contains(ts.Name, "add_numbers") {
			toolNames["add_numbers"] = true
		}
		if strings.Contains(ts.Name, "echo") {
			toolNames["echo"] = true
		}
		if strings.Contains(ts.Name, "search") {
			toolNames["search"] = true
		}
	}

	if len(toolNames) == 0 {
		t.Error("Expected ToolStartEvent for at least one typed tool")
	} else {
		t.Logf("Tools that were started: %v", toolNames)
	}

	// Turn should complete successfully
	if events.TurnComplete == nil {
		t.Fatal("Expected TurnCompleteEvent")
	}
	if !events.TurnComplete.Success {
		t.Errorf("Turn should have succeeded, got error: %v", events.TurnComplete.Error)
	}

	t.Logf("Turn completed: success=%v, duration=%dms, cost=$%.6f",
		events.TurnComplete.Success,
		events.TurnComplete.DurationMs,
		events.TurnComplete.Usage.CostUSD)

	// Check response contains expected results
	fullResponse := ""
	for _, te := range events.TextEvents {
		fullResponse += te.FullText
	}

	t.Logf("Full response length: %d characters", len(fullResponse))

	// Check for expected outputs
	expectedOutputs := map[string]string{
		"42":          "sum of 17 and 25",
		"hello world": "echo output",
		"golang":      "search query",
	}

	for output, description := range expectedOutputs {
		if strings.Contains(fullResponse, output) {
			t.Logf("✓ Response contains expected %s: '%s'", description, output)
		}
	}
}

func TestSession_Integration_TypedToolWithError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	registry := claude.NewTypedToolRegistry()

	// Add a tool that always returns an error
	type ErrorParams struct {
		Message string `json:"message" jsonschema:"required,description=Error message to return"`
	}

	claude.AddTool(registry, "failing_tool", "A tool that always fails with the given message",
		func(ctx context.Context, p ErrorParams) (string, error) {
			return "", fmt.Errorf("tool error: %s", p.Message)
		})

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithSDKTools("error-tools", registry),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer session.Stop()

	t.Log("Sending prompt to trigger error tool...")
	_, err := session.SendMessage(ctx,
		"Use the failing_tool with message 'test error'. Just tell me what happened.")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	events, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents failed: %v", err)
	}

	// Turn should still complete (errors are handled gracefully)
	if events.TurnComplete == nil {
		t.Fatal("Expected TurnCompleteEvent")
	}

	t.Logf("Turn completed: success=%v", events.TurnComplete.Success)

	// Check that tool was called
	hasErrorTool := false
	for _, ts := range events.ToolStarts {
		if strings.Contains(ts.Name, "failing_tool") {
			hasErrorTool = true
			t.Logf("Error tool was called: %s", ts.Name)
		}
	}

	if !hasErrorTool {
		t.Error("Expected failing_tool to be called")
	}
}

func TestSession_Integration_TypedToolMultipleTypes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	registry := claude.NewTypedToolRegistry()

	// Test various parameter types in a single tool
	type MultiTypeParams struct {
		StringVal  string   `json:"string_val" jsonschema:"required,description=A string value"`
		NumberVal  float64  `json:"number_val" jsonschema:"required,description=A number value"`
		BoolVal    bool     `json:"bool_val" jsonschema:"required,description=A boolean value"`
		ArrayVal   []string `json:"array_val,omitempty" jsonschema:"description=An array of strings"`
		OptionalVal string  `json:"optional_val,omitempty" jsonschema:"description=An optional string"`
	}

	callCount := 0
	var lastParams MultiTypeParams
	var mu sync.Mutex

	claude.AddTool(registry, "multi_type", "Tool with multiple parameter types",
		func(ctx context.Context, p MultiTypeParams) (string, error) {
			mu.Lock()
			callCount++
			lastParams = p
			mu.Unlock()

			return fmt.Sprintf("Received: %s, %g, %v, %v, %s",
				p.StringVal, p.NumberVal, p.BoolVal, p.ArrayVal, p.OptionalVal), nil
		})

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithSDKTools("multi-type-tools", registry),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer session.Stop()

	t.Log("Sending prompt to test multiple parameter types...")
	_, err := session.SendMessage(ctx,
		`Use the multi_type tool with: string_val="test", number_val=42.5, bool_val=true, array_val=["a", "b", "c"]. Just tell me what happened.`)
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	events, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents failed: %v", err)
	}

	mu.Lock()
	count := callCount
	params := lastParams
	mu.Unlock()

	if count == 0 {
		t.Error("Expected multi_type tool to be called")
	} else {
		t.Logf("Tool called %d time(s)", count)
		t.Logf("Last params: string=%s, number=%g, bool=%v, array=%v, optional=%s",
			params.StringVal, params.NumberVal, params.BoolVal, params.ArrayVal, params.OptionalVal)
	}

	if events.TurnComplete == nil {
		t.Fatal("Expected TurnCompleteEvent")
	}
	if !events.TurnComplete.Success {
		t.Errorf("Turn should have succeeded, got error: %v", events.TurnComplete.Error)
	}
}
