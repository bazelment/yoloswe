// Package builder implements the Builder sub-agent.
package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/protocol"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// Builder is an ephemeral agent that implements code based on designs.
type Builder struct {
	session *agent.EphemeralSession
	config  agent.AgentConfig
}

// New creates a new Builder agent.
func New(config agent.AgentConfig, swarmSessionID string) *Builder {
	config.Role = agent.RoleBuilder
	if config.SystemPrompt == "" {
		config.SystemPrompt = SystemPrompt
	}

	return &Builder{
		session: agent.NewEphemeralSession(config, swarmSessionID),
		config:  config,
	}
}

// Role returns the agent's role.
func (b *Builder) Role() agent.AgentRole {
	return agent.RoleBuilder
}

// SessionDir returns the base directory for task recordings.
func (b *Builder) SessionDir() string {
	return b.session.BaseSessionDir()
}

// TotalCost returns the accumulated cost.
func (b *Builder) TotalCost() float64 {
	return b.session.TotalCost()
}

// TaskCount returns the number of tasks executed.
func (b *Builder) TaskCount() int {
	return b.session.TaskCount()
}

// Execute runs a build task and returns the raw result.
func (b *Builder) Execute(ctx context.Context, prompt string) (*claude.TurnResult, string, error) {
	return b.session.Execute(ctx, prompt)
}

// ExecuteWithFiles runs a build task and returns the result with file tracking.
func (b *Builder) ExecuteWithFiles(ctx context.Context, prompt string) (*claude.TurnResult, *agent.ExecuteResult, string, error) {
	return b.session.ExecuteWithFiles(ctx, prompt)
}

// Build implements code based on the given request.
func (b *Builder) Build(ctx context.Context, req *protocol.BuildRequest) (*protocol.BuildResponse, string, error) {
	prompt := formatBuildPrompt(req)

	result, taskID, err := b.session.Execute(ctx, prompt)
	if err != nil {
		return nil, taskID, fmt.Errorf("build execution failed: %w", err)
	}

	if !result.Success {
		return nil, taskID, fmt.Errorf("build task failed: %v", result.Error)
	}

	// Parse response would need text from events
	response := &protocol.BuildResponse{}
	return response, taskID, nil
}

// formatBuildPrompt formats a BuildRequest into a prompt string.
func formatBuildPrompt(req *protocol.BuildRequest) string {
	var sb strings.Builder

	sb.WriteString("Please implement the following task based on the provided design.\n\n")

	sb.WriteString("## Task\n")
	sb.WriteString(req.Task)
	sb.WriteString("\n\n")

	sb.WriteString("## Working Directory\n")
	sb.WriteString(req.WorkDir)
	sb.WriteString("\n\n")

	if req.Design != nil {
		sb.WriteString("## Design\n")
		sb.WriteString("### Architecture\n")
		sb.WriteString(req.Design.Architecture)
		sb.WriteString("\n\n")

		if len(req.Design.Files) > 0 {
			sb.WriteString("### Files to Create/Modify\n")
			for _, f := range req.Design.Files {
				sb.WriteString(fmt.Sprintf("- %s (%s): %s\n", f.Path, f.Action, f.Purpose))
			}
			sb.WriteString("\n")
		}

		if req.Design.Interfaces != "" {
			sb.WriteString("### Interfaces\n")
			sb.WriteString("```\n")
			sb.WriteString(req.Design.Interfaces)
			sb.WriteString("\n```\n\n")
		}

		if len(req.Design.ImplementationNotes) > 0 {
			sb.WriteString("### Implementation Notes\n")
			for _, note := range req.Design.ImplementationNotes {
				sb.WriteString("- ")
				sb.WriteString(note)
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		}
	}

	if req.Feedback != nil && len(req.Feedback.Issues) > 0 {
		sb.WriteString("## Review Feedback to Address\n")
		for _, issue := range req.Feedback.Issues {
			sb.WriteString(fmt.Sprintf("- [%s] %s: %s\n", issue.Severity, issue.File, issue.Message))
			if issue.Suggestion != "" {
				sb.WriteString(fmt.Sprintf("  Suggestion: %s\n", issue.Suggestion))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("After implementing, run tests and provide a JSON summary of your work.\n")

	return sb.String()
}

// ParseBuildResponse parses a JSON string into a BuildResponse.
func ParseBuildResponse(text string) (*protocol.BuildResponse, error) {
	jsonStr := extractJSON(text)

	var response protocol.BuildResponse
	if err := json.Unmarshal([]byte(jsonStr), &response); err != nil {
		return nil, fmt.Errorf("failed to parse build response: %w", err)
	}

	return &response, nil
}

// extractJSON attempts to extract JSON from a string that might contain markdown.
func extractJSON(text string) string {
	// Check for <build_json> tag first (from MCP tool responses)
	if idx := strings.Index(text, "<build_json>"); idx != -1 {
		start := idx + len("<build_json>")
		if end := strings.Index(text[start:], "</build_json>"); end != -1 {
			return strings.TrimSpace(text[start : start+end])
		}
	}

	// Check if the text contains a JSON code block
	if idx := strings.Index(text, "```json"); idx != -1 {
		start := idx + 7
		end := strings.Index(text[start:], "```")
		if end != -1 {
			return strings.TrimSpace(text[start : start+end])
		}
	}

	// Check for generic code block
	if idx := strings.Index(text, "```"); idx != -1 {
		start := idx + 3
		if newline := strings.Index(text[start:], "\n"); newline != -1 {
			start += newline + 1
		}
		end := strings.Index(text[start:], "```")
		if end != -1 {
			return strings.TrimSpace(text[start : start+end])
		}
	}

	// Try to find JSON object directly
	if idx := strings.Index(text, "{"); idx != -1 {
		depth := 0
		for i := idx; i < len(text); i++ {
			switch text[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return text[idx : i+1]
				}
			}
		}
	}

	return text
}
