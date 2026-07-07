package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/wt"
)

const restoreStateEnv = "JIRADOZER_RESTORE_STATE"

//nolint:govet // fieldalignment: keep runtime dependencies and mutable state grouped.
type teamSupervisor struct {
	app       *cliapp.App
	cfg       *jiradozer.Config
	args      runArgs
	logger    *slog.Logger
	disc      *jiradozer.Discovery
	orch      *jiradozer.Orchestrator
	selfPath  string
	absConfig string
	logDir    string
	reloadMu  sync.Mutex
}

func newTeamSupervisor(app *cliapp.App, issueTracker tracker.IssueTracker, cfg *jiradozer.Config, args runArgs) (*teamSupervisor, error) {
	repoName := resolveRepoName(cfg)
	selfPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve jiradozer binary path: %w", err)
	}
	absConfig, err := filepath.Abs(args.configPath)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	childArgs := buildChildArgs(app, args, absConfig)
	logDir, err := cliapp.LogDir("jiradozer")
	if err != nil {
		app.Logger.Warn("could not create log dir, child logs go to temp dir", "error", err)
		logDir = os.TempDir()
	}

	var wtMgr jiradozer.WorktreeManager
	if cfg.Source.DryRun {
		wtMgr = &wtAdapter{mgr: wt.NewManager(".", repoName)}
	} else {
		mgr, err := resolveWTManager()
		if err != nil {
			return nil, fmt.Errorf("team mode requires a wt-managed repository: %w", err)
		}
		app.Logger.Info("resolved wt-managed repository", "repo_dir", mgr.RepoDir())
		wtMgr = &wtAdapter{mgr: mgr}
	}

	disc := jiradozer.NewDiscovery(issueTracker, cfg.Source.ToFilter(), cfg.PollInterval, app.Logger)
	orch := jiradozer.NewOrchestrator(issueTracker, cfg, wtMgr, repoName, app.Logger)
	orch.SetSubprocessMode(selfPath, childArgs, logDir)
	orch.SetForceCleanup(args.forceCleanup)
	// Discovery reconciles its seen set against the orchestrator's runtime state
	// on every poll: the active set protects the claim window and pins
	// runtime-failure-capped issues, while the released set un-suppresses
	// failed/cancelled issues so a re-queued issue is re-emitted without any exit
	// path clearing seen. Restored children (in the active set) are suppressed
	// without a manual MarkSeen re-seed.
	disc.SetReconcileProviders(orch.ActiveIssueIDs, orch.DrainReleasedForRetry)

	s := &teamSupervisor{
		app:       app,
		cfg:       cfg,
		args:      args,
		logger:    app.Logger,
		disc:      disc,
		orch:      orch,
		selfPath:  selfPath,
		absConfig: absConfig,
		logDir:    logDir,
	}
	// Per-issue subprocess failures are reported by the orchestrator (the
	// single reporter; children suppress their own alert). Applied here and
	// re-applied on reload so a live notify.slack_webhook change takes effect.
	s.applyFailureReporting()
	return s, nil
}

// applyFailureReporting (re)configures the orchestrator's per-issue failure
// sinks from the current config. The tracker-comment sink uses the orchestrator's
// tracker; the optional Slack sink mirrors single-issue mode. Called at startup
// and on every config reload so a changed webhook propagates without a restart.
func (s *teamSupervisor) applyFailureReporting() {
	var notifier jiradozer.Notifier
	if s.cfg.Notify.SlackWebhook != "" {
		notifier = jiradozer.SlackWebhookNotifier{WebhookURL: s.cfg.Notify.SlackWebhook}
	}
	// Build revision is optional (FailureReport omits an empty one). Guard
	// against a nil app so the supervisor is usable in tests that construct it
	// directly without a cliapp.App.
	var buildRevision string
	if s.app != nil {
		buildRevision = s.app.Build.ShortRevision()
	}
	s.orch.SetFailureReporting(notifier, buildRevision)
}

func (s *teamSupervisor) Run(ctx context.Context) error {
	orchCtx, orchCancel := context.WithCancel(ctx)
	defer orchCancel()
	go s.printStatusUpdates()
	if err := s.restoreFromEnv(); err != nil {
		orchCancel()
		s.orch.Shutdown()
		return err
	}

	sigStop := watchTeamSignals(s.logger, func(sig teamSignal) {
		switch sig {
		case teamSignalReload:
			go s.reload()
		case teamSignalRestart:
			if err := s.execRestart(); err != nil {
				s.logger.Error("exec restart failed", "error", err)
			}
		}
	})
	defer sigStop()

	err := s.orch.RunWithDiscovery(orchCtx, s.disc)
	orchCancel()
	s.orch.Shutdown()
	s.reportPreservedWorktrees()
	return err
}

