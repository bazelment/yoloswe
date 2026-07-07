package jiradozer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	agentpkg "github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

type fakeRetryProvider struct {
	results       []*agentpkg.AgentResult
	errs          []error
	resumeSession []string
}

func (p *fakeRetryProvider) Name() string { return "fake" }

func (p *fakeRetryProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...agentpkg.ExecuteOption) (*agentpkg.AgentResult, error) {
	var cfg agentpkg.ExecuteConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	p.resumeSession = append(p.resumeSession, cfg.ResumeSessionID)

	idx := len(p.resumeSession) - 1
	var result *agentpkg.AgentResult
	if idx < len(p.results) {
		result = p.results[idx]
	}
	var err error
	if idx < len(p.errs) {
		err = p.errs[idx]
	}
	return result, err
}

func (p *fakeRetryProvider) Events() <-chan agentpkg.AgentEvent { return nil }
func (p *fakeRetryProvider) Close() error                       { return nil }

func TestRunAgentRetryTransientThenSuccess(t *testing.T) {
	provider := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{
			{SessionID: "sess-1"},
			{Success: true, SessionID: "sess-1", Text: "done"},
		},
		errs: []error{
			&codex.TransientError{Message: "stream idle"},
			nil,
		},
	}
	runner := agentRunner{
		newProviderForModel: func(agentpkg.AgentModel) (agentpkg.Provider, error) { return provider, nil },
		retryBackoffs:       []time.Duration{0},
	}

	got, err := runner.runAgent(context.Background(), "build", "prompt", StepConfig{
		Model:            "gpt-5.5",
		TransientRetries: 2,
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, "done", got.Output)
	require.Equal(t, "sess-1", got.SessionID)
	require.Len(t, provider.resumeSession, 2)
	require.Empty(t, provider.resumeSession[0])
	require.Equal(t, "sess-1", provider.resumeSession[1])
}

func TestRunAgentResolvesCursorPrefixModel(t *testing.T) {
	// A prefix-only model (not curated in AllModels) must resolve to its
	// provider and flow into newProviderForModel, not be rejected as unknown.
	provider := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{{Success: true, SessionID: "sess-1", Text: "done"}},
		errs:    []error{nil},
	}
	var gotModel agentpkg.AgentModel
	runner := agentRunner{
		newProviderForModel: func(m agentpkg.AgentModel) (agentpkg.Provider, error) {
			gotModel = m
			return provider, nil
		},
		retryBackoffs: []time.Duration{0},
	}

	got, err := runner.runAgent(context.Background(), "build", "prompt", StepConfig{
		Model: "composer-2.5",
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, "done", got.Output)
	require.Equal(t, "composer-2.5", gotModel.ID)
	require.Equal(t, agentpkg.ProviderCursor, gotModel.Provider)
}

func TestRunAgentRetryTransientResultErrorThenSuccess(t *testing.T) {
	provider := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{
			{Success: false, SessionID: "sess-1", Error: &codex.TransientError{Message: "stream idle", Reason: "stream_idle"}},
			{Success: true, SessionID: "sess-1", Text: "done"},
		},
		errs: []error{nil, nil},
	}
	runner := agentRunner{
		newProviderForModel: func(agentpkg.AgentModel) (agentpkg.Provider, error) { return provider, nil },
		retryBackoffs:       []time.Duration{0},
	}

	got, err := runner.runAgent(context.Background(), "build", "prompt", StepConfig{
		Model:            "gpt-5.5",
		TransientRetries: 2,
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, "done", got.Output)
	require.Equal(t, "sess-1", got.SessionID)
	require.Len(t, provider.resumeSession, 2)
	require.Empty(t, provider.resumeSession[0])
	require.Equal(t, "sess-1", provider.resumeSession[1])
}

