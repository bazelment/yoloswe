// Package app provides the root TUI application model.
package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

// FocusArea indicates which area has focus.
type FocusArea int

const (
	FocusOutput           FocusArea = iota // Main center area (default)
	FocusInput                             // Input line at bottom
	FocusWorktreeDropdown                  // Alt-W dropdown open
	FocusSessionDropdown                   // Alt-S dropdown open
	FocusTaskModal                         // Task modal open
)

// Model is the root application model.
type Model struct {
	ctx                   context.Context
	inputHandler          func(string) tea.Cmd
	viewingHistoryData    *session.StoredSession
	sessionManager        *session.Manager
	mdRenderer            *MarkdownRenderer
	worktreeStatuses      map[string]*wt.WorktreeStatus
	worktreeDropdown      *Dropdown
	sessionDropdown       *Dropdown
	taskModal             *TaskModal
	inputArea             *TextArea
	pendingPlannerPrompt  string
	pendingWorktreeSelect string
	repoName              string
	editor                string
	inputPrompt           string
	viewingSessionID      session.SessionID
	wtRoot                string
	lastError             string
	worktrees             []wt.Worktree
	sessions              []session.SessionInfo
	worktreeOpMessages    []string
	focus                 FocusArea
	scrollOffset          int
	height                int
	width                 int
	inputMode             bool
}

// NewModel creates a new root model for a specific repo.
func NewModel(ctx context.Context, wtRoot, repoName, editor string, sessionManager *session.Manager) Model {
	if editor == "" {
		editor = "code"
	}
	m := Model{
		ctx:              ctx,
		wtRoot:           wtRoot,
		repoName:         repoName,
		editor:           editor,
		sessionManager:   sessionManager,
		focus:            FocusOutput,
		worktreeDropdown: NewDropdown(nil),
		sessionDropdown:  NewDropdown(nil),
		taskModal:        NewTaskModal(),
		inputArea:        NewTextArea(),
	}

	return m
}

// Init initializes the model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.refreshWorktrees(),
		m.listenForSessionEvents(),
		tickCmd(),
	)
}

// tickCmd returns a command that sends a tick message every 100ms.
func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg{time: t}
	})
}

// refreshWorktrees fetches worktrees for the current repo.
func (m Model) refreshWorktrees() tea.Cmd {
	if m.repoName == "" {
		return nil
	}
	manager := wt.NewManager(m.wtRoot, m.repoName)

	return func() tea.Msg {
		worktrees, err := manager.List(m.ctx)
		if err != nil {
			return errMsg{err}
		}
		return worktreesMsg{worktrees}
	}
}

// listenForSessionEvents listens for session manager events.
func (m Model) listenForSessionEvents() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-m.ctx.Done():
			return nil
		case event := <-m.sessionManager.Events():
			return sessionEventMsg{event}
		}
	}
}

// selectedWorktree returns the currently selected worktree.
func (m *Model) selectedWorktree() *wt.Worktree {
	item := m.worktreeDropdown.SelectedItem()
	if item == nil {
		return nil
	}
	for i := range m.worktrees {
		if m.worktrees[i].Branch == item.ID {
			return &m.worktrees[i]
		}
	}
	return nil
}

// selectedSession returns the currently selected/viewing session.
func (m *Model) selectedSession() *session.SessionInfo {
	if m.viewingSessionID == "" {
		return nil
	}
	info, ok := m.sessionManager.GetSessionInfo(m.viewingSessionID)
	if !ok {
		return nil
	}
	return &info
}

// currentWorktreeSessions returns sessions for the current worktree.
func (m *Model) currentWorktreeSessions() []session.SessionInfo {
	wt := m.selectedWorktree()
	if wt == nil {
		return nil
	}
	return m.sessionManager.GetSessionsForWorktree(wt.Path)
}

// updateWorktreeDropdown updates the worktree dropdown items.
func (m *Model) updateWorktreeDropdown() {
	items := make([]DropdownItem, len(m.worktrees))
	for i, w := range m.worktrees {
		// Count sessions for badge
		sessionCount := len(m.sessionManager.GetSessionsForWorktree(w.Path))
		badge := ""
		if sessionCount > 0 {
			badge = statusBadge(sessionCount)
		}

		// Build label with inline status
		label := w.Branch
		if m.worktreeStatuses != nil {
			if s, ok := m.worktreeStatuses[w.Branch]; ok {
				label += "  " + formatWorktreeStatus(s)
			}
		}

		items[i] = DropdownItem{
			ID:    w.Branch,
			Label: label,
			Badge: badge,
		}
	}
	m.worktreeDropdown.SetItems(items)
}

// updateSessionDropdown updates the session dropdown items.
// Includes both live sessions and history sessions from the store.
func (m *Model) updateSessionDropdown() {
	var items []DropdownItem

	// Add live sessions first
	sessions := m.currentWorktreeSessions()
	for i := range sessions {
		sess := &sessions[i]
		// Type icon
		icon := "📋" // planner
		if sess.Type == session.SessionTypeBuilder {
			icon = "🔨"
		}

		// Status badge
		badge := statusIcon(sess.Status)

		// Use title if available, otherwise derive from prompt
		label := sess.Title
		if label == "" {
			label = generateDropdownTitle(sess.Prompt, 20)
		}

		// Truncate prompt for subtitle
		subtitle := truncate(sess.Prompt, 40)

		items = append(items, DropdownItem{
			ID:       string(sess.ID),
			Label:    label,
			Subtitle: subtitle,
			Icon:     icon,
			Badge:    badge,
		})
	}

	// Add separator if we have both live and history
	historySessions, _ := m.loadHistorySessions()
	if len(items) > 0 && len(historySessions) > 0 {
		items = append(items, DropdownItem{
			ID:    "---separator---",
			Label: "─── History ───",
		})
	}

	// Add history sessions (that aren't already in live sessions)
	liveIDs := make(map[string]bool)
	for i := range sessions {
		liveIDs[string(sessions[i].ID)] = true
	}
	for _, hist := range historySessions {
		if liveIDs[string(hist.ID)] {
			continue // Skip if already in live list
		}

		icon := "📋"
		if hist.Type == session.SessionTypeBuilder {
			icon = "🔨"
		}

		badge := dimStyle.Render("(history)")

		// Use title if available, otherwise derive from prompt
		label := hist.Title
		if label == "" {
			label = generateDropdownTitle(hist.Prompt, 20)
		}

		subtitle := truncate(hist.Prompt, 40)

		items = append(items, DropdownItem{
			ID:       string(hist.ID),
			Label:    label,
			Subtitle: subtitle,
			Icon:     icon,
			Badge:    badge,
		})
	}

	m.sessionDropdown.SetItems(items)
}

