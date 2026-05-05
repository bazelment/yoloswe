package codex

import (
	"encoding/json"
	"testing"
)

func TestParseMappedNotification_TurnCompleted(t *testing.T) {
	params := json.RawMessage(`{"threadId":"t1","turn":{"id":"turn-2","status":"completed","error":null,"items":[]}}`)
	ev, ok := ParseMappedNotification(NotifyTurnCompleted, params)
	if !ok {
		t.Fatal("expected mapped event")
	}
	if ev.Kind != MappedEventTurnCompleted {
		t.Fatalf("Kind = %v, want turn completed", ev.Kind)
	}
	if ev.ThreadID != "t1" {
		t.Fatalf("ThreadID = %q, want %q", ev.ThreadID, "t1")
	}
	if ev.TurnID != "turn-2" {
		t.Fatalf("TurnID = %q, want %q", ev.TurnID, "turn-2")
	}
	if !ev.Success {
		t.Fatal("Success = false, want true")
	}
}

// TestParseMappedNotification_TokenCount_FallbackToTotal verifies that when
// a TokenCount notification carries only TotalTokenUsage, the mapper still
// emits a usage event by falling back to the cumulative total.
func TestParseMappedNotification_TokenCount_FallbackToTotal(t *testing.T) {
	notif := CodexEventNotification{
		ConversationID: "conv-1",
		Msg: mustJSON(t, TokenCountMsg{
			Info: &TokenUsageInfo{
				TotalTokenUsage: &TokenUsage{InputTokens: 200, OutputTokens: 75, TotalTokens: 275},
			},
		}),
	}
	params := mustJSON(t, notif)

	ev, ok := ParseMappedNotification(NotifyCodexEventTokenCount, params)
	if !ok {
		t.Fatal("expected mapped event when only TotalTokenUsage is set")
	}
	if ev.Kind != MappedEventTokenUsage {
		t.Fatalf("Kind = %v, want token usage", ev.Kind)
	}
	if ev.Usage.InputTokens != 200 || ev.Usage.OutputTokens != 75 {
		t.Fatalf("Usage = %+v, want input=200 output=75", ev.Usage)
	}
	if !ev.UsageIsCumulative {
		t.Fatal("UsageIsCumulative = false, want true (only TotalTokenUsage was set)")
	}
}

// TestParseMappedNotification_TokenCount_EmptyLastFallsBackCumulative
// verifies that when LastTokenUsage is present-but-all-zero (the
// `last_token_usage: {}` wire shape) and TotalTokenUsage is populated,
// the mapper falls back to TotalTokenUsage AND sets UsageIsCumulative
// so replay subtracts the baseline. Without this, the producer/consumer
// would desync: PreferredUsage falls through but UsageIsCumulative
// stays false, and replay would render cumulative totals as per-turn.
func TestParseMappedNotification_TokenCount_EmptyLastFallsBackCumulative(t *testing.T) {
	notif := CodexEventNotification{
		ConversationID: "conv-1",
		Msg: mustJSON(t, TokenCountMsg{
			Info: &TokenUsageInfo{
				TotalTokenUsage: &TokenUsage{InputTokens: 200, OutputTokens: 75, TotalTokens: 275},
				LastTokenUsage:  &TokenUsage{}, // empty struct, all zeros
			},
		}),
	}
	params := mustJSON(t, notif)

	ev, ok := ParseMappedNotification(NotifyCodexEventTokenCount, params)
	if !ok {
		t.Fatal("expected mapped event for empty-Last fallback")
	}
	if ev.Usage.InputTokens != 200 || ev.Usage.OutputTokens != 75 {
		t.Fatalf("Usage = %+v, want input=200 output=75 (from Total fallback)", ev.Usage)
	}
	if !ev.UsageIsCumulative {
		t.Fatal("UsageIsCumulative = false, want true (Total was the fallback source)")
	}
}

// TestParseMappedNotification_TokenCount_PrefersLast verifies that when both
// LastTokenUsage and TotalTokenUsage are present, the per-turn last value wins.
func TestParseMappedNotification_TokenCount_PrefersLast(t *testing.T) {
	notif := CodexEventNotification{
		ConversationID: "conv-1",
		Msg: mustJSON(t, TokenCountMsg{
			Info: &TokenUsageInfo{
				TotalTokenUsage: &TokenUsage{InputTokens: 1000, OutputTokens: 500},
				LastTokenUsage:  &TokenUsage{InputTokens: 100, OutputTokens: 50},
			},
		}),
	}
	params := mustJSON(t, notif)

	ev, ok := ParseMappedNotification(NotifyCodexEventTokenCount, params)
	if !ok {
		t.Fatal("expected mapped event")
	}
	if ev.Usage.InputTokens != 100 || ev.Usage.OutputTokens != 50 {
		t.Fatalf("Usage = %+v, want input=100 output=50 (from Last, not Total)", ev.Usage)
	}
	if ev.UsageIsCumulative {
		t.Fatal("UsageIsCumulative = true, want false (LastTokenUsage was per-turn)")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestTurnNumberFromID(t *testing.T) {
	tests := []struct {
		name   string
		turnID string
		want   int
	}{
		{name: "numeric-zero-based", turnID: "2", want: 3},
		{name: "prefixed", turnID: "turn-456", want: 456},
		{name: "prefixed-zero", turnID: "turn-0", want: 1},
		{name: "invalid", turnID: "n/a", want: 1},
		{name: "empty", turnID: "", want: 1},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := TurnNumberFromID(tc.turnID); got != tc.want {
				t.Fatalf("TurnNumberFromID(%q) = %d, want %d", tc.turnID, got, tc.want)
			}
		})
	}
}
