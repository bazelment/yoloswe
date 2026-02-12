package app

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bazelment/yoloswe/bramble/session"
)

func TestThemeShortcutOpensSettingsDialog(t *testing.T) {
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24, nil, nil)

	nextModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}})
	m2 := nextModel.(Model)

	if m2.focus != FocusRepoSettings {
		t.Fatalf("focus = %v, want %v", m2.focus, FocusRepoSettings)
	}
	if !m2.repoSettingsDialog.IsVisible() {
		t.Fatal("repo settings dialog should be visible")
	}
}

func TestSettingsShortcutCtrlLOpensSettingsDialog(t *testing.T) {
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24, nil, nil)

	nextModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlL})
	m2 := nextModel.(Model)

	if m2.focus != FocusRepoSettings {
		t.Fatalf("focus = %v, want %v", m2.focus, FocusRepoSettings)
	}
	if !m2.repoSettingsDialog.IsVisible() {
		t.Fatal("repo settings dialog should be visible")
	}
}

func TestSettingsDialogCancelRevertsThemePreview(t *testing.T) {
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24, nil, nil)
	orig := m.styles.Palette.Name

	nextModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}})
	m2 := nextModel.(Model)

	previewModel, _ := m2.handleRepoSettingsDialog(tea.KeyMsg{Type: tea.KeyRight})
	m3 := previewModel.(Model)
	if m3.styles.Palette.Name == orig {
		t.Fatal("theme preview should change after right arrow in settings dialog")
	}

	cancelModel, _ := m3.handleRepoSettingsDialog(tea.KeyMsg{Type: tea.KeyEsc})
	m4 := cancelModel.(Model)
	if m4.styles.Palette.Name != orig {
		t.Fatalf("theme after cancel = %q, want %q", m4.styles.Palette.Name, orig)
	}
	if m4.focus != FocusOutput {
		t.Fatalf("focus after cancel = %v, want %v", m4.focus, FocusOutput)
	}
}
