// Package app provides the root TUI application model.
package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

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
	FocusHelp                              // Help overlay open
)

// Model is the root application model.
type Model struct { //nolint:govet // fieldalignment: readability over padding for app state
	ctx                   context.Context
	worktrees             []wt.Worktree
	sessions              []session.SessionInfo
	cachedHistory         []*session.SessionMeta
	worktreeOpMessages    []string
	inputHandler          func(string) tea.Cmd
	worktreeStatuses      map[string]*wt.WorktreeStatus
	scrollPositions       map[session.SessionID]int
	viewingHistoryData    *session.StoredSession
	sessionManager        *session.Manager
	mdRenderer            *MarkdownRenderer
	worktreeDropdown      *Dropdown
	sessionDropdown       *Dropdown
	taskModal             *TaskModal
	toasts                *ToastManager
	helpOverlay           *HelpOverlay
	inputArea             *TextArea
	splitPane             *SplitPane
	fileTree              *FileTree
	pendingPlannerPrompt  string
	pendingWorktreeSelect string
	repoName              string
	editor                string
	inputPrompt           string
	wtRoot                string
	viewingSessionID      session.SessionID
	historyBranch         string
	scrollOffset          int
	selectedSessionIndex  int
	height                int
	width                 int
	focus                 FocusArea
	inputMode             bool
	confirmQuit           bool
}

// NewModel creates a new root model for a specific repo.
// If initialWorktrees is non-nil, the model is pre-populated so the first
// render shows branch names immediately without waiting for an async refresh.
// width/height set the initial terminal dimensions so the first View() can
// render a proper layout without waiting for WindowSizeMsg.
func NewModel(ctx context.Context, wtRoot, repoName, editor string, sessionManager *session.Manager, initialWorktrees []wt.Worktree, width, height int) Model {
	if editor == "" {
		editor = "code"
	}
	wtDropdown := NewDropdown(nil)
	wtDropdown.SetMaxVisible(20)

	m := Model{
		ctx:              ctx,
		wtRoot:           wtRoot,
		repoName:         repoName,
		editor:           editor,
		sessionManager:   sessionManager,
		focus:            FocusOutput,
		width:            width,
		height:           height,
		worktreeDropdown: wtDropdown,
		sessionDropdown:  NewDropdown(nil),
		taskModal:        NewTaskModal(),
		toasts:           NewToastManager(),
		helpOverlay:      NewHelpOverlay(),
		inputArea:        NewTextArea(),
		splitPane:        NewSplitPane(),
		fileTree:         NewFileTree("", nil),
		scrollPositions:  make(map[session.SessionID]int),
	}

	// Pre-populate worktrees so the first View() render shows branch names.
	if len(initialWorktrees) > 0 {
		m.worktrees = initialWorktrees
		m.updateWorktreeDropdown()
		m.worktreeDropdown.SelectIndex(0)
		m.updateSessionDropdown()
	}

	return m
}

// Init initializes the model.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		m.listenForSessionEvents(),
		tickCmd(),
	}

	if len(m.worktrees) > 0 {
		// Worktrees were pre-loaded — skip the initial refresh and go
		// straight to deferred loading of statuses, file tree, and history.
		cmds = append(cmds, deferredRefreshCmd())
	} else {
		// No pre-loaded data; fetch worktrees asynchronously.
		cmds = append(cmds, m.refreshWorktrees())
	}

	return tea.Batch(cmds...)
}

// tickCmd returns a command that sends a tick message every 100ms.
func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg{time: t}
	})
}

