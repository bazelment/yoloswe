package protocol

import (
	"encoding/json"
	"testing"
)

func TestParseMCPMessageControlRequest(t *testing.T) {
	raw := json.RawMessage(`{"subtype":"mcp_message","server_name":"test-tools","message":{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}}`)

	reqData, err := ParseControlRequest(raw)
	if err != nil {
		t.Fatalf("ParseControlRequest failed: %v", err)
	}

	mcpReq, ok := reqData.(MCPMessageRequest)
	if !ok {
		t.Fatalf("expected MCPMessageRequest, got %T", reqData)
	}

	if mcpReq.Subtype() != ControlRequestSubtypeMCPMessage {
		t.Errorf("expected subtype %q, got %q", ControlRequestSubtypeMCPMessage, mcpReq.Subtype())
	}

	if mcpReq.ServerName != "test-tools" {
		t.Errorf("expected server_name 'test-tools', got %q", mcpReq.ServerName)
	}

	// Verify the message can be parsed as a JSON-RPC request
	var rpcReq JSONRPCRequest
	if err := json.Unmarshal(mcpReq.Message, &rpcReq); err != nil {
		t.Fatalf("failed to parse message as JSON-RPC: %v", err)
	}

	if rpcReq.Method != "initialize" {
		t.Errorf("expected method 'initialize', got %q", rpcReq.Method)
	}
}

func TestJSONRPCRequest_Marshal(t *testing.T) {
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"add_numbers","arguments":{"a":1,"b":2}}`),
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc '2.0', got %v", parsed["jsonrpc"])
	}
	if parsed["method"] != "tools/call" {
		t.Errorf("expected method 'tools/call', got %v", parsed["method"])
	}
}

func TestJSONRPCResponse_MarshalSuccess(t *testing.T) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      1,
		Result:  map[string]string{"status": "ok"},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc '2.0', got %v", parsed["jsonrpc"])
	}
	if parsed["error"] != nil {
		t.Error("error should be omitted for success response")
	}

	result := parsed["result"].(map[string]interface{})
	if result["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", result["status"])
	}
}

func TestJSONRPCResponse_MarshalError(t *testing.T) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      1,
		Error: &JSONRPCError{
			Code:    -32601,
			Message: "Method not found",
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	errorObj := parsed["error"].(map[string]interface{})
	if errorObj["code"].(float64) != -32601 {
		t.Errorf("expected error code -32601, got %v", errorObj["code"])
	}
	if errorObj["message"] != "Method not found" {
		t.Errorf("expected error message 'Method not found', got %v", errorObj["message"])
	}
}

func TestMCPToolDefinition_Marshal(t *testing.T) {
	tool := MCPToolDefinition{
		Name:        "add_numbers",
		Description: "Add two numbers together",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`),
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["name"] != "add_numbers" {
		t.Errorf("expected name 'add_numbers', got %v", parsed["name"])
	}
	if parsed["description"] != "Add two numbers together" {
		t.Errorf("expected description 'Add two numbers together', got %v", parsed["description"])
	}

	// Verify inputSchema is an object, not a string
	schema, ok := parsed["inputSchema"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected inputSchema to be object, got %T", parsed["inputSchema"])
	}
	if schema["type"] != "object" {
		t.Errorf("expected schema type 'object', got %v", schema["type"])
	}
}

func TestMCPToolCallResult_Marshal(t *testing.T) {
	result := MCPToolCallResult{
		Content: []MCPContentItem{
			{Type: "text", Text: "42"},
		},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	content := parsed["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(content))
	}

	item := content[0].(map[string]interface{})
	if item["type"] != "text" {
		t.Errorf("expected type 'text', got %v", item["type"])
	}
	if item["text"] != "42" {
		t.Errorf("expected text '42', got %v", item["text"])
	}
}

func TestMCPToolCallResult_Error(t *testing.T) {
	result := MCPToolCallResult{
		Content: []MCPContentItem{
			{Type: "text", Text: "something went wrong"},
		},
		IsError: true,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["isError"] != true {
		t.Errorf("expected isError true, got %v", parsed["isError"])
	}
}

func TestMCPInitializeResult_Marshal(t *testing.T) {
	result := MCPInitializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities: MCPServerCapabilities{
			Tools: &MCPToolsCapability{},
		},
		ServerInfo: MCPServerInfo{
			Name:    "test-server",
			Version: "1.0.0",
		},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["protocolVersion"] != "2024-11-05" {
		t.Errorf("expected protocolVersion '2024-11-05', got %v", parsed["protocolVersion"])
	}

	serverInfo := parsed["serverInfo"].(map[string]interface{})
	if serverInfo["name"] != "test-server" {
		t.Errorf("expected server name 'test-server', got %v", serverInfo["name"])
	}
}

func TestMCPToolsListResult_Marshal(t *testing.T) {
	result := MCPToolsListResult{
		Tools: []MCPToolDefinition{
			{
				Name:        "tool_a",
				Description: "Tool A",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
			{
				Name:        "tool_b",
				Description: "Tool B",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	tools := parsed["tools"].([]interface{})
	if len(tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(tools))
	}
}

func TestMCPResponsePayload_Marshal(t *testing.T) {
	rpcResp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      1,
		Result:  map[string]string{"ok": "true"},
	}

	payload := MCPResponsePayload{MCPResponse: rpcResp}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	mcpResp, ok := parsed["mcp_response"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected mcp_response to be object, got %T", parsed["mcp_response"])
	}

	if mcpResp["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc '2.0', got %v", mcpResp["jsonrpc"])
	}
}
