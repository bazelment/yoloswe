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

// TurnCompleteEvent marks the end of a print-mode invocation.
type TurnCompleteEvent struct {
	Error      error
	DurationMs int64
	Success    bool
}

func (e TurnCompleteEvent) eventType() string { return "turn_complete" }

// ErrorEvent reports a wrapper or process error.
type ErrorEvent struct {
	Error   error
	Context string
}

func (e ErrorEvent) eventType() string { return "error" }
