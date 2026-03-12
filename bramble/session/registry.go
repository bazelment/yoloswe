package session

import (
	"fmt"
	"sync"
)

// SessionRegistry aggregates multiple Manager instances so that IPC handlers
// can look up sessions across all repos (initial + those opened via Alt-R).
type SessionRegistry struct { //nolint:govet // fieldalignment: readability over packing
	mu       sync.RWMutex
	managers []*Manager
}

// NewSessionRegistry creates an empty registry.
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{}
}

// Register adds a manager to the registry. Safe for concurrent use.
func (r *SessionRegistry) Register(mgr *Manager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.managers = append(r.managers, mgr)
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
