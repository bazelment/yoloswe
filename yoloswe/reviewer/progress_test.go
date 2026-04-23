package reviewer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNDJSONProgressEmitter_WritesOneLinePerEvent(t *testing.T) {
	var buf bytes.Buffer
	e := NewNDJSONProgressEmitter(&buf, 0)
	e.Emit(ProgressEvent{Kind: ProgressKindSessionStarted, Backend: "codex"})
	e.Emit(ProgressEvent{Kind: ProgressKindToolUse, Tool: "read"})
	e.Emit(ProgressEvent{Kind: ProgressKindVerdict, IssueCount: 2, Detail: "rejected"})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d: %q", len(lines), buf.String())
	}
	for i, line := range lines {
		var got ProgressEvent
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Errorf("line %d not valid JSON: %q err=%v", i, line, err)
			continue
		}
		if got.Event != "progress" {
			t.Errorf("line %d event = %q, want %q", i, got.Event, "progress")
		}
	}
}

func TestNDJSONProgressEmitter_CoalescesToolUseBurst(t *testing.T) {
	// Monitor's contract is one notification per event; a reviewer that
	// spams 50 read calls in a single turn must not translate to 50
	// notifications. The coalescer emits the first event in a burst and
	// drops subsequent same-(kind, tool) events inside the interval.
	var buf bytes.Buffer
	e := NewNDJSONProgressEmitter(&buf, 10*time.Second)
	start := time.Unix(1_000_000, 0)
	e.SetNow(func() time.Time { return start })
	for i := 0; i < 50; i++ {
		e.Emit(ProgressEvent{Kind: ProgressKindToolUse, Tool: "read"})
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("want 1 emitted line, got %d: %q", len(lines), buf.String())
	}
}

func TestNDJSONProgressEmitter_CoalescerReleasesAfterInterval(t *testing.T) {
	var buf bytes.Buffer
	e := NewNDJSONProgressEmitter(&buf, 10*time.Second)
	base := time.Unix(1_000_000, 0)
	now := base
	e.SetNow(func() time.Time { return now })
	e.Emit(ProgressEvent{Kind: ProgressKindToolUse, Tool: "read"})
	now = base.Add(5 * time.Second)
	e.Emit(ProgressEvent{Kind: ProgressKindToolUse, Tool: "read"}) // suppressed
	now = base.Add(11 * time.Second)
	e.Emit(ProgressEvent{Kind: ProgressKindToolUse, Tool: "read"}) // passes

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (suppressed middle), got %d: %q", len(lines), buf.String())
	}
}

func TestNDJSONProgressEmitter_StructuralEventsNeverCoalesced(t *testing.T) {
	// session-started, verdict, turn-complete, and error are structural
	// markers — consumers use them to bracket the review. Suppressing any
	// of them would leave an ambiguous stream, so the coalescer must let
	// them through unconditionally regardless of how close in time.
	var buf bytes.Buffer
	e := NewNDJSONProgressEmitter(&buf, 10*time.Second)
	fixed := time.Unix(1_000_000, 0)
	e.SetNow(func() time.Time { return fixed })

	structural := []string{
		ProgressKindSessionStarted,
		ProgressKindVerdict,
		ProgressKindTurnComplete,
		ProgressKindError,
	}
	for _, k := range structural {
		e.Emit(ProgressEvent{Kind: k})
		e.Emit(ProgressEvent{Kind: k}) // same instant; must still pass
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(structural)*2 {
		t.Fatalf("want %d lines, got %d: %q", len(structural)*2, len(lines), buf.String())
	}
}

func TestNDJSONProgressEmitter_NilWriterIsNoop(t *testing.T) {
	// A nil writer comes up when --json is not set but some caller still
	// hands the reviewer a non-nil emitter — the emitter must not crash
	// and must produce no output.
	e := NewNDJSONProgressEmitter(nil, 10*time.Second)
	e.Emit(ProgressEvent{Kind: ProgressKindSessionStarted})
	// No assertion beyond "did not panic"; the nil check guards the write.
}

func TestNoopProgressEmitter_Silent(t *testing.T) {
	p := NoopProgressEmitter()
	p.Emit(ProgressEvent{Kind: ProgressKindSessionStarted})
	// Nothing to assert — a missing side effect is the contract.
}

func TestNDJSONProgressEmitter_EventFieldAlwaysSet(t *testing.T) {
	// Consumers key on event=="progress" to separate progress lines from
	// the final envelope. The emitter is responsible for stamping this
	// regardless of what the caller passed in, so a mistaken caller can't
	// poison the stream by passing an empty or spoofed Event.
	var buf bytes.Buffer
	e := NewNDJSONProgressEmitter(&buf, 0)
	e.Emit(ProgressEvent{Event: "bogus", Kind: ProgressKindSessionStarted})

	var got ProgressEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Event != "progress" {
		t.Errorf("event = %q, want progress", got.Event)
	}
}
