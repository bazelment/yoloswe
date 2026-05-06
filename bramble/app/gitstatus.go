package app

import (
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/fsnotify/fsnotify"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func (m Model) listenForGitInvalidations() tea.Cmd {
	ch := m.sharedGitInvalidates
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		return <-ch
	}
}

func (m *Model) markWorktreeDirty(repoName, worktreePath string) {
	rc, ok := m.repos[repoName]
	if !ok {
		return
	}
	rc.markDirtyWorktree(worktreePath)
}

func (m *Model) applySingleWorktreeStatus(msg singleWorktreeStatusMsg) {
	if msg.status == nil {
		return
	}
	if msg.repoName != m.repoName {
		if rc, ok := m.repos[msg.repoName]; ok {
			rc.worktreeStatuses = mergeGitStatus(rc.worktreeStatuses, msg.branch, msg.status)
		}
		return
	}
	m.worktreeStatuses = mergeGitStatus(m.worktreeStatuses, msg.branch, msg.status)
	m.updateWorktreeDropdown()
}

func mergeGitStatus(statuses map[string]*wt.WorktreeStatus, branch string, status *wt.WorktreeStatus) map[string]*wt.WorktreeStatus {
	if statuses == nil {
		statuses = make(map[string]*wt.WorktreeStatus)
	}
	existing := statuses[branch]
	if existing == nil {
		statuses[branch] = status
		return statuses
	}
	existing.IsDirty = status.IsDirty
	existing.Ahead = status.Ahead
	existing.Behind = status.Behind
	existing.LastCommitTime = status.LastCommitTime
	existing.LastCommitMsg = status.LastCommitMsg
	existing.Worktree = status.Worktree
	return statuses
}

func (m Model) fetchDirtyGitStatuses() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.repos))
	for repoName, rc := range m.repos {
		paths := rc.drainDirtyWorktrees()
		if len(paths) == 0 {
			continue
		}
		worktrees := worktreesByPath(rc.worktrees, paths)
		if len(worktrees) == 0 {
			continue
		}
		cmds = append(cmds, m.fetchRepoGitStatuses(repoName, worktrees, RefreshActiveOnly))
	}
	return tea.Batch(cmds...)
}

func worktreesByPath(worktrees []wt.Worktree, paths []string) []wt.Worktree {
	pathSet := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		pathSet[path] = struct{}{}
	}
	var out []wt.Worktree
	for _, w := range worktrees {
		if _, ok := pathSet[w.Path]; ok {
			out = append(out, w)
		}
	}
	return out
}

func (m Model) fetchAllOpenedGitStatuses(scope RefreshScope) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.openedRepos))
	for _, repoName := range m.openedRepos {
		rc, ok := m.repos[repoName]
		if !ok {
			continue
		}
		cmds = append(cmds, m.fetchRepoGitStatuses(repoName, rc.worktrees, scope))
	}
	return tea.Batch(cmds...)
}

func (m Model) shouldRefreshWorktree(repoName string, worktree wt.Worktree, scope RefreshScope) bool {
	if worktree.IsGone {
		return false
	}
	if scope == RefreshAll {
		return true
	}
	if m.repoName == repoName && m.worktreeDropdown != nil && m.worktreeDropdown.IsOpen() {
		return true
	}
	return m.repoHasActiveSessionForWorktree(repoName, worktree.Path)
}

func (m Model) repoHasActiveSessionForWorktree(repoName, worktreePath string) bool {
	rc, ok := m.repos[repoName]
	if !ok || rc.sessionManager == nil {
		return false
	}
	sessions := rc.sessionManager.GetSessionsForWorktree(worktreePath)
	for i := range sessions {
		switch sessions[i].Status {
		case session.StatusPending, session.StatusRunning, session.StatusIdle:
			return true
		}
	}
	return false
}

func (m *Model) syncGitWatcher(repoName string) {
	rc, ok := m.repos[repoName]
	if !ok || m.sharedGitInvalidates == nil {
		return
	}
	_ = rc.syncGitWatcher(repoName, m.sharedGitInvalidates)
}

func (rc *RepoContext) syncGitWatcher(repoName string, out chan<- gitWorktreeInvalidation) error {
	if rc.fsWatcher == nil {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return err
		}
		rc.fsWatcher = watcher
		rc.watchedGitPaths = make(map[string]string)
		go rc.forwardGitWatcherEvents(repoName, out)
	}

	next := make(map[string]string)
	for _, worktree := range rc.worktrees {
		if worktree.IsGone {
			continue
		}
		for _, path := range gitStatusWatchPaths(worktree) {
			next[path] = worktree.Path
			rc.dirtyWorktreesMu.Lock()
			_, alreadyWatched := rc.watchedGitPaths[path]
			rc.dirtyWorktreesMu.Unlock()
			if alreadyWatched {
				continue
			}
			if err := rc.fsWatcher.Add(path); err == nil {
				rc.dirtyWorktreesMu.Lock()
				rc.watchedGitPaths[path] = worktree.Path
				rc.dirtyWorktreesMu.Unlock()
			}
		}
	}
	rc.dirtyWorktreesMu.Lock()
	watched := make([]string, 0, len(rc.watchedGitPaths))
	for path := range rc.watchedGitPaths {
		watched = append(watched, path)
	}
	rc.dirtyWorktreesMu.Unlock()
	for _, path := range watched {
		if _, ok := next[path]; ok {
			continue
		}
		_ = rc.fsWatcher.Remove(path)
		rc.dirtyWorktreesMu.Lock()
		delete(rc.watchedGitPaths, path)
		rc.dirtyWorktreesMu.Unlock()
	}
	return nil
}

func (rc *RepoContext) forwardGitWatcherEvents(repoName string, out chan<- gitWorktreeInvalidation) {
	for {
		select {
		case event, ok := <-rc.fsWatcher.Events:
			if !ok {
				return
			}
			rc.dirtyWorktreesMu.Lock()
			worktreePath := rc.watchedGitPaths[event.Name]
			rc.dirtyWorktreesMu.Unlock()
			if worktreePath == "" {
				continue
			}
			rc.markDirtyWorktree(worktreePath)
			select {
			case out <- gitWorktreeInvalidation{repoName: repoName, worktreePath: worktreePath}:
			default:
			}
		case _, ok := <-rc.fsWatcher.Errors:
			if !ok {
				return
			}
		}
	}
}

func gitStatusWatchPaths(worktree wt.Worktree) []string {
	gitDir := filepath.Join(worktree.Path, ".git")
	if data, err := os.ReadFile(gitDir); err == nil {
		if path, ok := parseGitDirFile(string(data), worktree.Path); ok {
			gitDir = path
		}
	}
	paths := []string{
		filepath.Join(gitDir, "HEAD"),
		filepath.Join(gitDir, "index"),
	}
	if worktree.Branch != "" && !worktree.IsDetached {
		paths = append(paths, filepath.Join(gitDir, "refs", "heads", worktree.Branch))
		if common := filepath.Clean(filepath.Join(gitDir, "..", "..")); filepath.Base(filepath.Dir(gitDir)) == "worktrees" {
			paths = append(paths, filepath.Join(common, "refs", "heads", worktree.Branch))
		}
	}
	out := paths[:0]
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}

func parseGitDirFile(contents, worktreePath string) (string, bool) {
	contents = strings.TrimSpace(contents)
	const prefix = "gitdir:"
	if !strings.HasPrefix(contents, prefix) {
		return "", false
	}
	path := strings.TrimSpace(strings.TrimPrefix(contents, prefix))
	if path == "" {
		return "", false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(worktreePath, path)
	}
	return filepath.Clean(path), true
}
