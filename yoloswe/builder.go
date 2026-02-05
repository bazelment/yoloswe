// Package yoloswe provides a builder-reviewer loop for software engineering tasks.
package yoloswe

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
)

// BuilderConfig holds configuration for the builder session.
type BuilderConfig struct {
	Model           string
	WorkDir         string
	RecordingDir    string
	SystemPrompt    string
	ResumeSessionID string
	Verbose         bool
	RequireApproval bool
}

// BuilderSession wraps a claude.Session for builder operations.
type BuilderSession struct {
	output   io.Writer
	session  *claude.Session
	renderer *render.Renderer
	config   BuilderConfig
}

// NewBuilderSession creates a new builder session with the given config.
func NewBuilderSession(config BuilderConfig, output io.Writer) *BuilderSession {
	if config.Model == "" {
		config.Model = "sonnet"
	}
	if config.RecordingDir == "" {
		if homeDir, err := os.UserHomeDir(); err == nil {
			config.RecordingDir = filepath.Join(homeDir, ".yoloswe")
		} else {
			config.RecordingDir = ".yoloswe"
		}
	}
	return &BuilderSession{
		config:   config,
		output:   output,
		renderer: render.NewRenderer(output, config.Verbose),
	}
}

// NewBuilderSessionWithEvents creates a new builder session that emits semantic events.
// The event handler receives structured events for tool calls, text blocks, etc.
// This is useful for TUI applications that need to capture and display events.
// If output is nil, text output is discarded (events are still emitted).
func NewBuilderSessionWithEvents(config BuilderConfig, output io.Writer, eventHandler render.EventHandler) *BuilderSession {
	if config.Model == "" {
		config.Model = "sonnet"
	}
	if config.RecordingDir == "" {
		if homeDir, err := os.UserHomeDir(); err == nil {
			config.RecordingDir = filepath.Join(homeDir, ".yoloswe")
		} else {
			config.RecordingDir = ".yoloswe"
		}
	}
	if output == nil {
		output = io.Discard
	}
	return &BuilderSession{
		config:   config,
		output:   output,
		renderer: render.NewRendererWithEvents(output, config.Verbose, eventHandler),
	}
}

// Start initializes and starts the claude session.
func (b *BuilderSession) Start(ctx context.Context) error {
	opts := []claude.SessionOption{
		claude.WithModel(b.config.Model),
		claude.WithPermissionPromptToolStdio(),
		claude.WithRecording(b.config.RecordingDir),
	}

	// Default: bypass permissions (auto-approve all tools)
	// Use --require-approval to enable manual approval
	if b.config.RequireApproval {
		opts = append(opts, claude.WithPermissionMode(claude.PermissionModeDefault))
	} else {
		opts = append(opts,
			claude.WithPermissionMode(claude.PermissionModeBypass),
			claude.WithPermissionHandler(claude.AllowAllPermissionHandler()),
		)
	}

	if b.config.WorkDir != "" {
		opts = append(opts, claude.WithWorkDir(b.config.WorkDir))
	}

	if b.config.SystemPrompt != "" {
		opts = append(opts, claude.WithSystemPrompt(b.config.SystemPrompt))
	}

	if b.config.ResumeSessionID != "" {
		opts = append(opts, claude.WithResume(b.config.ResumeSessionID))
	}

	// Use interactive tool handler for AskUserQuestion (auto-answers)
	opts = append(opts, claude.WithInteractiveToolHandler(&builderInteractiveHandler{b}))

	b.session = claude.NewSession(opts...)
	return b.session.Start(ctx)
}

// builderInteractiveHandler implements claude.InteractiveToolHandler for the builder.
type builderInteractiveHandler struct {
	b *BuilderSession
}

// HandleAskUserQuestion auto-answers questions by selecting the first option.
func (h *builderInteractiveHandler) HandleAskUserQuestion(ctx context.Context, questions []claude.Question) (map[string]string, error) {
	answers := make(map[string]string)

	for _, q := range questions {
		var response string
		if len(q.Options) > 0 {
			// Select first option
			response = q.Options[0].Label
			h.b.renderer.Status(fmt.Sprintf("Auto-answering: %s -> %s", q.Text, response))
		} else {
			// No options, use default "yes"
			response = "yes"
			h.b.renderer.Status(fmt.Sprintf("Auto-answering (no options): %s -> %s", q.Text, response))
		}
		answers[q.Text] = response
	}

	return answers, nil
}

// HandleExitPlanMode auto-approves plans (builder doesn't use plan mode).
func (h *builderInteractiveHandler) HandleExitPlanMode(ctx context.Context, plan claude.PlanInfo) (string, error) {
	return "Approved. Please proceed with implementation.", nil
}

// Stop gracefully shuts down the session. Safe to call before Start.
func (b *BuilderSession) Stop() error {
	if b.session == nil {
		return nil
	}
	return b.session.Stop()
}

// RunTurn sends a message and processes the turn until completion.
// Returns the turn usage for budget tracking.
func (b *BuilderSession) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	if strings.TrimSpace(message) == "" {
		return nil, fmt.Errorf("message cannot be empty")
	}

	_, err := b.session.SendMessage(ctx, message)
	if err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-b.session.Events():
			if !ok {
				return nil, fmt.Errorf("session ended unexpectedly")
			}

			switch e := event.(type) {
			case claude.ReadyEvent:
				b.renderer.Status(fmt.Sprintf("Builder session started: %s (model: %s)", e.Info.SessionID, e.Info.Model))

			case claude.TextEvent:
				b.renderer.Text(e.Text)

			case claude.ThinkingEvent:
				b.renderer.Thinking(e.Thinking)

			case claude.ToolStartEvent:
				b.renderer.ToolStart(e.Name, e.ID)

			case claude.ToolCompleteEvent:
				b.renderer.ToolComplete(e.Name, e.Input)
				// Note: AskUserQuestion is now handled by InteractiveToolHandler

			case claude.CLIToolResultEvent:
				b.renderer.ToolResult(e.Content, e.IsError)

			case claude.TurnCompleteEvent:
				b.renderer.TurnSummary(e.TurnNumber, e.Success, e.DurationMs, e.Usage.CostUSD)
				if !e.Success {
					return &e.Usage, fmt.Errorf("turn completed with success=false")
				}
				return &e.Usage, nil

			case claude.ErrorEvent:
				b.renderer.Error(e.Error, e.Context)
				// Return partial usage if available, along with error
				return nil, fmt.Errorf("builder error: %v (context: %s)", e.Error, e.Context)
			}
		}
	}
}

// RecordingPath returns the path to the session recording directory.
// Returns empty string if session not started.
func (b *BuilderSession) RecordingPath() string {
	if b.session == nil {
		return ""
	}
	return b.session.RecordingPath()
}
