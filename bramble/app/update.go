package app

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
	"github.com/bazelment/yoloswe/wt/taskrouter"
)

// Update handles messages and updates the model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Handle task modal first
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
		// Update dropdown widths
		m.worktreeDropdown.SetWidth(m.width * 2 / 3)
		m.sessionDropdown.SetWidth(m.width / 2)
		// Initialize or update markdown renderer
		if m.mdRenderer == nil {
			m.mdRenderer, _ = NewMarkdownRenderer(m.width - 8)
		} else {
			_ = m.mdRenderer.SetWidth(m.width - 8)
		}
		return m, nil

	case worktreesMsg:
		m.worktrees = msg.worktrees
		m.updateWorktreeDropdown()

		// Check for pending worktree selection (from task creation)
		if m.pendingWorktreeSelect != "" {
			worktreeName := m.pendingWorktreeSelect
			prompt := m.pendingPlannerPrompt
			m.pendingWorktreeSelect = ""
			m.pendingPlannerPrompt = ""
			m.worktreeDropdown.SelectByID(worktreeName)
			m.updateSessionDropdown()
			model, cmd := m.startSession(session.SessionTypePlanner, prompt)
			return model, tea.Batch(cmd, m.refreshWorktreeStatuses())
		}

		// Auto-select first worktree if none selected
		if m.worktreeDropdown.SelectedItem() == nil && len(m.worktrees) > 0 {
			m.worktreeDropdown.SelectIndex(0)
		}
		// Update session dropdown for new worktree
		m.updateSessionDropdown()
		cmds = append(cmds, m.refreshWorktreeStatuses())
		return m, tea.Batch(cmds...)

	case worktreeStatusMsg:
		m.worktreeStatuses = msg.statuses
		m.updateWorktreeDropdown()
		// Schedule next refresh in 30 seconds
		cmds = append(cmds, tea.Tick(30*time.Second, func(t time.Time) tea.Msg {
			return refreshStatusTickMsg{}
		}))
		return m, tea.Batch(cmds...)

	case refreshStatusTickMsg:
		return m, m.refreshWorktreeStatuses()

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
		m.lastError = msg.Error()
		return m, nil

	case promptInputMsg:
		// Input completed
		m.inputMode = false
		if m.inputHandler != nil {
			cmd := m.inputHandler(msg.value)
			m.inputHandler = nil
			return m, cmd
		}
		return m, nil

	case startSessionMsg:
		return m.startSession(msg.sessionType, msg.prompt)

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
			m.lastError = msg.err.Error()
		}
		m.worktreeOpMessages = msg.messages
		return m, m.refreshWorktrees()

	case editorResultMsg:
		if msg.err != nil {
			m.lastError = "Failed to open editor: " + msg.err.Error()
		}
		return m, nil

	case taskWorktreeCreatedMsg:
		m.worktreeOpMessages = msg.messages
		// Set pending selection - will be processed after worktrees refresh
		m.pendingWorktreeSelect = msg.worktreeName
		m.pendingPlannerPrompt = msg.prompt
		return m, m.refreshWorktrees()

	case deleteWorktreeMsg:
		return m.deleteWorktree(msg.branch, msg.deleteBranch)

	case tickMsg:
		// Continue ticking for running tool timer animation
		return m, tickCmd()
	}

	return m, tea.Batch(cmds...)
}

