package app

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bazelment/yoloswe/wt"
)

// RepoPickerModel is the model for the repo selection screen.
type RepoPickerModel struct {
	err             error
	wtRoot          string
	chosenRepo      string
	filterText      string
	repos           []string
	filteredIndices []int // indices into repos; nil = no filter (show all)
	selectedIdx     int
	width           int
	height          int
	loading         bool
}

// NewRepoPickerModel creates a new repo picker model.
func NewRepoPickerModel(wtRoot string) RepoPickerModel {
	return RepoPickerModel{
		wtRoot:  wtRoot,
		loading: true,
	}
}

// RepoSelectedMsg is sent when a repo is selected.
type RepoSelectedMsg struct {
	RepoName string
}

// Init initializes the repo picker.
func (m RepoPickerModel) Init() tea.Cmd {
	return m.loadRepos()
}

func (m RepoPickerModel) loadRepos() tea.Cmd {
	return func() tea.Msg {
		repos, err := wt.ListAllRepos(m.wtRoot)
		if err != nil {
			return repoLoadErrorMsg{err}
		}
		return repoLoadedMsg{repos}
	}
}

type repoLoadedMsg struct {
	repos []string
}

type repoLoadErrorMsg struct {
	err error
}

// Update handles messages.
func (m RepoPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "q":
			// Only quit if no filter active; otherwise treat as filter char
			if m.filterText == "" {
				return m, tea.Quit
			}
			m.filterText += "q"
			m.applyFilter()
			return m, nil

		case "esc":
			// Clear filter first, then quit
			if m.filterText != "" {
				m.clearFilter()
				return m, nil
			}
			return m, tea.Quit

		case "j", "down":
			eff := m.effectiveRepos()
			if m.selectedIdx < len(eff)-1 {
				m.selectedIdx++
			}
			return m, nil

		case "k", "up":
			if m.selectedIdx > 0 {
				m.selectedIdx--
			}
			return m, nil

		case "enter":
			eff := m.effectiveRepos()
			if len(eff) > 0 && m.selectedIdx >= 0 && m.selectedIdx < len(eff) {
				m.chosenRepo = eff[m.selectedIdx]
				return m, tea.Quit
			}
			return m, nil

		case "r":
			// Only refresh if no filter active; otherwise treat as filter char
			if m.filterText == "" {
				m.loading = true
				return m, m.loadRepos()
			}
			m.filterText += "r"
			m.applyFilter()
			return m, nil

		case "backspace":
			if m.filterText != "" {
				runes := []rune(m.filterText)
				m.filterText = string(runes[:len(runes)-1])
				m.applyFilter()
			}
			return m, nil

		default:
			// Type-to-filter: printable characters
			if r, ok := printableRune(msg); ok {
				m.filterText += string(r)
				m.applyFilter()
				return m, nil
			}
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case repoLoadedMsg:
		m.repos = msg.repos
		m.loading = false
		m.err = nil
		// Re-apply active filter to new repo list
		if m.filterText != "" {
			m.applyFilter()
		}
		return m, nil

	case repoLoadErrorMsg:
		m.err = msg.err
		m.loading = false
		return m, nil
	}

	return m, nil
}

