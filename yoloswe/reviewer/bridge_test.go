package reviewer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
)

// testEvent types for driving bridgeStreamEvents without a real backend.

type testTextEvent struct{ delta string }

func (e testTextEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindText }
func (e testTextEvent) StreamDelta() string                    { return e.delta }

type testThinkingEvent struct{ delta string }

func (e testThinkingEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindThinking }
func (e testThinkingEvent) StreamDelta() string                    { return e.delta }

type testToolStartEvent struct{ name, callID string }

func (e testToolStartEvent) StreamEventKind() agentstream.EventKind      { return agentstream.KindToolStart }
func (e testToolStartEvent) StreamToolName() string                      { return e.name }
func (e testToolStartEvent) StreamToolCallID() string                    { return e.callID }
func (e testToolStartEvent) StreamToolInput() map[string]interface{}     { return nil }

type testToolEndEvent struct{ name, callID string; isError bool }

func (e testToolEndEvent) StreamEventKind() agentstream.EventKind      { return agentstream.KindToolEnd }
func (e testToolEndEvent) StreamToolName() string                      { return e.name }
func (e testToolEndEvent) StreamToolCallID() string                    { return e.callID }
func (e testToolEndEvent) StreamToolInput() map[string]interface{}     { return nil }
func (e testToolEndEvent) StreamToolResult() interface{}               { return nil }
func (e testToolEndEvent) StreamToolIsError() bool                     { return e.isError }

type testTurnCompleteEvent struct{ success bool; durationMs int64 }

func (e testTurnCompleteEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindTurnComplete }
func (e testTurnCompleteEvent) StreamTurnNum() int                     { return 1 }
func (e testTurnCompleteEvent) StreamIsSuccess() bool                  { return e.success }
func (e testTurnCompleteEvent) StreamDuration() int64                  { return e.durationMs }
func (e testTurnCompleteEvent) StreamCost() float64                    { return 0 }

type testErrorEvent struct{ err error; ctx string }

func (e testErrorEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindError }
func (e testErrorEvent) StreamErr() error                       { return e.err }
func (e testErrorEvent) StreamErrorContext() string             { return e.ctx }

type testScopedTextEvent struct {
	testTextEvent
	scopeID string
}

func (e testScopedTextEvent) ScopeID() string { return e.scopeID }

type testUnknownEvent struct{}

func (e testUnknownEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindUnknown }

// recordingHandler records all EventHandler calls.
type recordingHandler struct {
	texts      []string
	reasonings []string
	toolStarts []string
	toolEnds   []string
	turns      []bool
	errors     []error
}

func (h *recordingHandler) OnSessionInfo(_, _ string)                                                     {}
func (h *recordingHandler) OnText(delta string)                                                           { h.texts = append(h.texts, delta) }
func (h *recordingHandler) OnReasoning(delta string)                                                      { h.reasonings = append(h.reasonings, delta) }
func (h *recordingHandler) OnToolStart(name, callID string, _ map[string]interface{})                     { h.toolStarts = append(h.toolStarts, name) }
func (h *recordingHandler) OnToolComplete(name, callID string, _ map[string]interface{}, _ interface{}, _ bool) { h.toolEnds = append(h.toolEnds, name) }
func (h *recordingHandler) OnTurnComplete(success bool, _ int64)                                          { h.turns = append(h.turns, success) }
func (h *recordingHandler) OnError(err error, _ string)                                                   { h.errors = append(h.errors, err) }

