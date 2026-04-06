package tui

import (
	"fmt"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/bazelment/yoloswe/jiradozer"
)

// focusArea tracks which view is active.
type focusArea int

const (
	focusDashboard focusArea = iota
	focusDetail
)

// Model is the root bubbletea model for the jiradozer TUI.
type Model struct {
	orchestrator *jiradozer.Orchestrator
	dashboard    *dashboard
	detail       *detail
	statuses     []jiradozer.IssueStatus
	width        int
	height       int
	focus        focusArea
	quitting     bool
}

// NewModel creates a new TUI model connected to the given orchestrator.
func NewModel(orch *jiradozer.Orchestrator) *Model {
	return &Model{
		orchestrator: orch,
		dashboard:    newDashboard(),
		detail:       newDetail(),
		focus:        focusDashboard,
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		m.listenForStatus(),
		m.tickRefresh(),
	)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.dashboard.height = m.height - 6 // header + status bar
		m.detail.height = m.height - 4
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case issueStatusMsg:
		m.updateStatus(msg.Status)
		cmd := m.listenForStatus()
		return m, cmd

	case statusTickMsg:
		m.refreshSnapshot()
		cmd := m.tickRefresh()
		return m, cmd
	}

	return m, nil
}

func (m *Model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}

	content := m.viewContent()
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func (m *Model) viewContent() string {
	var content string
	switch m.focus {
	case focusDashboard:
		content = m.dashboard.view(m.width)
	case focusDetail:
		content = m.detail.view(m.width)
	}

	// Title bar
	active := 0
	done := 0
	failed := 0
	for _, s := range m.statuses {
		switch {
		case s.Step == jiradozer.StepDone:
			done++
		case s.Step == jiradozer.StepFailed:
			failed++
		default:
			active++
		}
	}
	title := styleTitle.Render(" jiradozer ")
	stats := styleDim.Render(fmt.Sprintf(" %d active  %d done  %d failed  %d total",
		active, done, failed, len(m.statuses)))
	titleBar := lipgloss.JoinHorizontal(lipgloss.Top, title, stats)

	// Status bar
	help := "  j/k:navigate  Enter:detail  c:cancel  q:quit"
	if m.focus == focusDetail {
		help = "  j/k:scroll  Esc:back  q:quit"
	}
	statusBar := styleStatusBar.Render(
		fmt.Sprintf("%-*s", m.width, help))

	return lipgloss.JoinVertical(lipgloss.Left,
		titleBar,
		"",
		content,
		statusBar,
	)
}

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	}

	switch m.focus {
	case focusDashboard:
		return m.handleDashboardKey(msg)
	case focusDetail:
		return m.handleDetailKey(msg)
	}
	return m, nil
}

func (m *Model) handleDashboardKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		m.dashboard.moveDown()
	case "k", "up":
		m.dashboard.moveUp()
	case "enter":
		if sel := m.dashboard.selected(); sel != nil {
			m.detail.setStatus(sel)
			m.focus = focusDetail
		}
	case "c":
		if sel := m.dashboard.selected(); sel != nil {
			m.orchestrator.Cancel(sel.Issue.ID)
		}
	}
	return m, nil
}

func (m *Model) handleDetailKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		m.focus = focusDashboard
	case "j", "down":
		m.detail.scrollDown()
	case "k", "up":
		m.detail.scrollUp()
	}
	return m, nil
}

func (m *Model) updateStatus(s jiradozer.IssueStatus) {
	found := false
	for i, existing := range m.statuses {
		if existing.Issue.ID == s.Issue.ID {
			m.statuses[i] = s
			found = true
			break
		}
	}
	if !found {
		m.statuses = append(m.statuses, s)
	}
	m.sortStatuses()
	m.dashboard.setStatuses(m.statuses)

	// Update detail view if it's showing this issue.
	if m.focus == focusDetail && m.detail.status != nil && m.detail.status.Issue.ID == s.Issue.ID {
		m.detail.setStatus(&s)
	}
}

func (m *Model) refreshSnapshot() {
	snap := m.orchestrator.Snapshot()
	// Merge snapshot into existing statuses (preserving completed/failed entries).
	snapMap := make(map[string]jiradozer.IssueStatus, len(snap))
	for _, s := range snap {
		snapMap[s.Issue.ID] = s
	}
	for i, existing := range m.statuses {
		if updated, ok := snapMap[existing.Issue.ID]; ok {
			if !existing.Done {
				m.statuses[i] = updated
			}
		}
	}
	m.sortStatuses()
	m.dashboard.setStatuses(m.statuses)
}

func (m *Model) sortStatuses() {
	sort.Slice(m.statuses, func(i, j int) bool {
		si, sj := m.statuses[i], m.statuses[j]
		// Active first, then done, then failed.
		pi, pj := stepPriority(si.Step), stepPriority(sj.Step)
		if pi != pj {
			return pi < pj
		}
		return si.Issue.Identifier < sj.Issue.Identifier
	})
}

func stepPriority(step jiradozer.WorkflowStep) int {
	switch step {
	case jiradozer.StepDone:
		return 2
	case jiradozer.StepFailed:
		return 3
	default:
		return 1
	}
}

func (m *Model) listenForStatus() tea.Cmd {
	ch := m.orchestrator.StatusUpdates()
	return func() tea.Msg {
		s, ok := <-ch
		if !ok {
			return tea.Quit
		}
		return issueStatusMsg{Status: s}
	}
}

func (m *Model) tickRefresh() tea.Cmd {
	return tea.Tick(time.Second, func(_ time.Time) tea.Msg {
		return statusTickMsg{}
	})
}

// ViewForTest exposes the view content for testing without running the full program.
func (m *Model) ViewForTest() string {
	return m.viewContent()
}

// SetSize sets the terminal dimensions (for testing).
func (m *Model) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.dashboard.height = h - 6
	m.detail.height = h - 4
}

// AddStatusForTest adds a status entry directly (for testing without orchestrator).
func (m *Model) AddStatusForTest(s jiradozer.IssueStatus) {
	m.updateStatus(s)
}

// SimulateUpdate processes a message and returns the command (for testing).
func (m *Model) SimulateUpdate(msg tea.Msg) tea.Cmd {
	_, cmd := m.Update(msg)
	return cmd
}

// FocusArea is the type for tracking which view is active.
type FocusArea = focusArea

// FocusDashboard is the dashboard focus area constant.
const FocusDashboard = focusDashboard

// FocusDetail is the detail focus area constant.
const FocusDetail = focusDetail

// GetFocus returns the current focus area (for testing).
func (m *Model) GetFocus() FocusArea { return m.focus }

// Statuses returns the current status list (for testing).
func (m *Model) Statuses() []jiradozer.IssueStatus { return m.statuses }
