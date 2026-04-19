package prdozer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/wt"
)

// Watcher polls a single PR and reacts to changes by invoking the polish agent.
type Watcher struct {
	gh       wt.GHRunner
	polish   PolishRunner
	cfg      *Config
	renderer *render.Renderer
	logger   *slog.Logger
	repo     string
	workDir  string
	self     string
	pr       int
	dryRun   bool
}

// WatcherOption configures a new Watcher.
type WatcherOption func(*Watcher)

// WithRenderer attaches a renderer for terminal output.
func WithRenderer(r *render.Renderer) WatcherOption {
	return func(w *Watcher) { w.renderer = r }
}

// WithSelfLogin tells the watcher which GitHub login to ignore when looking
// for new comments (so prdozer doesn't react to its own comments).
func WithSelfLogin(login string) WatcherOption {
	return func(w *Watcher) { w.self = login }
}

// WithDryRun puts the watcher in observe-only mode — snapshots and changesets
// are computed and logged, but no agent is invoked.
func WithDryRun(dryRun bool) WatcherOption {
	return func(w *Watcher) { w.dryRun = dryRun }
}

// NewWatcher creates a watcher for a single PR.
func NewWatcher(cfg *Config, gh wt.GHRunner, polish PolishRunner, prNumber int, workDir, repo string, logger *slog.Logger, opts ...WatcherOption) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	w := &Watcher{
		cfg:     cfg,
		gh:      gh,
		polish:  polish,
		logger:  logger.With("pr", prNumber),
		pr:      prNumber,
		repo:    repo,
		workDir: workDir,
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Run loops until ctx is cancelled, ticking at cfg.PollInterval.
// If once is true, runs a single tick and exits.
func (w *Watcher) Run(ctx context.Context, once bool) error {
	if once {
		_, err := w.Tick(ctx)
		return err
	}
	// Tick immediately, then on the configured interval.
	if _, err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.logger.Warn("tick failed", "error", err)
	}
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.Warn("tick failed", "error", err)
			}
		}
	}
}

// TickResult summarizes what happened in one polling cycle.
type TickResult struct {
	Snapshot  *Snapshot
	Action    LastAction
	Changeset Changeset
}

// Tick performs a single polling cycle: snapshot → changeset → maybe-polish → save state.
func (w *Watcher) Tick(ctx context.Context) (TickResult, error) {
	statePath := StatePath(w.repo, w.pr)
	state, err := LoadState(statePath)
	if err != nil {
		return TickResult{}, fmt.Errorf("load state: %w", err)
	}
	state.PRNumber = w.pr
	state.Repo = w.repo

	if !state.CooldownUntil.IsZero() && time.Now().Before(state.CooldownUntil) {
		w.logger.Info("in cooldown, skipping tick", "until", state.CooldownUntil)
		w.status("PR #%d in cooldown until %s", w.pr, state.CooldownUntil.Format(time.RFC3339))
		return TickResult{Action: LastActionIdle}, nil
	}

	snapOpts := SnapshotOptions{Self: w.self}
	snap, err := TakeSnapshot(ctx, w.gh, w.workDir, w.pr, snapOpts)
	if err != nil {
		return TickResult{}, fmt.Errorf("snapshot: %w", err)
	}
	cs := ComputeChangeset(state, snap)
	w.logger.Info("snapshot",
		"head", snap.PR.HeadRefOid,
		"base_sha", snap.BaseSHA,
		"rollup", snap.StatusRollup,
		"review", snap.PR.ReviewDecision,
		"comments", len(snap.Comments),
		"failed_runs", len(snap.FailedRunIDs),
		"new_comments", len(cs.NewCommentIDs),
		"new_failed_runs", len(cs.NewFailedRuns),
		"base_moved", cs.BaseMoved,
		"ci_failed", cs.CIFailed,
		"mergeable", cs.Mergeable,
		"pr_closed", cs.PRClosed,
	)

	action := w.decideAndAct(ctx, snap, cs)
	w.recordSnapshot(state, snap, action)
	if err := state.Save(statePath); err != nil {
		w.logger.Warn("failed to save state", "path", statePath, "error", err)
	}
	return TickResult{Snapshot: snap, Changeset: cs, Action: action}, nil
}

