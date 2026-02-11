package acp

import (
	"encoding/json"
	"sync/atomic"
)

// ACP JSON-RPC method constants.
const (
	// Agent-provided methods (client sends, agent responds)
	MethodInitialize     = "initialize"
	MethodSessionNew     = "session/new"
	MethodSessionLoad    = "session/load"
	MethodSessionPrompt  = "session/prompt"
	MethodSessionSetMode = "session/set_mode"

	// Client-sent notifications
	MethodSessionCancel = "session/cancel"

	// Agent-sent notifications
	MethodSessionUpdate = "session/update"

	// Client-provided methods (agent sends, client responds)
	MethodRequestPermission = "session/request_permission"
	MethodFsReadTextFile    = "fs/read_text_file"
	MethodFsWriteTextFile   = "fs/write_text_file"
	MethodTerminalCreate    = "terminal/create"
	MethodTerminalOutput    = "terminal/output"
	MethodTerminalWaitExit  = "terminal/wait_for_exit"
	MethodTerminalKill      = "terminal/kill"
	MethodTerminalRelease   = "terminal/release"
)

// Session update type constants (Gemini CLI naming).
const (
	UpdateTypeAgentMessage      = "agent_message_chunk"
	UpdateTypeAgentThought      = "agent_thought_chunk"
	UpdateTypeToolCall          = "tool_call"
	UpdateTypeToolCallUpdate    = "tool_call_update"
	UpdateTypeToolCallResult    = "tool_call_result"
	UpdateTypePlanUpdate        = "plan_update"
	UpdateTypeAvailableCommands = "available_commands_update"
	UpdateTypeCurrentMode       = "current_mode_update"
	UpdateTypeConfigOption      = "config_option_update"
)

// JSONRPCRequest represents a JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      int64           `json:"id"`
}

// JSONRPCResponse represents a JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	Error   *JSONRPCError   `json:"error,omitempty"`
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	ID      int64           `json:"id"`
}

// JSONRPCNotification represents a JSON-RPC 2.0 notification (no id).
type JSONRPCNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCError represents a JSON-RPC 2.0 error.
type JSONRPCError struct {
	Data    interface{} `json:"data,omitempty"`
	Message string      `json:"message"`
	Code    int         `json:"code"`
}

// Standard JSON-RPC error codes.
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternalError  = -32603
)

// ACP-specific error codes.
const (
	ErrCodeResourceNotFound      = -32001
	ErrCodePermissionDenied      = -32002
	ErrCodeInvalidStateCode      = -32003
	ErrCodeCapabilityUnsupported = -32004
)

// idGenerator generates unique request IDs.
type idGenerator struct {
	next atomic.Int64
}

func (g *idGenerator) Next() int64 {
	return g.next.Add(1)
}

// newRequest creates a new JSON-RPC 2.0 request.
func newRequest(id int64, method string, params interface{}) (*JSONRPCRequest, error) {
	paramsData, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return &JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  paramsData,
	}, nil
}

// newResponse creates a new JSON-RPC 2.0 response.
func newResponse(id int64, result interface{}) (*JSONRPCResponse, error) {
	resultData, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  resultData,
	}, nil
}

// newErrorResponse creates a new JSON-RPC 2.0 error response.
func newErrorResponse(id int64, code int, message string) *JSONRPCResponse {
	return &JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &JSONRPCError{
			Code:    code,
			Message: message,
		},
	}
}

// newNotification creates a new JSON-RPC 2.0 notification.
func newNotification(method string, params interface{}) (*JSONRPCNotification, error) {
	paramsData, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return &JSONRPCNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramsData,
	}, nil
}
