package jiradozer

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
	require.Equal(t, "sess-1", got.SessionID)
	require.Len(t, provider.resumeSession, 3)

	var claudeTransient *claude.TransientError
	require.True(t, errors.As(err, &claudeTransient), "final error should preserve typed transient cause")
}
