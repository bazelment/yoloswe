package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
)

func TestDelegatorSessionCreatesChildren(t *testing.T) {
	m := session.NewManagerWithConfig(session.ManagerConfig{
		SessionMode: session.SessionModeTUI,
	})
	defer m.Close()

	// Start a delegator session with a simple task
	id, err := m.StartSession(session.SessionTypeDelegator, t.TempDir(),
		"Create a simple hello world Go program in main.go", "sonnet")
	require.NoError(t, err)

	// Verify the delegator session exists and is running
	require.Eventually(t, func() bool {
		info, ok := m.GetSessionInfo(id)
		return ok && (info.Status == session.StatusRunning || info.Status == session.StatusIdle)
	}, 30*time.Second, 100*time.Millisecond, "delegator session should start running")

	// Verify the delegator session has the right type and model
	info, ok := m.GetSessionInfo(id)
	require.True(t, ok)
	assert.Equal(t, session.SessionTypeDelegator, info.Type)
	assert.Equal(t, "sonnet", info.Model)

	// Wait for the delegator to create at least one child session
	require.Eventually(t, func() bool {
		sessions := m.GetAllSessions()
		childCount := 0
		for _, s := range sessions {
			if s.ID != id && (s.Type == session.SessionTypePlanner || s.Type == session.SessionTypeBuilder) {
				childCount++
			}
		}
		return childCount > 0
	}, 60*time.Second, 500*time.Millisecond, "delegator should create at least one child session")

	// Verify delegator produces output
	output := m.GetSessionOutput(id)
	assert.NotEmpty(t, output, "delegator should produce some output")
}

func TestDelegatorSessionAutoResumesOnChildStateChange(t *testing.T) {
	m := session.NewManagerWithConfig(session.ManagerConfig{
		SessionMode: session.SessionModeTUI,
	})
	defer m.Close()

	// Start a delegator
	id, err := m.StartSession(session.SessionTypeDelegator, t.TempDir(),
		"List files in the current directory using a planner session", "sonnet")
	require.NoError(t, err)

	// Wait for delegator to become idle (after spawning a child)
	require.Eventually(t, func() bool {
		info, ok := m.GetSessionInfo(id)
		if !ok {
			return false
		}
		// The delegator should go through at least one turn
		return info.Progress.TurnCount >= 1
	}, 60*time.Second, 500*time.Millisecond, "delegator should complete at least one turn")

	// Verify the delegator eventually processes child state changes
	// (it will auto-resume when children change state)
	require.Eventually(t, func() bool {
		info, ok := m.GetSessionInfo(id)
		if !ok {
			return false
		}
		// More than 1 turn means the delegator was auto-resumed
		return info.Progress.TurnCount > 1
	}, 120*time.Second, 1*time.Second, "delegator should auto-resume on child state changes")
}
