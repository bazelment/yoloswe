package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/controller"
	"github.com/bazelment/yoloswe/orch/dispatcher"
	"github.com/bazelment/yoloswe/orch/lane"
	"github.com/bazelment/yoloswe/orch/model"
	"github.com/bazelment/yoloswe/orch/recipe"
	"github.com/bazelment/yoloswe/orch/store"
)

// FakePhaseBehavior scripts the outcome of a single (laneID, phase, round, attempt) execution
// on ScriptedFakeDriver.
type FakePhaseBehavior struct {
	LaneID   string             `json:"lane_id,omitempty"`
	Phase    string             `json:"phase,omitempty"`
	Outcome  lane.ReportOutcome `json:"outcome,omitempty"`
	HeadSHA  string             `json:"head_sha,omitempty"`
	Checks   string             `json:"checks,omitempty"`
	Reason   string             `json:"reason,omitempty"`
	Breaks   string             `json:"breaks,omitempty"`
	Summary  string             `json:"summary,omitempty"`
	Round    int                `json:"round,omitempty"`
	Attempt  int                `json:"attempt,omitempty"`
	PR       int                `json:"pr,omitempty"`
	ExitCode int                `json:"exit_code,omitempty"`
	Stall    bool               `json:"stall,omitempty"`
	NoReport bool               `json:"no_report,omitempty"`
}

// FakeScenario is the JSON structure for scripting FakeDriver behavior from a file.
type FakeScenario struct {
	Behaviors []FakePhaseBehavior `json:"behaviors,omitempty"`
}

// ScriptedFakeDriver wraps lane.FakeDriver to emit scripted (or automatic happy-path)
// phase reports and CI events into store.EventStore when terminals start.
type ScriptedFakeDriver struct {
	*lane.FakeDriver
	events       store.EventStore
	clock        clock.Clock
	lanePRs      map[string]int
	behaviors    []FakePhaseBehavior
	nextPR       int
	autoComplete bool
	mu           sync.Mutex
}

var _ lane.LaneDriver = (*ScriptedFakeDriver)(nil)

// NewScriptedFakeDriver creates a ScriptedFakeDriver backed by inner and events.
func NewScriptedFakeDriver(
	inner *lane.FakeDriver,
	events store.EventStore,
	clk clock.Clock,
	autoComplete bool,
	behaviors ...FakePhaseBehavior,
) *ScriptedFakeDriver {
	if inner == nil {
		inner = lane.NewFakeDriver()
	}
	if clk == nil {
		clk = clock.NewRealClock()
	}
	return &ScriptedFakeDriver{
		FakeDriver:   inner,
		events:       events,
		clock:        clk,
		lanePRs:      make(map[string]int),
		behaviors:    slices.Clone(behaviors),
		nextPR:       100,
		autoComplete: autoComplete,
	}
}

// LoadFakeScenario loads a FakeScenario JSON file.
func LoadFakeScenario(path string) (FakeScenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FakeScenario{}, fmt.Errorf("read fake scenario %q: %w", path, err)
	}
	var sc FakeScenario
	if err := json.Unmarshal(data, &sc); err != nil {
		return FakeScenario{}, fmt.Errorf("parse fake scenario %q: %w", path, err)
	}
	return sc, nil
}

// AddBehavior registers a scripted phase behavior.
func (d *ScriptedFakeDriver) AddBehavior(b FakePhaseBehavior) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.behaviors = append(d.behaviors, b)
}

// SpawnSession records the spawn in FakeDriver and triggers any scripted/auto phase completion.
func (d *ScriptedFakeDriver) SpawnSession(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	res, err := d.FakeDriver.SpawnSession(ctx, req)
	if err != nil {
		return res, err
	}
	if req.Phase != "" && res.TerminalID != "" {
		if err := d.onTerminalStarted(ctx, req.RunID, req.LaneID, req.Phase, max(req.Round, 1), res.TerminalID); err != nil {
			return res, err
		}
	}
	return res, nil
}

