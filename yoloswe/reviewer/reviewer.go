// Package reviewer provides a multi-backend wrapper for code review using
// agent CLIs (Codex, Cursor).
package reviewer

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex/render"
)

// BackendType identifies which agent backend to use.
type BackendType string

const (
	BackendCodex  BackendType = "codex"
	BackendCursor BackendType = "cursor"
)

// Config holds reviewer configuration.
type Config struct {
	Model          string
	WorkDir        string
	Goal           string
	SessionLogPath string
	Effort         string // Reasoning effort level for codex (low, medium, high)
	ApprovalPolicy codex.ApprovalPolicy
	BackendType    BackendType
	Verbose        bool
	NoColor        bool
	JSONOutput     bool
}

// buildGoalText formats the goal text for review prompts.
func buildGoalText(goal string) string {
	if goal == "" {
		return "Review all changes on this branch. Use commit messages to understand their purpose."
	}
	return "Review all changes on this branch. The main goal of the change on this branch is: " + goal
}

// buildBasePrompt creates the common review prompt content.
func buildBasePrompt(goal string) string {
	return fmt.Sprintf(`You are experienced software engineer, with bias toward code quality and correctness.
%s

Focus on these areas:
- Is the implementation correct? Is there any gap that should be addressed.
- Does it provide sufficient test coverage about the code path it touched.
- maintainability. also look at code around it, is there any code duplication that can be avoided.
- developer experience.
- performance.
- security.

When you flag an issue, provide a short, direct explanation and cite the affected file and line range.
Prioritize severe issues and avoid nit-level comments unless they block understanding of the diff.
Ensure that file citations and line numbers are exactly correct using the tools available; if they are incorrect your comments will be rejected.`, buildGoalText(goal))
}

// BuildPrompt creates the review prompt with free-form text output.
func BuildPrompt(goal string) string {
	return buildBasePrompt(goal) + `

After listing findings, produce an overall correctness verdict ("patch is correct" or "patch is incorrect") with a concise justification and a confidence score between 0 and 1.`
}

// BuildJSONPrompt creates a review prompt that requests JSON output format.
func BuildJSONPrompt(goal string) string {
	return buildBasePrompt(goal) + `

## Output Format
You MUST respond with valid JSON in this exact format:
{
  "verdict": "accepted" or "rejected",
  "summary": "Brief overall assessment of the changes",
  "issues": [
    {
      "severity": "critical|high|medium|low",
      "file": "path/to/file.go",
      "line": 42,
      "message": "Description of the issue",
      "suggestion": "How to fix it"
    }
  ]
}

## Severity Levels
- critical: Bugs, security vulnerabilities, broken functionality, data loss risks
- high: Missing error handling, incorrect logic that could cause failures
- medium: Code style issues, minor inefficiencies, missing edge cases
- low: Naming preferences, formatting, optional improvements

## Rules
- verdict MUST be exactly "accepted" or "rejected"
- If there are any critical or high severity issues, verdict MUST be "rejected"
- issues array can be empty if verdict is "accepted"
- Output ONLY the JSON object, no other text`
}

// ReviewResult contains the result of a review turn.
type ReviewResult struct {
	ResponseText string // Full response text
	Success      bool
	DurationMs   int64
	InputTokens  int64
	OutputTokens int64
}

// Reviewer wraps an agent backend for code review operations.
type Reviewer struct {
	output   io.Writer
	backend  Backend
	renderer *render.Renderer
	config   Config
}

// New creates a new Reviewer with the given config.
func New(config Config) *Reviewer {
	if config.BackendType == "" {
		config.BackendType = BackendCodex
	}
	// Apply codex-specific defaults only for codex backend
	if config.BackendType == BackendCodex {
		if config.Model == "" {
			config.Model = "gpt-5.2-codex"
		}
		if config.ApprovalPolicy == "" {
			config.ApprovalPolicy = codex.ApprovalPolicyOnFailure
		}
	}

	var backend Backend
	switch config.BackendType {
	case BackendCursor:
		backend = newCursorBackend(config)
	default:
		backend = newCodexBackend(config)
	}

	return &Reviewer{
		config:   config,
		output:   os.Stdout,
		renderer: render.NewRenderer(os.Stdout, config.Verbose, config.NoColor),
		backend:  backend,
	}
}

// SetOutput sets the output writer for streaming responses.
// This also recreates the renderer to use the new writer.
func (r *Reviewer) SetOutput(w io.Writer) {
	r.output = w
	r.renderer = render.NewRenderer(w, r.config.Verbose, r.config.NoColor)
}

// Start initializes the backend.
func (r *Reviewer) Start(ctx context.Context) error {
	return r.backend.Start(ctx)
}

// Stop shuts down the backend.
func (r *Reviewer) Stop() error {
	return r.backend.Stop()
}

// Review sends a review prompt and streams the response to output.
func (r *Reviewer) Review(ctx context.Context, prompt string) error {
	_, err := r.ReviewWithResult(ctx, prompt)
	return err
}

// ReviewWithResult sends a review prompt and returns the result with response text.
func (r *Reviewer) ReviewWithResult(ctx context.Context, prompt string) (*ReviewResult, error) {
	model := r.config.Model
	if model == "" {
		model = "default"
	}
	status := fmt.Sprintf("Running review with %s (model: %s", r.config.BackendType, model)
	if r.config.Effort != "" {
		status += fmt.Sprintf(", effort: %s", r.config.Effort)
	}
	status += ")..."
	r.renderer.Status(status)
	handler := newRendererEventHandler(r.renderer)
	result, err := r.backend.RunPrompt(ctx, prompt, handler)
	if err != nil {
		return nil, err
	}
	r.renderer.TurnComplete(result.Success, result.DurationMs, result.InputTokens, result.OutputTokens)
	return result, nil
}

// FollowUp sends a follow-up message to the existing backend session.
func (r *Reviewer) FollowUp(ctx context.Context, prompt string) (*ReviewResult, error) {
	handler := newRendererEventHandler(r.renderer)
	result, err := r.backend.RunPrompt(ctx, prompt, handler)
	if err != nil {
		return nil, err
	}
	r.renderer.TurnComplete(result.Success, result.DurationMs, result.InputTokens, result.OutputTokens)
	return result, nil
}
