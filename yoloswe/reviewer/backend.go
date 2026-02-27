package reviewer

import (
	"context"
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex/render"
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
	responseText string
	success      bool
	durationMs   int64
	// turnEvent is the raw TurnComplete event for backend-specific extraction
	// (e.g., codex token usage).
	turnEvent agentstream.TurnComplete
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

// rendererEventHandler adapts EventHandler to a render.Renderer.
type rendererEventHandler struct {
	r *render.Renderer
}

func newRendererEventHandler(r *render.Renderer) *rendererEventHandler {
	return &rendererEventHandler{r: r}
}

func (h *rendererEventHandler) OnSessionInfo(sessionID, model string) {
	h.r.SessionInfo(sessionID, model)
}

func (h *rendererEventHandler) OnText(delta string) {
	h.r.Text(delta)
}

func (h *rendererEventHandler) OnReasoning(delta string) {
	h.r.Reasoning(delta)
}

func (h *rendererEventHandler) OnToolStart(name, callID string, _ map[string]interface{}) {
	h.r.CommandStart(callID, name)
}

func (h *rendererEventHandler) OnToolComplete(_ string, callID string, _ map[string]interface{}, _ interface{}, isError bool) {
	exitCode := 0
	if isError {
		exitCode = 1
	}
	h.r.CommandEnd(callID, exitCode, 0)
}

func (h *rendererEventHandler) OnTurnComplete(_ bool, _ int64) {
	// No-op: reviewer.go calls r.renderer.TurnComplete() after RunPrompt returns.
}

func (h *rendererEventHandler) OnError(err error, context string) {
	h.r.Error(err, context)
}
