package reviewer

import (
	"context"

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
	OnText(delta string)
	OnReasoning(delta string)
	OnToolStart(callID, name, input string)
	OnToolOutput(callID, output string)
	OnToolEnd(callID string, exitCode int, durationMs int64)
	OnError(err error, context string)
}

// rendererEventHandler adapts EventHandler to a render.Renderer.
type rendererEventHandler struct {
	r *render.Renderer
}

func newRendererEventHandler(r *render.Renderer) *rendererEventHandler {
	return &rendererEventHandler{r: r}
}

func (h *rendererEventHandler) OnText(delta string) {
	h.r.Text(delta)
}

func (h *rendererEventHandler) OnReasoning(delta string) {
	h.r.Reasoning(delta)
}

func (h *rendererEventHandler) OnToolStart(callID, name, input string) {
	h.r.CommandStart(callID, name)
}

func (h *rendererEventHandler) OnToolOutput(callID, output string) {
	h.r.CommandOutput(callID, output)
}

func (h *rendererEventHandler) OnToolEnd(callID string, exitCode int, durationMs int64) {
	h.r.CommandEnd(callID, exitCode, durationMs)
}

func (h *rendererEventHandler) OnError(err error, context string) {
	h.r.Error(err, context)
}
