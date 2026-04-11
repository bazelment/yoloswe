// Package app provides the root TUI application model.
package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/taskrouter"
	"github.com/bazelment/yoloswe/multiagent/agent"
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
	FocusConfirm                           // Single-keypress confirmation prompt
	FocusAllSessions                       // All sessions overlay open
	FocusThemePicker                       // Theme picker overlay open
	FocusRepoSettings                      // Repo settings overlay open
	FocusRepoDropdown                      // Alt-R repo dropdown open
	FocusCommandCenter                     // Command center full-screen view
)

// Model is the root application model.
type Model struct { //nolint:govet // fieldalignment: readability over packing
	settings              Settings
	ctx                   context.Context
	toasts                *ToastManager
	confirmHandler        func(string) tea.Cmd
	confirmPrompt         *ConfirmPrompt
	worktreeStatuses      map[string]*wt.WorktreeStatus
	scrollPositions       map[session.SessionID]int
	viewingHistoryData    *session.StoredSession
	sessionManager        *session.Manager
	taskRouter            *taskrouter.Router
	mdRenderer            *MarkdownRenderer
	worktreeDropdown      *Dropdown
	sessionDropdown       *Dropdown
	allSessionsOverlay    *AllSessionsOverlay
	commandCenter         *CommandCenter
	confirmCancelHandler  func() tea.Cmd
	providerAvailability  *agent.ProviderAvailability
	taskModal             *TaskModal
	themePicker           *ThemePicker
	repoSettingsDialog    *RepoSettingsDialog
	repos                 map[string]*RepoContext
	repoDropdown          *Dropdown
	fileTree              *FileTree
	splitPane             *SplitPane
	inputArea             *TextArea
	modelRegistry         *agent.ModelRegistry
	sharedEvents          chan repoSessionEvent
	helpOverlay           *HelpOverlay
	styles                *Styles
	inputHandler          func(value, model string, sessionType session.SessionType) tea.Cmd
	sharedManagerConfig   session.ManagerConfig
	pendingModel          string
	repoName              string
	historyBranch         string
	viewingSessionID      session.SessionID
	pendingPlannerPrompt  string
	pendingWorktreeSelect string
	defaultBuildModel     string
	editor                string
	inputPrompt           string
	wtRoot                string
	pendingSessionType    session.SessionType
	defaultPlanModel      string
	defaultCodeTalkModel  string
	openedRepos           []string
	resumeRepos           []string
	cachedHistory         []*session.SessionMeta
	worktrees             []wt.Worktree
	sessions              []session.SessionInfo
	worktreeOpMessages    []string
	scrollOffset          int
	selectedSessionIndex  int
	height                int
	width                 int
	focus                 FocusArea
	inputMode             bool
	confirmQuit           bool
	// Voice reporting.
	voiceReporter *VoiceReporter
}

