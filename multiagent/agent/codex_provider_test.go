package agent

import (
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
)

type recordingHandler struct { //nolint:govet // fieldalignment: test fixture readability
	mu            sync.Mutex
	textCalls     []string
	thinkingCalls []string
	toolStarts    []toolStartRecord
	toolCompletes []toolCompleteRecord
	turnCompletes []turnCompleteRecord
	errorCalls    []string
}

type toolStartRecord struct { //nolint:govet // fieldalignment: test fixture readability
	name  string
	id    string
	input map[string]interface{}
}

type toolCompleteRecord struct { //nolint:govet // fieldalignment: test fixture readability
	name    string
	id      string
	input   map[string]interface{}
	result  interface{}
	isError bool
}

type turnCompleteRecord struct {
	turnNumber int
	success    bool
	durationMs int64
	costUSD    float64
}

func (h *recordingHandler) OnText(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.textCalls = append(h.textCalls, text)
}

func (h *recordingHandler) OnThinking(thinking string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.thinkingCalls = append(h.thinkingCalls, thinking)
}

func (h *recordingHandler) OnToolStart(name, id string, input map[string]interface{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.toolStarts = append(h.toolStarts, toolStartRecord{name: name, id: id, input: input})
}

func (h *recordingHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.toolCompletes = append(h.toolCompletes, toolCompleteRecord{
		name:    name,
		id:      id,
		input:   input,
		result:  result,
		isError: isError,
	})
}

func (h *recordingHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.turnCompletes = append(h.turnCompletes, turnCompleteRecord{
		turnNumber: turnNumber,
		success:    success,
		durationMs: durationMs,
		costUSD:    costUSD,
	})
}

func (h *recordingHandler) OnError(err error, context string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errorCalls = append(h.errorCalls, context)
}

func TestBridgeCodexEvents_MapsEventsToHandlerAndChannel(t *testing.T) {
	t.Parallel()

	events := make(chan codex.Event, 10)
	agentEvents := make(chan AgentEvent, 10)
	stop := make(chan struct{})
	turnDone := make(chan struct{})
	handler := &recordingHandler{}

	turnDoneOnce := sync.Once{}
	bridgeDone := make(chan struct{})
	go func() {
		bridgeEvents(events, handler, agentEvents, stop, "thread-1",
			func() { turnDoneOnce.Do(func() { close(turnDone) }) })
		close(bridgeDone)
	}()

	events <- codex.TextDeltaEvent{ThreadID: "thread-1", Delta: "hello "}
	events <- codex.ReasoningDeltaEvent{ThreadID: "thread-1", Delta: "thinking"}
	events <- codex.CommandStartEvent{
		ThreadID:  "thread-1",
		CallID:    "call-1",
		ParsedCmd: "echo hello",
		CWD:       "/tmp/work",
	}
	events <- codex.CommandEndEvent{
		ThreadID:   "thread-1",
		CallID:     "call-1",
		Stdout:     "hello\n",
		ExitCode:   0,
		DurationMs: 42,
	}
	events <- codex.TurnCompletedEvent{
		ThreadID:   "thread-1",
		TurnID:     "2",
		Success:    true,
		DurationMs: 123,
	}

	select {
	case <-turnDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for turnDone")
	}

	close(stop)
	<-bridgeDone

	handler.mu.Lock()
	defer handler.mu.Unlock()

	if len(handler.textCalls) == 0 || handler.textCalls[0] != "hello " {
		t.Fatalf("unexpected text calls: %#v", handler.textCalls)
	}
	if len(handler.thinkingCalls) == 0 || handler.thinkingCalls[0] != "thinking" {
		t.Fatalf("unexpected thinking calls: %#v", handler.thinkingCalls)
	}
	if len(handler.toolStarts) != 1 || handler.toolStarts[0].name != "Bash" {
		t.Fatalf("unexpected tool starts: %#v", handler.toolStarts)
	}
	if got := handler.toolStarts[0].input["command"]; got != "echo hello" {
		t.Fatalf("unexpected tool command input: %#v", handler.toolStarts[0].input)
	}
	if len(handler.toolCompletes) != 1 || handler.toolCompletes[0].isError {
		t.Fatalf("unexpected tool completes: %#v", handler.toolCompletes)
	}
	if len(handler.turnCompletes) != 1 {
		t.Fatalf("unexpected turn completes: %#v", handler.turnCompletes)
	}
	if handler.turnCompletes[0].turnNumber != 3 {
		t.Fatalf("unexpected turn number: %#v", handler.turnCompletes[0])
	}

	var sawText bool
	for {
		select {
		case ev := <-agentEvents:
			if textEv, ok := ev.(TextAgentEvent); ok && textEv.Text == "hello " {
				sawText = true
			}
		default:
			if !sawText {
				t.Fatal("expected TextAgentEvent in output channel")
			}
			return
		}
	}
}

func TestBridgeCodexEvents_FiltersOtherThreads(t *testing.T) {
	t.Parallel()

	events := make(chan codex.Event, 4)
	agentEvents := make(chan AgentEvent, 4)
	stop := make(chan struct{})
	turnDone := make(chan struct{})
	handler := &recordingHandler{}

	turnDoneOnce := sync.Once{}
	bridgeDone := make(chan struct{})
	go func() {
		bridgeEvents(events, handler, agentEvents, stop, "thread-target",
			func() { turnDoneOnce.Do(func() { close(turnDone) }) })
		close(bridgeDone)
	}()

	events <- codex.TextDeltaEvent{ThreadID: "other-thread", Delta: "ignore me"}
	events <- codex.TurnCompletedEvent{ThreadID: "thread-target", TurnID: "0", Success: true}

	select {
	case <-turnDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for turnDone")
	}

	close(stop)
	<-bridgeDone

	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.textCalls) != 0 {
		t.Fatalf("expected no text calls for other thread, got %#v", handler.textCalls)
	}
}

func TestCodexResultToAgentResult_MapsCachedInputTokens(t *testing.T) {
	t.Parallel()

	result := codexResultToAgentResult(&codex.TurnResult{
		FullText: "ok",
		Success:  true,
		Usage: codex.TurnUsage{
			InputTokens:       120,
			CachedInputTokens: 33,
			OutputTokens:      45,
		},
	})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Usage.InputTokens != 120 {
		t.Fatalf("InputTokens = %d, want 120", result.Usage.InputTokens)
	}
	if result.Usage.CacheReadTokens != 33 {
		t.Fatalf("CacheReadTokens = %d, want 33", result.Usage.CacheReadTokens)
	}
	if result.Usage.OutputTokens != 45 {
		t.Fatalf("OutputTokens = %d, want 45", result.Usage.OutputTokens)
	}
}

func TestCodexApprovalPolicyForPermissionMode(t *testing.T) {
	t.Parallel()

	policy, ok := codexApprovalPolicyForPermissionMode("bypass")
	if !ok || policy != codex.ApprovalPolicyNever {
		t.Fatalf("bypass mapping = (%q,%v), want (%q,true)", policy, ok, codex.ApprovalPolicyNever)
	}

	policy, ok = codexApprovalPolicyForPermissionMode("plan")
	if !ok || policy != codex.ApprovalPolicyOnRequest {
		t.Fatalf("plan mapping = (%q,%v), want (%q,true)", policy, ok, codex.ApprovalPolicyOnRequest)
	}

	_, ok = codexApprovalPolicyForPermissionMode("")
	if ok {
		t.Fatal("empty permission mode should not map to explicit approval policy")
	}
}
