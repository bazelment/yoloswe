// Package reviewer provides a simple wrapper around the Codex SDK for code review.
package reviewer

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex/render"
)

// Config holds reviewer configuration.
type Config struct {
	Model          string
	WorkDir        string
	Goal           string
	SessionLogPath string               // Path to write session log (JSON messages)
	Verbose        bool                 // Show progress information (tool use, etc.)
	NoColor        bool                 // Disable ANSI color codes
	ApprovalPolicy codex.ApprovalPolicy // Tool approval policy (default: on-failure)
	JSONOutput     bool                 // Request JSON output format instead of text
}

// BuildPrompt creates the review prompt from the config.
func BuildPrompt(goal string) string {
	goalText := goal
	if goalText == "" {
		goalText = "(not specified)"
	}

	return fmt.Sprintf(`You are experienced software engineer, reviewing changes by another engineer on this branch (this is using git worktree). The main goal of the change on this branch is %s.

Focus on these areas:
- Is the implementation correct? Is there any gap that should be addressed.
- Does it provide sufficient test coverage about the code path it touched.
- maintainability. also look at code around it, is there any code duplication that can be avoided.
- developer experience.
- performance.
- security.

When you flag an issue, provide a short, direct explanation and cite the affected file and line range.
Prioritize severe issues and avoid nit-level comments unless they block understanding of the diff.
After listing findings, produce an overall correctness verdict ("patch is correct" or "patch is incorrect") with a concise justification and a confidence score between 0 and 1.
Ensure that file citations and line numbers are exactly correct using the tools available; if they are incorrect your comments will be rejected.`, goalText)
}

// BuildJSONPrompt creates a review prompt that requests JSON output format.
func BuildJSONPrompt(goal string) string {
	goalText := goal
	if goalText == "" {
		goalText = "(not specified)"
	}

	return fmt.Sprintf(`You are experienced software engineer, reviewing changes by another engineer on this branch (this is using git worktree). The main goal of the change on this branch is %s.

Focus on these areas:
- Is the implementation correct? Is there any gap that should be addressed.
- Does it provide sufficient test coverage about the code path it touched.
- maintainability. also look at code around it, is there any code duplication that can be avoided.
- developer experience.
- performance.
- security.

When you flag an issue, provide a short, direct explanation and cite the affected file and line range.
Prioritize severe issues and avoid nit-level comments unless they block understanding of the diff.
Ensure that file citations and line numbers are exactly correct using the tools available; if they are incorrect your comments will be rejected.

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
- Output ONLY the JSON object, no other text`, goalText)
}

// ReviewResult contains the result of a review turn.
type ReviewResult struct {
	ResponseText string // Full response text
	Success      bool
	DurationMs   int64
	InputTokens  int64
	OutputTokens int64
}

// Reviewer wraps the Codex SDK for code review operations.
type Reviewer struct {
	client   *codex.Client
	thread   *codex.Thread // Persisted thread for follow-up reviews
	config   Config
	output   io.Writer
	renderer *render.Renderer
}

// New creates a new Reviewer with the given config.
func New(config Config) *Reviewer {
	if config.Model == "" {
		config.Model = "gpt-5.2-codex"
	}
	if config.ApprovalPolicy == "" {
		config.ApprovalPolicy = codex.ApprovalPolicyOnFailure
	}
	return &Reviewer{
		config:   config,
		output:   os.Stdout,
		renderer: render.NewRenderer(os.Stdout, config.Verbose, config.NoColor),
	}
}

// SetOutput sets the output writer for streaming responses.
// This also recreates the renderer to use the new writer.
func (r *Reviewer) SetOutput(w io.Writer) {
	r.output = w
	r.renderer = render.NewRenderer(w, r.config.Verbose, r.config.NoColor)
}

// Start initializes the Codex client.
func (r *Reviewer) Start(ctx context.Context) error {
	opts := []codex.ClientOption{
		codex.WithClientName("codex-review"),
		codex.WithClientVersion("1.0.0"),
	}
	if r.config.SessionLogPath != "" {
		opts = append(opts, codex.WithSessionLogPath(r.config.SessionLogPath))
	}
	r.client = codex.NewClient(opts...)
	return r.client.Start(ctx)
}

