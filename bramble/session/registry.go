package session

import (
	"fmt"
	"sync"
)

// SessionRegistry aggregates multiple Manager instances so that IPC handlers
// can look up sessions across all repos (initial + those opened via Alt-R).
type SessionRegistry struct { //nolint:govet // fieldalignment: readability over packing
	mu         sync.RWMutex
	managers   []*Manager
	onRegister []func(*Manager)
}

// NewSessionRegistry creates an empty registry.
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{}
}

// Register adds a manager to the registry. Safe for concurrent use.
func (r *SessionRegistry) Register(mgr *Manager) {
	r.mu.Lock()
	hooks := make([]func(*Manager), len(r.onRegister))
	copy(hooks, r.onRegister)
	r.managers = append(r.managers, mgr)
	r.mu.Unlock()

	// Hooks run outside the lock: a hook subscribes to the manager's state
	// changes, and holding the registry lock while touching a manager is how
	// two locks end up taken in two different orders.
	for _, fn := range hooks {
		fn(mgr)
	}
}

// OnRegister installs a callback run for every manager already registered and
// for each one registered later. Repos opened mid-session with Alt-R create a
// new manager, so anything that must watch every manager has to be told about
// the late ones too — a one-shot loop over the current list would silently miss
// them.
func (r *SessionRegistry) OnRegister(fn func(*Manager)) {
	r.mu.Lock()
	existing := append([]*Manager(nil), r.managers...)
	r.onRegister = append(r.onRegister, fn)
	r.mu.Unlock()

	for _, mgr := range existing {
		fn(mgr)
	}
}

// GetSessionInfo searches all registered managers for the given session ID.
// Returns the session info and the owning manager on success.
func (r *SessionRegistry) GetSessionInfo(id SessionID) (SessionInfo, *Manager, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, mgr := range r.managers {
		if info, ok := mgr.GetSessionInfo(id); ok {
			return info, mgr, true
		}
	}
	return SessionInfo{}, nil, false
}

// findManager returns the first registered manager that owns the given session.
// Must be called with r.mu held (at least RLock).
func (r *SessionRegistry) findManager(id SessionID) *Manager {
	for _, mgr := range r.managers {
		if _, ok := mgr.GetSessionInfo(id); ok {
			return mgr
		}
	}
	return nil
}

// SetSessionIdle finds the owning manager for the session and marks it idle.
func (r *SessionRegistry) SetSessionIdle(id SessionID) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if mgr := r.findManager(id); mgr != nil {
		mgr.SetSessionIdle(id)
	}
}

// SetSessionRunning finds the owning manager and marks the session running.
func (r *SessionRegistry) SetSessionRunning(id SessionID) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if mgr := r.findManager(id); mgr != nil {
		mgr.SetSessionRunning(id)
	}
}

// CapturePaneText finds the owning manager and delegates the capture.
func (r *SessionRegistry) CapturePaneText(id SessionID, n int) ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mgr := r.findManager(id)
	if mgr == nil {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	return mgr.CapturePaneText(id, n)
}

// ResolveTmuxTarget finds the owning manager and resolves the session's tmux
// target, applying the manager's runner-type guard.
func (r *SessionRegistry) ResolveTmuxTarget(id SessionID) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mgr := r.findManager(id)
	if mgr == nil {
		return "", fmt.Errorf("session not found: %s", id)
	}
	return mgr.ResolveTmuxTarget(id)
}

// FindManagerByRepo returns the manager registered for the given repo name.
func (r *SessionRegistry) FindManagerByRepo(repoName string) (*Manager, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, mgr := range r.managers {
		if mgr.RepoName() == repoName {
			return mgr, true
		}
	}
	return nil, false
}

// StopSession finds the owning manager and stops the session.
func (r *SessionRegistry) StopSession(id SessionID) error {
	r.mu.RLock()
	mgr := r.findManager(id)
	r.mu.RUnlock()
	if mgr == nil {
		return fmt.Errorf("session not found: %s", id)
	}
	return mgr.StopSession(id)
}

// GetAllSessions returns sessions from all registered managers.
func (r *SessionRegistry) GetAllSessions() []SessionInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var all []SessionInfo
	for _, mgr := range r.managers {
		all = append(all, mgr.GetAllSessions()...)
	}
	return all
}
