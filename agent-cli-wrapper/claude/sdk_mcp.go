package claude

import (
	"context"
	"encoding/json"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// SDKToolHandler is the interface for handling MCP tool calls routed
// through the CLI's stdin/stdout control protocol (SDK MCP servers).
type SDKToolHandler interface {
	// Tools returns the tool definitions exposed by this handler.
	Tools() []protocol.MCPToolDefinition
	// HandleToolCall handles a tool invocation and returns the result.
	HandleToolCall(ctx context.Context, name string, args json.RawMessage) (*protocol.MCPToolCallResult, error)
}

// MCPSDKServerConfig is the MCP server config for SDK (type: "sdk") servers.
// The CLI routes MCP traffic through the existing stdin/stdout control protocol.
type MCPSDKServerConfig struct {
	Type MCPServerType `json:"type"`
}

func (c MCPSDKServerConfig) serverType() MCPServerType {
	return MCPServerTypeSDK
}

// MarshalJSON implements json.Marshaler.
func (c MCPSDKServerConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type MCPServerType `json:"type"`
	}{
		Type: MCPServerTypeSDK,
	})
}

// buildInitializeResult builds the MCP initialize response for an SDK server.
func buildInitializeResult(serverName string) *protocol.MCPInitializeResult {
	return &protocol.MCPInitializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities: protocol.MCPServerCapabilities{
			Tools: &protocol.MCPToolsCapability{},
		},
		ServerInfo: protocol.MCPServerInfo{
			Name:    serverName,
			Version: "1.0.0",
		},
	}
}

// buildToolsListResult builds the MCP tools/list response from an SDKToolHandler.
func buildToolsListResult(handler SDKToolHandler) *protocol.MCPToolsListResult {
	return &protocol.MCPToolsListResult{
		Tools: handler.Tools(),
	}
}
