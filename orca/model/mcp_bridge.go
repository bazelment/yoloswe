package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// ToolCallObserver is notified after each MCP tool call completes.
type ToolCallObserver func(name string, args json.RawMessage, output string, isError bool)

// MCPToolBridge adapts a ToolRegistry to claude.SDKToolHandler and an
// HTTP MCP JSON-RPC 2.0 endpoint (for Codex app-server).
type MCPToolBridge struct {
	registry       *ToolRegistry
	wireToOriginal map[string]string
	observer       ToolCallObserver
	maxCalls       int
	callCount      int
	mu             sync.RWMutex
	execMu         sync.Mutex
}

var _ claude.SDKToolHandler = (*MCPToolBridge)(nil)
var _ http.Handler = (*MCPToolBridge)(nil)

// NewMCPToolBridge creates an MCPToolBridge backed by reg.
func NewMCPToolBridge(reg *ToolRegistry) *MCPToolBridge {
	b := &MCPToolBridge{
		wireToOriginal: make(map[string]string),
	}
	b.SetRegistry(reg)
	return b
}

// SetRegistry updates the active ToolRegistry and resets the per-turn call counter.
func (b *MCPToolBridge) SetRegistry(reg *ToolRegistry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.registry = reg
	b.callCount = 0
	if reg == nil {
		return
	}
	for _, def := range reg.Definitions() {
		wire := encodeToolName(def.Name)
		b.wireToOriginal[wire] = def.Name
		b.wireToOriginal[def.Name] = def.Name
	}
}

// SetMaxCalls sets the maximum number of tool calls allowed in the current turn (0 = unlimited).
func (b *MCPToolBridge) SetMaxCalls(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maxCalls = n
}

// CallCount returns the number of tool calls attempted in the current turn.
func (b *MCPToolBridge) CallCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.callCount
}

// SetObserver sets the callback invoked after each tool execution.
func (b *MCPToolBridge) SetObserver(obs ToolCallObserver) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observer = obs
}

// WireToolName returns the MCP-safe wire name for a canonical tool name.
func WireToolName(name string) string {
	return encodeToolName(name)
}

// CanonicalToolName resolves a wire or MCP-prefixed tool name back to its
// canonical ToolRegistry name.
func (b *MCPToolBridge) CanonicalToolName(name string) string {
	trimmed := strings.TrimSpace(name)
	if strings.HasPrefix(trimmed, "mcp__") {
		parts := strings.SplitN(trimmed, "__", 3)
		if len(parts) == 3 {
			trimmed = parts[2]
		}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if orig, ok := b.wireToOriginal[trimmed]; ok {
		return orig
	}
	return trimmed
}

// Tools returns the MCP tool definitions for all tools in the current registry.
func (b *MCPToolBridge) Tools() []protocol.MCPToolDefinition {
	b.mu.RLock()
	reg := b.registry
	b.mu.RUnlock()
	if reg == nil {
		return nil
	}
	defs := reg.Definitions()
	out := make([]protocol.MCPToolDefinition, 0, len(defs))
	for _, d := range defs {
		schema := d.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, protocol.MCPToolDefinition{
			Name:        encodeToolName(d.Name),
			Description: d.Description,
			InputSchema: schema,
		})
	}
	return out
}

// HandleToolCall executes a tool call against the active ToolRegistry.
// Concurrency ordering: execMu is acquired before mu.Lock so at most one tool
// handler and its observer run at a time across concurrent SDK or HTTP requests.
func (b *MCPToolBridge) HandleToolCall(
	ctx context.Context,
	name string,
	args json.RawMessage,
) (*protocol.MCPToolCallResult, error) {
	b.execMu.Lock()
	defer b.execMu.Unlock()

	canonical := b.CanonicalToolName(name)
	b.mu.Lock()
	reg := b.registry
	obs := b.observer
	maxCalls := b.maxCalls
	if reg != nil {
		b.callCount++
	}
	curCall := b.callCount
	b.mu.Unlock()

	if reg == nil {
		errText := fmt.Sprintf("no tool registry active for tool %q", canonical)
		return &protocol.MCPToolCallResult{
			Content: []protocol.MCPContentItem{{Type: "text", Text: errText}},
			IsError: true,
		}, nil
	}
	if maxCalls > 0 && curCall > maxCalls {
		errText := fmt.Sprintf("max tool calls per turn (%d) exceeded; complete your turn without further tool calls", maxCalls)
		return &protocol.MCPToolCallResult{
			Content: []protocol.MCPContentItem{{Type: "text", Text: errText}},
			IsError: true,
		}, nil
	}

	resBlock := reg.Execute(ctx, ToolCall{
		Name:  canonical,
		Input: args,
	})
	if obs != nil {
		obs(canonical, args, resBlock.Content, resBlock.IsError)
	}
	return &protocol.MCPToolCallResult{
		Content: []protocol.MCPContentItem{{Type: "text", Text: resBlock.Content}},
		IsError: resBlock.IsError,
	}, nil
}

// ServeHTTP implements a Streamable HTTP MCP JSON-RPC 2.0 endpoint so CLIs like
// Codex app-server can invoke the same in-process ToolRegistry over localhost.
func (b *MCPToolBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if strings.TrimSpace(r.Header.Get("Origin")) != "" {
		http.Error(w, "cross-origin requests not permitted", http.StatusForbidden)
		return
	}
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req protocol.JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var resp *protocol.JSONRPCResponse
	switch req.Method {
	case "initialize":
		resp = &protocol.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: protocol.MCPInitializeResult{
				ProtocolVersion: "2024-11-05",
				Capabilities: protocol.MCPServerCapabilities{
					Tools: &protocol.MCPToolsCapability{ListChanged: true},
				},
				ServerInfo: protocol.MCPServerInfo{
					Name:    "orca",
					Version: "1.0.0",
				},
			},
		}
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "tools/list":
		resp = &protocol.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: protocol.MCPToolsListResult{
				Tools: b.Tools(),
			},
		}
	case "tools/call":
		var params protocol.MCPToolsCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp = &protocol.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &protocol.JSONRPCError{
					Code:    -32602,
					Message: fmt.Sprintf("invalid params: %v", err),
				},
			}
		} else {
			res, callErr := b.HandleToolCall(r.Context(), params.Name, params.Arguments)
			if callErr != nil {
				res = &protocol.MCPToolCallResult{
					Content: []protocol.MCPContentItem{{Type: "text", Text: callErr.Error()}},
					IsError: true,
				}
			}
			resp = &protocol.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  res,
			}
		}
	default:
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp = &protocol.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &protocol.JSONRPCError{
				Code:    -32601,
				Message: fmt.Sprintf("method not found: %s", req.Method),
			},
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