// View renders the repo picker.
func (m RepoPickerModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading..."
	}

	// Build the picker box
	boxWidth := 60
	if m.width < 70 {
		boxWidth = m.width - 10
	}
	if boxWidth < 20 {
		boxWidth = 20
	}

	var content strings.Builder

	// Title
	title := titleStyle.Render("Bramble — Choose repository")
	content.WriteString(title)
	content.WriteString("\n")
	content.WriteString(strings.Repeat("─", boxWidth-4))
	content.WriteString("\n\n")

	if m.loading {
		content.WriteString(dimStyle.Render("  Loading repositories..."))
		content.WriteString("\n")
	} else if m.err != nil {
		content.WriteString(errorStyle.Render("  Error: " + m.err.Error()))
		content.WriteString("\n\n")
		content.WriteString(dimStyle.Render("  Press [r] to retry or [q] to quit"))
		content.WriteString("\n")
	} else if len(m.repos) == 0 {
		content.WriteString(dimStyle.Render("  No repos found in " + m.wtRoot))
		content.WriteString("\n\n")
		content.WriteString(dimStyle.Render("  Run 'wt init <url>' to add a repository"))
		content.WriteString("\n\n")
		content.WriteString(dimStyle.Render("  Press [q] to quit"))
		content.WriteString("\n")
	} else {
		content.WriteString("  Select a repo (type to filter, ↑/↓ then Enter):\n\n")

		// Show filter indicator when active
		if m.filterText != "" {
			filterLine := dimStyle.Render("  Filter: ") + m.filterText
			content.WriteString(filterLine)
			content.WriteString("\n\n")
		}

		// Show effective repos (filtered or all)
		eff := m.effectiveRepos()

		if len(eff) == 0 && m.filterText != "" {
			content.WriteString(dimStyle.Render("  No matches for \"" + m.filterText + "\""))
			content.WriteString("\n")
		} else {
			maxVisible := min(10, m.height-10)
			startIdx := 0
			if m.selectedIdx >= maxVisible {
				startIdx = m.selectedIdx - maxVisible + 1
			}
			endIdx := min(startIdx+maxVisible, len(eff))

			if startIdx > 0 {
				content.WriteString(dimStyle.Render("    ↑ more"))
				content.WriteString("\n")
			}

			for i := startIdx; i < endIdx; i++ {
				prefix := "    "
				if i == m.selectedIdx {
					prefix = "  > "
				}

				line := prefix + eff[i]
				if i == m.selectedIdx {
					content.WriteString(selectedStyle.Render(line))
				} else {
					content.WriteString(line)
				}
				content.WriteString("\n")
			}

			if endIdx < len(eff) {
				content.WriteString(dimStyle.Render("    ↓ more"))
				content.WriteString("\n")
			}
		}

		content.WriteString("\n")
		content.WriteString(dimStyle.Render("  " + formatKeyHints("Enter", "open") + "  " + formatKeyHints("Esc", "clear filter/quit") + "  " + formatKeyHints("q", "quit")))
		content.WriteString("\n")
	}

	// Create bordered box
	box := modalBoxStyle.Width(boxWidth).Render(content.String())

	// Center the box
	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		box,
	)
}

// SelectedRepo returns the chosen repo name, or empty if none was chosen.
func (m RepoPickerModel) SelectedRepo() string {
	return m.chosenRepo
}

// effectiveRepos returns the repos currently visible (filtered or all).
func (m *RepoPickerModel) effectiveRepos() []string {
	if m.filteredIndices == nil {
		return m.repos
	}
	result := make([]string, len(m.filteredIndices))
	for i, idx := range m.filteredIndices {
		result[i] = m.repos[idx]
	}
	return result
}

// applyFilter recomputes filteredIndices from filterText.
func (m *RepoPickerModel) applyFilter() {
	if m.filterText == "" {
		m.clearFilter()
		return
	}

	lower := strings.ToLower(m.filterText)
	m.filteredIndices = []int{}
	for i, repo := range m.repos {
		if strings.Contains(strings.ToLower(repo), lower) {
			m.filteredIndices = append(m.filteredIndices, i)
		}
	}

	if len(m.filteredIndices) > 0 {
		m.selectedIdx = 0
	} else {
		m.selectedIdx = -1
	}
}

// clearFilter resets the filter and shows all repos.
func (m *RepoPickerModel) clearFilter() {
	// Map filtered selectedIdx back to original index before clearing
	if m.filteredIndices != nil && m.selectedIdx >= 0 && m.selectedIdx < len(m.filteredIndices) {
		m.selectedIdx = m.filteredIndices[m.selectedIdx]
	}
	m.filterText = ""
	m.filteredIndices = nil
	if m.selectedIdx < 0 || m.selectedIdx >= len(m.repos) {
		m.selectedIdx = max(0, len(m.repos)-1)
	}
}
