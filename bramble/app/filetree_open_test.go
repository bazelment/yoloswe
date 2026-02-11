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
	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "code", session.NewManager(), nil, nil, 80, 24)

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
	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "code", session.NewManager(), nil, nil, 80, 24)

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
	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "code", session.NewManager(), nil, nil, 80, 24)

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
	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "code", mgr, nil, nil, 80, 24)
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

func TestHandleKeyPress_F2TogglesSplitInTmuxMode(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	// Initially not split
	if m.splitPane.IsSplit() {
		t.Fatal("expected split pane to be inactive initially")
	}

	// Press F2
	msg := tea.KeyMsg{Type: tea.KeyF2}
	newModel, _ := m.handleKeyPress(msg)
	m2 := newModel.(Model)

	if !m2.splitPane.IsSplit() {
		t.Error("expected split pane to be active after F2")
	}
	if !m2.splitPane.FocusLeft() {
		t.Error("expected focus on file tree (left) after F2")
	}
}

func TestHandleKeyPress_UpDownNavigatesFileTreeInTmuxSplit(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	// Set up split pane with file tree focused
	m.splitPane.Toggle()
	m.splitPane.SetFocusLeft(true)
	m.fileTree = NewFileTree("/tmp/wt/main", &wt.WorktreeContext{
		ChangedFiles: []string{"a.go", "b.go", "c.go"},
	})
	m.fileTree.cursor = 0

	startCursor := m.fileTree.cursor

	// Press down
	msg := tea.KeyMsg{Type: tea.KeyDown}
	newModel, _ := m.handleKeyPress(msg)
	m2 := newModel.(Model)

	if m2.fileTree.cursor != startCursor+1 {
		t.Errorf("expected cursor to move down, got %d (was %d)", m2.fileTree.cursor, startCursor)
	}

	// Press up
	msgUp := tea.KeyMsg{Type: tea.KeyUp}
	newModel2, _ := m2.handleKeyPress(msgUp)
	m3 := newModel2.(Model)

	if m3.fileTree.cursor != startCursor {
		t.Errorf("expected cursor to move back up, got %d (was %d)", m3.fileTree.cursor, startCursor)
	}
}

func TestHandleKeyPress_UpDownNavigatesSessionListInTmuxSplitRightFocus(t *testing.T) {
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTmux})
	t.Cleanup(func() { mgr.Close() })

	worktrees := []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}
	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "", mgr, nil, worktrees, 80, 24)

	// Start sessions so navigation has items
	_, _ = mgr.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "session A", "")
	_, _ = mgr.StartSession(session.SessionTypeBuilder, "/tmp/wt/main", "session B", "")

	// Set up split pane with right focus (session list)
	m.splitPane.Toggle()
	m.splitPane.SetFocusLeft(false)
	m.selectedSessionIndex = 0
	m.worktreeDropdown.SelectIndex(0)

	// Press down — should navigate session list, not file tree
	msg := tea.KeyMsg{Type: tea.KeyDown}
	newModel, _ := m.handleKeyPress(msg)
	m2 := newModel.(Model)

	if m2.selectedSessionIndex != 1 {
		t.Errorf("expected session index to increment to 1, got %d", m2.selectedSessionIndex)
	}
}

func TestBuildHelpSections_TmuxModeIncludesF2(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	sections := buildHelpSections(&m)

	var navSection *HelpSection
	for i := range sections {
		if sections[i].Title == "Navigation" {
			navSection = &sections[i]
			break
		}
	}

	if navSection == nil {
		t.Fatal("expected Navigation section in help")
	}

	hasF2 := false
	hasTab := false
	for _, b := range navSection.Bindings {
		if b.Key == "F2" {
			hasF2 = true
		}
		if b.Key == "Tab" {
			hasTab = true
		}
	}

	if !hasF2 {
		t.Error("expected F2 binding in tmux mode help")
	}
	if !hasTab {
		t.Error("expected Tab binding in tmux mode help")
	}
}

func TestHandleKeyPress_TabTogglesFocusInTmuxSplit(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	// Activate split pane with left focus
	m.splitPane.Toggle()
	m.splitPane.SetFocusLeft(true)

	if !m.splitPane.FocusLeft() {
		t.Fatal("expected focus on left pane initially")
	}

	// Press Tab
	msg := tea.KeyMsg{Type: tea.KeyTab}
	newModel, _ := m.handleKeyPress(msg)
	m2 := newModel.(Model)

	if m2.splitPane.FocusLeft() {
		t.Error("expected focus to switch to right pane after Tab")
	}

	// Press Tab again
	newModel2, _ := m2.handleKeyPress(msg)
	m3 := newModel2.(Model)

	if !m3.splitPane.FocusLeft() {
		t.Error("expected focus to switch back to left pane after second Tab")
	}
}

func TestHandleKeyPress_EnterOpensFileInTmuxSplit(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.editor = "code"

	// Activate split pane with left focus
	m.splitPane.Toggle()
	m.splitPane.SetFocusLeft(true)

	// Set up file tree with a file
	m.fileTree = NewFileTree("/tmp/wt/main", &wt.WorktreeContext{
		ChangedFiles: []string{"auth.go"},
	})
	for i := 0; i < len(m.fileTree.entries); i++ {
		if m.fileTree.entries[i].Path != "" {
			m.fileTree.cursor = i
			break
		}
	}

	// Press Enter — should open file, not switch tmux window
	msg := tea.KeyMsg{Type: tea.KeyEnter}
	newModel, cmd := m.handleKeyPress(msg)
	m2 := newModel.(Model)

	if cmd == nil {
		t.Error("expected non-nil command for opening file")
	}
	if !m2.toasts.HasToasts() {
		t.Error("expected toast notification for file open")
	}
}

func TestBuildHelpSections_TmuxSplitShowsEnterFileBinding(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	// Activate split pane
	m.splitPane.Toggle()

	sections := buildHelpSections(&m)

	var sessionListSection *HelpSection
	for i := range sections {
		if sections[i].Title == "Session List" {
			sessionListSection = &sections[i]
			break
		}
	}

	if sessionListSection == nil {
		t.Fatal("expected Session List section in tmux mode help")
	}

	var enterBindings []string
	for _, b := range sessionListSection.Bindings {
		if b.Key == "Enter" {
			enterBindings = append(enterBindings, b.Description)
		}
	}

	if len(enterBindings) != 1 {
		t.Errorf("expected exactly 1 Enter binding in tmux split help, got %d: %v", len(enterBindings), enterBindings)
	}
	if len(enterBindings) > 0 && !contains(enterBindings[0], "file") {
		t.Errorf("expected Enter binding to mention file, got: %s", enterBindings[0])
	}
}

func TestBuildHelpSections_SplitPaneInactive(t *testing.T) {
	// Force TUI mode
	mgr := session.NewManagerWithConfig(session.ManagerConfig{
		SessionMode: session.SessionModeTUI,
	})
	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "code", mgr, nil, nil, 80, 24)
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
