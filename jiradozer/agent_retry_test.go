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
