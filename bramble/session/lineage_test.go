package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParentSessionIDSurvivesStoreRoundTrip pins the lineage link across the
// persistence boundary. A subagent's parent is the address its completion
// report goes to, so if the field is dropped by SessionToStored or by the
// reload path, a bramble restart silently orphans every running subagent —
// they finish and nobody is told.
func TestParentSessionIDSurvivesStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	child := &Session{
		ID:              "wt-builder-abc123",
		Type:            SessionTypeBuilder,
		Status:          StatusRunning,
		WorktreePath:    "/tmp/wt",
		WorktreeName:    "wt",
		Prompt:          "do the thing",
		Model:           "gpt-5.4-mini",
		ParentSessionID: "wt-planner-parent1",
		CreatedAt:       time.Now().Truncate(time.Second),
		Progress:        &SessionProgress{},
	}

	stored := SessionToStored(child, "myrepo", nil)
	require.NotNil(t, stored)
	assert.Equal(t, SessionID("wt-planner-parent1"), stored.ParentSessionID)

	require.NoError(t, store.SaveSession(stored))

	reloaded, err := store.LoadSession("myrepo", "wt", child.ID)
	require.NoError(t, err)
	assert.Equal(t, SessionID("wt-planner-parent1"), reloaded.ParentSessionID)

	info := StoredToSessionInfo(reloaded)
	assert.Equal(t, SessionID("wt-planner-parent1"), info.ParentSessionID)
}

// TestTopLevelSessionHasNoParent guards the other direction: an ordinary
// session must not acquire a parent, or it would report to a session that
// never asked for it.
func TestTopLevelSessionHasNoParent(t *testing.T) {
	t.Parallel()

	top := &Session{
		ID:           "wt-planner-top",
		Type:         SessionTypePlanner,
		Status:       StatusRunning,
		WorktreePath: "/tmp/wt",
		WorktreeName: "wt",
		Progress:     &SessionProgress{},
	}

	stored := SessionToStored(top, "myrepo", nil)
	require.NotNil(t, stored)
	assert.Empty(t, stored.ParentSessionID)
	assert.Empty(t, StoredToSessionInfo(stored).ParentSessionID)
	assert.Empty(t, top.ToInfo().ParentSessionID)
}

// TestToInfoCarriesParent covers the live (non-persisted) read path the IPC
// list-sessions handler uses to report lineage.
func TestToInfoCarriesParent(t *testing.T) {
	t.Parallel()

	child := &Session{
		ID:              "wt-codetalk-child",
		Type:            SessionTypeCodeTalk,
		ParentSessionID: "wt-builder-parent",
		Progress:        &SessionProgress{},
	}
	assert.Equal(t, SessionID("wt-builder-parent"), child.ToInfo().ParentSessionID)
}

// TestStartSessionWithOptsRecordsParent checks the spawn path itself: the
// parent must be on the Session before runSession can observe it, since the
// completion watcher reads it to decide where to report.
func TestStartSessionWithOptsRecordsParent(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{RepoName: "myrepo"})
	defer m.Close()

	id, err := m.StartSessionWithOpts(SessionTypeBuilder, t.TempDir(), "prompt", "sonnet",
		SpawnOpts{ParentSessionID: "wt-planner-parent1"})
	require.NoError(t, err)

	info, ok := m.GetSessionInfo(id)
	require.True(t, ok)
	assert.Equal(t, SessionID("wt-planner-parent1"), info.ParentSessionID)
}

func TestStartSessionWithOptsReconcilesEffort(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		backend    string
		model      string
		effort     string
		wantModel  string
		wantEffort string
		wantErr    bool
	}{
		{name: "codex keeps xhigh", backend: ProviderCodex, model: "gpt-5.5", effort: "xhigh", wantModel: "gpt-5.5", wantEffort: "xhigh"},
		{name: "claude clamps xhigh", backend: ProviderClaude, model: "sonnet", effort: "xhigh", wantModel: "sonnet", wantEffort: "max"},
		{name: "agy retargets pinned model", backend: ProviderAgy, model: "gemini-3.1-pro-low", effort: "xhigh", wantModel: "gemini-3.1-pro-high"},
		{name: "auto is omitted", backend: ProviderCodex, model: "gpt-5.5", effort: "auto", wantModel: "gpt-5.5"},
		{name: "cursor rejects effort", backend: ProviderCursor, model: "cursor-fast", effort: "high", wantErr: true},
		{name: "invalid effort rejected", backend: ProviderCodex, model: "gpt-5.5", effort: "turbo", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewManagerWithConfig(ManagerConfig{RepoName: "myrepo", Provider: &silentEphemeralProvider{}, SessionMode: SessionModeTUI})
			t.Cleanup(m.Close)

			id, err := m.StartSessionWithOpts(SessionTypeBuilder, t.TempDir(), "prompt", tc.model,
				SpawnOpts{Backend: tc.backend, Effort: tc.effort})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			info, ok := m.GetSessionInfo(id)
			require.True(t, ok)
			assert.Equal(t, tc.wantModel, info.Model)
			assert.Equal(t, tc.wantEffort, info.Effort)
		})
	}
}

// TestStartSessionLeavesParentEmpty pins that the plain entry point stays a
// top-level spawn — every existing caller goes through it.
func TestStartSessionLeavesParentEmpty(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{RepoName: "myrepo"})
	defer m.Close()

	id, err := m.StartSession(SessionTypeBuilder, t.TempDir(), "prompt", "sonnet")
	require.NoError(t, err)

	info, ok := m.GetSessionInfo(id)
	require.True(t, ok)
	assert.Empty(t, info.ParentSessionID)
}

// TestFastIdleIsNotClobberedByStartup pins a race that leaves a tmux session
// stuck at "running" for good.
//
// runner.Start() lingers ~100ms after creating the window, and an agent inside
// can finish a turn and fire its notify hook in that gap. runSession used to
// write StatusRunning unconditionally once Start returned, overwriting that
// idle — and because SetSessionIdle only advances a *Running* session, the next
// notify never comes: the agent is sitting at a prompt waiting for input. The
// session then reports "running" forever, so a polling parent never sees it
// finish.
func TestFastIdleIsNotClobberedByStartup(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer m.Close()

	s := &Session{
		ID:       "wt-codetalk-fast",
		Type:     SessionTypeCodeTalk,
		Status:   StatusRunning,
		Progress: &SessionProgress{},
	}
	m.AddSession(s)

	// The agent's notify hook lands while startup is still finishing.
	m.SetSessionIdle(s.ID)
	require.Equal(t, StatusIdle, s.ToInfo().Status)

	// Startup completes and re-asserts "running". It must not undo the idle.
	m.tryUpdateSessionStatus(s, StatusPending, StatusRunning)

	assert.Equal(t, StatusIdle, s.ToInfo().Status,
		"startup overwrote an idle the agent had already reported")
}

// TestStartupStillPromotesAPendingSession keeps the guard from being too
// strict: a session that has not been marked running yet still must be.
func TestStartupStillPromotesAPendingSession(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer m.Close()

	s := &Session{ID: "wt-codetalk-pending", Status: StatusPending, Progress: &SessionProgress{}}
	m.AddSession(s)

	m.tryUpdateSessionStatus(s, StatusPending, StatusRunning)
	assert.Equal(t, StatusRunning, s.ToInfo().Status)
}
