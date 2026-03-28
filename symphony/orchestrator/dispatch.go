package orchestrator

import (
	"sort"

	"github.com/bazelment/yoloswe/symphony/config"
	"github.com/bazelment/yoloswe/symphony/model"
)

// sortForDispatch sorts issues by dispatch priority.
// Spec Section 8.2: priority ascending (null last), created_at oldest first, identifier lexicographic.
func sortForDispatch(issues []model.Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]

		// Priority ascending, null sorts last.
		aPri := priorityOr(a.Priority, 999999)
		bPri := priorityOr(b.Priority, 999999)
		if aPri != bPri {
			return aPri < bPri
		}

		// Created at oldest first.
		if a.CreatedAt != nil && b.CreatedAt != nil {
			if !a.CreatedAt.Equal(*b.CreatedAt) {
				return a.CreatedAt.Before(*b.CreatedAt)
			}
		} else if a.CreatedAt != nil {
			return true // a has date, b doesn't → a first
		} else if b.CreatedAt != nil {
			return false
		}

		// Identifier lexicographic.
		return a.Identifier < b.Identifier
	})
}

func priorityOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// shouldDispatch checks if an issue is eligible for dispatch.
// Spec Section 8.2: Candidate Selection Rules.
func (o *Orchestrator) shouldDispatch(issue model.Issue, cfg *config.ServiceConfig) bool {
	// Required fields.
	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" || issue.State == "" {
		return false
	}

	// Not already running or claimed.
	if _, running := o.running[issue.ID]; running {
		return false
	}
	if _, claimed := o.claimed[issue.ID]; claimed {
		return false
	}

	// State must be active and not terminal.
	normState := model.NormalizeState(issue.State)
	isActive := false
	for _, s := range cfg.ActiveStates {
		if model.NormalizeState(s) == normState {
			isActive = true
			break
		}
	}
	if !isActive {
		return false
	}
	for _, s := range cfg.TerminalStates {
		if model.NormalizeState(s) == normState {
			return false
		}
	}

	// Global concurrency.
	if o.availableSlots(cfg) <= 0 {
		return false
	}

	// Per-state concurrency.
	if !o.perStateSlotAvailable(issue.State, cfg) {
		return false
	}

	// Blocker rule for Todo state. Spec Section 8.2.
	if normState == "todo" {
		for _, b := range issue.BlockedBy {
			if !o.isBlockerTerminal(b, cfg) {
				return false
			}
		}
	}

	return true
}

// availableSlots returns the number of global concurrency slots available.
// Spec Section 8.3.
func (o *Orchestrator) availableSlots(cfg *config.ServiceConfig) int {
	slots := cfg.MaxConcurrentAgents - len(o.running)
	if slots < 0 {
		return 0
	}
	return slots
}

// perStateSlotAvailable checks if per-state concurrency allows another dispatch.
func (o *Orchestrator) perStateSlotAvailable(state string, cfg *config.ServiceConfig) bool {
	if cfg.MaxConcurrentByState == nil {
		return true
	}
	normState := model.NormalizeState(state)
	limit, hasLimit := cfg.MaxConcurrentByState[normState]
	if !hasLimit {
		return true
	}

	count := o.perStateCount(normState)
	return count < limit
}

// perStateCount counts running issues with the given normalized state.
func (o *Orchestrator) perStateCount(normState string) int {
	count := 0
	for _, entry := range o.running {
		if model.NormalizeState(entry.Issue.State) == normState {
			count++
		}
	}
	return count
}

// isBlockerTerminal checks if a blocker is in a terminal state.
func (o *Orchestrator) isBlockerTerminal(b model.BlockerRef, cfg *config.ServiceConfig) bool {
	if b.State == nil {
		return false
	}
	normState := model.NormalizeState(*b.State)
	for _, ts := range cfg.TerminalStates {
		if model.NormalizeState(ts) == normState {
			return true
		}
	}
	return false
}
