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

	return &teamSupervisor{
		app:       app,
		cfg:       cfg,
		args:      args,
		logger:    app.Logger,
		disc:      disc,
		orch:      orch,
		selfPath:  selfPath,
		absConfig: absConfig,
		logDir:    logDir,
	}, nil
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
	if oldCfg.Tracker.Kind == "github" && oldCfg.Source.Filters[tracker.FilterTeam] != newCfg.Source.Filters[tracker.FilterTeam] {
		return fmt.Errorf("github repository filter change is not supported during reload")
	}
	return nil
}

func (s *teamSupervisor) execRestart() error {
	statePath := filepath.Join(s.logDir, "team-state", fmt.Sprintf("%d.json", os.Getpid()))
	state := jiradozer.RuntimeState{
		ActiveWorkflow: s.orch.ActiveWorkflowSnapshots(),
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
	s.orch.RestoreActive(state.ActiveWorkflow)
	for _, snap := range state.ActiveWorkflow {
		if snap.Issue != nil && snap.Issue.ID != "" {
			s.disc.MarkSeen(snap.Issue.ID)
		}
	}
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
