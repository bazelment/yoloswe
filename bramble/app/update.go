package app

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/taskrouter"
	"github.com/bazelment/yoloswe/wt"
)

// Update handles messages and updates the model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Handle help overlay first (highest visual priority)
		if m.focus == FocusHelp {
			return m.handleHelpOverlay(msg)
		}
		// Handle theme picker overlay
		if m.focus == FocusThemePicker {
			return m.handleThemePicker(msg)
		}
		// Handle repo settings overlay
		if m.focus == FocusRepoSettings {
			return m.handleRepoSettingsDialog(msg)
		}
		// Handle all sessions overlay
		if m.focus == FocusAllSessions {
			return m.handleAllSessionsOverlay(msg)
		}
		// Handle confirm prompt
		if m.focus == FocusConfirm {
			return m.handleConfirmMode(msg)
		}
		// Handle task modal
		if m.taskModal.IsVisible() {
			return m.handleTaskModal(msg)
		}
		// Handle input mode (when typing prompts)
		if m.inputMode {
			return m.handleInputMode(msg)
		}
		// Handle dropdown navigation
		if m.focus == FocusWorktreeDropdown || m.focus == FocusSessionDropdown {
			return m.handleDropdownMode(msg)
		}
		// Handle normal key presses
		return m.handleKeyPress(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.helpOverlay.SetSize(msg.Width, msg.Height)
		m.allSessionsOverlay.SetSize(msg.Width, msg.Height)
		m.themePicker.SetSize(msg.Width, msg.Height)
		m.repoSettingsDialog.SetSize(msg.Width, msg.Height)
		// Update dropdown widths
		m.worktreeDropdown.SetWidth(m.width * 2 / 3)
		m.sessionDropdown.SetWidth(m.width / 2)
		// Initialize or update markdown renderer
		if m.mdRenderer == nil {
			m.mdRenderer, _ = NewMarkdownRenderer(m.width-8, m.styles.Palette.GlamourStyle)
		} else {
			_ = m.mdRenderer.SetWidth(m.width - 8)
		}
		return m, nil

	case worktreesMsg:
		m.worktrees = msg.worktrees
		m.updateWorktreeDropdown()

		// Check for pending worktree selection (from worktree creation)
		if m.pendingWorktreeSelect != "" {
			worktreeName := m.pendingWorktreeSelect
			prompt := m.pendingPlannerPrompt
			m.pendingWorktreeSelect = ""
			m.pendingPlannerPrompt = ""
			m.worktreeDropdown.SelectByID(worktreeName)
			m.updateSessionDropdown()
			if prompt != "" {
				model, cmd := m.startSession(session.SessionTypePlanner, prompt, m.defaultPlanModel)
				// Defer heavy loading so the UI renders the worktree name first
				return model, tea.Batch(cmd, deferredRefreshCmd())
			}
			return m, deferredRefreshCmd()
		}

		// Auto-select first worktree if none selected
		if m.worktreeDropdown.SelectedItem() == nil && len(m.worktrees) > 0 {
			m.worktreeDropdown.SelectIndex(0)
		}
		// Update session dropdown with live sessions immediately;
		// defer git statuses, file tree, and history to let the UI render first.
		m.updateSessionDropdown()
		return m, deferredRefreshCmd()

	case deferredRefreshMsg:
		return m, tea.Batch(
			m.fetchGitStatuses(), scheduleGitStatusTick(),
			m.fetchPRStatuses(), schedulePRStatusTick(),
			m.refreshFileTree(), m.refreshHistorySessions(),
		)

	case singleWorktreeStatusMsg:
		if msg.status != nil {
			if m.worktreeStatuses == nil {
				m.worktreeStatuses = make(map[string]*wt.WorktreeStatus)
			}
			// Merge git-only fields into existing status to preserve PR data
			existing := m.worktreeStatuses[msg.branch]
			if existing == nil {
				m.worktreeStatuses[msg.branch] = msg.status
			} else {
				existing.IsDirty = msg.status.IsDirty
				existing.Ahead = msg.status.Ahead
				existing.Behind = msg.status.Behind
				existing.LastCommitTime = msg.status.LastCommitTime
				existing.LastCommitMsg = msg.status.LastCommitMsg
				existing.Worktree = msg.status.Worktree
			}
			m.updateWorktreeDropdown()
		}
		return m, tea.Batch(cmds...)

	case batchPRInfoMsg:
		if m.worktreeStatuses == nil {
			m.worktreeStatuses = make(map[string]*wt.WorktreeStatus)
		}
		// Build headRefName -> PRInfo map
		prByBranch := make(map[string]*wt.PRInfo, len(msg.prs))
		for i := range msg.prs {
			prByBranch[msg.prs[i].HeadRefName] = &msg.prs[i]
		}
		// Apply to each worktree's status
		for _, w := range m.worktrees {
			status := m.worktreeStatuses[w.Branch]
			if status == nil {
				status = &wt.WorktreeStatus{}
				m.worktreeStatuses[w.Branch] = status
			}
			if pr, ok := prByBranch[w.Branch]; ok {
				status.PRNumber = pr.Number
				status.PRURL = pr.URL
				status.PRState = pr.State
				status.PRIsDraft = pr.IsDraft
				status.PRReviewStatus = pr.ReviewDecision
			} else {
				// No open PR for this branch — clear PR data so we don't
				// show stale OPEN state after a PR is merged or closed.
				status.PRNumber = 0
				status.PRURL = ""
				status.PRState = ""
				status.PRIsDraft = false
				status.PRReviewStatus = ""
			}
		}
		m.updateWorktreeDropdown()
		return m, tea.Batch(cmds...)

	case historySessionsMsg:
		m.historyBranch = msg.branch
		m.cachedHistory = msg.sessions
		m.updateSessionDropdown()
		return m, tea.Batch(cmds...)

	case refreshGitStatusTickMsg:
		return m, tea.Batch(m.fetchGitStatuses(), scheduleGitStatusTick())

	case refreshPRStatusTickMsg:
		return m, tea.Batch(m.fetchPRStatuses(), schedulePRStatusTick())

	case sessionEventMsg:
		// Update sessions list
		m.sessions = m.sessionManager.GetAllSessions()
		m.updateSessionDropdown()

		// Keep listening for events
		cmds = append(cmds, m.listenForSessionEvents())
		return m, tea.Batch(cmds...)

	case sessionsUpdated:
		m.sessions = m.sessionManager.GetAllSessions()
		m.updateSessionDropdown()
		return m, nil

	case errMsg:
		cmd := m.addToast(msg.Error(), ToastError)
		return m, cmd

	case promptInputMsg:
		// Input completed
		m.inputMode = false
		// Save pending state before clearing — closures created at prompt-open
		// time capture a stale copy of m, so we snapshot the live values here.
		pendingModel := m.pendingModel
		pendingSessionType := m.pendingSessionType
		m.pendingModel = ""
		m.pendingSessionType = ""
		if m.inputHandler != nil {
			cmd := m.inputHandler(msg.value, pendingModel, pendingSessionType)
			m.inputHandler = nil
			return m, cmd
		}
		return m, nil

	case startSessionMsg:
		// Save the chosen model as the new default for this session type
		if msg.model != "" {
			switch msg.sessionType {
			case session.SessionTypePlanner:
				m.defaultPlanModel = msg.model
			case session.SessionTypeBuilder:
				m.defaultBuildModel = msg.model
			}
		}
		return m.startSession(msg.sessionType, msg.prompt, msg.model)

	case createWorktreeMsg:
		return m.createWorktree(msg.branch)

	case taskRouteMsg:
		// Start the routing process
		m.taskModal.StartRouting()
		return m, m.routeTask(msg.prompt)

	case taskProposalMsg:
		if msg.err != nil {
			m.taskModal.SetError(msg.err)
		} else if msg.proposal != nil {
			m.taskModal.SetProposal(&taskrouter.RouteProposal{
				Action:    taskrouter.ProposalAction(msg.proposal.Action),
				Worktree:  msg.proposal.Worktree,
				Parent:    msg.proposal.Parent,
				Reasoning: msg.proposal.Reasoning,
			})
		}
		return m, nil

	case taskConfirmMsg:
		return m.confirmTask(msg)

	case worktreeOpResultMsg:
		if msg.err != nil {
			cmds = append(cmds, m.addToast(msg.err.Error(), ToastError))
		} else if len(msg.messages) > 0 {
			cmds = append(cmds, m.addToast("Worktree operation completed", ToastSuccess))
		}
		if msg.warning != "" {
			cmds = append(cmds, m.addToast(msg.warning, ToastInfo))
		}
		m.worktreeOpMessages = msg.messages
		// Auto-switch to newly created worktree
		if msg.branch != "" && msg.err == nil {
			m.pendingWorktreeSelect = msg.branch
			m.pendingPlannerPrompt = "" // clear any stale task prompt
		}
		// Refresh worktrees and one-shot PR fetch (no new timer)
		cmds = append(cmds, m.refreshWorktrees(), m.fetchPRStatuses())
		return m, tea.Batch(cmds...)

	case editorResultMsg:
		if msg.err != nil {
			cmds = append(cmds, m.addToast("Failed to open editor: "+msg.err.Error(), ToastError))
		}
		return m, tea.Batch(cmds...)

	case tmuxWindowMsg:
		if msg.err != nil {
			cmds = append(cmds, m.addToast("Failed to open tmux window: "+msg.err.Error(), ToastError))
			return m, tea.Batch(cmds...)
		}
		// Only track in tmux mode
		if m.sessionManager.IsInTmuxMode() && msg.windowName != "" && msg.windowID != "" && msg.worktreePath != "" {
			if _, err := m.sessionManager.TrackTmuxWindow(msg.worktreePath, msg.windowName, msg.windowID); err != nil {
				cmds = append(cmds, m.addToast("Failed to track tmux window: "+err.Error(), ToastError))
				return m, tea.Batch(cmds...)
			}
			m.sessions = m.sessionManager.GetAllSessions()
			m.updateSessionDropdown()
			m.updateWorktreeDropdown()
		}
		return m, tea.Batch(cmds...)

	case taskWorktreeCreatedMsg:
		m.worktreeOpMessages = msg.messages
		if msg.warning != "" {
			cmds = append(cmds, m.addToast(msg.warning, ToastInfo))
		}
		// Set pending selection - will be processed after worktrees refresh
		m.pendingWorktreeSelect = msg.worktreeName
		m.pendingPlannerPrompt = msg.prompt
		cmds = append(cmds, m.refreshWorktrees())
		return m, tea.Batch(cmds...)

	case deleteWorktreeMsg:
		return m.deleteWorktree(msg.branch, msg.deleteBranch)

	case mergePRMsg:
		return m.mergePR(msg.branch, msg.mergeMethod)

	case mergePRDoneMsg:
		return m.handleMergePRDone(msg)

	case postMergeActionMsg:
		return m.handlePostMergeAction(msg)

	case syncWorktreesMsg:
		return m.syncWorktrees()

	case syncWorktreeMsg:
		return m.syncWorktree(msg.branch)

	case fileTreeContextMsg:
		m.fileTree = NewFileTree(msg.worktreePath, msg.wtCtx)
		return m, nil

	case tickMsg:
		// Continue ticking for running tool timer animation
		return m, tickCmd()

	case toastExpireMsg:
		m.toasts.Tick(time.Now())
		// If toasts remain, schedule the next expiry check
		if m.toasts.HasToasts() {
			expiryCmd := m.scheduleToastExpiry()
			return m, expiryCmd
		}
		return m, nil
	}

	return m, tea.Batch(cmds...)
}

