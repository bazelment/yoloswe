package app

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bazelment/yoloswe/wt"
)

type pickerMode int

const (
	pickerModeList pickerMode = iota
	pickerModeURLInput
	pickerModeCloning
)

// RepoPickerModel is the model for the repo selection screen.
type RepoPickerModel struct { //nolint:govet // fieldalignment: readability over packed layout
	ctx               context.Context
	cloneCancel       context.CancelFunc
	err               error
	cloneErr          error
	styles            *Styles
	wtRoot            string
	chosenRepo        string
	filterText        string
	urlInput          string
	urlInputField     textinput.Model
	cloneRepoName     string
	pendingSelectRepo string
	repos             []string
	filteredIndices   []int // indices into repos; nil = no filter (show all)
	selectedIdx       int
	width             int
	height            int
	mode              pickerMode
	loading           bool
}

// NewRepoPickerModel creates a new repo picker model.
func NewRepoPickerModel(ctx context.Context, wtRoot string, styles *Styles) RepoPickerModel {
	if styles == nil {
		styles = NewStyles(Dark)
	}
	urlInputField := newRepoPickerURLInput(styles)
	return RepoPickerModel{
		ctx:           ctx,
		wtRoot:        wtRoot,
		styles:        styles,
		urlInputField: urlInputField,
		loading:       true,
	}
}

func newRepoPickerURLInput(styles *Styles) textinput.Model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Placeholder = "https://github.com/user/repo"
	ti.CharLimit = 0
	ti.PromptStyle = lipgloss.NewStyle()
	ti.TextStyle = lipgloss.NewStyle()
	ti.Cursor.Style = lipgloss.NewStyle()
	if styles != nil {
		ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(styles.Palette.Dim))
	}
	ti.Blur()
	return ti
}

// RepoSelectedMsg is sent when a repo is selected.
type RepoSelectedMsg struct {
	RepoName string
}

type repoInitSuccessMsg struct {
	repoName string
}

type repoInitErrorMsg struct {
	err error
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

func (m RepoPickerModel) initRepo(ctx context.Context, url string) tea.Cmd {
	return func() tea.Msg {
		// Validate that the URL yields a usable repo name before cloning.
		repoName := wt.GetRepoNameFromURL(url)
		if repoName == "" || repoName == "." || repoName == ".." || strings.ContainsAny(repoName, "/\\") {
			return repoInitErrorMsg{err: fmt.Errorf("could not determine repository name from URL")}
		}
		var buf bytes.Buffer
		output := wt.NewOutput(&buf, false)
		manager := wt.NewManager(m.wtRoot, "", wt.WithOutput(output))
		if _, err := manager.Init(ctx, url); err != nil {
			return repoInitErrorMsg{err: err}
		}
		return repoInitSuccessMsg{repoName: repoName}
	}
}

// Update handles messages.
func (m RepoPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case pickerModeURLInput:
		return m.updateURLInput(msg)
	case pickerModeCloning:
		return m.updateCloning(msg)
	default:
		return m.updateList(msg)
	}
}

func (m RepoPickerModel) updateURLInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.ensureURLInputReady()
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.mode = pickerModeList
			m.clearURLInput()
			m.cloneErr = nil
			m.urlInputField.Blur()
			return m, nil
		case "enter":
			url := strings.TrimSpace(m.urlInputField.Value())
			if url == "" {
				return m, nil
			}
			cloneCtx, cloneCancel := context.WithCancel(m.ctx)
			m.mode = pickerModeCloning
			m.cloneRepoName = url
			m.cloneErr = nil
			m.cloneCancel = cloneCancel
			m.urlInputField.Blur()
			return m, m.initRepo(cloneCtx, url)
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeURLInputField()
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
	var cmd tea.Cmd
	m.urlInputField, cmd = m.urlInputField.Update(msg)
	m.syncURLInputFromField()
	return m, cmd
}

