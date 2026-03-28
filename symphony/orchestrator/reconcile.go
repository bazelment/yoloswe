package orchestrator

import (
	"context"

	"github.com/bazelment/yoloswe/symphony/config"
	"github.com/bazelment/yoloswe/symphony/model"
)

// reconcileRunningIssues checks stall detection and refreshes tracker state.
// Runs asynchronously in a goroutine, results sent via tickResults channel.
// Spec Section 8.5.
func (o *Orchestrator) reconcileAndFetch(ctx context.Context, cfg *config.ServiceConfig, runningSnapshot map[string]*model.RunningEntry) tickResult {
	var actions []reconcileAction

	// Part A: Stall detection. Spec Section 8.5 Part A.
	if cfg.CodexStallTimeoutMs > 0 {
		now := o.clock.Now()
		for issueID, entry := range runningSnapshot {
			var elapsed int64
			if entry.Session.LastCodexTimestamp != nil {
				elapsed = now.Sub(*entry.Session.LastCodexTimestamp).Milliseconds()
			} else {
				elapsed = now.Sub(entry.StartedAt).Milliseconds()
			}

			if elapsed > int64(cfg.CodexStallTimeoutMs) {
				actions = append(actions, reconcileAction{
					IssueID: issueID,
					Action:  reconcileStalled,
				})
			}
		}
	}

	// Part B: Tracker state refresh. Spec Section 8.5 Part B.
	runningIDs := make([]string, 0, len(runningSnapshot))
	for id := range runningSnapshot {
		runningIDs = append(runningIDs, id)
	}

	var candidates []model.Issue

	if len(runningIDs) > 0 {
		refreshed, err := o.tracker.FetchIssueStatesByIDs(ctx, runningIDs)
		if err != nil {
			// State refresh failed: keep workers running, try again next tick.
			o.logger.Debug("reconcile state refresh failed, keeping workers", "error", err)
		} else {
			refreshMap := make(map[string]model.Issue, len(refreshed))
			for i := range refreshed {
				refreshMap[refreshed[i].ID] = refreshed[i]
			}

			for issueID := range runningSnapshot {
				issue, found := refreshMap[issueID]
				if !found {
					continue
				}

				normState := model.NormalizeState(issue.State)
				isTerminal := false
				for _, ts := range cfg.TerminalStates {
					if model.NormalizeState(ts) == normState {
						isTerminal = true
						break
					}
				}

				if isTerminal {
					actions = append(actions, reconcileAction{
						IssueID:          issueID,
						Action:           reconcileTerminate,
						CleanupWorkspace: true,
					})
					continue
				}

				isActive := false
				for _, as := range cfg.ActiveStates {
					if model.NormalizeState(as) == normState {
						isActive = true
						break
					}
				}

				if isActive {
					actions = append(actions, reconcileAction{
						IssueID: issueID,
						Action:  reconcileUpdate,
						Issue:   &issue,
					})
				} else {
					// Neither active nor terminal: stop without cleanup.
					actions = append(actions, reconcileAction{
						IssueID:          issueID,
						Action:           reconcileTerminate,
						CleanupWorkspace: false,
					})
				}
			}
		}
	}

	// Fetch candidates for dispatch.
	fetched, err := o.tracker.FetchCandidateIssues(ctx, cfg.ActiveStates, cfg.TrackerProjectSlug)
	if err != nil {
		o.logger.Warn("candidate fetch failed, skipping dispatch", "error", err)
		return tickResult{ReconcileActions: actions, Err: err}
	}
	candidates = fetched

	return tickResult{ReconcileActions: actions, Candidates: candidates}
}