// handleKeyPress handles key presses in normal mode (not input, not dropdown).
func (m Model) handleKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Handle quit confirmation at the top
	if m.confirmQuit {
		m.confirmQuit = false
		switch msg.String() {
		case "q", "y", "ctrl+c":
			return m, tea.Quit
		default:
			toastCmd := m.addToast("Quit cancelled", ToastInfo)
			return m, toastCmd
		}
	}

	switch msg.String() {
	case "?":
		// Open help overlay
		m.helpOverlay.previousFocus = m.focus
		m.helpOverlay.SetSize(m.width, m.height)
		sections := buildHelpSections(&m)
		m.helpOverlay.SetSections(sections)
		m.focus = FocusHelp
		return m, nil

	case "ctrl+c":
		return m, tea.Quit

	case "q":
		// Check for active sessions
		var activeSessions []session.SessionInfo
		allSessions := m.sessionManager.GetAllSessions()
		for i := range allSessions {
			if !allSessions[i].Status.IsTerminal() {
				activeSessions = append(activeSessions, allSessions[i])
			}
		}
		if len(activeSessions) > 0 {
			m.confirmQuit = true
			toastMsg := fmt.Sprintf("%d active session(s). Press 'q' or 'y' to confirm quit, any other key to cancel", len(activeSessions))
			toastCmd := m.addToast(toastMsg, ToastInfo)
			return m, toastCmd
		}
		return m, tea.Quit

	case "f2":
		// Toggle split pane (file tree + output)
		m.splitPane.Toggle()
		// Focus the file tree when opening, focus output when closing
		if m.splitPane.IsSplit() {
			m.splitPane.SetFocusLeft(true)
		}
		return m, nil

	case "tab":
		// Toggle focus between panes when split is active
		if m.splitPane.IsSplit() {
			m.splitPane.ToggleFocus()
			return m, nil
		}
		return m, nil

	case "alt+w":
		// Open worktree dropdown
		m.worktreeDropdown.Open()
		m.focus = FocusWorktreeDropdown
		return m, nil

	case "alt+s":
		// In tmux mode, Alt-S does nothing (no dropdown)
		if m.sessionManager.IsInTmuxMode() {
			toastCmd := m.addToast("Sessions are in tmux windows; use prefix+w to list", ToastInfo)
			return m, toastCmd
		}
		// TUI mode: open session dropdown
		m.sessionDropdown.Open()
		m.focus = FocusSessionDropdown
		return m, nil

	// Output scrolling (TUI mode) or session list navigation (tmux mode)
	case "up", "k":
		if m.sessionManager.IsInTmuxMode() {
			if m.splitPane.IsSplit() && m.splitPane.FocusLeft() {
				// Tmux mode + split pane: navigate file tree
				m.fileTree.MoveUp()
			} else if m.selectedSessionIndex > 0 {
				// Tmux mode: navigate session list
				m.selectedSessionIndex--
			}
		} else if m.splitPane.IsSplit() && m.splitPane.FocusLeft() {
			// Split pane: navigate file tree
			m.fileTree.MoveUp()
		} else {
			// TUI mode: scroll output
			m.scrollOutput(1)
		}
		return m, nil

	case "down", "j":
		if m.sessionManager.IsInTmuxMode() {
			if m.splitPane.IsSplit() && m.splitPane.FocusLeft() {
				// Tmux mode + split pane: navigate file tree
				m.fileTree.MoveDown()
			} else {
				// Tmux mode: navigate session list
				sessions := m.visibleSessions()
				if m.selectedSessionIndex < len(sessions)-1 {
					m.selectedSessionIndex++
				}
			}
		} else if m.splitPane.IsSplit() && m.splitPane.FocusLeft() {
			// Split pane: navigate file tree
			m.fileTree.MoveDown()
		} else {
			// TUI mode: scroll output
			m.scrollOutput(-1)
		}
		return m, nil

	case "enter":
		// Split pane: open selected file in editor
		if m.splitPane.IsSplit() && m.splitPane.FocusLeft() {
			filePath := m.fileTree.AbsSelectedPath()
			if filePath == "" {
				toastCmd := m.addToast("No file selected", ToastInfo)
				return m, toastCmd
			}
			fileName := filepath.Base(filePath)
			editor := m.editor
			toastCmd := m.addToast("Opening "+fileName+" in editor", ToastSuccess)
			return m, tea.Batch(toastCmd, func() tea.Msg {
				cmd := exec.Command(editor, filePath)
				err := cmd.Start()
				return editorResultMsg{err: err}
			})
		}
		// In tmux mode, Enter switches to the selected window
		if m.sessionManager.IsInTmuxMode() {
			currentSessions := m.visibleSessions()

			if m.selectedSessionIndex >= 0 && m.selectedSessionIndex < len(currentSessions) {
				sess := currentSessions[m.selectedSessionIndex]
				if sess.TmuxWindowName != "" {
					return m, func() tea.Msg {
						cmd := exec.Command("tmux", "select-window", "-t", sess.TmuxWindowName)
						if err := cmd.Run(); err != nil {
							return errMsg{fmt.Errorf("failed to switch to tmux window: %w", err)}
						}
						return nil
					}
				}
				// No toast for missing tmux window name - it's a rare edge case
			} else {
				toastCmd := m.addToast("No sessions to switch to", ToastInfo)
				return m, toastCmd
			}
		}
		return m, nil

	case "pgup":
		m.scrollOutput(10)
		return m, nil

	case "pgdown":
		m.scrollOutput(-10)
		return m, nil

	case "home":
		m.scrollToTop()
		return m, nil

	case "end":
		m.scrollToBottom()
		return m, nil

	case "n":
		// New worktree
		if m.repoName != "" {
			return m.promptInput("Branch name: ", func(branch string, _ string, _ session.SessionType) tea.Cmd {
				return func() tea.Msg {
					return createWorktreeMsg{branch}
				}
			}, "e.g. feature/my-feature")
		}
		toastCmd := m.addToast("No repository loaded", ToastError)
		return m, toastCmd

	case "t":
		// New task (prompt-first flow with AI routing)
		m.taskModal.SetSize(m.width, m.height)
		m.taskModal.Show()
		m.focus = FocusTaskModal
		return m, nil

	case "p":
		// Start planner
		if m.selectedWorktree() != nil {
			m.pendingModel = m.defaultPlanModel
			m.pendingSessionType = session.SessionTypePlanner
			return m.promptInput(fmt.Sprintf("Plan prompt [%s]:", m.pendingModel), func(prompt, model string, _ session.SessionType) tea.Cmd {
				return func() tea.Msg {
					return startSessionMsg{session.SessionTypePlanner, prompt, model}
				}
			}, "Describe what you want to plan...")
		}
		toastCmd := m.addToast("Select a worktree first (Alt-W)", ToastInfo)
		return m, toastCmd

	case "b":
		// Start builder
		if m.selectedWorktree() != nil {
			m.pendingModel = m.defaultBuildModel
			m.pendingSessionType = session.SessionTypeBuilder
			return m.promptInput(fmt.Sprintf("Build prompt [%s]:", m.pendingModel), func(prompt, model string, _ session.SessionType) tea.Cmd {
				return func() tea.Msg {
					return startSessionMsg{session.SessionTypeBuilder, prompt, model}
				}
			}, "Describe what to build...")
		}
		toastCmd := m.addToast("Select a worktree first (Alt-W)", ToastInfo)
		return m, toastCmd

	case "e":
		// Open editor for worktree
		if wt := m.selectedWorktree(); wt != nil {
			return m, func() tea.Msg {
				cmd := exec.Command(m.editor, wt.Path)
				err := cmd.Start()
				return editorResultMsg{err: err}
			}
		}
		toastCmd := m.addToast("Select a worktree first (Alt-W)", ToastInfo)
		return m, toastCmd

	case "w":
		// Open new tmux window in worktree directory
		if !session.IsInsideTmux() || !session.IsTmuxAvailable() {
			toastCmd := m.addToast("Not inside tmux", ToastInfo)
			return m, toastCmd
		}
		if wt := m.selectedWorktree(); wt != nil {
			wtPath := wt.Path
			toastCmd := m.addToast("Opening tmux window in "+filepath.Base(wtPath), ToastSuccess)
			return m, tea.Batch(toastCmd, func() tea.Msg {
				// Generate unique window name like build/plan sessions
				windowName := session.GenerateTmuxWindowName()

				// Create window with unique name and capture both name and ID
				cmd := exec.Command("tmux", "new-window", "-n", windowName, "-c", wtPath, "-P", "-F", "#{window_name},#{window_id}")
				out, err := cmd.CombinedOutput()
				if err != nil {
					detail := strings.TrimSpace(string(out))
					if detail != "" {
						return tmuxWindowMsg{err: fmt.Errorf("%w: %s", err, detail)}
					}
					return tmuxWindowMsg{err: err}
				}

				// Parse output: "window-name,@ID"
				output := strings.TrimSpace(string(out))
				parts := strings.Split(output, ",")
				if len(parts) != 2 {
					return tmuxWindowMsg{err: fmt.Errorf("unexpected tmux output format: %s", output)}
				}

				return tmuxWindowMsg{
					worktreePath: wtPath,
					windowName:   parts[0],
					windowID:     parts[1],
				}
			})
		}
		toastCmd := m.addToast("Select a worktree first (Alt-W)", ToastInfo)
		return m, toastCmd

	case "s":
		// Stop session with confirmation (TUI mode only)
		if m.sessionManager.IsInTmuxMode() {
			toastCmd := m.addToast("Close tmux windows directly with prefix+& or 'exit' command", ToastInfo)
			return m, toastCmd
		}
		if sess := m.selectedSession(); sess != nil {
			sessID := sess.ID
			title := sess.Title
			if title == "" {
				title = string(sessID)[:12]
			}
			return m.showConfirm("Stop session '"+title+"'?", []ConfirmOption{
				{Key: "y", Label: "yes"},
			}, func(key string) tea.Cmd {
				return func() tea.Msg {
					m.sessionManager.StopSession(sessID)
					return sessionsUpdated{}
				}
			})
		}
		toastCmd := m.addToast("No active session to stop (Alt-S to select)", ToastInfo)
		return m, toastCmd

	case "S":
		// Open all sessions overlay — fetch fresh from manager to avoid stale cache
		allSessions := m.sessionManager.GetAllSessions()
		var activeSessions []session.SessionInfo
		for i := range allSessions {
			if !allSessions[i].Status.IsTerminal() {
				activeSessions = append(activeSessions, allSessions[i])
			}
		}
		m.allSessionsOverlay.Show(activeSessions, m.width, m.height)
		m.focus = FocusAllSessions
		return m, nil

	case "T":
		// Open unified settings dialog focused on theme.
		if m.repoName == "" {
			toastCmd := m.addToast("No repository loaded", ToastError)
			return m, toastCmd
		}
		cfg := m.settings.RepoSettingsFor(m.repoName)
		m.repoSettingsDialog.Show(m.repoName, cfg, m.styles.Palette.Name, m.width, m.height, lipgloss.Color(m.styles.Palette.Dim), m.providerStatusList(), m.settings.EnabledProviders)
		m.repoSettingsDialog.FocusTheme()
		m.focus = FocusRepoSettings
		return m, nil

	case "R":
		// Open repo settings
		if m.repoName == "" {
			toastCmd := m.addToast("No repository loaded", ToastError)
			return m, toastCmd
		}
		cfg := m.settings.RepoSettingsFor(m.repoName)
		m.repoSettingsDialog.Show(m.repoName, cfg, m.styles.Palette.Name, m.width, m.height, lipgloss.Color(m.styles.Palette.Dim), m.providerStatusList(), m.settings.EnabledProviders)
		m.focus = FocusRepoSettings
		return m, nil

	case "ctrl+l":
		// Primary settings shortcut.
		if m.repoName == "" {
			toastCmd := m.addToast("No repository loaded", ToastError)
			return m, toastCmd
		}
		cfg := m.settings.RepoSettingsFor(m.repoName)
		m.repoSettingsDialog.Show(m.repoName, cfg, m.styles.Palette.Name, m.width, m.height, lipgloss.Color(m.styles.Palette.Dim), m.providerStatusList(), m.settings.EnabledProviders)
		m.focus = FocusRepoSettings
		return m, nil

	case "f":
		// Follow-up on idle session (TUI mode only)
		if m.sessionManager.IsInTmuxMode() {
			toastCmd := m.addToast("Follow-ups must be done in the tmux window directly", ToastInfo)
			return m, toastCmd
		}
		if sess := m.selectedSession(); sess != nil && sess.Status == session.StatusIdle {
			return m.promptInput("Follow-up: ", func(message string, _ string, _ session.SessionType) tea.Cmd {
				return func() tea.Msg {
					if err := m.sessionManager.SendFollowUp(sess.ID, message); err != nil {
						return errMsg{err}
					}
					return sessionsUpdated{}
				}
			}, "Type your follow-up message...")
		}
		toastCmd := m.addToast("No idle session for follow-up", ToastInfo)
		return m, toastCmd

	case "a":
		// Approve plan and start builder session
		if sess := m.selectedSession(); sess != nil &&
			sess.Status == session.StatusIdle &&
			sess.Type == session.SessionTypePlanner &&
			sess.PlanFilePath != "" {
			worktreePath := sess.WorktreePath
			planPath := sess.PlanFilePath
			_ = m.sessionManager.CompleteSession(sess.ID)
			m.sessions = m.sessionManager.GetAllSessions()
			m.updateSessionDropdown()
			planPrompt := fmt.Sprintf("Implement the plan in %s", planPath)
			sessionID, err := m.sessionManager.StartSession(session.SessionTypeBuilder, worktreePath, planPrompt, m.defaultBuildModel)
			if err != nil {
				toastCmd := m.addToast(err.Error(), ToastError)
				return m, toastCmd
			}
			if m.viewingSessionID != "" {
				m.scrollPositions[m.viewingSessionID] = m.scrollOffset
			}
			m.viewingSessionID = sessionID
			m.scrollOffset = 0 // New builder session starts at bottom
			m.sessions = m.sessionManager.GetAllSessions()
			m.updateSessionDropdown()
			return m, nil
		}
		toastCmd := m.addToast("No plan ready to approve", ToastInfo)
		return m, toastCmd

	case "m":
		// Merge PR
		return m.handleMergeKey()

	case "d":
		// Delete worktree
		if w := m.selectedWorktree(); w != nil {
			branch := w.Branch
			return m.showConfirm("Delete worktree '"+branch+"'?", []ConfirmOption{
				{Key: "y", Label: "yes, keep branch"},
				{Key: "d", Label: "yes + delete branch"},
			}, func(key string) tea.Cmd {
				deleteBranch := key == "d"
				return func() tea.Msg {
					return deleteWorktreeMsg{branch: branch, deleteBranch: deleteBranch}
				}
			})
		}
		toastCmd := m.addToast("Select a worktree first (Alt-W)", ToastInfo)
		return m, toastCmd

	case "r":
		// Refresh (worktrees + one-shot PR fetch, no new timer)
		return m, tea.Batch(m.refreshWorktrees(), m.fetchPRStatuses())

	case "g":
		// Sync current worktree (fetch + rebase)
		if m.repoName == "" {
			toastCmd := m.addToast("No repository loaded", ToastError)
			return m, toastCmd
		}
		selected := m.selectedWorktree()
		if selected == nil {
			toastCmd := m.addToast("No worktree selected", ToastError)
			return m, toastCmd
		}
		branch := selected.Branch
		return m, func() tea.Msg {
			return syncWorktreeMsg{branch: branch}
		}

	case "G":
		// Sync all worktrees (fetch + rebase)
		if m.repoName == "" {
			toastCmd := m.addToast("No repository loaded", ToastError)
			return m, toastCmd
		}
		return m, func() tea.Msg {
			return syncWorktreesMsg{}
		}

	case "esc":
		// Reset scroll
		m.scrollOffset = 0
		return m, nil

	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		idx := int(msg.String()[0]-'0') - 1
		liveSessions := m.visibleSessions()
		if idx >= len(liveSessions) {
			toastCmd := m.addToast(fmt.Sprintf("No session #%s", msg.String()), ToastInfo)
			return m, toastCmd
		}
		if m.sessionManager.IsInTmuxMode() {
			m.selectedSessionIndex = idx
			return m, nil
		}
		m.switchViewingSession(liveSessions[idx].ID)
		return m, nil
	}

	return m, nil
}