func TestRunAgentRetryTransientExhaustsBudget(t *testing.T) {
	transient := &claude.TransientError{Message: "stream idle"}
	provider := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{
			{SessionID: "sess-1"},
			{SessionID: "sess-1"},
			{SessionID: "sess-1"},
		},
		errs: []error{transient, transient, transient},
	}
	runner := agentRunner{
		newProviderForModel: func(agentpkg.AgentModel) (agentpkg.Provider, error) { return provider, nil },
		retryBackoffs:       []time.Duration{0},
	}

	got, err := runner.runAgent(context.Background(), "build", "prompt", StepConfig{
		Model:            "gpt-5.5",
		TransientRetries: 2,
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "agent execution:")
	require.Equal(t, "sess-1", got.SessionID)
	require.Len(t, provider.resumeSession, 3)

	var claudeTransient *claude.TransientError
	require.True(t, errors.As(err, &claudeTransient), "final error should preserve typed transient cause")
}

// TestRunAgentNilResultReturnsError pins the post-loop nil guard: a provider
// that returns (nil, nil) must surface a wrapped error rather than panic on a
// nil result.Success access.
func TestRunAgentNilResultReturnsError(t *testing.T) {
	provider := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{nil},
		errs:    []error{nil},
	}
	runner := agentRunner{
		newProviderForModel: func(agentpkg.AgentModel) (agentpkg.Provider, error) { return provider, nil },
		retryBackoffs:       []time.Duration{0},
	}

	got, err := runner.runAgent(context.Background(), "build", "prompt", StepConfig{
		Model:            "gpt-5.5",
		TransientRetries: 2,
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "agent execution:")
	require.Contains(t, err.Error(), "no result")
	require.Empty(t, got.SessionID)
	require.Len(t, provider.resumeSession, 1, "should not retry a nil result")
}

// TestRunAgentResetsPlanStateAcrossRetries pins the per-attempt reset of the
// log handler's plan-file detection state. A failed attempt that detects a
// plan file must not leak that path into a retry that doesn't write one.
func TestRunAgentResetsPlanStateAcrossRetries(t *testing.T) {
	tempDir := t.TempDir()
	stalePlanPath := tempDir + "/stale-plan.md"
	require.NoError(t, writeFile(stalePlanPath, "stale plan body"))

	provider := &planEmittingFakeProvider{
		fakeRetryProvider: fakeRetryProvider{
			results: []*agentpkg.AgentResult{
				{SessionID: "sess-1", Success: false, Error: &codex.TransientError{Message: "stream idle", Reason: "stream_idle"}},
				{SessionID: "sess-1", Success: true, Text: "fresh agent text"},
			},
			errs: []error{nil, nil},
		},
		planPath: stalePlanPath,
	}
	runner := agentRunner{
		newProviderForModel: func(agentpkg.AgentModel) (agentpkg.Provider, error) { return provider, nil },
		retryBackoffs:       []time.Duration{0},
	}

	got, err := runner.runAgent(context.Background(), "build", "prompt", StepConfig{
		Model:            "gpt-5.5",
		TransientRetries: 2,
	}, tempDir, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, "fresh agent text", got.Output, "output must reflect successful retry, not stale plan file")
}

// planEmittingFakeProvider drives the EventHandler so the first attempt
// looks like a plan-mode agent (Write+ExitPlanMode), and the second attempt
// emits no plan events. Used by TestRunAgentResetsPlanStateAcrossRetries.
type planEmittingFakeProvider struct {
	planPath string
	fakeRetryProvider
}

