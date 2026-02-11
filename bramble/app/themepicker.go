package app

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ThemePicker is the overlay for selecting a color theme.
type ThemePicker struct { //nolint:govet // fieldalignment: readability over padding
	themes        []ColorPalette
	originalTheme string // theme name to revert to on Esc
	selectedIdx   int
	width         int
	height        int
	visible       bool
}

// NewThemePicker creates a new theme picker.
func NewThemePicker() *ThemePicker {
	return &ThemePicker{
		themes: BuiltinThemes,
	}
}

// Show opens the picker centered in the terminal.
func (tp *ThemePicker) Show(currentTheme string, w, h int) {
	tp.visible = true
	tp.width = w
	tp.height = h
	tp.originalTheme = currentTheme
	// Select the current theme
	for i := range tp.themes {
		if tp.themes[i].Name == currentTheme {
			tp.selectedIdx = i
			return
		}
	}
	tp.selectedIdx = 0
}

// Hide closes the picker.
func (tp *ThemePicker) Hide() {
	tp.visible = false
}

// SetSize updates the overlay dimensions (e.g. on terminal resize).
func (tp *ThemePicker) SetSize(w, h int) {
	tp.width = w
	tp.height = h
}

// IsVisible returns whether the picker is showing.
func (tp *ThemePicker) IsVisible() bool {
	return tp.visible
}

// MoveSelection moves the selection by delta (positive = down).
func (tp *ThemePicker) MoveSelection(delta int) {
	tp.selectedIdx += delta
	if tp.selectedIdx < 0 {
		tp.selectedIdx = 0
	}
	if tp.selectedIdx >= len(tp.themes) {
		tp.selectedIdx = len(tp.themes) - 1
	}
}

// SelectedTheme returns the currently highlighted theme.
func (tp *ThemePicker) SelectedTheme() ColorPalette {
	if tp.selectedIdx >= 0 && tp.selectedIdx < len(tp.themes) {
		return tp.themes[tp.selectedIdx]
	}
	return DefaultDark
}

// OriginalTheme returns the theme name that was active when the picker opened.
func (tp *ThemePicker) OriginalTheme() string {
	return tp.originalTheme
}

// View renders the theme picker overlay.
func (tp *ThemePicker) View(styles *Styles) string {
	var lines []string

	lines = append(lines, styles.Title.Render("Color Theme"), "")

	for i := range tp.themes {
		prefix := "  "
		if i == tp.selectedIdx {
			prefix = "> "
		}

		line := prefix + tp.themes[i].Name
		if tp.themes[i].Name == tp.originalTheme {
			line += " " + styles.Dim.Render("(current)")
		}

		if i == tp.selectedIdx {
			lines = append(lines, styles.Selected.Render(line))
		} else {
			lines = append(lines, line)
		}
	}

	lines = append(lines, "", styles.Dim.Render("[Up/Down] Navigate  [Enter] Apply  [Esc] Cancel"))

	contentStr := strings.Join(lines, "\n")

	boxWidth := 50
	if tp.width > 0 && tp.width < 60 {
		boxWidth = tp.width - 10
	}

	box := styles.ModalBox.Width(boxWidth).Render(contentStr)

	if tp.width > 0 && tp.height > 0 {
		return lipgloss.Place(
			tp.width, tp.height,
			lipgloss.Center, lipgloss.Center,
			box,
		)
	}
	return box
}