// scrollOutput adjusts the scroll offset by delta.
// Positive delta scrolls up (towards older content), negative scrolls down (towards newer).
func (m *Model) scrollOutput(delta int) {
	m.scrollOffset += delta
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
	// Max offset is clamped in renderCenter based on actual line count
}

// scrollToTop scrolls to the beginning of the output.
func (m *Model) scrollToTop() {
	// Set to a large value; will be clamped in renderCenter
	m.scrollOffset = 999999
}

// scrollToBottom scrolls to the end (latest) output.
func (m *Model) scrollToBottom() {
	m.scrollOffset = 0
}

// switchViewingSession saves the scroll position for the current session,
// sets the viewing session to newID, and restores the saved scroll position
// (or 0 if none was saved).
func (m *Model) switchViewingSession(newID session.SessionID) {
	if m.viewingSessionID != "" {
		m.scrollPositions[m.viewingSessionID] = m.scrollOffset
	}
	m.viewingSessionID = newID
	m.scrollOffset = m.scrollPositions[newID] // zero-value (0) if not found
	m.viewingHistoryData = nil
}

// handleDropdownMode handles key presses when a dropdown is open.
func (m Model) handleDropdownMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "?":
		// Open help overlay
		m.helpOverlay.previousFocus = m.focus
		m.helpOverlay.SetSize(m.width, m.height)
		sections := buildHelpSections(&m)
		m.helpOverlay.SetSections(sections)
		m.focus = FocusHelp
		return m, nil

	case "alt+w", "alt+s":
		// Always close dropdown immediately
		m.worktreeDropdown.Close()
		m.sessionDropdown.Close()
		m.focus = FocusOutput
		return m, nil

	case "esc":
		// If filter is active, clear it first. If already empty, close dropdown.
		dd := m.worktreeDropdown
		if m.focus == FocusSessionDropdown {
			dd = m.sessionDropdown
		}
		if dd.FilterText() != "" {
			dd.ClearFilter()
			return m, nil
		}
		m.worktreeDropdown.Close()
		m.sessionDropdown.Close()
		m.focus = FocusOutput
		return m, nil

	case "j", "down":
		if m.focus == FocusWorktreeDropdown {
			m.worktreeDropdown.MoveSelection(1)
		} else {
			m.sessionDropdown.MoveSelection(1)
		}
		return m, nil

	case "k", "up":
		if m.focus == FocusWorktreeDropdown {
			m.worktreeDropdown.MoveSelection(-1)
		} else {
			m.sessionDropdown.MoveSelection(-1)
		}
		return m, nil

	case "backspace":
		// Remove last filter character
		if m.focus == FocusWorktreeDropdown {
			m.worktreeDropdown.BackspaceFilter()
		} else {
			m.sessionDropdown.BackspaceFilter()
		}
		return m, nil

	case "enter":
		if m.focus == FocusWorktreeDropdown {
			// Worktree selected - update session dropdown
			m.worktreeDropdown.Close()
			m.updateSessionDropdown()
			// Save scroll position and clear viewing session when switching worktrees
			m.switchViewingSession("")
			m.selectedSessionIndex = 0
			// Refresh file tree and history for new worktree
			m.focus = FocusOutput
			return m, tea.Batch(m.refreshFileTree(), m.refreshHistorySessions())
		}
		// Session selected - view it
		if item := m.sessionDropdown.SelectedItem(); item != nil {
			if item.ID == "---separator---" {
				// Can't select separator
				return m, nil
			}
			m.switchViewingSession(session.SessionID(item.ID))
			// Check if this is a live session or history
			if _, ok := m.sessionManager.GetSessionInfo(m.viewingSessionID); ok {
				// Live session -- viewingHistoryData already nil from switchViewingSession
			} else {
				// History session - load from store
				wt := m.selectedWorktree()
				if wt != nil {
					histData, err := m.sessionManager.LoadSessionFromHistory(wt.Name(), m.viewingSessionID)
					if err == nil {
						m.viewingHistoryData = histData
					}
				}
			}
		}
		m.sessionDropdown.Close()
		m.focus = FocusOutput
		return m, nil

	case "q", "ctrl+c":
		return m, tea.Quit

	default:
		// Type-to-filter: route printable characters to the dropdown
		if r, ok := printableRune(msg); ok {
			if m.focus == FocusWorktreeDropdown {
				m.worktreeDropdown.AppendFilter(r)
			} else {
				m.sessionDropdown.AppendFilter(r)
			}
			return m, nil
		}
		return m, nil
	}
}

