package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
)

// Integration tests for session lifecycle and persistence.
// These tests verify the interaction between Manager and Store.

func TestSessionPersistenceRoundtrip(t *testing.T) {
	// Create a store
	store, err := session.NewStore(t.TempDir())
	require.NoError(t, err)

	// Create a manager with the store
	manager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName: "test-repo",
		Store:    store,
	})
	defer manager.Close()

	// Create a session manually (simulating session creation)
	sess := &session.Session{
		ID:           "test-session-persist",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusCompleted,
		WorktreePath: "/tmp/test-worktree",
		WorktreeName: "feature-branch",
		Prompt:       "Test persistence roundtrip",
		CreatedAt:    time.Now(),
		Progress: &session.SessionProgress{
			TurnCount:    3,
			TotalCostUSD: 0.05,
			InputTokens:  500,
			OutputTokens: 200,
		},
	}

	// Add to manager
	manager.AddSession(sess)

	// Add some output
	manager.AddOutputLine(sess.ID, session.OutputLine{Type: session.OutputTypeStatus, Content: "Started"})
	manager.AddOutputLine(sess.ID, session.OutputLine{Type: session.OutputTypeThinking, Content: "Processing..."})
	manager.AddOutputLine(sess.ID, session.OutputLine{Type: session.OutputTypeStatus, Content: "Completed"})

	// Persist the session
	manager.PersistSession(sess)

	// Load from store directly
	loaded, err := store.LoadSession("test-repo", "feature-branch", "test-session-persist")
	require.NoError(t, err)
	require.NotNil(t, loaded)

	// Verify data
	assert.Equal(t, sess.ID, loaded.ID)
	assert.Equal(t, sess.Type, loaded.Type)
	assert.Equal(t, sess.Status, loaded.Status)
	assert.Equal(t, sess.Prompt, loaded.Prompt)
	assert.Equal(t, "test-repo", loaded.RepoName)
	assert.Len(t, loaded.Output, 3)
	assert.NotNil(t, loaded.Progress)
	assert.Equal(t, 3, loaded.Progress.TurnCount)
}

func TestManagerLoadHistorySessionsIntegration(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	require.NoError(t, err)

	// Save some sessions directly to store
	for i := 0; i < 5; i++ {
		s := &session.StoredSession{
			ID:           session.SessionID("session-" + string(rune('a'+i))),
			Type:         session.SessionTypePlanner,
			Status:       session.StatusCompleted,
			RepoName:     "test-repo",
			WorktreeName: "main",
			Prompt:       "Test session",
			CreatedAt:    time.Now().Add(-time.Duration(i) * time.Hour),
		}
		require.NoError(t, store.SaveSession(s))
	}

	// Create manager
	manager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName: "test-repo",
		Store:    store,
	})
	defer manager.Close()

	// Load history
	sessions, err := manager.LoadHistorySessions("main")
	require.NoError(t, err)
	assert.Len(t, sessions, 5)

	// Should be sorted newest first
	assert.Equal(t, session.SessionID("session-a"), sessions[0].ID)
}

func TestSessionStateTransitions(t *testing.T) {
	manager := session.NewManager()
	defer manager.Close()

	// Test status helper methods
	tests := []struct {
		status     session.SessionStatus
		isTerminal bool
		canAccept  bool
	}{
		{session.StatusPending, false, false},
		{session.StatusRunning, false, false},
		{session.StatusIdle, false, true},
		{session.StatusCompleted, true, false},
		{session.StatusFailed, true, false},
		{session.StatusStopped, true, false},
	}

	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			assert.Equal(t, tc.isTerminal, tc.status.IsTerminal())
			assert.Equal(t, tc.canAccept, tc.status.CanAcceptInput())
		})
	}
}

