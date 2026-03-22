// Package yoloswe provides a builder-reviewer loop for software engineering tasks.
package yoloswe

import (
	"context"
	"io"

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
type BuilderSession struct { //nolint:govet // fieldalignment: baseSession embedding controls layout
	baseSession
	config BuilderConfig
}

// NewBuilderSession creates a new builder session with the given config.
func NewBuilderSession(config BuilderConfig, output io.Writer) *BuilderSession {
	if config.Model == "" {
		config.Model = "sonnet"
	}
	if config.RecordingDir == "" {
		config.RecordingDir = defaultRecordingDir()
	}
	return &BuilderSession{
		config:      config,
		baseSession: newBaseSession(output, config.Verbose, "Builder", "builder"),
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
		config.RecordingDir = defaultRecordingDir()
	}
	if output == nil {
		output = io.Discard
	}
	return &BuilderSession{
		config:      config,
		baseSession: newBaseSessionWithEvents(output, config.Verbose, eventHandler, "Builder", "builder"),
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
	return autoAnswerQuestions(h.b.renderer, questions)
}

// HandleExitPlanMode auto-approves plans (builder doesn't use plan mode).
func (h *builderInteractiveHandler) HandleExitPlanMode(ctx context.Context, plan claude.PlanInfo) (string, error) {
	return "Approved. Please proceed with implementation.", nil
}

// RunTurn sends a message and processes the turn until completion.
// Returns the turn usage for budget tracking.
func (b *BuilderSession) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	return b.baseSession.RunTurn(ctx, message)
}
