package claude

import (
	"encoding/json"
	"testing"
)

func TestMCPServerType_Constants(t *testing.T) {
	// Verify the string values match expected JSON values
	if MCPServerTypeStdio != "stdio" {
		t.Errorf("expected MCPServerTypeStdio to be 'stdio', got %q", MCPServerTypeStdio)
	}
	if MCPServerTypeHTTP != "http" {
		t.Errorf("expected MCPServerTypeHTTP to be 'http', got %q", MCPServerTypeHTTP)
	}
	if MCPServerTypeSSE != "sse" {
		t.Errorf("expected MCPServerTypeSSE to be 'sse', got %q", MCPServerTypeSSE)
	}
}

func TestMCPStdioServerConfig_ServerType(t *testing.T) {
	// Without explicit type
	cfg := MCPStdioServerConfig{
		Command: "node",
		Args:    []string{"server.js"},
	}
	if cfg.serverType() != MCPServerTypeStdio {
		t.Errorf("expected serverType() to return 'stdio', got %q", cfg.serverType())
	}

	// With explicit type
	cfg.Type = MCPServerTypeStdio
	if cfg.serverType() != MCPServerTypeStdio {
		t.Errorf("expected serverType() to return 'stdio', got %q", cfg.serverType())
	}
}

func TestMCPStdioServerConfig_MarshalJSON(t *testing.T) {
	cfg := MCPStdioServerConfig{
		Command: "npx",
		Args:    []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
		Env:     map[string]string{"DEBUG": "true"},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Unmarshal to verify structure
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// Verify type is set even when not explicitly provided
	if result["type"] != "stdio" {
		t.Errorf("expected type 'stdio', got %v", result["type"])
	}
	if result["command"] != "npx" {
		t.Errorf("expected command 'npx', got %v", result["command"])
	}

	// Verify args array
	args, ok := result["args"].([]interface{})
	if !ok {
		t.Fatalf("expected args to be array, got %T", result["args"])
	}
	if len(args) != 3 {
		t.Errorf("expected 3 args, got %d", len(args))
	}

	// Verify env map
	env, ok := result["env"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected env to be map, got %T", result["env"])
	}
	if env["DEBUG"] != "true" {
		t.Errorf("expected env DEBUG=true, got %v", env["DEBUG"])
	}
}

func TestMCPHTTPServerConfig_ServerType(t *testing.T) {
	cfg := MCPHTTPServerConfig{
		Type: MCPServerTypeHTTP,
		URL:  "http://localhost:8080/mcp",
	}
	if cfg.serverType() != MCPServerTypeHTTP {
		t.Errorf("expected serverType() to return 'http', got %q", cfg.serverType())
	}
}

func TestMCPSSEServerConfig_ServerType(t *testing.T) {
	cfg := MCPSSEServerConfig{
		Type: MCPServerTypeSSE,
		URL:  "http://localhost:8080/sse",
	}
	if cfg.serverType() != MCPServerTypeSSE {
		t.Errorf("expected serverType() to return 'sse', got %q", cfg.serverType())
	}
}

func TestNewMCPConfig(t *testing.T) {
	cfg := NewMCPConfig()
	if cfg == nil {
		t.Fatal("NewMCPConfig returned nil")
	}
	if cfg.MCPServers == nil {
		t.Error("MCPServers map should be initialized")
	}
	if len(cfg.MCPServers) != 0 {
		t.Errorf("expected empty MCPServers map, got %d entries", len(cfg.MCPServers))
	}
}

func TestMCPConfig_AddStdioServer(t *testing.T) {
	cfg := NewMCPConfig()
	result := cfg.AddStdioServer("filesystem", "npx", []string{"-y", "@mcp/server-filesystem"})

	// Should return self for chaining
	if result != cfg {
		t.Error("AddStdioServer should return self for chaining")
	}

	// Verify server was added
	server, ok := cfg.MCPServers["filesystem"]
	if !ok {
		t.Fatal("expected filesystem server to be added")
	}

	stdio, ok := server.(MCPStdioServerConfig)
	if !ok {
		t.Fatalf("expected MCPStdioServerConfig, got %T", server)
	}

	if stdio.Type != MCPServerTypeStdio {
		t.Errorf("expected type 'stdio', got %q", stdio.Type)
	}
	if stdio.Command != "npx" {
		t.Errorf("expected command 'npx', got %q", stdio.Command)
	}
	if len(stdio.Args) != 2 {
		t.Errorf("expected 2 args, got %d", len(stdio.Args))
	}
}

func TestMCPConfig_AddHTTPServer(t *testing.T) {
	cfg := NewMCPConfig()
	result := cfg.AddHTTPServer("api", "http://localhost:8080/mcp")

	// Should return self for chaining
	if result != cfg {
		t.Error("AddHTTPServer should return self for chaining")
	}

	// Verify server was added
	server, ok := cfg.MCPServers["api"]
	if !ok {
		t.Fatal("expected api server to be added")
	}

	http, ok := server.(MCPHTTPServerConfig)
	if !ok {
		t.Fatalf("expected MCPHTTPServerConfig, got %T", server)
	}

	if http.Type != MCPServerTypeHTTP {
		t.Errorf("expected type 'http', got %q", http.Type)
	}
	if http.URL != "http://localhost:8080/mcp" {
		t.Errorf("expected URL 'http://localhost:8080/mcp', got %q", http.URL)
	}
}

func TestMCPConfig_AddSSEServer(t *testing.T) {
	cfg := NewMCPConfig()
	result := cfg.AddSSEServer("stream", "http://localhost:8080/sse")

	// Should return self for chaining
	if result != cfg {
		t.Error("AddSSEServer should return self for chaining")
	}

	// Verify server was added
	server, ok := cfg.MCPServers["stream"]
	if !ok {
		t.Fatal("expected stream server to be added")
	}

	sse, ok := server.(MCPSSEServerConfig)
	if !ok {
		t.Fatalf("expected MCPSSEServerConfig, got %T", server)
	}

	if sse.Type != MCPServerTypeSSE {
		t.Errorf("expected type 'sse', got %q", sse.Type)
	}
	if sse.URL != "http://localhost:8080/sse" {
		t.Errorf("expected URL 'http://localhost:8080/sse', got %q", sse.URL)
	}
}

func TestMCPConfig_Chaining(t *testing.T) {
	cfg := NewMCPConfig().
		AddStdioServer("filesystem", "npx", []string{"-y", "@mcp/server-fs"}).
		AddHTTPServer("api", "http://localhost:8080").
		AddSSEServer("events", "http://localhost:8080/sse")

	if len(cfg.MCPServers) != 3 {
		t.Errorf("expected 3 servers, got %d", len(cfg.MCPServers))
	}

	// Verify all servers exist
	if _, ok := cfg.MCPServers["filesystem"]; !ok {
		t.Error("expected filesystem server")
	}
	if _, ok := cfg.MCPServers["api"]; !ok {
		t.Error("expected api server")
	}
	if _, ok := cfg.MCPServers["events"]; !ok {
		t.Error("expected events server")
	}
}

func TestMCPConfig_MarshalJSON(t *testing.T) {
	cfg := NewMCPConfig().
		AddStdioServer("filesystem", "npx", []string{"-y", "@mcp/server-fs"}).
		AddHTTPServer("api", "http://localhost:8080")

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Unmarshal to verify structure
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// Verify mcpServers key exists
	servers, ok := result["mcpServers"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected mcpServers to be map, got %T", result["mcpServers"])
	}

	if len(servers) != 2 {
		t.Errorf("expected 2 servers, got %d", len(servers))
	}

	// Verify filesystem server
	fs, ok := servers["filesystem"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected filesystem to be map, got %T", servers["filesystem"])
	}
	if fs["type"] != "stdio" {
		t.Errorf("expected filesystem type 'stdio', got %v", fs["type"])
	}
	if fs["command"] != "npx" {
		t.Errorf("expected filesystem command 'npx', got %v", fs["command"])
	}

	// Verify api server
	api, ok := servers["api"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected api to be map, got %T", servers["api"])
	}
	if api["type"] != "http" {
		t.Errorf("expected api type 'http', got %v", api["type"])
	}
	if api["url"] != "http://localhost:8080" {
		t.Errorf("expected api url 'http://localhost:8080', got %v", api["url"])
	}
}

func TestMCPConfig_MarshalJSON_Empty(t *testing.T) {
	cfg := NewMCPConfig()

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Unmarshal to verify structure
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// Verify mcpServers key exists and is empty
	servers, ok := result["mcpServers"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected mcpServers to be map, got %T", result["mcpServers"])
	}

	if len(servers) != 0 {
		t.Errorf("expected empty mcpServers, got %d entries", len(servers))
	}
}

func TestMCPHTTPServerConfig_WithHeaders(t *testing.T) {
	cfg := MCPHTTPServerConfig{
		Type:    MCPServerTypeHTTP,
		URL:     "http://localhost:8080/mcp",
		Headers: map[string]string{"Authorization": "Bearer token123"},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	headers, ok := result["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected headers to be map, got %T", result["headers"])
	}

	if headers["Authorization"] != "Bearer token123" {
		t.Errorf("expected Authorization header, got %v", headers["Authorization"])
	}
}

func TestMCPSSEServerConfig_WithHeaders(t *testing.T) {
	cfg := MCPSSEServerConfig{
		Type:    MCPServerTypeSSE,
		URL:     "http://localhost:8080/sse",
		Headers: map[string]string{"X-API-Key": "secret"},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	headers, ok := result["headers"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected headers to be map, got %T", result["headers"])
	}

	if headers["X-API-Key"] != "secret" {
		t.Errorf("expected X-API-Key header, got %v", headers["X-API-Key"])
	}
}
