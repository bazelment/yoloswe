package app

import (
	"strings"

	"github.com/bazelment/yoloswe/wt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// RepoPickerModel is the model for the repo selection screen.
type RepoPickerModel struct {
	wtRoot       string
	repos        []string
	selectedIdx  int
	width        int
	height       int
	loading      bool
	err          error
	chosenRepo   string // Set when user makes a selection
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
		case "q", "ctrl+c", "esc":
			return m, tea.Quit

		case "j", "down":
			if m.selectedIdx < len(m.repos)-1 {
				m.selectedIdx++
			}
			return m, nil

		case "k", "up":
			if m.selectedIdx > 0 {
				m.selectedIdx--
			}
			return m, nil

		case "enter":
			if len(m.repos) > 0 && m.selectedIdx < len(m.repos) {
				m.chosenRepo = m.repos[m.selectedIdx]
				return m, tea.Quit
			}
			return m, nil

		case "r":
			m.loading = true
			return m, m.loadRepos()
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case repoLoadedMsg:
		m.repos = msg.repos
		m.loading = false
		m.err = nil
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
		content.WriteString("  Select a repo (↑/↓ then Enter):\n\n")

		// Show repos
		maxVisible := min(10, m.height-10)
		startIdx := 0
		if m.selectedIdx >= maxVisible {
			startIdx = m.selectedIdx - maxVisible + 1
		}
		endIdx := min(startIdx+maxVisible, len(m.repos))

		if startIdx > 0 {
			content.WriteString(dimStyle.Render("    ↑ more"))
			content.WriteString("\n")
		}

		for i := startIdx; i < endIdx; i++ {
			prefix := "    "
			if i == m.selectedIdx {
				prefix = "  > "
			}

			line := prefix + m.repos[i]
			if i == m.selectedIdx {
				content.WriteString(selectedStyle.Render(line))
			} else {
				content.WriteString(line)
			}
			content.WriteString("\n")
		}

		if endIdx < len(m.repos) {
			content.WriteString(dimStyle.Render("    ↓ more"))
			content.WriteString("\n")
		}

		content.WriteString("\n")
		content.WriteString(dimStyle.Render("  [ Tab: next  Enter: open  q: quit ]"))
		content.WriteString("\n")
	}

	// Create bordered box
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(1, 2).
		Width(boxWidth)

	box := boxStyle.Render(content.String())

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
