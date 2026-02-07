// Package planner implements the Planner agent that coordinates sub-agents.
package planner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
	maprotocol "github.com/bazelment/yoloswe/multiagent/protocol"
)

// PlannerToolHandler implements claude.SDKToolHandler to expose
// designer, builder, and reviewer tools via SDK MCP.
type PlannerToolHandler struct {
	planner *Planner
}

// NewPlannerToolHandler creates a new PlannerToolHandler bound to the given Planner.
func NewPlannerToolHandler(planner *Planner) *PlannerToolHandler {
	return &PlannerToolHandler{planner: planner}
}

// Tools returns the MCP tool definitions for the planner's sub-agents.
func (h *PlannerToolHandler) Tools() []protocol.MCPToolDefinition {
	return []protocol.MCPToolDefinition{
		{
			Name:        "designer",
			Description: "Create a technical design for a task. Use this to analyze requirements and produce an architecture/design document before building.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"task": {
						"type": "string",
						"description": "The task to design a solution for"
					},
					"context": {
						"type": "string",
						"description": "Additional context about the codebase or requirements"
					},
					"constraints": {
						"type": "array",
						"description": "Constraints or requirements to consider",
						"items": {"type": "string"}
					}
				},
				"required": ["task"]
			}`),
		},
		{
			Name:        "builder",
			Description: "Implement code changes based on a task and optional design. Use this to write, modify, or refactor code.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"task": {
						"type": "string",
						"description": "The implementation task to perform"
					},
					"workdir": {
						"type": "string",
						"description": "Working directory for the implementation"
					},
					"design": {
						"type": "string",
						"description": "Design or architecture to follow (from designer)"
					}
				},
				"required": ["task"]
			}`),
		},
		{
			Name:        "reviewer",
			Description: "Review code changes for correctness, style, and adherence to design. Use this after building to verify the implementation.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"task": {
						"type": "string",
						"description": "Description of what to review"
					},
					"files": {
						"type": "array",
						"description": "List of files that were changed",
						"items": {"type": "string"}
					},
					"design": {
						"type": "string",
						"description": "Original design to review against"
					}
				},
				"required": ["task"]
			}`),
		},
	}
}

// HandleToolCall dispatches a tool call to the appropriate Planner method.
func (h *PlannerToolHandler) HandleToolCall(ctx context.Context, name string, args json.RawMessage) (*protocol.MCPToolCallResult, error) {
	switch name {
	case "designer":
		return h.callDesigner(ctx, args)
	case "builder":
		return h.callBuilder(ctx, args)
	case "reviewer":
		return h.callReviewer(ctx, args)
	default:
		return &protocol.MCPToolCallResult{
			Content: []protocol.MCPContentItem{
				{Type: "text", Text: fmt.Sprintf("Unknown tool: %s", name)},
			},
			IsError: true,
		}, nil
	}
}

// callDesigner handles the designer tool call.
func (h *PlannerToolHandler) callDesigner(ctx context.Context, args json.RawMessage) (*protocol.MCPToolCallResult, error) {
	var input struct {
		Task        string   `json:"task"`
		Context     string   `json:"context"`
		Constraints []string `json:"constraints"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	req := &maprotocol.DesignRequest{
		Task:        input.Task,
		Context:     input.Context,
		Constraints: input.Constraints,
	}

	resp, err := h.planner.CallDesigner(ctx, req)
	if err != nil {
		return &protocol.MCPToolCallResult{
			Content: []protocol.MCPContentItem{
				{Type: "text", Text: fmt.Sprintf("Designer failed: %v", err)},
			},
			IsError: true,
		}, nil
	}

	jsonBytes, _ := json.Marshal(resp)
	text := fmt.Sprintf("Design completed.\n\n<design_json>\n%s\n</design_json>\n\nSummary:\n%s",
		string(jsonBytes), resp.Architecture)

	return &protocol.MCPToolCallResult{
		Content: []protocol.MCPContentItem{
			{Type: "text", Text: text},
		},
	}, nil
}

// callBuilder handles the builder tool call.
func (h *PlannerToolHandler) callBuilder(ctx context.Context, args json.RawMessage) (*protocol.MCPToolCallResult, error) {
	var input struct {
		Task    string `json:"task"`
		WorkDir string `json:"workdir"`
		Design  string `json:"design"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	workDir := input.WorkDir
	if workDir == "" {
		workDir = h.planner.config.WorkDir
	}

	req := &maprotocol.BuildRequest{
		Task:    input.Task,
		WorkDir: workDir,
	}

	if input.Design != "" {
		req.Design = &maprotocol.DesignResponse{
			Architecture: input.Design,
		}
	}

	resp, err := h.planner.CallBuilder(ctx, req)
	if err != nil {
		return &protocol.MCPToolCallResult{
			Content: []protocol.MCPContentItem{
				{Type: "text", Text: fmt.Sprintf("Builder failed: %v", err)},
			},
			IsError: true,
		}, nil
	}

	jsonBytes, _ := json.Marshal(resp)
	text := fmt.Sprintf("Build completed.\n\n<build_json>\n%s\n</build_json>\n\nFiles created: %v\nFiles modified: %v",
		string(jsonBytes), resp.FilesCreated, resp.FilesModified)

	return &protocol.MCPToolCallResult{
		Content: []protocol.MCPContentItem{
			{Type: "text", Text: text},
		},
	}, nil
}

// callReviewer handles the reviewer tool call.
func (h *PlannerToolHandler) callReviewer(ctx context.Context, args json.RawMessage) (*protocol.MCPToolCallResult, error) {
	var input struct {
		Task   string   `json:"task"`
		Design string   `json:"design"`
		Files  []string `json:"files"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	req := &maprotocol.ReviewRequest{
		Task:         input.Task,
		FilesChanged: input.Files,
	}

	if input.Design != "" {
		req.OriginalDesign = &maprotocol.DesignResponse{
			Architecture: input.Design,
		}
	}

	resp, err := h.planner.CallReviewer(ctx, req)
	if err != nil {
		return &protocol.MCPToolCallResult{
			Content: []protocol.MCPContentItem{
				{Type: "text", Text: fmt.Sprintf("Reviewer failed: %v", err)},
			},
			IsError: true,
		}, nil
	}

	jsonBytes, _ := json.Marshal(resp)
	approved := !resp.HasCriticalIssues()
	text := fmt.Sprintf("Review completed.\n\n<review_json>\n%s\n</review_json>\n\nApproved: %v", string(jsonBytes), approved)
	if resp.Summary != "" {
		text += fmt.Sprintf("\nSummary: %s", resp.Summary)
	}
	if len(resp.Issues) > 0 {
		text += "\nIssues found:"
		for i := range resp.Issues {
			text += fmt.Sprintf("\n- [%s] %s: %s", resp.Issues[i].Severity, resp.Issues[i].File, resp.Issues[i].Message)
		}
	}

	return &protocol.MCPToolCallResult{
		Content: []protocol.MCPContentItem{
			{Type: "text", Text: text},
		},
	}, nil
}