func TestSessionOutputBuffer(t *testing.T) {
	manager := session.NewManager()
	defer manager.Close()

	sessionID := session.SessionID("test-output-buffer")

	// Initialize output buffer
	manager.InitOutputBuffer(sessionID)

	// Add lines
	for i := 0; i < 1500; i++ {
		manager.AddOutputLine(sessionID, session.OutputLine{
			Type:    session.OutputTypeText,
			Content: "Line",
		})
	}

	// Should be capped at 1000
	output := manager.GetSessionOutput(sessionID)
	assert.Len(t, output, 1000)
}

func TestSessionDeleteFromManagerAndStore(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	require.NoError(t, err)

	manager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName: "test-repo",
		Store:    store,
	})
	defer manager.Close()

	// Create and persist a session
	sess := &session.Session{
		ID:           "to-delete",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusCompleted,
		WorktreePath: "/tmp/wt",
		WorktreeName: "main",
		CreatedAt:    time.Now(),
		Progress:     &session.SessionProgress{},
	}

	manager.AddSession(sess)
	manager.AddOutputLine(sess.ID, session.OutputLine{Content: "test"})
	manager.PersistSession(sess)

	// Verify it exists in store
	_, err = store.LoadSession("test-repo", "main", "to-delete")
	require.NoError(t, err)

	// Delete from manager
	err = manager.DeleteSession("to-delete")
	require.NoError(t, err)

	// Should be gone from manager
	_, ok := manager.GetSession("to-delete")
	assert.False(t, ok)

	// Should also be gone from store
	_, err = store.LoadSession("test-repo", "main", "to-delete")
	assert.Error(t, err)
}

func TestEventEmission(t *testing.T) {
	manager := session.NewManager()
	defer manager.Close()

	// Create a session
	sess := &session.Session{
		ID:       "event-test",
		Status:   session.StatusPending,
		Progress: &session.SessionProgress{},
	}

	manager.AddSession(sess)

	// Start listening for events in a goroutine
	events := make([]interface{}, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		timeout := time.After(time.Second)
		for {
			select {
			case event := <-manager.Events():
				events = append(events, event)
				if len(events) >= 2 {
					return
				}
			case <-timeout:
				return
			}
		}
	}()

	// Emit status change
	manager.UpdateSessionStatus(sess, session.StatusRunning)
	manager.UpdateSessionStatus(sess, session.StatusCompleted)

	// Wait for events
	<-done

	// Should have received state change events
	assert.GreaterOrEqual(t, len(events), 1)
}

func TestSendFollowUpToIdleSession(t *testing.T) {
	manager := session.NewManager()
	defer manager.Close()

	// Create an idle session with a follow-up channel
	sess := &session.Session{
		ID:       "followup-test",
		Status:   session.StatusIdle,
		Progress: &session.SessionProgress{},
	}

	manager.AddSession(sess)

	followUpChan := make(chan string, 1)
	manager.SetFollowUpChan(sess.ID, followUpChan)

	// Send follow-up
	err := manager.SendFollowUp("followup-test", "Continue with the task")
	require.NoError(t, err)

	// Verify message received
	select {
	case msg := <-followUpChan:
		assert.Equal(t, "Continue with the task", msg)
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for follow-up message")
	}
}

func TestCompleteIdleSession(t *testing.T) {
	manager := session.NewManager()
	defer manager.Close()

	// Create an idle session with a follow-up channel
	sess := &session.Session{
		ID:       "complete-test",
		Status:   session.StatusIdle,
		Progress: &session.SessionProgress{},
	}

	manager.AddSession(sess)

	followUpChan := make(chan string, 1)
	manager.SetFollowUpChan(sess.ID, followUpChan)

	// Complete the session
	err := manager.CompleteSession("complete-test")
	require.NoError(t, err)

	// Channel should be closed
	_, ok := <-followUpChan
	assert.False(t, ok, "Channel should be closed")

	// Follow-up channel should be removed
	assert.False(t, manager.HasFollowUpChan(sess.ID))
}