// StartPhaseTerminal records the phase start in FakeDriver and triggers any scripted/auto phase completion.
func (d *ScriptedFakeDriver) StartPhaseTerminal(ctx context.Context, req lane.StartPhaseRequest) (lane.StartPhaseResult, error) {
	res, err := d.FakeDriver.StartPhaseTerminal(ctx, req)
	if err != nil {
		return res, err
	}
	if req.Phase != "" && res.TerminalID != "" {
		if err := d.onTerminalStarted(ctx, req.RunID, req.LaneID, req.Phase, max(req.Round, 1), res.TerminalID); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (d *ScriptedFakeDriver) onTerminalStarted(
	ctx context.Context,
	runID, laneID, phase string,
	round int,
	terminalID string,
) error {
	if d.events == nil {
		return nil
	}

	d.mu.Lock()
	prNum, hasPR := d.lanePRs[laneID]
	if !hasPR {
		d.nextPR++
		prNum = d.nextPR
		d.lanePRs[laneID] = prNum
	}
	behavior, matched := d.popMatchingBehaviorLocked(laneID, phase, round)
	auto := d.autoComplete
	d.mu.Unlock()

	if !matched && !auto {
		return nil
	}
	if !matched {
		behavior = FakePhaseBehavior{
			LaneID:  laneID,
			Phase:   phase,
			Round:   round,
			Outcome: lane.OutcomeDone,
			HeadSHA: fmt.Sprintf("sha-%s", laneID),
			Checks:  "green",
			PR:      prNum,
			Summary: fmt.Sprintf("Completed %s round %d for %s", phase, round, laneID),
		}
	}

	if behavior.PR <= 0 {
		behavior.PR = prNum
	}
	if behavior.HeadSHA == "" {
		behavior.HeadSHA = fmt.Sprintf("sha-%s", laneID)
	}
	if behavior.Outcome == "" {
		behavior.Outcome = lane.OutcomeDone
	}
	if behavior.Checks == "" {
		behavior.Checks = "green"
	}

	if behavior.Stall {
		_, err := d.events.Append(ctx, store.Event{
			RunID:   runID,
			Kind:    store.EventStall,
			Class:   store.EventClassJudgment,
			LaneID:  laneID,
			Source:  controller.ActorController,
			Summary: cmp.Or(behavior.Reason, fmt.Sprintf("lane %s stalled in phase %s (round %d)", laneID, phase, round)),
			Urgent:  true,
		})
		return err
	}

	if behavior.NoReport {
		exitPayload, _ := json.Marshal(controller.PhaseExitPayload{
			TerminalID: terminalID,
			Phase:      phase,
			Round:      round,
			ExitCode:   behavior.ExitCode,
			Error:      behavior.Reason,
		})
		_, err := d.events.Append(ctx, store.Event{
			RunID:   runID,
			Kind:    store.EventPhaseExit,
			LaneID:  laneID,
			Summary: fmt.Sprintf("phase %s terminal %s exited without report", phase, terminalID),
			Payload: exitPayload,
		})
		return err
	}

	rep := lane.PhaseReport{
		ReportedAt: d.clock.Now().UTC(),
		LaneID:     laneID,
		TerminalID: terminalID,
		Phase:      phase,
		Round:      round,
		Outcome:    behavior.Outcome,
		HeadSHA:    behavior.HeadSHA,
		PR:         behavior.PR,
		Reason:     behavior.Reason,
		Breaks:     behavior.Breaks,
		Summary:    cmp.Or(behavior.Summary, fmt.Sprintf("%s round %d -> %s", phase, round, behavior.Outcome)),
	}
	d.FakeDriver.SetReport(terminalID, rep)

	ciGreen := strings.EqualFold(behavior.Checks, "green") || strings.EqualFold(behavior.Checks, "pass")
	repPayload, err := json.Marshal(controller.LaneReportPayload{
		CIGreen:     &ciGreen,
		Head:        rep.HeadSHA,
		Checks:      behavior.Checks,
		PhaseReport: rep,
	})
	if err != nil {
		return err
	}

	_, err = d.events.Append(ctx, store.Event{
		RunID:   runID,
		Kind:    store.EventLaneReport,
		LaneID:  laneID,
		Summary: fmt.Sprintf("lane %s phase %s round %d outcome=%s", laneID, phase, round, rep.Outcome),
		Payload: repPayload,
	})
	return err
}

func (d *ScriptedFakeDriver) popMatchingBehaviorLocked(laneID, phase string, round int) (FakePhaseBehavior, bool) {
	for i := range d.behaviors {
		b := d.behaviors[i]
		if b.LaneID != "" && b.LaneID != laneID {
			continue
		}
		if b.Phase != "" && b.Phase != phase {
			continue
		}
		if b.Round > 0 && b.Round != round {
			continue
		}
		d.behaviors = slices.Delete(d.behaviors, i, i+1)
		return b, true
	}
	return FakePhaseBehavior{}, false
}

// Runner coordinates the Controller, Dispatcher, and Planner in a single process
// with run lease ownership, structured logging, metrics persistence, and graceful shutdown.
type Runner struct {
	store             store.Store
	driver            lane.LaneDriver
	clock             clock.Clock
	client            model.ModelClient
	controller        *controller.Controller
	dispatcher        *dispatcher.Dispatcher
	planner           *Planner
	logger            *slog.Logger
	dispatchedOpenSet map[string]bool
	cfg               Config
	lastLoggedSeq     int64
	lastMetricsSeq    int64
	mu                sync.Mutex
}

// NewRunner wires a Runner from cfg, st, drv, clk, client, and logger.
func NewRunner(
	cfg Config,
	st store.Store,
	drv lane.LaneDriver,
	clk clock.Clock,
	client model.ModelClient,
	logger *slog.Logger,
) *Runner {
	cfg = cfg.withDefaults()
	if clk == nil {
		clk = clock.NewRealClock()
	}
	if logger == nil {
		logger = slog.Default()
	}

	planner := NewPlanner(st, clk, client, cfg.Models.Planner, logger)

	disp := dispatcher.New(dispatcher.Config{
		Model:            cfg.Models.Dispatcher,
		HardBatchModel:   cfg.Models.DispatcherHard,
		RecipeCompiler:   planner,
		OnPlannerRequest: planner.HandlePlannerRequest,
	}, st, drv, clk, client)

	r := &Runner{
		cfg:               cfg,
		store:             st,
		driver:            drv,
		clock:             clk,
		client:            client,
		planner:           planner,
		dispatcher:        disp,
		logger:            logger,
		dispatchedOpenSet: make(map[string]bool),
	}

	ctrl := controller.New(controller.Config{
		RunID:             cfg.RunID,
		HolderID:          cfg.HolderID,
		LeaseTTL:          cfg.LeaseTTL,
		HeartbeatInterval: cfg.HeartbeatInterval,
		PollInterval:      cfg.PollInterval,
		JudgmentDebounce:  cfg.JudgmentDebounce,
		StallTimeout:      cfg.StallTimeout,
	}, st, drv, clk, controller.JudgeFunc(r.judgeBatch))

	r.controller = ctrl
	return r
}

// Controller returns the underlying deterministic Controller.
func (r *Runner) Controller() *controller.Controller {
	return r.controller
}

// Dispatcher returns the underlying Dispatcher.
func (r *Runner) Dispatcher() *dispatcher.Dispatcher {
	return r.dispatcher
}

// Planner returns the underlying Planner.
func (r *Runner) Planner() *Planner {
	return r.planner
}

func (r *Runner) judgeBatch(ctx context.Context, runID string, events []store.Event) error {
	r.logger.Info("dispatching judgment batch",
		"run_id", runID,
		"event_count", len(events),
		"model", r.cfg.Models.Dispatcher,
	)
	if r.client == nil {
		return nil
	}
	res, err := r.dispatcher.Dispatch(ctx, runID, events)
	if err != nil {
		r.logger.Error("dispatcher batch failed", "run_id", runID, "error", err)
		return err
	}
	r.logger.Info("dispatcher batch completed",
		"run_id", runID,
		"turn_id", res.TurnID,
		"model", res.Model,
		"turns", res.TurnCount,
		"accepted", len(res.Accepted),
		"rejected", len(res.Rejected),
		"input_tokens", res.Usage.InputTokens,
		"output_tokens", res.Usage.OutputTokens,
		"cost_usd", res.Usage.CostUSD,
	)
	return nil
}

// Bootstrap initializes the Run record, default recipe (with concurrency cap),
// and any configured recipe files in Store.
func (r *Runner) Bootstrap(ctx context.Context) error {
	runID := r.cfg.RunID
	existing, err := r.store.Runs().Get(ctx, runID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := r.store.Runs().Create(ctx, store.Run{
			ID:            runID,
			Objective:     cmp.Or(strings.TrimSpace(r.cfg.Objective), "Orchestrate swarm run "+runID),
			PlanRef:       strings.TrimSpace(r.cfg.PlanRef),
			DefaultRecipe: recipe.DefaultName,
		}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return err
		}
	} else if (r.cfg.Objective != "" && existing.Objective != r.cfg.Objective) || (r.cfg.PlanRef != "" && existing.PlanRef != r.cfg.PlanRef) {
		obj := cmp.Or(strings.TrimSpace(r.cfg.Objective), existing.Objective)
		ref := cmp.Or(strings.TrimSpace(r.cfg.PlanRef), existing.PlanRef)
		if err := r.store.Runs().UpdateObjective(ctx, runID, obj, ref); err != nil {
			return err
		}
	}

	for _, path := range r.cfg.RecipeFiles {
		rec, err := LoadRecipeFile(ctx, path, runID, r.cfg.ConcurrencyCap, r.store, r.planner)
		if err != nil {
			return err
		}
		r.logger.Info("loaded recipe file",
			"run_id", runID,
			"path", path,
			"recipe", rec.Name,
			"version", rec.Version,
		)
	}

	// Ensure default recipe is persisted with configured concurrency cap.
	if existingRec, err := r.store.Recipes().Get(ctx, runID, recipe.DefaultName, 0); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			def := recipe.Default()
			def.RunID = runID
			def.Version = 0
			if r.cfg.ConcurrencyCap > 0 {
				def.Limits.MaxConcurrentLanes = r.cfg.ConcurrencyCap
			}
			if _, err := r.store.Recipes().Save(ctx, def); err != nil {
				return err
			}
		} else {
			return err
		}
	} else if r.cfg.ConcurrencyCap > 0 && existingRec.Limits.MaxConcurrentLanes != r.cfg.ConcurrencyCap {
		updated := existingRec.Clone()
		updated.Version = 0
		updated.Limits.MaxConcurrentLanes = r.cfg.ConcurrencyCap
		if _, err := r.store.Recipes().Save(ctx, updated); err != nil {
			return err
		}
	}
	return nil
}

