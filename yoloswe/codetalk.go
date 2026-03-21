package yoloswe

import (
	"context"
	"fmt"
	"io"
	"os"

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
	baseSession
	config CodeTalkConfig
}

// NewCodeTalkSession creates a new code understanding session.
// If output is nil, text output goes to os.Stdout.
func NewCodeTalkSession(config CodeTalkConfig, output io.Writer) *CodeTalkSession {
	if config.Model == "" {
		config.Model = "opus"
	}
	if config.RecordingDir == "" {
		config.RecordingDir = defaultRecordingDir()
	}
	if output == nil {
		output = os.Stdout
	}
	return &CodeTalkSession{
		config:      config,
		baseSession: newBaseSession(output, config.Verbose, "CodeTalk", "codetalk"),
	}
}

// NewCodeTalkSessionWithEvents creates a new code understanding session that emits semantic events.
// If output is nil, text output is discarded (events are still emitted).
func NewCodeTalkSessionWithEvents(config CodeTalkConfig, output io.Writer, eventHandler render.EventHandler) *CodeTalkSession {
	if config.Model == "" {
		config.Model = "opus"
	}
	if config.RecordingDir == "" {
		config.RecordingDir = defaultRecordingDir()
	}
	if output == nil {
		output = io.Discard
	}
	return &CodeTalkSession{
		config:      config,
		baseSession: newBaseSessionWithEvents(output, config.Verbose, eventHandler, "CodeTalk", "codetalk"),
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
	return autoAnswerQuestions(h.ct.renderer, questions)
}

// HandleExitPlanMode denies any attempt to exit plan mode. CodeTalk sessions
// are intentionally read-only; the LLM must not be granted write access.
func (h *codetalkInteractiveHandler) HandleExitPlanMode(ctx context.Context, plan claude.PlanInfo) (string, error) {
	return "", fmt.Errorf("codetalk sessions are read-only; exiting plan mode is not allowed")
}

// RunTurn sends a message and processes the turn until completion.
func (ct *CodeTalkSession) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	return ct.baseSession.RunTurn(ctx, message)
}
