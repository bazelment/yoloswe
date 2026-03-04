package session

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewManager(t *testing.T) {
	m := NewManager()
	require.NotNil(t, m)
	assert.NotNil(t, m.sessions)
	assert.NotNil(t, m.events)
	assert.NotNil(t, m.outputs)
}

func TestNewManagerWithConfig(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	m := NewManagerWithConfig(ManagerConfig{
		RepoName: "test-repo",
		Store:    store,
	})
	require.NotNil(t, m)
	assert.Equal(t, "test-repo", m.config.RepoName)
	assert.NotNil(t, m.config.Store)
}

func TestSessionStatusIsTerminal(t *testing.T) {
	tests := []struct {
		status   SessionStatus
		terminal bool
	}{
		{StatusPending, false},
		{StatusRunning, false},
		{StatusIdle, false},
		{StatusCompleted, true},
		{StatusFailed, true},
		{StatusStopped, true},
	}

	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			assert.Equal(t, tc.terminal, tc.status.IsTerminal())
		})
	}
}

func TestSessionStatusCanAcceptInput(t *testing.T) {
	tests := []struct {
		status    SessionStatus
		canAccept bool
	}{
		{StatusPending, false},
		{StatusRunning, false},
		{StatusIdle, true},
		{StatusCompleted, false},
		{StatusFailed, false},
		{StatusStopped, false},
	}

	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			assert.Equal(t, tc.canAccept, tc.status.CanAcceptInput())
		})
	}
}

func TestManagerGetSession(t *testing.T) {
	m := NewManager()
	defer m.Close()

	// Create a session directly for testing
	session := &Session{
		ID:           "test-session",
		Type:         SessionTypePlanner,
		Status:       StatusPending,
		WorktreePath: "/tmp/test",
		WorktreeName: "test",
		Prompt:       "test prompt",
		CreatedAt:    time.Now(),
		Progress:     &SessionProgress{},
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	// Get existing session
	got, ok := m.GetSession("test-session")
	assert.True(t, ok)
	assert.Equal(t, session.ID, got.ID)

	// Get non-existing session
	_, ok = m.GetSession("nonexistent")
	assert.False(t, ok)
}

func TestManagerGetSessionInfo(t *testing.T) {
	m := NewManager()
	defer m.Close()

	now := time.Now()
	session := &Session{
		ID:           "test-session",
		Type:         SessionTypeBuilder,
		Status:       StatusRunning,
		WorktreePath: "/tmp/test",
		WorktreeName: "feature",
		Prompt:       "build something",
		CreatedAt:    now,
		StartedAt:    &now,
		Progress: &SessionProgress{
			TurnCount:    2,
			TotalCostUSD: 0.05,
		},
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	info, ok := m.GetSessionInfo("test-session")
	assert.True(t, ok)
	assert.Equal(t, SessionID("test-session"), info.ID)
	assert.Equal(t, SessionTypeBuilder, info.Type)
	assert.Equal(t, StatusRunning, info.Status)
	assert.Equal(t, "feature", info.WorktreeName)
	assert.Equal(t, "build something", info.Prompt)
	assert.Equal(t, 2, info.Progress.TurnCount)
}

func TestManagerGetSessionsForWorktree(t *testing.T) {
	m := NewManager()
	defer m.Close()

	sessions := []*Session{
		{ID: "s1", WorktreePath: "/wt1", WorktreeName: "wt1", Progress: &SessionProgress{}},
		{ID: "s2", WorktreePath: "/wt1", WorktreeName: "wt1", Progress: &SessionProgress{}},
		{ID: "s3", WorktreePath: "/wt2", WorktreeName: "wt2", Progress: &SessionProgress{}},
	}

	m.mu.Lock()
	for _, s := range sessions {
		m.sessions[s.ID] = s
	}
	m.mu.Unlock()

	wt1Sessions := m.GetSessionsForWorktree("/wt1")
	assert.Len(t, wt1Sessions, 2)

	wt2Sessions := m.GetSessionsForWorktree("/wt2")
	assert.Len(t, wt2Sessions, 1)

	wt3Sessions := m.GetSessionsForWorktree("/wt3")
	assert.Len(t, wt3Sessions, 0)
}

func TestManagerTrackTmuxWindow(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTmux})
	defer m.Close()

	sessionID, err := m.TrackTmuxWindow("/worktrees/repo/main", "scratch", "@1")
	require.NoError(t, err)
	require.NotEmpty(t, sessionID)

	info, ok := m.GetSessionInfo(sessionID)
	require.True(t, ok)
	assert.Equal(t, SessionTypeBuilder, info.Type)
	assert.Equal(t, StatusRunning, info.Status)
	assert.Equal(t, "/worktrees/repo/main", info.WorktreePath)
	assert.Equal(t, "main", info.WorktreeName)
	assert.Equal(t, "scratch", info.TmuxWindowName)
	assert.Equal(t, "@1", info.TmuxWindowID)

	worktreeSessions := m.GetSessionsForWorktree("/worktrees/repo/main")
	require.Len(t, worktreeSessions, 1)
	assert.Equal(t, sessionID, worktreeSessions[0].ID)
}

