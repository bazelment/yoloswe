package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store provides persistence for session history.
// Sessions are stored as JSON files under ~/.bramble/sessions/<repo>/<worktree>/<session-id>.json
type Store struct {
	baseDir string
	mu      sync.RWMutex
}

// StoredSession is the serializable representation of a session.
type StoredSession struct {
	ID           SessionID       `json:"id"`
	Type         SessionType     `json:"type"`
	Status       SessionStatus   `json:"status"`
	RepoName     string          `json:"repo_name"`
	WorktreePath string          `json:"worktree_path"`
	WorktreeName string          `json:"worktree_name"`
	Prompt       string          `json:"prompt"`
	Title        string          `json:"title,omitempty"`
	Model        string          `json:"model,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	CompletedAt  *time.Time      `json:"completed_at,omitempty"`
	ErrorMsg     string          `json:"error_msg,omitempty"`
	Progress     *StoredProgress `json:"progress,omitempty"`
	Output       []OutputLine    `json:"output,omitempty"`
}

// StoredProgress is the serializable representation of session progress.
type StoredProgress struct {
	TurnCount    int     `json:"turn_count"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
}

// SessionMeta contains minimal session info for listing.
type SessionMeta struct {
	ID           SessionID     `json:"id"`
	Type         SessionType   `json:"type"`
	Status       SessionStatus `json:"status"`
	RepoName     string        `json:"repo_name"`
	WorktreeName string        `json:"worktree_name"`
	Prompt       string        `json:"prompt"`
	Title        string        `json:"title,omitempty"`
	Model        string        `json:"model,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	CompletedAt  *time.Time    `json:"completed_at,omitempty"`
}

// DefaultStoreDir returns the default store directory (~/.bramble/sessions).
func DefaultStoreDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".bramble", "sessions"), nil
}

// NewStore creates a new session store.
// If baseDir is empty, uses the default (~/.bramble/sessions).
func NewStore(baseDir string) (*Store, error) {
	if baseDir == "" {
		var err error
		baseDir, err = DefaultStoreDir()
		if err != nil {
			return nil, err
		}
	}

	// Ensure base directory exists
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %w", err)
	}

	return &Store{
		baseDir: baseDir,
	}, nil
}

// sessionDir returns the directory for a session's repo/worktree.
func (s *Store) sessionDir(repoName, worktreeName string) string {
	// Sanitize names for filesystem
	repoName = sanitizeName(repoName)
	worktreeName = sanitizeName(worktreeName)
	return filepath.Join(s.baseDir, repoName, worktreeName)
}

// sessionPath returns the file path for a session.
func (s *Store) sessionPath(repoName, worktreeName string, id SessionID) string {
	return filepath.Join(s.sessionDir(repoName, worktreeName), string(id)+".json")
}

// sanitizeName sanitizes a name for use as a directory name.
func sanitizeName(name string) string {
	// Replace problematic characters
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, ":", "_")
	name = strings.ReplaceAll(name, " ", "_")
	return name
}

// SaveSession saves a session to disk.
func (s *Store) SaveSession(session *StoredSession) error {
	if session == nil {
		return fmt.Errorf("session is nil")
	}
	if session.ID == "" {
		return fmt.Errorf("session ID is empty")
	}
	if session.RepoName == "" {
		return fmt.Errorf("repo name is empty")
	}
	if session.WorktreeName == "" {
		return fmt.Errorf("worktree name is empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.sessionDir(session.RepoName, session.WorktreeName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	path := s.sessionPath(session.RepoName, session.WorktreeName, session.ID)

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}

	// Write atomically using temp file + rename
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write session file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Clean up temp file
		return fmt.Errorf("failed to rename session file: %w", err)
	}

	return nil
}

// LoadSession loads a session from disk.
func (s *Store) LoadSession(repoName, worktreeName string, id SessionID) (*StoredSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.sessionPath(repoName, worktreeName, id)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("session not found: %s", id)
		}
		return nil, fmt.Errorf("failed to read session file: %w", err)
	}

	var session StoredSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}

	return &session, nil
}

// DeleteSession removes a session from disk.
func (s *Store) DeleteSession(repoName, worktreeName string, id SessionID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.sessionPath(repoName, worktreeName, id)

	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("session not found: %s", id)
		}
		return fmt.Errorf("failed to delete session: %w", err)
	}

	return nil
}

// ListSessions returns metadata for all sessions in a worktree.
// Sessions are sorted by creation time, newest first.
func (s *Store) ListSessions(repoName, worktreeName string) ([]*SessionMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	dir := s.sessionDir(repoName, worktreeName)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []*SessionMeta{}, nil
		}
		return nil, fmt.Errorf("failed to read session directory: %w", err)
	}

	var sessions []*SessionMeta

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue // Skip files we can't read
		}

		var stored StoredSession
		if err := json.Unmarshal(data, &stored); err != nil {
			continue // Skip malformed files
		}

		sessions = append(sessions, &SessionMeta{
			ID:           stored.ID,
			Type:         stored.Type,
			Status:       stored.Status,
			RepoName:     stored.RepoName,
			WorktreeName: stored.WorktreeName,
			Prompt:       stored.Prompt,
			Title:        stored.Title,
			Model:        stored.Model,
			CreatedAt:    stored.CreatedAt,
			CompletedAt:  stored.CompletedAt,
		})
	}

	// Sort by creation time, newest first
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})

	return sessions, nil
}

// ListAllSessions returns metadata for all sessions across all repos and worktrees.
// Sessions are sorted by creation time, newest first.
func (s *Store) ListAllSessions() ([]*SessionMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var sessions []*SessionMeta

	// Walk the base directory
	err := filepath.WalkDir(s.baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil // Skip files we can't read
		}

		var stored StoredSession
		if err := json.Unmarshal(data, &stored); err != nil {
			return nil // Skip malformed files
		}

		sessions = append(sessions, &SessionMeta{
			ID:           stored.ID,
			Type:         stored.Type,
			Status:       stored.Status,
			RepoName:     stored.RepoName,
			WorktreeName: stored.WorktreeName,
			Prompt:       stored.Prompt,
			Title:        stored.Title,
			Model:        stored.Model,
			CreatedAt:    stored.CreatedAt,
			CompletedAt:  stored.CompletedAt,
		})

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to walk session directory: %w", err)
	}

	// Sort by creation time, newest first
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})

	return sessions, nil
}

// ListRepos returns all repo names that have sessions.
func (s *Store) ListRepos() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to read store directory: %w", err)
	}

	var repos []string
	for _, entry := range entries {
		if entry.IsDir() {
			repos = append(repos, entry.Name())
		}
	}

	sort.Strings(repos)
	return repos, nil
}

// ListWorktrees returns all worktree names for a repo that have sessions.
func (s *Store) ListWorktrees(repoName string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	repoDir := filepath.Join(s.baseDir, sanitizeName(repoName))

	entries, err := os.ReadDir(repoDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to read repo directory: %w", err)
	}

	var worktrees []string
	for _, entry := range entries {
		if entry.IsDir() {
			worktrees = append(worktrees, entry.Name())
		}
	}

	sort.Strings(worktrees)
	return worktrees, nil
}

// SessionToStored converts a Session and output to a StoredSession.
func SessionToStored(session *Session, repoName string, output []OutputLine) *StoredSession {
	if session == nil {
		return nil
	}

	session.mu.RLock()
	defer session.mu.RUnlock()

	stored := &StoredSession{
		ID:           session.ID,
		Type:         session.Type,
		Status:       session.Status,
		RepoName:     repoName,
		WorktreePath: session.WorktreePath,
		WorktreeName: session.WorktreeName,
		Prompt:       session.Prompt,
		Title:        session.Title,
		Model:        session.Model,
		CreatedAt:    session.CreatedAt,
		StartedAt:    session.StartedAt,
		CompletedAt:  session.CompletedAt,
		Output:       output,
	}

	if session.Error != nil {
		stored.ErrorMsg = session.Error.Error()
	}

	if session.Progress != nil {
		progress := session.Progress.Clone()
		stored.Progress = &StoredProgress{
			TurnCount:    progress.TurnCount,
			TotalCostUSD: progress.TotalCostUSD,
			InputTokens:  progress.InputTokens,
			OutputTokens: progress.OutputTokens,
		}
	}

	return stored
}

// StoredToSessionInfo converts a StoredSession to SessionInfo for display.
func StoredToSessionInfo(stored *StoredSession) SessionInfo {
	if stored == nil {
		return SessionInfo{}
	}

	info := SessionInfo{
		ID:           stored.ID,
		Type:         stored.Type,
		Status:       stored.Status,
		WorktreePath: stored.WorktreePath,
		WorktreeName: stored.WorktreeName,
		Prompt:       stored.Prompt,
		Title:        stored.Title,
		Model:        stored.Model,
		CreatedAt:    stored.CreatedAt,
		StartedAt:    stored.StartedAt,
		CompletedAt:  stored.CompletedAt,
		ErrorMsg:     stored.ErrorMsg,
	}

	if stored.Progress != nil {
		info.Progress = SessionProgress{
			TurnCount:    stored.Progress.TurnCount,
			TotalCostUSD: stored.Progress.TotalCostUSD,
			InputTokens:  stored.Progress.InputTokens,
			OutputTokens: stored.Progress.OutputTokens,
		}
	}

	return info
}
