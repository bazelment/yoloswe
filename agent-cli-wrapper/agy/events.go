package agy

// Event is implemented by all agy wrapper events.
type Event interface {
	eventType() string
}

// TextEvent contains the final stdout emitted by print mode.
type TextEvent struct {
	Text string
}

func (e TextEvent) eventType() string { return "text" }

// Usage carries the token accounting agy reports in its --output-format
// json result object.
type Usage struct {
	InputTokens     int
	OutputTokens    int
	ThinkingTokens  int
	CacheReadTokens int
	TotalTokens     int
}

// TurnCompleteEvent marks the end of a print-mode invocation.
//
// ConversationID and Usage are populated from agy's --output-format json
// result object (see resultPayload in session.go). ConversationID is agy's
// own id for the conversation this turn ran in — pass it to WithConversation
// on a later Session to resume with full context. It is empty if the turn
// errored before agy could assign or report one.
type TurnCompleteEvent struct {
	Error          error
	ConversationID string
	DurationMs     int64
	Usage          Usage
	Success        bool
}

func (e TurnCompleteEvent) eventType() string { return "turn_complete" }

// ErrorEvent reports a wrapper or process error.
type ErrorEvent struct {
	Error   error
	Context string
}

func (e ErrorEvent) eventType() string { return "error" }
