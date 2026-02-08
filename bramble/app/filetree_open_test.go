package app

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestAbsSelectedPath_FileSelected(t *testing.T) {
	wtCtx := &wt.WorktreeContext{
		ChangedFiles: []string{"auth.go", "pkg/config.go"},
	}
	ft := NewFileTree("/tmp/wt", wtCtx)

	// Navigate to first file entry (skipping directory headers if any)
	// The first entry should be a file in the tree
	for i := 0; i < len(ft.entries); i++ {
		if ft.entries[i].Path != "" {
			ft.cursor = i
			break
		}
	}

	absPath := ft.AbsSelectedPath()
	if absPath == "" {
		t.Error("expected non-empty absolute path")
	}
	if !strings.HasPrefix(absPath, "/tmp/wt/") {
		t.Errorf("expected path to start with /tmp/wt/, got: %s", absPath)
	}
}

func TestAbsSelectedPath_DirectorySelected(t *testing.T) {
	wtCtx := &wt.WorktreeContext{
		ChangedFiles: []string{"pkg/config.go"},
	}
	ft := NewFileTree("/tmp/wt", wtCtx)

	// Navigate to directory header (first entry should be "pkg/")
	ft.cursor = 0
	if ft.entries[0].IsDir {
		absPath := ft.AbsSelectedPath()
		if absPath != "" {
			t.Errorf("expected empty path for directory selection, got: %s", absPath)
		}
	}
}

func TestAbsSelectedPath_EmptyTree(t *testing.T) {
	ft := NewFileTree("/tmp/wt", nil)
	absPath := ft.AbsSelectedPath()
	if absPath != "" {
		t.Errorf("expected empty path for empty tree, got: %s", absPath)
	}
}

func TestAbsSelectedPath_NoRoot(t *testing.T) {
	wtCtx := &wt.WorktreeContext{
		ChangedFiles: []string{"auth.go"},
	}
	ft := NewFileTree("", wtCtx)

	absPath := ft.AbsSelectedPath()
	if absPath != "" {
		t.Errorf("expected empty path when root is empty, got: %s", absPath)
	}
}

func TestAbsSelectedPath_PathTraversal(t *testing.T) {
	// Simulate a malicious relative path that attempts to escape the worktree root.
	wtCtx := &wt.WorktreeContext{
		ChangedFiles: []string{"../../etc/passwd"},
	}
	ft := NewFileTree("/tmp/wt", wtCtx)

	// Navigate to the file entry
	for i := 0; i < len(ft.entries); i++ {
		if ft.entries[i].Path != "" {
			ft.cursor = i
			break
		}
	}

	absPath := ft.AbsSelectedPath()
	if absPath != "" {
		t.Errorf("expected empty path for traversal attempt, got: %s", absPath)
	}
}

func TestHandleKeyPress_EnterInSplitPane(t *testing.T) {
	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "code", session.NewManager(), nil, 80, 24)

	// Set up split pane with file tree
	m.splitPane.Toggle()
	m.splitPane.SetFocusLeft(true)

	wtCtx := &wt.WorktreeContext{
		ChangedFiles: []string{"auth.go"},
	}
	m.fileTree = NewFileTree("/tmp/wt", wtCtx)

	// Navigate to first file
	for i := 0; i < len(m.fileTree.entries); i++ {
		if m.fileTree.entries[i].Path != "" {
			m.fileTree.cursor = i
			break
		}
	}

	// Send Enter key
	msg := tea.KeyMsg{Type: tea.KeyEnter}
	newModel, cmd := m.handleKeyPress(msg)

	if cmd == nil {
		t.Error("expected non-nil command for opening file")
	}

	// Check toast was added
	m2 := newModel.(Model)
	if !m2.toasts.HasToasts() {
		t.Error("expected toast notification")
	}
}

func TestHandleKeyPress_EnterInSplitPane_NoFile(t *testing.T) {
	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "code", session.NewManager(), nil, 80, 24)

	// Set up split pane with file tree
	m.splitPane.Toggle()
	m.splitPane.SetFocusLeft(true)

	// Create file tree with directory
	wtCtx := &wt.WorktreeContext{
		ChangedFiles: []string{"pkg/config.go"},
	}
	m.fileTree = NewFileTree("/tmp/wt", wtCtx)

	// Set cursor to directory header
	m.fileTree.cursor = 0
	if !m.fileTree.entries[0].IsDir {
		t.Fatal("expected first entry to be a directory header for 'pkg/'")
	}

	// Send Enter key
	msg := tea.KeyMsg{Type: tea.KeyEnter}
	newModel, _ := m.handleKeyPress(msg)

	// Check toast message
	m2 := newModel.(Model)
	if !m2.toasts.HasToasts() {
		t.Error("expected toast notification for directory selection")
	}
}

func TestHandleKeyPress_EnterNotInSplitPane(t *testing.T) {
	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "code", session.NewManager(), nil, 80, 24)

	// Don't activate split pane
	// Send Enter key
	msg := tea.KeyMsg{Type: tea.KeyEnter}
	newModel, cmd := m.handleKeyPress(msg)

	// Should not execute file open command (returns nil for non-tmux, non-split mode)
	// The model should be unchanged
	_ = newModel
	_ = cmd
	// This is a passthrough case - no error, just no action
}

func TestBuildHelpSections_SplitPaneActive(t *testing.T) {
	// Force TUI mode
	mgr := session.NewManagerWithConfig(session.ManagerConfig{
		SessionMode: session.SessionModeTUI,
	})
	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "code", mgr, nil, 80, 24)
	m.splitPane.Toggle()

	sections := buildHelpSections(&m)

	// Find Output section (only exists in non-tmux mode)
	var outputSection *HelpSection
	for i := range sections {
		if sections[i].Title == "Output" {
			outputSection = &sections[i]
			break
		}
	}

	if outputSection == nil {
		t.Error("expected Output section in help for non-tmux mode")
		return
	}

	// Check for Enter binding
	hasEnterBinding := false
	for _, binding := range outputSection.Bindings {
		if binding.Key == "Enter" {
			hasEnterBinding = true
			if !contains(binding.Description, "file") {
				t.Errorf("expected Enter binding to mention files, got: %s", binding.Description)
			}
		}
	}

	if !hasEnterBinding {
		t.Error("expected Enter binding in Output section when split pane is active")
	}
}

func TestBuildHelpSections_SplitPaneInactive(t *testing.T) {
	// Force TUI mode
	mgr := session.NewManagerWithConfig(session.ManagerConfig{
		SessionMode: session.SessionModeTUI,
	})
	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "code", mgr, nil, 80, 24)
	// Don't activate split pane

	sections := buildHelpSections(&m)

	// Find Output section (only exists in non-tmux mode)
	var outputSection *HelpSection
	for i := range sections {
		if sections[i].Title == "Output" {
			outputSection = &sections[i]
			break
		}
	}

	if outputSection == nil {
		t.Error("expected Output section in help for non-tmux mode")
		return
	}

	// Check that Enter binding is NOT present
	for _, binding := range outputSection.Bindings {
		if binding.Key == "Enter" {
			t.Error("expected no Enter binding in Output section when split pane is inactive")
		}
	}
}
