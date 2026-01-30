package codex

import "time"

// EventType discriminates between event kinds.
type EventType int

const (
	// EventTypeClientReady fires when the client is initialized.
	EventTypeClientReady EventType = iota

	// EventTypeThreadStarted fires when a thread is created.
	EventTypeThreadStarted

	// EventTypeThreadReady fires when MCP startup completes for a thread.
	EventTypeThreadReady

	// EventTypeTurnStarted fires when a turn begins.
	EventTypeTurnStarted

	// EventTypeTurnCompleted fires when a turn finishes.
	EventTypeTurnCompleted

	// EventTypeTextDelta fires for streaming text chunks.
	EventTypeTextDelta

	// EventTypeItemStarted fires when an item (message, tool) starts.
	EventTypeItemStarted

	// EventTypeItemCompleted fires when an item completes.
	EventTypeItemCompleted

	// EventTypeTokenUsage fires with token usage information.
	EventTypeTokenUsage

	// EventTypeError fires on errors.
	EventTypeError

	// EventTypeStateChange fires on state transitions.
	EventTypeStateChange

	// EventTypeCommandStart fires when a shell command begins.
	EventTypeCommandStart

	// EventTypeCommandOutput fires for streaming command output.
	EventTypeCommandOutput

	// EventTypeCommandEnd fires when a shell command completes.
	EventTypeCommandEnd

	// EventTypeReasoningDelta fires for streaming reasoning/thinking text.
	EventTypeReasoningDelta
)

// Event is the interface for all events.
type Event interface {
	Type() EventType
}

// ClientReadyEvent fires when the client is initialized.
type ClientReadyEvent struct {
	UserAgent string
}

// Type returns the event type.
func (e ClientReadyEvent) Type() EventType { return EventTypeClientReady }

// ThreadStartedEvent fires when a thread is created.
type ThreadStartedEvent struct {
	ThreadID      string
	Model         string
	ModelProvider string
	WorkDir       string
}

// Type returns the event type.
func (e ThreadStartedEvent) Type() EventType { return EventTypeThreadStarted }

// ThreadReadyEvent fires when MCP startup completes for a thread.
type ThreadReadyEvent struct {
	ThreadID string
}

// Type returns the event type.
func (e ThreadReadyEvent) Type() EventType { return EventTypeThreadReady }

// TurnStartedEvent fires when a turn begins.
type TurnStartedEvent struct {
	ThreadID string
	TurnID   string
}

// Type returns the event type.
func (e TurnStartedEvent) Type() EventType { return EventTypeTurnStarted }

// TurnCompletedEvent fires when a turn finishes.
type TurnCompletedEvent struct {
	ThreadID   string
	TurnID     string
	Success    bool
	FullText   string
	DurationMs int64
	Usage      TurnUsage
	Error      error
}

// Type returns the event type.
func (e TurnCompletedEvent) Type() EventType { return EventTypeTurnCompleted }

// TextDeltaEvent contains streaming text chunks.
type TextDeltaEvent struct {
	ThreadID string
	TurnID   string
	ItemID   string
	Delta    string   // New text chunk
	FullText string   // Accumulated text so far
}

// Type returns the event type.
func (e TextDeltaEvent) Type() EventType { return EventTypeTextDelta }

// ItemStartedEvent fires when an item (message, tool) starts.
type ItemStartedEvent struct {
	ThreadID string
	TurnID   string
	ItemID   string
	ItemType string
}

// Type returns the event type.
func (e ItemStartedEvent) Type() EventType { return EventTypeItemStarted }

// ItemCompletedEvent fires when an item completes.
type ItemCompletedEvent struct {
	ThreadID string
	TurnID   string
	ItemID   string
	ItemType string
	Text     string // For message items
}

// Type returns the event type.
func (e ItemCompletedEvent) Type() EventType { return EventTypeItemCompleted }

// TokenUsageEvent contains token usage information.
type TokenUsageEvent struct {
	ThreadID   string
	TotalUsage *TokenUsage
	LastUsage  *TokenUsage
	RateLimits *RateLimits
}

// Type returns the event type.
func (e TokenUsageEvent) Type() EventType { return EventTypeTokenUsage }

// TurnUsage contains token usage for a turn.
type TurnUsage struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
}

// ErrorEvent contains errors.
type ErrorEvent struct {
	ThreadID  string
	TurnID    string
	Error     error
	Context   string
	Timestamp time.Time
}

// Type returns the event type.
func (e ErrorEvent) Type() EventType { return EventTypeError }

// StateChangeEvent fires on state transitions.
type StateChangeEvent struct {
	ThreadID string // Empty for client-level state changes
	From     string
	To       string
}

// Type returns the event type.
func (e StateChangeEvent) Type() EventType { return EventTypeStateChange }

// CommandStartEvent fires when a shell command begins.
type CommandStartEvent struct {
	ThreadID  string
	TurnID    string
	CallID    string
	Command   []string // e.g., ["/bin/zsh", "-lc", "ls"]
	CWD       string
	ParsedCmd string // Simplified command display
}

// Type returns the event type.
func (e CommandStartEvent) Type() EventType { return EventTypeCommandStart }

// CommandOutputEvent fires for streaming command output.
type CommandOutputEvent struct {
	ThreadID string
	TurnID   string
	CallID   string
	Stream   string // "stdout" or "stderr"
	Chunk    string
}

// Type returns the event type.
func (e CommandOutputEvent) Type() EventType { return EventTypeCommandOutput }

// CommandEndEvent fires when a shell command completes.
type CommandEndEvent struct {
	ThreadID   string
	TurnID     string
	CallID     string
	ExitCode   int
	Stdout     string
	Stderr     string
	DurationMs int64
}

// Type returns the event type.
func (e CommandEndEvent) Type() EventType { return EventTypeCommandEnd }

// ReasoningDeltaEvent fires for streaming reasoning/thinking text.
type ReasoningDeltaEvent struct {
	ThreadID string
	TurnID   string
	ItemID   string
	Delta    string
}

// Type returns the event type.
func (e ReasoningDeltaEvent) Type() EventType { return EventTypeReasoningDelta }
