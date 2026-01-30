// Package designer implements the Designer sub-agent.
package designer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

// Designer is an ephemeral agent that creates technical designs.
type Designer struct {
	session *agent.EphemeralSession
	config  agent.AgentConfig
}

// New creates a new Designer agent.
func New(config agent.AgentConfig, swarmSessionID string) *Designer {
	config.Role = agent.RoleDesigner
	if config.SystemPrompt == "" {
		config.SystemPrompt = SystemPrompt
	}

	return &Designer{
		session: agent.NewEphemeralSession(config, swarmSessionID),
		config:  config,
	}
}

// Role returns the agent's role.
func (d *Designer) Role() agent.AgentRole {
	return agent.RoleDesigner
}

// SessionDir returns the base directory for task recordings.
func (d *Designer) SessionDir() string {
	return d.session.BaseSessionDir()
}

// TotalCost returns the accumulated cost.
func (d *Designer) TotalCost() float64 {
	return d.session.TotalCost()
}

// TaskCount returns the number of tasks executed.
func (d *Designer) TaskCount() int {
	return d.session.TaskCount()
}

// Execute runs a design task and returns a DesignResponse.
func (d *Designer) Execute(ctx context.Context, prompt string) (*claude.TurnResult, string, error) {
	return d.session.Execute(ctx, prompt)
}

// ExecuteWithFiles runs a design task and returns the result with file tracking.
func (d *Designer) ExecuteWithFiles(ctx context.Context, prompt string) (*claude.TurnResult, *agent.ExecuteResult, string, error) {
	return d.session.ExecuteWithFiles(ctx, prompt)
}

// Design creates a technical design for the given request.
func (d *Designer) Design(ctx context.Context, req *protocol.DesignRequest) (*protocol.DesignResponse, string, error) {
	// Format the prompt
	prompt := formatDesignPrompt(req)

	// Execute the design task
	result, taskID, err := d.session.Execute(ctx, prompt)
	if err != nil {
		return nil, taskID, fmt.Errorf("design execution failed: %w", err)
	}

	if !result.Success {
		return nil, taskID, fmt.Errorf("design task failed: %v", result.Error)
	}

	// Parse the response - we need to extract text from the result
	// The SDK doesn't directly expose the text, so we'll need to get it from the recording
	// For now, we'll return a placeholder and let the caller handle text extraction
	response := &protocol.DesignResponse{}

	return response, taskID, nil
}

// DesignWithText creates a technical design and returns both the parsed response and raw text.
func (d *Designer) DesignWithText(ctx context.Context, req *protocol.DesignRequest) (*protocol.DesignResponse, string, string, error) {
	prompt := formatDesignPrompt(req)

	result, taskID, err := d.session.Execute(ctx, prompt)
	if err != nil {
		return nil, "", taskID, fmt.Errorf("design execution failed: %w", err)
	}

	if !result.Success {
		return nil, "", taskID, fmt.Errorf("design task failed: %v", result.Error)
	}

	// We need to collect the text from the session
	// This is a limitation of the current approach - the text needs to be
	// collected during execution via events
	// For now, return empty and let higher-level code handle this
	return nil, "", taskID, nil
}

// formatDesignPrompt formats a DesignRequest into a prompt string.
func formatDesignPrompt(req *protocol.DesignRequest) string {
	var sb strings.Builder

	sb.WriteString("Please create a technical design for the following task.\n\n")
	sb.WriteString("## Task\n")
	sb.WriteString(req.Task)
	sb.WriteString("\n\n")

	if req.Context != "" {
		sb.WriteString("## Context\n")
		sb.WriteString(req.Context)
		sb.WriteString("\n\n")
	}

	if len(req.Constraints) > 0 {
		sb.WriteString("## Constraints\n")
		for _, c := range req.Constraints {
			sb.WriteString("- ")
			sb.WriteString(c)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("Respond with a JSON object containing your design.\n")

	return sb.String()
}

// ParseDesignResponse parses a JSON string into a DesignResponse.
func ParseDesignResponse(text string) (*protocol.DesignResponse, error) {
	// Try to extract JSON from the response (it might be wrapped in markdown code blocks)
	jsonStr := extractJSON(text)

	var response protocol.DesignResponse
	if err := json.Unmarshal([]byte(jsonStr), &response); err != nil {
		return nil, fmt.Errorf("failed to parse design response: %w", err)
	}

	return &response, nil
}

// extractJSON attempts to extract JSON from a string that might contain markdown.
func extractJSON(text string) string {
	// Check for <design_json> tag first (from MCP tool responses)
	if idx := strings.Index(text, "<design_json>"); idx != -1 {
		start := idx + len("<design_json>")
		if end := strings.Index(text[start:], "</design_json>"); end != -1 {
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
		// Skip language identifier if present
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
		// Find the matching closing brace
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
