package reviewer

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
)

// Backend abstracts the agent lifecycle for different providers.
type Backend interface {
	Start(ctx context.Context) error
	Stop() error
	RunPrompt(ctx context.Context, prompt string, handler EventHandler) (*ReviewResult, error)
}

// EventHandler receives streaming events from the agent backend.
type EventHandler interface {
	OnSessionInfo(sessionID, model string)
	OnText(delta string)
	OnReasoning(delta string)
	OnToolStart(name, callID string, input map[string]interface{})
	OnToolComplete(name, callID string, input map[string]interface{}, result interface{}, isError bool)
	OnTurnComplete(success bool, durationMs int64)
	OnError(err error, context string)
}

// bridgeResult holds the outcome of bridgeStreamEvents.
type bridgeResult struct {
	// turnEvent is the raw TurnComplete event for backend-specific extraction
	// (e.g., codex token usage).
	turnEvent    agentstream.TurnComplete
	responseText string
	durationMs   int64
	success      bool
}

// bridgeStreamEvents reads SDK events from a typed channel and dispatches them
// to an EventHandler. It accumulates text deltas and returns when a TurnComplete
// or Error event is received, or the channel closes.
//
// scopeID enables filtering for multiplexed channels (e.g., codex thread ID).
// Pass "" to disable scope filtering.
func bridgeStreamEvents[E any](ctx context.Context, events <-chan E, handler EventHandler, scopeID string) (*bridgeResult, error) {
	if events == nil {
		return nil, fmt.Errorf("nil event channel")
	}

	var responseText strings.Builder

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				// Channel closed without TurnComplete.
				text := responseText.String()
				if text != "" {
					return nil, fmt.Errorf("session ended unexpectedly (partial response: %d chars)", len(text))
				}
				return nil, fmt.Errorf("session ended without result")
			}

			sev, ok := any(ev).(agentstream.Event)
			if !ok {
				continue
			}

			kind := sev.StreamEventKind()
			if kind == agentstream.KindUnknown {
				continue
			}

			// Scope filtering for multiplexed channels.
			if scopeID != "" {
				if scoped, ok := any(ev).(agentstream.Scoped); ok {
					if id := scoped.ScopeID(); id != "" && id != scopeID {
						continue
					}
				}
			}

			switch kind {
			case agentstream.KindText:
				te := sev.(agentstream.Text)
				delta := te.StreamDelta()
				responseText.WriteString(delta)
				if handler != nil {
					handler.OnText(delta)
				}

			case agentstream.KindThinking:
				te := sev.(agentstream.Text)
				if handler != nil {
					handler.OnReasoning(te.StreamDelta())
				}

			case agentstream.KindToolStart:
				ts := sev.(agentstream.ToolStart)
				if handler != nil {
					handler.OnToolStart(ts.StreamToolName(), ts.StreamToolCallID(), ts.StreamToolInput())
				}

			case agentstream.KindToolEnd:
				te := sev.(agentstream.ToolEnd)
				if handler != nil {
					handler.OnToolComplete(
						te.StreamToolName(),
						te.StreamToolCallID(),
						te.StreamToolInput(),
						te.StreamToolResult(),
						te.StreamToolIsError(),
					)
				}

			case agentstream.KindTurnComplete:
				tc := sev.(agentstream.TurnComplete)
				success := tc.StreamIsSuccess()
				durationMs := tc.StreamDuration()
				if handler != nil {
					handler.OnTurnComplete(success, durationMs)
				}
				return &bridgeResult{
					responseText: responseText.String(),
					success:      success,
					durationMs:   durationMs,
					turnEvent:    tc,
				}, nil

			case agentstream.KindError:
				ee := sev.(agentstream.Error)
				if handler != nil {
					handler.OnError(ee.StreamErr(), ee.StreamErrorContext())
				}
				return nil, fmt.Errorf("error: %w", ee.StreamErr())
			}
		}
	}
}

// rendererEventHandler adapts EventHandler to a render.Renderer and also
// emits a structured slog record for each boundary event (session info, tool
// start/end, turn complete, error). The slog side is cheap and writes to the
// handler installed by SetupRunLog; when no file handler is installed it
// still flows through the default slog writer, which tests may override.
//
// It also prints plain-text progress lines to stdout so the Monitor tool can
// surface live activity to Claude without requiring JSON parsing.
type rendererEventHandler struct {
	r        *render.Renderer
	reviewer *Reviewer // optional; captures lastSessionID when set
}

