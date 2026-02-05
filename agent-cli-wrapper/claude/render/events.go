// Package render provides ANSI-colored terminal rendering for Claude sessions.
package render

// EventHandler receives semantic events for structured output capture.
// All methods receive complete, accumulated data at semantic boundaries,
// unlike the streaming text output from Renderer.
//
// Implementations should be thread-safe as methods may be called from
// multiple goroutines.
type EventHandler interface {
	// OnText is called with accumulated text at semantic boundaries.
	// Unlike streaming Text() calls, this provides complete text blocks
	// (e.g., all text between tool calls).
	OnText(text string)

	// OnThinking is called when thinking/reasoning content is emitted.
	OnThinking(thinking string)

	// OnToolStart is called when a tool begins execution.
	OnToolStart(name, id string, input map[string]interface{})

	// OnToolComplete is called when a tool finishes.
	// The result may be nil if not captured; isError indicates failure.
	OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool)

	// OnTurnComplete is called when a conversation turn finishes.
	OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64)

	// OnStatus is called for status messages.
	OnStatus(msg string)

	// OnError is called for error events.
	OnError(err error, context string)
}

// NoOpEventHandler is an EventHandler that does nothing.
// Useful as a default or for testing.
type NoOpEventHandler struct{}

func (NoOpEventHandler) OnText(string)                                                     {}
func (NoOpEventHandler) OnThinking(string)                                                 {}
func (NoOpEventHandler) OnToolStart(string, string, map[string]interface{})                {}
func (NoOpEventHandler) OnToolComplete(string, string, map[string]interface{}, any, bool)  {}
func (NoOpEventHandler) OnTurnComplete(int, bool, int64, float64)                          {}
func (NoOpEventHandler) OnStatus(string)                                                   {}
func (NoOpEventHandler) OnError(error, string)                                             {}

// Ensure NoOpEventHandler implements EventHandler
var _ EventHandler = NoOpEventHandler{}
