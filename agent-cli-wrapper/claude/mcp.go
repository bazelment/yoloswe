package claude

import (
	"encoding/json"
)

// MCPServerType identifies the MCP server transport type.
type MCPServerType string

const (
	// MCPServerTypeStdio uses stdio transport (command + args).
	MCPServerTypeStdio MCPServerType = "stdio"
	// MCPServerTypeHTTP uses HTTP transport.
	MCPServerTypeHTTP MCPServerType = "http"
	// MCPServerTypeSSE uses Server-Sent Events transport.
	MCPServerTypeSSE MCPServerType = "sse"
)

// MCPServerConfig is the base interface for MCP server configurations.
type MCPServerConfig interface {
	serverType() MCPServerType
}

// MCPStdioServerConfig configures a stdio-based MCP server.
type MCPStdioServerConfig struct {
	Type    MCPServerType     `json:"type,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

func (c MCPStdioServerConfig) serverType() MCPServerType {
	if c.Type == "" {
		return MCPServerTypeStdio
	}
	return c.Type
}

// MarshalJSON implements json.Marshaler.
func (c MCPStdioServerConfig) MarshalJSON() ([]byte, error) {
	type alias MCPStdioServerConfig
	a := alias(c)
	if a.Type == "" {
		a.Type = MCPServerTypeStdio
	}
	return json.Marshal(a)
}

// MCPHTTPServerConfig configures an HTTP-based MCP server.
type MCPHTTPServerConfig struct {
	Type    MCPServerType     `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

func (c MCPHTTPServerConfig) serverType() MCPServerType {
	return MCPServerTypeHTTP
}

// MCPSSEServerConfig configures an SSE-based MCP server.
type MCPSSEServerConfig struct {
	Type    MCPServerType     `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

func (c MCPSSEServerConfig) serverType() MCPServerType {
	return MCPServerTypeSSE
}

// MCPConfig holds the MCP server configuration for a session.
// The keys are server names, values are server configurations.
type MCPConfig struct {
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
}

// MarshalJSON implements json.Marshaler for MCPConfig.
func (c MCPConfig) MarshalJSON() ([]byte, error) {
	// Convert MCPServerConfig interface values to their concrete types
	servers := make(map[string]interface{})
	for name, cfg := range c.MCPServers {
		servers[name] = cfg
	}
	return json.Marshal(map[string]interface{}{
		"mcpServers": servers,
	})
}

// NewMCPConfig creates a new MCP configuration.
func NewMCPConfig() *MCPConfig {
	return &MCPConfig{
		MCPServers: make(map[string]MCPServerConfig),
	}
}

// AddStdioServer adds a stdio-based MCP server.
func (c *MCPConfig) AddStdioServer(name, command string, args []string) *MCPConfig {
	c.MCPServers[name] = MCPStdioServerConfig{
		Type:    MCPServerTypeStdio,
		Command: command,
		Args:    args,
	}
	return c
}

// AddHTTPServer adds an HTTP-based MCP server.
func (c *MCPConfig) AddHTTPServer(name, url string) *MCPConfig {
	c.MCPServers[name] = MCPHTTPServerConfig{
		Type: MCPServerTypeHTTP,
		URL:  url,
	}
	return c
}

// AddSSEServer adds an SSE-based MCP server.
func (c *MCPConfig) AddSSEServer(name, url string) *MCPConfig {
	c.MCPServers[name] = MCPSSEServerConfig{
		Type: MCPServerTypeSSE,
		URL:  url,
	}
	return c
}
