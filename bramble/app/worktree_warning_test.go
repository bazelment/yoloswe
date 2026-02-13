package app

import (
	"context"
	"testing"

	"github.com/bazelment/yoloswe/bramble/session"
)

func TestWorktreeOpResultWarningShowsToast(t *testing.T) {
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24, nil, nil)

	updated, _ := m.Update(worktreeOpResultMsg{
		branch:   "feature/a",
		messages: []string{"Created worktree feature/a"},
		warning:  "Worktree operation completed, but hook command failed",
	})
	m2 := updated.(Model)

	if !m2.toasts.HasToasts() {
		t.Fatal("expected warning toast")
	}

	foundWarning := false
	for _, toast := range m2.toasts.toasts {
		if toast.Message == "Worktree operation completed, but hook command failed" {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Fatalf("expected warning toast message, got: %+v", m2.toasts.toasts)
	}
}

func TestExtractHookWarningDetectsHookFailure(t *testing.T) {
	got := extractHookWarning([]string{
		"→ Removing worktree feature...",
		"! Post-remove hook failed: exit status 1",
	})
	if got == "" {
		t.Fatal("expected warning when hook failure message is present")
	}
}