// handleInputMode handles key presses in input mode.
// Tab cycles focus between text input and buttons.
// Enter activates the focused element.
func (m Model) handleInputMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Alt+M: cycle model selection when in a plan/build prompt.
	// Note: Ctrl+M is aliased to Enter in terminals, so we use Alt+M instead.
	if msg.String() == "alt+m" && m.pendingModel != "" {
		var next session.AgentModel
		if m.modelRegistry != nil {
			next = m.modelRegistry.NextModel(m.pendingModel)
		} else {
			next = session.NextModel(m.pendingModel)
		}
		m.pendingModel = next.ID
		prefix := "Plan"
		if m.pendingSessionType == session.SessionTypeBuilder {
			prefix = "Build"
		}
		m.inputPrompt = fmt.Sprintf("%s prompt [%s]:", prefix, m.pendingModel)
		return m, nil
	}

	action := m.inputArea.HandleKey(msg)

	switch action {
	case TextAreaSubmit:
		value := m.inputArea.Value()
		if value == "" {
			return m, nil
		}
		m.inputArea.Reset()
		return m, func() tea.Msg {
			return promptInputMsg{value}
		}

	case TextAreaCancel:
		m.inputMode = false
		m.inputArea.Reset()
		m.inputHandler = nil
		m.pendingModel = ""
		m.pendingSessionType = ""
		return m, nil

	case TextAreaQuit:
		return m, tea.Quit

	default:
		// TextAreaHandled or TextAreaUnhandled — surface any inner command
		// (e.g. cursor blink scheduling) from the bubbles textarea.
		return m, m.inputArea.Cmd()
	}
}