func (r *Reviewer) newEventHandler() *rendererEventHandler {
	return &rendererEventHandler{r: r.renderer, reviewer: r}
}

// backendName returns the configured backend as a plain string, or "" if no
// reviewer is attached. Used as a default for ProgressEvent.Backend.
func (h *rendererEventHandler) backendName() string {
	if h.reviewer == nil {
		return ""
	}
	return string(h.reviewer.config.BackendType)
}

func (h *rendererEventHandler) OnSessionInfo(sessionID, model string) {
	h.r.SessionInfo(sessionID, model)
	if h.reviewer != nil {
		h.reviewer.lastSessionID = sessionID
		if model != "" {
			h.reviewer.effectiveModel = model
		}
	}
	slog.Info("reviewer session started", "session_id", sessionID, "model", model)
	fmt.Fprintf(os.Stdout, "session started (%s %s)\n", h.backendName(), model)
}

func (h *rendererEventHandler) OnText(delta string) {
	h.r.Text(delta)
}

func (h *rendererEventHandler) OnReasoning(delta string) {
	h.r.Reasoning(delta)
}

func (h *rendererEventHandler) OnToolStart(name, callID string, input map[string]interface{}) {
	h.r.CommandStart(callID, name)
	slog.Debug("tool call start",
		"tool", name,
		"call_id", callID,
		"input_summary", summarizeToolInput(input))
	fmt.Fprintf(os.Stdout, "%s\n", name)
}

func (h *rendererEventHandler) OnToolComplete(name string, callID string, _ map[string]interface{}, result interface{}, isError bool) {
	exitCode := 0
	if isError {
		exitCode = 1
	}
	h.r.CommandEnd(callID, exitCode, 0)
	resultLen := 0
	if s, ok := result.(string); ok {
		resultLen = len(s)
	}
	slog.Debug("tool call end",
		"tool", name,
		"call_id", callID,
		"is_error", isError,
		"result_len", resultLen)
}

func (h *rendererEventHandler) OnTurnComplete(success bool, durationMs int64) {
	// Renderer update is handled by reviewer.go after RunPrompt returns.
	slog.Info("reviewer turn complete",
		"success", success,
		"duration_ms", durationMs)
	fmt.Fprintf(os.Stdout, "turn complete\n")
}

func (h *rendererEventHandler) OnError(err error, context string) {
	h.r.Error(err, context)
	slog.Error("reviewer error",
		"context", context,
		"error", err.Error())
	fmt.Fprintf(os.Stdout, "error: %s\n", context)
}

// sensitiveToolInputKeys names keys whose values may contain shell commands,
// file paths, edit payloads, or other content that should not be written to
// the per-run log verbatim. For these keys summarizeToolInput records only the
// value length, not the value itself.
var sensitiveToolInputKeys = map[string]bool{
	"command":          true,
	"content":          true,
	"cwd":              true,
	"file_text":        true,
	"globPattern":      true,
	"new_string":       true,
	"old_string":       true,
	"path":             true,
	"file_path":        true,
	"pattern":          true,
	"simpleCommands":   true,
	"parsingResult":    true,
	"args":             true,
	"workingDirectory": true,
}

// summarizeToolInput collapses a tool input map to a short preview for
// logging. Non-sensitive primitive values are truncated and included; values
// under sensitive keys (commands, paths, edit payloads — see
// sensitiveToolInputKeys) are replaced with a length marker so the per-run
// log never stores shell commands or file contents verbatim.
func summarizeToolInput(input map[string]interface{}) string {
	if len(input) == 0 {
		return ""
	}
	var b strings.Builder
	const maxLen = 200
	for k, v := range input {
		if b.Len() >= maxLen {
			b.WriteString("...")
			break
		}
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(k)
		b.WriteString("=")
		if sensitiveToolInputKeys[k] {
			b.WriteString(fmt.Sprintf("<redacted:%d>", redactedLen(v)))
			continue
		}
		s := fmt.Sprintf("%v", v)
		if len(s) > 80 {
			s = s[:77] + "..."
		}
		b.WriteString(s)
	}
	return b.String()
}

// redactedLen reports a byte length for a redacted tool-input value so the
// log retains "how big was it" without the content itself.
func redactedLen(v interface{}) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case nil:
		return 0
	default:
		return len(fmt.Sprintf("%v", x))
	}
}
