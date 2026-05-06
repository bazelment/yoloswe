package app

import (
	"sync"

	"github.com/fsnotify/fsnotify"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/taskrouter"
	"github.com/bazelment/yoloswe/wt"
)

// RepoContext bundles all per-repo state that must be saved/restored when
// switching between opened repositories. The save/load pattern lets update.go
// and view.go keep accessing Model fields directly without per-access
// indirection through a map.
type RepoContext struct {
	worktreeDropdown     *Dropdown
	sessionManager       *session.Manager
	taskRouter           *taskrouter.Router
	fsWatcher            *fsnotify.Watcher
	scrollPositions      map[session.SessionID]int
	worktreeStatuses     map[string]*wt.WorktreeStatus
	dirtyWorktrees       map[string]struct{}
	watchedGitPaths      map[string]string
	viewingHistoryData   *session.StoredSession
	sessionDropdown      *Dropdown
	historyBranch        string
	viewingSessionID     session.SessionID
	sessions             []session.SessionInfo
	cachedHistory        []*session.SessionMeta
	worktrees            []wt.Worktree
	selectedSessionIndex int
	scrollOffset         int
	worktreesLoaded      bool
	dirtyWorktreesMu     sync.Mutex
}

func (rc *RepoContext) markDirtyWorktree(worktreePath string) {
	if worktreePath == "" {
		return
	}
	rc.dirtyWorktreesMu.Lock()
	defer rc.dirtyWorktreesMu.Unlock()
	if rc.dirtyWorktrees == nil {
		rc.dirtyWorktrees = make(map[string]struct{})
	}
	rc.dirtyWorktrees[worktreePath] = struct{}{}
}

func (rc *RepoContext) drainDirtyWorktrees() []string {
	rc.dirtyWorktreesMu.Lock()
	defer rc.dirtyWorktreesMu.Unlock()
	if len(rc.dirtyWorktrees) == 0 {
		return nil
	}
	paths := make([]string, 0, len(rc.dirtyWorktrees))
	for path := range rc.dirtyWorktrees {
		paths = append(paths, path)
	}
	rc.dirtyWorktrees = make(map[string]struct{})
	return paths
}

// saveActiveContext copies per-repo fields from Model into the active RepoContext.
func (m *Model) saveActiveContext() {
	rc, ok := m.repos[m.repoName]
	if !ok {
		return
	}
	rc.sessionManager = m.sessionManager
	rc.taskRouter = m.taskRouter
	rc.worktrees = m.worktrees
	rc.worktreeStatuses = m.worktreeStatuses
	rc.cachedHistory = m.cachedHistory
	rc.historyBranch = m.historyBranch
	rc.sessions = m.sessions
	rc.worktreeDropdown = m.worktreeDropdown
	rc.worktreesLoaded = m.worktreesLoaded
	rc.sessionDropdown = m.sessionDropdown
	rc.viewingSessionID = m.viewingSessionID
	rc.viewingHistoryData = m.viewingHistoryData
	rc.selectedSessionIndex = m.selectedSessionIndex
	rc.scrollOffset = m.scrollOffset
	rc.scrollPositions = m.scrollPositions
}

// managerForSession returns the session manager that owns the given session.
// When the session belongs to a different repo (multi-repo support) the
// relevant manager is retrieved from m.repos; otherwise m.sessionManager is
// returned as the default.
func (m *Model) managerForSession(sess *session.SessionInfo) *session.Manager {
	if sess != nil && sess.RepoName != "" {
		if rc, ok := m.repos[sess.RepoName]; ok && rc.sessionManager != nil {
			return rc.sessionManager
		}
	}
	return m.sessionManager
}

// loadContext restores per-repo fields from a RepoContext into Model.
func (m *Model) loadContext(repoName string) {
	rc, ok := m.repos[repoName]
	if !ok {
		return
	}
	m.repoName = repoName
	m.sessionManager = rc.sessionManager
	m.taskRouter = rc.taskRouter
	m.worktrees = rc.worktrees
	m.worktreesLoaded = rc.worktreesLoaded
	m.worktreeStatuses = rc.worktreeStatuses
	m.cachedHistory = rc.cachedHistory
	m.historyBranch = rc.historyBranch
	m.sessions = rc.sessions
	m.worktreeDropdown = rc.worktreeDropdown
	m.sessionDropdown = rc.sessionDropdown
	m.viewingSessionID = rc.viewingSessionID
	m.viewingHistoryData = rc.viewingHistoryData
	m.selectedSessionIndex = rc.selectedSessionIndex
	m.scrollOffset = rc.scrollOffset
	m.scrollPositions = rc.scrollPositions
}
