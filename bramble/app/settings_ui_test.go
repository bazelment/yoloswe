package app

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/bazelment/yoloswe/bramble/session"
)

func TestThemeShortcutOpensSettingsDialog(t *testing.T) {
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24, nil, nil, session.ManagerConfig{}, nil)

	nextModel, _ := m.handleKeyPress(keyPress('T'))
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

	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24, nil, nil, session.ManagerConfig{}, nil)

	nextModel, _ := m.handleKeyPress(tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
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

	m := NewModel(context.Background(), "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24, nil, nil, session.ManagerConfig{}, nil)
	orig := m.styles.Palette.Name

	nextModel, _ := m.handleKeyPress(keyPress('T'))
	m2 := nextModel.(Model)

	previewModel, _ := m2.handleRepoSettingsDialog(specialKey(tea.KeyRight))
	m3 := previewModel.(Model)
	if m3.styles.Palette.Name == orig {
		t.Fatal("theme preview should change after right arrow in settings dialog")
	}

	cancelModel, _ := m3.handleRepoSettingsDialog(specialKey(tea.KeyEsc))
	m4 := cancelModel.(Model)
	if m4.styles.Palette.Name != orig {
		t.Fatalf("theme after cancel = %q, want %q", m4.styles.Palette.Name, orig)
	}
	if m4.focus != FocusOutput {
		t.Fatalf("focus after cancel = %v, want %v", m4.focus, FocusOutput)
	}
}
