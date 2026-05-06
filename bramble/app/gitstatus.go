package app

import (
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/fsnotify/fsnotify"

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

func makeGitDirtyCallback(out chan<- gitWorktreeInvalidation) func(repoName, worktreePath string) {
	return func(repoName, worktreePath string) {
		select {
		case out <- gitWorktreeInvalidation{repoName: repoName, worktreePath: worktreePath}:
		default:
		}
	}
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

func (m *Model) applyBatchWorktreeStatuses(msg batchWorktreeStatusMsg) {
	activeChanged := false
	for _, statusMsg := range msg.statuses {
		if statusMsg.status == nil {
			continue
		}
		if statusMsg.repoName != m.repoName {
			if rc, ok := m.repos[statusMsg.repoName]; ok {
				rc.worktreeStatuses = mergeGitStatus(rc.worktreeStatuses, statusMsg.branch, statusMsg.status)
			}
			continue
		}
		m.worktreeStatuses = mergeGitStatus(m.worktreeStatuses, statusMsg.branch, statusMsg.status)
		activeChanged = true
	}
	if activeChanged {
		m.updateWorktreeDropdown()
	}
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

func (m Model) shouldRefreshWorktree(repoName string, worktree wt.Worktree, scope RefreshScope, activeWorktrees map[string]struct{}) bool {
	if worktree.IsGone {
		return false
	}
	if scope == RefreshAll {
		return true
	}
	if m.repoName == repoName && m.worktreeDropdown != nil && m.worktreeDropdown.IsOpen() {
		return true
	}
	_, ok := activeWorktrees[worktree.Path]
	return ok
}

func (m Model) activeSessionWorktreePaths(repoName string) map[string]struct{} {
	rc, ok := m.repos[repoName]
	if !ok || rc.sessionManager == nil {
		return nil
	}
	return rc.sessionManager.ActiveWorktreePaths()
}

func (m *Model) syncGitWatcher(repoName string) {
	rc, ok := m.repos[repoName]
	if !ok || m.sharedGitInvalidates == nil {
		return
	}
	manager := wt.NewManager(m.wtRoot, repoName)
	_ = rc.syncGitWatcher(repoName, manager.BareDir(), m.sharedGitInvalidates)
}

func (rc *RepoContext) syncGitWatcher(repoName, bareDir string, out chan<- gitWorktreeInvalidation) error {
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
		for _, path := range gitStatusWatchPaths(worktree, bareDir) {
			next[path] = worktree.Path
		}
	}

	rc.dirtyWorktreesMu.Lock()
	watched := make(map[string]string, len(rc.watchedGitPaths))
	for path, worktreePath := range rc.watchedGitPaths {
		watched[path] = worktreePath
	}
	rc.dirtyWorktreesMu.Unlock()

	added := make(map[string]string)
	for path, worktreePath := range next {
		if _, alreadyWatched := watched[path]; alreadyWatched {
			continue
		}
		if err := rc.fsWatcher.Add(path); err == nil {
			added[path] = worktreePath
		}
	}

	var removed []string
	for path := range watched {
		if _, stillWatched := next[path]; stillWatched {
			continue
		}
		_ = rc.fsWatcher.Remove(path)
		removed = append(removed, path)
	}

	rc.dirtyWorktreesMu.Lock()
	for path, worktreePath := range added {
		rc.watchedGitPaths[path] = worktreePath
	}
	for _, path := range removed {
		delete(rc.watchedGitPaths, path)
	}
	rc.dirtyWorktreesMu.Unlock()
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

func gitStatusWatchPaths(worktree wt.Worktree, bareDir string) []string {
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
		if bareDir != "" && filepath.Base(filepath.Dir(gitDir)) == "worktrees" {
			paths = append(paths, filepath.Join(bareDir, "refs", "heads", worktree.Branch))
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
