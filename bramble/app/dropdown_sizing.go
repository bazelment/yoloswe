package app

const (
	dropdownHorizontalPadding = 4
	dropdownVerticalPadding   = 6
	minDropdownVisibleItems   = 5
)

// configureDropdownForViewport sizes a dropdown to use most of the terminal.
func (m *Model) configureDropdownForViewport(d *Dropdown) {
	if d == nil {
		return
	}

	width := m.width - dropdownHorizontalPadding
	if width < 1 {
		width = 1
	}
	d.SetWidth(width)

	maxVisible := m.height - dropdownVerticalPadding
	if maxVisible < minDropdownVisibleItems {
		maxVisible = minDropdownVisibleItems
	}
	d.SetMaxVisible(maxVisible)
}

// configureAllDropdownsForViewport applies viewport sizing to all top-level dropdowns.
func (m *Model) configureAllDropdownsForViewport() {
	m.configureDropdownForViewport(m.worktreeDropdown)
	m.configureDropdownForViewport(m.sessionDropdown)
	m.configureDropdownForViewport(m.repoDropdown)
}
