package cursor

import "github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"

// EventType discriminates between event kinds.
type EventType int

const (
	// EventTypeReady fires when the session is initialized (system init received).
	EventTypeReady EventType = iota
	// EventTypeText fires for streaming text chunks.
	EventTypeText
	// EventTypeToolStart fires when a tool begins execution.
	EventTypeToolStart
	// EventTypeToolComplete fires when a tool execution completes.
	EventTypeToolComplete
	// EventTypeTurnComplete fires when the session finishes.
	EventTypeTurnComplete
	// EventTypeError fires on session errors.
	EventTypeError
)

// Event is the interface for all cursor events.
type Event interface {
	Type() EventType
}

// ReadyEvent fires when the system init message is received.
type ReadyEvent struct {
	SessionID string
	Model     string
}

func (e ReadyEvent) Type() EventType { return EventTypeReady }

// TextEvent contains streaming text chunks.
type TextEvent struct {
	Text     string
	FullText string
}

func (e TextEvent) Type() EventType                         { return EventTypeText }
func (e TextEvent) StreamEventKind() agentstream.EventKind  { return agentstream.KindText }
func (e TextEvent) StreamDelta() string                     { return e.Text }

// ToolStartEvent fires when a tool call starts.
type ToolStartEvent struct {
	Input map[string]interface{}
	ID    string
	Name  string
}

func (e ToolStartEvent) Type() EventType                             { return EventTypeToolStart }
func (e ToolStartEvent) StreamEventKind() agentstream.EventKind      { return agentstream.KindToolStart }
func (e ToolStartEvent) StreamToolName() string                      { return e.Name }
func (e ToolStartEvent) StreamToolCallID() string                    { return e.ID }
func (e ToolStartEvent) StreamToolInput() map[string]interface{}     { return e.Input }

// ToolCompleteEvent fires when a tool call completes.
type ToolCompleteEvent struct {
	Result  interface{}
	Input   map[string]interface{}
	ID      string
	Name    string
	IsError bool
}

func (e ToolCompleteEvent) Type() EventType                             { return EventTypeToolComplete }
func (e ToolCompleteEvent) StreamEventKind() agentstream.EventKind      { return agentstream.KindToolEnd }
func (e ToolCompleteEvent) StreamToolName() string                      { return e.Name }
func (e ToolCompleteEvent) StreamToolCallID() string                    { return e.ID }
func (e ToolCompleteEvent) StreamToolInput() map[string]interface{}     { return e.Input }
func (e ToolCompleteEvent) StreamToolResult() interface{}               { return e.Result }
func (e ToolCompleteEvent) StreamToolIsError() bool                     { return e.IsError }

// TurnCompleteEvent fires when the session result is received.
type TurnCompleteEvent struct {
	Error         error
	DurationMs    int64
	DurationAPIMs int64
	Success       bool
}

func (e TurnCompleteEvent) Type() EventType                         { return EventTypeTurnComplete }
func (e TurnCompleteEvent) StreamEventKind() agentstream.EventKind  { return agentstream.KindTurnComplete }
func (e TurnCompleteEvent) StreamTurnNum() int                      { return 1 }
func (e TurnCompleteEvent) StreamIsSuccess() bool                   { return e.Success }
func (e TurnCompleteEvent) StreamDuration() int64                   { return e.DurationMs }
func (e TurnCompleteEvent) StreamCost() float64                     { return 0 }

// ErrorEvent contains session errors.
type ErrorEvent struct {
	Error   error
	Context string
}

func (e ErrorEvent) Type() EventType                         { return EventTypeError }
func (e ErrorEvent) StreamEventKind() agentstream.EventKind  { return agentstream.KindError }
func (e ErrorEvent) StreamErr() error                        { return e.Error }
func (e ErrorEvent) StreamErrorContext() string              { return e.Context }
