package agent

import (
	"encoding/json"
	"time"
)

// EventType identifies the kind of agent event.
type EventType string

const (
	EventSessionStarted  EventType = "session_started"
	EventTurnCompleted   EventType = "turn_completed"
	EventTurnFailed      EventType = "turn_failed"
	EventTurnCancelled   EventType = "turn_cancelled"
	EventApprovalHandled EventType = "approval_auto_approved"
	EventUnsupportedTool EventType = "unsupported_tool_call"
	EventInputRequired   EventType = "turn_input_required"
	EventTokenUsage      EventType = "token_usage"
	EventRateLimit       EventType = "rate_limit"
	EventNotification    EventType = "notification"
	EventOther           EventType = "other_message"
)

// Event is a structured event emitted from an agent session to the orchestrator.
type Event struct {
	Timestamp    time.Time
	PID          *int
	Type         EventType
	SessionID    string
	ThreadID     string
	TurnID       string
	Message      string
	RateLimits   json.RawMessage
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}
