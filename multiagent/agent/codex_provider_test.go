package agent

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
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

func TestCodexTurnOptions_NoEffortYieldsNoOptions(t *testing.T) {
	t.Parallel()

	opts := codexTurnOptions(ExecuteConfig{})
	assert.Empty(t, opts, "no effort set => no turn options")
}

func TestCodexTurnOptions_WiresAllValidLevels(t *testing.T) {
	t.Parallel()

	for _, level := range []EffortLevel{EffortLow, EffortMedium, EffortHigh, EffortMax, EffortAuto} {
		level := level
		t.Run(string(level), func(t *testing.T) {
			t.Parallel()

			opts := codexTurnOptions(ExecuteConfig{Effort: level})
			require.Len(t, opts, 1, "expected exactly one turn option for effort=%q", level)

			// Apply the option to a default TurnConfig and assert the
			// underlying SDK field carries the requested string verbatim.
			// Mirrors the pattern in agent-cli-wrapper/codex/client_options_test.go:250.
			cfg := codex.TurnConfig{}
			opts[0](&cfg)
			assert.Equal(t, string(level), cfg.Effort)
		})
	}
}

func TestEndpointsEqual_CanonicalizesDefaults(t *testing.T) {
	t.Parallel()
	// Empty Wire and empty ProviderName resolve to "chat" / "custom" via the
	// canonical accessors, so they must compare equal to their explicit-default
	// counterparts. Otherwise the divergence guard in CodexProvider.Execute would
	// spuriously reject a second Execute call that re-uses the same endpoint
	// shape but happens to spell out the defaults.
	a := llmendpoint.Endpoint{BaseURL: "https://x", APIKeyEnv: "K"}
	b := llmendpoint.Endpoint{
		BaseURL:      "https://x",
		APIKeyEnv:    "K",
		ProviderName: llmendpoint.DefaultProviderName,
		Wire:         llmendpoint.WireAPIChat,
	}
	assert.True(t, endpointsEqual(a, b), "implicit defaults must equal explicit defaults")
}

func TestEndpointsEqual_DivergentFieldsAreUnequal(t *testing.T) {
	t.Parallel()
	base := llmendpoint.Endpoint{
		BaseURL:   "https://x",
		APIKeyEnv: "K",
	}
	cases := []struct {
		mut  func(*llmendpoint.Endpoint)
		name string
	}{
		{func(e *llmendpoint.Endpoint) { e.BaseURL = "https://y" }, "different base url"},
		{func(e *llmendpoint.Endpoint) { e.APIKey = "k1" }, "different api key"},
		{func(e *llmendpoint.Endpoint) { e.APIKeyEnv = "K2" }, "different api key env"},
		{func(e *llmendpoint.Endpoint) { e.ProviderName = "baseten" }, "different provider name"},
		{func(e *llmendpoint.Endpoint) { e.Wire = llmendpoint.WireAPIResponses }, "different wire"},
		{func(e *llmendpoint.Endpoint) { e.Headers = map[string]string{"X-A": "1"} }, "extra header"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mutated := base
			tc.mut(&mutated)
			assert.False(t, endpointsEqual(base, mutated), "expected unequal after %s", tc.name)
		})
	}
}

func TestEndpointsEqual_HeaderEqualityIsOrderIndependent(t *testing.T) {
	t.Parallel()
	a := llmendpoint.Endpoint{
		BaseURL:   "https://x",
		APIKeyEnv: "K",
		Headers:   map[string]string{"X-A": "1", "X-B": "2"},
	}
	b := llmendpoint.Endpoint{
		BaseURL:   "https://x",
		APIKeyEnv: "K",
		Headers:   map[string]string{"X-B": "2", "X-A": "1"},
	}
	assert.True(t, endpointsEqual(a, b))
}

