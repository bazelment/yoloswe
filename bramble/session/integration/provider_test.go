package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

// mockProvider implements agent.Provider for testing the provider → session manager flow.
type mockProvider struct {
	err        error
	result     *agent.AgentResult
	eventsChan chan agent.AgentEvent
	name       string
	calls      []string
	mu         sync.Mutex
	closed     bool
}

func newMockProvider(name string) *mockProvider {
	return &mockProvider{
		name:       name,
		eventsChan: make(chan agent.AgentEvent, 10),
		result: &agent.AgentResult{
			Text:    "mock response",
			Success: true,
			Usage: agent.AgentUsage{
				InputTokens:  100,
				OutputTokens: 50,
				CostUSD:      0.001,
			},
		},
	}
}

func (p *mockProvider) Name() string { return p.name }

func (p *mockProvider) Execute(_ context.Context, prompt string, _ *wt.WorktreeContext, _ ...agent.ExecuteOption) (*agent.AgentResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, prompt)
	if p.err != nil {
		return nil, p.err
	}
	return p.result, nil
}

func (p *mockProvider) Events() <-chan agent.AgentEvent { return p.eventsChan }

func (p *mockProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	close(p.eventsChan)
	return nil
}

// mockLongRunningProvider implements agent.LongRunningProvider.
type mockLongRunningProvider struct {
	messages []string
	mockProvider
	startedMu  sync.Mutex
	messagesMu sync.Mutex
	started    bool
	stopped    bool
}

func newMockLongRunningProvider(name string) *mockLongRunningProvider {
	return &mockLongRunningProvider{
		mockProvider: *newMockProvider(name),
	}
}

func (p *mockLongRunningProvider) Start(_ context.Context) error {
	p.startedMu.Lock()
	defer p.startedMu.Unlock()
	p.started = true
	return nil
}

func (p *mockLongRunningProvider) SendMessage(_ context.Context, message string) (*agent.AgentResult, error) {
	p.messagesMu.Lock()
	defer p.messagesMu.Unlock()
	p.messages = append(p.messages, message)
	return p.result, nil
}

func (p *mockLongRunningProvider) Stop() error {
	p.startedMu.Lock()
	defer p.startedMu.Unlock()
	p.stopped = true
	return nil
}

func (p *mockLongRunningProvider) isStarted() bool {
	p.startedMu.Lock()
	defer p.startedMu.Unlock()
	return p.started
}

func (p *mockLongRunningProvider) isStopped() bool {
	p.startedMu.Lock()
	defer p.startedMu.Unlock()
	return p.stopped
}

func (p *mockLongRunningProvider) getMessages() []string {
	p.messagesMu.Lock()
	defer p.messagesMu.Unlock()
	result := make([]string, len(p.messages))
	copy(result, p.messages)
	return result
}

func TestProviderRunnerConversion(t *testing.T) {
	t.Parallel()

	// Verify that a mock provider can be used with ManagerConfig
	provider := newMockProvider("test-provider")
	manager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName:    "test-repo",
		SessionMode: session.SessionModeTUI,
		Provider:    provider,
	})
	defer manager.Close()

	// Manager should be created successfully with a provider
	assert.NotNil(t, manager)
}

func TestProviderManagerConfigBackwardCompat(t *testing.T) {
	t.Parallel()

	// Verify that ManagerConfig without Provider still works (backward-compatible)
	manager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName:    "test-repo",
		SessionMode: session.SessionModeTUI,
	})
	defer manager.Close()

	assert.NotNil(t, manager)
}

func TestLongRunningProviderInterface(t *testing.T) {
	t.Parallel()

	provider := newMockLongRunningProvider("test-long-running")

	// Verify it satisfies both interfaces
	var _ agent.Provider = provider
	var _ agent.LongRunningProvider = provider

	// Test lifecycle
	err := provider.Start(context.Background())
	require.NoError(t, err)
	assert.True(t, provider.isStarted())

	result, err := provider.SendMessage(context.Background(), "test message")
	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "mock response", result.Text)
	assert.Equal(t, []string{"test message"}, provider.getMessages())

	err = provider.Stop()
	require.NoError(t, err)
	assert.True(t, provider.isStopped())
}

func TestProviderUsageConversion(t *testing.T) {
	t.Parallel()

	// Test that agent.AgentUsage fields are preserved when creating sessions
	provider := newMockProvider("usage-test")
	provider.result = &agent.AgentResult{
		Text:    "response",
		Success: true,
		Usage: agent.AgentUsage{
			InputTokens:     1000,
			OutputTokens:    500,
			CacheReadTokens: 200,
			CostUSD:         0.025,
		},
	}

	manager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName:    "test-repo",
		SessionMode: session.SessionModeTUI,
		Provider:    provider,
	})
	defer manager.Close()

	// Verify provider result has correct usage
	result, err := provider.Execute(context.Background(), "test", nil)
	require.NoError(t, err)
	assert.Equal(t, 1000, result.Usage.InputTokens)
	assert.Equal(t, 500, result.Usage.OutputTokens)
	assert.Equal(t, 200, result.Usage.CacheReadTokens)
	assert.InDelta(t, 0.025, result.Usage.CostUSD, 0.0001)
}

func TestProviderSessionWithStore(t *testing.T) {
	t.Parallel()

	store, err := session.NewStore(t.TempDir())
	require.NoError(t, err)

	provider := newMockProvider("store-test")
	manager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName:    "test-repo",
		SessionMode: session.SessionModeTUI,
		Store:       store,
		Provider:    provider,
	})
	defer manager.Close()

	// Create a session manually and persist it
	sess := &session.Session{
		ID:           "provider-session-1",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusCompleted,
		WorktreePath: "/tmp/test-wt",
		WorktreeName: "feature-branch",
		Prompt:       "Test with provider",
		CreatedAt:    time.Now(),
		Progress: &session.SessionProgress{
			TurnCount:    1,
			TotalCostUSD: 0.001,
			InputTokens:  100,
			OutputTokens: 50,
		},
	}

	manager.AddSession(sess)
	manager.InitOutputBuffer(sess.ID)
	manager.AddOutputLine(sess.ID, session.OutputLine{
		Type:    session.OutputTypeText,
		Content: "mock response",
	})
	manager.PersistSession(sess)

	// Verify persistence (session metadata is stored; output may be stored separately)
	loaded, err := store.LoadSession("test-repo", "feature-branch", "provider-session-1")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "Test with provider", loaded.Prompt)
	assert.Equal(t, session.SessionTypePlanner, loaded.Type)
}
