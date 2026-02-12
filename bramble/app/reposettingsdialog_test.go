package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestRepoSettingsDialogRoundTrip(t *testing.T) {
	d := NewRepoSettingsDialog()
	d.Show("repo-a", RepoSettings{
		OnWorktreeCreate: []string{"npm ci", "go test ./..."},
		OnWorktreeDelete: []string{"rm -rf .cache"},
	}, "dark", 100, 40, lipgloss.Color("245"))

	got := d.RepoSettings()
	if len(got.OnWorktreeCreate) != 2 {
		t.Fatalf("len(OnWorktreeCreate) = %d, want 2", len(got.OnWorktreeCreate))
	}
	if got.OnWorktreeCreate[0] != "npm ci" || got.OnWorktreeCreate[1] != "go test ./..." {
		t.Fatalf("OnWorktreeCreate = %v", got.OnWorktreeCreate)
	}
	if len(got.OnWorktreeDelete) != 1 || got.OnWorktreeDelete[0] != "rm -rf .cache" {
		t.Fatalf("OnWorktreeDelete = %v", got.OnWorktreeDelete)
	}
}

func TestRepoSettingsDialogParseCommandLines(t *testing.T) {
	d := NewRepoSettingsDialog()
	d.Show("repo-a", RepoSettings{}, "dark", 100, 40, lipgloss.Color("245"))
	d.createInput.SetValue("  npm ci \n\n go test ./... \n ")
	d.deleteInput.SetValue(" \n rm -rf .cache \n")

	got := d.RepoSettings()
	if len(got.OnWorktreeCreate) != 2 {
		t.Fatalf("len(OnWorktreeCreate) = %d, want 2", len(got.OnWorktreeCreate))
	}
	if got.OnWorktreeCreate[0] != "npm ci" || got.OnWorktreeCreate[1] != "go test ./..." {
		t.Fatalf("OnWorktreeCreate = %v", got.OnWorktreeCreate)
	}
	if len(got.OnWorktreeDelete) != 1 || got.OnWorktreeDelete[0] != "rm -rf .cache" {
		t.Fatalf("OnWorktreeDelete = %v", got.OnWorktreeDelete)
	}
}

func TestRepoSettingsDialogSaveShortcut(t *testing.T) {
	d := NewRepoSettingsDialog()
	d.Show("repo-a", RepoSettings{}, "dark", 100, 40, lipgloss.Color("245"))

	_, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab})
	_, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab})
	action, _ := d.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if action != RepoSettingsActionSave {
		t.Fatalf("action = %v, want RepoSettingsActionSave", action)
	}
}

func TestRepoSettingsDialogThemeSelection(t *testing.T) {
	d := NewRepoSettingsDialog()
	d.Show("repo-a", RepoSettings{}, "dark", 100, 40, lipgloss.Color("245"))

	original := d.SelectedTheme().Name
	_, _ = d.Update(tea.KeyMsg{Type: tea.KeyRight})
	next := d.SelectedTheme().Name
	if next == original {
		t.Fatalf("expected theme to change from %q", original)
	}
	if d.OriginalThemeName() != "dark" {
		t.Fatalf("OriginalThemeName() = %q, want %q", d.OriginalThemeName(), "dark")
	}
}