func (w *Watcher) decideAndAct(ctx context.Context, snap *Snapshot, cs Changeset) LastAction {
	switch {
	case cs.PRClosed:
		w.status("PR #%d is %s — nothing to do", w.pr, snap.PR.State)
		return LastActionMerged
	case cs.Mergeable && w.cfg.Polish.AutoMerge && !w.dryRun:
		if err := w.merge(ctx); err != nil {
			w.logger.Error("auto-merge failed", "error", err)
			w.status("PR #%d auto-merge failed: %v", w.pr, err)
			return LastActionFailed
		}
		w.status("PR #%d auto-merged", w.pr)
		return LastActionMerged
	case cs.Mergeable:
		w.status("PR #%d is mergeable — idle", w.pr)
		return LastActionIdle
	case !cs.NeedsPolish():
		w.status("PR #%d unchanged — idle", w.pr)
		return LastActionIdle
	}

	if w.dryRun {
		w.status("PR #%d would polish (base_moved=%t ci_failed=%t new_comments=%d) — dry run, skipping",
			w.pr, cs.BaseMoved, cs.CIFailed, len(cs.NewCommentIDs))
		return LastActionDryRun
	}
	if w.polish == nil {
		w.logger.Warn("no polish runner configured; skipping action")
		return LastActionIdle
	}
	w.status("PR #%d polishing (base_moved=%t ci_failed=%t new_comments=%d)",
		w.pr, cs.BaseMoved, cs.CIFailed, len(cs.NewCommentIDs))
	req := PolishRequest{
		PRNumber: w.pr,
		WorkDir:  w.workDir,
		Local:    w.cfg.Polish.Local,
		Cfg:      w.cfg.Polish,
		Model:    w.cfg.Agent.Model,
	}
	if _, err := w.polish.Run(ctx, req); err != nil {
		w.logger.Error("polish failed", "error", err)
		w.status("PR #%d polish failed: %v", w.pr, err)
		return LastActionFailed
	}
	w.status("PR #%d polish completed", w.pr)
	return LastActionPolished
}

func (w *Watcher) merge(ctx context.Context) error {
	res, err := w.gh.Run(ctx, []string{"pr", "merge", strconv.Itoa(w.pr), "--squash", "--delete-branch=false"}, w.workDir)
	if err != nil {
		if res != nil && res.Stderr != "" {
			return fmt.Errorf("%w: %s", err, res.Stderr)
		}
		return err
	}
	return nil
}

// recordSnapshot updates the state to reflect the post-tick world.
func (w *Watcher) recordSnapshot(s *State, snap *Snapshot, action LastAction) {
	s.LastCheckAt = snap.TakenAt
	s.LastSeenHeadSHA = snap.PR.HeadRefOid
	if snap.BaseSHA != "" {
		s.LastSeenBaseSHA = snap.BaseSHA
	}
	commentIDs := make([]string, 0, len(snap.Comments))
	for _, c := range snap.Comments {
		commentIDs = append(commentIDs, c.ID)
	}
	s.MergeSeenComments(commentIDs)
	s.MergeSeenRuns(snap.FailedRunIDs)
	s.LastAction = action
	switch action {
	case LastActionFailed:
		s.ConsecutiveFailures++
		if w.cfg.Backoff.MaxConsecutiveFailures > 0 && s.ConsecutiveFailures >= w.cfg.Backoff.MaxConsecutiveFailures && w.cfg.Backoff.Cooldown > 0 {
			s.CooldownUntil = time.Now().Add(w.cfg.Backoff.Cooldown)
			w.logger.Warn("entering cooldown after repeated failures",
				"failures", s.ConsecutiveFailures,
				"until", s.CooldownUntil,
			)
		}
	case LastActionPolished, LastActionMerged, LastActionIdle, LastActionDryRun:
		s.ConsecutiveFailures = 0
		s.CooldownUntil = time.Time{}
	}
}

func (w *Watcher) status(format string, args ...interface{}) {
	if w.renderer != nil {
		w.renderer.Status(fmt.Sprintf(format, args...))
	}
}