// handleConfirmMode handles key presses in the single-keypress confirmation prompt.
func (m Model) handleConfirmMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	result := m.confirmPrompt.HandleKey(msg)

	switch {
	case result.Quit:
		return m, tea.Quit
	case result.Cancelled:
		cancelHandler := m.confirmCancelHandler
		m.focus = FocusOutput
		m.confirmPrompt = nil
		m.confirmHandler = nil
		m.confirmCancelHandler = nil
		if cancelHandler != nil {
			return m, cancelHandler()
		}
		return m, nil
	case result.Matched != "":
		handler := m.confirmHandler
		m.focus = FocusOutput
		m.confirmPrompt = nil
		m.confirmHandler = nil
		m.confirmCancelHandler = nil
		if handler != nil {
			return m, handler(result.Matched)
		}
		return m, nil
	default:
		// Unrecognized key — stay in confirm mode
		return m, nil
	}
}

// showConfirm switches to confirmation mode with a single-keypress prompt.
func (m Model) showConfirm(message string, options []ConfirmOption, handler func(string) tea.Cmd) (tea.Model, tea.Cmd) {
	m.confirmPrompt = NewConfirmPrompt(message, options)
	m.confirmHandler = handler
	m.focus = FocusConfirm
	return m, nil
}

// promptInput switches to input mode with an optional placeholder.
func (m Model) promptInput(prompt string, handler func(value, model string, sessionType session.SessionType) tea.Cmd, placeholder ...string) (tea.Model, tea.Cmd) {
	m.inputMode = true
	m.inputPrompt = prompt
	m.inputArea.Reset()
	if len(placeholder) > 0 {
		m.inputArea.SetPlaceholder(placeholder[0])
	}
	m.inputHandler = handler
	m.focus = FocusInput
	return m, nil
}

// startSession starts a session of the given type with the specified model.
func (m Model) startSession(sessionType session.SessionType, prompt, model string) (tea.Model, tea.Cmd) {
	wt := m.selectedWorktree()
	if wt == nil || prompt == "" {
		return m, nil
	}

	sessionID, err := m.sessionManager.StartSession(sessionType, wt.Path, prompt, model)
	if err != nil {
		toastCmd := m.addToast(err.Error(), ToastError)
		return m, toastCmd
	}

	if m.viewingSessionID != "" {
		m.scrollPositions[m.viewingSessionID] = m.scrollOffset
	}
	m.viewingSessionID = sessionID
	m.scrollOffset = 0 // New session starts at bottom
	m.sessions = m.sessionManager.GetAllSessions()
	m.updateSessionDropdown()
	toastCmd := m.addToast("Session started: "+string(sessionID)[:12], ToastSuccess)
	return m, toastCmd
}

// createWorktree creates a new worktree asynchronously with captured output.
func (m Model) createWorktree(branch string) (tea.Model, tea.Cmd) {
	if branch == "" {
		return m, nil
	}

	if m.repoName == "" {
		toastCmd := m.addToast("No repository selected", ToastError)
		return m, toastCmd
	}

	// Show pending message
	m.worktreeOpMessages = []string{"Creating worktree " + branch + "..."}

	// Run asynchronously
	wtRoot := m.wtRoot
	repoName := m.repoName
	repoSettings := m.settings.RepoSettingsFor(repoName)
	ctx := m.ctx
	return m, func() tea.Msg {
		var buf bytes.Buffer
		output := wt.NewOutput(&buf, false) // No colors for captured output
		manager := wt.NewManager(wtRoot, repoName, wt.WithOutput(output))

		worktreePath, err := manager.NewAtomic(ctx, branch, "", "")
		messages := parseHookOutput(buf.String())
		if err != nil {
			return worktreeOpResultMsg{messages: messages, err: err}
		}

		var warning string
		if err := runRepoHookCommands(repoSettings.OnWorktreeCreate, worktreePath, branch, &messages); err != nil {
			warning = "Worktree created, but on-worktree-create command failed"
			messages = append(messages, "Non-fatal: on-worktree-create command failed")
		}
		if warning == "" {
			warning = extractHookWarning(messages)
		}

		return worktreeOpResultMsg{messages: messages, err: nil, branch: branch, warning: warning}
	}
}

// deleteWorktree deletes a worktree asynchronously.
func (m Model) deleteWorktree(branch string, deleteBranch bool) (tea.Model, tea.Cmd) {
	if branch == "" || m.repoName == "" {
		return m, nil
	}

	// Clear viewing session if it belongs to this worktree
	if w := m.selectedWorktree(); w != nil && w.Branch == branch {
		// Save scroll position before clearing (session being deleted,
		// so the saved position will be stale, but that's fine -- it's
		// a no-op to save for a soon-to-be-deleted session).
		m.switchViewingSession("")
	}

	// Show pending message
	m.worktreeOpMessages = []string{"Deleting worktree " + branch + "..."}

	wtRoot := m.wtRoot
	repoName := m.repoName
	repoSettings := m.settings.RepoSettingsFor(repoName)
	worktreePath := filepath.Join(wtRoot, repoName, branch)
	for _, w := range m.worktrees {
		if w.Branch == branch {
			worktreePath = w.Path
			break
		}
	}
	ctx := m.ctx
	return m, func() tea.Msg {
		var buf bytes.Buffer
		output := wt.NewOutput(&buf, false)
		manager := wt.NewManager(wtRoot, repoName, wt.WithOutput(output))

		var warning string
		var messages []string
		if err := runRepoHookCommands(repoSettings.OnWorktreeDelete, worktreePath, branch, &messages); err != nil {
			warning = "Worktree delete continued, but on-worktree-delete command failed"
			messages = append(messages, "Non-fatal: on-worktree-delete command failed")
		}

		err := manager.Remove(ctx, branch, deleteBranch)

		messages = append(messages, parseHookOutput(buf.String())...)
		if warning == "" {
			warning = extractHookWarning(messages)
		}

		return worktreeOpResultMsg{messages: messages, err: err, warning: warning}
	}
}

