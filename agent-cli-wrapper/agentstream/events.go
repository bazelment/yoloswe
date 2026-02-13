package agentstream

// EventKind identifies the common event category for cross-provider bridging.
type EventKind int

const (
	// KindUnknown is the zero value. Events returning KindUnknown are skipped
	// by the generic bridge (e.g., ACP ToolCallUpdateEvent with non-terminal status).
	KindUnknown EventKind = iota
	KindText
	KindThinking
	KindToolStart
	KindToolEnd
	KindTurnComplete
	KindError
)

// Event is the common interface that SDK event types implement to participate
// in the generic provider bridge. SDK events that don't implement this
// interface are silently skipped.
type Event interface {
	StreamEventKind() EventKind
}

// Text provides streaming text deltas.
// Method names are prefixed with "Stream" to avoid conflicts with SDK struct fields.
type Text interface {
	Event
	StreamDelta() string
}

// ToolStart provides tool invocation start metadata.
// Method names are prefixed with "Stream" to avoid conflicts with SDK struct fields.
type ToolStart interface {
	Event
	StreamToolName() string
	StreamToolCallID() string
	StreamToolInput() map[string]interface{}
}

// ToolEnd provides tool invocation completion metadata.
// Method names are prefixed with "Stream" to avoid conflicts with SDK struct fields.
type ToolEnd interface {
	Event
	StreamToolName() string
	StreamToolCallID() string
	StreamToolInput() map[string]interface{}
	StreamToolResult() interface{}
	StreamToolIsError() bool
}

// TurnComplete provides turn completion metadata.
type TurnComplete interface {
	Event
	StreamTurnNum() int
	StreamIsSuccess() bool
	StreamDuration() int64
	StreamCost() float64
}

// Error provides error information.
type Error interface {
	Event
	StreamErr() error
	StreamErrorContext() string
}

// Scoped is an optional interface for events that belong to a named scope
// (e.g., codex thread ID). When the bridge is configured with a non-empty
// scopeID, events implementing Scoped are filtered by ScopeID() match.
type Scoped interface {
	ScopeID() string
}
