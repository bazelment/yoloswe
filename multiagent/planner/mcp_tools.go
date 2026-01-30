// Package planner implements the Planner agent that coordinates sub-agents.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/bazelment/yoloswe/multiagent/protocol"
)

// MCPServer provides an HTTP MCP server that exposes designer, builder, and reviewer tools.
// When Claude calls these tools, the server dispatches to the Planner's methods.
type MCPServer struct {
	planner  *Planner
	listener net.Listener
	server   *http.Server
	addr     string
	started  int32
}

// NewMCPServer creates a new MCP server bound to the given Planner.
func NewMCPServer(planner *Planner) *MCPServer {
	return &MCPServer{
		planner: planner,
	}
}

// Start starts the MCP HTTP server on a random available port.
// Returns the URL that should be used in --mcp-config.
func (s *MCPServer) Start() (string, error) {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		return s.addr, nil // Already started
	}

	// Listen on a random available port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("failed to listen: %w", err)
	}
	s.listener = listener
	s.addr = fmt.Sprintf("http://%s/mcp", listener.Addr().String())

	// Create the HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)

	s.server = &http.Server{Handler: mux}

	// Start serving in the background
	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			// Log error but don't crash - the Planner will notice when it can't connect
			fmt.Printf("MCP server error: %v\n", err)
		}
	}()

	return s.addr, nil
}

// Stop stops the MCP HTTP server.
func (s *MCPServer) Stop() error {
	if !atomic.CompareAndSwapInt32(&s.started, 1, 0) {
		return nil // Not started
	}

	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

// Addr returns the server's address (URL).
func (s *MCPServer) Addr() string {
	return s.addr
}

// handleMCP handles all MCP JSON-RPC requests.
func (s *MCPServer) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, nil, -32700, "Parse error", nil)
		return
	}

	var result interface{}
	var rpcErr *jsonRPCError

	switch req.Method {
	case "initialize":
		result = s.handleInitialize(req.Params)
	case "notifications/initialized":
		// Client acknowledgment, no response needed for notifications
		w.WriteHeader(http.StatusNoContent)
		return
	case "tools/list":
		result = s.handleToolsList()
	case "tools/call":
		result, rpcErr = s.handleToolsCall(r.Context(), req.Params)
	default:
		rpcErr = &jsonRPCError{Code: -32601, Message: "Method not found"}
	}

	if rpcErr != nil {
		s.writeError(w, req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}

	s.writeResult(w, req.ID, result)
}

// handleInitialize handles the MCP initialize request.
func (s *MCPServer) handleInitialize(params json.RawMessage) *initializeResult {
	return &initializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities: serverCapabilities{
			Tools: &toolsCapability{},
		},
		ServerInfo: serverInfo{
			Name:    "planner-tools",
			Version: "1.0.0",
		},
	}
}

// handleToolsList returns the list of available tools.
func (s *MCPServer) handleToolsList() *toolsListResult {
	return &toolsListResult{
		Tools: []toolDefinition{
			{
				Name:        "designer",
				Description: "Create a technical design for a task. Use this to analyze requirements and produce an architecture/design document before building.",
				InputSchema: jsonSchema{
					Type: "object",
					Properties: map[string]jsonSchema{
						"task": {
							Type:        "string",
							Description: "The task to design a solution for",
						},
						"context": {
							Type:        "string",
							Description: "Additional context about the codebase or requirements",
						},
						"constraints": {
							Type:        "array",
							Description: "Constraints or requirements to consider",
							Items:       &jsonSchema{Type: "string"},
						},
					},
					Required: []string{"task"},
				},
			},
			{
				Name:        "builder",
				Description: "Implement code changes based on a task and optional design. Use this to write, modify, or refactor code.",
				InputSchema: jsonSchema{
					Type: "object",
					Properties: map[string]jsonSchema{
						"task": {
							Type:        "string",
							Description: "The implementation task to perform",
						},
						"workdir": {
							Type:        "string",
							Description: "Working directory for the implementation",
						},
						"design": {
							Type:        "string",
							Description: "Design or architecture to follow (from designer)",
						},
					},
					Required: []string{"task"},
				},
			},
			{
				Name:        "reviewer",
				Description: "Review code changes for correctness, style, and adherence to design. Use this after building to verify the implementation.",
				InputSchema: jsonSchema{
					Type: "object",
					Properties: map[string]jsonSchema{
						"task": {
							Type:        "string",
							Description: "Description of what to review",
						},
						"files": {
							Type:        "array",
							Description: "List of files that were changed",
							Items:       &jsonSchema{Type: "string"},
						},
						"design": {
							Type:        "string",
							Description: "Original design to review against",
						},
					},
					Required: []string{"task"},
				},
			},
		},
	}
}

// handleToolsCall dispatches a tool call to the appropriate Planner method.
func (s *MCPServer) handleToolsCall(ctx context.Context, params json.RawMessage) (*toolCallResult, *jsonRPCError) {
	var req toolsCallRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &jsonRPCError{Code: -32602, Message: "Invalid params", Data: err.Error()}
	}

	switch req.Name {
	case "designer":
		return s.callDesigner(ctx, req.Arguments)
	case "builder":
		return s.callBuilder(ctx, req.Arguments)
	case "reviewer":
		return s.callReviewer(ctx, req.Arguments)
	default:
		return nil, &jsonRPCError{Code: -32602, Message: fmt.Sprintf("Unknown tool: %s", req.Name)}
	}
}

