// Package reviewer implements the Reviewer sub-agent.
package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

// Reviewer is an ephemeral agent that reviews code changes.
type Reviewer struct {
	session *agent.EphemeralSession
	config  agent.AgentConfig
}

// New creates a new Reviewer agent.
func New(config agent.AgentConfig, swarmSessionID string) *Reviewer {
	config.Role = agent.RoleReviewer
	if config.SystemPrompt == "" {
		config.SystemPrompt = SystemPrompt
	}

	return &Reviewer{
		session: agent.NewEphemeralSession(config, swarmSessionID),
		config:  config,
	}
}

// Role returns the agent's role.
func (r *Reviewer) Role() agent.AgentRole {
	return agent.RoleReviewer
}

// SessionDir returns the base directory for task recordings.
func (r *Reviewer) SessionDir() string {
	return r.session.BaseSessionDir()
}

// TotalCost returns the accumulated cost.
func (r *Reviewer) TotalCost() float64 {
	return r.session.TotalCost()
}

// TaskCount returns the number of tasks executed.
func (r *Reviewer) TaskCount() int {
	return r.session.TaskCount()
}

// Execute runs a review task and returns the raw result.
func (r *Reviewer) Execute(ctx context.Context, prompt string) (*agent.AgentResult, string, error) {
	return r.session.Execute(ctx, prompt)
}

// ExecuteWithFiles runs a review task and returns the result with file tracking.
func (r *Reviewer) ExecuteWithFiles(ctx context.Context, prompt string) (*agent.AgentResult, *agent.ExecuteResult, string, error) {
	return r.session.ExecuteWithFiles(ctx, prompt)
}

// Review analyzes code changes and provides feedback.
func (r *Reviewer) Review(ctx context.Context, req *protocol.ReviewRequest) (*protocol.ReviewResponse, string, error) {
	prompt := formatReviewPrompt(req)

	result, taskID, err := r.session.Execute(ctx, prompt)
	if err != nil {
		return nil, taskID, fmt.Errorf("review execution failed: %w", err)
	}

	if !result.Success {
		return nil, taskID, fmt.Errorf("review task failed: %v", result.Error)
	}

	// Parse response would need text from events
	response := &protocol.ReviewResponse{}
	return response, taskID, nil
}

// formatReviewPrompt formats a ReviewRequest into a prompt string.
func formatReviewPrompt(req *protocol.ReviewRequest) string {
	var sb strings.Builder

	sb.WriteString("Please review the following code changes.\n\n")

	sb.WriteString("## Task\n")
	sb.WriteString(req.Task)
	sb.WriteString("\n\n")

	sb.WriteString("## Files Changed\n")
	for _, f := range req.FilesChanged {
		sb.WriteString("- ")
		sb.WriteString(f)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	sb.WriteString("Please read each of these files and review the implementation.\n\n")

	if req.OriginalDesign != nil {
		sb.WriteString("## Original Design\n")
		sb.WriteString("### Architecture\n")
		sb.WriteString(req.OriginalDesign.Architecture)
		sb.WriteString("\n\n")

		if len(req.OriginalDesign.Files) > 0 {
			sb.WriteString("### Expected Files\n")
			for _, f := range req.OriginalDesign.Files {
				sb.WriteString(fmt.Sprintf("- %s: %s\n", f.Path, f.Purpose))
			}
			sb.WriteString("\n")
		}

		if req.OriginalDesign.Interfaces != "" {
			sb.WriteString("### Expected Interfaces\n")
			sb.WriteString("```\n")
			sb.WriteString(req.OriginalDesign.Interfaces)
			sb.WriteString("\n```\n\n")
		}
	}

	sb.WriteString("After reviewing, provide your feedback as JSON.\n")
	sb.WriteString("Remember: You provide feedback only. You do NOT approve or reject.\n")

	return sb.String()
}

// ParseReviewResponse parses a JSON string into a ReviewResponse.
func ParseReviewResponse(text string) (*protocol.ReviewResponse, error) {
	jsonStr := extractJSON(text)

	var response protocol.ReviewResponse
	if err := json.Unmarshal([]byte(jsonStr), &response); err != nil {
		return nil, fmt.Errorf("failed to parse review response: %w", err)
	}

	return &response, nil
}

// extractJSON attempts to extract JSON from a string that might contain markdown.
func extractJSON(text string) string {
	// Check for <review_json> tag first (from MCP tool responses)
	if idx := strings.Index(text, "<review_json>"); idx != -1 {
		start := idx + len("<review_json>")
		if end := strings.Index(text[start:], "</review_json>"); end != -1 {
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