// handleKeyPress handles key presses in normal mode (not input, not dropdown).
func (m Model) handleKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "alt+w":
		// Open worktree dropdown
		m.worktreeDropdown.Open()
		m.focus = FocusWorktreeDropdown
		return m, nil

	case "alt+s":
		// In tmux mode, Alt-S does nothing (no dropdown)
		if m.sessionManager.IsInTmuxMode() {
			return m, nil
		}
		// TUI mode: open session dropdown
		m.sessionDropdown.Open()
		m.focus = FocusSessionDropdown
		return m, nil

	// Output scrolling (TUI mode) or session list navigation (tmux mode)
	case "up", "k":
		if m.sessionManager.IsInTmuxMode() {
			// Tmux mode: navigate session list
			if m.selectedSessionIndex > 0 {
				m.selectedSessionIndex--
			}
		} else {
			// TUI mode: scroll output
			m.scrollOutput(1)
		}
		return m, nil

	case "down", "j":
		if m.sessionManager.IsInTmuxMode() {
			// Tmux mode: navigate session list
			// Find number of sessions for current worktree
			var sessionCount int
			if wt := m.selectedWorktree(); wt != nil {
				allSessions := m.sessionManager.GetAllSessions()
				for _, sess := range allSessions {
					if sess.WorktreePath == wt.Path {
						sessionCount++
					}
				}
			}
			if m.selectedSessionIndex < sessionCount-1 {
				m.selectedSessionIndex++
			}
		} else {
			// TUI mode: scroll output
			m.scrollOutput(-1)
		}
		return m, nil

	case "enter":
		// In tmux mode, Enter switches to the selected window
		if m.sessionManager.IsInTmuxMode() {
			// Get the currently selected session
			var currentSessions []session.SessionInfo
			if wt := m.selectedWorktree(); wt != nil {
				allSessions := m.sessionManager.GetAllSessions()
				for _, sess := range allSessions {
					if sess.WorktreePath == wt.Path {
						currentSessions = append(currentSessions, sess)
					}
				}
			}

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
				} else {
					m.lastError = "Session has no tmux window name"
				}
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
			return m.promptInput("Branch name: ", func(branch string) tea.Cmd {
				return func() tea.Msg {
					return createWorktreeMsg{branch}
				}
			})
		}
		return m, nil

	case "t":
		// New task (prompt-first flow with AI routing)
		m.taskModal.SetSize(m.width, m.height)
		m.taskModal.Show()
		m.focus = FocusTaskModal
		return m, nil

	case "p":
		// Start planner
		if m.selectedWorktree() != nil {
			return m.promptInput("Plan prompt: ", func(prompt string) tea.Cmd {
				return func() tea.Msg {
					return startSessionMsg{session.SessionTypePlanner, prompt}
				}
			})
		}
		return m, nil

	case "b":
		// Start builder
		if m.selectedWorktree() != nil {
			return m.promptInput("Build prompt: ", func(prompt string) tea.Cmd {
				return func() tea.Msg {
					return startSessionMsg{session.SessionTypeBuilder, prompt}
				}
			})
		}
		return m, nil

	case "e":
		// Open editor for worktree
		if wt := m.selectedWorktree(); wt != nil {
			return m, func() tea.Msg {
				cmd := exec.Command(m.editor, wt.Path)
				err := cmd.Start()
				return editorResultMsg{err: err}
			}
		}
		return m, nil

	case "s":
		// Stop session with confirmation (TUI mode only)
		if m.sessionManager.IsInTmuxMode() {
			m.lastError = "Close tmux windows directly with prefix+& or 'exit' command"
			return m, nil
		}
		if sess := m.selectedSession(); sess != nil {
			sessID := sess.ID
			title := sess.Title
			if title == "" {
				title = string(sessID)[:12]
			}
			return m.promptInput("Stop session '"+title+"'? [y/n]: ", func(answer string) tea.Cmd {
				if strings.TrimSpace(strings.ToLower(answer)) == "y" {
					return func() tea.Msg {
						m.sessionManager.StopSession(sessID)
						return sessionsUpdated{}
					}
				}
				return nil
			})
		}
		return m, nil

	case "f":
		// Follow-up on idle session (TUI mode only)
		if m.sessionManager.IsInTmuxMode() {
			m.lastError = "Follow-ups must be done in the tmux window directly"
			return m, nil
		}
		if sess := m.selectedSession(); sess != nil && sess.Status == session.StatusIdle {
			return m.promptInput("Follow-up: ", func(message string) tea.Cmd {
				return func() tea.Msg {
					if err := m.sessionManager.SendFollowUp(sess.ID, message); err != nil {
						return errMsg{err}
					}
					return sessionsUpdated{}
				}
			})
		}
		return m, nil

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
			sessionID, err := m.sessionManager.StartSession(session.SessionTypeBuilder, worktreePath, planPrompt)
			if err != nil {
				m.lastError = err.Error()
				return m, nil
			}
			m.viewingSessionID = sessionID
			m.sessions = m.sessionManager.GetAllSessions()
			m.updateSessionDropdown()
			return m, nil
		}
		return m, nil

	case "d":
		// Delete worktree
		if w := m.selectedWorktree(); w != nil {
			branch := w.Branch
			return m.promptInput("Delete worktree '"+branch+"'? [y]es / [y+branch] yes and delete branch / [n]o: ", func(answer string) tea.Cmd {
				switch strings.TrimSpace(strings.ToLower(answer)) {
				case "y", "yes":
					return func() tea.Msg {
						return deleteWorktreeMsg{branch: branch, deleteBranch: false}
					}
				case "y+branch":
					return func() tea.Msg {
						return deleteWorktreeMsg{branch: branch, deleteBranch: true}
					}
				default:
					return nil // Cancel
				}
			})
		}
		return m, nil

	case "r":
		// Refresh
		return m, m.refreshWorktrees()

	case "esc":
		// Clear error and reset scroll
		m.lastError = ""
		m.scrollOffset = 0
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

