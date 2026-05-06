package reviewer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/cursor"
)

// cursorBackend wraps the Cursor Agent SDK as a Backend.
// Each RunPrompt call is a one-shot execution (no persistent session).
type cursorBackend struct {
	config Config
}

func newCursorBackend(config Config) *cursorBackend {
	return &cursorBackend{config: config}
}

// Start is a no-op for cursor (one-shot per prompt).
func (b *cursorBackend) Start(_ context.Context) error {
	return nil
}

// Stop is a no-op for cursor (one-shot per prompt).
func (b *cursorBackend) Stop() error {
	return nil
}

func (b *cursorBackend) RunPrompt(ctx context.Context, prompt string, handler EventHandler) (*ReviewResult, error) {
	opts := b.baseSessionOptions()
	resumeOpts := opts
	var resumeStatus ResumeStatus
	if b.config.ResumeSessionID != "" {
		resumeOpts = append(append([]cursor.SessionOption{}, opts...), cursor.WithResume(b.config.ResumeSessionID))
		resumeStatus = ResumeStatusOK
	}

	result, err := b.runPromptWithOptions(ctx, prompt, handler, resumeOpts, resumeStatus)
	if err != nil && b.config.ResumeSessionID != "" && isCursorResumeNotFound(err) {
		slog.Warn("cursor resume failed; falling back to fresh session", "session_id", b.config.ResumeSessionID, "error", err.Error())
		return b.runPromptWithOptions(ctx, prompt, handler, opts, ResumeStatusFallback)
	}
	return result, err
}

func (b *cursorBackend) baseSessionOptions() []cursor.SessionOption {
	var opts []cursor.SessionOption
	if b.config.Model != "" {
		opts = append(opts, cursor.WithModel(b.config.Model))
	}
	if b.config.WorkDir != "" {
		opts = append(opts, cursor.WithWorkDir(b.config.WorkDir))
	}
	// Non-interactive flags for automation:
	// --trust: trust the workspace without prompting
	// --force: allow all tool calls (shell, write, etc.) without approval
	//
	// Cursor also supports --sandbox, but it fails immediately on systems
	// with AppArmor unprivileged userns restrictions (same bwrap issue as
	// Codex—see Config doc in reviewer.go). With or without --force, the
	// session ends without result when --sandbox is enabled.
	opts = append(opts, cursor.WithTrust(), cursor.WithForce(), cursor.WithStderrHandler(stderrPrefixHandler("cursor")))
	return opts
}

func (b *cursorBackend) runPromptWithOptions(ctx context.Context, prompt string, handler EventHandler, opts []cursor.SessionOption, resumeStatus ResumeStatus) (*ReviewResult, error) {
	events, err := cursor.QueryStream(ctx, prompt, opts...)
	if err != nil {
		return nil, fmt.Errorf("cursor query failed: %w", err)
	}

	// Use a derived context so that the adapter goroutine is unblocked
	// when RunPrompt returns early (e.g., on ErrorEvent), preventing
	// goroutine leaks even if the parent context is still active.
	adapterCtx, adapterCancel := context.WithCancel(ctx)
	defer adapterCancel()

	// Wrap handler to format cursor-specific tool display names and
	// intercept ReadyEvent (which isn't part of agentstream).
	adapter := &cursorEventAdapter{handler: handler, events: events}
	bridged, err := bridgeStreamEvents(adapterCtx, adapter.filtered(adapterCtx), handler, "")
	if err != nil {
		return nil, fmt.Errorf("cursor: %w", err)
	}

	// Check for turn-level errors (TurnCompleteEvent.Error).
	if tc, ok := bridged.turnEvent.(cursor.TurnCompleteEvent); ok && tc.Error != nil {
		if handler != nil {
			handler.OnError(tc.Error, "turn_complete")
		}
		return nil, fmt.Errorf("cursor turn failed: %w", tc.Error)
	}

	return &ReviewResult{
		ResponseText: bridged.responseText,
		Success:      bridged.success,
		DurationMs:   bridged.durationMs,
		ResumeStatus: resumeStatus,
	}, nil
}

func isCursorResumeNotFound(err error) bool {
	msg := err.Error()
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "session not found") ||
		strings.Contains(lower, "chat not found") ||
		strings.Contains(lower, "resume") && isResumeUnavailableMessage(msg)
}

// cursorEventAdapter filters cursor events, handling ReadyEvent out-of-band
// and formatting tool names before they reach the bridge.
type cursorEventAdapter struct {
	handler EventHandler
	events  <-chan cursor.Event
}

// filtered returns a channel that re-emits cursor events, handling ReadyEvent
// separately and wrapping ToolStartEvent with formatted display names.
// The context is used to unblock sends when the consumer (bridgeStreamEvents)
// returns early, preventing goroutine leaks.
func (a *cursorEventAdapter) filtered(ctx context.Context) <-chan cursor.Event {
	out := make(chan cursor.Event)
	go func() {
		defer close(out)
		for ev := range a.events {
			switch e := ev.(type) {
			case cursor.ReadyEvent:
				// ReadyEvent doesn't implement agentstream.Event; handle directly.
				if a.handler != nil {
					a.handler.OnSessionInfo(e.SessionID, e.Model)
				}
			case cursor.ToolStartEvent:
				// Rewrite tool name for display before the bridge sees it.
				e.Name = formatCursorToolDisplay(e.Name, e.Input)
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			default:
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// cursorToolDisplay renders Cursor's ToolCall events for terminal output.
var cursorToolDisplay = toolDisplay{
	tools: map[string]toolInfo{
		"readToolCall":        {Display: "read", ArgKey: "path", ArgFormat: argFormatPath},
		"shellToolCall":       {Display: "shell", ArgKey: "command", ArgFormat: argFormatCommand},
		"globToolCall":        {Display: "glob", ArgKey: "globPattern", ArgFormat: argFormatPlain},
		"grepToolCall":        {Display: "grep", ArgKey: "pattern", ArgFormat: argFormatPlain},
		"editToolCall":        {Display: "edit", ArgKey: "path", ArgFormat: argFormatPath},
		"writeToolCall":       {Display: "write", ArgKey: "path", ArgFormat: argFormatPath},
		"updateTodosToolCall": {Display: "updateTodos"},
	},
	fallback: cursorFallbackName,
}

// cursorFallbackName strips the "ToolCall" suffix and lowercases the first
// letter; e.g. "listFilesToolCall" → "listFiles".
func cursorFallbackName(name string) string {
	if !strings.HasSuffix(name, "ToolCall") {
		return name
	}
	s := strings.TrimSuffix(name, "ToolCall")
	if s == "" {
		return name
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func formatCursorToolDisplay(name string, input map[string]interface{}) string {
	return cursorToolDisplay.format(name, input)
}