func TestManagerTrackTmuxWindow_NotTmuxMode(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	defer m.Close()

	_, err := m.TrackTmuxWindow("/worktrees/repo/main", "scratch", "@1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tmux mode")
}

func TestManagerGetAllSessions(t *testing.T) {
	m := NewManager()
	defer m.Close()

	sessions := []*Session{
		{ID: "s1", Progress: &SessionProgress{}},
		{ID: "s2", Progress: &SessionProgress{}},
		{ID: "s3", Progress: &SessionProgress{}},
	}

	m.mu.Lock()
	for _, s := range sessions {
		m.sessions[s.ID] = s
	}
	m.mu.Unlock()

	all := m.GetAllSessions()
	assert.Len(t, all, 3)
}

func TestManagerCountByStatus(t *testing.T) {
	m := NewManager()
	defer m.Close()

	sessions := []*Session{
		{ID: "s1", Status: StatusRunning, Progress: &SessionProgress{}},
		{ID: "s2", Status: StatusRunning, Progress: &SessionProgress{}},
		{ID: "s3", Status: StatusIdle, Progress: &SessionProgress{}},
		{ID: "s4", Status: StatusCompleted, Progress: &SessionProgress{}},
		{ID: "s5", Status: StatusFailed, Progress: &SessionProgress{}},
	}

	m.mu.Lock()
	for _, s := range sessions {
		m.sessions[s.ID] = s
	}
	m.mu.Unlock()

	counts := m.CountByStatus()
	assert.Equal(t, 2, counts[StatusRunning])
	assert.Equal(t, 1, counts[StatusIdle])
	assert.Equal(t, 1, counts[StatusCompleted])
	assert.Equal(t, 1, counts[StatusFailed])
	assert.Equal(t, 0, counts[StatusPending])
}

func TestManagerGetSessionOutput(t *testing.T) {
	m := NewManager()
	defer m.Close()

	sessionID := SessionID("test-session")

	m.outputsMu.Lock()
	m.outputs[sessionID] = []OutputLine{
		{Type: OutputTypeStatus, Content: "Line 1"},
		{Type: OutputTypeText, Content: "Line 2"},
		{Type: OutputTypeError, Content: "Line 3"},
	}
	m.outputsMu.Unlock()

	output := m.GetSessionOutput(sessionID)
	require.Len(t, output, 3)
	assert.Equal(t, "Line 1", output[0].Content)
	assert.Equal(t, OutputTypeText, output[1].Type)

	// Non-existing session
	output = m.GetSessionOutput("nonexistent")
	assert.Nil(t, output)
}

func TestManagerAddOutput(t *testing.T) {
	m := NewManager()
	defer m.Close()

	sessionID := SessionID("test-session")

	m.outputsMu.Lock()
	m.outputs[sessionID] = make([]OutputLine, 0)
	m.outputsMu.Unlock()

	// Add output
	m.addOutput(sessionID, OutputLine{
		Type:    OutputTypeText,
		Content: "Hello",
	})

	output := m.GetSessionOutput(sessionID)
	require.Len(t, output, 1)
	assert.Equal(t, "Hello", output[0].Content)
}

