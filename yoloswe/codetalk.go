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

// CodeTalkSystemPrompt is the default system prompt for code understanding sessions.
const CodeTalkSystemPrompt = `You are a code understanding agent. Your task is to deeply explore and understand the specified area of the codebase before providing your analysis.

Exploration phase:
- Use Read, Grep, Glob, and Bash (read-only commands) to trace code paths thoroughly
- Follow imports, function calls, and data flow across files
- Identify key types, interfaces, and their relationships
- Look at tests to understand expected behavior and edge cases

Once you have a comprehensive understanding, provide a clear structured explanation covering:
- Entry points and overall flow
- Key files and their roles
- Important types and interfaces
- Design patterns and architectural decisions
- Non-obvious behaviors or gotchas

When the user asks follow-up questions, explore further if your current understanding is insufficient.`

// CodeTalkConfig holds configuration for a code understanding session.
type CodeTalkConfig struct {
	Model           string
	WorkDir         string
	RecordingDir    string
	SystemPrompt    string
	ResumeSessionID string
	Verbose         bool
}

// CodeTalkSession wraps a claude.Session for read-only code understanding.
// Unlike PlannerWrapper, it has no ExitPlanMode state machine or plan files.
// Every turn is uniform: send message, drain events until TurnComplete.
type CodeTalkSession struct {
	output   io.Writer
	session  *claude.Session
	renderer *render.Renderer
	config   CodeTalkConfig
}

// NewCodeTalkSession creates a new code understanding session.
func NewCodeTalkSession(config CodeTalkConfig, output io.Writer) *CodeTalkSession {
	if config.Model == "" {
		config.Model = "opus"
	}
	if config.RecordingDir == "" {
		if homeDir, err := os.UserHomeDir(); err == nil {
			config.RecordingDir = filepath.Join(homeDir, ".yoloswe")
		} else {
			config.RecordingDir = ".yoloswe"
		}
	}
	if output == nil {
		output = os.Stdout
	}
	return &CodeTalkSession{
		config:   config,
		output:   output,
		renderer: render.NewRenderer(output, config.Verbose),
	}
}

// NewCodeTalkSessionWithEvents creates a new code understanding session that emits semantic events.
// If output is nil, text output is discarded (events are still emitted).
func NewCodeTalkSessionWithEvents(config CodeTalkConfig, output io.Writer, eventHandler render.EventHandler) *CodeTalkSession {
	if config.Model == "" {
		config.Model = "opus"
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
	return &CodeTalkSession{
		config:   config,
		output:   output,
		renderer: render.NewRendererWithEvents(output, config.Verbose, eventHandler),
	}
}

// Start initializes and starts the claude session in plan (read-only) mode.
func (ct *CodeTalkSession) Start(ctx context.Context) error {
	systemPrompt := ct.config.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = CodeTalkSystemPrompt
	}

	opts := []claude.SessionOption{
		claude.WithModel(ct.config.Model),
		claude.WithSystemPrompt(systemPrompt),
		claude.WithPermissionMode(claude.PermissionModeDefault),
		claude.WithPermissionPromptToolStdio(),
		claude.WithPermissionHandler(claude.AllowAllPermissionHandler()),
		claude.WithInteractiveToolHandler(&codetalkInteractiveHandler{ct}),
		claude.WithRecording(ct.config.RecordingDir),
	}

	if ct.config.WorkDir != "" {
		opts = append(opts, claude.WithWorkDir(ct.config.WorkDir))
	}

	if ct.config.ResumeSessionID != "" {
		opts = append(opts, claude.WithResume(ct.config.ResumeSessionID))
	}

	ct.session = claude.NewSession(opts...)

	if err := ct.session.Start(ctx); err != nil {
		return err
	}

	// Switch to plan mode (read-only) via control message.
	// When resuming, the CLI restores the previous permission mode.
	if ct.config.ResumeSessionID == "" {
		return ct.session.SetPermissionMode(ctx, claude.PermissionModePlan)
	}
	return nil
}

// codetalkInteractiveHandler implements claude.InteractiveToolHandler.
type codetalkInteractiveHandler struct {
	ct *CodeTalkSession
}

// HandleAskUserQuestion auto-answers questions by selecting the first option.
func (h *codetalkInteractiveHandler) HandleAskUserQuestion(ctx context.Context, questions []claude.Question) (map[string]string, error) {
	answers := make(map[string]string)
	for _, q := range questions {
		var response string
		if len(q.Options) > 0 {
			response = q.Options[0].Label
			h.ct.renderer.Status(fmt.Sprintf("Auto-answering: %s -> %s", q.Text, response))
		} else {
			response = "yes"
			h.ct.renderer.Status(fmt.Sprintf("Auto-answering (no options): %s -> %s", q.Text, response))
		}
		answers[q.Text] = response
	}
	return answers, nil
}

// HandleExitPlanMode auto-approves (codetalk doesn't use plan files).
func (h *codetalkInteractiveHandler) HandleExitPlanMode(ctx context.Context, plan claude.PlanInfo) (string, error) {
	return "Continue with your analysis.", nil
}

// CLISessionID returns the CLI session ID from the underlying claude session.
func (ct *CodeTalkSession) CLISessionID() string {
	if ct.session == nil {
		return ""
	}
	info := ct.session.Info()
	if info == nil {
		return ""
	}
	return info.SessionID
}

// Stop gracefully shuts down the session. Safe to call before Start.
func (ct *CodeTalkSession) Stop() error {
	if ct.session == nil {
		return nil
	}
	return ct.session.Stop()
}

// RunTurn sends a message and processes the turn until completion.
func (ct *CodeTalkSession) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	if strings.TrimSpace(message) == "" {
		return nil, fmt.Errorf("message cannot be empty")
	}

	_, err := ct.session.SendMessage(ctx, message)
	if err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-ct.session.Events():
			if !ok {
				return nil, fmt.Errorf("session ended unexpectedly")
			}

			switch e := event.(type) {
			case claude.ReadyEvent:
				ct.renderer.Status(fmt.Sprintf("CodeTalk session started: %s (model: %s)", e.Info.SessionID, e.Info.Model))

			case claude.TextEvent:
				ct.renderer.Text(e.Text)

			case claude.ThinkingEvent:
				ct.renderer.Thinking(e.Thinking)

			case claude.ToolStartEvent:
				ct.renderer.ToolStart(e.Name, e.ID)

			case claude.ToolCompleteEvent:
				ct.renderer.ToolComplete(e.Name, e.Input)

			case claude.CLIToolResultEvent:
				ct.renderer.ToolResult(e.Content, e.IsError)

			case claude.TurnCompleteEvent:
				ct.renderer.TurnSummary(e.TurnNumber, e.Success, e.DurationMs, e.Usage.CostUSD)
				if !e.Success {
					return &e.Usage, fmt.Errorf("turn completed with success=false")
				}
				return &e.Usage, nil

			case claude.ErrorEvent:
				ct.renderer.Error(e.Error, e.Context)
				return nil, fmt.Errorf("codetalk error: %v (context: %s)", e.Error, e.Context)
			}
		}
	}
}

// RecordingPath returns the path to the session recording directory.
func (ct *CodeTalkSession) RecordingPath() string {
	if ct.session == nil {
		return ""
	}
	return ct.session.RecordingPath()
}
