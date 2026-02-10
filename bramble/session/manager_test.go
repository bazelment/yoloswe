package session

import (
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
