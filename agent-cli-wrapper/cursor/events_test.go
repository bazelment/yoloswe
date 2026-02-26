package cursor

import (
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
)

// Compile-time interface compliance checks.
var (
	_ agentstream.Text         = TextEvent{}
	_ agentstream.ToolStart    = ToolStartEvent{}
	_ agentstream.ToolEnd      = ToolCompleteEvent{}
	_ agentstream.TurnComplete = TurnCompleteEvent{}
	_ agentstream.Error        = ErrorEvent{}
)

func TestTextEvent_StreamMethods(t *testing.T) {
	e := TextEvent{Text: "hello", FullText: "hello world"}
	if e.StreamEventKind() != agentstream.KindText {
		t.Errorf("expected KindText, got %v", e.StreamEventKind())
	}
	if e.StreamDelta() != "hello" {
		t.Errorf("expected delta 'hello', got %q", e.StreamDelta())
	}
}

func TestToolStartEvent_StreamMethods(t *testing.T) {
	e := ToolStartEvent{ID: "call-1", Name: "Read", Input: map[string]interface{}{"path": "/tmp"}}
	if e.StreamEventKind() != agentstream.KindToolStart {
		t.Errorf("expected KindToolStart, got %v", e.StreamEventKind())
	}
	if e.StreamToolName() != "Read" {
		t.Errorf("expected 'Read', got %q", e.StreamToolName())
	}
	if e.StreamToolCallID() != "call-1" {
		t.Errorf("expected 'call-1', got %q", e.StreamToolCallID())
	}
}

func TestToolCompleteEvent_StreamMethods(t *testing.T) {
	e := ToolCompleteEvent{
		ID:      "call-1",
		Name:    "Read",
		Input:   map[string]interface{}{"path": "/tmp"},
		Result:  "file contents",
		IsError: false,
	}
	if e.StreamEventKind() != agentstream.KindToolEnd {
		t.Errorf("expected KindToolEnd, got %v", e.StreamEventKind())
	}
	if e.StreamToolResult() != "file contents" {
		t.Errorf("expected 'file contents', got %v", e.StreamToolResult())
	}
	if e.StreamToolIsError() {
		t.Error("expected IsError=false")
	}
}

func TestTurnCompleteEvent_StreamMethods(t *testing.T) {
	e := TurnCompleteEvent{Success: true, DurationMs: 1234}
	if e.StreamEventKind() != agentstream.KindTurnComplete {
		t.Errorf("expected KindTurnComplete, got %v", e.StreamEventKind())
	}
	if e.StreamTurnNum() != 1 {
		t.Errorf("expected turn 1, got %d", e.StreamTurnNum())
	}
	if !e.StreamIsSuccess() {
		t.Error("expected success=true")
	}
	if e.StreamDuration() != 1234 {
		t.Errorf("expected 1234ms, got %d", e.StreamDuration())
	}
	if e.StreamCost() != 0 {
		t.Errorf("expected cost 0, got %f", e.StreamCost())
	}
}

func TestErrorEvent_StreamMethods(t *testing.T) {
	e := ErrorEvent{Error: ErrSessionClosed, Context: "test"}
	if e.StreamEventKind() != agentstream.KindError {
		t.Errorf("expected KindError, got %v", e.StreamEventKind())
	}
	if e.StreamErr() != ErrSessionClosed {
		t.Errorf("expected ErrSessionClosed, got %v", e.StreamErr())
	}
	if e.StreamErrorContext() != "test" {
		t.Errorf("expected 'test', got %q", e.StreamErrorContext())
	}
}
