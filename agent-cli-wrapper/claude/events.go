package claude

import "time"

// EventType discriminates between event kinds.
type EventType int

const (
	// EventTypeReady fires when the session is initialized.
	EventTypeReady EventType = iota
	// EventTypeText fires for streaming text chunks.
	EventTypeText
	// EventTypeThinking fires for thinking chunks.
	EventTypeThinking
	// EventTypeToolStart fires when a tool begins execution.
	EventTypeToolStart
	// EventTypeToolProgress fires as tool input streams in.
	EventTypeToolProgress
	// EventTypeToolComplete fires when tool input is fully parsed.
	EventTypeToolComplete
	// EventTypeCLIToolResult fires when CLI sends back auto-executed tool results.
	EventTypeCLIToolResult
	// EventTypeTurnComplete fires when a turn finishes.
	EventTypeTurnComplete
	// EventTypeError fires on session errors.
	EventTypeError
	// EventTypeStateChange fires on session state transitions.
	EventTypeStateChange
)

// Event is the interface for all events.
type Event interface {
	Type() EventType
}

// ReadyEvent fires when the session is initialized.
type ReadyEvent struct {
	Info SessionInfo
}

// Type returns the event type.
func (e ReadyEvent) Type() EventType { return EventTypeReady }

// TextEvent contains streaming text chunks.
type TextEvent struct {
	Text       string
	FullText   string
	TurnNumber int
}

// Type returns the event type.
func (e TextEvent) Type() EventType { return EventTypeText }

// ThinkingEvent contains thinking chunks.
type ThinkingEvent struct {
	Thinking     string
	FullThinking string
	TurnNumber   int
}

// Type returns the event type.
func (e ThinkingEvent) Type() EventType { return EventTypeThinking }

// ToolStartEvent fires when a tool begins execution.
type ToolStartEvent struct {
	Timestamp  time.Time
	ID         string
	Name       string
	TurnNumber int
}

// Type returns the event type.
func (e ToolStartEvent) Type() EventType { return EventTypeToolStart }

// ToolProgressEvent contains partial tool input.
type ToolProgressEvent struct {
	ID           string
	Name         string
	PartialInput string
	InputChunk   string
	TurnNumber   int
}

// Type returns the event type.
func (e ToolProgressEvent) Type() EventType { return EventTypeToolProgress }

// ToolCompleteEvent fires when tool input is fully parsed.
type ToolCompleteEvent struct {
	Timestamp  time.Time
	Input      map[string]interface{}
	ID         string
	Name       string
	TurnNumber int
}

// Type returns the event type.
func (e ToolCompleteEvent) Type() EventType { return EventTypeToolComplete }

// CLIToolResultEvent fires when CLI sends back auto-executed tool results.
type CLIToolResultEvent struct {
	Content    interface{}
	ToolUseID  string
	ToolName   string
	TurnNumber int
	IsError    bool
}

// Type returns the event type.
func (e CLIToolResultEvent) Type() EventType { return EventTypeCLIToolResult }

// TurnCompleteEvent fires when a turn finishes.
type TurnCompleteEvent struct {
	Error      error
	Usage      TurnUsage
	TurnNumber int
	DurationMs int64
	Success    bool
}

// Type returns the event type.
func (e TurnCompleteEvent) Type() EventType { return EventTypeTurnComplete }

// ErrorEvent contains session errors.
type ErrorEvent struct {
	Error      error
	Context    string
	TurnNumber int
}

// Type returns the event type.
func (e ErrorEvent) Type() EventType { return EventTypeError }

// StateChangeEvent fires on session state transitions.
type StateChangeEvent struct {
	From SessionState
	To   SessionState
}

// Type returns the event type.
func (e StateChangeEvent) Type() EventType { return EventTypeStateChange }
