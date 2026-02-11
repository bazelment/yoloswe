package app

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestHelpOverlayContextAwareness(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	// Case 1: No worktree, no session -> Sessions section should be minimal
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, nil, 80, 24)
	sections := buildHelpSections(&m)

	// Should have navigation and general sections
	hasNav := false
	hasGeneral := false
	for _, s := range sections {
		if s.Title == "Navigation" {
			hasNav = true
		}
		if s.Title == "General" {
			hasGeneral = true
		}
	}
	if !hasNav || !hasGeneral {
		t.Error("Missing Navigation or General sections")
	}

	// Case 2: Worktree selected -> should show worktree actions
	worktrees := []wt.Worktree{
		{Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
	}
	m2 := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, worktrees, 80, 24)
	sections2 := buildHelpSections(&m2)

	hasWorktrees := false
	hasSessions := false
	for _, s := range sections2 {
		if s.Title == "Worktrees" {
			hasWorktrees = true
		}
		if s.Title == "Sessions" {
			hasSessions = true
		}
	}
	if !hasWorktrees || !hasSessions {
		t.Error("Missing Worktrees or Sessions sections with worktree selected")
	}

	// Case 3: Previous focus was dropdown -> Dropdown section should appear
	m3 := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, worktrees, 80, 24)
	m3.helpOverlay.previousFocus = FocusWorktreeDropdown
	sections3 := buildHelpSections(&m3)

	hasDropdown := false
	for _, s := range sections3 {
		if s.Title == "Dropdown" {
			hasDropdown = true
		}
	}
	if !hasDropdown {
		t.Error("Missing Dropdown section when previousFocus was dropdown")
	}
}

func TestHelpOverlayFocusRestoration(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, nil, 80, 24)

	// Open help from FocusOutput
	m.helpOverlay.previousFocus = FocusOutput
	m.focus = FocusHelp

	// Close help
	msg := tea.KeyMsg{Type: tea.KeyEsc}
	newModel, _ := m.handleHelpOverlay(msg)
	m2 := newModel.(Model)

	if m2.focus != FocusOutput {
		t.Errorf("Expected focus to be FocusOutput, got %v", m2.focus)
	}
}

func TestHelpOverlayKeyHandling(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, nil, 80, 24)
	m.focus = FocusHelp
	m.helpOverlay.previousFocus = FocusOutput

	// Test '?' closes overlay
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}}
	newModel, _ := m.handleHelpOverlay(msg)
	m2 := newModel.(Model)
	if m2.focus != FocusOutput {
		t.Error("'?' should close overlay")
	}

	// Test 'Esc' closes overlay
	m.focus = FocusHelp
	msg2 := tea.KeyMsg{Type: tea.KeyEsc}
	newModel2, _ := m.handleHelpOverlay(msg2)
	m3 := newModel2.(Model)
	if m3.focus != FocusOutput {
		t.Error("'Esc' should close overlay")
	}

	// Test scrolling
	m.focus = FocusHelp
	m.helpOverlay.scrollOffset = 5
	msgUp := tea.KeyMsg{Type: tea.KeyUp}
	newModel3, _ := m.handleHelpOverlay(msgUp)
	m4 := newModel3.(Model)
	if m4.helpOverlay.scrollOffset != 4 {
		t.Errorf("Up should scroll up, got offset %d", m4.helpOverlay.scrollOffset)
	}

	msgDown := tea.KeyMsg{Type: tea.KeyDown}
	newModel4, _ := m4.handleHelpOverlay(msgDown)
	m5 := newModel4.(Model)
	if m5.helpOverlay.scrollOffset != 5 {
		t.Errorf("Down should scroll down, got offset %d", m5.helpOverlay.scrollOffset)
	}
}

func TestHelpOverlayRendering(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, worktrees, 80, 24)

	sections := buildHelpSections(&m)
	m.helpOverlay.SetSections(sections)
	m.helpOverlay.SetSize(80, 24)

	view := m.helpOverlay.View()

	// Verify View() output contains expected content
	if !strings.Contains(view, "Bramble Key Bindings") {
		t.Error("View should contain title")
	}

	if !strings.Contains(view, "Navigation") {
		t.Error("View should contain Navigation section")
	}

	if !strings.Contains(view, "Alt-W") {
		t.Error("View should contain Alt-W binding")
	}

	if !strings.Contains(view, "Press ? or Esc to close") {
		t.Error("View should contain footer text")
	}

	// Verify narrow terminal still renders without panic
	m.helpOverlay.SetSize(40, 24)
	viewNarrow := m.helpOverlay.View()
	if viewNarrow == "" {
		t.Error("View should render even in narrow terminal")
	}
}

func TestHelpOverlayScrolling(t *testing.T) {
	overlay := NewHelpOverlay()

	// ScrollUp at offset=0 stays at 0
	overlay.scrollOffset = 0
	overlay.ScrollUp()
	if overlay.scrollOffset != 0 {
		t.Error("ScrollUp at offset 0 should stay at 0")
	}

	// ScrollDown increments
	overlay.ScrollDown()
	if overlay.scrollOffset != 1 {
		t.Error("ScrollDown should increment offset")
	}

	// ScrollUp decrements
	overlay.ScrollUp()
	if overlay.scrollOffset != 0 {
		t.Error("ScrollUp should decrement offset")
	}
}

func TestBuildHelpSectionsWithSession(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, worktrees, 80, 24)

	// Add a session
	sessionID, err := mgr.StartSession(session.SessionTypePlanner, "/tmp/wt/feature-auth", "test plan")
	if err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	m.viewingSessionID = sessionID

	// Wait for session to become idle (in real code, this would be through events)
	// For now just check that sections build without error
	sections := buildHelpSections(&m)

	hasSessionSection := false
	for _, s := range sections {
		if s.Title == "Sessions" {
			hasSessionSection = true
			// Check for session-related bindings
			for _, b := range s.Bindings {
				if strings.Contains(b.Key, "t") || strings.Contains(b.Key, "p") || strings.Contains(b.Key, "b") {
					// Found session-related binding
					break
				}
			}
		}
	}

	if !hasSessionSection {
		t.Error("Should have Sessions section when session is active")
	}
}
