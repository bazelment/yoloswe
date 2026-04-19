package prdozer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/wt"
)

// Orchestrator drives one Watcher per PR with a concurrency cap.
type Orchestrator struct {
	cfg      *Config
	gh       wt.GHRunner
	polish   PolishRunner
	renderer *render.Renderer
	logger   *slog.Logger
	repo     string
	workDir  string
	self     string
	dryRun   bool
}

// OrchOption configures an Orchestrator.
type OrchOption func(*Orchestrator)

// WithOrchRenderer attaches a renderer used for top-level status messages and
// passed down to each Watcher.
func WithOrchRenderer(r *render.Renderer) OrchOption {
	return func(o *Orchestrator) { o.renderer = r }
}

// WithOrchSelfLogin sets the GitHub login used for self-comment filtering.
func WithOrchSelfLogin(login string) OrchOption {
	return func(o *Orchestrator) { o.self = login }
}

// WithOrchDryRun runs in observe-only mode.
func WithOrchDryRun(dryRun bool) OrchOption {
	return func(o *Orchestrator) { o.dryRun = dryRun }
}

// NewOrchestrator constructs an Orchestrator. repo is the short repo name
// (e.g. "yoloswe") used for state-file paths.
func NewOrchestrator(cfg *Config, gh wt.GHRunner, polish PolishRunner, workDir, repo string, logger *slog.Logger, opts ...OrchOption) *Orchestrator {
	if logger == nil {
		logger = slog.Default()
	}
	o := &Orchestrator{
		cfg:     cfg,
		gh:      gh,
		polish:  polish,
		logger:  logger,
		repo:    repo,
		workDir: workDir,
	}
	for _, op := range opts {
		op(o)
	}
	return o
}

// RunOnce ticks every PR in the source set exactly once, respecting
// MaxConcurrent. Returns the per-PR tick results in PR-number order.
func (o *Orchestrator) RunOnce(ctx context.Context) (map[int]TickResult, error) {
	prs, err := DiscoverPRs(ctx, o.gh, o.workDir, o.cfg.Source)
	if err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}
	o.logger.Info("discovered PRs", "count", len(prs))
	o.status("Discovered %d PR(s)", len(prs))
	return o.tickAll(ctx, prs, true)
}

// Run loops, re-discovering and ticking every cfg.PollInterval, until ctx is done.
func (o *Orchestrator) Run(ctx context.Context) error {
	tick := func() {
		prs, err := DiscoverPRs(ctx, o.gh, o.workDir, o.cfg.Source)
		if err != nil {
			o.logger.Warn("discovery failed", "error", err)
			return
		}
		if _, err := o.tickAll(ctx, prs, false); err != nil && !errors.Is(err, context.Canceled) {
			o.logger.Warn("tickAll failed", "error", err)
		}
	}
	tick()
	ticker := time.NewTicker(o.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			tick()
		}
	}
}

func (o *Orchestrator) tickAll(ctx context.Context, prs []DiscoveredPR, returnResults bool) (map[int]TickResult, error) {
	maxConc := o.cfg.Source.MaxConcurrent
	if maxConc < 1 {
		maxConc = 1
	}
	sem := make(chan struct{}, maxConc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make(map[int]TickResult)

	for _, pr := range prs {
		pr := pr
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			w := o.newWatcher(pr.Number)
			res, err := w.Tick(ctx)
			if err != nil {
				o.logger.Warn("tick failed", "pr", pr.Number, "error", err)
				return
			}
			if returnResults {
				mu.Lock()
				results[pr.Number] = res
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return results, nil
}

func (o *Orchestrator) newWatcher(prNumber int) *Watcher {
	opts := []WatcherOption{
		WithSelfLogin(o.self),
		WithDryRun(o.dryRun),
	}
	if o.renderer != nil {
		opts = append(opts, WithRenderer(o.renderer))
	}
	return NewWatcher(o.cfg, o.gh, o.polish, prNumber, o.workDir, o.repo, o.logger, opts...)
}

func (o *Orchestrator) status(format string, args ...interface{}) {
	if o.renderer != nil {
		o.renderer.Status(fmt.Sprintf(format, args...))
	}
}