func sendEvents(events ...agentstream.Event) <-chan agentstream.Event {
	ch := make(chan agentstream.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

func TestBridgeStreamEvents_HappyPath(t *testing.T) {
	handler := &recordingHandler{}
	events := sendEvents(
		testTextEvent{delta: "hello "},
		testTextEvent{delta: "world"},
		testTurnCompleteEvent{success: true, durationMs: 100},
	)

	result, err := bridgeStreamEvents(context.Background(), events, handler, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.responseText != "hello world" {
		t.Errorf("responseText = %q, want %q", result.responseText, "hello world")
	}
	if !result.success {
		t.Error("expected success=true")
	}
	if result.durationMs != 100 {
		t.Errorf("durationMs = %d, want 100", result.durationMs)
	}
	if len(handler.texts) != 2 {
		t.Errorf("expected 2 text events, got %d", len(handler.texts))
	}
	if len(handler.turns) != 1 || !handler.turns[0] {
		t.Error("expected one successful turn complete")
	}
}

func TestBridgeStreamEvents_AllEventKinds(t *testing.T) {
	handler := &recordingHandler{}
	events := sendEvents(
		testThinkingEvent{delta: "thinking..."},
		testTextEvent{delta: "answer"},
		testToolStartEvent{name: "read", callID: "c1"},
		testToolEndEvent{name: "read", callID: "c1", isError: false},
		testTurnCompleteEvent{success: true, durationMs: 50},
	)

	result, err := bridgeStreamEvents(context.Background(), events, handler, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.responseText != "answer" {
		t.Errorf("responseText = %q, want %q", result.responseText, "answer")
	}
	if len(handler.reasonings) != 1 || handler.reasonings[0] != "thinking..." {
		t.Errorf("expected reasoning event, got %v", handler.reasonings)
	}
	if len(handler.toolStarts) != 1 || handler.toolStarts[0] != "read" {
		t.Errorf("expected tool start, got %v", handler.toolStarts)
	}
	if len(handler.toolEnds) != 1 || handler.toolEnds[0] != "read" {
		t.Errorf("expected tool end, got %v", handler.toolEnds)
	}
}

func TestBridgeStreamEvents_ErrorEvent(t *testing.T) {
	handler := &recordingHandler{}
	testErr := errors.New("something broke")
	events := sendEvents(
		testTextEvent{delta: "partial"},
		testErrorEvent{err: testErr, ctx: "test"},
	)

	_, err := bridgeStreamEvents(context.Background(), events, handler, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, testErr) {
		t.Errorf("expected wrapped testErr, got: %v", err)
	}
	if len(handler.errors) != 1 {
		t.Errorf("expected 1 error event, got %d", len(handler.errors))
	}
}

func TestBridgeStreamEvents_NilChannel(t *testing.T) {
	_, err := bridgeStreamEvents[agentstream.Event](context.Background(), nil, nil, "")
	if err == nil {
		t.Fatal("expected error for nil channel")
	}
}

func TestBridgeStreamEvents_ClosedWithoutTurnComplete(t *testing.T) {
	events := sendEvents(
		testTextEvent{delta: "partial response"},
	)

	_, err := bridgeStreamEvents(context.Background(), events, nil, "")
	if err == nil {
		t.Fatal("expected error when channel closes without TurnComplete")
	}
}

func TestBridgeStreamEvents_EmptyChannelClosed(t *testing.T) {
	ch := make(chan agentstream.Event)
	close(ch)

	_, err := bridgeStreamEvents(context.Background(), ch, nil, "")
	if err == nil {
		t.Fatal("expected error when empty channel closes")
	}
}

func TestBridgeStreamEvents_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Blocking channel that never sends — ctx.Done() should fire.
	ch := make(chan agentstream.Event)

	_, err := bridgeStreamEvents(ctx, ch, nil, "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestBridgeStreamEvents_ContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	ch := make(chan agentstream.Event)

	_, err := bridgeStreamEvents(ctx, ch, nil, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got: %v", err)
	}
}

func TestBridgeStreamEvents_ScopeFiltering(t *testing.T) {
	handler := &recordingHandler{}

	ch := make(chan agentstream.Event, 3)
	ch <- testScopedTextEvent{testTextEvent: testTextEvent{delta: "match"}, scopeID: "thread-1"}
	ch <- testScopedTextEvent{testTextEvent: testTextEvent{delta: "skip"}, scopeID: "thread-2"}
	ch <- testTurnCompleteEvent{success: true, durationMs: 10}
	close(ch)

	result, err := bridgeStreamEvents(context.Background(), ch, handler, "thread-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.responseText != "match" {
		t.Errorf("responseText = %q, want %q (scope filtering failed)", result.responseText, "match")
	}
	if len(handler.texts) != 1 {
		t.Errorf("expected 1 text event after scope filtering, got %d", len(handler.texts))
	}
}

func TestBridgeStreamEvents_UnknownEventsSkipped(t *testing.T) {
	handler := &recordingHandler{}
	events := sendEvents(
		testUnknownEvent{},
		testTextEvent{delta: "visible"},
		testTurnCompleteEvent{success: true, durationMs: 5},
	)

	result, err := bridgeStreamEvents(context.Background(), events, handler, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.responseText != "visible" {
		t.Errorf("responseText = %q, want %q", result.responseText, "visible")
	}
}

func TestBridgeStreamEvents_TurnCompleteFailed(t *testing.T) {
	handler := &recordingHandler{}
	events := sendEvents(
		testTextEvent{delta: "partial"},
		testTurnCompleteEvent{success: false, durationMs: 42},
	)

	result, err := bridgeStreamEvents(context.Background(), events, handler, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.success {
		t.Error("expected success=false")
	}
	if result.responseText != "partial" {
		t.Errorf("responseText = %q, want %q", result.responseText, "partial")
	}
	if result.durationMs != 42 {
		t.Errorf("durationMs = %d, want 42", result.durationMs)
	}
	if len(handler.turns) != 1 || handler.turns[0] {
		t.Error("expected one failed turn complete")
	}
}

func TestBridgeStreamEvents_NilHandler(t *testing.T) {
	events := sendEvents(
		testTextEvent{delta: "hello"},
		testTurnCompleteEvent{success: true, durationMs: 1},
	)

	result, err := bridgeStreamEvents(context.Background(), events, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Text should still be accumulated even with nil handler.
	if result.responseText != "hello" {
		t.Errorf("responseText = %q, want %q", result.responseText, "hello")
	}
}
