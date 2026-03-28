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

// Event is a structured event emitted from the agent session to the orchestrator.
// Spec Section 10.4.
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

// ExtractEvent parses a JSON-RPC message into a structured Event.
// Handles token usage, rate limits, turn results, and notifications.
// Spec Section 13.5: prefer absolute thread totals.
func ExtractEvent(msg *Message) Event {
	now := time.Now().UTC()
	ev := Event{Timestamp: now}

	switch msg.Method {
	case "turn/completed":
		ev.Type = EventTurnCompleted
		extractUsageFromParams(msg.Params, &ev)

	case "turn/failed":
		ev.Type = EventTurnFailed

	case "turn/cancelled":
		ev.Type = EventTurnCancelled

	case "thread/tokenUsage/updated":
		ev.Type = EventTokenUsage
		extractTokenUsage(msg.Params, &ev)

	case "notification":
		ev.Type = EventNotification
		extractNotificationMessage(msg.Params, &ev)

	default:
		if msg.Method != "" {
			ev.Type = EventOther
			ev.Message = msg.Method
		}
	}

	// Extract rate limits from any message that carries them.
	if rl := ExtractRateLimits(msg); rl != nil {
		ev.RateLimits = rl
	}

	return ev
}

// extractUsageFromParams extracts token usage from turn/completed params.
func extractUsageFromParams(params json.RawMessage, ev *Event) {
	if params == nil {
		return
	}

	// Try common payload shapes for token usage.
	var p struct {
		Usage struct {
			TotalTokenUsage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				TotalTokens  int64 `json:"total_tokens"`
			} `json:"total_token_usage"`
		} `json:"usage"`
		TotalTokenUsage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"total_token_usage"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}

	// Prefer nested usage.total_token_usage, then top-level total_token_usage.
	if p.Usage.TotalTokenUsage.TotalTokens > 0 {
		ev.InputTokens = p.Usage.TotalTokenUsage.InputTokens
		ev.OutputTokens = p.Usage.TotalTokenUsage.OutputTokens
		ev.TotalTokens = p.Usage.TotalTokenUsage.TotalTokens
	} else if p.TotalTokenUsage.TotalTokens > 0 {
		ev.InputTokens = p.TotalTokenUsage.InputTokens
		ev.OutputTokens = p.TotalTokenUsage.OutputTokens
		ev.TotalTokens = p.TotalTokenUsage.TotalTokens
	}
}

// extractTokenUsage extracts token usage from thread/tokenUsage/updated params.
func extractTokenUsage(params json.RawMessage, ev *Event) {
	if params == nil {
		return
	}

	var p struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	ev.InputTokens = p.InputTokens
	ev.OutputTokens = p.OutputTokens
	ev.TotalTokens = p.TotalTokens
}

// extractNotificationMessage extracts a human-readable message from notification params.
func extractNotificationMessage(params json.RawMessage, ev *Event) {
	if params == nil {
		return
	}

	var p struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(params, &p); err == nil {
		ev.Message = p.Message
	}
}

// ExtractRateLimits extracts rate limit info from a message if present.
func ExtractRateLimits(msg *Message) json.RawMessage {
	if msg.Params == nil {
		return nil
	}

	var p struct {
		RateLimits json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil
	}
	return p.RateLimits
}
