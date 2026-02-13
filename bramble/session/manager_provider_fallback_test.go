package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

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

type emptyStreamEphemeralProvider struct{}

func (p *emptyStreamEphemeralProvider) Name() string { return "empty-stream-ephemeral" }
func (p *emptyStreamEphemeralProvider) Events() <-chan agent.AgentEvent {
	return nil
}
func (p *emptyStreamEphemeralProvider) Close() error { return nil }
func (p *emptyStreamEphemeralProvider) Execute(_ context.Context, _ string, _ *wt.WorktreeContext, opts ...agent.ExecuteOption) (*agent.AgentResult, error) {
	cfg := agent.ExecuteConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.EventHandler != nil {
		cfg.EventHandler.OnText("")
	}
	return &agent.AgentResult{
		Text:    "fallback text",
		Success: true,
	}, nil
}

type delayedEventEphemeralProvider struct { //nolint:govet // fieldalignment: test fixture readability
	mu            sync.Mutex
	calls         int
	firstHandler  agent.EventHandler
	secondStarted chan struct{}
	releaseSecond chan struct{}
}

func newDelayedEventEphemeralProvider() *delayedEventEphemeralProvider {
	return &delayedEventEphemeralProvider{
		secondStarted: make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
}

func (p *delayedEventEphemeralProvider) Name() string { return "delayed-event-ephemeral" }
func (p *delayedEventEphemeralProvider) Events() <-chan agent.AgentEvent {
	return nil
}
func (p *delayedEventEphemeralProvider) Close() error { return nil }
func (p *delayedEventEphemeralProvider) Execute(_ context.Context, _ string, _ *wt.WorktreeContext, opts ...agent.ExecuteOption) (*agent.AgentResult, error) {
	cfg := agent.ExecuteConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	p.mu.Lock()
	p.calls++
	call := p.calls
	if call == 1 {
		p.firstHandler = cfg.EventHandler
		p.mu.Unlock()
		return &agent.AgentResult{
			Text:    "first",
			Success: true,
		}, nil
	}
	if call == 2 {
		close(p.secondStarted)
		p.mu.Unlock()
		<-p.releaseSecond
		return &agent.AgentResult{
			Text:    "second",
			Success: true,
		}, nil
	}
	p.mu.Unlock()
	return &agent.AgentResult{
		Text:    "extra",
		Success: true,
	}, nil
}

func (p *delayedEventEphemeralProvider) emitLateFirstTurnText(text string) {
	p.mu.Lock()
	handler := p.firstHandler
	p.mu.Unlock()
	if handler != nil {
		handler.OnText(text)
	}
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

func TestProviderRunner_RunTurnIgnoresEmptyStreamEventsForFallback(t *testing.T) {
	t.Parallel()

	manager, sessionID, handler := setupProviderRunnerHarness(t)
	runner := &providerRunner{
		provider:     &emptyStreamEphemeralProvider{},
		eventHandler: handler,
	}

	_, err := runner.RunTurn(context.Background(), "ignored")
	require.NoError(t, err)

	lines := manager.GetSessionOutput(sessionID)
	require.Len(t, lines, 1)
	assert.Equal(t, OutputTypeText, lines[0].Type)
	assert.Equal(t, "fallback text", lines[0].Content)
}

func TestProviderRunner_RunTurnIgnoresStalePriorTurnEvents(t *testing.T) {
	t.Parallel()

	manager, sessionID, handler := setupProviderRunnerHarness(t)
	provider := newDelayedEventEphemeralProvider()
	runner := &providerRunner{
		provider:     provider,
		eventHandler: handler,
	}

	_, err := runner.RunTurn(context.Background(), "turn1")
	require.NoError(t, err)

	secondDone := make(chan error, 1)
	go func() {
		_, runErr := runner.RunTurn(context.Background(), "turn2")
		secondDone <- runErr
	}()

	select {
	case <-provider.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second turn start")
	}

	provider.emitLateFirstTurnText(" stale")
	close(provider.releaseSecond)

	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second turn completion")
	}

	lines := manager.GetSessionOutput(sessionID)
	require.Len(t, lines, 1)
	assert.Equal(t, OutputTypeText, lines[0].Type)
	assert.Equal(t, "firstsecond", lines[0].Content)
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