func (p *planEmittingFakeProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...agentpkg.ExecuteOption) (*agentpkg.AgentResult, error) {
	var cfg agentpkg.ExecuteConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	attempt := len(p.resumeSession)
	if attempt == 0 && cfg.EventHandler != nil {
		cfg.EventHandler.OnToolComplete("Write", "tc-1", map[string]interface{}{"file_path": p.planPath}, nil, false)
		cfg.EventHandler.OnToolComplete("ExitPlanMode", "tc-2", nil, nil, false)
	}
	return p.fakeRetryProvider.Execute(ctx, prompt, wtCtx, opts...)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// providersByModel returns a newProviderForModel that hands out a distinct
// fake provider per resolved model ID, recording the order models were tried.
func providersByModel(t *testing.T, byModel map[string]*fakeRetryProvider) (func(agentpkg.AgentModel) (agentpkg.Provider, error), *[]string) {
	t.Helper()
	var order []string
	return func(m agentpkg.AgentModel) (agentpkg.Provider, error) {
		p, ok := byModel[m.ID]
		if !ok {
			t.Fatalf("no fake provider registered for model %q", m.ID)
		}
		order = append(order, m.ID)
		return p, nil
	}, &order
}

func TestRunAgent_OutOfCredits_FallsBackToNextModel(t *testing.T) {
	primary := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{
			{Success: false, SessionID: "sess-primary", Error: &codex.TurnError{Message: "Your workspace is out of credits. Ask your workspace owner to refill."}},
		},
		errs: []error{nil},
	}
	fallback := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{
			{Success: true, SessionID: "sess-fallback", Text: "done on fallback"},
		},
		errs: []error{nil},
	}
	newProvider, order := providersByModel(t, map[string]*fakeRetryProvider{
		"gpt-5.5": primary,
		"opus":    fallback,
	})
	runner := agentRunner{newProviderForModel: newProvider, retryBackoffs: []time.Duration{0}}

	got, err := runner.runAgent(context.Background(), "ship", "prompt", StepConfig{
		Model:            "gpt-5.5",
		FallbackModels:   []string{"opus"},
		TransientRetries: 2,
	}, t.TempDir(), "resume-orig", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, "done on fallback", got.Output)
	require.Equal(t, "sess-fallback", got.SessionID)
	require.Equal(t, []string{"gpt-5.5", "opus"}, *order, "both models tried in order")

	// The primary saw the original resume session; the fallback must start
	// FRESH (empty resume) since cross-provider resume is unreliable.
	require.Len(t, primary.resumeSession, 1)
	require.Equal(t, "resume-orig", primary.resumeSession[0])
	require.Len(t, fallback.resumeSession, 1)
	require.Empty(t, fallback.resumeSession[0], "fallback must start a fresh session")
}

// TestRunAgent_OutOfCredits_ExecuteErrorPath_FallsBack covers the second
// out-of-credits detection site: when the provider surfaces the failure as
// Execute's returned error (not a result.Error). A regression removing the
// err-path IsOutOfCredits branch in runAgentForModel would otherwise go
// uncaught — the existing fallback tests only drive the result.Error path.
func TestRunAgent_OutOfCredits_ExecuteErrorPath_FallsBack(t *testing.T) {
	primary := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{nil},
		errs:    []error{&codex.TurnError{Message: "Your workspace is out of credits. Ask your workspace owner to refill."}},
	}
	fallback := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{
			{Success: true, SessionID: "sess-fallback", Text: "done on fallback"},
		},
		errs: []error{nil},
	}
	newProvider, order := providersByModel(t, map[string]*fakeRetryProvider{
		"gpt-5.5": primary,
		"opus":    fallback,
	})
	runner := agentRunner{newProviderForModel: newProvider, retryBackoffs: []time.Duration{0}}

	got, err := runner.runAgent(context.Background(), "ship", "prompt", StepConfig{
		Model:            "gpt-5.5",
		FallbackModels:   []string{"opus"},
		TransientRetries: 2,
	}, t.TempDir(), "resume-orig", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, "done on fallback", got.Output)
	require.Equal(t, []string{"gpt-5.5", "opus"}, *order, "both models tried in order")
	require.Empty(t, fallback.resumeSession[0], "fallback must start a fresh session")
}

func TestRunAgent_OutOfCredits_NoFallbackConfigured_Fails(t *testing.T) {
	primary := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{
			{Success: false, SessionID: "sess-primary", Error: &codex.TurnError{Message: "out of credits"}},
		},
		errs: []error{nil},
	}
	newProvider, order := providersByModel(t, map[string]*fakeRetryProvider{"gpt-5.5": primary})
	runner := agentRunner{newProviderForModel: newProvider, retryBackoffs: []time.Duration{0}}

	got, err := runner.runAgent(context.Background(), "ship", "prompt", StepConfig{
		Model:            "gpt-5.5",
		TransientRetries: 2,
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "out of credits")
	require.Equal(t, []string{"gpt-5.5"}, *order, "no fallback configured — only primary tried")
	require.Equal(t, "sess-primary", got.SessionID)
}