func TestManagerAddOutputLimit(t *testing.T) {
	m := NewManager()
	defer m.Close()

	sessionID := SessionID("test-session")

	m.outputsMu.Lock()
	m.outputs[sessionID] = make([]OutputLine, 0)
	m.outputsMu.Unlock()

	// Add more than 1000 lines
	for i := 0; i < 1005; i++ {
		m.addOutput(sessionID, OutputLine{
			Type:    OutputTypeText,
			Content: "Line",
		})
	}

	output := m.GetSessionOutput(sessionID)
	assert.Len(t, output, 1000, "Should keep only last 1000 lines")
}

func TestManagerSendFollowUpNotFound(t *testing.T) {
	m := NewManager()
	defer m.Close()

	err := m.SendFollowUp("nonexistent", "message")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestManagerSendFollowUpNotIdle(t *testing.T) {
	m := NewManager()
	defer m.Close()

	session := &Session{
		ID:       "test-session",
		Status:   StatusRunning,
		Progress: &SessionProgress{},
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	err := m.SendFollowUp("test-session", "message")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not idle")
}

func TestManagerCompleteSessionNotFound(t *testing.T) {
	m := NewManager()
	defer m.Close()

	err := m.CompleteSession("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestManagerCompleteSessionNotIdle(t *testing.T) {
	m := NewManager()
	defer m.Close()

	session := &Session{
		ID:       "test-session",
		Status:   StatusRunning,
		Progress: &SessionProgress{},
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	err := m.CompleteSession("test-session")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not idle")
}

func TestManagerDeleteSession(t *testing.T) {
	m := NewManager()
	defer m.Close()

	session := &Session{
		ID:           "test-session",
		Status:       StatusCompleted,
		WorktreeName: "wt",
		Progress:     &SessionProgress{},
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	m.outputsMu.Lock()
	m.outputs[session.ID] = []OutputLine{{Content: "test"}}
	m.outputsMu.Unlock()

	err := m.DeleteSession("test-session")
	require.NoError(t, err)

	_, ok := m.GetSession("test-session")
	assert.False(t, ok)

	output := m.GetSessionOutput("test-session")
	assert.Nil(t, output)
}

func TestManagerDeleteSessionNotFound(t *testing.T) {
	m := NewManager()
	defer m.Close()

	err := m.DeleteSession("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestManagerDeleteSessionNotTerminal(t *testing.T) {
	m := NewManager()
	defer m.Close()

	session := &Session{
		ID:       "test-session",
		Status:   StatusRunning,
		Progress: &SessionProgress{},
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	err := m.DeleteSession("test-session")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot delete")
}

func TestManagerWithStorePersistence(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	m := NewManagerWithConfig(ManagerConfig{
		RepoName: "test-repo",
		Store:    store,
	})
	defer m.Close()

	// Create and persist a session manually
	session := &Session{
		ID:           "persist-test",
		Type:         SessionTypePlanner,
		Status:       StatusCompleted,
		WorktreePath: "/path/to/wt",
		WorktreeName: "feature",
		Prompt:       "test prompt",
		CreatedAt:    time.Now(),
		Progress:     &SessionProgress{TurnCount: 5},
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	m.outputsMu.Lock()
	m.outputs[session.ID] = []OutputLine{
		{Type: OutputTypeStatus, Content: "Started"},
		{Type: OutputTypeStatus, Content: "Completed"},
	}
	m.outputsMu.Unlock()

	// Persist
	m.persistSession(session)

	// Load from store
	loaded, err := store.LoadSession("test-repo", "feature", "persist-test")
	require.NoError(t, err)
	assert.Equal(t, SessionID("persist-test"), loaded.ID)
	assert.Equal(t, "test prompt", loaded.Prompt)
	assert.Len(t, loaded.Output, 2)
}

func TestManagerLoadHistorySessions(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	// Save some sessions
	for i := 0; i < 3; i++ {
		s := &StoredSession{
			ID:           SessionID("session-" + string(rune('a'+i))),
			Type:         SessionTypePlanner,
			Status:       StatusCompleted,
			RepoName:     "test-repo",
			WorktreeName: "feature",
			CreatedAt:    time.Now().Add(time.Duration(i) * time.Hour),
		}
		require.NoError(t, store.SaveSession(s))
	}

	m := NewManagerWithConfig(ManagerConfig{
		RepoName: "test-repo",
		Store:    store,
	})
	defer m.Close()

	sessions, err := m.LoadHistorySessions("feature")
	require.NoError(t, err)
	assert.Len(t, sessions, 3)
}

func TestManagerLoadSessionFromHistory(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	stored := &StoredSession{
		ID:           "test-session",
		Type:         SessionTypeBuilder,
		Status:       StatusCompleted,
		RepoName:     "test-repo",
		WorktreePath: "/path",
		WorktreeName: "main",
		Prompt:       "build it",
		CreatedAt:    time.Now(),
		Output: []OutputLine{
			{Type: OutputTypeText, Content: "Hello"},
		},
	}
	require.NoError(t, store.SaveSession(stored))

	m := NewManagerWithConfig(ManagerConfig{
		RepoName: "test-repo",
		Store:    store,
	})
	defer m.Close()

	loaded, err := m.LoadSessionFromHistory("main", "test-session")
	require.NoError(t, err)
	assert.Equal(t, "build it", loaded.Prompt)
	assert.Len(t, loaded.Output, 1)
}

func TestGenerateTitle(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   string
		maxLen int
	}{
		{
			name:   "short prompt fits entirely",
			prompt: "fix the bug",
			maxLen: 20,
			want:   "fix the bug",
		},
		{
			name:   "truncates at word boundary",
			prompt: "implement user authentication system",
			maxLen: 20,
			want:   "implement user",
		},
		{
			name:   "single long word truncated with ellipsis",
			prompt: "supercalifragilisticexpialidocious",
			maxLen: 20,
			want:   "supercalifragilis...",
		},
		{
			name:   "empty prompt",
			prompt: "",
			maxLen: 20,
			want:   "",
		},
		{
			name:   "single word that fits",
			prompt: "hello",
			maxLen: 20,
			want:   "hello",
		},
		{
			name:   "prompt with multiple spaces",
			prompt: "  fix   the   bug  ",
			maxLen: 20,
			want:   "fix the bug",
		},
		{
			name:   "exact max length",
			prompt: "twelve chars",
			maxLen: 12,
			want:   "twelve chars",
		},
		{
			name:   "first word exceeds maxLen",
			prompt: "abcdefghijklmnop rest",
			maxLen: 10,
			want:   "abcdefg...",
		},
		{
			name:   "single short word under limit",
			prompt: "hi",
			maxLen: 20,
			want:   "hi",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := generateTitle(tc.prompt, tc.maxLen)
			assert.Equal(t, tc.want, got)
			if tc.maxLen > 0 && tc.prompt != "" {
				assert.LessOrEqual(t, len(got), tc.maxLen, "title should not exceed maxLen")
			}
		})
	}
}

func TestSessionTitleAndModelInToInfo(t *testing.T) {
	session := &Session{
		ID:       "test-session",
		Type:     SessionTypePlanner,
		Status:   StatusRunning,
		Prompt:   "implement auth",
		Title:    "implement auth",
		Model:    "sonnet",
		Progress: &SessionProgress{},
	}

	info := session.ToInfo()
	assert.Equal(t, "implement auth", info.Title)
	assert.Equal(t, "sonnet", info.Model)
}

func TestSessionTitleAndModelInGetSessionInfo(t *testing.T) {
	m := NewManager()
	defer m.Close()

	session := &Session{
		ID:       "test-session",
		Type:     SessionTypePlanner,
		Status:   StatusRunning,
		Prompt:   "build the feature",
		Title:    "build the feature",
		Model:    "sonnet",
		Progress: &SessionProgress{},
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	info, ok := m.GetSessionInfo("test-session")
	assert.True(t, ok)
	assert.Equal(t, "build the feature", info.Title)
	assert.Equal(t, "sonnet", info.Model)
}

// TestLiveSessionsFoundByPathNotBranch verifies that GetSessionsForWorktree
// uses the worktree path (not branch name) to find sessions. This means
// sessions are found regardless of what branch is currently checked out.
func TestLiveSessionsFoundByPathNotBranch(t *testing.T) {
	m := NewManager()
	defer m.Close()

	// Add sessions with a specific worktree path
	sessions := []*Session{
		{ID: "s1", WorktreePath: "/worktrees/repo/feature-a", WorktreeName: "feature-a", Progress: &SessionProgress{}},
		{ID: "s2", WorktreePath: "/worktrees/repo/feature-a", WorktreeName: "feature-a", Progress: &SessionProgress{}},
		{ID: "s3", WorktreePath: "/worktrees/repo/feature-b", WorktreeName: "feature-b", Progress: &SessionProgress{}},
	}

	m.mu.Lock()
	for _, s := range sessions {
		m.sessions[s.ID] = s
	}
	m.mu.Unlock()

	// GetSessionsForWorktree uses path, so even if the branch inside
	// /worktrees/repo/feature-a has been changed to "new-branch",
	// sessions are still found by path
	result := m.GetSessionsForWorktree("/worktrees/repo/feature-a")
	assert.Len(t, result, 2, "should find sessions by path regardless of branch name")

	// Different path returns different sessions
	result = m.GetSessionsForWorktree("/worktrees/repo/feature-b")
	assert.Len(t, result, 1)

	// Non-existent path returns empty
	result = m.GetSessionsForWorktree("/worktrees/repo/nonexistent")
	assert.Empty(t, result)
}

func TestAppendOrAddText(t *testing.T) {
	m := NewManager()
	defer m.Close()

	sessID := SessionID("text-test")
	m.AddSession(&Session{ID: sessID, Status: StatusRunning})
	m.InitOutputBuffer(sessID)

	// First text creates a new line
	m.appendOrAddText(sessID, "Hello ")
	lines := m.GetSessionOutput(sessID)
	require.Equal(t, 1, len(lines))
	assert.Equal(t, OutputTypeText, lines[0].Type)
	assert.Equal(t, "Hello ", lines[0].Content)

	// Second text appends to the same line
	m.appendOrAddText(sessID, "World!")
	lines = m.GetSessionOutput(sessID)
	require.Equal(t, 1, len(lines), "should still be 1 line after append")
	assert.Equal(t, "Hello World!", lines[0].Content)

	// Non-text line breaks the chain
	m.addOutput(sessID, OutputLine{Type: OutputTypeToolStart, ToolName: "Read"})
	lines = m.GetSessionOutput(sessID)
	require.Equal(t, 2, len(lines))

	// New text after tool creates a NEW text line
	m.appendOrAddText(sessID, "After tool")
	lines = m.GetSessionOutput(sessID)
	require.Equal(t, 3, len(lines), "text after tool should be new line")
	assert.Equal(t, OutputTypeText, lines[2].Type)
	assert.Equal(t, "After tool", lines[2].Content)

	// Appending to that new text line works
	m.appendOrAddText(sessID, " more")
	lines = m.GetSessionOutput(sessID)
	require.Equal(t, 3, len(lines), "should still be 3 lines")
	assert.Equal(t, "After tool more", lines[2].Content)
}

func TestManagerRecentOutputLines(t *testing.T) {
	t.Parallel()

	m := NewManager()
	defer m.Close()

	sessionID := SessionID("test-recent")

	// Populate a mix of output types and user/assistant lines.
	m.outputsMu.Lock()
	m.outputs[sessionID] = []OutputLine{
		{Type: OutputTypeText, IsUserPrompt: true, Content: "user input"},        // should be skipped
		{Type: OutputTypeTool, Content: "tool call"},                             // not OutputTypeText, skipped
		{Type: OutputTypeText, IsUserPrompt: false, Content: "assistant line 1"}, // included
		{Type: OutputTypeText, IsUserPrompt: false, Content: "   "},              // blank, skipped
		{Type: OutputTypeText, IsUserPrompt: false, Content: "assistant line 2"}, // included
		{Type: OutputTypeText, IsUserPrompt: false, Content: "assistant line 3"}, // included
		{Type: OutputTypeText, IsUserPrompt: false, Content: "assistant line 4"}, // included (most recent)
	}
	m.outputsMu.Unlock()

	// Request last 3 — should skip user prompt, blank, and non-text, return chronological order.
	got := m.RecentOutputLines(sessionID, 3)
	require.Len(t, got, 3)
	assert.Equal(t, "assistant line 2", got[0])
	assert.Equal(t, "assistant line 3", got[1])
	assert.Equal(t, "assistant line 4", got[2])

	// Request more than available (only 4 qualifying lines).
	got = m.RecentOutputLines(sessionID, 10)
	require.Len(t, got, 4)
	assert.Equal(t, "assistant line 1", got[0])
	assert.Equal(t, "assistant line 4", got[3])

	// Non-existing session returns nil.
	got = m.RecentOutputLines("nonexistent", 3)
	assert.Nil(t, got)
}

func TestManagerGetAllSessions_RecentOutputFromBuffer(t *testing.T) {
	t.Parallel()

	m := NewManager()
	defer m.Close()

	sess := &Session{
		ID:       "test-all",
		Progress: &SessionProgress{},
	}
	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	// Add output lines to the live buffer — RecentOutput on SessionProgress is empty.
	m.outputsMu.Lock()
	m.outputs[sess.ID] = []OutputLine{
		{Type: OutputTypeText, IsUserPrompt: false, Content: "live line 1"},
		{Type: OutputTypeText, IsUserPrompt: false, Content: "live line 2"},
	}
	m.outputsMu.Unlock()

	all := m.GetAllSessions()
	require.Len(t, all, 1)
	// GetAllSessions must populate RecentOutput from the live buffer, not the stale snapshot.
	assert.Equal(t, []string{"live line 1", "live line 2"}, all[0].Progress.RecentOutput)
}

// --- ResumeSession tests ---

func TestResumeSession_NotFound(t *testing.T) {
	t.Parallel()

	m := NewManager()
	defer m.Close()

	err := m.ResumeSession("nonexistent", "hello")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestResumeSession_NoCLISessionID(t *testing.T) {
	t.Parallel()

	m := NewManager()
	defer m.Close()

	sess := &Session{
		ID:       "test-session",
		Status:   StatusCompleted,
		Progress: &SessionProgress{},
		// CLISessionID intentionally empty
	}
	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	err := m.ResumeSession("test-session", "hello")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no CLI session ID")
}

func TestResumeSession_WrongStatus(t *testing.T) {
	t.Parallel()

	m := NewManager()
	defer m.Close()

	for _, status := range []SessionStatus{StatusPending, StatusRunning, StatusIdle} {
		sess := &Session{
			ID:           SessionID("sess-" + string(status)),
			Status:       status,
			CLISessionID: "some-cli-id",
			Progress:     &SessionProgress{},
		}
		m.mu.Lock()
		m.sessions[sess.ID] = sess
		m.mu.Unlock()

		err := m.ResumeSession(sess.ID, "hello")
		require.Error(t, err, "expected error for status %s", status)
		assert.Contains(t, err.Error(), string(status))
	}
}

func TestResumeSession_ResetsStateAndSchedulesRun(t *testing.T) {
	t.Parallel()

	m := NewManager()
	defer m.Close()

	sess := &Session{
		ID:           "resume-test",
		Type:         SessionTypePlanner,
		Status:       StatusCompleted,
		CLISessionID: "abc123defghi",
		WorktreePath: "/tmp/wt",
		WorktreeName: "feature",
		Prompt:       "original prompt",
		Progress:     &SessionProgress{},
	}
	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()
	m.InitOutputBuffer(sess.ID)

	// ResumeSession should schedule a goroutine and return immediately.
	// We cannot wait for the goroutine to actually run a real runner,
	// but we can verify the synchronous state changes it makes.
	//
	// The goroutine will fail quickly because there is no real runner
	// configured; we just check the state was reset before the run.
	//
	// Wait up to 2 seconds for the session status to transition away from
	// StatusPending (either to running or failed), which means the goroutine
	// was scheduled and started.
	err := m.ResumeSession("resume-test", "new prompt")
	require.NoError(t, err)

	// Status must have been reset to StatusPending synchronously.
	sess.mu.RLock()
	statusAfterResume := sess.Status
	ctxAfterResume := sess.ctx
	sess.mu.RUnlock()
	assert.Equal(t, StatusPending, statusAfterResume, "status should be reset to Pending")
	assert.NotNil(t, ctxAfterResume, "ctx should be set by ResumeSession")

	// Output buffer should have been re-initialized and contain the status line.
	require.Eventually(t, func() bool {
		output := m.GetSessionOutput("resume-test")
		for _, line := range output {
			if line.Type == OutputTypeStatus && strings.Contains(line.Content, "Resuming") {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "expected 'Resuming' status line in output")
}

func TestResumeSession_ShortCLISessionIDNoPanic(t *testing.T) {
	t.Parallel()

	m := NewManager()
	defer m.Close()

	// A CLISessionID shorter than 12 characters must not cause a panic.
	sess := &Session{
		ID:           "short-id-test",
		Type:         SessionTypePlanner,
		Status:       StatusCompleted,
		CLISessionID: "short", // only 5 chars
		WorktreePath: "/tmp/wt",
		WorktreeName: "feature",
		Prompt:       "original",
		Progress:     &SessionProgress{},
	}
	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()
	m.InitOutputBuffer(sess.ID)

	// Must not panic regardless of runner outcome.
	require.NotPanics(t, func() {
		_ = m.ResumeSession("short-id-test", "hello")
	})
}

// --- rehydrateSession tests ---

func TestRehydrateSession_NoStore(t *testing.T) {
	t.Parallel()

	// Manager without a store should return (nil, false).
	m := NewManager()
	defer m.Close()

	sess, ok := m.rehydrateSession("does-not-exist")
	assert.Nil(t, sess)
	assert.False(t, ok)
}

func TestRehydrateSession_NotInStore(t *testing.T) {
	t.Parallel()

	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	m := NewManagerWithConfig(ManagerConfig{
		RepoName: "test-repo",
		Store:    store,
	})
	defer m.Close()

	sess, ok := m.rehydrateSession("missing-session")
	assert.Nil(t, sess)
	assert.False(t, ok)
}

func TestRehydrateSession_FoundInStore(t *testing.T) {
	t.Parallel()

	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	stored := &StoredSession{
		ID:           "stored-sess",
		Type:         SessionTypeBuilder,
		Status:       StatusCompleted,
		RepoName:     "test-repo",
		WorktreePath: "/path/wt",
		WorktreeName: "feature",
		Prompt:       "do the thing",
		CLISessionID: "clisessid123",
		CreatedAt:    time.Now(),
	}
	require.NoError(t, store.SaveSession(stored))

	m := NewManagerWithConfig(ManagerConfig{
		RepoName: "test-repo",
		Store:    store,
	})
	defer m.Close()

	sess, ok := m.rehydrateSession("stored-sess")
	require.True(t, ok)
	require.NotNil(t, sess)

	assert.Equal(t, SessionID("stored-sess"), sess.ID)
	assert.Equal(t, "do the thing", sess.Prompt)
	assert.Equal(t, "clisessid123", sess.CLISessionID)
	assert.Equal(t, StatusCompleted, sess.Status)
	// ctx and cancel must NOT be set — ResumeSession sets them.
	assert.Nil(t, sess.ctx, "rehydrated session should not have a context yet")
	assert.Nil(t, sess.cancel, "rehydrated session should not have a cancel func yet")

	// Session must have been added to the manager's in-memory map.
	_, inMap := m.GetSession("stored-sess")
	assert.True(t, inMap, "rehydrated session should be in manager's sessions map")
}

func TestSessionInfoIsResumable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		status       SessionStatus
		cliSessionID string
		want         bool
	}{
		{"completed with ID", StatusCompleted, "abc123", true},
		{"failed with ID", StatusFailed, "abc123", true},
		{"stopped with ID", StatusStopped, "abc123", true},
		{"completed no ID", StatusCompleted, "", false},
		{"idle with ID", StatusIdle, "abc123", false},
		{"running with ID", StatusRunning, "abc123", false},
		{"pending with ID", StatusPending, "abc123", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info := SessionInfo{
				Status:       tc.status,
				CLISessionID: tc.cliSessionID,
			}
			assert.Equal(t, tc.want, info.IsResumable())
		})
	}
}

func TestReconcileTmuxSessions_MarksGoneWindowsCompleted(t *testing.T) {
	// When not inside tmux, all tmux windows appear "gone",
	// so reconciliation should mark running sessions as completed.
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	now := time.Now()
	startedAt := now.Add(-time.Minute)

	// Save a session that looks like it was running in tmux
	stored := &StoredSession{
		ID:             "tmux-session-1",
		Type:           SessionTypeBuilder,
		Status:         StatusRunning,
		RepoName:       "test-repo",
		WorktreePath:   "/path/to/wt",
		WorktreeName:   "feature",
		Prompt:         "build something",
		TmuxWindowName: "test-repo/feature:0",
		TmuxWindowID:   "@99",
		RunnerType:     "tmux",
		CreatedAt:      now,
		StartedAt:      &startedAt,
	}
	require.NoError(t, store.SaveSession(stored))

	m := NewManagerWithConfig(ManagerConfig{
		RepoName:    "test-repo",
		Store:       store,
		SessionMode: SessionModeTmux,
	})
	defer m.Close()

	err = m.ReconcileTmuxSessions()
	require.NoError(t, err)

	// Session should now be completed in the store since the window doesn't exist
	reloaded, err := store.LoadSession("test-repo", "feature", "tmux-session-1")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, reloaded.Status)
	assert.NotNil(t, reloaded.CompletedAt)

	// Session should NOT be in the manager's in-memory map (window was gone)
	_, inMap := m.GetSession("tmux-session-1")
	assert.False(t, inMap, "gone session should not be tracked in memory")
}

func TestReconcileTmuxSessions_SkipsNonTmux(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	// Save a TUI session that was running — should be ignored by reconciliation
	stored := &StoredSession{
		ID:           "tui-session-1",
		Type:         SessionTypeBuilder,
		Status:       StatusRunning,
		RepoName:     "test-repo",
		WorktreePath: "/path/to/wt",
		WorktreeName: "feature",
		Prompt:       "build something",
		RunnerType:   "tui",
		CreatedAt:    time.Now(),
	}
	require.NoError(t, store.SaveSession(stored))

	m := NewManagerWithConfig(ManagerConfig{
		RepoName:    "test-repo",
		Store:       store,
		SessionMode: SessionModeTmux,
	})
	defer m.Close()

	err = m.ReconcileTmuxSessions()
	require.NoError(t, err)

	// TUI session should remain untouched
	reloaded, err := store.LoadSession("test-repo", "feature", "tui-session-1")
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, reloaded.Status, "TUI session should not be modified by reconciliation")
}

func TestReconcileTmuxSessions_SkipsCompletedSessions(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	completedAt := time.Now()
	stored := &StoredSession{
		ID:             "tmux-done",
		Type:           SessionTypeBuilder,
		Status:         StatusCompleted,
		RepoName:       "test-repo",
		WorktreePath:   "/path/to/wt",
		WorktreeName:   "feature",
		Prompt:         "done",
		TmuxWindowName: "test-repo/feature:0",
		TmuxWindowID:   "@42",
		RunnerType:     "tmux",
		CreatedAt:      time.Now(),
		CompletedAt:    &completedAt,
	}
	require.NoError(t, store.SaveSession(stored))

	m := NewManagerWithConfig(ManagerConfig{
		RepoName:    "test-repo",
		Store:       store,
		SessionMode: SessionModeTmux,
	})
	defer m.Close()

	err = m.ReconcileTmuxSessions()
	require.NoError(t, err)

	// Already-completed session should remain completed with same CompletedAt
	reloaded, err := store.LoadSession("test-repo", "feature", "tmux-done")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, reloaded.Status)
	assert.Equal(t, completedAt.Unix(), reloaded.CompletedAt.Unix())
}

func TestReconcileTmuxSessions_NoopWithoutStore(t *testing.T) {
	m := NewManagerWithConfig(ManagerConfig{
		SessionMode: SessionModeTmux,
	})
	defer m.Close()

	err := m.ReconcileTmuxSessions()
	assert.NoError(t, err)
}

func TestReconcileTmuxSessions_NoopInTUIMode(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	m := NewManagerWithConfig(ManagerConfig{
		RepoName:    "test-repo",
		Store:       store,
		SessionMode: SessionModeTUI,
	})
	defer m.Close()

	err = m.ReconcileTmuxSessions()
	assert.NoError(t, err)
}
