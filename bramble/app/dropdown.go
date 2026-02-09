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
type Dropdown struct { //nolint:govet // fieldalignment: readability over padding
	items           []DropdownItem
	filteredIndices []int // indices into items; nil = no filter (show all)
	filterText      string
	selectedIdx     int
	width           int
	maxVisible      int
	scrollOffset    int
	isOpen          bool
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
	d.ClearFilter()
	if d.selectedIdx >= len(items) {
		d.selectedIdx = max(0, len(items)-1)
	}
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
	d.ClearFilter()
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
	eff := d.effectiveItems()
	if d.selectedIdx >= 0 && d.selectedIdx < len(eff) {
		// Return pointer to original item (not the copy in eff)
		if d.filteredIndices != nil {
			origIdx := d.filteredIndices[d.selectedIdx]
			return &d.items[origIdx]
		}
		return &d.items[d.selectedIdx]
	}
	return nil
}

// SelectByID selects an item by its ID.
func (d *Dropdown) SelectByID(id string) bool {
	// Clear filter so selectedIdx maps to full items list
	d.ClearFilter()
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
	d.ClearFilter()
	if idx >= 0 && idx < len(d.items) {
		d.selectedIdx = idx
		d.ensureVisible()
	}
}

// MoveSelection moves the selection up or down, skipping separator items.
func (d *Dropdown) MoveSelection(delta int) {
	effLen := d.effectiveLen()
	if effLen == 0 {
		return
	}
	eff := d.effectiveItems()
	newIdx := clamp(d.selectedIdx+delta, 0, effLen-1)
	// Skip separator items by continuing in the same direction
	step := 1
	if delta < 0 {
		step = -1
	}
	for newIdx >= 0 && newIdx < effLen && eff[newIdx].ID == "---separator---" {
		newIdx += step
	}
	newIdx = clamp(newIdx, 0, effLen-1)
	if eff[newIdx].ID == "---separator---" {
		return
	}
	d.selectedIdx = newIdx
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

// FilterText returns the current filter string.
func (d *Dropdown) FilterText() string {
	return d.filterText
}

// AppendFilter adds a rune to the filter and recomputes the filtered list.
func (d *Dropdown) AppendFilter(r rune) {
	d.filterText += string(r)
	d.applyFilter()
}

// BackspaceFilter removes the last rune from the filter.
// If the filter becomes empty, the full list is restored.
func (d *Dropdown) BackspaceFilter() {
	if d.filterText == "" {
		return
	}
	runes := []rune(d.filterText)
	d.filterText = string(runes[:len(runes)-1])
	d.applyFilter()
}

// ClearFilter resets the filter and shows all items.
// Maps the current filtered selection back to its original index so the
// user's selection is preserved.
func (d *Dropdown) ClearFilter() {
	// Map filtered selectedIdx back to original index before clearing
	if d.filteredIndices != nil && d.selectedIdx >= 0 && d.selectedIdx < len(d.filteredIndices) {
		d.selectedIdx = d.filteredIndices[d.selectedIdx]
	}
	d.filterText = ""
	d.filteredIndices = nil
	// Clamp selectedIdx to valid range
	if d.selectedIdx < 0 || d.selectedIdx >= len(d.items) {
		d.selectedIdx = max(0, len(d.items)-1)
	}
	d.scrollOffset = 0
}

// applyFilter recomputes filteredIndices from filterText.
func (d *Dropdown) applyFilter() {
	if d.filterText == "" {
		d.ClearFilter()
		return
	}

	lower := strings.ToLower(d.filterText)
	// Initialize to empty slice (not nil) to distinguish from "no filter"
	d.filteredIndices = []int{}
	for i, item := range d.items {
		if item.ID == "---separator---" {
			continue // Never include separators in filtered results
		}
		if strings.Contains(strings.ToLower(item.Label), lower) {
			d.filteredIndices = append(d.filteredIndices, i)
		}
	}

	// Reset selection to first match when filter changes
	if len(d.filteredIndices) > 0 {
		d.selectedIdx = 0
	} else {
		d.selectedIdx = -1 // No matches
	}
	d.scrollOffset = 0
	d.ensureVisible()
}

// effectiveItems returns the items currently visible (filtered or all).
func (d *Dropdown) effectiveItems() []DropdownItem {
	if d.filteredIndices == nil {
		return d.items
	}
	result := make([]DropdownItem, len(d.filteredIndices))
	for i, idx := range d.filteredIndices {
		result[i] = d.items[idx]
	}
	return result
}

// effectiveLen returns the count of items currently visible.
func (d *Dropdown) effectiveLen() int {
	if d.filteredIndices == nil {
		return len(d.items)
	}
	return len(d.filteredIndices)
}

// Count returns the number of items.
func (d *Dropdown) Count() int {
	return len(d.items)
}

// ViewHeader renders the dropdown header (closed state).
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
	eff := d.effectiveItems()
	if len(eff) == 0 {
		if d.filterText != "" {
			return dimStyle.Render("  No matches for \"" + d.filterText + "\"")
		}
		return dimStyle.Render("  (empty)")
	}

	var b strings.Builder

	// Show filter indicator when active
	if d.filterText != "" {
		filterLine := dimStyle.Render("  Filter: ") + d.filterText
		b.WriteString(filterLine)
		b.WriteString("\n")
	}

	endIdx := min(d.scrollOffset+d.maxVisible, len(eff))

	// Show scroll indicator at top if needed
	if d.scrollOffset > 0 {
		b.WriteString(dimStyle.Render("  ↑ more"))
		b.WriteString("\n")
	}

	for i := d.scrollOffset; i < endIdx; i++ {
		item := eff[i]

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
	if endIdx < len(eff) {
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

	style := inputBoxStyle
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