func TestRunAgent_OutOfCredits_AllModelsExhausted(t *testing.T) {
	ooc := func(sess string) *fakeRetryProvider {
		return &fakeRetryProvider{
			results: []*agentpkg.AgentResult{
				{Success: false, SessionID: sess, Error: &claude.TurnError{Message: "please ask your Workspace Owner to refill"}},
			},
			errs: []error{nil},
		}
	}
	newProvider, order := providersByModel(t, map[string]*fakeRetryProvider{
		"gpt-5.5": ooc("sess-a"),
		"opus":    ooc("sess-b"),
		"sonnet":  ooc("sess-c"),
	})
	runner := agentRunner{newProviderForModel: newProvider, retryBackoffs: []time.Duration{0}}

	_, err := runner.runAgent(context.Background(), "ship", "prompt", StepConfig{
		Model:            "gpt-5.5",
		FallbackModels:   []string{"opus", "sonnet"},
		TransientRetries: 2,
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "refill")
	require.Equal(t, []string{"gpt-5.5", "opus", "sonnet"}, *order, "all models tried in order before failing")
}

// TestRunAgent_PreflightSkip_ClaudeNearLimit_SkipsToFallback pins Part B: when
// the Claude primary is already at/over the plan limit, the pre-flight skips it
// WITHOUT invoking its provider Execute and runs the Cursor fallback instead.
func TestRunAgent_PreflightSkip_ClaudeNearLimit_SkipsToFallback(t *testing.T) {
	claudePrimary := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{{Success: true, SessionID: "sess-claude", Text: "should not run"}},
		errs:    []error{nil},
	}
	cursorFallback := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{{Success: true, SessionID: "sess-cursor", Text: "done on cursor"}},
		errs:    []error{nil},
	}
	newProvider, order := providersByModel(t, map[string]*fakeRetryProvider{
		"sonnet":       claudePrimary,
		"composer-2.5": cursorFallback,
	})
	runner := agentRunner{
		newProviderForModel: newProvider,
		retryBackoffs:       []time.Duration{0},
		// Claude primary reports 99% utilization (>= threshold); the Cursor
		// fallback is non-Claude so utilization returns ok=false and it runs.
		claudeUtilization: func(_ context.Context, m agentpkg.AgentModel) (float64, bool) {
			if m.Provider == agentpkg.ProviderClaude {
				return 99.0, true
			}
			return 0, false
		},
	}

	got, err := runner.runAgent(context.Background(), "ship", "prompt", StepConfig{
		Model:            "sonnet",
		FallbackModels:   []string{"composer-2.5"},
		TransientRetries: 2,
	}, t.TempDir(), "resume-orig", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, "done on cursor", got.Output)
	require.Equal(t, "sess-cursor", got.SessionID)
	// Only the fallback provider was ever constructed; the exhausted Claude
	// primary was skipped before its provider ran.
	require.Equal(t, []string{"composer-2.5"}, *order, "claude primary skipped, only fallback provider built")
	require.Empty(t, claudePrimary.resumeSession, "claude primary Execute must never be called")
	require.Empty(t, cursorFallback.resumeSession[0], "fallback must start a fresh session")
}

