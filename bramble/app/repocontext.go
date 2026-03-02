package app

import (
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/taskrouter"
	"github.com/bazelment/yoloswe/wt"
)

// RepoContext bundles all per-repo state that must be saved/restored when
// switching between opened repositories. The save/load pattern lets update.go
// and view.go keep accessing Model fields directly without per-access
// indirection through a map.
type RepoContext struct {
	repoName             string
	sessionManager       *session.Manager
	taskRouter           *taskrouter.Router
	worktrees            []wt.Worktree
	worktreeStatuses     map[string]*wt.WorktreeStatus
	cachedHistory        []*session.SessionMeta
	historyBranch        string
	sessions             []session.SessionInfo
	worktreeDropdown     *Dropdown
	sessionDropdown      *Dropdown
	viewingSessionID     session.SessionID
	viewingHistoryData   *session.StoredSession
	selectedSessionIndex int
	scrollOffset         int
	scrollPositions      map[session.SessionID]int
}

// saveActiveContext copies per-repo fields from Model into the active RepoContext.
func (m *Model) saveActiveContext() {
	rc, ok := m.repos[m.repoName]
	if !ok {
		return
	}
	rc.repoName = m.repoName
	rc.sessionManager = m.sessionManager
	rc.taskRouter = m.taskRouter
	rc.worktrees = m.worktrees
	rc.worktreeStatuses = m.worktreeStatuses
	rc.cachedHistory = m.cachedHistory
	rc.historyBranch = m.historyBranch
	rc.sessions = m.sessions
	rc.worktreeDropdown = m.worktreeDropdown
	rc.sessionDropdown = m.sessionDropdown
	rc.viewingSessionID = m.viewingSessionID
	rc.viewingHistoryData = m.viewingHistoryData
	rc.selectedSessionIndex = m.selectedSessionIndex
	rc.scrollOffset = m.scrollOffset
	rc.scrollPositions = m.scrollPositions
}

// loadContext restores per-repo fields from a RepoContext into Model.
func (m *Model) loadContext(repoName string) {
	rc, ok := m.repos[repoName]
	if !ok {
		return
	}
	m.repoName = rc.repoName
	m.sessionManager = rc.sessionManager
	m.taskRouter = rc.taskRouter
	m.worktrees = rc.worktrees
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
