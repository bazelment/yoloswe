package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/symphony/agent"
	"github.com/bazelment/yoloswe/symphony/config"
	"github.com/bazelment/yoloswe/symphony/model"
	"github.com/bazelment/yoloswe/symphony/tracker"
)

// WorkerResult is sent from a worker goroutine to the orchestrator on exit.
type WorkerResult struct {
	Error      error
	IssueID    string
	Identifier string
	ExitReason model.ExitReason
	Duration   time.Duration
}

// CodexUpdate is sent from a worker goroutine to the orchestrator on agent events.
type CodexUpdate struct {
	IssueID string
	Event   agent.Event
}

// retryFired is sent when a retry timer fires. Includes generation to detect stale fires.
type retryFired struct {
	IssueID    string
	Generation uint64
}

// tickResult holds the async results of a poll tick (tracker API calls).
type tickResult struct {
	Err              error
	ReconcileActions []reconcileAction
	Candidates       []model.Issue
}

// reconcileAction describes what to do with a running issue after state refresh.
type reconcileAction struct {
	Issue            *model.Issue
	IssueID          string
	Action           reconcileActionType
	CleanupWorkspace bool
}

type reconcileActionType string

const (
	reconcileUpdate    reconcileActionType = "update"
	reconcileTerminate reconcileActionType = "terminate"
	reconcileStalled   reconcileActionType = "stalled"
)

// Snapshot is a point-in-time view of orchestrator state for the HTTP API.
type Snapshot struct {
	GeneratedAt time.Time
	Running     []RunningSnapshot
	Retrying    []RetrySnapshot
	RateLimits  json.RawMessage
	Totals      model.CodexTotals
}

// RunningSnapshot is one running entry for the snapshot.
type RunningSnapshot struct {
	StartedAt       time.Time
	LastEventAt     *time.Time
	IssueID         string
	IssueIdentifier string
	State           string
	SessionID       string
	LastEvent       string
	LastMessage     string
	Tokens          model.CodexTotals
	TurnCount       int
}

// RetrySnapshot is one retry entry for the snapshot.
type RetrySnapshot struct {
	DueAt           time.Time
	IssueID         string
	IssueIdentifier string
	Error           string
	Attempt         int
}

// Orchestrator is the single-authority event loop.
type Orchestrator struct {
	clock         Clock
	tracker       tracker.Tracker
	running       map[string]*model.RunningEntry
	claimed       map[string]struct{}
	workerResults chan WorkerResult
	codexUpdates  chan CodexUpdate
	retryTimers   chan retryFired
	tickResults   chan tickResult
	snapshotReqs  chan chan *Snapshot
	refreshReqs   chan chan struct{}
	cfg           func() *config.ServiceConfig
	logger        *slog.Logger
	retryAttempts map[string]*model.RetryEntry
	completed     map[string]struct{}
	retryTimerMap map[string]Timer
	cancel        context.CancelFunc
	rateLimits    json.RawMessage
	totals        model.CodexTotals
	wg            sync.WaitGroup
	nextGen       uint64
}

// New creates a new Orchestrator.
func New(cfgFn func() *config.ServiceConfig, t tracker.Tracker, clock Clock, logger *slog.Logger) *Orchestrator {
	return &Orchestrator{
		cfg:           cfgFn,
		tracker:       t,
		clock:         clock,
		logger:        logger,
		workerResults: make(chan WorkerResult, 64),
		codexUpdates:  make(chan CodexUpdate, 256),
		retryTimers:   make(chan retryFired, 64),
		tickResults:   make(chan tickResult, 1),
		snapshotReqs:  make(chan chan *Snapshot, 8),
		refreshReqs:   make(chan chan struct{}, 8),
		running:       make(map[string]*model.RunningEntry),
		claimed:       make(map[string]struct{}),
		retryAttempts: make(map[string]*model.RetryEntry),
		completed:     make(map[string]struct{}),
		retryTimerMap: make(map[string]Timer),
	}
}

// Run starts the orchestrator event loop. Blocks until ctx is cancelled.
// Spec Section 16.1.
func (o *Orchestrator) Run(ctx context.Context) error {
	ctx, o.cancel = context.WithCancel(ctx)

	cfg := o.cfg()

	// Startup validation. Spec Section 6.3.
	if err := config.ValidateForDispatch(cfg); err != nil {
		return err
	}

	// Startup terminal workspace cleanup. Spec Section 8.6.
	o.startupCleanup(ctx, cfg)

	// Schedule immediate first tick.
	ticker := o.clock.NewTicker(time.Duration(cfg.PollIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	// Trigger immediate first tick.
	o.startAsyncTick(ctx)

	tickInFlight := true

	for {
		select {
		case <-ticker.C():
			if !tickInFlight {
				o.startAsyncTick(ctx)
				tickInFlight = true
			}

		case tr := <-o.tickResults:
			tickInFlight = false
			o.handleTickResult(ctx, tr)

			// Update ticker interval if config changed.
			newCfg := o.cfg()
			ticker.Stop()
			ticker = o.clock.NewTicker(time.Duration(newCfg.PollIntervalMs) * time.Millisecond)

		case result := <-o.workerResults:
			o.handleWorkerExit(result)

		case update := <-o.codexUpdates:
			o.handleCodexUpdate(update)

		case rf := <-o.retryTimers:
			o.handleRetryFired(ctx, rf)

		case req := <-o.snapshotReqs:
			req <- o.buildSnapshot()

		case req := <-o.refreshReqs:
			if !tickInFlight {
				o.startAsyncTick(ctx)
				tickInFlight = true
			}
			close(req)

		case <-ctx.Done():
			o.shutdown()
			return nil
		}
	}
}

// RequestSnapshot returns a snapshot of the current orchestrator state.
// Thread-safe: communicates with the event loop via channel.
func (o *Orchestrator) RequestSnapshot(ctx context.Context) (*Snapshot, error) {
	ch := make(chan *Snapshot, 1)
	select {
	case o.snapshotReqs <- ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case snap := <-ch:
		return snap, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// RequestRefresh triggers an immediate poll+reconciliation cycle.
func (o *Orchestrator) RequestRefresh(ctx context.Context) error {
	ch := make(chan struct{}, 1)
	select {
	case o.refreshReqs <- ch:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shutdown cancels all workers and waits for them to finish (30s deadline).
func (o *Orchestrator) shutdown() {
	o.logger.Info("shutting down orchestrator")
	o.cancel()

	done := make(chan struct{})
	go func() {
		o.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		o.logger.Info("all workers stopped")
	case <-time.After(30 * time.Second):
		o.logger.Warn("shutdown deadline exceeded, some workers may be orphaned")
	}
}