// handleDropdownMode handles key presses when a dropdown is open.
func (m Model) handleDropdownMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "alt+w", "alt+s":
		// Close dropdown
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

	case "enter":
		if m.focus == FocusWorktreeDropdown {
			// Worktree selected - update session dropdown
			m.worktreeDropdown.Close()
			m.updateSessionDropdown()
			// Clear viewing session when switching worktrees
			m.viewingSessionID = ""
			m.viewingHistoryData = nil
			m.scrollOffset = 0
		} else {
			// Session selected - view it
			if item := m.sessionDropdown.SelectedItem(); item != nil {
				if item.ID == "---separator---" {
					// Can't select separator
					return m, nil
				}
				m.viewingSessionID = session.SessionID(item.ID)
				m.scrollOffset = 0 // Reset scroll when switching sessions
				// Check if this is a live session or history
				if _, ok := m.sessionManager.GetSessionInfo(m.viewingSessionID); ok {
					// Live session
					m.viewingHistoryData = nil
				} else {
					// History session - load from store
					wt := m.selectedWorktree()
					if wt != nil {
						histData, err := m.sessionManager.LoadSessionFromHistory(wt.Branch, m.viewingSessionID)
						if err == nil {
							m.viewingHistoryData = histData
						} else {
							m.lastError = "Failed to load history: " + err.Error()
							m.viewingHistoryData = nil
						}
					}
				}
			}
			m.sessionDropdown.Close()
		}
		m.focus = FocusOutput
		return m, nil

	case "q", "ctrl+c":
		return m, tea.Quit
	}

	return m, nil
}

// handleInputMode handles key presses in input mode.
// Tab cycles focus between text input and buttons.
// Enter activates the focused element.
func (m Model) handleInputMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "tab":
		// Cycle focus forward
		m.inputArea.CycleForward()
		return m, nil

	case "shift+tab":
		// Cycle focus backward
		m.inputArea.CycleBackward()
		return m, nil

	case "ctrl+enter":
		// Submit from any focus
		value := m.inputArea.Value()
		if value == "" {
			return m, nil
		}
		m.inputArea.Reset()
		return m, func() tea.Msg {
			return promptInputMsg{value}
		}

	case "enter":
		// Action depends on current focus
		switch m.inputArea.Focus() {
		case FocusTextInput:
			// Insert newline when editing text
			m.inputArea.InsertNewline()
			return m, nil
		case FocusSendButton:
			// Submit the prompt
			value := m.inputArea.Value()
			if value == "" {
				return m, nil // Don't submit empty
			}
			m.inputArea.Reset()
			return m, func() tea.Msg {
				return promptInputMsg{value}
			}
		case FocusCancelButton:
			// Cancel
			m.inputMode = false
			m.inputArea.Reset()
			m.inputHandler = nil
			return m, nil
		}
		return m, nil

	case "esc":
		m.inputMode = false
		m.inputArea.Reset()
		m.inputHandler = nil
		return m, nil

	case "backspace":
		if m.inputArea.Focus() == FocusTextInput {
			m.inputArea.DeleteChar()
		}
		return m, nil

	case "delete":
		if m.inputArea.Focus() == FocusTextInput {
			m.inputArea.DeleteCharForward()
		}
		return m, nil

	case "up":
		if m.inputArea.Focus() == FocusTextInput {
			m.inputArea.MoveCursorUp()
		}
		return m, nil

	case "down":
		if m.inputArea.Focus() == FocusTextInput {
			m.inputArea.MoveCursorDown()
		}
		return m, nil

	case "left":
		if m.inputArea.Focus() == FocusTextInput {
			m.inputArea.MoveCursorLeft()
		}
		return m, nil

	case "right":
		if m.inputArea.Focus() == FocusTextInput {
			m.inputArea.MoveCursorRight()
		}
		return m, nil

	case "ctrl+c":
		return m, tea.Quit

	default:
		// Only insert characters when text input is focused
		if m.inputArea.Focus() == FocusTextInput {
			keyStr := msg.String()
			if keyStr == "space" {
				m.inputArea.InsertChar(' ')
			} else if len(keyStr) == 1 {
				m.inputArea.InsertChar(rune(keyStr[0]))
			} else if len(msg.Runes) > 0 {
				for _, r := range msg.Runes {
					m.inputArea.InsertChar(r)
				}
			}
		}
		return m, nil
	}
}

