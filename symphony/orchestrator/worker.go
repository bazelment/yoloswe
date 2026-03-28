package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bazelment/yoloswe/symphony/agent"
	"github.com/bazelment/yoloswe/symphony/config"
	"github.com/bazelment/yoloswe/symphony/model"
	"github.com/bazelment/yoloswe/symphony/prompt"
	"github.com/bazelment/yoloswe/symphony/workspace"
)

// runWorker is the worker goroutine for a single issue. Spec Section 16.5.
func (o *Orchestrator) runWorker(ctx context.Context, issue model.Issue, attempt *int, cfg *config.ServiceConfig) {
	defer o.wg.Done()

	startedAt := o.clock.Now()
	logger := o.logger.With("issue_id", issue.ID, "issue_identifier", issue.Identifier)

	result := WorkerResult{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
	}

	defer func() {
		result.Duration = o.clock.Now().Sub(startedAt)
		o.workerResults <- result
	}()

	// 1. Create/reuse workspace.
	ws, err := workspace.CreateForIssue(cfg, issue.Identifier)
	if err != nil {
		logger.Error("workspace creation failed", "error", err)
		result.ExitReason = model.ExitReasonFailed
		result.Error = fmt.Errorf("workspace error: %w", err)
		return
	}

	// 2. Run before_run hook.
	if cfg.HookBeforeRun != "" {
		if err := workspace.RunHook(cfg.HookBeforeRun, ws.Path, cfg.HookTimeoutMs); err != nil {
			logger.Error("before_run hook failed", "error", err)
			result.ExitReason = model.ExitReasonFailed
			result.Error = fmt.Errorf("before_run hook error: %w", err)
			return
		}
	}

	// 3. Build initial prompt.
	rendered, err := prompt.RenderInitialPrompt(cfg.Workflow.PromptTemplate, issue, attempt)
	if err != nil {
		logger.Error("prompt rendering failed", "error", err)
		runAfterRunHook(cfg, ws.Path, logger)
		result.ExitReason = model.ExitReasonFailed
		result.Error = fmt.Errorf("prompt error: %w", err)
		return
	}

	// 4. Start agent session.
	sessionCfg := agent.SessionConfig{
		Command:           cfg.CodexCommand,
		WorkDir:           ws.Path,
		ApprovalPolicy:    cfg.CodexApprovalPolicy,
		ThreadSandbox:     cfg.CodexThreadSandbox,
		TurnSandboxPolicy: cfg.CodexTurnSandboxPolicy,
		TurnTimeoutMs:     cfg.CodexTurnTimeoutMs,
		ReadTimeoutMs:     cfg.CodexReadTimeoutMs,
		IssueIdentifier:   issue.Identifier,
		IssueTitle:        issue.Title,
	}

	session, err := agent.NewSession(ctx, sessionCfg, logger)
	if err != nil {
		logger.Error("agent session start failed", "error", err)
		runAfterRunHook(cfg, ws.Path, logger)
		result.ExitReason = model.ExitReasonFailed
		result.Error = fmt.Errorf("agent session startup error: %w", err)
		return
	}

	// Event callback sends updates to orchestrator.
	onEvent := func(ev agent.Event) {
		o.codexUpdates <- CodexUpdate{IssueID: issue.ID, Event: ev}
	}

	// 5. Turn loop (up to max_turns). Spec Section 7.1.
	maxTurns := cfg.MaxTurns
	currentPrompt := rendered

	for turnNumber := 1; turnNumber <= maxTurns; turnNumber++ {
		turnResult, err := session.RunTurn(ctx, currentPrompt, onEvent)
		if err != nil || turnResult.Status != agent.TurnCompleted {
			session.Stop()
			runAfterRunHook(cfg, ws.Path, logger)

			switch turnResult.Status {
			case agent.TurnTimedOut:
				result.ExitReason = model.ExitReasonTimedOut
			case agent.TurnCancelled:
				result.ExitReason = model.ExitReasonCanceled
			default:
				result.ExitReason = model.ExitReasonFailed
			}
			result.Error = err
			return
		}

		// Between turns: refresh issue state. Spec Section 7.1.
		refreshed, err := o.tracker.FetchIssueStatesByIDs(ctx, []string{issue.ID})
		if err != nil {
			logger.Error("issue state refresh failed between turns", "error", err)
			session.Stop()
			runAfterRunHook(cfg, ws.Path, logger)
			result.ExitReason = model.ExitReasonFailed
			result.Error = fmt.Errorf("issue state refresh error: %w", err)
			return
		}

		if len(refreshed) > 0 {
			issue = refreshed[0]
		}

		// Check if issue is still active.
		normState := model.NormalizeState(issue.State)
		isActive := false
		for _, s := range cfg.ActiveStates {
			if model.NormalizeState(s) == normState {
				isActive = true
				break
			}
		}
		if !isActive {
			break
		}

		if turnNumber >= maxTurns {
			break
		}

		// Continuation prompt for subsequent turns.
		currentPrompt = prompt.RenderContinuationPrompt(issue, turnNumber+1)
	}

	// Normal exit.
	session.Stop()
	runAfterRunHook(cfg, ws.Path, logger)
	result.ExitReason = model.ExitReasonNormal
}

func runAfterRunHook(cfg *config.ServiceConfig, workDir string, logger *slog.Logger) {
	if cfg.HookAfterRun != "" {
		workspace.RunHookBestEffort(cfg.HookAfterRun, workDir, cfg.HookTimeoutMs, logger)
	}
}
