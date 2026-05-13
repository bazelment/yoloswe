package agentstream

import (
	"errors"
	"testing"
)

func TestEventInterfaces(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	toolInput := map[string]interface{}{"command": "bazel test //..."}

	events := []Event{
		readyEvent{sessionID: "session-1"},
		textEvent{kind: KindText, delta: "hello"},
		textEvent{kind: KindThinking, delta: "thinking"},
		toolStartEvent{name: "Bash", callID: "tool-1", input: toolInput},
		toolEndEvent{name: "Bash", callID: "tool-1", input: toolInput, result: "ok", isError: false},
		turnCompleteEvent{turnNum: 2, success: true, duration: 1234, cost: 0.25},
		errorEvent{err: errBoom, context: "stream"},
	}

	if events[0].StreamEventKind() != KindReady {
		t.Fatalf("ready kind = %v, want %v", events[0].StreamEventKind(), KindReady)
	}
	if events[1].(Text).StreamDelta() != "hello" {
		t.Fatalf("text delta = %q", events[1].(Text).StreamDelta())
	}
	if events[2].(Text).StreamEventKind() != KindThinking {
		t.Fatalf("thinking kind = %v", events[2].(Text).StreamEventKind())
	}
	if events[3].(ToolStart).StreamToolInput()["command"] != "bazel test //..." {
		t.Fatalf("tool start input = %#v", events[3].(ToolStart).StreamToolInput())
	}
	if events[4].(ToolEnd).StreamToolResult() != "ok" || events[4].(ToolEnd).StreamToolIsError() {
		t.Fatalf("tool end result = %#v isError=%v", events[4].(ToolEnd).StreamToolResult(), events[4].(ToolEnd).StreamToolIsError())
	}
	if events[5].(TurnComplete).StreamTurnNum() != 2 || events[5].(TurnComplete).StreamCost() != 0.25 {
		t.Fatalf("turn complete = turn %d cost %.2f", events[5].(TurnComplete).StreamTurnNum(), events[5].(TurnComplete).StreamCost())
	}
	if !errors.Is(events[6].(Error).StreamErr(), errBoom) || events[6].(Error).StreamErrorContext() != "stream" {
		t.Fatalf("error event = %v context=%q", events[6].(Error).StreamErr(), events[6].(Error).StreamErrorContext())
	}
}

func TestScoped(t *testing.T) {
	t.Parallel()

	ev := scopedEvent{scopeID: "thread-1"}
	if ev.ScopeID() != "thread-1" {
		t.Fatalf("ScopeID() = %q, want thread-1", ev.ScopeID())
	}
}

type readyEvent struct {
	sessionID string
}

func (e readyEvent) StreamEventKind() EventKind { return KindReady }
func (e readyEvent) StreamSessionID() string    { return e.sessionID }

type textEvent struct {
	delta string
	kind  EventKind
}

func (e textEvent) StreamEventKind() EventKind { return e.kind }
func (e textEvent) StreamDelta() string        { return e.delta }

type toolStartEvent struct {
	input  map[string]interface{}
	name   string
	callID string
}

func (e toolStartEvent) StreamEventKind() EventKind              { return KindToolStart }
func (e toolStartEvent) StreamToolName() string                  { return e.name }
func (e toolStartEvent) StreamToolCallID() string                { return e.callID }
func (e toolStartEvent) StreamToolInput() map[string]interface{} { return e.input }

type toolEndEvent struct {
	input   map[string]interface{}
	result  interface{}
	name    string
	callID  string
	isError bool
}

func (e toolEndEvent) StreamEventKind() EventKind              { return KindToolEnd }
func (e toolEndEvent) StreamToolName() string                  { return e.name }
func (e toolEndEvent) StreamToolCallID() string                { return e.callID }
func (e toolEndEvent) StreamToolInput() map[string]interface{} { return e.input }
func (e toolEndEvent) StreamToolResult() interface{}           { return e.result }
func (e toolEndEvent) StreamToolIsError() bool                 { return e.isError }

type turnCompleteEvent struct {
	cost     float64
	duration int64
	turnNum  int
	success  bool
}

func (e turnCompleteEvent) StreamEventKind() EventKind { return KindTurnComplete }
func (e turnCompleteEvent) StreamTurnNum() int         { return e.turnNum }
func (e turnCompleteEvent) StreamIsSuccess() bool      { return e.success }
func (e turnCompleteEvent) StreamDuration() int64      { return e.duration }
func (e turnCompleteEvent) StreamCost() float64        { return e.cost }

type errorEvent struct {
	err     error
	context string
}

func (e errorEvent) StreamEventKind() EventKind { return KindError }
func (e errorEvent) StreamErr() error           { return e.err }
func (e errorEvent) StreamErrorContext() string { return e.context }

type scopedEvent struct {
	scopeID string
}

func (e scopedEvent) ScopeID() string { return e.scopeID }

var (
	_ Ready        = readyEvent{}
	_ Text         = textEvent{}
	_ ToolStart    = toolStartEvent{}
	_ ToolEnd      = toolEndEvent{}
	_ TurnComplete = turnCompleteEvent{}
	_ Error        = errorEvent{}
	_ Scoped       = scopedEvent{}
)
