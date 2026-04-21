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

	// "main" must be visible with no "(gone)" decoration.
	foundMain := false
	for _, item := range items {
		if item.ID == "main" {
			foundMain = true
			if strings.Contains(item.Label, "gone") {
				t.Errorf("healthy worktree label should not contain 'gone', got %q", item.Label)
			}
		}
	}
	if !foundMain {
		t.Errorf("healthy worktree 'main' missing from dropdown")
	}

	// "gone-with-sessions" must be visible and labeled with "(gone)".
	foundGoneWithSessions := false
	for _, item := range items {
		if item.ID == "gone-with-sessions" {
			foundGoneWithSessions = true
			if !strings.Contains(item.Label, "gone") {
				t.Errorf("gone worktree with sessions should have 'gone' in label, got %q", item.Label)
			}
		}
	}
	if !foundGoneWithSessions {
		t.Errorf("gone worktree with live sessions should remain in dropdown")
	}
}