// NewModel creates a new root model for a specific repo.
// If initialWorktrees is non-nil, the model is pre-populated so the first
// render shows branch names immediately without waiting for an async refresh.
// width/height set the initial terminal dimensions so the first View() can
// render a proper layout without waiting for WindowSizeMsg.
func NewModel(ctx context.Context, wtRoot, repoName, editor string, sessionManager *session.Manager, taskRouter *taskrouter.Router, initialWorktrees []wt.Worktree, width, height int, providerAvailability *agent.ProviderAvailability, modelRegistry *agent.ModelRegistry, sharedManagerConfig session.ManagerConfig, resumeRepos []string) Model {
	if editor == "" {
		editor = "code"
	}
	wtDropdown := NewDropdown(nil)

	// Load settings and resolve theme
	settings := LoadSettings()
	palette := Dark
	if p, ok := ThemeByName(settings.ThemeName); ok {
		palette = p
	}
	styles := NewStyles(palette)

	// Resolve default models from the registry (prefer claude if available)
	defaultPlanModel := "opus"
	defaultCodeTalkModel := "opus"
	defaultBuildModel := "sonnet"
	if modelRegistry != nil {
		if m, ok := modelRegistry.ModelByID("opus"); ok {
			defaultPlanModel = m.ID
			defaultCodeTalkModel = m.ID
		} else if models := modelRegistry.Models(); len(models) > 0 {
			defaultPlanModel = models[0].ID
			defaultCodeTalkModel = models[0].ID
		}
		if m, ok := modelRegistry.ModelByID("sonnet"); ok {
			defaultBuildModel = m.ID
		} else if models := modelRegistry.Models(); len(models) > 0 {
			defaultBuildModel = models[0].ID
		}
	}

	sharedEvents := make(chan repoSessionEvent, 64)

	m := Model{
		ctx:                  ctx,
		wtRoot:               wtRoot,
		repoName:             repoName,
		editor:               editor,
		sessionManager:       sessionManager,
		taskRouter:           taskRouter,
		providerAvailability: providerAvailability,
		modelRegistry:        modelRegistry,
		repos:                make(map[string]*RepoContext),
		openedRepos:          []string{repoName},
		repoDropdown:         NewDropdown(nil),
		sharedEvents:         sharedEvents,
		sharedManagerConfig:  sharedManagerConfig,
		styles:               styles,
		settings:             settings,
		themePicker:          NewThemePicker(),
		repoSettingsDialog:   NewRepoSettingsDialog(),
		focus:                FocusOutput,
		width:                width,
		height:               height,
		defaultPlanModel:     defaultPlanModel,
		defaultCodeTalkModel: defaultCodeTalkModel,
		defaultBuildModel:    defaultBuildModel,
		worktreeDropdown:     wtDropdown,
		sessionDropdown:      NewDropdown(nil),
		taskModal:            NewTaskModal(),
		toasts:               NewToastManager(),
		helpOverlay:          NewHelpOverlay(),
		allSessionsOverlay:   NewAllSessionsOverlay(),
		commandCenter:        NewCommandCenter(),
		inputArea:            NewTextArea(),
		splitPane:            NewSplitPane(),
		fileTree:             NewFileTree("", nil),
		scrollPositions:      make(map[session.SessionID]int),
		resumeRepos:          resumeRepos,
	}

	// Sync placeholder colors with the loaded theme (NewTextArea defaults to "245")
	dimColor := lipgloss.Color(palette.Dim)
	m.inputArea.SetPlaceholderColor(dimColor)
	m.taskModal.SetPlaceholderColor(dimColor)
	m.repoSettingsDialog.SetSize(width, height)
	m.configureAllDropdownsForViewport()

	// Pre-populate worktrees so the first View() render shows branch names.
	if len(initialWorktrees) > 0 {
		m.worktrees = initialWorktrees
		m.updateWorktreeDropdown()
		m.worktreeDropdown.SelectIndex(0)
		m.updateSessionDropdown()
	}

	// Create initial RepoContext for the startup repo.
	m.repos[repoName] = &RepoContext{
		sessionManager:   sessionManager,
		taskRouter:       taskRouter,
		worktrees:        m.worktrees,
		worktreeDropdown: m.worktreeDropdown,
		sessionDropdown:  m.sessionDropdown,
		scrollPositions:  m.scrollPositions,
	}

	// Start fan-in goroutine for the initial manager.
	go fanInEvents(m.ctx, repoName, sessionManager, sharedEvents)

	return m
}

// SetVoiceReporter configures voice reporting on the model.
// Must be called after NewModel and before Init.
func (m *Model) SetVoiceReporter(reporter *VoiceReporter) {
	m.voiceReporter = reporter
}

// reportSessionVoice generates a voice report for a completed session.
// Runs in a background goroutine to avoid blocking the TUI event loop.
func (m *Model) reportSessionVoice(info session.SessionInfo) {
	if m.voiceReporter == nil {
		return
	}

	reporter := m.voiceReporter
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), SynthesisTimeout)
		defer cancel()
		reporter.Report(ctx, info)
	}()
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

	// Auto-open repos that have live tmux sessions from a previous run.
	if len(m.resumeRepos) > 0 {
		repos := m.resumeRepos
		cmds = append(cmds, func() tea.Msg {
			return resumeReposMsg{repos: repos}
		})
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
	repoName := m.repoName
	manager := wt.NewManager(m.wtRoot, repoName)

	return func() tea.Msg {
		worktrees, err := manager.List(m.ctx)
		if err != nil {
			return errMsg{err}
		}
		return worktreesMsg{worktrees: worktrees, repoName: repoName}
	}
}

