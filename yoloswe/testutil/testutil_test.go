package testutil

import (
	"context"
	"errors"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

func TestCollectTurnEvents(t *testing.T) {
	t.Parallel()

	events := make(chan claude.Event, 7)
	events <- claude.ReadyEvent{Info: claude.SessionInfo{SessionID: "session-1"}}
	events <- claude.TextEvent{Text: "hello"}
	events <- claude.ToolStartEvent{ID: "tool-1", Name: "Read"}
	events <- claude.ToolCompleteEvent{ID: "tool-1", Name: "Read", Input: map[string]interface{}{"file_path": "main.go"}}
	events <- claude.CLIToolResultEvent{ToolUseID: "tool-1", ToolName: "Read", Content: "ok"}
	events <- claude.ErrorEvent{Context: "recoverable", Error: errors.New("temporary")}
	events <- claude.TurnCompleteEvent{Success: true, TurnNumber: 1}

	got, err := CollectTurnEvents(context.Background(), events)
	if err != nil {
		t.Fatalf("CollectTurnEvents() error = %v", err)
	}
	if got.Ready == nil || got.Ready.Info.SessionID != "session-1" {
		t.Fatalf("Ready = %+v", got.Ready)
	}
	if len(got.TextEvents) != 1 || got.TextEvents[0].Text != "hello" {
		t.Fatalf("TextEvents = %+v", got.TextEvents)
	}
	if !got.HasToolNamed("Read") || got.HasToolNamed("Write") {
		t.Fatalf("HasToolNamed results incorrect for tools %+v", got.ToolStarts)
	}
	if len(got.ToolComplete) != 1 || got.ToolComplete[0].Input["file_path"] != "main.go" {
		t.Fatalf("ToolComplete = %+v", got.ToolComplete)
	}
	if len(got.ToolResults) != 1 || got.ToolResults[0].ToolName != "Read" {
		t.Fatalf("ToolResults = %+v", got.ToolResults)
	}
	if len(got.Errors) != 1 || got.Errors[0].Context != "recoverable" {
		t.Fatalf("Errors = %+v", got.Errors)
	}
	if got.TurnComplete == nil || !got.TurnComplete.Success {
		t.Fatalf("TurnComplete = %+v", got.TurnComplete)
	}
}

func TestCollectTurnEventsClosedChannel(t *testing.T) {
	t.Parallel()

	events := make(chan claude.Event)
	close(events)

	got, err := CollectTurnEvents(context.Background(), events)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CollectTurnEvents() error = %v, want context.Canceled", err)
	}
	if got == nil {
		t.Fatal("CollectTurnEvents() result = nil")
	}
}

func TestCollectTurnEventsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := CollectTurnEvents(ctx, make(chan claude.Event))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CollectTurnEvents() error = %v, want context.Canceled", err)
	}
	if got == nil {
		t.Fatal("CollectTurnEvents() result = nil")
	}
}