// callDesigner handles the designer tool call.
func (s *MCPServer) callDesigner(ctx context.Context, args json.RawMessage) (*toolCallResult, *jsonRPCError) {
	var input struct {
		Task        string   `json:"task"`
		Context     string   `json:"context"`
		Constraints []string `json:"constraints"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &jsonRPCError{Code: -32602, Message: "Invalid arguments", Data: err.Error()}
	}

	req := &protocol.DesignRequest{
		Task:        input.Task,
		Context:     input.Context,
		Constraints: input.Constraints,
	}

	resp, err := s.planner.CallDesigner(ctx, req)
	if err != nil {
		return &toolCallResult{
			Content: []contentItem{
				{Type: "text", Text: fmt.Sprintf("Designer failed: %v", err)},
			},
			IsError: true,
		}, nil
	}

	// Include structured JSON for parsing by Planner
	jsonBytes, _ := json.Marshal(resp)
	text := fmt.Sprintf("Design completed.\n\n<design_json>\n%s\n</design_json>\n\nSummary:\n%s",
		string(jsonBytes), resp.Architecture)

	return &toolCallResult{
		Content: []contentItem{
			{Type: "text", Text: text},
		},
	}, nil
}

// callBuilder handles the builder tool call.
func (s *MCPServer) callBuilder(ctx context.Context, args json.RawMessage) (*toolCallResult, *jsonRPCError) {
	var input struct {
		Task    string `json:"task"`
		WorkDir string `json:"workdir"`
		Design  string `json:"design"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &jsonRPCError{Code: -32602, Message: "Invalid arguments", Data: err.Error()}
	}

	workDir := input.WorkDir
	if workDir == "" {
		workDir = s.planner.config.WorkDir
	}

	req := &protocol.BuildRequest{
		Task:    input.Task,
		WorkDir: workDir,
	}

	if input.Design != "" {
		req.Design = &protocol.DesignResponse{
			Architecture: input.Design,
		}
	}

	resp, err := s.planner.CallBuilder(ctx, req)
	if err != nil {
		return &toolCallResult{
			Content: []contentItem{
				{Type: "text", Text: fmt.Sprintf("Builder failed: %v", err)},
			},
			IsError: true,
		}, nil
	}

	// Include structured JSON for parsing by Planner
	jsonBytes, _ := json.Marshal(resp)
	text := fmt.Sprintf("Build completed.\n\n<build_json>\n%s\n</build_json>\n\nFiles created: %v\nFiles modified: %v",
		string(jsonBytes), resp.FilesCreated, resp.FilesModified)

	return &toolCallResult{
		Content: []contentItem{
			{Type: "text", Text: text},
		},
	}, nil
}

// callReviewer handles the reviewer tool call.
func (s *MCPServer) callReviewer(ctx context.Context, args json.RawMessage) (*toolCallResult, *jsonRPCError) {
	var input struct {
		Task   string   `json:"task"`
		Design string   `json:"design"`
		Files  []string `json:"files"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, &jsonRPCError{Code: -32602, Message: "Invalid arguments", Data: err.Error()}
	}

	req := &protocol.ReviewRequest{
		Task:         input.Task,
		FilesChanged: input.Files,
	}

	if input.Design != "" {
		req.OriginalDesign = &protocol.DesignResponse{
			Architecture: input.Design,
		}
	}

	resp, err := s.planner.CallReviewer(ctx, req)
	if err != nil {
		return &toolCallResult{
			Content: []contentItem{
				{Type: "text", Text: fmt.Sprintf("Reviewer failed: %v", err)},
			},
			IsError: true,
		}, nil
	}

	// Include structured JSON for parsing by Planner
	jsonBytes, _ := json.Marshal(resp)
	approved := !resp.HasCriticalIssues()
	text := fmt.Sprintf("Review completed.\n\n<review_json>\n%s\n</review_json>\n\nApproved: %v", string(jsonBytes), approved)
	if resp.Summary != "" {
		text += fmt.Sprintf("\nSummary: %s", resp.Summary)
	}
	if len(resp.Issues) > 0 {
		text += "\nIssues found:"
		for _, issue := range resp.Issues {
			text += fmt.Sprintf("\n- [%s] %s: %s", issue.Severity, issue.File, issue.Message)
		}
	}

	return &toolCallResult{
		Content: []contentItem{
			{Type: "text", Text: text},
		},
	}, nil
}

// writeResult writes a successful JSON-RPC response.
func (s *MCPServer) writeResult(w http.ResponseWriter, id interface{}, result interface{}) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// writeError writes a JSON-RPC error response.
func (s *MCPServer) writeError(w http.ResponseWriter, id interface{}, code int, message string, data interface{}) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonRPCError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// JSON-RPC types

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	ID      interface{}   `json:"id,omitempty"`
	Result  interface{}   `json:"result,omitempty"`
	Error   *jsonRPCError `json:"error,omitempty"`
	JSONRPC string        `json:"jsonrpc"`
}

type jsonRPCError struct {
	Data    interface{} `json:"data,omitempty"`
	Message string      `json:"message"`
	Code    int         `json:"code"`
}

// MCP types

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfo         `json:"serverInfo"`
}

type serverCapabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolsListResult struct {
	Tools []toolDefinition `json:"tools"`
}

type toolDefinition struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	InputSchema jsonSchema `json:"inputSchema"`
}

type jsonSchema struct {
	Properties  map[string]jsonSchema `json:"properties,omitempty"`
	Items       *jsonSchema           `json:"items,omitempty"`
	Type        string                `json:"type"`
	Description string                `json:"description,omitempty"`
	Required    []string              `json:"required,omitempty"`
}

type toolsCallRequest struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolCallResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}
