package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartSessionTool(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir())

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

	handler := NewDelegatorToolHandler(m, t.TempDir())

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

	handler := NewDelegatorToolHandler(m, t.TempDir())

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

	handler := NewDelegatorToolHandler(m, t.TempDir())

	// Start a session first
	id, err := m.StartSession(SessionTypeBuilder, t.TempDir(), "test", "sonnet")
	require.NoError(t, err)

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
}

func TestGetSessionProgressTool(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir())

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

	handler := NewDelegatorToolHandler(m, t.TempDir())

	_, err := handler.handleGetSessionProgress(context.Background(), getSessionProgressParams{
		SessionID: "nonexistent",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not found")
}

func TestStartSessionTracksChildren(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	handler := NewDelegatorToolHandler(m, t.TempDir())

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

	handler := NewDelegatorToolHandler(m, t.TempDir())

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