// handleMergeKey runs pre-flight checks and shows the merge confirmation prompt.
func (m Model) handleMergeKey() (tea.Model, tea.Cmd) {
	w := m.selectedWorktree()
	if w == nil {
		toastCmd := m.addToast("Select a worktree first (Alt-W)", ToastInfo)
		return m, toastCmd
	}

	branch := w.Branch
	status := m.worktreeStatuses[branch]

	if status == nil {
		toastCmd := m.addToast("Worktree status not loaded yet", ToastInfo)
		return m, toastCmd
	}

	if status.PRNumber == 0 {
		toastCmd := m.addToast(fmt.Sprintf("No PR found for %s", branch), ToastInfo)
		return m, toastCmd
	}

	if status.PRState != "OPEN" {
		toastCmd := m.addToast(fmt.Sprintf("PR #%d is %s", status.PRNumber, status.PRState), ToastInfo)
		return m, toastCmd
	}

	if status.IsDirty {
		toastCmd := m.addToast("Uncommitted changes. Commit or stash first.", ToastInfo)
		return m, toastCmd
	}

	if status.Ahead > 0 {
		toastCmd := m.addToast(fmt.Sprintf("%d unpushed commits. Push first.", status.Ahead), ToastInfo)
		return m, toastCmd
	}

	// Check for running/pending sessions (idle is OK)
	sessions := m.sessionManager.GetSessionsForWorktree(w.Path)
	for i := range sessions {
		if !sessions[i].Status.IsTerminal() && sessions[i].Status != session.StatusIdle {
			toastCmd := m.addToast("Stop active sessions first.", ToastInfo)
			return m, toastCmd
		}
	}

	// Build confirmation message
	msg := fmt.Sprintf("Merge PR #%d (%s)?", status.PRNumber, branch)
	if status.PRReviewStatus != "" {
		msg += fmt.Sprintf("\nReview: %s", status.PRReviewStatus)
	}
	if status.PRIsDraft {
		msg += "\n[Draft PR]"
	}

	return m.showConfirm(msg, []ConfirmOption{
		{Key: "s", Label: "squash"},
		{Key: "r", Label: "rebase"},
		{Key: "m", Label: "merge commit"},
	}, func(key string) tea.Cmd {
		method := map[string]string{"s": "squash", "r": "rebase", "m": "merge"}[key]
		return func() tea.Msg {
			return mergePRMsg{branch: branch, mergeMethod: method}
		}
	})
}

// mergePR runs the async merge operation.
func (m Model) mergePR(branch, mergeMethod string) (tea.Model, tea.Cmd) {
	if branch == "" || m.repoName == "" {
		return m, nil
	}

	m.worktreeOpMessages = []string{fmt.Sprintf("Merging PR for %s...", branch)}

	wtRoot := m.wtRoot
	repoName := m.repoName
	ctx := m.ctx
	return m, func() tea.Msg {
		var buf bytes.Buffer
		output := wt.NewOutput(&buf, false)
		manager := wt.NewManager(wtRoot, repoName, wt.WithOutput(output))

		prNumber, err := manager.MergePRForBranch(ctx, branch, wt.MergeOptions{
			MergeMethod: mergeMethod,
			Keep:        true,
		})

		var messages []string
		for _, line := range strings.Split(buf.String(), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				messages = append(messages, line)
			}
		}

		return mergePRDoneMsg{branch: branch, prNumber: prNumber, messages: messages, err: err}
	}
}

// handleMergePRDone handles the result of a merge operation.
func (m Model) handleMergePRDone(msg mergePRDoneMsg) (tea.Model, tea.Cmd) {
	m.worktreeOpMessages = msg.messages

	if msg.err != nil {
		toastCmd := m.addToast(msg.err.Error(), ToastError)
		return m, toastCmd
	}

	prompt := fmt.Sprintf("PR #%d merged! What to do with worktree '%s'?", msg.prNumber, msg.branch)
	branch := msg.branch
	newModel, cmd := m.showConfirm(prompt, []ConfirmOption{
		{Key: "d", Label: "delete worktree + branch"},
		{Key: "r", Label: "reset to main"},
		{Key: "k", Label: "keep as-is"},
	}, func(key string) tea.Cmd {
		action := map[string]string{"d": "delete", "r": "reset", "k": "keep"}[key]
		return func() tea.Msg {
			return postMergeActionMsg{branch: branch, action: action}
		}
	})
	// Refresh even if user cancels — the merge already happened on GitHub.
	m2 := newModel.(Model)
	m2.confirmCancelHandler = func() tea.Cmd {
		return func() tea.Msg {
			return postMergeActionMsg{branch: branch, action: "keep"}
		}
	}
	return m2, cmd
}

// handlePostMergeAction handles the user's choice after a successful merge.
func (m Model) handlePostMergeAction(msg postMergeActionMsg) (tea.Model, tea.Cmd) {
	switch msg.action {
	case "delete":
		return m.deleteWorktree(msg.branch, true)

	case "reset":
		m.worktreeOpMessages = []string{fmt.Sprintf("Resetting %s to default branch...", msg.branch)}
		wtRoot := m.wtRoot
		repoName := m.repoName
		ctx := m.ctx
		branch := msg.branch
		return m, func() tea.Msg {
			var buf bytes.Buffer
			output := wt.NewOutput(&buf, false)
			manager := wt.NewManager(wtRoot, repoName, wt.WithOutput(output))

			bareDir := manager.BareDir()
			defaultBranch, _ := wt.GetDefaultBranch(ctx, manager.GitRunner(), bareDir)
			if defaultBranch == "" {
				defaultBranch = "main"
			}

			// Find the worktree path for this branch
			worktrees, _ := manager.List(ctx)
			var wtPath string
			for _, w := range worktrees {
				if w.Branch == branch {
					wtPath = w.Path
					break
				}
			}
			if wtPath == "" {
				return worktreeOpResultMsg{err: fmt.Errorf("worktree for %s not found", branch)}
			}

			// Fetch and reset to origin/default
			manager.GitRunner().Run(ctx, []string{"fetch", "origin"}, wtPath)
			if _, err := manager.GitRunner().Run(ctx, []string{"reset", "--hard", "origin/" + defaultBranch}, wtPath); err != nil {
				return worktreeOpResultMsg{err: fmt.Errorf("failed to reset: %w", err)}
			}

			var messages []string
			for _, line := range strings.Split(buf.String(), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					messages = append(messages, line)
				}
			}
			messages = append(messages, fmt.Sprintf("Reset %s to %s", branch, defaultBranch))
			return worktreeOpResultMsg{messages: messages}
		}

	default: // "keep"
		return m, tea.Batch(m.refreshWorktrees(), m.fetchPRStatuses())
	}
}

// syncWorktrees syncs all worktrees asynchronously (fetch + rebase).
func (m Model) syncWorktrees() (tea.Model, tea.Cmd) {
	return m.syncWorktree("")
}

