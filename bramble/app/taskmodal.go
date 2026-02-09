package app

import (
	"context"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/bazelment/yoloswe/wt/taskrouter"
)

// TaskModalState represents the current state of the new task flow.
type TaskModalState int

const (
	TaskModalHidden   TaskModalState = iota
	TaskModalInput                   // User entering task description
	TaskModalRouting                 // AI is deciding where to route
	TaskModalProposal                // Showing AI proposal
	TaskModalAdjust                  // User adjusting the proposal
)

// TaskModal handles the "new task" flow UI.
type TaskModal struct {
	err            error
	textArea       *TextArea
	adjustTextArea *TextArea // for editing branch name in adjust state
	proposal       *taskrouter.RouteProposal
	adjustWorktree string
	adjustParent   string
	state          TaskModalState
	width          int
	height         int
}

// NewTaskModal creates a new task modal.
func NewTaskModal() *TaskModal {
	ta := NewTextArea()
	ta.SetMinHeight(3)
	ta.SetMaxHeight(8)
	ta.SetLabels("Continue", "Cancel")

	adjustTA := NewTextArea()
	adjustTA.SetMinHeight(1)
	adjustTA.SetMaxHeight(1)
	adjustTA.SetLabels("Confirm", "Back")

	return &TaskModal{
		state:          TaskModalHidden,
		textArea:       ta,
		adjustTextArea: adjustTA,
	}
}

// SetSize sets the modal dimensions.
func (m *TaskModal) SetSize(w, h int) {
	m.width = w
	m.height = h
}

// IsVisible returns true if the modal is visible.
func (m *TaskModal) IsVisible() bool {
	return m.state != TaskModalHidden
}

// State returns the current state.
func (m *TaskModal) State() TaskModalState {
	return m.state
}

// Show shows the modal in input state.
func (m *TaskModal) Show() {
	m.state = TaskModalInput
	m.textArea.Reset()
	m.textArea.SetPlaceholder("Describe what you want to work on...")
	m.proposal = nil
	m.err = nil
}

// Hide hides the modal.
func (m *TaskModal) Hide() {
	m.state = TaskModalHidden
}

// SetPrompt sets the task prompt (for direct string setting).
func (m *TaskModal) SetPrompt(prompt string) {
	m.textArea.SetValue(prompt)
}

// Prompt returns the current prompt.
func (m *TaskModal) Prompt() string {
	return m.textArea.Value()
}

// TextArea returns the text area for direct manipulation.
func (m *TaskModal) TextArea() *TextArea {
	return m.textArea
}

// StartRouting transitions to the routing state.
func (m *TaskModal) StartRouting() {
	m.state = TaskModalRouting
}

// SetProposal sets the routing proposal.
func (m *TaskModal) SetProposal(proposal *taskrouter.RouteProposal) {
	m.proposal = proposal
	m.state = TaskModalProposal
	// Initialize adjustment fields
	if proposal != nil {
		m.adjustWorktree = proposal.Worktree
		m.adjustParent = proposal.Parent
	}
}

// SetError sets an error.
func (m *TaskModal) SetError(err error) {
	m.err = err
	m.state = TaskModalProposal // Show error in proposal state
}

// Proposal returns the current proposal.
func (m *TaskModal) Proposal() *taskrouter.RouteProposal {
	return m.proposal
}

// Error returns the current error.
func (m *TaskModal) Error() error {
	return m.err
}

// StartAdjust transitions to the adjust state with pre-populated branch name.
func (m *TaskModal) StartAdjust() {
	if m.proposal != nil {
		m.state = TaskModalAdjust
		m.adjustTextArea.Reset()
		m.adjustTextArea.SetValue(m.adjustWorktree)
		m.adjustTextArea.SetPlaceholder("e.g. feature/my-feature")
	}
}

// AdjustTextArea returns the text area for the adjust state.
func (m *TaskModal) AdjustTextArea() *TextArea {
	return m.adjustTextArea
}

// AdjustedWorktree returns the adjusted worktree name.
func (m *TaskModal) AdjustedWorktree() string {
	return m.adjustWorktree
}

// AdjustedParent returns the adjusted parent name.
func (m *TaskModal) AdjustedParent() string {
	return m.adjustParent
}

