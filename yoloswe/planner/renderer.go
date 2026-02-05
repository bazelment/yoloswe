package planner

import (
	"io"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
)

// QuestionOption is an alias for render.QuestionOption for convenience.
type QuestionOption = render.QuestionOption

// Renderer wraps the shared render.Renderer with yoloplanner-specific methods.
type Renderer struct {
	*render.Renderer
}

// NewRenderer creates a new renderer writing to the given output.
// If verbose is false, only error tool results are displayed.
func NewRenderer(out io.Writer, verbose bool) *Renderer {
	return &Renderer{
		Renderer: render.NewRenderer(out, verbose),
	}
}

// NewRendererWithEvents creates a new renderer that emits semantic events.
// The event handler receives structured events for tool calls, text blocks, etc.
func NewRendererWithEvents(out io.Writer, verbose bool, handler render.EventHandler) *Renderer {
	return &Renderer{
		Renderer: render.NewRendererWithEvents(out, verbose, handler),
	}
}

// TurnSummary prints a summary of the completed turn using claude.TurnCompleteEvent.
func (r *Renderer) TurnSummary(e claude.TurnCompleteEvent) {
	r.Renderer.TurnSummary(e.TurnNumber, e.Success, e.DurationMs, e.Usage.CostUSD)
}