// syncWorktree syncs a single worktree asynchronously (fetch + rebase).
// Pass an empty branch string to sync all worktrees.
func (m Model) syncWorktree(branch string) (tea.Model, tea.Cmd) {
	if m.repoName == "" {
		toastCmd := m.addToast("No repository selected", ToastError)
		return m, toastCmd
	}

	if branch == "" {
		m.worktreeOpMessages = []string{"Syncing worktrees..."}
	} else {
		m.worktreeOpMessages = []string{fmt.Sprintf("Syncing worktree %s...", branch)}
	}

	wtRoot := m.wtRoot
	repoName := m.repoName
	ctx := m.ctx
	return m, func() tea.Msg {
		var buf bytes.Buffer
		output := wt.NewOutput(&buf, false)
		manager := wt.NewManager(wtRoot, repoName, wt.WithOutput(output))

		err := manager.Sync(ctx, branch)

		var messages []string
		for _, line := range strings.Split(buf.String(), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				messages = append(messages, line)
			}
		}

		return worktreeOpResultMsg{messages: messages, err: err}
	}
}

// handleTaskModal handles key presses in the task modal.
func (m Model) handleTaskModal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	state := m.taskModal.State()

	switch state {
	case TaskModalInput:
		ta := m.taskModal.TextArea()
		action := ta.HandleKey(msg)

		switch action {
		case TextAreaSubmit:
			prompt := m.taskModal.Prompt()
			if prompt != "" {
				return m, func() tea.Msg {
					return taskRouteMsg{prompt: prompt}
				}
			}
			return m, nil

		case TextAreaCancel:
			m.taskModal.Hide()
			m.focus = FocusOutput
			return m, nil

		case TextAreaQuit:
			return m, tea.Quit

		default:
			return m, ta.Cmd()
		}

	case TaskModalRouting:
		// Only allow quit while routing
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.taskModal.Hide()
			m.focus = FocusOutput
			return m, nil
		}
		return m, nil

	case TaskModalProposal:
		switch msg.String() {
		case "esc":
			m.taskModal.Hide()
			m.focus = FocusOutput
			return m, nil

		case "enter":
			// Confirm the proposal
			proposal := m.taskModal.Proposal()
			if proposal != nil {
				return m, func() tea.Msg {
					return taskConfirmMsg{
						worktree: proposal.Worktree,
						parent:   proposal.Parent,
						isNew:    proposal.Action == taskrouter.ActionCreateNew,
						prompt:   m.taskModal.Prompt(),
					}
				}
			}
			return m, nil

		case "a":
			// Adjust the proposal
			m.taskModal.StartAdjust()
			return m, nil

		case "ctrl+c":
			return m, tea.Quit
		}
		return m, nil

	case TaskModalAdjust:
		if m.taskModal.Proposal().Action == taskrouter.ActionCreateNew {
			// Route key events to the adjust TextArea for branch name editing
			ta := m.taskModal.AdjustTextArea()
			action := ta.HandleKey(msg)

			switch action {
			case TextAreaSubmit:
				// Read edited value and confirm
				edited := ta.Value()
				if edited != "" {
					m.taskModal.adjustWorktree = edited
				}
				return m, func() tea.Msg {
					return taskConfirmMsg{
						worktree: m.taskModal.AdjustedWorktree(),
						parent:   m.taskModal.AdjustedParent(),
						isNew:    true,
						prompt:   m.taskModal.Prompt(),
					}
				}
			case TextAreaCancel:
				// Go back to proposal state (discard edits)
				m.taskModal.SetProposal(m.taskModal.Proposal())
				return m, nil
			case TextAreaQuit:
				return m, tea.Quit
			default:
				return m, ta.Cmd()
			}
		}

		// Existing worktree mode — original behavior
		switch msg.String() {
		case "esc":
			m.taskModal.SetProposal(m.taskModal.Proposal())
			return m, nil
		case "enter":
			return m, func() tea.Msg {
				return taskConfirmMsg{
					worktree: m.taskModal.AdjustedWorktree(),
					parent:   m.taskModal.AdjustedParent(),
					isNew:    false,
					prompt:   m.taskModal.Prompt(),
				}
			}
		case "ctrl+c":
			return m, tea.Quit
		}
		return m, nil
	}

	return m, nil
}

// routeTask runs the task router asynchronously.
func (m Model) routeTask(prompt string) tea.Cmd {
	// Capture all fields on the main goroutine to avoid data races.
	// In particular, worktreeStatuses is a map that the Update loop mutates,
	// so we snapshot the needed values here rather than reading the map in the
	// async closure.
	router := m.taskRouter
	repoName := m.repoName
	ctx := m.ctx

	currentWT := ""
	if w := m.selectedWorktree(); w != nil {
		currentWT = w.Branch
	}

	// Build enriched worktree info synchronously (main goroutine).
	worktreeInfos := make([]taskrouter.WorktreeInfo, len(m.worktrees))
	for i, wt := range m.worktrees {
		info := taskrouter.WorktreeInfo{
			Name: wt.Branch,
			Path: wt.Path,
		}
		if m.worktreeStatuses != nil {
			if s, ok := m.worktreeStatuses[wt.Branch]; ok {
				info.IsDirty = s.IsDirty
				info.IsAhead = s.Ahead > 0
				info.PRState = s.PRState
				info.IsMerged = s.PRState == "MERGED"
				info.LastCommit = s.LastCommitMsg
			}
		}
		worktreeInfos[i] = info
	}

	return func() tea.Msg {
		req := taskrouter.RouteRequest{
			Prompt:    prompt,
			Worktrees: worktreeInfos,
			CurrentWT: currentWT,
			RepoName:  repoName,
		}

		proposal, err := RouteTask(ctx, router, req)
		if err != nil {
			return taskProposalMsg{err: err}
		}

		return taskProposalMsg{
			proposal: &RouteProposal{
				Action:    string(proposal.Action),
				Worktree:  proposal.Worktree,
				Parent:    proposal.Parent,
				Reasoning: proposal.Reasoning,
			},
		}
	}
}

// confirmTask confirms the task routing decision and starts the planner.
func (m Model) confirmTask(msg taskConfirmMsg) (tea.Model, tea.Cmd) {
	m.taskModal.Hide()
	m.focus = FocusOutput

	toastCmd := m.addToast("Task confirmed, starting session...", ToastSuccess)

	// If creating a new worktree, do that first
	if msg.isNew {
		if m.repoName == "" {
			errToastCmd := m.addToast("No repository selected", ToastError)
			return m, errToastCmd
		}

		// Show pending message
		m.worktreeOpMessages = []string{"Creating worktree " + msg.worktree + "..."}

		// Run asynchronously and start planner after
		wtRoot := m.wtRoot
		repoName := m.repoName
		ctx := m.ctx
		worktreeName := msg.worktree
		parent := msg.parent
		prompt := msg.prompt
		repoSettings := m.settings.RepoSettingsFor(repoName)
		return m, tea.Batch(toastCmd, func() tea.Msg {
			var buf bytes.Buffer
			output := wt.NewOutput(&buf, false)
			manager := wt.NewManager(wtRoot, repoName, wt.WithOutput(output))

			worktreePath, err := manager.NewAtomic(ctx, worktreeName, parent, "")
			messages := parseHookOutput(buf.String())

			if err != nil {
				return worktreeOpResultMsg{messages: messages, err: err}
			}

			// Run per-repo hook commands
			var warning string
			if err := runRepoHookCommands(repoSettings.OnWorktreeCreate, worktreePath, worktreeName, &messages); err != nil {
				warning = "Worktree created, but on-worktree-create command failed"
				messages = append(messages, "Non-fatal: on-worktree-create command failed")
			}
			if warning == "" {
				warning = extractHookWarning(messages)
			}

			// Return a message that will trigger worktree refresh and planner start
			return taskWorktreeCreatedMsg{
				messages:     messages,
				worktreeName: worktreeName,
				prompt:       prompt,
				warning:      warning,
			}
		})
	}

	// Use existing worktree - select it and start planner
	m.worktreeDropdown.SelectByID(msg.worktree)
	model, cmd := m.startSession(session.SessionTypePlanner, msg.prompt, m.defaultPlanModel)
	return model, tea.Batch(toastCmd, cmd)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// handleAllSessionsOverlay handles key presses when the all sessions overlay is visible.
func (m Model) handleAllSessionsOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.allSessionsOverlay.Hide()
		m.focus = FocusOutput
		return m, nil

	case "up", "k":
		m.allSessionsOverlay.MoveSelection(-1)
		return m, nil

	case "down", "j":
		m.allSessionsOverlay.MoveSelection(1)
		return m, nil

	case "enter":
		return m.switchToOverlaySession()

	case "q", "ctrl+c":
		return m, tea.Quit

	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		n := int(msg.String()[0] - '0')
		if !m.allSessionsOverlay.SelectByNumber(n) {
			return m, nil
		}
		return m.switchToOverlaySession()
	}
	return m, nil
}