// Stop shuts down the Codex client.
func (r *Reviewer) Stop() error {
	if r.client != nil {
		return r.client.Stop()
	}
	return nil
}

// Review sends a review prompt and streams the response to output.
// This creates a new thread for each call (one-shot review).
func (r *Reviewer) Review(ctx context.Context, prompt string) error {
	_, err := r.ReviewWithResult(ctx, prompt)
	return err
}

// ReviewWithResult sends a review prompt and returns the result with response text.
// Creates a new thread for each call (one-shot review).
func (r *Reviewer) ReviewWithResult(ctx context.Context, prompt string) (*ReviewResult, error) {
	thread, err := r.createThread(ctx)
	if err != nil {
		return nil, err
	}
	r.thread = thread
	return r.runTurn(ctx, prompt)
}

// FollowUp sends a follow-up message to the existing thread.
// Must be called after ReviewWithResult.
func (r *Reviewer) FollowUp(ctx context.Context, prompt string) (*ReviewResult, error) {
	if r.thread == nil {
		return nil, fmt.Errorf("no active thread, call ReviewWithResult first")
	}
	return r.runTurn(ctx, prompt)
}

// createThread creates a new thread and waits for it to be ready.
func (r *Reviewer) createThread(ctx context.Context) (*codex.Thread, error) {
	r.renderer.Status(fmt.Sprintf("Creating thread with model %s...", r.config.Model))

	thread, err := r.client.CreateThread(ctx,
		codex.WithModel(r.config.Model),
		codex.WithWorkDir(r.config.WorkDir),
		codex.WithApprovalPolicy(r.config.ApprovalPolicy),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create thread: %w", err)
	}

	r.renderer.Status("Waiting for thread to be ready...")

	if err := r.waitForThreadReady(ctx, thread.ID()); err != nil {
		return nil, fmt.Errorf("thread not ready: %w", err)
	}

	r.renderer.Status("Thread ready, sending review prompt...")
	return thread, nil
}

// runTurn sends a message and collects the response.
func (r *Reviewer) runTurn(ctx context.Context, prompt string) (*ReviewResult, error) {
	_, err := r.thread.SendMessage(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	result := &ReviewResult{}
	var responseText strings.Builder

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-r.client.Events():
			if !ok {
				return nil, fmt.Errorf("event channel closed unexpectedly")
			}

			switch e := event.(type) {
			case codex.TextDeltaEvent:
				if e.ThreadID == r.thread.ID() {
					r.renderer.Text(e.Delta)
					responseText.WriteString(e.Delta)
				}
			case codex.ReasoningDeltaEvent:
				if e.ThreadID == r.thread.ID() {
					r.renderer.Reasoning(e.Delta)
				}
			case codex.CommandStartEvent:
				if e.ThreadID == r.thread.ID() {
					r.renderer.CommandStart(e.CallID, e.ParsedCmd)
				}
			case codex.CommandOutputEvent:
				if e.ThreadID == r.thread.ID() {
					r.renderer.CommandOutput(e.CallID, e.Chunk)
				}
			case codex.CommandEndEvent:
				if e.ThreadID == r.thread.ID() {
					r.renderer.CommandEnd(e.CallID, e.ExitCode, e.DurationMs)
				}
			case codex.TurnCompletedEvent:
				if e.ThreadID == r.thread.ID() {
					result.ResponseText = responseText.String()
					result.Success = e.Success
					result.DurationMs = e.DurationMs
					result.InputTokens = e.Usage.InputTokens
					result.OutputTokens = e.Usage.OutputTokens
					r.renderer.TurnComplete(e.Success, e.DurationMs, e.Usage.InputTokens, e.Usage.OutputTokens)
					return result, nil
				}
			case codex.ErrorEvent:
				r.renderer.Error(e.Error, e.Context)
				return nil, fmt.Errorf("error: %v", e.Error)
			}
		}
	}
}

// waitForThreadReady waits for the thread to be ready for messages.
func (r *Reviewer) waitForThreadReady(ctx context.Context, threadID string) error {
	thread, ok := r.client.GetThread(threadID)
	if !ok {
		return fmt.Errorf("thread not found")
	}
	return thread.WaitReady(ctx)
}
