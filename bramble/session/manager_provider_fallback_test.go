package session

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

type silentEphemeralProvider struct{}

func (p *silentEphemeralProvider) Name() string { return "silent-ephemeral" }
func (p *silentEphemeralProvider) Events() <-chan agent.AgentEvent {
	return nil
}
func (p *silentEphemeralProvider) Close() error { return nil }
func (p *silentEphemeralProvider) Execute(_ context.Context, prompt string, _ *wt.WorktreeContext, _ ...agent.ExecuteOption) (*agent.AgentResult, error) {
	return &agent.AgentResult{
		Text:    fmt.Sprintf("response: %s", prompt),
		Success: true,
	}, nil
}

type streamingEphemeralProvider struct{}

func (p *streamingEphemeralProvider) Name() string { return "streaming-ephemeral" }
func (p *streamingEphemeralProvider) Events() <-chan agent.AgentEvent {
	return nil
}
func (p *streamingEphemeralProvider) Close() error { return nil }
func (p *streamingEphemeralProvider) Execute(_ context.Context, _ string, _ *wt.WorktreeContext, opts ...agent.ExecuteOption) (*agent.AgentResult, error) {
	cfg := agent.ExecuteConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.EventHandler != nil {
		cfg.EventHandler.OnText("streamed")
	}
	return &agent.AgentResult{
		Text:    "streamed",
		Success: true,
	}, nil
}

type silentLongRunningProvider struct { //nolint:govet // fieldalignment: test fixture readability
	mu      sync.Mutex
	started bool
	stopped bool
	events  chan agent.AgentEvent
}

func newSilentLongRunningProvider() *silentLongRunningProvider {
	return &silentLongRunningProvider{
		events: make(chan agent.AgentEvent, 1),
	}
}

func (p *silentLongRunningProvider) Name() string { return "silent-long-running" }
func (p *silentLongRunningProvider) Events() <-chan agent.AgentEvent {
	return p.events
}
func (p *silentLongRunningProvider) Close() error {
	close(p.events)
	return nil
}
func (p *silentLongRunningProvider) Execute(_ context.Context, _ string, _ *wt.WorktreeContext, _ ...agent.ExecuteOption) (*agent.AgentResult, error) {
	return &agent.AgentResult{Text: "unused", Success: true}, nil
}
func (p *silentLongRunningProvider) Start(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
	return nil
}
func (p *silentLongRunningProvider) SendMessage(_ context.Context, message string) (*agent.AgentResult, error) {
	return &agent.AgentResult{
		Text:    "long-running response: " + message,
		Success: true,
	}, nil
}
func (p *silentLongRunningProvider) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	return nil
}

func setupProviderRunnerHarness(t *testing.T) (*Manager, SessionID, *sessionEventHandler) {
	t.Helper()

	manager := NewManager()
	t.Cleanup(manager.Close)

	sessionID := SessionID("provider-fallback-test")
	manager.AddSession(&Session{
		ID:       sessionID,
		Status:   StatusRunning,
		Progress: &SessionProgress{},
	})
	manager.InitOutputBuffer(sessionID)

	return manager, sessionID, newSessionEventHandler(manager, sessionID)
}

func TestProviderRunner_RunTurnFallsBackToResultTextForSilentEphemeralProvider(t *testing.T) {
	t.Parallel()

	manager, sessionID, handler := setupProviderRunnerHarness(t)
	runner := &providerRunner{
		provider:     &silentEphemeralProvider{},
		eventHandler: handler,
	}

	usage, err := runner.RunTurn(context.Background(), "hello")
	require.NoError(t, err)
	require.NotNil(t, usage)

	lines := manager.GetSessionOutput(sessionID)
	require.Len(t, lines, 1)
	assert.Equal(t, OutputTypeText, lines[0].Type)
	assert.Equal(t, "response: hello", lines[0].Content)
}

func TestProviderRunner_RunTurnDoesNotDuplicateStreamedText(t *testing.T) {
	t.Parallel()

	manager, sessionID, handler := setupProviderRunnerHarness(t)
	runner := &providerRunner{
		provider:     &streamingEphemeralProvider{},
		eventHandler: handler,
	}

	_, err := runner.RunTurn(context.Background(), "ignored")
	require.NoError(t, err)

	lines := manager.GetSessionOutput(sessionID)
	require.Len(t, lines, 1)
	assert.Equal(t, OutputTypeText, lines[0].Type)
	assert.Equal(t, "streamed", lines[0].Content)
}

func TestProviderRunner_RunTurnFallsBackToResultTextForSilentLongRunningProvider(t *testing.T) {
	t.Parallel()

	provider := newSilentLongRunningProvider()
	manager, sessionID, handler := setupProviderRunnerHarness(t)
	runner := &providerRunner{
		provider:     provider,
		eventHandler: handler,
	}

	require.NoError(t, runner.Start(context.Background()))
	t.Cleanup(func() {
		require.NoError(t, runner.Stop())
	})

	_, err := runner.RunTurn(context.Background(), "follow-up")
	require.NoError(t, err)

	lines := manager.GetSessionOutput(sessionID)
	require.Len(t, lines, 1)
	assert.Equal(t, OutputTypeText, lines[0].Type)
	assert.Equal(t, "long-running response: follow-up", lines[0].Content)
}
