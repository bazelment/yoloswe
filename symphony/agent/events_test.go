package agent

import (
	"encoding/json"
	"testing"
)

func TestExtractEvent_TurnCompleted(t *testing.T) {
	t.Parallel()
	msg := &Message{
		Method: "turn/completed",
		Params: json.RawMessage(`{
			"usage": {
				"total_token_usage": {
					"input_tokens": 1000,
					"output_tokens": 500,
					"total_tokens": 1500
				}
			}
		}`),
	}

	ev := ExtractEvent(msg)
	if ev.Type != EventTurnCompleted {
		t.Errorf("Type = %q, want turn_completed", ev.Type)
	}
	if ev.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000", ev.InputTokens)
	}
	if ev.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500", ev.OutputTokens)
	}
	if ev.TotalTokens != 1500 {
		t.Errorf("TotalTokens = %d, want 1500", ev.TotalTokens)
	}
}

func TestExtractEvent_TurnCompletedTopLevelUsage(t *testing.T) {
	t.Parallel()
	msg := &Message{
		Method: "turn/completed",
		Params: json.RawMessage(`{
			"total_token_usage": {
				"input_tokens": 2000,
				"output_tokens": 1000,
				"total_tokens": 3000
			}
		}`),
	}

	ev := ExtractEvent(msg)
	if ev.TotalTokens != 3000 {
		t.Errorf("TotalTokens = %d, want 3000", ev.TotalTokens)
	}
}

func TestExtractEvent_TokenUsageUpdated(t *testing.T) {
	t.Parallel()
	msg := &Message{
		Method: "thread/tokenUsage/updated",
		Params: json.RawMessage(`{
			"input_tokens": 5000,
			"output_tokens": 2500,
			"total_tokens": 7500
		}`),
	}

	ev := ExtractEvent(msg)
	if ev.Type != EventTokenUsage {
		t.Errorf("Type = %q, want token_usage", ev.Type)
	}
	if ev.TotalTokens != 7500 {
		t.Errorf("TotalTokens = %d, want 7500", ev.TotalTokens)
	}
}

func TestExtractEvent_TurnFailed(t *testing.T) {
	t.Parallel()
	msg := &Message{Method: "turn/failed"}
	ev := ExtractEvent(msg)
	if ev.Type != EventTurnFailed {
		t.Errorf("Type = %q, want turn_failed", ev.Type)
	}
}

func TestExtractEvent_TurnCancelled(t *testing.T) {
	t.Parallel()
	msg := &Message{Method: "turn/cancelled"}
	ev := ExtractEvent(msg)
	if ev.Type != EventTurnCancelled {
		t.Errorf("Type = %q, want turn_cancelled", ev.Type)
	}
}

func TestExtractEvent_Notification(t *testing.T) {
	t.Parallel()
	msg := &Message{
		Method: "notification",
		Params: json.RawMessage(`{"message": "Working on tests"}`),
	}
	ev := ExtractEvent(msg)
	if ev.Type != EventNotification {
		t.Errorf("Type = %q, want notification", ev.Type)
	}
	if ev.Message != "Working on tests" {
		t.Errorf("Message = %q, want 'Working on tests'", ev.Message)
	}
}

func TestExtractEvent_Unknown(t *testing.T) {
	t.Parallel()
	msg := &Message{Method: "some/other/method"}
	ev := ExtractEvent(msg)
	if ev.Type != EventOther {
		t.Errorf("Type = %q, want other_message", ev.Type)
	}
}

func TestExtractEvent_NoMethod(t *testing.T) {
	t.Parallel()
	msg := &Message{}
	ev := ExtractEvent(msg)
	if ev.Type != "" {
		t.Errorf("Type = %q, want empty", ev.Type)
	}
}

func TestExtractRateLimits(t *testing.T) {
	t.Parallel()
	msg := &Message{
		Params: json.RawMessage(`{"rate_limits": {"requests_per_minute": 100}}`),
	}
	rl := ExtractRateLimits(msg)
	if rl == nil {
		t.Fatal("expected rate limits")
	}
	if string(rl) != `{"requests_per_minute": 100}` {
		t.Errorf("rate_limits = %s", string(rl))
	}
}

func TestExtractRateLimits_None(t *testing.T) {
	t.Parallel()
	msg := &Message{Params: json.RawMessage(`{"other": "data"}`)}
	rl := ExtractRateLimits(msg)
	if rl != nil {
		t.Errorf("expected nil, got %s", string(rl))
	}
}