// Step executes one orchestration pass (Planner directives, open-backlog wakeup,
// Controller step, structured decision logging, and metrics recording when settled).
func (r *Runner) Step(ctx context.Context) error {
	if err := r.planner.ReconcileDirectives(ctx, r.cfg.RunID, r.cfg.ConcurrencyCap); err != nil {
		return err
	}

	if err := r.enqueueOpenBacklogTriggerIfNeeded(ctx); err != nil {
		return err
	}

	if err := r.controller.Step(ctx); err != nil {
		return err
	}

	return r.logNewDecisions(ctx)
}

func (r *Runner) enqueueOpenBacklogTriggerIfNeeded(ctx context.Context) error {
	if r.client == nil {
		return nil
	}

	cur, err := r.store.Events().GetCursor(ctx, r.cfg.RunID, controller.DefaultConsumer)
	if err != nil {
		return err
	}
	unprocessed, err := r.store.Events().List(ctx, store.EventFilter{
		RunID:    r.cfg.RunID,
		AfterSeq: cur.Seq,
	})
	if err != nil {
		return err
	}
	if len(unprocessed) > 0 {
		return nil
	}

	lanes, err := r.store.Lanes().List(ctx, store.LaneFilter{RunID: r.cfg.RunID})
	if err != nil {
		return err
	}
	activeOrPlanned := 0
	lanesByItem := make(map[string]bool, len(lanes))
	for i := range lanes {
		ln := lanes[i]
		if ln.ItemID != "" {
			lanesByItem[ln.ItemID] = true
		}
		if ln.Status == lane.StatusRunning || ln.Status == lane.StatusPlanned {
			activeOrPlanned++
		}
	}
	if r.cfg.ConcurrencyCap > 0 && activeOrPlanned >= r.cfg.ConcurrencyCap {
		return nil
	}

	backlog, err := r.store.Backlog().List(ctx, store.BacklogFilter{RunID: r.cfg.RunID})
	if err != nil {
		return err
	}
	statusByID := make(map[string]store.BacklogStatus, len(backlog))
	for i := range backlog {
		statusByID[backlog[i].ID] = backlog[i].Status
	}

	var readyIDs []string
	for i := range backlog {
		it := backlog[i]
		if it.Status != store.BacklogOpen || it.LaneID != "" || lanesByItem[it.ID] {
			continue
		}
		depsDone := true
		for _, depID := range it.DependsOn {
			if statusByID[depID] != store.BacklogDone {
				depsDone = false
				break
			}
		}
		if depsDone {
			readyIDs = append(readyIDs, it.ID)
		}
	}
	if len(readyIDs) == 0 {
		return nil
	}

	stateKey := fmt.Sprintf("open:%s:active:%d", strings.Join(readyIDs, ","), activeOrPlanned)
	r.mu.Lock()
	alreadyTriggered := r.dispatchedOpenSet[stateKey]
	if !alreadyTriggered {
		r.dispatchedOpenSet[stateKey] = true
	}
	r.mu.Unlock()
	if alreadyTriggered {
		return nil
	}

	_, err = r.store.Events().Append(ctx, store.Event{
		RunID:   r.cfg.RunID,
		Kind:    store.EventTimer,
		Class:   store.EventClassJudgment,
		Source:  controller.ActorController,
		Summary: fmt.Sprintf("open backlog items ready for dispatch: %s", strings.Join(readyIDs, ", ")),
		Urgent:  true,
	})
	return err
}

