package planner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

func newTestPlanner(t *testing.T) *Planner {
	t.Helper()
	cfg := Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "sonnet",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		DesignerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		BuilderConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		ReviewerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
	}
	return New(cfg, "test-session")
}

func TestPlannerToolHandler_Tools(t *testing.T) {
	p := newTestPlanner(t)
	handler := NewPlannerToolHandler(p)

	tools := handler.Tools()
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}

	toolNames := make(map[string]bool)
	for _, tool := range tools {
		toolNames[tool.Name] = true

		// Verify inputSchema is valid JSON
		var schema map[string]interface{}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("tool %q has invalid inputSchema: %v", tool.Name, err)
		}
	}

	if !toolNames["designer"] {
		t.Error("missing designer tool")
	}
	if !toolNames["builder"] {
		t.Error("missing builder tool")
	}
	if !toolNames["reviewer"] {
		t.Error("missing reviewer tool")
	}
}

func TestPlannerToolHandler_UnknownTool(t *testing.T) {
	p := newTestPlanner(t)
	handler := NewPlannerToolHandler(p)

	result, err := handler.HandleToolCall(context.Background(), "nonexistent", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.IsError {
		t.Error("expected IsError for unknown tool")
	}

	if len(result.Content) == 0 {
		t.Fatal("expected content in error result")
	}

	if result.Content[0].Type != "text" {
		t.Errorf("expected content type 'text', got %q", result.Content[0].Type)
	}
}

func TestPlannerToolHandler_ToolDefinitionSchemas(t *testing.T) {
	p := newTestPlanner(t)
	handler := NewPlannerToolHandler(p)

	tools := handler.Tools()
	for _, tool := range tools {
		var schema map[string]interface{}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("tool %q: failed to parse inputSchema: %v", tool.Name, err)
			continue
		}

		if schema["type"] != "object" {
			t.Errorf("tool %q: expected schema type 'object', got %v", tool.Name, schema["type"])
		}

		props, ok := schema["properties"].(map[string]interface{})
		if !ok {
			t.Errorf("tool %q: expected properties to be object", tool.Name)
			continue
		}

		// All tools should have a "task" property
		if _, ok := props["task"]; !ok {
			t.Errorf("tool %q: missing 'task' property", tool.Name)
		}

		required, ok := schema["required"].([]interface{})
		if !ok {
			t.Errorf("tool %q: expected required to be array", tool.Name)
			continue
		}

		hasTaskRequired := false
		for _, r := range required {
			if r == "task" {
				hasTaskRequired = true
			}
		}
		if !hasTaskRequired {
			t.Errorf("tool %q: 'task' should be required", tool.Name)
		}
	}
}

func TestPlannerToolHandler_ImplementsSDKToolHandler(t *testing.T) {
	p := newTestPlanner(t)
	handler := NewPlannerToolHandler(p)

	// Verify it has the right method signatures
	_ = handler.Tools()
	_, _ = handler.HandleToolCall(context.Background(), "test", json.RawMessage(`{}`))

	// Verify MCPToolDefinition fields match expected types
	tools := handler.Tools()
	for _, tool := range tools {
		if tool.Name == "" {
			t.Error("tool name should not be empty")
		}
		if tool.Description == "" {
			t.Error("tool description should not be empty")
		}
		if len(tool.InputSchema) == 0 {
			t.Error("tool inputSchema should not be empty")
		}

		// Verify it's a valid protocol.MCPToolDefinition
		_ = protocol.MCPToolDefinition{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		}
	}
}