// View renders the task modal.
func (m *TaskModal) View() string {
	if m.state == TaskModalHidden {
		return ""
	}

	boxWidth := 70
	if m.width > 0 && m.width < 80 {
		boxWidth = m.width - 10
	}

	var content strings.Builder

	switch m.state {
	case TaskModalInput:
		content.WriteString(titleStyle.Render("New task — Describe what you want to work on:"))
		content.WriteString("\n")

		// Render the text area (includes buttons)
		m.textArea.SetWidth(boxWidth - 6)
		m.textArea.SetPrompt("")
		content.WriteString(m.textArea.View())

	case TaskModalRouting:
		content.WriteString(titleStyle.Render("New task"))
		content.WriteString("\n\n")
		content.WriteString(dimStyle.Render("  Deciding where to run this..."))
		content.WriteString("\n")

	case TaskModalProposal:
		if m.err != nil {
			content.WriteString(titleStyle.Render("New task — Error"))
			content.WriteString("\n\n")
			content.WriteString(errorStyle.Render("  " + m.err.Error()))
			content.WriteString("\n\n")
			content.WriteString(dimStyle.Render("  " + formatKeyHints("Esc", "cancel")))
		} else if m.proposal != nil {
			content.WriteString(titleStyle.Render("New task — Proposal"))
			content.WriteString("\n\n")

			if m.proposal.Action == taskrouter.ActionUseExisting {
				content.WriteString("  Proposed: Use existing worktree ")
				content.WriteString(selectedStyle.Render(m.proposal.Worktree))
				content.WriteString("\n")
				content.WriteString("    → Start planning session with your prompt there.")
			} else {
				content.WriteString("  Proposed: Create worktree ")
				content.WriteString(selectedStyle.Render(m.proposal.Worktree))
				content.WriteString("\n")
				content.WriteString("    from ")
				content.WriteString(dimStyle.Render(m.proposal.Parent))
				content.WriteString(" → start planning session there.")
			}
			content.WriteString("\n\n")

			if m.proposal.Reasoning != "" {
				content.WriteString(dimStyle.Render("  Reasoning: " + truncate(m.proposal.Reasoning, boxWidth-14)))
				content.WriteString("\n\n")
			}

			content.WriteString(dimStyle.Render("  " + formatKeyHints("Enter", "confirm") + "  " + formatKeyHints("a", "adjust") + "  " + formatKeyHints("Esc", "cancel")))
		}

	case TaskModalAdjust:
		content.WriteString(titleStyle.Render("New task — Adjust"))
		content.WriteString("\n\n")
		if m.proposal.Action == taskrouter.ActionUseExisting {
			content.WriteString("  Worktree: " + m.adjustWorktree)
			content.WriteString("\n\n")
			content.WriteString(dimStyle.Render("  (Use ↑/↓ to select from existing worktrees)"))
			content.WriteString("\n\n")
			content.WriteString(dimStyle.Render("  " + formatKeyHints("Enter", "confirm") + "  " + formatKeyHints("Esc", "back")))
		} else {
			content.WriteString("  Branch name:\n")
			m.adjustTextArea.SetWidth(boxWidth - 10)
			m.adjustTextArea.SetPrompt("")
			content.WriteString(m.adjustTextArea.View())
			content.WriteString("\n")
			content.WriteString("  Parent: " + dimStyle.Render(m.adjustParent))
		}
	}

	// Create bordered box
	box := modalBoxStyle.Width(boxWidth).Render(content.String())

	// Center the box
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(
			m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			box,
		)
	}
	return box
}

// BuildRouteRequest builds a route request from the current state.
func (m *TaskModal) BuildRouteRequest(worktrees []taskrouter.WorktreeInfo, currentWT, repoName string) taskrouter.RouteRequest {
	return taskrouter.RouteRequest{
		Prompt:    m.Prompt(),
		Worktrees: worktrees,
		CurrentWT: currentWT,
		RepoName:  repoName,
	}
}

// MockRouteForTesting returns a mock proposal for testing without AI.
func MockRouteForTesting(prompt string, hasWorktrees bool) *taskrouter.RouteProposal {
	if hasWorktrees {
		return &taskrouter.RouteProposal{
			Action:    taskrouter.ActionCreateNew,
			Worktree:  suggestBranchName(prompt),
			Parent:    "main",
			Reasoning: "Creating new branch for this task",
		}
	}
	return &taskrouter.RouteProposal{
		Action:    taskrouter.ActionCreateNew,
		Worktree:  suggestBranchName(prompt),
		Parent:    "main",
		Reasoning: "First feature branch for this repo",
	}
}

// suggestBranchName generates a simple branch name from a prompt.
func suggestBranchName(prompt string) string {
	// Simple heuristic: take first few words, make kebab-case
	words := strings.Fields(strings.ToLower(prompt))
	if len(words) > 4 {
		words = words[:4]
	}

	// Filter common words
	filtered := make([]string, 0, len(words))
	commonWords := map[string]bool{"a": true, "an": true, "the": true, "to": true, "for": true, "and": true, "or": true, "in": true, "on": true, "with": true}
	for _, w := range words {
		// Remove punctuation
		w = strings.Trim(w, ".,!?;:")
		if !commonWords[w] && len(w) > 1 {
			filtered = append(filtered, w)
		}
	}

	if len(filtered) == 0 {
		return "feature-new"
	}

	return "feature-" + strings.Join(filtered, "-")
}

// RouteTask runs the task router (placeholder - actual implementation would use the router).
func RouteTask(ctx context.Context, router *taskrouter.Router, req taskrouter.RouteRequest) (*taskrouter.RouteProposal, error) {
	if router == nil {
		// Use mock for now
		return MockRouteForTesting(req.Prompt, len(req.Worktrees) > 0), nil
	}
	return router.Route(ctx, req)
}