func (r *Runner) logNewDecisions(ctx context.Context) error {
	r.mu.Lock()
	sinceSeq := r.lastLoggedSeq
	r.mu.Unlock()

	decs, err := r.store.Decisions().List(ctx, store.DecisionFilter{
		RunID:    r.cfg.RunID,
		SinceSeq: sinceSeq,
	})
	if err != nil {
		return err
	}

	maxSeq := sinceSeq
	nonMetricCount := 0
	for i := range decs {
		d := decs[i]
		if d.Seq > maxSeq {
			maxSeq = d.Seq
		}
		if d.Tool != ToolMetricsSnapshot {
			nonMetricCount++
		}
		r.logger.Info("decision recorded",
			"run_id", d.RunID,
			"seq", d.Seq,
			"category", d.Category,
			"actor", d.Actor,
			"lane_id", d.LaneID,
			"tool", d.Tool,
			"reason", d.Reason,
			"result", d.Result,
			"input_tokens", d.InputTokens,
			"output_tokens", d.OutputTokens,
			"cost_usd", d.CostUSD,
		)
	}

	r.mu.Lock()
	if maxSeq > r.lastLoggedSeq {
		r.lastLoggedSeq = maxSeq
	}
	r.mu.Unlock()

	return nil
}

// IsSettledAndComplete reports whether the run has at least one backlog item or lane,
// all backlog items and lanes are in terminal states, and no unprocessed events remain.
func (r *Runner) IsSettledAndComplete(ctx context.Context) (bool, error) {
	cur, err := r.store.Events().GetCursor(ctx, r.cfg.RunID, controller.DefaultConsumer)
	if err != nil {
		return false, err
	}
	unprocessed, err := r.store.Events().List(ctx, store.EventFilter{
		RunID:    r.cfg.RunID,
		AfterSeq: cur.Seq,
	})
	if err != nil {
		return false, err
	}
	if len(unprocessed) > 0 {
		return false, nil
	}

	dirs, err := r.store.Directives().List(ctx, store.DirectiveFilter{
		RunID:      r.cfg.RunID,
		Kinds:      []store.DirectiveKind{store.DirectiveRecipeEdit, store.DirectivePlanImport},
		ActiveOnly: true,
	})
	if err != nil {
		return false, err
	}
	if len(dirs) > 0 {
		return false, nil
	}

	backlog, err := r.store.Backlog().List(ctx, store.BacklogFilter{RunID: r.cfg.RunID})
	if err != nil {
		return false, err
	}
	lanes, err := r.store.Lanes().List(ctx, store.LaneFilter{RunID: r.cfg.RunID})
	if err != nil {
		return false, err
	}
	if len(backlog) == 0 && len(lanes) == 0 {
		return false, nil
	}

	for i := range backlog {
		if !backlog[i].Status.Terminal() {
			return false, nil
		}
	}
	for i := range lanes {
		if !lanes[i].Status.Terminal() {
			return false, nil
		}
	}
	return true, nil
}

