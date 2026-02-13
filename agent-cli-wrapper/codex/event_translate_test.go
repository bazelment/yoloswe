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