func (s *teamSupervisor) reload() {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	next, err := loadRunConfig(s.args)
	if err != nil {
		s.logger.Error("config reload failed, keeping last-known-good config", "error", err)
		return
	}
	if err := validateReloadCompatible(s.cfg, next); err != nil {
		s.logger.Error("config reload rejected, keeping last-known-good config", "error", err)
		return
	}

	s.cfg = next
	s.orch.UpdateConfig(next)
	// Re-apply failure-reporting sinks so a changed notify.slack_webhook takes
	// effect on the next subprocess failure instead of waiting for a restart.
	s.applyFailureReporting()
	s.disc.Update(next.Source.ToFilter(), next.PollInterval)
	s.logger.Info("config reloaded",
		"path", s.absConfig,
		"poll_interval", next.PollInterval,
		"max_concurrent", next.Source.MaxConcurrent,
	)
}

func validateReloadCompatible(oldCfg, newCfg *jiradozer.Config) error {
	if oldCfg.Tracker.Kind != newCfg.Tracker.Kind {
		return fmt.Errorf("tracker kind change is not supported during reload: %q -> %q", oldCfg.Tracker.Kind, newCfg.Tracker.Kind)
	}
	if !reflect.DeepEqual(oldCfg.Tracker, newCfg.Tracker) {
		return fmt.Errorf("tracker config changes are not supported during reload")
	}
	if oldCfg.Source.HasSource() != newCfg.Source.HasSource() {
		return fmt.Errorf("source mode change is not supported during reload")
	}
	if oldCfg.Source.DryRun != newCfg.Source.DryRun {
		return fmt.Errorf("source dry-run change is not supported during reload")
	}
	if oldCfg.WorkDir != newCfg.WorkDir {
		return fmt.Errorf("work_dir change is not supported during reload: %q -> %q", oldCfg.WorkDir, newCfg.WorkDir)
	}
	if resolveRepoName(oldCfg) != resolveRepoName(newCfg) {
		return fmt.Errorf("source team/repository filter change is not supported during reload")
	}
	return nil
}

func (s *teamSupervisor) execRestart() error {
	statePath := filepath.Join(s.logDir, "team-state", fmt.Sprintf("%d.json", os.Getpid()))
	state := jiradozer.RuntimeState{
		ActiveWorkflow:    s.orch.ActiveWorkflowSnapshots(),
		PreservedWorktree: s.orch.PreservedWorktrees(),
	}
	if err := jiradozer.WriteRuntimeStateAtomically(statePath, state); err != nil {
		return err
	}
	s.logger.Info("exec restarting team-mode parent",
		"state", statePath,
		"active_children", len(state.ActiveWorkflow),
	)
	env := append(os.Environ(), restoreStateEnv+"="+statePath)
	return execRestart(s.selfPath, os.Args, env)
}

func (s *teamSupervisor) restoreFromEnv() error {
	statePath := os.Getenv(restoreStateEnv)
	if statePath == "" {
		return nil
	}
	if err := os.Unsetenv(restoreStateEnv); err != nil {
		s.logger.Warn("failed to unset restore state env", "env", restoreStateEnv, "error", err)
	}
	state, err := jiradozer.LoadRuntimeState(statePath)
	if err != nil {
		return fmt.Errorf("restore runtime state: %w", err)
	}
	// RestoreActive re-adds the restored children to the orchestrator's active
	// set; discovery's per-poll reconciliation (SetReconcileProviders) then keeps
	// them suppressed automatically, so no MarkSeen re-seed is needed here.
	s.orch.RestoreActive(state.ActiveWorkflow)
	s.orch.RestorePreservedWorktrees(state.PreservedWorktree)
	s.logger.Info("restored team-mode runtime state",
		"state", statePath,
		"active_children", len(state.ActiveWorkflow),
	)
	return nil
}

func (s *teamSupervisor) printStatusUpdates() {
	renderer := s.app.Renderer
	for status := range s.orch.StatusUpdates() {
		switch {
		case status.Step == jiradozer.StepInit:
			renderer.Status(fmt.Sprintf("[%s] started - %s", status.Issue.Identifier, status.Issue.Title))
		case status.Step == jiradozer.StepDone:
			elapsed := time.Since(status.StartedAt).Truncate(time.Second)
			renderer.Status(fmt.Sprintf("[%s] completed (%s)", status.Issue.Identifier, elapsed))
		case status.Step == jiradozer.StepCancelled:
			renderer.Status(fmt.Sprintf("[%s] cancelled", status.Issue.Identifier))
		case status.Step == jiradozer.StepFailed:
			renderer.Error(status.Error, fmt.Sprintf("[%s] failed", status.Issue.Identifier))
		}
	}
}

func (s *teamSupervisor) reportPreservedWorktrees() {
	preserved := s.orch.PreservedWorktrees()
	if len(preserved) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\nPreserved %d worktree(s):\n", len(preserved))
	for _, pw := range preserved {
		reason := "cancelled"
		switch pw.Step {
		case jiradozer.StepDone:
			reason = "shipped (PR open, not yet merged)"
		case jiradozer.StepFailed:
			reason = "failed (inspect and recover; pushed branch / open PR still intact)"
		}
		fmt.Fprintf(os.Stderr, "  %s  %s  (%s)  [%s]\n", pw.Issue, pw.Branch, pw.WorktreePath, reason)
	}
	fmt.Fprintf(os.Stderr, "\nTo remove after merging: wt remove <branch>\n")
	fmt.Fprintf(os.Stderr, "To remove failed/cancelled worktrees on next run: re-run with --force-cleanup\n")
}
