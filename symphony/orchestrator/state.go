package orchestrator

import (
	"context"
	"time"

	"github.com/bazelment/yoloswe/symphony/config"
	"github.com/bazelment/yoloswe/symphony/model"
	"github.com/bazelment/yoloswe/symphony/workspace"
)

// startAsyncTick kicks off an async goroutine for reconcile + fetch.
// The event loop stays responsive while tracker API calls happen.
func (o *Orchestrator) startAsyncTick(ctx context.Context) {
	cfg := o.cfg()

	// Snapshot running entries for the goroutine (avoid concurrent map access).
	snapshot := make(map[string]*model.RunningEntry, len(o.running))
	for id, entry := range o.running {
		entryCopy := *entry
		snapshot[id] = &entryCopy
	}

	go func() {
		result := o.reconcileAndFetch(ctx, cfg, snapshot)
		select {
		case o.tickResults <- result:
		case <-ctx.Done():
		}
	}()
}

// handleTickResult processes the async tick results: apply reconcile actions, then dispatch.
func (o *Orchestrator) handleTickResult(ctx context.Context, tr tickResult) {
	cfg := o.cfg()

	// Apply reconcile actions.
	for _, action := range tr.ReconcileActions {
		switch action.Action {
		case reconcileUpdate:
			if entry, ok := o.running[action.IssueID]; ok && action.Issue != nil {
				entry.Issue = *action.Issue
			}
		case reconcileTerminate:
			o.terminateRunning(action.IssueID, action.CleanupWorkspace, cfg)
		case reconcileStalled:
			o.terminateRunning(action.IssueID, false, cfg)
			entry := o.running[action.IssueID]
			if entry != nil {
				o.scheduleRetry(action.IssueID, entry.Identifier, 1, "stalled", false)
			}
		}
	}

	// Skip dispatch if fetch failed.
	if tr.Err != nil {
		return
	}

	// Validate config before dispatch. Spec Section 6.3.
	if err := config.ValidateForDispatch(cfg); err != nil {
		o.logger.Error("dispatch validation failed", "error", err)
		return
	}

	// Sort and dispatch. Spec Section 16.2.
	sortForDispatch(tr.Candidates)
	for i := range tr.Candidates {
		if o.availableSlots(cfg) <= 0 {
			break
		}
		if o.shouldDispatch(tr.Candidates[i], cfg) {
			o.dispatchIssue(ctx, tr.Candidates[i], nil, cfg)
		}
	}
}

// dispatchIssue spawns a worker for an issue. Spec Section 16.4.
func (o *Orchestrator) dispatchIssue(ctx context.Context, issue model.Issue, attempt *int, cfg *config.ServiceConfig) {
	now := o.clock.Now()
	retryAttempt := 0
	if attempt != nil {
		retryAttempt = *attempt
	}

	entry := &model.RunningEntry{
		Identifier:   issue.Identifier,
		Issue:        issue,
		RetryAttempt: retryAttempt,
		StartedAt:    now,
	}
	o.running[issue.ID] = entry
	o.claimed[issue.ID] = struct{}{}
	delete(o.retryAttempts, issue.ID)

	o.wg.Add(1)
	go o.runWorker(ctx, issue, attempt, cfg)
}

// handleWorkerExit processes a worker exit. Spec Section 16.6.
func (o *Orchestrator) handleWorkerExit(result WorkerResult) {
	entry, ok := o.running[result.IssueID]
	if ok {
		// Add runtime to totals.
		o.totals.SecondsRunning += result.Duration.Seconds()

		// Add per-session tokens to aggregate.
		o.totals.InputTokens += entry.Session.CodexInputTokens
		o.totals.OutputTokens += entry.Session.CodexOutputTokens
		o.totals.TotalTokens += entry.Session.CodexTotalTokens

		delete(o.running, result.IssueID)
	}

	switch result.ExitReason {
	case model.ExitReasonNormal:
		o.completed[result.IssueID] = struct{}{} // bookkeeping only
		o.scheduleRetry(result.IssueID, result.Identifier, 1, "", true)
	default:
		nextAttempt := 1
		if entry != nil {
			nextAttempt = entry.RetryAttempt + 1
		}
		errMsg := ""
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		o.scheduleRetry(result.IssueID, result.Identifier, nextAttempt, errMsg, false)
	}
}