// TestRunAgent_PreflightSkip_LastModelNeverSkipped ensures the pre-flight never
// skips the final model — even at 99% there is nothing to fall back to, so the
// run must still be attempted rather than failing with no attempt at all.
func TestRunAgent_PreflightSkip_LastModelNeverSkipped(t *testing.T) {
	claudeOnly := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{{Success: true, SessionID: "sess-claude", Text: "ran anyway"}},
		errs:    []error{nil},
	}
	newProvider, order := providersByModel(t, map[string]*fakeRetryProvider{"sonnet": claudeOnly})
	runner := agentRunner{
		newProviderForModel: newProvider,
		retryBackoffs:       []time.Duration{0},
		claudeUtilization: func(_ context.Context, _ agentpkg.AgentModel) (float64, bool) {
			return 99.0, true
		},
	}

	got, err := runner.runAgent(context.Background(), "ship", "prompt", StepConfig{
		Model:            "sonnet",
		TransientRetries: 2,
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, "ran anyway", got.Output)
	require.Equal(t, []string{"sonnet"}, *order, "sole model must still run despite being over limit")
}

// TestRunAgent_PreflightFailsOpen_UnavailableUsage runs the Claude primary
// normally when utilization is unavailable (ok=false) — a best-effort
// pre-flight must never block a run.
func TestRunAgent_PreflightFailsOpen_UnavailableUsage(t *testing.T) {
	claudePrimary := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{{Success: true, SessionID: "sess-claude", Text: "ran"}},
		errs:    []error{nil},
	}
	newProvider, order := providersByModel(t, map[string]*fakeRetryProvider{"sonnet": claudePrimary})
	runner := agentRunner{
		newProviderForModel: newProvider,
		retryBackoffs:       []time.Duration{0},
		claudeUtilization: func(_ context.Context, _ agentpkg.AgentModel) (float64, bool) {
			return 0, false // usage unavailable
		},
	}

	got, err := runner.runAgent(context.Background(), "ship", "prompt", StepConfig{
		Model:            "sonnet",
		FallbackModels:   []string{"composer-2.5"},
		TransientRetries: 2,
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, "ran", got.Output)
	require.Equal(t, []string{"sonnet"}, *order, "primary must run when usage is unavailable")
}

// TestRunAgent_PreflightSkip_CustomEndpoint_NotSkipped pins the endpoint-scope
// invariant: a Claude model ID redirected at a third-party llm_endpoint does not
// spend Claude.ai plan credits, so the pre-flight must NOT skip it on the default
// account's utilization — it runs the model even at 99%.
func TestRunAgent_PreflightSkip_CustomEndpoint_NotSkipped(t *testing.T) {
	claudePrimary := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{{Success: true, SessionID: "sess-claude", Text: "ran on endpoint"}},
		errs:    []error{nil},
	}
	newProvider, order := providersByModel(t, map[string]*fakeRetryProvider{"sonnet": claudePrimary})
	utilCalls := 0
	runner := agentRunner{
		newProviderForModel: newProvider,
		retryBackoffs:       []time.Duration{0},
		claudeUtilization: func(_ context.Context, _ agentpkg.AgentModel) (float64, bool) {
			utilCalls++
			return 99.0, true
		},
	}

	got, err := runner.runAgent(context.Background(), "ship", "prompt", StepConfig{
		Model:            "sonnet",
		FallbackModels:   []string{"composer-2.5"},
		TransientRetries: 2,
		LLMEndpoint:      &LLMEndpointConfig{BaseURL: "https://inference.baseten.co/v1", APIKeyEnv: "BASETEN_API_KEY"},
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, "ran on endpoint", got.Output)
	require.Equal(t, []string{"sonnet"}, *order, "custom-endpoint model must run, not skip on default-account usage")
	require.Zero(t, utilCalls, "usage must not even be fetched for a redirected endpoint")
}

// TestRunAgent_ConnectionClosed_NowRetried pins #272: a "connection closed
// mid-response" error is now transient and retried rather than terminal.
func TestRunAgent_ConnectionClosed_NowRetried(t *testing.T) {
	// Verbatim provider error string — must stay capitalized to exercise the
	// case-insensitive classifier match.
	connClosed := errors.New("API Error: Connection closed mid-response. The response above may be incomplete.") //nolint:revive // verbatim provider error
	provider := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{
			{SessionID: "sess-1"},
			{Success: true, SessionID: "sess-1", Text: "recovered"},
		},
		errs: []error{connClosed, nil},
	}
	runner := agentRunner{
		newProviderForModel: func(agentpkg.AgentModel) (agentpkg.Provider, error) { return provider, nil },
		retryBackoffs:       []time.Duration{0},
	}

	got, err := runner.runAgent(context.Background(), "build", "prompt", StepConfig{
		Model:            "gpt-5.5",
		TransientRetries: 2,
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, "recovered", got.Output)
	require.Len(t, provider.resumeSession, 2, "connection-closed error must be retried")
}

