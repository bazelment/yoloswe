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
	TurnNumber int
	Text       string // New text chunk
	FullText   string // Accumulated text
}

// Type returns the event type.
func (e TextEvent) Type() EventType { return EventTypeText }

// ThinkingEvent contains thinking chunks.
type ThinkingEvent struct {
	TurnNumber   int
	Thinking     string // New thinking chunk
	FullThinking string // Accumulated thinking
}

// Type returns the event type.
func (e ThinkingEvent) Type() EventType { return EventTypeThinking }

// ToolStartEvent fires when a tool begins execution.
type ToolStartEvent struct {
	TurnNumber int
	ID         string
	Name       string
	Timestamp  time.Time
}

// Type returns the event type.
func (e ToolStartEvent) Type() EventType { return EventTypeToolStart }

// ToolProgressEvent contains partial tool input.
type ToolProgressEvent struct {
	TurnNumber   int
	ID           string
	Name         string
	PartialInput string // Accumulated partial JSON
	InputChunk   string // New chunk
}

// Type returns the event type.
func (e ToolProgressEvent) Type() EventType { return EventTypeToolProgress }

// ToolCompleteEvent fires when tool input is fully parsed.
type ToolCompleteEvent struct {
	TurnNumber int
	ID         string
	Name       string
	Input      map[string]interface{}
	Timestamp  time.Time
}

// Type returns the event type.
func (e ToolCompleteEvent) Type() EventType { return EventTypeToolComplete }

// CLIToolResultEvent fires when CLI sends back auto-executed tool results.
type CLIToolResultEvent struct {
	TurnNumber int
	ToolUseID  string
	ToolName   string
	Content    interface{} // string or []ContentBlock
	IsError    bool
}

// Type returns the event type.
func (e CLIToolResultEvent) Type() EventType { return EventTypeCLIToolResult }

// TurnCompleteEvent fires when a turn finishes.
type TurnCompleteEvent struct {
	TurnNumber int
	Success    bool
	DurationMs int64
	Usage      TurnUsage
	Error      error
}

// Type returns the event type.
func (e TurnCompleteEvent) Type() EventType { return EventTypeTurnComplete }

// ErrorEvent contains session errors.
type ErrorEvent struct {
	TurnNumber int
	Error      error
	Context    string
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
