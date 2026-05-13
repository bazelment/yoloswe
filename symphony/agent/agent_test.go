package agent

import "testing"

func TestIsValidAgentType(t *testing.T) {
	t.Parallel()

	for _, typ := range []string{"", AgentTypeCodex} {
		if !IsValidAgentType(typ) {
			t.Fatalf("IsValidAgentType(%q) = false, want true", typ)
		}
	}

	for _, typ := range []string{"codxe", "claude", "gemini"} {
		if IsValidAgentType(typ) {
			t.Fatalf("IsValidAgentType(%q) = true, want false", typ)
		}
	}
}

func TestPublicConstants(t *testing.T) {
	t.Parallel()

	if AgentTypeCodex != "codex" {
		t.Fatalf("AgentTypeCodex = %q, want codex", AgentTypeCodex)
	}

	statuses := map[TurnStatus]string{
		TurnCompleted: "completed",
		TurnFailed:    "failed",
		TurnTimedOut:  "timed_out",
		TurnCancelled: "cancelled",
	}
	for status, want := range statuses {
		if string(status) != want {
			t.Fatalf("TurnStatus %v = %q, want %q", status, string(status), want)
		}
	}
}

func TestEventTypeConstants(t *testing.T) {
	t.Parallel()

	events := map[EventType]string{
		EventSessionStarted:  "session_started",
		EventTurnCompleted:   "turn_completed",
		EventTurnFailed:      "turn_failed",
		EventTurnCancelled:   "turn_cancelled",
		EventApprovalHandled: "approval_auto_approved",
		EventUnsupportedTool: "unsupported_tool_call",
		EventInputRequired:   "turn_input_required",
		EventTokenUsage:      "token_usage",
		EventRateLimit:       "rate_limit",
		EventNotification:    "notification",
		EventOther:           "other_message",
	}
	for eventType, want := range events {
		if string(eventType) != want {
			t.Fatalf("EventType %v = %q, want %q", eventType, string(eventType), want)
		}
	}
}