// switchToOverlaySession closes the overlay and switches to the selected session.
func (m Model) switchToOverlaySession() (tea.Model, tea.Cmd) {
	sess := m.allSessionsOverlay.SelectedSession()
	if sess == nil {
		m.allSessionsOverlay.Hide()
		m.focus = FocusOutput
		return m, nil
	}
	m.allSessionsOverlay.Hide()
	m.focus = FocusOutput
	if m.sessionManager.IsInTmuxMode() {
		if sess.TmuxWindowName != "" {
			return m, func() tea.Msg {
				cmd := exec.Command("tmux", "select-window", "-t", sess.TmuxWindowName)
				if err := cmd.Run(); err != nil {
					return errMsg{fmt.Errorf("failed to switch to tmux window: %w", err)}
				}
				return nil
			}
		}
		toastCmd := m.addToast("Session has no tmux window", ToastInfo)
		return m, toastCmd
	}
	m.switchViewingSession(sess.ID)
	return m, nil
}

// handleHelpOverlay handles key presses when the help overlay is visible.
func (m Model) handleHelpOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "?", "esc":
		// Close help, restore previous focus
		m.focus = m.helpOverlay.previousFocus
		return m, nil
	case "up", "k":
		m.helpOverlay.ScrollUp()
		return m, nil
	case "down", "j":
		m.helpOverlay.ScrollDown()
		return m, nil
	case "q", "ctrl+c":
		return m, tea.Quit
	}
	// Ignore all other keys while help is open
	return m, nil
}

// handleThemePicker handles key presses when the theme picker overlay is visible.
func (m Model) handleThemePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		// Revert to original theme
		origName := m.themePicker.OriginalTheme()
		if origName != m.styles.Palette.Name {
			if p, ok := ThemeByName(origName); ok {
				m.applyTheme(p)
			}
		}
		m.themePicker.Hide()
		m.focus = FocusOutput
		return m, nil

	case "enter":
		// Confirm the selected theme and persist
		selected := m.themePicker.SelectedTheme()
		m.applyTheme(selected)
		m.settings.ThemeName = selected.Name
		m.themePicker.Hide()
		m.focus = FocusOutput
		var toastCmd tea.Cmd
		if err := SaveSettings(m.settings); err != nil {
			toastCmd = m.addToast("Theme applied but failed to save: "+err.Error(), ToastError)
		} else {
			toastCmd = m.addToast("Theme set to "+selected.Name, ToastSuccess)
		}
		return m, toastCmd

	case "up", "k":
		prev := m.themePicker.SelectedTheme().Name
		m.themePicker.MoveSelection(-1)
		if sel := m.themePicker.SelectedTheme(); sel.Name != prev {
			m.applyTheme(sel)
		}
		return m, nil

	case "down", "j":
		prev := m.themePicker.SelectedTheme().Name
		m.themePicker.MoveSelection(1)
		if sel := m.themePicker.SelectedTheme(); sel.Name != prev {
			m.applyTheme(sel)
		}
		return m, nil

	case "q", "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

// handleRepoSettingsDialog handles key presses when the repo settings dialog is visible.
func (m Model) handleRepoSettingsDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	prevTheme := m.repoSettingsDialog.SelectedTheme().Name
	action, cmd := m.repoSettingsDialog.Update(msg)
	if sel := m.repoSettingsDialog.SelectedTheme(); sel.Name != prevTheme {
		m.applyTheme(sel)
	}
	switch action {
	case RepoSettingsActionQuit:
		return m, tea.Quit
	case RepoSettingsActionCancel:
		origName := m.repoSettingsDialog.OriginalThemeName()
		if origName != m.styles.Palette.Name {
			if p, ok := ThemeByName(origName); ok {
				m.applyTheme(p)
			}
		}
		m.repoSettingsDialog.Hide()
		m.focus = FocusOutput
		return m, cmd
	case RepoSettingsActionSave:
		cfg := m.repoSettingsDialog.RepoSettings()
		selected := m.repoSettingsDialog.SelectedTheme()
		m.applyTheme(selected)
		m.settings.ThemeName = selected.Name
		m.settings.SetRepoSettings(m.repoName, cfg)

		// Save provider preferences and rebuild the model registry
		enabledProviders := m.repoSettingsDialog.EnabledProviders()
		m.settings.SetEnabledProviders(enabledProviders)
		if m.modelRegistry != nil && m.providerAvailability != nil {
			m.modelRegistry.Rebuild(m.providerAvailability, m.settings.EnabledProviders)
			// Reset defaults if current defaults are no longer available
			if _, ok := m.modelRegistry.ModelByID(m.defaultPlanModel); !ok {
				if models := m.modelRegistry.Models(); len(models) > 0 {
					m.defaultPlanModel = models[0].ID
				}
			}
			if _, ok := m.modelRegistry.ModelByID(m.defaultBuildModel); !ok {
				if models := m.modelRegistry.Models(); len(models) > 0 {
					m.defaultBuildModel = models[0].ID
				}
			}
		}

		m.repoSettingsDialog.Hide()
		m.focus = FocusOutput
		err := SaveSettings(m.settings)
		if err != nil {
			return m, tea.Batch(cmd, m.addToast("Settings updated but failed to save: "+err.Error(), ToastError))
		}
		return m, tea.Batch(cmd, m.addToast("Repo settings saved", ToastSuccess))
	default:
		return m, cmd
	}
}

func parseHookOutput(out string) []string {
	var messages []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			messages = append(messages, line)
		}
	}
	return messages
}

func runRepoHookCommands(commands []string, worktreePath, branch string, messages *[]string) error {
	env := os.Environ()
	env = append(env, "WT_BRANCH="+branch, "WT_PATH="+worktreePath)

	for _, cmdStr := range commands {
		cmdStr = strings.TrimSpace(cmdStr)
		if cmdStr == "" {
			continue
		}
		*messages = append(*messages, "Running: "+cmdStr)
		cmd := exec.Command("sh", "-c", cmdStr)
		cmd.Dir = worktreePath
		cmd.Env = env

		// Capture output to buffer instead of writing to os.Stdout/Stderr
		// to prevent TUI corruption
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf

		if err := cmd.Run(); err != nil {
			*messages = append(*messages, "Command failed: "+cmdStr)
			// Include stderr output in messages for debugging
			if errBuf.Len() > 0 {
				for _, line := range strings.Split(strings.TrimSpace(errBuf.String()), "\n") {
					if line != "" {
						*messages = append(*messages, "  "+line)
					}
				}
			}
			return err
		}

		// Include stdout output in messages
		if outBuf.Len() > 0 {
			for _, line := range strings.Split(strings.TrimSpace(outBuf.String()), "\n") {
				if line != "" {
					*messages = append(*messages, "  "+line)
				}
			}
		}
	}
	return nil
}

func extractHookWarning(messages []string) string {
	for _, msg := range messages {
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "hook failed") {
			return "Worktree operation completed, but hook command failed"
		}
	}
	return ""
}
