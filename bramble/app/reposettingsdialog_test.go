package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestRepoSettingsDialogRoundTrip(t *testing.T) {
	d := NewRepoSettingsDialog()
	d.Show("repo-a", RepoSettings{
		OnWorktreeCreate: []string{"npm ci", "go test ./..."},
		OnWorktreeDelete: []string{"rm -rf .cache"},
	}, "dark", 100, 40, lipgloss.Color("245"), nil, nil)

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
	d.Show("repo-a", RepoSettings{}, "dark", 100, 40, lipgloss.Color("245"), nil, nil)
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
	d.Show("repo-a", RepoSettings{}, "dark", 100, 40, lipgloss.Color("245"), nil, nil)

	_, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab}) // Theme → Providers
	_, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab}) // Providers → Create
	_, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab}) // Create → Delete
	_, _ = d.Update(tea.KeyMsg{Type: tea.KeyTab}) // Delete → Save
	action, _ := d.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if action != RepoSettingsActionSave {
		t.Fatalf("action = %v, want RepoSettingsActionSave", action)
	}
}

func TestRepoSettingsDialogThemeSelection(t *testing.T) {
	d := NewRepoSettingsDialog()
	d.Show("repo-a", RepoSettings{}, "dark", 100, 40, lipgloss.Color("245"), nil, nil)

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

func TestRepoSettingsDialogThemeGridNavigation(t *testing.T) {
	// At width=100, boxWidth=84, innerWidth=78, cols=78/25=3
	d := NewRepoSettingsDialog()
	d.Show("repo-a", RepoSettings{}, "dark", 100, 40, lipgloss.Color("245"), nil, nil)

	// Themes: dark(0), light(1), dark-daltonized(2), light-daltonized(3), dark-ansi(4), light-ansi(5)
	// Grid 3 cols:
	//   row0: 0 1 2
	//   row1: 3 4 5
	if d.selectedIdx != 0 {
		t.Fatalf("initial selectedIdx = %d, want 0", d.selectedIdx)
	}

	// Right: 0 → 1
	d.Update(tea.KeyMsg{Type: tea.KeyRight})
	if d.selectedIdx != 1 {
		t.Fatalf("after right: selectedIdx = %d, want 1", d.selectedIdx)
	}

	// Right: 1 → 2
	d.Update(tea.KeyMsg{Type: tea.KeyRight})
	if d.selectedIdx != 2 {
		t.Fatalf("after right: selectedIdx = %d, want 2", d.selectedIdx)
	}

	// Right wraps: 2 → 0
	d.Update(tea.KeyMsg{Type: tea.KeyRight})
	if d.selectedIdx != 0 {
		t.Fatalf("after right wrap: selectedIdx = %d, want 0", d.selectedIdx)
	}

	// Down: 0 → 3
	d.Update(tea.KeyMsg{Type: tea.KeyDown})
	if d.selectedIdx != 3 {
		t.Fatalf("after down: selectedIdx = %d, want 3", d.selectedIdx)
	}

	// Down wraps: 3 → 0
	d.Update(tea.KeyMsg{Type: tea.KeyDown})
	if d.selectedIdx != 0 {
		t.Fatalf("after down wrap: selectedIdx = %d, want 0", d.selectedIdx)
	}

	// Up wraps: 0 → 3
	d.Update(tea.KeyMsg{Type: tea.KeyUp})
	if d.selectedIdx != 3 {
		t.Fatalf("after up wrap: selectedIdx = %d, want 3", d.selectedIdx)
	}

	// Up: 3 → 0
	d.Update(tea.KeyMsg{Type: tea.KeyUp})
	if d.selectedIdx != 0 {
		t.Fatalf("after up: selectedIdx = %d, want 0", d.selectedIdx)
	}

	// Left wraps: 0 → 2
	d.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if d.selectedIdx != 2 {
		t.Fatalf("after left wrap: selectedIdx = %d, want 2", d.selectedIdx)
	}
}

func TestRepoSettingsDialogThemeGrid2Cols(t *testing.T) {
	// At width=72, boxWidth=64, innerWidth=58, cols=58/25=2
	d := NewRepoSettingsDialog()
	d.Show("repo-a", RepoSettings{}, "dark", 72, 40, lipgloss.Color("245"), nil, nil)

	cols := d.themeGridCols()
	if cols != 2 {
		t.Fatalf("themeGridCols() = %d, want 2", cols)
	}

	// Grid 2 cols:
	//   row0: 0 1
	//   row1: 2 3
	//   row2: 4 5

	// Right: 0 → 1
	d.Update(tea.KeyMsg{Type: tea.KeyRight})
	if d.selectedIdx != 1 {
		t.Fatalf("after right: selectedIdx = %d, want 1", d.selectedIdx)
	}

	// Right wraps: 1 → 0
	d.Update(tea.KeyMsg{Type: tea.KeyRight})
	if d.selectedIdx != 0 {
		t.Fatalf("after right wrap: selectedIdx = %d, want 0", d.selectedIdx)
	}

	// Down: 0 → 2
	d.Update(tea.KeyMsg{Type: tea.KeyDown})
	if d.selectedIdx != 2 {
		t.Fatalf("after down: selectedIdx = %d, want 2", d.selectedIdx)
	}

	// Down: 2 → 4
	d.Update(tea.KeyMsg{Type: tea.KeyDown})
	if d.selectedIdx != 4 {
		t.Fatalf("after down: selectedIdx = %d, want 4", d.selectedIdx)
	}

	// Down wraps: 4 → 0
	d.Update(tea.KeyMsg{Type: tea.KeyDown})
	if d.selectedIdx != 0 {
		t.Fatalf("after down wrap: selectedIdx = %d, want 0", d.selectedIdx)
	}
}

func TestRepoSettingsDialogThemeGridRender(t *testing.T) {
	d := NewRepoSettingsDialog()
	d.Show("repo-a", RepoSettings{}, "dark", 100, 40, lipgloss.Color("245"), nil, nil)

	styles := NewStyles(Dark)
	output := d.View(styles)

	for _, theme := range BuiltinThemes {
		if !strings.Contains(output, theme.Name) {
			t.Errorf("View() output missing theme name %q", theme.Name)
		}
	}
}