// listenForSessionEvents listens for session events from all opened repos
// via the shared fan-in channel.
func (m Model) listenForSessionEvents() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-m.ctx.Done():
			return nil
		case ev := <-m.sharedEvents:
			return repoSessionEventMsg{repoName: ev.repoName, event: ev.event}
		}
	}
}

// fanInEvents forwards events from a single manager to the shared channel.
func fanInEvents(ctx context.Context, repoName string, mgr *session.Manager, out chan<- repoSessionEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-mgr.Events():
			if !ok {
				return
			}
			select {
			case out <- repoSessionEvent{repoName: repoName, event: event}:
			case <-ctx.Done():
				return
			}
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
	if ok {
		return &info
	}
	// Fall back to history data if viewing a persisted session
	if m.viewingHistoryData != nil {
		si := session.StoredToSessionInfo(m.viewingHistoryData)
		return &si
	}
	return nil
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

// visibleSessions returns the sessions that should be displayed in the session list.
// Returns sessions for the current worktree only.
func (m *Model) visibleSessions() []session.SessionInfo {
	return m.currentWorktreeSessions()
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
			if st, ok := m.worktreeStatuses[w.Branch]; ok {
				subtitle = formatWorktreeStatus(st, sessionCount, m.styles)
			}
		}
		if subtitle == "" && sessionCount > 0 {
			subtitle = m.styles.Dim.Render(fmt.Sprintf("%d sessions", sessionCount))
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
	sessions := m.visibleSessions()
	for i := range sessions {
		sess := &sessions[i]
		// Type icon
		icon := sessionTypeEmojiIcon(sess.Type)

		// Status badge
		badge := statusIcon(sess.Status, m.styles)

		// Prefer a human-readable title; fall back to tmux window name, then prompt.
		label := sess.Title
		if label == "" {
			label = sess.TmuxWindowName
		}
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

	// Use cached history for the current worktree (loaded async).
	// Compare by directory name (w.Name()) since historyBranch stores the
	// directory name, not the git branch — this survives branch checkouts.
	w := m.selectedWorktree()
	currentName := ""
	if w != nil {
		currentName = w.Name()
	}
	var historySessions []*session.SessionMeta
	if currentName != "" && m.historyBranch == currentName {
		historySessions = m.cachedHistory
	}

	if len(items) > 0 && len(historySessions) > 0 {
		items = append(items, DropdownItem{
			ID:    dropdownSeparatorID,
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
		} else if hist.Type == session.SessionTypeCodeTalk {
			icon = "💬"
		}

		badge := m.styles.Dim.Render("(history)")

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
	branch := w.Name()
	repoName := m.repoName
	mgr := m.sessionManager
	return func() tea.Msg {
		sessions, _ := mgr.LoadHistorySessions(branch)
		return historySessionsMsg{branch: branch, sessions: sessions, repoName: repoName}
	}
}

// fetchGitStatuses fetches local git status for each worktree (no network).
// Does NOT schedule the next tick — callers must manage timers separately.
func (m Model) fetchGitStatuses() tea.Cmd {
	if m.repoName == "" || len(m.worktrees) == 0 {
		return nil
	}
	wtRoot := m.wtRoot
	repoName := m.repoName
	ctx := m.ctx

	cmds := make([]tea.Cmd, 0, len(m.worktrees))
	for _, w := range m.worktrees {
		w := w // capture loop variable
		cmds = append(cmds, func() tea.Msg {
			manager := wt.NewManager(wtRoot, repoName)
			status, err := manager.GetGitStatus(ctx, w)
			if err != nil {
				return nil
			}
			return singleWorktreeStatusMsg{branch: w.Branch, status: status, repoName: repoName}
		})
	}

	return tea.Batch(cmds...)
}

// fetchPRStatuses fetches all open PRs in a single batch API call.
// Does NOT schedule the next tick — callers must manage timers separately.
func (m Model) fetchPRStatuses() tea.Cmd {
	if m.repoName == "" || len(m.worktrees) == 0 {
		return nil
	}
	wtRoot := m.wtRoot
	repoName := m.repoName
	ctx := m.ctx
	// Use first worktree's path as working dir for gh CLI (needs a valid Git repo)
	wtDir := m.worktrees[0].Path

	return func() tea.Msg {
		manager := wt.NewManager(wtRoot, repoName)
		prs, err := manager.FetchAllPRInfo(ctx, wtDir)
		if err != nil {
			return nil
		}
		return batchPRInfoMsg{prs: prs, repoName: repoName}
	}
}

// scheduleGitStatusTick schedules the next periodic git status refresh (30s).
func scheduleGitStatusTick() tea.Cmd {
	return tea.Tick(30*time.Second, func(t time.Time) tea.Msg {
		return refreshGitStatusTickMsg{}
	})
}

// schedulePRStatusTick schedules the next periodic PR status refresh (5min).
func schedulePRStatusTick() tea.Cmd {
	return tea.Tick(5*time.Minute, func(t time.Time) tea.Msg {
		return refreshPRStatusTickMsg{}
	})
}

// formatWorktreeStatus formats a WorktreeStatus for dropdown subtitle display with colors.
func formatWorktreeStatus(ws *wt.WorktreeStatus, sessionCount int, s *Styles) string {
	var parts []string

	if ws.IsDirty {
		parts = append(parts, s.Failed.Render("dirty"))
	} else {
		parts = append(parts, s.Completed.Render("clean"))
	}

	if ws.Ahead > 0 || ws.Behind > 0 {
		var ab []string
		if ws.Ahead > 0 {
			ab = append(ab, s.Running.Render(fmt.Sprintf("↑%d", ws.Ahead)))
		}
		if ws.Behind > 0 {
			ab = append(ab, s.Pending.Render(fmt.Sprintf("↓%d", ws.Behind)))
		}
		parts = append(parts, strings.Join(ab, " "))
	}

	if ws.PRNumber > 0 {
		prText := fmt.Sprintf("PR#%d %s", ws.PRNumber, ws.PRState)
		switch ws.PRState {
		case "OPEN":
			prText = fmt.Sprintf("PR#%d", ws.PRNumber) + " " + s.Running.Render("OPEN")
			if ws.PRIsDraft {
				prText = fmt.Sprintf("PR#%d", ws.PRNumber) + " " + s.Dim.Render("DRAFT")
			}
			if ws.PRReviewStatus == "APPROVED" {
				prText += " " + s.Completed.Render("✓approved")
			} else if ws.PRReviewStatus == "CHANGES_REQUESTED" {
				prText += " " + s.Failed.Render("changes requested")
			}
		case "MERGED":
			prText = fmt.Sprintf("PR#%d", ws.PRNumber) + " " + s.Idle.Render("MERGED")
		case "CLOSED":
			prText = fmt.Sprintf("PR#%d", ws.PRNumber) + " " + s.Dim.Render("CLOSED")
		}
		parts = append(parts, prText)
	}

	if !ws.LastCommitTime.IsZero() {
		parts = append(parts, s.Dim.Render(timeAgo(ws.LastCommitTime)))
	}

	if sessionCount > 0 {
		parts = append(parts, s.Idle.Render(fmt.Sprintf("%d sessions", sessionCount)))
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
			repoName:     repoName,
		}
	}
}

// repoSessionEvent wraps a session event with the repo it came from.
type repoSessionEvent struct {
	event    interface{}
	repoName string
}

// Message types
type (
	errMsg       struct{ error }
	worktreesMsg struct {
		repoName  string
		worktrees []wt.Worktree
	}
	// repoSessionEventMsg is sent when any opened repo's manager emits an event.
	repoSessionEventMsg struct {
		event    interface{}
		repoName string
	}
	// reposLoadedMsg is sent when the available repo list has been loaded.
	reposLoadedMsg  struct{ repos []string }
	sessionsUpdated struct{}
	promptInputMsg  struct{ value string }
	startSessionMsg struct {
		sessionType  session.SessionType
		prompt       string
		model        string
		worktreePath string // if set, starts on this path instead of selected worktree
		repoName     string // if set and != m.repoName, starts on that repo's manager
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
		branch   string
		warning  string
		messages []string
	}
	// taskWorktreeCreatedMsg is sent when a worktree is created for a task (then planner should start)
	taskWorktreeCreatedMsg struct {
		worktreeName string
		prompt       string
		warning      string
		messages     []string
	}
	// tickMsg is sent periodically to update running tool timers
	tickMsg struct {
		time time.Time
	}
	// singleWorktreeStatusMsg carries the git-only status for one worktree (fast, local).
	singleWorktreeStatusMsg struct {
		status   *wt.WorktreeStatus
		branch   string
		repoName string
	}
	// batchPRInfoMsg carries all open PRs fetched in a single API call.
	batchPRInfoMsg struct {
		repoName string
		prs      []wt.PRInfo
	}
	// fileTreeContextMsg carries gathered worktree context for the file tree
	fileTreeContextMsg struct {
		wtCtx        *wt.WorktreeContext
		worktreePath string
		repoName     string
	}
	// historySessionsMsg carries async-loaded history sessions for a worktree.
	historySessionsMsg struct {
		repoName string
		branch   string
		sessions []*session.SessionMeta
	}
	// refreshGitStatusTickMsg triggers a periodic git status refresh (30s)
	refreshGitStatusTickMsg struct{}
	// refreshPRStatusTickMsg triggers a periodic PR status refresh (5min)
	refreshPRStatusTickMsg struct{}
	// deferredRefreshMsg is sent after a short delay so the initial UI
	// renders with just worktree names before loading statuses/file tree.
	deferredRefreshMsg struct{}
	// resumeReposMsg triggers auto-opening of repos that have live tmux sessions
	// from a previous run.
	resumeReposMsg struct{ repos []string }
	// deleteWorktreeMsg is sent to delete a worktree
	deleteWorktreeMsg struct {
		branch       string
		deleteBranch bool
	}
	// syncWorktreesMsg is sent to sync all worktrees (fetch + rebase)
	syncWorktreesMsg struct{}
	// syncWorktreeMsg is sent to sync the currently selected worktree (fetch + rebase)
	syncWorktreeMsg struct {
		branch string
	}
	// tmuxWindowMsg carries the result of opening a new tmux window.
	tmuxWindowMsg struct {
		err          error
		worktreePath string
		windowName   string
		windowID     string
	}
	// toastExpireMsg is sent when a toast timer fires to check for expired toasts.
	toastExpireMsg struct{}
	// mergePRMsg triggers the async PR merge operation.
	mergePRMsg struct {
		branch      string
		mergeMethod string // "squash", "rebase", "merge"
	}
	// mergePRDoneMsg signals merge completed, triggers post-merge prompt.
	mergePRDoneMsg struct {
		err      error
		branch   string
		messages []string
		prNumber int
	}
	// postMergeActionMsg triggers post-merge worktree action.
	postMergeActionMsg struct {
		branch string
		action string // "delete", "reset", "keep"
	}
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

// providerStatusList returns provider statuses for UI display.
// Returns nil if no availability info is configured.
func (m *Model) providerStatusList() []agent.ProviderStatus {
	if m.providerAvailability == nil {
		return nil
	}
	return m.providerAvailability.AllStatuses()
}

// applyTheme rebuilds styles from a palette and recreates the markdown renderer.
func (m *Model) applyTheme(palette ColorPalette) {
	m.styles = NewStyles(palette)
	// Recreate the markdown renderer with the new glamour style
	if m.mdRenderer != nil {
		if newRenderer, err := NewMarkdownRenderer(m.width-8, palette.GlamourStyle); err == nil {
			m.mdRenderer = newRenderer
		}
		// If creation fails, preserve the old renderer
	}
	// Update text area placeholder colors from the palette
	m.inputArea.SetPlaceholderColor(lipgloss.Color(palette.Dim))
	m.taskModal.SetPlaceholderColor(lipgloss.Color(palette.Dim))
}

// CloseSecondaryManagers closes all session managers for repos that are NOT
// the initial repo. The initial manager is closed by the caller (main.go)
// via defer. This must be called after p.Run() returns to ensure tmux windows
// from secondary repos are properly cleaned up on exit.
func (m Model) CloseSecondaryManagers(initialRepoName string) {
	for repoName, rc := range m.repos {
		if repoName == initialRepoName {
			continue
		}
		if rc.sessionManager != nil {
			rc.sessionManager.Close()
		}
	}
}
