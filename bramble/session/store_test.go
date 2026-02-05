package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStore(t *testing.T) {
	t.Run("creates directory", func(t *testing.T) {
		dir := t.TempDir()
		storeDir := filepath.Join(dir, "sessions")

		store, err := NewStore(storeDir)
		require.NoError(t, err)
		require.NotNil(t, store)

		// Verify directory was created
		info, err := os.Stat(storeDir)
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	})

	t.Run("uses existing directory", func(t *testing.T) {
		dir := t.TempDir()

		store, err := NewStore(dir)
		require.NoError(t, err)
		require.NotNil(t, store)
	})
}

func TestStoreSaveAndLoad(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	now := time.Now().Truncate(time.Second)
	startedAt := now.Add(time.Second)
	completedAt := now.Add(time.Minute)

	session := &StoredSession{
		ID:           "test-session-1",
		Type:         SessionTypePlanner,
		Status:       StatusCompleted,
		RepoName:     "my-repo",
		WorktreePath: "/path/to/worktree",
		WorktreeName: "feature-branch",
		Prompt:       "Test prompt",
		CreatedAt:    now,
		StartedAt:    &startedAt,
		CompletedAt:  &completedAt,
		Progress: &StoredProgress{
			TurnCount:    5,
			TotalCostUSD: 0.0123,
			InputTokens:  1000,
			OutputTokens: 500,
		},
		Output: []OutputLine{
			{Timestamp: now, Type: OutputTypeStatus, Content: "Starting"},
			{Timestamp: now.Add(time.Second), Type: OutputTypeText, Content: "Hello"},
		},
	}

	// Save
	err = store.SaveSession(session)
	require.NoError(t, err)

	// Load
	loaded, err := store.LoadSession("my-repo", "feature-branch", "test-session-1")
	require.NoError(t, err)
	require.NotNil(t, loaded)

	// Verify fields
	assert.Equal(t, session.ID, loaded.ID)
	assert.Equal(t, session.Type, loaded.Type)
	assert.Equal(t, session.Status, loaded.Status)
	assert.Equal(t, session.RepoName, loaded.RepoName)
	assert.Equal(t, session.WorktreePath, loaded.WorktreePath)
	assert.Equal(t, session.WorktreeName, loaded.WorktreeName)
	assert.Equal(t, session.Prompt, loaded.Prompt)
	assert.Equal(t, session.CreatedAt.Unix(), loaded.CreatedAt.Unix())
	assert.Equal(t, session.StartedAt.Unix(), loaded.StartedAt.Unix())
	assert.Equal(t, session.CompletedAt.Unix(), loaded.CompletedAt.Unix())

	require.NotNil(t, loaded.Progress)
	assert.Equal(t, session.Progress.TurnCount, loaded.Progress.TurnCount)
	assert.Equal(t, session.Progress.TotalCostUSD, loaded.Progress.TotalCostUSD)
	assert.Equal(t, session.Progress.InputTokens, loaded.Progress.InputTokens)
	assert.Equal(t, session.Progress.OutputTokens, loaded.Progress.OutputTokens)

	require.Len(t, loaded.Output, 2)
	assert.Equal(t, session.Output[0].Content, loaded.Output[0].Content)
	assert.Equal(t, session.Output[1].Type, loaded.Output[1].Type)
}

