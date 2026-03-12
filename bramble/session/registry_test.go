package session

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/sessionmodel"
)

func newTestManager(t *testing.T, repoName string) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := &Manager{
		ctx:           ctx,
		cancel:        cancel,
		sessions:      make(map[SessionID]*Session),
		events:        make(chan interface{}, 100),
		outputs:       make(map[SessionID][]OutputLine),
		models:        make(map[SessionID]*sessionmodel.SessionModel),
		followUpChans: make(map[SessionID]chan string),
		config:        ManagerConfig{RepoName: repoName},
	}
	return mgr
}

// addFakeSession inserts a minimal session into a manager for testing.
func addFakeSession(mgr *Manager, id SessionID, prompt string) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.sessions[id] = &Session{
		ID:     id,
		Status: StatusRunning,
		Prompt: prompt,
		Type:   SessionTypePlanner,
	}
}

func TestRegistry_SingleManager(t *testing.T) {
	reg := NewSessionRegistry()
	mgr := newTestManager(t, "repo-a")
	reg.Register(mgr)

	addFakeSession(mgr, "sess-1", "do stuff")

	info, owner, ok := reg.GetSessionInfo("sess-1")
	require.True(t, ok)
	assert.Equal(t, SessionID("sess-1"), info.ID)
	assert.Equal(t, mgr, owner)
}

func TestRegistry_MultiManager(t *testing.T) {
	reg := NewSessionRegistry()
	mgr1 := newTestManager(t, "repo-a")
	mgr2 := newTestManager(t, "repo-b")
	reg.Register(mgr1)
	reg.Register(mgr2)

	addFakeSession(mgr1, "sess-a", "task a")
	addFakeSession(mgr2, "sess-b", "task b")

	// Find session in second manager.
	info, owner, ok := reg.GetSessionInfo("sess-b")
	require.True(t, ok)
	assert.Equal(t, SessionID("sess-b"), info.ID)
	assert.Equal(t, mgr2, owner)

	// Find session in first manager.
	info, owner, ok = reg.GetSessionInfo("sess-a")
	require.True(t, ok)
	assert.Equal(t, SessionID("sess-a"), info.ID)
	assert.Equal(t, mgr1, owner)
}

func TestRegistry_SessionNotFound(t *testing.T) {
	reg := NewSessionRegistry()
	mgr := newTestManager(t, "repo-a")
	reg.Register(mgr)

	_, _, ok := reg.GetSessionInfo("nonexistent")
	assert.False(t, ok)
}

func TestRegistry_SetSessionIdle(t *testing.T) {
	reg := NewSessionRegistry()
	mgr1 := newTestManager(t, "repo-a")
	mgr2 := newTestManager(t, "repo-b")
	reg.Register(mgr1)
	reg.Register(mgr2)

	addFakeSession(mgr2, "sess-b", "task b")

	// Session starts as Running.
	info, _, ok := reg.GetSessionInfo("sess-b")
	require.True(t, ok)
	assert.Equal(t, StatusRunning, info.Status)

	// Mark idle via registry — should target mgr2.
	reg.SetSessionIdle("sess-b")

	info, _, ok = reg.GetSessionInfo("sess-b")
	require.True(t, ok)
	assert.Equal(t, StatusIdle, info.Status)
}

func TestRegistry_GetAllSessions(t *testing.T) {
	reg := NewSessionRegistry()
	mgr1 := newTestManager(t, "repo-a")
	mgr2 := newTestManager(t, "repo-b")
	reg.Register(mgr1)
	reg.Register(mgr2)

	addFakeSession(mgr1, "sess-a1", "task 1")
	addFakeSession(mgr1, "sess-a2", "task 2")
	addFakeSession(mgr2, "sess-b1", "task 3")

	all := reg.GetAllSessions()
	assert.Len(t, all, 3)

	ids := make(map[SessionID]bool)
	for _, s := range all {
		ids[s.ID] = true
	}
	assert.True(t, ids["sess-a1"])
	assert.True(t, ids["sess-a2"])
	assert.True(t, ids["sess-b1"])
}

func TestRegistry_ConcurrentRegisterAndLookup(t *testing.T) {
	reg := NewSessionRegistry()

	var wg sync.WaitGroup
	// Concurrently register managers and look up sessions.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr := newTestManager(t, "repo")
			addFakeSession(mgr, SessionID("s"), "task")
			reg.Register(mgr)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Lookup may or may not find anything — just ensure no race.
			reg.GetSessionInfo("s")
			reg.GetAllSessions()
		}()
	}
	wg.Wait()
}
