package planner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	maprotocol "github.com/bazelment/yoloswe/multiagent/protocol"
)

// DesignerParams defines the parameters for the designer tool using typed approach.
type DesignerParams struct {
	Task        string   `json:"task" jsonschema:"required,description=The task to design a solution for"`
	Context     string   `json:"context,omitempty" jsonschema:"description=Additional context about the codebase or requirements"`
	Constraints []string `json:"constraints,omitempty" jsonschema:"description=Constraints or requirements to consider"`
}

// BuilderParams defines the parameters for the builder tool using typed approach.
type BuilderParams struct {
	Task    string `json:"task" jsonschema:"required,description=The implementation task to perform"`
	WorkDir string `json:"workdir,omitempty" jsonschema:"description=Working directory for the implementation"`
	Design  string `json:"design,omitempty" jsonschema:"description=Design or architecture to follow (from designer)"`
}

// ReviewerParams defines the parameters for the reviewer tool using typed approach.
type ReviewerParams struct {
	Task   string   `json:"task" jsonschema:"required,description=Description of what to review"`
	Files  []string `json:"files,omitempty" jsonschema:"description=List of files that were changed"`
	Design string   `json:"design,omitempty" jsonschema:"description=Original design to review against"`
}

// NewPlannerToolHandlerTyped creates a TypedToolRegistry-based handler for planner tools.
// This is a cleaner, type-safe alternative to the manual PlannerToolHandler implementation.
func NewPlannerToolHandlerTyped(planner *Planner) *claude.TypedToolRegistry {
	registry := claude.NewTypedToolRegistry()

	// Register designer tool
	claude.AddTool(registry, "designer",
		"Create a technical design for a task. Use this to analyze requirements and produce an architecture/design document before building.",
		func(ctx context.Context, params DesignerParams) (string, error) {
			req := &maprotocol.DesignRequest{
				Task:        params.Task,
				Context:     params.Context,
				Constraints: params.Constraints,
			}

			resp, err := planner.CallDesigner(ctx, req)
			if err != nil {
				return "", fmt.Errorf("designer failed: %w", err)
			}

			jsonBytes, _ := json.Marshal(resp)
			text := fmt.Sprintf("Design completed.\n\n<design_json>\n%s\n</design_json>\n\nSummary:\n%s",
				string(jsonBytes), resp.Architecture)

			return text, nil
		})

	// Register builder tool
	claude.AddTool(registry, "builder",
		"Implement code changes based on a task and optional design. Use this to write, modify, or refactor code.",
		func(ctx context.Context, params BuilderParams) (string, error) {
			workDir := params.WorkDir
			if workDir == "" {
				workDir = planner.config.WorkDir
			}

			req := &maprotocol.BuildRequest{
				Task:    params.Task,
				WorkDir: workDir,
			}

			if params.Design != "" {
				req.Design = &maprotocol.DesignResponse{
					Architecture: params.Design,
				}
			}

			resp, err := planner.CallBuilder(ctx, req)
			if err != nil {
				return "", fmt.Errorf("builder failed: %w", err)
			}

			jsonBytes, _ := json.Marshal(resp)
			text := fmt.Sprintf("Build completed.\n\n<build_json>\n%s\n</build_json>\n\nFiles created: %v\nFiles modified: %v",
				string(jsonBytes), resp.FilesCreated, resp.FilesModified)

			return text, nil
		})

	// Register reviewer tool
	claude.AddTool(registry, "reviewer",
		"Review code changes for correctness, style, and adherence to design. Use this after building to verify the implementation.",
		func(ctx context.Context, params ReviewerParams) (string, error) {
			req := &maprotocol.ReviewRequest{
				Task:         params.Task,
				FilesChanged: params.Files,
			}

			if params.Design != "" {
				req.OriginalDesign = &maprotocol.DesignResponse{
					Architecture: params.Design,
				}
			}

			resp, err := planner.CallReviewer(ctx, req)
			if err != nil {
				return "", fmt.Errorf("reviewer failed: %w", err)
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

			return text, nil
		})

	return registry
}

/*
Code Comparison: Manual vs Typed Approach

Original Implementation (mcp_tools.go):
- 254 lines of code
- Manual JSON schema strings (lines 30-93): 64 lines
- Switch statement dispatch (lines 100-114): 15 lines
- Three handler methods with manual unmarshaling (lines 117-253): 137 lines

Typed Implementation (this file):
- ~140 lines of code
- Struct definitions with tags (lines 13-30): 18 lines
- Tool registration (lines 34-136): 103 lines
- No manual unmarshaling, no switch statement

Boilerplate Eliminated:
- No manual JSON schemas (64 lines → 18 lines of structs with tags)
- No switch statement dispatch (15 lines → 0 lines)
- No manual unmarshaling (3 × ~10 lines = 30 lines → 0 lines)
- No manual result construction (simplified by returning strings)

Total Reduction: ~45% less code with better type safety
*/