// promptInput switches to input mode.
func (m Model) promptInput(prompt string, handler func(string) tea.Cmd) (tea.Model, tea.Cmd) {
	m.inputMode = true
	m.inputPrompt = prompt
	m.inputArea.Reset()
	m.inputHandler = handler
	m.focus = FocusInput
	return m, nil
}

// startSession starts a session of the given type.
func (m Model) startSession(sessionType session.SessionType, prompt string) (tea.Model, tea.Cmd) {
	wt := m.selectedWorktree()
	if wt == nil || prompt == "" {
		return m, nil
	}

	sessionID, err := m.sessionManager.StartSession(sessionType, wt.Path, prompt)
	if err != nil {
		m.lastError = err.Error()
		return m, nil
	}

	m.viewingSessionID = sessionID
	m.sessions = m.sessionManager.GetAllSessions()
	m.updateSessionDropdown()
	return m, nil
}

// createWorktree creates a new worktree asynchronously with captured output.
func (m Model) createWorktree(branch string) (tea.Model, tea.Cmd) {
	if branch == "" {
		return m, nil
	}

	if m.repoName == "" {
		m.lastError = "No repository selected"
		return m, nil
	}

	// Show pending message
	m.worktreeOpMessages = []string{"Creating worktree " + branch + "..."}

	// Run asynchronously
	wtRoot := m.wtRoot
	repoName := m.repoName
	ctx := m.ctx
	return m, func() tea.Msg {
		var buf bytes.Buffer
		output := wt.NewOutput(&buf, false) // No colors for captured output
		manager := wt.NewManager(wtRoot, repoName, wt.WithOutput(output))

		_, err := manager.New(ctx, branch, "", "")

		// Parse captured messages
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

// deleteWorktree deletes a worktree asynchronously.
func (m Model) deleteWorktree(branch string, deleteBranch bool) (tea.Model, tea.Cmd) {
	if branch == "" || m.repoName == "" {
		return m, nil
	}

	// Clear viewing session if it belongs to this worktree
	if w := m.selectedWorktree(); w != nil && w.Branch == branch {
		m.viewingSessionID = ""
		m.viewingHistoryData = nil
		m.scrollOffset = 0
	}

	// Show pending message
	m.worktreeOpMessages = []string{"Deleting worktree " + branch + "..."}

	wtRoot := m.wtRoot
	repoName := m.repoName
	ctx := m.ctx
	return m, func() tea.Msg {
		var buf bytes.Buffer
		output := wt.NewOutput(&buf, false)
		manager := wt.NewManager(wtRoot, repoName, wt.WithOutput(output))

		err := manager.Remove(ctx, branch, deleteBranch)

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
		switch msg.String() {
		case "tab":
			ta.CycleForward()
			return m, nil

		case "shift+tab":
			ta.CycleBackward()
			return m, nil

		case "esc":
			m.taskModal.Hide()
			m.focus = FocusOutput
			return m, nil

		case "ctrl+enter":
			// Submit from any focus
			prompt := m.taskModal.Prompt()
			if prompt != "" {
				return m, func() tea.Msg {
					return taskRouteMsg{prompt: prompt}
				}
			}
			return m, nil

		case "enter":
			// Action depends on current focus
			switch ta.Focus() {
			case FocusTextInput:
				// Insert newline when editing text
				ta.InsertNewline()
				return m, nil
			case FocusSendButton:
				// Submit the prompt
				prompt := m.taskModal.Prompt()
				if prompt != "" {
					return m, func() tea.Msg {
						return taskRouteMsg{prompt: prompt}
					}
				}
				return m, nil
			case FocusCancelButton:
				// Cancel
				m.taskModal.Hide()
				m.focus = FocusOutput
				return m, nil
			}
			return m, nil

		case "backspace":
			if ta.Focus() == FocusTextInput {
				ta.DeleteChar()
			}
			return m, nil

		case "delete":
			if ta.Focus() == FocusTextInput {
				ta.DeleteCharForward()
			}
			return m, nil

		case "up":
			if ta.Focus() == FocusTextInput {
				ta.MoveCursorUp()
			}
			return m, nil

		case "down":
			if ta.Focus() == FocusTextInput {
				ta.MoveCursorDown()
			}
			return m, nil

		case "left":
			if ta.Focus() == FocusTextInput {
				ta.MoveCursorLeft()
			}
			return m, nil

		case "right":
			if ta.Focus() == FocusTextInput {
				ta.MoveCursorRight()
			}
			return m, nil

		case "ctrl+c":
			return m, tea.Quit

		default:
			// Only insert characters when text input is focused
			if ta.Focus() == FocusTextInput {
				keyStr := msg.String()
				if keyStr == "space" {
					ta.InsertChar(' ')
				} else if len(keyStr) == 1 {
					ta.InsertChar(rune(keyStr[0]))
				} else if len(msg.Runes) > 0 {
					for _, r := range msg.Runes {
						ta.InsertChar(r)
					}
				}
			}
			return m, nil
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
		switch msg.String() {
		case "esc":
			// Go back to proposal
			m.taskModal.SetProposal(m.taskModal.Proposal())
			return m, nil

		case "enter":
			// Confirm with adjustments
			return m, func() tea.Msg {
				return taskConfirmMsg{
					worktree: m.taskModal.AdjustedWorktree(),
					parent:   m.taskModal.AdjustedParent(),
					isNew:    m.taskModal.Proposal().Action == taskrouter.ActionCreateNew,
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
	return func() tea.Msg {
		// Build worktree info for the router
		worktreeInfos := make([]taskrouter.WorktreeInfo, len(m.worktrees))
		for i, wt := range m.worktrees {
			worktreeInfos[i] = taskrouter.WorktreeInfo{
				Name: wt.Branch,
				Path: wt.Path,
			}
		}

		// Use mock router for now (real router would need Codex client)
		proposal := MockRouteForTesting(prompt, len(worktreeInfos) > 0)

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

	// If creating a new worktree, do that first
	if msg.isNew {
		if m.repoName == "" {
			m.lastError = "No repository selected"
			return m, nil
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
		return m, func() tea.Msg {
			var buf bytes.Buffer
			output := wt.NewOutput(&buf, false)
			manager := wt.NewManager(wtRoot, repoName, wt.WithOutput(output))

			_, err := manager.New(ctx, worktreeName, parent, "")

			// Parse captured messages
			var messages []string
			for _, line := range strings.Split(buf.String(), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					messages = append(messages, line)
				}
			}

			if err != nil {
				return worktreeOpResultMsg{messages: messages, err: err}
			}

			// Return a message that will trigger worktree refresh and planner start
			return taskWorktreeCreatedMsg{
				messages:     messages,
				worktreeName: worktreeName,
				prompt:       prompt,
			}
		}
	}

	// Use existing worktree - select it and start planner
	m.worktreeDropdown.SelectByID(msg.worktree)
	return m.startSession(session.SessionTypePlanner, msg.prompt)
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