func (r *Runner) hasImmediateWork(ctx context.Context) (bool, error) {
	if err := r.enqueueOpenBacklogTriggerIfNeeded(ctx); err != nil {
		return false, err
	}
	cur, err := r.store.Events().GetCursor(ctx, r.cfg.RunID, controller.DefaultConsumer)
	if err != nil {
		return false, err
	}
	pending, err := r.store.Events().List(ctx, store.EventFilter{
		RunID:    r.cfg.RunID,
		AfterSeq: cur.Seq,
	})
	if err != nil {
		return false, err
	}
	if len(pending) > 0 {
		return true, nil
	}

	dirs, err := r.store.Directives().List(ctx, store.DirectiveFilter{
		RunID:      r.cfg.RunID,
		Kinds:      []store.DirectiveKind{store.DirectiveRecipeEdit, store.DirectivePlanImport},
		ActiveOnly: true,
	})
	if err != nil {
		return false, err
	}
	if len(dirs) > 0 {
		return true, nil
	}

	lanes, err := r.store.Lanes().List(ctx, store.LaneFilter{RunID: r.cfg.RunID})
	if err != nil {
		return false, err
	}
	planned := 0
	running := 0
	for i := range lanes {
		switch lanes[i].Status {
		case lane.StatusPlanned:
			planned++
		case lane.StatusRunning:
			running++
		}
	}
	if planned > 0 && (r.cfg.ConcurrencyCap <= 0 || running < r.cfg.ConcurrencyCap) {
		return true, nil
	}
	return false, nil
}

