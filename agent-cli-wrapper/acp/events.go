package acp

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

// ThinkingDeltaEvent contains streaming thought/reasoning content.
type ThinkingDeltaEvent struct {
	SessionID string
	Delta     string
	FullText  string // Accumulated thinking so far
}

// Type returns the event type.
func (e ThinkingDeltaEvent) Type() EventType { return EventTypeThinkingDelta }

// ToolCallStartEvent fires when a tool call starts.
type ToolCallStartEvent struct {
	Input      map[string]interface{}
	SessionID  string
	ToolCallID string
	ToolName   string
}

// Type returns the event type.
func (e ToolCallStartEvent) Type() EventType { return EventTypeToolCallStart }

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
