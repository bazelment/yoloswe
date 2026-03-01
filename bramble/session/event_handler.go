package session

import (
	"fmt"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/bramble/sessionmodel"
)

// sessionEventHandler implements render.EventHandler for TUI capture.
// It converts semantic events from the renderer into structured OutputLine
// entries that the TUI can display.
type sessionEventHandler struct {
	manager   *Manager
	sessionID SessionID
}

// Ensure interface compliance at compile time
var _ render.EventHandler = (*sessionEventHandler)(nil)

// newSessionEventHandler creates a new event handler for a session.
func newSessionEventHandler(manager *Manager, sessionID SessionID) *sessionEventHandler {
	return &sessionEventHandler{
		manager:   manager,
		sessionID: sessionID,
	}
}

func (h *sessionEventHandler) OnText(text string) {
	// Append to the last text line if it exists (streaming accumulation),
	// otherwise create a new text line.
	h.manager.appendOrAddText(h.sessionID, text)
}

func (h *sessionEventHandler) OnThinking(thinking string) {
	h.manager.appendOrAddThinking(h.sessionID, thinking)
}

func (h *sessionEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	now := time.Now()
	h.manager.addOutput(h.sessionID, OutputLine{
		Timestamp: now,
		Type:      OutputTypeToolStart,
		Content:   formatToolContent(name, input),
		ToolName:  name,
		ToolID:    id,
		ToolInput: input,
		ToolState: ToolStateRunning,
		StartTime: now,
	})

	// Update session progress
	h.manager.updateSessionProgress(h.sessionID, func(p *SessionProgress) {
		p.CurrentTool = name
		p.CurrentPhase = "tool_execution"
		p.LastActivity = time.Now()
	})
}

func (h *sessionEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	now := time.Now()

	// Update the existing tool line in-place
	h.manager.updateToolOutput(h.sessionID, id, func(line *OutputLine) {
		// Update input and content (input is nil at OnToolStart, available now)
		if input != nil {
			line.ToolInput = input
			line.Content = formatToolContent(name, input)
		}
		line.ToolResult = result
		line.IsError = isError
		if isError {
			line.ToolState = ToolStateError
		} else {
			line.ToolState = ToolStateComplete
		}
		// Calculate duration from StartTime
		if !line.StartTime.IsZero() {
			line.DurationMs = now.Sub(line.StartTime).Milliseconds()
		}
	})

	h.manager.updateSessionProgress(h.sessionID, func(p *SessionProgress) {
		p.CurrentTool = ""
		p.CurrentPhase = ""
		p.LastActivity = time.Now()
	})
}

// formatToolContent creates a display-friendly content string for tool results.
// Delegates to sessionmodel.FormatToolContent to avoid code duplication.
func formatToolContent(name string, input map[string]interface{}) string {
	return sessionmodel.FormatToolContent(name, input)
}

func (h *sessionEventHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
	h.manager.addOutput(h.sessionID, OutputLine{
		Timestamp:  time.Now(),
		Type:       OutputTypeTurnEnd,
		Content:    fmt.Sprintf("Turn %d complete", turnNumber),
		TurnNumber: turnNumber,
		CostUSD:    costUSD,
		DurationMs: durationMs,
		IsError:    !success,
	})
}

func (h *sessionEventHandler) OnStatus(msg string) {
	h.manager.addOutput(h.sessionID, OutputLine{
		Timestamp: time.Now(),
		Type:      OutputTypeStatus,
		Content:   msg,
	})
}

func (h *sessionEventHandler) OnError(err error, context string) {
	h.manager.addOutput(h.sessionID, OutputLine{
		Timestamp: time.Now(),
		Type:      OutputTypeError,
		Content:   fmt.Sprintf("%s: %v", context, err),
		IsError:   true,
	})
}