// handleCodexUpdate processes a codex event from a worker.
func (o *Orchestrator) handleCodexUpdate(update CodexUpdate) {
	entry, ok := o.running[update.IssueID]
	if !ok {
		return
	}

	now := o.clock.Now()
	entry.Session.LastCodexTimestamp = &now
	if string(update.Event.Type) != "" {
		eventStr := string(update.Event.Type)
		entry.Session.LastCodexEvent = &eventStr
	}
	if update.Event.Message != "" {
		entry.Session.LastCodexMessage = update.Event.Message
	}

	// Update token totals using delta-based accounting. Spec Section 13.5.
	if update.Event.TotalTokens > 0 {
		inputDelta := update.Event.InputTokens - entry.Session.LastReportedInputToks
		outputDelta := update.Event.OutputTokens - entry.Session.LastReportedOutputToks

		if inputDelta > 0 {
			entry.Session.CodexInputTokens += inputDelta
			entry.Session.LastReportedInputToks = update.Event.InputTokens
		}
		if outputDelta > 0 {
			entry.Session.CodexOutputTokens += outputDelta
			entry.Session.LastReportedOutputToks = update.Event.OutputTokens
		}
		entry.Session.CodexTotalTokens = entry.Session.CodexInputTokens + entry.Session.CodexOutputTokens
		entry.Session.LastReportedTotalToks = update.Event.TotalTokens
	}

	// Update rate limits.
	if update.Event.RateLimits != nil {
		o.rateLimits = update.Event.RateLimits
	}
}

// handleRetryFired processes a retry timer fire. Spec Section 16.6 on_retry_timer.
func (o *Orchestrator) handleRetryFired(ctx context.Context, rf retryFired) {
	entry, ok := o.retryAttempts[rf.IssueID]
	if !ok {
		return
	}

	// Check generation to detect stale fires.
	if entry.Generation != rf.Generation {
		return
	}

	delete(o.retryAttempts, rf.IssueID)
	delete(o.retryTimerMap, rf.IssueID)

	cfg := o.cfg()

	// Fetch active candidates and find this issue.
	candidates, err := o.tracker.FetchCandidateIssues(ctx, cfg.ActiveStates, cfg.TrackerProjectSlug)
	if err != nil {
		o.scheduleRetry(rf.IssueID, entry.Identifier, entry.Attempt+1, "retry poll failed", false)
		return
	}

	var found *model.Issue
	for i := range candidates {
		if candidates[i].ID == rf.IssueID {
			found = &candidates[i]
			break
		}
	}

	if found == nil {
		// Issue no longer active: release claim.
		delete(o.claimed, rf.IssueID)
		return
	}

	if o.availableSlots(cfg) <= 0 {
		o.scheduleRetry(rf.IssueID, found.Identifier, entry.Attempt+1, "no available orchestrator slots", false)
		return
	}

	o.dispatchIssue(ctx, *found, &entry.Attempt, cfg)
}

// terminateRunning stops a running issue and optionally cleans its workspace.
func (o *Orchestrator) terminateRunning(issueID string, cleanWorkspace bool, cfg *config.ServiceConfig) {
	entry, ok := o.running[issueID]
	if !ok {
		return
	}

	o.logger.Info("terminating running issue",
		"issue_id", issueID,
		"identifier", entry.Identifier,
		"cleanup", cleanWorkspace,
	)

	delete(o.running, issueID)
	delete(o.claimed, issueID)

	if cleanWorkspace {
		workspace.CleanupWorkspace(cfg, entry.Identifier, o.logger)
	}
}

// buildSnapshot creates a point-in-time snapshot of orchestrator state.
func (o *Orchestrator) buildSnapshot() *Snapshot {
	now := o.clock.Now()

	running := make([]RunningSnapshot, 0, len(o.running))
	for _, entry := range o.running {
		rs := RunningSnapshot{
			IssueID:         entry.Issue.ID,
			IssueIdentifier: entry.Identifier,
			State:           entry.Issue.State,
			SessionID:       entry.Session.SessionID,
			TurnCount:       entry.Session.TurnCount,
			LastMessage:     entry.Session.LastCodexMessage,
			StartedAt:       entry.StartedAt,
			LastEventAt:     entry.Session.LastCodexTimestamp,
			Tokens: model.CodexTotals{
				InputTokens:  entry.Session.CodexInputTokens,
				OutputTokens: entry.Session.CodexOutputTokens,
				TotalTokens:  entry.Session.CodexTotalTokens,
			},
		}
		if entry.Session.LastCodexEvent != nil {
			rs.LastEvent = *entry.Session.LastCodexEvent
		}
		running = append(running, rs)
	}

	retrying := make([]RetrySnapshot, 0, len(o.retryAttempts))
	for _, entry := range o.retryAttempts {
		retrying = append(retrying, RetrySnapshot{
			IssueID:         entry.IssueID,
			IssueIdentifier: entry.Identifier,
			Attempt:         entry.Attempt,
			DueAt:           time.UnixMilli(entry.DueAtMs),
			Error:           entry.Error,
		})
	}

	// Compute live totals: cumulative + active elapsed.
	totals := o.totals
	for _, entry := range o.running {
		totals.SecondsRunning += now.Sub(entry.StartedAt).Seconds()
	}

	return &Snapshot{
		GeneratedAt: now,
		Running:     running,
		Retrying:    retrying,
		Totals:      totals,
		RateLimits:  o.rateLimits,
	}
}