func TestStoreSaveValidation(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	t.Run("nil session", func(t *testing.T) {
		err := store.SaveSession(nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "nil")
	})

	t.Run("empty ID", func(t *testing.T) {
		err := store.SaveSession(&StoredSession{
			RepoName:     "repo",
			WorktreeName: "wt",
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ID")
	})

	t.Run("empty repo name", func(t *testing.T) {
		err := store.SaveSession(&StoredSession{
			ID:           "id",
			WorktreeName: "wt",
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "repo")
	})

	t.Run("empty worktree name", func(t *testing.T) {
		err := store.SaveSession(&StoredSession{
			ID:       "id",
			RepoName: "repo",
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "worktree")
	})
}

func TestStoreLoadNotFound(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	_, err = store.LoadSession("repo", "wt", "nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestStoreDelete(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	session := &StoredSession{
		ID:           "to-delete",
		Type:         SessionTypeBuilder,
		Status:       StatusCompleted,
		RepoName:     "repo",
		WorktreeName: "wt",
		CreatedAt:    time.Now(),
	}

	// Save
	err = store.SaveSession(session)
	require.NoError(t, err)

	// Verify it exists
	_, err = store.LoadSession("repo", "wt", "to-delete")
	require.NoError(t, err)

	// Delete
	err = store.DeleteSession("repo", "wt", "to-delete")
	require.NoError(t, err)

	// Verify it's gone
	_, err = store.LoadSession("repo", "wt", "to-delete")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestStoreDeleteNotFound(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	err = store.DeleteSession("repo", "wt", "nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestStoreListSessions(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	// Create sessions with different times
	now := time.Now()
	sessions := []*StoredSession{
		{
			ID:           "session-1",
			Type:         SessionTypePlanner,
			Status:       StatusCompleted,
			RepoName:     "repo",
			WorktreeName: "wt",
			Prompt:       "First",
			CreatedAt:    now.Add(-2 * time.Hour),
		},
		{
			ID:           "session-2",
			Type:         SessionTypeBuilder,
			Status:       StatusRunning,
			RepoName:     "repo",
			WorktreeName: "wt",
			Prompt:       "Second",
			CreatedAt:    now.Add(-1 * time.Hour),
		},
		{
			ID:           "session-3",
			Type:         SessionTypePlanner,
			Status:       StatusFailed,
			RepoName:     "repo",
			WorktreeName: "wt",
			Prompt:       "Third",
			CreatedAt:    now,
		},
	}

	for _, s := range sessions {
		require.NoError(t, store.SaveSession(s))
	}

	// List sessions
	list, err := store.ListSessions("repo", "wt")
	require.NoError(t, err)
	require.Len(t, list, 3)

	// Should be sorted newest first
	assert.Equal(t, SessionID("session-3"), list[0].ID)
	assert.Equal(t, SessionID("session-2"), list[1].ID)
	assert.Equal(t, SessionID("session-1"), list[2].ID)

	// Verify metadata
	assert.Equal(t, SessionTypePlanner, list[0].Type)
	assert.Equal(t, StatusFailed, list[0].Status)
	assert.Equal(t, "Third", list[0].Prompt)
}

func TestStoreListSessionsEmpty(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	list, err := store.ListSessions("nonexistent", "wt")
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestStoreListAllSessions(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	now := time.Now()

	// Create sessions in different repos/worktrees
	sessions := []*StoredSession{
		{
			ID: "s1", Type: SessionTypePlanner, Status: StatusCompleted,
			RepoName: "repo1", WorktreeName: "main", CreatedAt: now.Add(-3 * time.Hour),
		},
		{
			ID: "s2", Type: SessionTypeBuilder, Status: StatusRunning,
			RepoName: "repo1", WorktreeName: "feature", CreatedAt: now.Add(-2 * time.Hour),
		},
		{
			ID: "s3", Type: SessionTypePlanner, Status: StatusCompleted,
			RepoName: "repo2", WorktreeName: "main", CreatedAt: now.Add(-1 * time.Hour),
		},
		{
			ID: "s4", Type: SessionTypeBuilder, Status: StatusFailed,
			RepoName: "repo2", WorktreeName: "fix", CreatedAt: now,
		},
	}

	for _, s := range sessions {
		require.NoError(t, store.SaveSession(s))
	}

	// List all
	list, err := store.ListAllSessions()
	require.NoError(t, err)
	require.Len(t, list, 4)

	// Should be sorted newest first
	assert.Equal(t, SessionID("s4"), list[0].ID)
	assert.Equal(t, SessionID("s3"), list[1].ID)
	assert.Equal(t, SessionID("s2"), list[2].ID)
	assert.Equal(t, SessionID("s1"), list[3].ID)
}

func TestStoreListRepos(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	// Create sessions in different repos
	sessions := []*StoredSession{
		{ID: "s1", Type: SessionTypePlanner, Status: StatusCompleted, RepoName: "alpha", WorktreeName: "main", CreatedAt: time.Now()},
		{ID: "s2", Type: SessionTypePlanner, Status: StatusCompleted, RepoName: "beta", WorktreeName: "main", CreatedAt: time.Now()},
		{ID: "s3", Type: SessionTypePlanner, Status: StatusCompleted, RepoName: "gamma", WorktreeName: "main", CreatedAt: time.Now()},
	}

	for _, s := range sessions {
		require.NoError(t, store.SaveSession(s))
	}

	repos, err := store.ListRepos()
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, repos)
}

func TestStoreListWorktrees(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	// Create sessions in different worktrees
	sessions := []*StoredSession{
		{ID: "s1", Type: SessionTypePlanner, Status: StatusCompleted, RepoName: "repo", WorktreeName: "main", CreatedAt: time.Now()},
		{ID: "s2", Type: SessionTypePlanner, Status: StatusCompleted, RepoName: "repo", WorktreeName: "feature-a", CreatedAt: time.Now()},
		{ID: "s3", Type: SessionTypePlanner, Status: StatusCompleted, RepoName: "repo", WorktreeName: "feature-b", CreatedAt: time.Now()},
	}

	for _, s := range sessions {
		require.NoError(t, store.SaveSession(s))
	}

	worktrees, err := store.ListWorktrees("repo")
	require.NoError(t, err)
	assert.Equal(t, []string{"feature-a", "feature-b", "main"}, worktrees)
}

func TestStoreSanitizeName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"with/slash", "with_slash"},
		{"with\\backslash", "with_backslash"},
		{"with:colon", "with_colon"},
		{"with space", "with_space"},
		{"complex/path\\name:with spaces", "complex_path_name_with_spaces"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.expected, sanitizeName(tc.input))
		})
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	const numGoroutines = 10
	const numOperations = 20

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*numOperations)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			for j := 0; j < numOperations; j++ {
				// Use a simple session ID without slashes
				sessionID := SessionID(fmt.Sprintf("session-%c-%d", 'A'+goroutineID, j))
				session := &StoredSession{
					ID:           sessionID,
					Type:         SessionTypePlanner,
					Status:       StatusCompleted,
					RepoName:     "concurrent-repo",
					WorktreeName: "concurrent-wt",
					Prompt:       "Test",
					CreatedAt:    time.Now(),
				}

				// Save
				if err := store.SaveSession(session); err != nil {
					errors <- err
					return
				}

				// Load
				if _, err := store.LoadSession("concurrent-repo", "concurrent-wt", sessionID); err != nil {
					errors <- err
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("Concurrent operation error: %v", err)
	}

	// Verify all sessions were saved
	list, err := store.ListSessions("concurrent-repo", "concurrent-wt")
	require.NoError(t, err)
	assert.Len(t, list, numGoroutines*numOperations)
}

func TestSessionToStored(t *testing.T) {
	now := time.Now()
	startedAt := now.Add(time.Second)

	session := &Session{
		ID:           "test-id",
		Type:         SessionTypePlanner,
		Status:       StatusRunning,
		WorktreePath: "/path/to/wt",
		WorktreeName: "feature",
		Prompt:       "Do something",
		CreatedAt:    now,
		StartedAt:    &startedAt,
		Progress: &SessionProgress{
			TurnCount:    3,
			TotalCostUSD: 0.05,
			InputTokens:  500,
			OutputTokens: 200,
		},
	}

	output := []OutputLine{
		{Timestamp: now, Type: OutputTypeStatus, Content: "Started"},
	}

	stored := SessionToStored(session, "my-repo", output)

	assert.Equal(t, session.ID, stored.ID)
	assert.Equal(t, session.Type, stored.Type)
	assert.Equal(t, session.Status, stored.Status)
	assert.Equal(t, "my-repo", stored.RepoName)
	assert.Equal(t, session.WorktreePath, stored.WorktreePath)
	assert.Equal(t, session.WorktreeName, stored.WorktreeName)
	assert.Equal(t, session.Prompt, stored.Prompt)
	assert.Equal(t, session.CreatedAt, stored.CreatedAt)
	assert.Equal(t, session.StartedAt, stored.StartedAt)
	assert.Nil(t, stored.CompletedAt)
	assert.Empty(t, stored.ErrorMsg)
	require.NotNil(t, stored.Progress)
	assert.Equal(t, 3, stored.Progress.TurnCount)
	assert.Len(t, stored.Output, 1)
}

func TestStoredToSessionInfo(t *testing.T) {
	now := time.Now()
	completedAt := now.Add(time.Minute)

	stored := &StoredSession{
		ID:           "stored-id",
		Type:         SessionTypeBuilder,
		Status:       StatusCompleted,
		RepoName:     "repo",
		WorktreePath: "/path",
		WorktreeName: "main",
		Prompt:       "Build it",
		CreatedAt:    now,
		CompletedAt:  &completedAt,
		ErrorMsg:     "",
		Progress: &StoredProgress{
			TurnCount:    10,
			TotalCostUSD: 0.25,
			InputTokens:  2000,
			OutputTokens: 1000,
		},
	}

	info := StoredToSessionInfo(stored)

	assert.Equal(t, stored.ID, info.ID)
	assert.Equal(t, stored.Type, info.Type)
	assert.Equal(t, stored.Status, info.Status)
	assert.Equal(t, stored.WorktreePath, info.WorktreePath)
	assert.Equal(t, stored.WorktreeName, info.WorktreeName)
	assert.Equal(t, stored.Prompt, info.Prompt)
	assert.Equal(t, stored.CreatedAt, info.CreatedAt)
	assert.Equal(t, stored.CompletedAt, info.CompletedAt)
	assert.Empty(t, info.ErrorMsg)
	assert.Equal(t, 10, info.Progress.TurnCount)
	assert.Equal(t, 0.25, info.Progress.TotalCostUSD)
}

func TestTitleAndModelRoundtrip(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	now := time.Now().Truncate(time.Second)

	session := &StoredSession{
		ID:           "title-model-test",
		Type:         SessionTypePlanner,
		Status:       StatusCompleted,
		RepoName:     "repo",
		WorktreePath: "/path",
		WorktreeName: "feature",
		Prompt:       "implement user authentication system",
		Title:        "implement user",
		Model:        "sonnet",
		CreatedAt:    now,
	}

	err = store.SaveSession(session)
	require.NoError(t, err)

	loaded, err := store.LoadSession("repo", "feature", "title-model-test")
	require.NoError(t, err)
	assert.Equal(t, "implement user", loaded.Title)
	assert.Equal(t, "sonnet", loaded.Model)
}

func TestTitleAndModelInSessionToStored(t *testing.T) {
	session := &Session{
		ID:           "test-id",
		Type:         SessionTypePlanner,
		Status:       StatusRunning,
		WorktreePath: "/path",
		WorktreeName: "feature",
		Prompt:       "fix the bug",
		Title:        "fix the bug",
		Model:        "sonnet",
		CreatedAt:    time.Now(),
		Progress:     &SessionProgress{},
	}

	stored := SessionToStored(session, "repo", nil)

	assert.Equal(t, "fix the bug", stored.Title)
	assert.Equal(t, "sonnet", stored.Model)
}

func TestTitleAndModelInStoredToSessionInfo(t *testing.T) {
	stored := &StoredSession{
		ID:           "test-id",
		Type:         SessionTypeBuilder,
		Status:       StatusCompleted,
		WorktreePath: "/path",
		WorktreeName: "main",
		Prompt:       "build the feature",
		Title:        "build the feature",
		Model:        "sonnet",
		CreatedAt:    time.Now(),
	}

	info := StoredToSessionInfo(stored)
	assert.Equal(t, "build the feature", info.Title)
	assert.Equal(t, "sonnet", info.Model)
}

func TestTitleAndModelInListSessions(t *testing.T) {
	store, err := NewStore(t.TempDir())
	require.NoError(t, err)

	session := &StoredSession{
		ID:           "list-test",
		Type:         SessionTypePlanner,
		Status:       StatusCompleted,
		RepoName:     "repo",
		WorktreeName: "main",
		Prompt:       "plan the migration",
		Title:        "plan the migration",
		Model:        "sonnet",
		CreatedAt:    time.Now(),
	}
	require.NoError(t, store.SaveSession(session))

	list, err := store.ListSessions("repo", "main")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "plan the migration", list[0].Title)
	assert.Equal(t, "sonnet", list[0].Model)
}