func (m RepoPickerModel) updateCloning(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.ensureURLInputReady()
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.cloneCancel != nil {
				m.cloneCancel()
			}
			return m, tea.Quit
		}
		return m, nil
	case repoInitSuccessMsg:
		if m.cloneCancel != nil {
			m.cloneCancel()
			m.cloneCancel = nil
		}
		m.pendingSelectRepo = msg.repoName
		m.mode = pickerModeList
		m.clearURLInput()
		m.cloneErr = nil
		m.cloneRepoName = ""
		m.loading = true
		return m, m.loadRepos()
	case repoInitErrorMsg:
		if m.cloneCancel != nil {
			m.cloneCancel()
			m.cloneCancel = nil
		}
		m.mode = pickerModeURLInput
		m.urlInputField.Focus()
		m.resizeURLInputField()
		m.cloneErr = msg.err
		m.cloneRepoName = ""
		return m, nil
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

func (m RepoPickerModel) updateList(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.ensureURLInputReady()
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

		case "a":
			// Only enter URL input if no filter active; otherwise treat as filter char
			if m.filterText == "" {
				m.mode = pickerModeURLInput
				m.clearURLInput()
				m.urlInputField.Focus()
				m.resizeURLInputField()
				m.cloneErr = nil
				return m, nil
			}
			m.filterText += "a"
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
		// If we just cloned a repo, auto-select it
		if m.pendingSelectRepo != "" {
			for i, repo := range m.repos {
				if repo == m.pendingSelectRepo {
					m.selectedIdx = i
					break
				}
			}
			m.pendingSelectRepo = ""
		}
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
	if m.mode == pickerModeURLInput || m.mode == pickerModeCloning {
		boxWidth = 92
	}
	if m.width < boxWidth+10 {
		boxWidth = m.width - 10
	}
	if boxWidth < 20 {
		boxWidth = 20
	}

	var content strings.Builder

	switch m.mode {
	case pickerModeURLInput:
		m.viewURLInput(&content, boxWidth)
	case pickerModeCloning:
		m.viewCloning(&content, boxWidth)
	default:
		m.viewList(&content, boxWidth)
	}

	// Create bordered box
	box := m.styles.ModalBox.Width(boxWidth).Render(content.String())

	// Center the box
	return lipgloss.Place(
		m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		box,
	)
}

func (m RepoPickerModel) viewURLInput(content *strings.Builder, boxWidth int) {
	m.ensureURLInputReady()
	m.resizeURLInputField()

	title := m.styles.Title.Render("Bramble — Add repository")
	content.WriteString(title)
	content.WriteString("\n")
	content.WriteString(strings.Repeat("─", boxWidth-4))
	content.WriteString("\n\n")

	content.WriteString("  Enter a git URL:\n\n")
	content.WriteString("  " + m.urlInputField.View() + "\n")

	if m.cloneErr != nil {
		content.WriteString("\n")
		content.WriteString(m.styles.Error.Render("  Error: " + m.cloneErr.Error()))
		content.WriteString("\n")
	}

	content.WriteString("\n")
	content.WriteString(m.styles.Dim.Render("  e.g. https://github.com/user/repo"))
	content.WriteString("\n")
	content.WriteString(m.styles.Dim.Render("       git@github.com:user/repo.git"))
	content.WriteString("\n\n")
	content.WriteString(m.styles.Dim.Render("  " + formatKeyHints("Enter", "clone") + "  " + formatKeyHints("Esc", "back") + "  " + formatKeyHints("Ctrl+v", "paste")))
	content.WriteString("\n")
}

func (m *RepoPickerModel) ensureURLInputReady() {
	if m.urlInputField.Prompt == "" {
		m.urlInputField = newRepoPickerURLInput(m.styles)
	}
	if m.mode == pickerModeURLInput && !m.urlInputField.Focused() {
		m.urlInputField.Focus()
	}
	if m.mode != pickerModeURLInput && m.urlInputField.Focused() {
		m.urlInputField.Blur()
	}
	if m.urlInput != "" && m.urlInputField.Value() == "" {
		m.urlInputField.SetValue(m.urlInput)
		m.urlInputField.CursorEnd()
	}
}

func (m *RepoPickerModel) clearURLInput() {
	m.urlInput = ""
	m.urlInputField.Reset()
}

