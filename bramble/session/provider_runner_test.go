package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

// mockLongRunningProvider is a mock long-running provider for testing.
type mockLongRunningProvider struct {
	events  chan agent.AgentEvent
	mu      sync.Mutex
	started bool
	stopped bool
}

func newMockLongRunningProvider() *mockLongRunningProvider {
	return &mockLongRunningProvider{
		events: make(chan agent.AgentEvent, 10),
	}
}

func (m *mockLongRunningProvider) Name() string {
	return "mock-provider"
}

func (m *mockLongRunningProvider) Events() <-chan agent.AgentEvent {
	return m.events
}

func (m *mockLongRunningProvider) Close() error {
	close(m.events)
	return nil
}

func (m *mockLongRunningProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...agent.ExecuteOption) (*agent.AgentResult, error) {
	return &agent.AgentResult{
		Text:    "response",
		Success: true,
	}, nil
}

func (m *mockLongRunningProvider) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = true
	return nil
}

func (m *mockLongRunningProvider) SendMessage(ctx context.Context, message string) (*agent.AgentResult, error) {
	return &agent.AgentResult{
		Text:    "response",
		Success: true,
	}, nil
}

func (m *mockLongRunningProvider) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
	return nil
}

func (m *mockLongRunningProvider) emitEvent(ev agent.AgentEvent) {
	m.events <- ev
}

// Test that providerRunner starts the event bridge for long-running providers.
func TestProviderRunner_EventBridge(t *testing.T) {
	mockProvider := newMockLongRunningProvider()

	// Create manager to collect events
	manager := NewManager()
	defer manager.Close()

	sessionID := SessionID("test-session")
	session := &Session{
		ID:       sessionID,
		Status:   StatusRunning,
		Progress: &SessionProgress{},
	}
	manager.AddSession(session)
	manager.InitOutputBuffer(sessionID)

	handler := newSessionEventHandler(manager, sessionID)

	runner := &providerRunner{
		provider:     mockProvider,
		eventHandler: handler,
	}

	// Start the runner
	ctx := context.Background()
	err := runner.Start(ctx)
	require.NoError(t, err)

	// Verify provider was started
	mockProvider.mu.Lock()
	assert.True(t, mockProvider.started)
	mockProvider.mu.Unlock()

	// Emit some events
	mockProvider.emitEvent(agent.TextAgentEvent{Text: "hello"})
	mockProvider.emitEvent(agent.TextAgentEvent{Text: " world"})
	mockProvider.emitEvent(agent.ToolStartAgentEvent{Name: "read_file", ID: "tool-1"})

	// Give bridge time to process events
	time.Sleep(100 * time.Millisecond)

	// Verify events were forwarded to session output
	output := manager.GetSessionOutput(sessionID)
	require.NotEmpty(t, output)

	// Find text output
	var textFound bool
	for _, line := range output {
		if line.Type == OutputTypeText && line.Content == "hello world" {
			textFound = true
			break
		}
	}
	assert.True(t, textFound, "expected to find accumulated text output")

	// Stop the runner
	err = runner.Stop()
	require.NoError(t, err)

	// Verify provider was stopped
	mockProvider.mu.Lock()
	assert.True(t, mockProvider.stopped)
	mockProvider.mu.Unlock()
}

// Test that providerRunner cleans up the event bridge on Stop.
func TestProviderRunner_EventBridgeCleanup(t *testing.T) {
	mockProvider := newMockLongRunningProvider()

	manager := NewManager()
	defer manager.Close()

	sessionID := SessionID("test-session")
	session := &Session{
		ID:       sessionID,
		Status:   StatusRunning,
		Progress: &SessionProgress{},
	}
	manager.AddSession(session)
	manager.InitOutputBuffer(sessionID)

	handler := newSessionEventHandler(manager, sessionID)

	runner := &providerRunner{
		provider:     mockProvider,
		eventHandler: handler,
	}

	ctx := context.Background()
	err := runner.Start(ctx)
	require.NoError(t, err)

	// Verify event bridge is running
	assert.NotNil(t, runner.eventBridgeDone)

	// Stop the runner
	err = runner.Stop()
	require.NoError(t, err)

	// Verify event bridge was cleaned up
	assert.Nil(t, runner.eventBridgeDone)
}

// Test that providerRunner handles provider events channel closing.
func TestProviderRunner_EventChannelClose(t *testing.T) {
	mockProvider := newMockLongRunningProvider()

	manager := NewManager()
	defer manager.Close()

	sessionID := SessionID("test-session")
	session := &Session{
		ID:       sessionID,
		Status:   StatusRunning,
		Progress: &SessionProgress{},
	}
	manager.AddSession(session)
	manager.InitOutputBuffer(sessionID)

	handler := newSessionEventHandler(manager, sessionID)

	runner := &providerRunner{
		provider:     mockProvider,
		eventHandler: handler,
	}

	ctx := context.Background()
	err := runner.Start(ctx)
	require.NoError(t, err)

	// Emit an event
	mockProvider.emitEvent(agent.TextAgentEvent{Text: "before close"})
	time.Sleep(50 * time.Millisecond)

	// Close the provider's event channel
	err = mockProvider.Close()
	require.NoError(t, err)

	// Give bridge time to detect channel close
	time.Sleep(50 * time.Millisecond)

	// Stop should not panic even though events channel is closed
	err = runner.Stop()
	require.NoError(t, err)
}

// Test that providerRunner doesn't start event bridge for non-long-running providers.
func TestProviderRunner_NoEventBridgeForEphemeralProviders(t *testing.T) {
	// Mock ephemeral provider (not long-running)
	mockProvider := &mockEphemeralProvider{
		events: make(chan agent.AgentEvent, 10),
	}

	manager := NewManager()
	defer manager.Close()

	sessionID := SessionID("test-session")
	session := &Session{
		ID:       sessionID,
		Status:   StatusRunning,
		Progress: &SessionProgress{},
	}
	manager.AddSession(session)
	manager.InitOutputBuffer(sessionID)

	handler := newSessionEventHandler(manager, sessionID)

	runner := &providerRunner{
		provider:     mockProvider,
		eventHandler: handler,
	}

	ctx := context.Background()
	err := runner.Start(ctx)
	require.NoError(t, err)

	// Verify event bridge was NOT started for ephemeral provider
	assert.Nil(t, runner.eventBridgeDone)

	err = runner.Stop()
	require.NoError(t, err)
}

// mockEphemeralProvider is a mock ephemeral (non-long-running) provider.
type mockEphemeralProvider struct {
	events chan agent.AgentEvent
}

func (m *mockEphemeralProvider) Name() string {
	return "mock-ephemeral-provider"
}

func (m *mockEphemeralProvider) Events() <-chan agent.AgentEvent {
	return m.events
}

func (m *mockEphemeralProvider) Close() error {
	close(m.events)
	return nil
}

func (m *mockEphemeralProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...agent.ExecuteOption) (*agent.AgentResult, error) {
	return &agent.AgentResult{
		Text:    "response",
		Success: true,
	}, nil
}
