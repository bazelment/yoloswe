package protocol

import "encoding/json"

// MCPMessageRequest is a control request with subtype "mcp_message".
// It wraps a JSON-RPC message from the CLI to an SDK MCP server.
type MCPMessageRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
	ServerName   string                `json:"server_name"`
	Message      json.RawMessage       `json:"message"`
}

// Subtype returns the control request subtype.
func (r MCPMessageRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// JSONRPCRequest is a standard JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	ID      interface{}     `json:"id,omitempty"`
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse is a standard JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	ID      interface{}   `json:"id,omitempty"`
	Result  interface{}   `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
	JSONRPC string        `json:"jsonrpc"`
}

// JSONRPCError is a JSON-RPC 2.0 error object.
type JSONRPCError struct {
	Data    interface{} `json:"data,omitempty"`
	Message string      `json:"message"`
	Code    int         `json:"code"`
}

// MCPToolDefinition describes an MCP tool exposed by an SDK server.
type MCPToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPToolCallResult is the result of an MCP tools/call invocation.
type MCPToolCallResult struct {
	Content []MCPContentItem `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

// MCPContentItem is a single content item in a tool call result.
type MCPContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// MCPInitializeResult is the MCP initialize response payload.
type MCPInitializeResult struct {
	ProtocolVersion string                `json:"protocolVersion"`
	Capabilities    MCPServerCapabilities `json:"capabilities"`
	ServerInfo      MCPServerInfo         `json:"serverInfo"`
}

// MCPServerCapabilities describes server capabilities.
type MCPServerCapabilities struct {
	Tools *MCPToolsCapability `json:"tools,omitempty"`
}

// MCPToolsCapability describes tools capability.
type MCPToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// MCPServerInfo describes the server identity.
type MCPServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// MCPToolsListResult is the MCP tools/list response payload.
type MCPToolsListResult struct {
	Tools []MCPToolDefinition `json:"tools"`
}

// MCPToolsCallParams is the params for a tools/call JSON-RPC request.
type MCPToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// MCPResponsePayload wraps an mcp_response inside a control_response.
type MCPResponsePayload struct {
	MCPResponse interface{} `json:"mcp_response"`
}
