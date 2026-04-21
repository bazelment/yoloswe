package app

import (
	"context"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestUpdateWorktreeDropdown_GoneFiltering(t *testing.T) {
	t.Parallel()

	// Use Tmux mode so we can inject a live session via TrackTmuxWindow.
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTmux})
	defer mgr.Close()

	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24, nil, nil, session.ManagerConfig{}, nil)

	m.worktrees = []wt.Worktree{
		{Path: "/tmp/wt/main", Branch: "main", IsGone: false},
		{Path: "/tmp/wt/gone-no-sessions", Branch: "gone-no-sessions", IsGone: true},
		{Path: "/tmp/wt/gone-with-sessions", Branch: "gone-with-sessions", IsGone: true},
	}

	// Inject a live session for the gone-with-sessions worktree.
	_, err := mgr.TrackTmuxWindow("/tmp/wt/gone-with-sessions", "some-task", "@42")
	if err != nil {
		t.Fatalf("TrackTmuxWindow: %v", err)
	}

	m.updateWorktreeDropdown()

	items := m.worktreeDropdown.items

	// "gone-no-sessions" must be hidden.
	for _, item := range items {
		if item.ID == "gone-no-sessions" {
			t.Errorf("gone worktree with no sessions should be hidden from dropdown")
		}
	}

	// "main" must be visible with no "(gone)" badge.
	foundMain := false
	for _, item := range items {
		if item.ID == "main" {
			foundMain = true
			if strings.Contains(item.Badge, "gone") {
				t.Errorf("healthy worktree badge should not contain 'gone', got %q", item.Badge)
			}
		}
	}
	if !foundMain {
		t.Errorf("healthy worktree 'main' missing from dropdown")
	}

	// "gone-with-sessions" must be visible and carry a "(gone)" badge.
	foundGoneWithSessions := false
	for _, item := range items {
		if item.ID == "gone-with-sessions" {
			foundGoneWithSessions = true
			if !strings.Contains(item.Badge, "gone") {
				t.Errorf("gone worktree with sessions should have 'gone' in badge, got %q", item.Badge)
			}
		}
	}
	if !foundGoneWithSessions {
		t.Errorf("gone worktree with live sessions should remain in dropdown")
	}
}

// TestUpdateWorktreeDropdown_GoneWorktreeSkipsGitStatus verifies that a gone
// worktree does not render a git-status-derived subtitle (e.g. "clean").
// GetGitStatus silently ignores errors on missing directories and returns a
// zero WorktreeStatus, which would otherwise render as a misleading green
// "clean" badge next to "(gone)". fetchGitStatuses is expected to skip gone
// worktrees, and updateWorktreeDropdown should not render cached status for
// them either — we simulate a stale cached status here and assert it's ignored.
func TestUpdateWorktreeDropdown_GoneWorktreeSkipsGitStatus(t *testing.T) {
	t.Parallel()

	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTmux})
	defer mgr.Close()

	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24, nil, nil, session.ManagerConfig{}, nil)

	m.worktrees = []wt.Worktree{
		{Path: "/tmp/wt/gone-with-sessions", Branch: "gone-with-sessions", IsGone: true},
	}

	// Simulate a stale cached status from before the worktree went away.
	m.worktreeStatuses = map[string]*wt.WorktreeStatus{
		"gone-with-sessions": {IsDirty: false},
	}

	if _, err := mgr.TrackTmuxWindow("/tmp/wt/gone-with-sessions", "some-task", "@42"); err != nil {
		t.Fatalf("TrackTmuxWindow: %v", err)
	}

	m.updateWorktreeDropdown()

	for _, item := range m.worktreeDropdown.items {
		if item.ID != "gone-with-sessions" {
			continue
		}
		if strings.Contains(item.Subtitle, "clean") {
			t.Errorf("gone worktree subtitle must not contain 'clean' (stale git status), got %q", item.Subtitle)
		}
		if !strings.Contains(item.Subtitle, "sessions") {
			t.Errorf("gone worktree subtitle should surface session count, got %q", item.Subtitle)
		}
	}
}

func TestUpdateWorktreeDropdown_PreservesSelection(t *testing.T) {
	t.Parallel()

	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTmux})
	defer mgr.Close()

	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24, nil, nil, session.ManagerConfig{}, nil)

	m.worktrees = []wt.Worktree{
		{Path: "/tmp/wt/main", Branch: "main", IsGone: false},
		{Path: "/tmp/wt/feature", Branch: "feature", IsGone: false},
		{Path: "/tmp/wt/gone-no-sessions", Branch: "gone-no-sessions", IsGone: true},
	}

	m.updateWorktreeDropdown()
	// Select "feature" (index 1 in the two-item list after filtering)
	m.worktreeDropdown.SelectByID("feature")
	if m.worktreeDropdown.SelectedItem() == nil || m.worktreeDropdown.SelectedItem().ID != "feature" {
		t.Fatal("pre-condition: expected 'feature' to be selected")
	}

	// Refresh with a new list where "gone-no-sessions" is still filtered.
	// Item count is unchanged, but SetItems must not drift the index.
	m.updateWorktreeDropdown()
	if got := m.worktreeDropdown.SelectedItem(); got == nil || got.ID != "feature" {
		gotID := "<nil>"
		if got != nil {
			gotID = got.ID
		}
		t.Errorf("selection after refresh = %q, want %q", gotID, "feature")
	}
}
