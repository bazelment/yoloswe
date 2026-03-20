package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

func TestStartSessionTool(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir(), "", nil)

	result, err := handler.handleStartSession(context.Background(), startSessionParams{
		Type:   "builder",
		Prompt: "test task",
	})
	require.NoError(t, err)
	assert.Contains(t, result, "Started builder session:")

	// Verify a session was created
	sessions := m.GetAllSessions()
	require.Len(t, sessions, 1)
	assert.Equal(t, SessionTypeBuilder, sessions[0].Type)
	assert.Equal(t, "test task", sessions[0].Prompt)
}

func TestStartSessionToolPlanner(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir(), "", nil)

	result, err := handler.handleStartSession(context.Background(), startSessionParams{
		Type:   "planner",
		Prompt: "analyze code",
		Model:  "opus",
	})
	require.NoError(t, err)
	assert.Contains(t, result, "Started planner session:")

	sessions := m.GetAllSessions()
	require.Len(t, sessions, 1)
	assert.Equal(t, SessionTypePlanner, sessions[0].Type)
	assert.Equal(t, "opus", sessions[0].Model)
}

func TestStartSessionToolInvalidType(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir(), "", nil)

	_, err := handler.handleStartSession(context.Background(), startSessionParams{
		Type:   "invalid",
		Prompt: "test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid session type")
}

func TestStopSessionTool(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir(), "", nil)

	// Start a child session via the handler so ownership is tracked
	startResult, err := handler.handleStartSession(context.Background(), startSessionParams{
		Type:   "builder",
		Prompt: "test",
	})
	require.NoError(t, err)

	childIDs := handler.ChildIDs()
	require.Len(t, childIDs, 1)
	id := childIDs[0]

	// Wait for it to be running or fail (it will fail since there's no CLI)
	require.Eventually(t, func() bool {
		info, ok := m.GetSessionInfo(id)
		return ok && info.Status != StatusPending
	}, 5*time.Second, 50*time.Millisecond)

	result, err := handler.handleStopSession(context.Background(), stopSessionParams{
		SessionID: string(id),
	})
	// May succeed or fail depending on session state, but shouldn't panic
	if err == nil {
		assert.Contains(t, result, "Stopped session:")
	}

	_ = startResult
}

func TestStopSessionToolNotOwned(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir(), "", nil)

	// Start a session directly (not via handler — not owned by this delegator)
	id, err := m.StartSession(SessionTypeBuilder, t.TempDir(), "test", "sonnet")
	require.NoError(t, err)

	_, err = handler.handleStopSession(context.Background(), stopSessionParams{
		SessionID: string(id),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not owned by this delegator")
}

func TestGetSessionProgressTool(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir(), "", nil)

	// Start a session
	id, err := m.StartSession(SessionTypeBuilder, t.TempDir(), "test prompt", "sonnet")
	require.NoError(t, err)

	result, err := handler.handleGetSessionProgress(context.Background(), getSessionProgressParams{
		SessionID: string(id),
	})
	require.NoError(t, err)
	assert.Contains(t, result, "Session:")
	assert.Contains(t, result, "Type: builder")
	assert.Contains(t, result, "Model: sonnet")
}

func TestGetSessionProgressToolNotFound(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir(), "", nil)

	_, err := handler.handleGetSessionProgress(context.Background(), getSessionProgressParams{
		SessionID: "nonexistent",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not found")
}

func TestStartSessionTracksChildren(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir(), "", nil)

	// Start two sessions
	_, err := handler.handleStartSession(context.Background(), startSessionParams{
		Type:   "builder",
		Prompt: "task 1",
	})
	require.NoError(t, err)

	_, err = handler.handleStartSession(context.Background(), startSessionParams{
		Type:   "planner",
		Prompt: "task 2",
	})
	require.NoError(t, err)

	childIDs := handler.ChildIDs()
	assert.Len(t, childIDs, 2)
}

func TestChildNotificationChannel(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir(), "", nil)

	// Start a child session via the handler
	result, err := handler.handleStartSession(context.Background(), startSessionParams{
		Type:   "builder",
		Prompt: "test",
	})
	require.NoError(t, err)

	childIDs := handler.ChildIDs()
	require.Len(t, childIDs, 1)
	childID := childIDs[0]

	// Set up notification channel
	notifyCh := make(chan SessionStateChangeEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	unsub := watchChildSessionChanges(ctx, m, handler, notifyCh)
	defer unsub()

	// Manually trigger a state change on the child session
	session, ok := m.GetSession(childID)
	require.True(t, ok)

	m.updateSessionStatus(session, StatusIdle)

	// Should receive notification
	select {
	case notif := <-notifyCh:
		assert.Equal(t, childID, notif.SessionID)
		assert.Equal(t, StatusIdle, notif.NewStatus)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for child notification")
	}

	_ = result
}

func newTestRegistry(providers ...string) *agent.ModelRegistry {
	statuses := make(map[string]agent.ProviderStatus)
	for _, p := range providers {
		statuses[p] = agent.ProviderStatus{Provider: p, Installed: true, Version: "test"}
	}
	pa := agent.NewProviderAvailabilityFromMap(statuses)
	return agent.NewModelRegistry(pa, nil)
}

func TestStartSessionWithModelValidation(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	registry := newTestRegistry(agent.ProviderClaude, agent.ProviderGemini)

	handler := NewDelegatorToolHandler(m, t.TempDir(), "", registry)

	// Valid model should work.
	result, err := handler.handleStartSession(context.Background(), startSessionParams{
		Type:   "builder",
		Prompt: "test",
		Model:  "sonnet",
	})
	require.NoError(t, err)
	assert.Contains(t, result, "Started builder session:")

	// Valid non-Claude model should work.
	result, err = handler.handleStartSession(context.Background(), startSessionParams{
		Type:   "planner",
		Prompt: "test",
		Model:  "gemini-2.5-pro",
	})
	require.NoError(t, err)
	assert.Contains(t, result, "Started planner session:")

	// Invalid model should fail with a clear error.
	_, err = handler.handleStartSession(context.Background(), startSessionParams{
		Type:   "builder",
		Prompt: "test",
		Model:  "nonexistent-model",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown model")
	assert.Contains(t, err.Error(), "nonexistent-model")
	assert.Contains(t, err.Error(), "opus")
}

func TestAvailableModelsDescription(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	t.Run("nil registry returns empty", func(t *testing.T) {
		handler := NewDelegatorToolHandler(m, t.TempDir(), "", nil)
		assert.Equal(t, "", handler.AvailableModelsDescription())
	})

	t.Run("lists models with provider in parens", func(t *testing.T) {
		registry := newTestRegistry(agent.ProviderClaude, agent.ProviderGemini)
		handler := NewDelegatorToolHandler(m, t.TempDir(), "", registry)
		desc := handler.AvailableModelsDescription()

		assert.Contains(t, desc, "opus (claude)")
		assert.Contains(t, desc, "sonnet (claude)")
		assert.Contains(t, desc, "haiku (claude)")
		assert.Contains(t, desc, "gemini-2.5-pro (gemini)")
	})
}