// loadHistorySessions loads history sessions for the current worktree.
func (m *Model) loadHistorySessions() ([]*session.SessionMeta, error) {
	wt := m.selectedWorktree()
	if wt == nil {
		return nil, nil
	}
	return m.sessionManager.LoadHistorySessions(wt.Branch)
}

func statusBadge(count int) string {
	return dimStyle.Render("[" + string(rune('0'+count%10)) + "]")
}

// refreshWorktreeStatuses fetches status for all worktrees in the background.
func (m Model) refreshWorktreeStatuses() tea.Cmd {
	if m.repoName == "" || len(m.worktrees) == 0 {
		return nil
	}
	worktrees := make([]wt.Worktree, len(m.worktrees))
	copy(worktrees, m.worktrees)
	wtRoot := m.wtRoot
	repoName := m.repoName
	ctx := m.ctx

	return func() tea.Msg {
		manager := wt.NewManager(wtRoot, repoName)
		statuses := make(map[string]*wt.WorktreeStatus)
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, w := range worktrees {
			wg.Add(1)
			go func(w wt.Worktree) {
				defer wg.Done()
				status, err := manager.GetStatus(ctx, w)
				if err != nil {
					return
				}
				mu.Lock()
				statuses[w.Branch] = status
				mu.Unlock()
			}(w)
		}
		wg.Wait()
		return worktreeStatusMsg{statuses: statuses}
	}
}

// formatWorktreeStatus formats a WorktreeStatus for dropdown subtitle display.
func formatWorktreeStatus(s *wt.WorktreeStatus) string {
	var parts []string

	if s.IsDirty {
		parts = append(parts, "dirty")
	} else {
		parts = append(parts, "clean")
	}

	if s.Ahead > 0 || s.Behind > 0 {
		var ab []string
		if s.Ahead > 0 {
			ab = append(ab, fmt.Sprintf("↑%d", s.Ahead))
		}
		if s.Behind > 0 {
			ab = append(ab, fmt.Sprintf("↓%d", s.Behind))
		}
		parts = append(parts, strings.Join(ab, " "))
	}

	if s.PRNumber > 0 {
		parts = append(parts, fmt.Sprintf("PR#%d %s", s.PRNumber, s.PRState))
	}

	if !s.LastCommitTime.IsZero() {
		parts = append(parts, timeAgo(s.LastCommitTime))
	}

	return strings.Join(parts, " | ")
}

// timeAgo returns a human-readable relative time string.
func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// generateDropdownTitle creates a short title from a prompt for dropdown display.
func generateDropdownTitle(prompt string, maxLen int) string {
	words := strings.Fields(prompt)
	var b strings.Builder
	for _, w := range words {
		if b.Len()+len(w)+1 > maxLen {
			break
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	if b.Len() == 0 && prompt != "" {
		if len(prompt) > maxLen-3 {
			return prompt[:maxLen-3] + "..."
		}
		return prompt
	}
	return b.String()
}

// Message types
type (
	errMsg            struct{ error }
	worktreesMsg      struct{ worktrees []wt.Worktree }
	sessionEventMsg   struct{ event interface{} }
	sessionsUpdated   struct{}
	promptInputMsg    struct{ value string }
	startSessionMsg struct {
		sessionType session.SessionType
		prompt      string
	}
	createWorktreeMsg struct{ branch string }
	editorResultMsg   struct{ err error }
	taskRouteMsg      struct{ prompt string }
	taskProposalMsg   struct {
		proposal *RouteProposal
		err      error
	}
	taskConfirmMsg struct {
		worktree string
		parent   string
		prompt   string
		isNew    bool
	}
	// worktreeOpResultMsg contains the result of a worktree operation
	worktreeOpResultMsg struct {
		err      error
		messages []string
	}
	// taskWorktreeCreatedMsg is sent when a worktree is created for a task (then planner should start)
	taskWorktreeCreatedMsg struct {
		worktreeName string
		prompt       string
		messages     []string
	}
	// tickMsg is sent periodically to update running tool timers
	tickMsg struct {
		time time.Time
	}
	// worktreeStatusMsg carries refreshed worktree statuses
	worktreeStatusMsg struct {
		statuses map[string]*wt.WorktreeStatus
	}
	// refreshStatusTickMsg triggers a periodic status refresh
	refreshStatusTickMsg struct{}
	// deleteWorktreeMsg is sent to delete a worktree
	deleteWorktreeMsg struct {
		branch       string
		deleteBranch bool
	}
)

// RouteProposal wraps taskrouter.RouteProposal for use in the app.
type RouteProposal = struct {
	Action    string
	Worktree  string
	Parent    string
	Reasoning string
}