// deferredRefreshCmd schedules a deferred refresh after a short delay,
// allowing the UI to render with just worktree names before loading
// git statuses, file tree, and history sessions.
func deferredRefreshCmd() tea.Cmd {
	return tea.Tick(time.Millisecond, func(time.Time) tea.Msg {
		return deferredRefreshMsg{}
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

// aggregateCost returns the sum of TotalCostUSD across all sessions.
func (m *Model) aggregateCost() float64 {
	var total float64
	for i := range m.sessions {
		total += m.sessions[i].Progress.TotalCostUSD
	}
	return total
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

		label := w.Branch

		// Build subtitle with status details
		var subtitle string
		if m.worktreeStatuses != nil {
			if s, ok := m.worktreeStatuses[w.Branch]; ok {
				subtitle = formatWorktreeStatus(s, sessionCount)
			}
		}
		if subtitle == "" && sessionCount > 0 {
			subtitle = dimStyle.Render(fmt.Sprintf("%d sessions", sessionCount))
		}

		items[i] = DropdownItem{
			ID:       w.Branch,
			Label:    label,
			Subtitle: subtitle,
		}
	}
	m.worktreeDropdown.SetItems(items)
}

// updateSessionDropdown updates the session dropdown items.
// Uses live sessions immediately and cached history (loaded async).
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

		// Add index prefix for sessions 1-9
		indexPrefix := ""
		if i < 9 {
			indexPrefix = fmt.Sprintf("%d. ", i+1)
		}

		// Format rich subtitle with progress and prompt
		subtitle := formatSessionSubtitle(sess)

		items = append(items, DropdownItem{
			ID:       string(sess.ID),
			Label:    indexPrefix + label,
			Subtitle: subtitle,
			Icon:     icon,
			Badge:    badge,
		})
	}

	// Use cached history for the current worktree (loaded async)
	w := m.selectedWorktree()
	currentBranch := ""
	if w != nil {
		currentBranch = w.Branch
	}
	var historySessions []*session.SessionMeta
	if currentBranch != "" && m.historyBranch == currentBranch {
		historySessions = m.cachedHistory
	}

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

// formatSessionSubtitle builds a rich subtitle for a live session dropdown item.
// Shows progress (turns, cost, elapsed) when available, followed by prompt excerpt.
func formatSessionSubtitle(sess *session.SessionInfo) string {
	var parts []string

	// Progress prefix: only show when session has started doing work
	if sess.Progress.TurnCount > 0 || sess.Progress.TotalCostUSD > 0 {
		parts = append(parts, fmt.Sprintf("T:%d $%.4f", sess.Progress.TurnCount, sess.Progress.TotalCostUSD))
	}

	// Elapsed time since creation (only when set and within a reasonable range)
	if !sess.CreatedAt.IsZero() && time.Since(sess.CreatedAt) < 365*24*time.Hour {
		parts = append(parts, timeAgo(sess.CreatedAt))
	}

	// Build prefix
	prefix := ""
	if len(parts) > 0 {
		prefix = strings.Join(parts, " ") + " | "
	}

	// Remaining budget for prompt (use runewidth for correct column count)
	maxPromptLen := 40 - runewidth.StringWidth(prefix)
	if maxPromptLen < 10 {
		maxPromptLen = 10
	}

	return prefix + truncate(sess.Prompt, maxPromptLen)
}

// refreshHistorySessions loads history sessions from disk asynchronously.
func (m Model) refreshHistorySessions() tea.Cmd {
	w := m.selectedWorktree()
	if w == nil {
		return nil
	}
	branch := w.Branch
	mgr := m.sessionManager
	return func() tea.Msg {
		sessions, _ := mgr.LoadHistorySessions(branch)
		return historySessionsMsg{branch: branch, sessions: sessions}
	}
}

// refreshWorktreeStatuses fetches status for each worktree in two phases:
// 1. Fast: local git status (dirty, ahead/behind, last commit) — updates UI immediately
// 2. Slow: PR info from GitHub API — trickles in as each network call completes
func (m Model) refreshWorktreeStatuses() tea.Cmd {
	if m.repoName == "" || len(m.worktrees) == 0 {
		return nil
	}
	wtRoot := m.wtRoot
	repoName := m.repoName
	ctx := m.ctx

	cmds := make([]tea.Cmd, 0, len(m.worktrees)*2+1)
	for _, w := range m.worktrees {
		w := w // capture loop variable

		// Fast: git-only status (no network), then slow: PR info (network call to GitHub)
		cmds = append(cmds, func() tea.Msg {
			manager := wt.NewManager(wtRoot, repoName)
			status, err := manager.GetGitStatus(ctx, w)
			if err != nil {
				return nil
			}
			return singleWorktreeStatusMsg{branch: w.Branch, status: status}
		}, func() tea.Msg {
			manager := wt.NewManager(wtRoot, repoName)
			pr, err := manager.FetchPRInfo(ctx, w)
			if err != nil || pr == nil {
				return nil
			}
			return worktreePRInfoMsg{branch: w.Branch, pr: pr}
		})
	}

	// Schedule the next periodic refresh
	cmds = append(cmds, tea.Tick(30*time.Second, func(t time.Time) tea.Msg {
		return refreshStatusTickMsg{}
	}))

	return tea.Batch(cmds...)
}

// formatWorktreeStatus formats a WorktreeStatus for dropdown subtitle display with colors.
func formatWorktreeStatus(s *wt.WorktreeStatus, sessionCount int) string {
	var parts []string

	if s.IsDirty {
		parts = append(parts, failedStyle.Render("dirty"))
	} else {
		parts = append(parts, completedStyle.Render("clean"))
	}

	if s.Ahead > 0 || s.Behind > 0 {
		var ab []string
		if s.Ahead > 0 {
			ab = append(ab, runningStyle.Render(fmt.Sprintf("↑%d", s.Ahead)))
		}
		if s.Behind > 0 {
			ab = append(ab, pendingStyle.Render(fmt.Sprintf("↓%d", s.Behind)))
		}
		parts = append(parts, strings.Join(ab, " "))
	}

	if s.PRNumber > 0 {
		prText := fmt.Sprintf("PR#%d %s", s.PRNumber, s.PRState)
		switch s.PRState {
		case "OPEN":
			prText = fmt.Sprintf("PR#%d", s.PRNumber) + " " + runningStyle.Render("OPEN")
			if s.PRIsDraft {
				prText = fmt.Sprintf("PR#%d", s.PRNumber) + " " + dimStyle.Render("DRAFT")
			}
			if s.PRReviewStatus == "APPROVED" {
				prText += " " + completedStyle.Render("✓approved")
			} else if s.PRReviewStatus == "CHANGES_REQUESTED" {
				prText += " " + failedStyle.Render("changes requested")
			}
		case "MERGED":
			prText = fmt.Sprintf("PR#%d", s.PRNumber) + " " + idleStyle.Render("MERGED")
		case "CLOSED":
			prText = fmt.Sprintf("PR#%d", s.PRNumber) + " " + dimStyle.Render("CLOSED")
		}
		parts = append(parts, prText)
	}

	if !s.LastCommitTime.IsZero() {
		parts = append(parts, dimStyle.Render(timeAgo(s.LastCommitTime)))
	}

	if sessionCount > 0 {
		parts = append(parts, idleStyle.Render(fmt.Sprintf("%d sessions", sessionCount)))
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
	var parts []string
	cols := 0
	for _, w := range words {
		wWidth := runewidth.StringWidth(w)
		needed := wWidth
		if cols > 0 {
			needed++ // space separator
		}
		if cols+needed > maxLen {
			break
		}
		parts = append(parts, w)
		cols += needed
	}
	if len(parts) == 0 && prompt != "" {
		return truncate(prompt, maxLen)
	}
	return strings.Join(parts, " ")
}

// refreshFileTree gathers worktree context for the file tree display.
func (m Model) refreshFileTree() tea.Cmd {
	w := m.selectedWorktree()
	if w == nil {
		return nil
	}
	wtRoot := m.wtRoot
	repoName := m.repoName
	ctx := m.ctx
	worktree := *w
	return func() tea.Msg {
		manager := wt.NewManager(wtRoot, repoName)
		opts := wt.ContextOptions{
			IncludeFileList: true,
		}
		wtCtx, err := manager.GatherContext(ctx, worktree, opts)
		if err != nil {
			return nil
		}
		return fileTreeContextMsg{
			worktreePath: worktree.Path,
			wtCtx:        wtCtx,
		}
	}
}

// Message types
type (
	errMsg          struct{ error }
	worktreesMsg    struct{ worktrees []wt.Worktree }
	sessionEventMsg struct{ event interface{} }
	sessionsUpdated struct{}
	promptInputMsg  struct{ value string }
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
	// singleWorktreeStatusMsg carries the git-only status for one worktree (fast, local).
	singleWorktreeStatusMsg struct {
		status *wt.WorktreeStatus
		branch string
	}
	// worktreePRInfoMsg carries PR info for one worktree (slow, network).
	worktreePRInfoMsg struct {
		pr     *wt.PRInfo
		branch string
	}
	// fileTreeContextMsg carries gathered worktree context for the file tree
	fileTreeContextMsg struct {
		wtCtx        *wt.WorktreeContext
		worktreePath string
	}
	// historySessionsMsg carries async-loaded history sessions for a worktree.
	historySessionsMsg struct {
		branch   string
		sessions []*session.SessionMeta
	}
	// refreshStatusTickMsg triggers a periodic status refresh
	refreshStatusTickMsg struct{}
	// deferredRefreshMsg is sent after a short delay so the initial UI
	// renders with just worktree names before loading statuses/file tree.
	deferredRefreshMsg struct{}
	// deleteWorktreeMsg is sent to delete a worktree
	deleteWorktreeMsg struct {
		branch       string
		deleteBranch bool
	}
	// tmuxWindowMsg carries the result of opening a new tmux window.
	tmuxWindowMsg struct{ err error }
	// toastExpireMsg is sent when a toast timer fires to check for expired toasts.
	toastExpireMsg struct{}
)

// RouteProposal wraps taskrouter.RouteProposal for use in the app.
type RouteProposal = struct {
	Action    string
	Worktree  string
	Parent    string
	Reasoning string
}

// addToast adds a notification and schedules expiry if this is the first toast.
func (m *Model) addToast(message string, level ToastLevel) tea.Cmd {
	m.toasts.Add(message, level)
	// Schedule a tick to check for expiration.
	// We schedule at the earliest expiration time of any active toast.
	return m.scheduleToastExpiry()
}

// scheduleToastExpiry schedules a tea.Tick at the earliest toast expiration time.
func (m *Model) scheduleToastExpiry() tea.Cmd {
	if !m.toasts.HasToasts() {
		return nil
	}
	// Find the earliest expiration
	earliest := m.toasts.toasts[0].CreatedAt.Add(m.toasts.toasts[0].Duration)
	for _, t := range m.toasts.toasts[1:] {
		exp := t.CreatedAt.Add(t.Duration)
		if exp.Before(earliest) {
			earliest = exp
		}
	}
	delay := time.Until(earliest)
	if delay < 0 {
		delay = 0
	}
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return toastExpireMsg{}
	})
}
