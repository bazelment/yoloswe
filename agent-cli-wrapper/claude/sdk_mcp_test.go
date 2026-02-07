package claude

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// testToolHandler implements SDKToolHandler for testing.
type testToolHandler struct{}

func (h *testToolHandler) Tools() []protocol.MCPToolDefinition {
	return []protocol.MCPToolDefinition{
		{
			Name:        "add_numbers",
			Description: "Add two numbers",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`),
		},
	}
}

func (h *testToolHandler) HandleToolCall(_ context.Context, name string, args json.RawMessage) (*protocol.MCPToolCallResult, error) {
	return &protocol.MCPToolCallResult{
		Content: []protocol.MCPContentItem{
			{Type: "text", Text: "42"},
		},
	}, nil
}

func TestMCPSDKServerConfig_ServerType(t *testing.T) {
	cfg := MCPSDKServerConfig{Type: MCPServerTypeSDK}
	if cfg.serverType() != MCPServerTypeSDK {
		t.Errorf("expected serverType() to return 'sdk', got %q", cfg.serverType())
	}
}

func TestMCPSDKServerConfig_MarshalJSON(t *testing.T) {
	cfg := MCPSDKServerConfig{Type: MCPServerTypeSDK, Name: "my-tools"}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["type"] != "sdk" {
		t.Errorf("expected type 'sdk', got %v", parsed["type"])
	}

	if parsed["name"] != "my-tools" {
		t.Errorf("expected name 'my-tools', got %v", parsed["name"])
	}

	// Should have type and name fields
	if len(parsed) != 2 {
		t.Errorf("expected 2 fields, got %d: %v", len(parsed), parsed)
	}
}

func TestMCPConfig_AddSDKServer(t *testing.T) {
	handler := &testToolHandler{}
	cfg := NewMCPConfig()
	result := cfg.AddSDKServer("test-tools", handler)

	// Should return self for chaining
	if result != cfg {
		t.Error("AddSDKServer should return self for chaining")
	}

	// Verify server was added to MCPServers
	server, ok := cfg.MCPServers["test-tools"]
	if !ok {
		t.Fatal("expected test-tools server to be added")
	}

	sdk, ok := server.(MCPSDKServerConfig)
	if !ok {
		t.Fatalf("expected MCPSDKServerConfig, got %T", server)
	}
	if sdk.Type != MCPServerTypeSDK {
		t.Errorf("expected type 'sdk', got %q", sdk.Type)
	}

	// Verify handler was registered
	handlers := cfg.SDKHandlers()
	if handlers == nil {
		t.Fatal("expected SDKHandlers to be non-nil")
	}
	if handlers["test-tools"] != handler {
		t.Error("expected handler to be registered")
	}
}

func TestMCPConfig_AddSDKServer_MarshalJSON(t *testing.T) {
	handler := &testToolHandler{}
	cfg := NewMCPConfig().AddSDKServer("my-tools", handler)

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	servers := parsed["mcpServers"].(map[string]interface{})
	myTools := servers["my-tools"].(map[string]interface{})
	if myTools["type"] != "sdk" {
		t.Errorf("expected type 'sdk', got %v", myTools["type"])
	}
}

func TestWithSDKTools_Option(t *testing.T) {
	handler := &testToolHandler{}

	config := defaultConfig()
	opt := WithSDKTools("test-tools", handler)
	opt(&config)

	if config.MCPConfig == nil {
		t.Fatal("expected MCPConfig to be created")
	}

	// Verify server is in config
	server, ok := config.MCPConfig.MCPServers["test-tools"]
	if !ok {
		t.Fatal("expected test-tools server in MCPServers")
	}

	if _, ok := server.(MCPSDKServerConfig); !ok {
		t.Fatalf("expected MCPSDKServerConfig, got %T", server)
	}

	// Verify handler is registered
	handlers := config.MCPConfig.SDKHandlers()
	if handlers["test-tools"] != handler {
		t.Error("expected handler to be registered via WithSDKTools")
	}
}

func TestWithSDKTools_PreservesExistingConfig(t *testing.T) {
	handler := &testToolHandler{}

	config := defaultConfig()
	config.MCPConfig = NewMCPConfig().AddHTTPServer("existing", "http://localhost:8080")

	opt := WithSDKTools("test-tools", handler)
	opt(&config)

	// Should still have the existing server
	if _, ok := config.MCPConfig.MCPServers["existing"]; !ok {
		t.Error("expected existing HTTP server to be preserved")
	}

	// And the new SDK server
	if _, ok := config.MCPConfig.MCPServers["test-tools"]; !ok {
		t.Error("expected test-tools SDK server to be added")
	}
}

func TestBuildInitializeResult(t *testing.T) {
	result := buildInitializeResult("my-server")

	if result.ProtocolVersion != "2024-11-05" {
		t.Errorf("expected protocol version '2024-11-05', got %q", result.ProtocolVersion)
	}
	if result.ServerInfo.Name != "my-server" {
		t.Errorf("expected server name 'my-server', got %q", result.ServerInfo.Name)
	}
	if result.Capabilities.Tools == nil {
		t.Error("expected tools capability to be set")
	}
}

func TestBuildToolsListResult(t *testing.T) {
	handler := &testToolHandler{}
	result := buildToolsListResult(handler)

	if len(result.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result.Tools))
	}

	if result.Tools[0].Name != "add_numbers" {
		t.Errorf("expected tool name 'add_numbers', got %q", result.Tools[0].Name)
	}
}

func TestMCPConfig_Chaining_WithSDK(t *testing.T) {
	handler := &testToolHandler{}

	cfg := NewMCPConfig().
		AddStdioServer("fs", "npx", []string{"-y", "@mcp/server-fs"}).
		AddSDKServer("test", handler).
		AddHTTPServer("api", "http://localhost:8080")

	if len(cfg.MCPServers) != 3 {
		t.Errorf("expected 3 servers, got %d", len(cfg.MCPServers))
	}

	if _, ok := cfg.MCPServers["fs"]; !ok {
		t.Error("expected fs server")
	}
	if _, ok := cfg.MCPServers["test"]; !ok {
		t.Error("expected test server")
	}
	if _, ok := cfg.MCPServers["api"]; !ok {
		t.Error("expected api server")
	}

	handlers := cfg.SDKHandlers()
	if len(handlers) != 1 {
		t.Errorf("expected 1 SDK handler, got %d", len(handlers))
	}
}
