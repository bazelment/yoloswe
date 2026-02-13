package acp

import "github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"

// EventType discriminates between event kinds.
type EventType int

const (
	// EventTypeClientReady fires when the client is initialized.
	EventTypeClientReady EventType = iota

	// EventTypeSessionCreated fires when a session is created.
	EventTypeSessionCreated

	// EventTypeTextDelta fires for streaming text chunks.
	EventTypeTextDelta

	// EventTypeThinkingDelta fires for thought/reasoning content.
	EventTypeThinkingDelta

	// EventTypeToolCallStart fires when a tool call begins.
	EventTypeToolCallStart

	// EventTypeToolCallUpdate fires when a tool call status changes.
	EventTypeToolCallUpdate

	// EventTypeTurnComplete fires when a prompt turn completes.
	EventTypeTurnComplete

	// EventTypePlanUpdate fires when the agent updates its plan.
	EventTypePlanUpdate

	// EventTypeError fires on errors.
	EventTypeError
)

// Event is the interface for all ACP SDK events.
type Event interface {
	Type() EventType
}

// ClientReadyEvent fires when the ACP client is initialized.
type ClientReadyEvent struct {
	AgentName    string
	AgentVersion string
}

// Type returns the event type.
func (e ClientReadyEvent) Type() EventType { return EventTypeClientReady }

// SessionCreatedEvent fires when a session is created.
type SessionCreatedEvent struct {
	SessionID string
}

// Type returns the event type.
func (e SessionCreatedEvent) Type() EventType { return EventTypeSessionCreated }

// TextDeltaEvent contains streaming text from the agent.
type TextDeltaEvent struct {
	SessionID string
	Delta     string // New text chunk
	FullText  string // Accumulated text so far
}

// Type returns the event type.
func (e TextDeltaEvent) Type() EventType { return EventTypeTextDelta }

func (e TextDeltaEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindText }
func (e TextDeltaEvent) StreamDelta() string                    { return e.Delta }

// ThinkingDeltaEvent contains streaming thought/reasoning content.
type ThinkingDeltaEvent struct {
	SessionID string
	Delta     string
	FullText  string // Accumulated thinking so far
}

// Type returns the event type.
func (e ThinkingDeltaEvent) Type() EventType { return EventTypeThinkingDelta }

func (e ThinkingDeltaEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindThinking }
func (e ThinkingDeltaEvent) StreamDelta() string                    { return e.Delta }

// ToolCallStartEvent fires when a tool call starts.
type ToolCallStartEvent struct {
	Input      map[string]interface{}
	SessionID  string
	ToolCallID string
	ToolName   string
}

// Type returns the event type.
func (e ToolCallStartEvent) Type() EventType { return EventTypeToolCallStart }

func (e ToolCallStartEvent) StreamEventKind() agentstream.EventKind  { return agentstream.KindToolStart }
func (e ToolCallStartEvent) StreamToolName() string                  { return e.ToolName }
func (e ToolCallStartEvent) StreamToolCallID() string                { return e.ToolCallID }
func (e ToolCallStartEvent) StreamToolInput() map[string]interface{} { return e.Input }

// ToolCallUpdateEvent fires when a tool call status changes.
type ToolCallUpdateEvent struct {
	Input      map[string]interface{}
	SessionID  string
	ToolCallID string
	ToolName   string
	Status     string // "running", "completed", "errored"
}

// Type returns the event type.
func (e ToolCallUpdateEvent) Type() EventType { return EventTypeToolCallUpdate }

func (e ToolCallUpdateEvent) StreamEventKind() agentstream.EventKind {
	if e.Status == "completed" || e.Status == "errored" {
		return agentstream.KindToolEnd
	}
	return agentstream.KindUnknown
}
func (e ToolCallUpdateEvent) StreamToolName() string                  { return e.ToolName }
func (e ToolCallUpdateEvent) StreamToolCallID() string                { return e.ToolCallID }
func (e ToolCallUpdateEvent) StreamToolInput() map[string]interface{} { return e.Input }
func (e ToolCallUpdateEvent) StreamToolResult() interface{}           { return nil }
func (e ToolCallUpdateEvent) StreamToolIsError() bool                 { return e.Status == "errored" }

// TurnCompleteEvent fires when a prompt turn completes.
type TurnCompleteEvent struct {
	Error      error
	SessionID  string
	FullText   string
	Thinking   string
	StopReason string
	DurationMs int64
	Success    bool
}

// Type returns the event type.
func (e TurnCompleteEvent) Type() EventType { return EventTypeTurnComplete }

func (e TurnCompleteEvent) StreamEventKind() agentstream.EventKind {
	return agentstream.KindTurnComplete
}
func (e TurnCompleteEvent) StreamTurnNum() int    { return 1 }
func (e TurnCompleteEvent) StreamIsSuccess() bool { return e.Success }
func (e TurnCompleteEvent) StreamDuration() int64 { return e.DurationMs }
func (e TurnCompleteEvent) StreamCost() float64   { return 0 }

// PlanUpdateEvent fires when the agent updates its plan.
type PlanUpdateEvent struct {
	Plan      *Plan
	SessionID string
}

// Type returns the event type.
func (e PlanUpdateEvent) Type() EventType { return EventTypePlanUpdate }

// ErrorEvent fires on errors.
type ErrorEvent struct {
	Error     error
	SessionID string
	Context   string
}

// Type returns the event type.
func (e ErrorEvent) Type() EventType { return EventTypeError }

func (e ErrorEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindError }
func (e ErrorEvent) StreamErr() error                       { return e.Error }
func (e ErrorEvent) StreamErrorContext() string             { return e.Context }