func TestCodexProvider_CheckEndpointDivergence(t *testing.T) {
	t.Parallel()
	bound := llmendpoint.Endpoint{
		BaseURL:      "https://inference.baseten.co/v1",
		APIKeyEnv:    "BASETEN_API_KEY",
		ProviderName: "baseten",
	}

	t.Run("no client yet returns nil", func(t *testing.T) {
		t.Parallel()
		p := &CodexProvider{}
		// boundEndpt is zero; with no client, divergence is moot.
		require.NoError(t, p.checkEndpointDivergence(bound))
	})

	t.Run("matching endpoint returns nil", func(t *testing.T) {
		t.Parallel()
		p := &CodexProvider{client: &codex.Client{}, boundEndpt: bound}
		require.NoError(t, p.checkEndpointDivergence(bound))
	})

	t.Run("base url divergence reports both endpoints", func(t *testing.T) {
		t.Parallel()
		p := &CodexProvider{client: &codex.Client{}, boundEndpt: bound}
		other := bound
		other.BaseURL = "https://example.com/v1"
		err := p.checkEndpointDivergence(other)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "https://inference.baseten.co/v1")
		assert.Contains(t, err.Error(), "https://example.com/v1")
	})

	t.Run("header-only divergence still rejected and surfaced", func(t *testing.T) {
		t.Parallel()
		p := &CodexProvider{client: &codex.Client{}, boundEndpt: bound}
		other := bound
		other.Headers = map[string]string{"X-Org": "acme"}
		err := p.checkEndpointDivergence(other)
		require.Error(t, err, "header divergence must reject — wrappers can't reapply headers post-boot")
		// Both endpoints' base URL is identical here; the error must still be
		// a rejection, not silently route header-only changes to the bound endpoint.
		assert.Contains(t, err.Error(), "LLMEndpoint changed")
	})

	t.Run("Headers map aliasing does not fool the divergence check", func(t *testing.T) {
		t.Parallel()
		// Simulate the production binding path: provider stores a Clone of the
		// caller's endpoint. If the caller later mutates the original's Headers
		// map, the divergence check must still see the divergence rather than
		// silently routing to the originally-bound endpoint.
		caller := llmendpoint.Endpoint{
			BaseURL:   "https://x",
			APIKeyEnv: "K",
			Headers:   map[string]string{"X-Org": "v1"},
		}
		p := &CodexProvider{client: &codex.Client{}, boundEndpt: caller.Clone()}
		caller.Headers["X-Org"] = "v2"
		err := p.checkEndpointDivergence(caller)
		require.Error(t, err, "post-bind mutation of caller Headers must not alias bound snapshot")
	})
}

func TestNonNilAgentResult_CoercesNil(t *testing.T) {
	t.Parallel()
	got := nonNilAgentResult(nil)
	require.NotNil(t, got, "nonNilAgentResult must never return nil")
	assert.Equal(t, &AgentResult{}, got)

	in := &AgentResult{Text: "hello", Success: true}
	assert.Same(t, in, nonNilAgentResult(in), "non-nil input should pass through unchanged")
}

// TestProvider_ValidateGate enforces the convention that every Provider.Execute
// call runs ExecuteConfig.validate() at the top, so a malformed endpoint
// produces an error before any subprocess starts. If a future edit drops
// `cfg.validate()` from a provider, this test fails for that provider.
//
// Three partial shapes plus one missing-auth shape are covered. The partial
// shapes (provider-only, wire-only, headers-only) hit hasOnlyDecorations and
// must surface "partially configured". The missing-auth shape covers the
// non-zero / non-decoration validation path so a future regression can't
// narrow validate() to only the partial branch and still pass this table.
func TestProvider_ValidateGate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		ep      llmendpoint.Endpoint
		wantErr string
	}{
		"provider-only":        {llmendpoint.Endpoint{ProviderName: "baseten"}, "partially configured"},
		"wire-only":            {llmendpoint.Endpoint{Wire: llmendpoint.WireAPIResponses}, "partially configured"},
		"headers-only":         {llmendpoint.Endpoint{Headers: map[string]string{"X-Org": "acme"}}, "partially configured"},
		"base-url-without-key": {llmendpoint.Endpoint{BaseURL: "https://example.com/v1"}, "APIKey or APIKeyEnv"},
	}
	providerCtors := map[string]func() Provider{
		"claude": func() Provider { return NewClaudeProvider() },
		"cursor": func() Provider { return NewCursorProvider() },
		"codex":  func() Provider { return NewCodexProvider() },
		"gemini": func() Provider { return NewGeminiProvider() },
	}
	for shape, tc := range cases {
		shape, tc := shape, tc
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			for name, ctor := range providerCtors {
				name, ctor := name, ctor
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					p := ctor()
					defer func() { _ = p.Close() }()
					_, err := p.Execute(t.Context(), "irrelevant", nil,
						WithProviderLLMEndpoint(tc.ep))
					require.Error(t, err, "%s.Execute must reject %s endpoint", name, shape)
					assert.Contains(t, err.Error(), tc.wantErr,
						"%s.Execute (%s) error should contain %q", name, shape, tc.wantErr)
				})
			}
		})
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