// TestRunAgent_DefaultMaxRetriesIsFour pins #272/#273: with no TransientRetries
// override, the default budget is 4 — a run that fails transiently 3 times then
// succeeds must NOT exhaust the budget.
func TestRunAgent_DefaultMaxRetriesIsFour(t *testing.T) {
	transient := &claude.TransientError{Message: "stream idle"}
	provider := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{
			{SessionID: "sess-1"},
			{SessionID: "sess-1"},
			{SessionID: "sess-1"},
			{Success: true, SessionID: "sess-1", Text: "done"},
		},
		errs: []error{transient, transient, transient, nil},
	}
	runner := agentRunner{
		newProviderForModel: func(agentpkg.AgentModel) (agentpkg.Provider, error) { return provider, nil },
		retryBackoffs:       []time.Duration{0},
	}

	got, err := runner.runAgent(context.Background(), "build", "prompt", StepConfig{
		Model: "gpt-5.5",
		// TransientRetries left 0 → default 4.
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err, "3 transient failures must fit within the default budget of 4")
	require.Equal(t, "done", got.Output)
	require.Len(t, provider.resumeSession, 4)
}

func TestRunAgentRetryTransientResultErrorExhaustsBudget(t *testing.T) {
	transient := &codex.TransientError{Message: "stream idle", Reason: "stream_idle"}
	provider := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{
			{Success: false, SessionID: "sess-1", Error: transient},
			{Success: false, SessionID: "sess-1", Error: transient},
			{Success: false, SessionID: "sess-1", Error: transient},
		},
		errs: []error{nil, nil, nil},
	}
	runner := agentRunner{
		newProviderForModel: func(agentpkg.AgentModel) (agentpkg.Provider, error) { return provider, nil },
		retryBackoffs:       []time.Duration{0},
	}

	got, err := runner.runAgent(context.Background(), "build", "prompt", StepConfig{
		Model:            "gpt-5.5",
		TransientRetries: 2,
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "agent execution:")
	require.Equal(t, "sess-1", got.SessionID)
	require.Len(t, provider.resumeSession, 3)

	var codexTransient *codex.TransientError
	require.True(t, errors.As(err, &codexTransient), "final error should preserve typed transient cause")
}

// Backstop for the INF-1871 silent-failure shape: a Success=false result with a
// nil error must not surface the old bare "agent failed". The wrapper now
// attaches a real error (claude.ErrBackgroundTaskFailed) for the known cause, so
// reaching this sink means an unclassified unsuccessful result — jiradozer must
// still emit a descriptive, diagnosable message.
func TestRunAgent_UnsuccessfulResultNoError_DescriptiveMessage(t *testing.T) {
	provider := &fakeRetryProvider{
		results: []*agentpkg.AgentResult{{SessionID: "sess-1"}}, // Success:false, Error:nil
		errs:    []error{nil},
	}
	runner := agentRunner{
		newProviderForModel: func(agentpkg.AgentModel) (agentpkg.Provider, error) { return provider, nil },
		retryBackoffs:       []time.Duration{0},
	}

	_, err := runner.runAgent(context.Background(), "plan", "prompt", StepConfig{
		Model: "gpt-5.5",
	}, t.TempDir(), "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.Error(t, err)
	require.NotEqual(t, "agent failed", err.Error(),
		"the bare opaque 'agent failed' message must not be surfaced")
	require.Contains(t, err.Error(), "unsuccessful result with no error",
		"the sink must emit a descriptive, diagnosable message")
}
