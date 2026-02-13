package claude

import (
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
)

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

func (e TextEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindText }
func (e TextEvent) StreamDelta() string                    { return e.Text }

// ThinkingEvent contains thinking chunks.
type ThinkingEvent struct {
	Thinking     string
	FullThinking string
	TurnNumber   int
}

// Type returns the event type.
func (e ThinkingEvent) Type() EventType { return EventTypeThinking }

func (e ThinkingEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindThinking }
func (e ThinkingEvent) StreamDelta() string                    { return e.Thinking }

// ToolStartEvent fires when a tool begins execution.
type ToolStartEvent struct {
	Timestamp  time.Time
	ID         string
	Name       string
	TurnNumber int
}

// Type returns the event type.
func (e ToolStartEvent) Type() EventType { return EventTypeToolStart }

func (e ToolStartEvent) StreamEventKind() agentstream.EventKind  { return agentstream.KindToolStart }
func (e ToolStartEvent) StreamToolName() string                  { return e.Name }
func (e ToolStartEvent) StreamToolCallID() string                { return e.ID }
func (e ToolStartEvent) StreamToolInput() map[string]interface{} { return nil }

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

func (e ToolCompleteEvent) StreamEventKind() agentstream.EventKind  { return agentstream.KindToolEnd }
func (e ToolCompleteEvent) StreamToolName() string                  { return e.Name }
func (e ToolCompleteEvent) StreamToolCallID() string                { return e.ID }
func (e ToolCompleteEvent) StreamToolInput() map[string]interface{} { return e.Input }
func (e ToolCompleteEvent) StreamToolResult() interface{}           { return nil }
func (e ToolCompleteEvent) StreamToolIsError() bool                 { return false }

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

func (e TurnCompleteEvent) StreamEventKind() agentstream.EventKind {
	return agentstream.KindTurnComplete
}
func (e TurnCompleteEvent) StreamTurnNum() int    { return e.TurnNumber }
func (e TurnCompleteEvent) StreamIsSuccess() bool { return e.Success }
func (e TurnCompleteEvent) StreamDuration() int64 { return e.DurationMs }
func (e TurnCompleteEvent) StreamCost() float64   { return e.Usage.CostUSD }

// ErrorEvent contains session errors.
type ErrorEvent struct {
	Error      error
	Context    string
	TurnNumber int
}

// Type returns the event type.
func (e ErrorEvent) Type() EventType { return EventTypeError }

func (e ErrorEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindError }
func (e ErrorEvent) StreamErr() error                       { return e.Error }
func (e ErrorEvent) StreamErrorContext() string             { return e.Context }

// StateChangeEvent fires on session state transitions.
type StateChangeEvent struct {
	From SessionState
	To   SessionState
}

// Type returns the event type.
func (e StateChangeEvent) Type() EventType { return EventTypeStateChange }
