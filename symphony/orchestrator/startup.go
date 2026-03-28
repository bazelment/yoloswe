package orchestrator

import (
	"context"

	"github.com/bazelment/yoloswe/symphony/config"
	"github.com/bazelment/yoloswe/symphony/workspace"
)

// startupCleanup removes workspaces for issues already in terminal states.
// Spec Section 8.6.
func (o *Orchestrator) startupCleanup(ctx context.Context, cfg *config.ServiceConfig) {
	if len(cfg.TerminalStates) == 0 {
		return
	}

	issues, err := o.tracker.FetchIssuesByStates(ctx, cfg.TerminalStates, cfg.TrackerProjectSlug)
	if err != nil {
		o.logger.Warn("startup terminal cleanup fetch failed, continuing", "error", err)
		return
	}

	for i := range issues {
		if err := workspace.CleanupWorkspace(cfg, issues[i].Identifier, o.logger); err != nil {
			o.logger.Warn("startup terminal cleanup failed for issue",
				"identifier", issues[i].Identifier,
				"error", err,
			)
		}
	}

	if len(issues) > 0 {
		o.logger.Info("startup terminal cleanup complete", "cleaned", len(issues))
	}
}
