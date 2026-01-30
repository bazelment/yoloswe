package planner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

func TestMCPServer_Initialize(t *testing.T) {
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

	p := New(cfg, "test-session")
	server := NewMCPServer(p)

	// Create a test request
	reqBody := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params:  json.RawMessage(`{}`),
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleMCP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp jsonRPCResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}

	// Check that result contains expected fields
	result, ok := resp.Result.(*initializeResult)
	if !ok {
		// Result is a map, need to convert
		resultMap, ok := resp.Result.(map[string]interface{})
		if !ok {
			t.Fatalf("result is not a map: %T", resp.Result)
		}
		if _, ok := resultMap["protocolVersion"]; !ok {
			t.Error("result missing protocolVersion")
		}
		if _, ok := resultMap["capabilities"]; !ok {
			t.Error("result missing capabilities")
		}
		if _, ok := resultMap["serverInfo"]; !ok {
			t.Error("result missing serverInfo")
		}
	} else {
		if result.ProtocolVersion == "" {
			t.Error("protocolVersion is empty")
		}
	}
}

func TestMCPServer_ToolsList(t *testing.T) {
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

	p := New(cfg, "test-session")
	server := NewMCPServer(p)

	// Create a test request
	reqBody := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/list",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleMCP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp jsonRPCResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}

	// Check that result contains tools
	resultMap, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}

	tools, ok := resultMap["tools"].([]interface{})
	if !ok {
		t.Fatalf("tools is not an array: %T", resultMap["tools"])
	}

	if len(tools) != 3 {
		t.Errorf("expected 3 tools, got %d", len(tools))
	}

	// Check tool names
	toolNames := make(map[string]bool)
	for _, tool := range tools {
		toolMap, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := toolMap["name"].(string); ok {
			toolNames[name] = true
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

func TestMCPServer_InvalidMethod(t *testing.T) {
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

	p := New(cfg, "test-session")
	server := NewMCPServer(p)

	// Create a test request with invalid method
	reqBody := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "invalid/method",
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleMCP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp jsonRPCResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error == nil {
		t.Error("expected error for invalid method")
	}

	if resp.Error.Code != -32601 {
		t.Errorf("expected error code -32601, got %d", resp.Error.Code)
	}
}

func TestMCPServer_StartStop(t *testing.T) {
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

	p := New(cfg, "test-session")
	server := NewMCPServer(p)

	// Start the server
	url, err := server.Start()
	if err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	if url == "" {
		t.Error("URL should not be empty")
	}

	// Verify the URL looks right
	if len(url) < 10 {
		t.Errorf("URL seems too short: %s", url)
	}

	// Stop the server
	if err := server.Stop(); err != nil {
		t.Errorf("failed to stop server: %v", err)
	}

	// Stopping again should be idempotent
	if err := server.Stop(); err != nil {
		t.Errorf("second stop failed: %v", err)
	}
}
