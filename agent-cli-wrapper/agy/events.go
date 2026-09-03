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

// Usage carries agy's token accounting.
type Usage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

// TurnCompleteEvent marks the end of a print-mode invocation. ConversationID
// resumes later turns, and Usage comes from agy's JSON result.
//
// Model is the id that actually reached the CLI, which is not always the one
// the caller configured: BuildCLIArgs retargets a model whose pinned level
// disagrees with an explicit --effort (see reconcileModelEffort). Callers that
// report "the model we ran" must read this rather than their own config.
type TurnCompleteEvent struct {
	Error          error
	ConversationID string
	Model          string
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
