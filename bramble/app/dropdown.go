package app

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// DropdownItem represents an item in a dropdown menu.
type DropdownItem struct {
	ID       string
	Label    string
	Subtitle string // Optional secondary text
	Icon     string // Optional icon
	Badge    string // Optional badge (e.g., status, count)
}

// Dropdown represents a dropdown menu component.
type Dropdown struct {
	items       []DropdownItem
	selectedIdx int
	isOpen      bool
	width       int
	maxVisible  int
	scrollOffset int
}

// NewDropdown creates a new dropdown with the given items.
func NewDropdown(items []DropdownItem) *Dropdown {
	return &Dropdown{
		items:      items,
		maxVisible: 10,
	}
}

// SetItems replaces the dropdown items.
func (d *Dropdown) SetItems(items []DropdownItem) {
	d.items = items
	if d.selectedIdx >= len(items) {
		d.selectedIdx = max(0, len(items)-1)
	}
	d.scrollOffset = 0
}

// SetWidth sets the dropdown width.
func (d *Dropdown) SetWidth(w int) {
	d.width = w
}

// Width returns the dropdown width.
func (d *Dropdown) Width() int {
	return d.width
}

// SetMaxVisible sets the maximum visible items.
func (d *Dropdown) SetMaxVisible(n int) {
	d.maxVisible = n
}

// Open opens the dropdown.
func (d *Dropdown) Open() {
	d.isOpen = true
}

// Close closes the dropdown.
func (d *Dropdown) Close() {
	d.isOpen = false
}

// Toggle toggles the dropdown open/closed state.
func (d *Dropdown) Toggle() {
	d.isOpen = !d.isOpen
}

// IsOpen returns whether the dropdown is open.
func (d *Dropdown) IsOpen() bool {
	return d.isOpen
}

// SelectedIndex returns the currently selected index.
func (d *Dropdown) SelectedIndex() int {
	return d.selectedIdx
}

// SelectedItem returns the currently selected item, or nil if none.
func (d *Dropdown) SelectedItem() *DropdownItem {
	if d.selectedIdx >= 0 && d.selectedIdx < len(d.items) {
		return &d.items[d.selectedIdx]
	}
	return nil
}

// SelectByID selects an item by its ID.
func (d *Dropdown) SelectByID(id string) bool {
	for i, item := range d.items {
		if item.ID == id {
			d.selectedIdx = i
			d.ensureVisible()
			return true
		}
	}
	return false
}

// SelectIndex selects an item by index.
func (d *Dropdown) SelectIndex(idx int) {
	if idx >= 0 && idx < len(d.items) {
		d.selectedIdx = idx
		d.ensureVisible()
	}
}

// MoveSelection moves the selection up or down.
func (d *Dropdown) MoveSelection(delta int) {
	if len(d.items) == 0 {
		return
	}
	d.selectedIdx = clamp(d.selectedIdx+delta, 0, len(d.items)-1)
	d.ensureVisible()
}

// ensureVisible ensures the selected item is visible.
func (d *Dropdown) ensureVisible() {
	if d.selectedIdx < d.scrollOffset {
		d.scrollOffset = d.selectedIdx
	}
	if d.selectedIdx >= d.scrollOffset+d.maxVisible {
		d.scrollOffset = d.selectedIdx - d.maxVisible + 1
	}
}

// Count returns the number of items.
func (d *Dropdown) Count() int {
	return len(d.items)
}

// View renders the dropdown header (closed state).
func (d *Dropdown) ViewHeader() string {
	item := d.SelectedItem()
	if item == nil {
		return dimStyle.Render("(none)")
	}

	var b strings.Builder
	if item.Icon != "" {
		b.WriteString(item.Icon)
		b.WriteString(" ")
	}
	b.WriteString(item.Label)
	if item.Badge != "" {
		b.WriteString(" ")
		b.WriteString(dimStyle.Render(item.Badge))
	}
	b.WriteString(" ")
	b.WriteString(dimStyle.Render("▼"))

	return b.String()
}

// ViewList renders the dropdown list (open state).
func (d *Dropdown) ViewList() string {
	if len(d.items) == 0 {
		return dimStyle.Render("  (empty)")
	}

	var b strings.Builder

	endIdx := min(d.scrollOffset+d.maxVisible, len(d.items))

	// Show scroll indicator at top if needed
	if d.scrollOffset > 0 {
		b.WriteString(dimStyle.Render("  ↑ more"))
		b.WriteString("\n")
	}

	for i := d.scrollOffset; i < endIdx; i++ {
		item := d.items[i]

		prefix := "  "
		if i == d.selectedIdx {
			prefix = "> "
		}

		var line strings.Builder
		line.WriteString(prefix)
		if item.Icon != "" {
			line.WriteString(item.Icon)
			line.WriteString(" ")
		}
		line.WriteString(item.Label)
		if item.Badge != "" {
			line.WriteString(" ")
			line.WriteString(item.Badge)
		}

		// Truncate if needed (use lipgloss.Width for correct terminal column width)
		lineStr := line.String()
		if d.width > 0 && lipgloss.Width(lineStr) > d.width-2 {
			lineStr = truncateVisual(lineStr, d.width-5)
		}

		if i == d.selectedIdx {
			b.WriteString(selectedStyle.Render(lineStr))
		} else {
			b.WriteString(lineStr)
		}
		b.WriteString("\n")

		// Show subtitle if present
		if item.Subtitle != "" {
			subtitle := "    " + dimStyle.Render(item.Subtitle)
			if d.width > 0 && lipgloss.Width(subtitle) > d.width-2 {
				subtitle = truncateVisual(subtitle, d.width-5)
			}
			b.WriteString(subtitle)
			b.WriteString("\n")
		}
	}

	// Show scroll indicator at bottom if needed
	if endIdx < len(d.items) {
		b.WriteString(dimStyle.Render("  ↓ more"))
	}

	return b.String()
}

// ViewOverlay renders the dropdown as an overlay box.
func (d *Dropdown) ViewOverlay() string {
	if !d.isOpen {
		return ""
	}

	content := d.ViewList()
	
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("12")).
		Padding(0, 1)

	if d.width > 0 {
		style = style.Width(d.width)
	}

	return style.Render(content)
}

// truncateVisual truncates a string by terminal display columns (ignoring ANSI codes,
// handling wide characters like emojis correctly) and appends "...".
func truncateVisual(s string, maxCols int) string {
	if maxCols <= 3 {
		return "..."
	}
	target := maxCols - 3
	var result strings.Builder
	cols := 0
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			result.WriteRune(r)
			continue
		}
		if inEscape {
			result.WriteRune(r)
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		w := runewidth.RuneWidth(r)
		if cols+w > target {
			break
		}
		result.WriteRune(r)
		cols += w
	}
	result.WriteString("...")
	return result.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