func (m *RepoPickerModel) syncURLInputFromField() {
	m.urlInput = m.urlInputField.Value()
}

func (m *RepoPickerModel) resizeURLInputField() {
	inputWidth := m.urlDialogInputWidth()
	if inputWidth < 12 {
		inputWidth = 12
	}
	m.urlInputField.Width = inputWidth
}

func (m RepoPickerModel) urlDialogInputWidth() int {
	boxWidth := 92
	if m.width > 0 && m.width < boxWidth+10 {
		boxWidth = m.width - 10
	}
	if boxWidth < 20 {
		boxWidth = 20
	}
	return boxWidth - 8 // box border + left indent + prompt
}

func (m RepoPickerModel) viewCloning(content *strings.Builder, boxWidth int) {
	title := m.styles.Title.Render("Bramble — Cloning repository")
	content.WriteString(title)
	content.WriteString("\n")
	content.WriteString(strings.Repeat("─", boxWidth-4))
	content.WriteString("\n\n")

	content.WriteString("  Cloning " + m.cloneRepoName + "...\n\n")
	content.WriteString(m.styles.Dim.Render("  This may take a moment."))
	content.WriteString("\n")
}

func (m RepoPickerModel) viewList(content *strings.Builder, boxWidth int) {
	// Title
	title := m.styles.Title.Render("Bramble — Choose repository")
	content.WriteString(title)
	content.WriteString("\n")
	content.WriteString(strings.Repeat("─", boxWidth-4))
	content.WriteString("\n\n")

	if m.loading {
		content.WriteString(m.styles.Dim.Render("  Loading repositories..."))
		content.WriteString("\n")
	} else if m.err != nil {
		content.WriteString(m.styles.Error.Render("  Error: " + m.err.Error()))
		content.WriteString("\n\n")
		content.WriteString(m.styles.Dim.Render("  Press [r] to retry or [q] to quit"))
		content.WriteString("\n")
	} else if len(m.repos) == 0 {
		content.WriteString(m.styles.Dim.Render("  No repos found in " + m.wtRoot))
		content.WriteString("\n\n")
		content.WriteString(m.styles.Dim.Render("  Press [a] to add a repository"))
		content.WriteString("\n\n")
		content.WriteString(m.styles.Dim.Render("  Press [q] to quit"))
		content.WriteString("\n")
	} else {
		content.WriteString("  Select a repo (type to filter, ↑/↓ then Enter):\n\n")

		// Show filter indicator when active
		if m.filterText != "" {
			filterLine := m.styles.Dim.Render("  Filter: ") + m.filterText
			content.WriteString(filterLine)
			content.WriteString("\n\n")
		}

		// Show effective repos (filtered or all)
		eff := m.effectiveRepos()

		if len(eff) == 0 && m.filterText != "" {
			content.WriteString(m.styles.Dim.Render("  No matches for \"" + m.filterText + "\""))
			content.WriteString("\n")
		} else {
			maxVisible := min(10, m.height-10)
			startIdx := 0
			if m.selectedIdx >= maxVisible {
				startIdx = m.selectedIdx - maxVisible + 1
			}
			endIdx := min(startIdx+maxVisible, len(eff))

			if startIdx > 0 {
				content.WriteString(m.styles.Dim.Render("    ↑ more"))
				content.WriteString("\n")
			}

			for i := startIdx; i < endIdx; i++ {
				prefix := "    "
				if i == m.selectedIdx {
					prefix = "  > "
				}

				line := prefix + eff[i]
				if i == m.selectedIdx {
					content.WriteString(m.styles.Selected.Render(line))
				} else {
					content.WriteString(line)
				}
				content.WriteString("\n")
			}

			if endIdx < len(eff) {
				content.WriteString(m.styles.Dim.Render("    ↓ more"))
				content.WriteString("\n")
			}
		}

		content.WriteString("\n")
		content.WriteString(m.styles.Dim.Render("  " + formatKeyHints("Enter", "open") + "  " + formatKeyHints("a", "add repo") + "  " + formatKeyHints("Esc", "clear filter/quit") + "  " + formatKeyHints("q", "quit")))
		content.WriteString("\n")
	}
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