// Run executes the long-running orchestration loop until ctx is cancelled,
// `--once` completion is reached, or `--max-steps` is reached, then writes
// the final metrics snapshot to DecisionStore and releases the run lease.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.Bootstrap(ctx); err != nil {
		return err
	}

	r.logger.Info("nebula-orch run starting",
		"run_id", r.cfg.RunID,
		"holder_id", r.cfg.HolderID,
		"store", r.cfg.StoreSpec,
		"driver", r.cfg.DriverKind,
		"dispatcher_model", r.cfg.Models.Dispatcher,
		"concierge_model", r.cfg.Models.Concierge,
		"planner_model", r.cfg.Models.Planner,
		"concurrency_cap", r.cfg.ConcurrencyCap,
	)

	defer r.shutdown(ctx)

	steps := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if err := r.Step(ctx); err != nil && !errors.Is(err, store.ErrLeaseHeld) && !errors.Is(err, controller.ErrControllerStopped) {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		steps++

		if r.cfg.Once {
			// Drain any immediately actionable work (events, planned lanes, open backlog items) before waiting on ticker.
			for range 64 {
				done, err := r.IsSettledAndComplete(ctx)
				if err == nil && done {
					return nil
				}
				more, mErr := r.hasImmediateWork(ctx)
				if mErr != nil || !more {
					break
				}
				if stepErr := r.Step(ctx); stepErr != nil {
					return stepErr
				}
				steps++
			}
			if done, err := r.IsSettledAndComplete(ctx); err == nil && done {
				return nil
			}
		}

		if r.cfg.MaxSteps > 0 && steps >= r.cfg.MaxSteps {
			return nil
		}

		pollTimer := r.clock.NewTimer(r.cfg.PollInterval)
		select {
		case <-ctx.Done():
			pollTimer.Stop()
			return nil
		case <-pollTimer.C():
		}
	}
}

func (r *Runner) shutdown(parentCtx context.Context) {
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(parentCtx), 5*time.Second)
	defer cancel()

	metrics, dec, err := RecordMetricsDecision(shutCtx, r.store, r.clock, r.cfg.RunID, "run summary metrics")
	if err != nil {
		r.logger.Warn("failed to record run metrics decision", "run_id", r.cfg.RunID, "error", err)
	} else {
		r.mu.Lock()
		r.lastMetricsSeq = dec.Seq
		r.mu.Unlock()
		r.logger.Info("recorded run metrics to decision log",
			"run_id", r.cfg.RunID,
			"decision_seq", dec.Seq,
			"total_decisions", metrics.TotalDecisions,
			"auto_decisions", metrics.AutoDecisions,
			"judgment_decisions", metrics.JudgmentDecisions,
			"total_tokens", metrics.TotalTokens,
			"tokens_per_decision", metrics.TokensPerDecision,
			"cost_usd", metrics.TotalCostUSD,
			"cost_per_decision_usd", metrics.CostPerDecisionUSD,
			"owner_asks", metrics.OwnerAsks,
			"lane_idle_time_ms", metrics.LaneIdleTimeMs,
		)
	}

	if err := r.controller.ReleaseLease(shutCtx); err != nil && !errors.Is(err, store.ErrLeaseNotHeld) {
		r.logger.Warn("failed to release run lease on shutdown", "run_id", r.cfg.RunID, "error", err)
	} else {
		r.logger.Info("released run lease", "run_id", r.cfg.RunID, "holder_id", r.cfg.HolderID)
	}
}
