package adopt

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

// JudgeFactory creates a dispatcher for an adopted run.
type JudgeFactory func(runID string, answer func(ctx context.Context, runID, askID, text string) error) controller.Judge

// escalationAskTTL bounds an unanswered escalation question.
const escalationAskTTL = 24 * time.Hour

// Controllers attaches host runs to orca's deterministic lane lifecycle.
type Controllers struct {
	ctx               context.Context
	store             store.Store
	driver            lane.LaneDriver
	clock             clock.Clock
	judges            JudgeFactory
	Mailboxes         func(runID string) []mailbox.Mailbox
	PRProbe           controller.PRProbe
	Planner           func(ctx context.Context, runID, reason, planRef string) error
	ledgerScript      *string
	runs              map[string]*controlledRun
	starting          map[string]*startup
	MaxCostPerHourUSD float64
	mu                sync.Mutex
}

//nolint:govet // field alignment
type controlledRun struct {
	dispatch   controller.Judge
	driver     lane.LaneDriver
	controller *controller.Controller
	syncer     *runLedgerSync
	cancel     context.CancelFunc
	done       chan struct{}
	holder     string
	run        Run
	statusMu   sync.RWMutex
	downReason string
	notice     string
	restarts   int
	stopped    bool
}

type startup struct {
	done chan struct{}
	cr   *controlledRun
	err  error
}

func (d *Controllers) lookupRun(runID string) *controlledRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cr := d.runs[runID]; cr != nil {
		return cr
	}
	if s := d.starting[runID]; s != nil {
		return s.cr
	}
	return nil
}

func NewControllers(ctx context.Context, st store.Store, driver lane.LaneDriver, clk clock.Clock, judges JudgeFactory) *Controllers {
	return &Controllers{ctx: ctx, store: st, driver: driver, clock: clk, judges: judges, runs: map[string]*controlledRun{}, starting: map[string]*startup{}}
}

// Wake asks an adopted controller to process pending events immediately.
func (d *Controllers) Wake(runID string) {
	d.mu.Lock()
	cr := d.runs[runID]
	d.mu.Unlock()
	if cr != nil {
		cr.controller.Wake()
	}
}

// Step executes one synchronous controller step for an active adopted run.
func (d *Controllers) Step(ctx context.Context, runID string) error {
	d.mu.Lock()
	cr := d.runs[runID]
	d.mu.Unlock()
	if cr == nil {
		return nil
	}
	return cr.controller.Step(ctx)
}

func (d *Controllers) prepareStep(ctx context.Context, cr *controlledRun) (bool, error) {
	settings, err := d.store.Runs().GetSettings(ctx, cr.run.ID)
	if err != nil {
		return false, err
	}
	cr.controller.SetRuntimeSettings(settings)
	wasPaused := settings.Paused
	paused, _, _, msg, err := store.EnforceCostGuard(ctx, d.store, cr.run.ID, d.clock.Now().UTC(), d.MaxCostPerHourUSD)
	if err != nil {
		return false, err
	}
	if paused && !wasPaused {
		cr.controller.NotifyOwnerCostGuard(ctx, msg)
	}
	if latest, getErr := d.store.Runs().GetSettings(ctx, cr.run.ID); getErr == nil {
		settings = latest
		cr.controller.SetRuntimeSettings(settings)
	}
	dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: cr.run.ID, ActiveOnly: true})
	if err != nil {
		return false, err
	}
	controlPaused := false
	for i := range dirs {
		if dirs[i].Ref != "chat:control" {
			continue
		}
		switch dirs[i].Text {
		case "pause":
			controlPaused = true
		case "stop":
			return false, controller.ErrControllerStopped
		}
	}
	return !settings.Paused && !controlPaused, nil
}

// Active reports whether this process currently runs a controller for runID.
func (d *Controllers) Active(runID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	cr := d.runs[runID]
	if cr == nil {
		return false
	}
	select {
	case <-cr.done:
		return false
	default:
		return true
	}
}

// ControllerStatus returns a short owner-facing status for an adopted run.
// An empty string means the controller is healthy.
func (d *Controllers) ControllerStatus(runID string) string {
	d.mu.Lock()
	cr := d.runs[runID]
	d.mu.Unlock()
	if cr == nil {
		return ""
	}
	cr.statusMu.RLock()
	defer cr.statusMu.RUnlock()
	return cr.downReason
}

// ControllerNotice returns the latest stop or restart line for an adopted run.
func (d *Controllers) ControllerNotice(runID string) string {
	d.mu.Lock()
	cr := d.runs[runID]
	d.mu.Unlock()
	if cr == nil {
		return ""
	}
	cr.statusMu.RLock()
	defer cr.statusMu.RUnlock()
	return cr.notice
}

func (d *Controllers) Start(ctx context.Context, r Run) (retErr error) {
	d.mu.Lock()
	if cr := d.runs[r.ID]; cr != nil {
		select {
		case <-cr.done:
			delete(d.runs, r.ID)
		default:
			d.mu.Unlock()
			return nil
		}
	}
	if s, ok := d.starting[r.ID]; ok {
		d.mu.Unlock()
		<-s.done
		return s.err
	}
	s := &startup{done: make(chan struct{})}
	d.starting[r.ID] = s
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.starting, r.ID)
		s.err = retErr
		close(s.done)
		d.mu.Unlock()
	}()
	journal, err := readJournal(filepath.Join(r.Dir, leaseFile))
	if err != nil {
		return err
	}
	generation := journal.Marker
	populateRunRepo(&r)
	_, _, originRepo, hasTier1Repo := readTier1AndOriginRepo(r.Dir)
	if !hasTier1Repo {
		if storeRepo := resolveStoreRepository(ctx, d.store, r.ID, originRepo); storeRepo != "" {
			r.Repo = ""
			r.RepoID = ""
			applyRepoValue(&r, storeRepo)
		}
	}
	workflow, lanes, items, err := controllerState(r)
	if err != nil {
		return err
	}
	repo := cmp.Or(r.Repo, r.RepoID)
	if err := d.store.Runs().Create(ctx, store.Run{ID: r.ID, Repository: repo, Objective: r.FullObjective(), DefaultRecipe: workflow.Name}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	holder := cmp.Or(strings.TrimSpace(r.Holder), fmt.Sprintf("orca-adopt:%p", d))
	if _, err := d.store.Runs().AcquireLease(ctx, r.ID, holder, controller.DefaultLeaseTTL); err != nil {
		if errors.Is(err, store.ErrLeaseHeld) {
			if existing, gErr := d.store.Runs().Get(ctx, r.ID); gErr == nil && existing.Lease != nil &&
				(existing.Lease.HolderID == ChatHolder("") || strings.HasPrefix(existing.Lease.HolderID, ChatHolder("")+":")) {
				_ = d.store.Runs().ReleaseLease(ctx, r.ID, existing.Lease.HolderID)
				_, err = d.store.Runs().AcquireLease(ctx, r.ID, holder, controller.DefaultLeaseTTL)
			}
		}
		if err != nil {
			return err
		}
	}
	started := false
	defer func() {
		if !started {
			_ = d.store.Runs().ReleaseLease(context.WithoutCancel(ctx), r.ID, holder)
		}
	}()
	storedRun, err := d.store.Runs().Get(ctx, r.ID)
	if err != nil {
		return err
	}
	resume := sameAdoptionGeneration(storedRun.PlanRef, generation)
	actionGeneration := markerGeneration(generation)
	if resume {
		// Older controllers used unscoped keys; preserve their restart dedup.
		actionGeneration = ""
		decs, err := d.store.Decisions().List(ctx, store.DecisionFilter{RunID: r.ID})
		if err != nil {
			return err
		}
		for i := range decs {
			dec := &decs[i]
			var args controller.DecisionArgs
			if json.Unmarshal(dec.Args, &args) == nil && args.ActionKey == "adopt-generation:"+markerGeneration(generation) {
				actionGeneration = markerGeneration(generation)
				break
			}
		}
	}
	// Host state is authoritative at a new handover. Retain the audit log but
	// retire waits and pending adopt judgments from the previous handover.
	if !resume {
		dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: r.ID, ActiveOnly: true})
		if err != nil {
			return err
		}
		for i := range dirs {
			dir := &dirs[i]
			if strings.HasPrefix(dir.ID, "wait-adopt-") {
				if err := d.store.Directives().UpdateStatus(ctx, r.ID, dir.ID, store.DirectiveExpired); err != nil {
					return err
				}
			}
		}
		events, err := d.store.Events().List(ctx, store.EventFilter{RunID: r.ID})
		if err != nil {
			return err
		}
		var superseded []string
		for i := range events {
			ev := &events[i]
			if (ev.Source == "adopt" && (strings.HasPrefix(ev.ID, "adopt-merge-judgment-") || strings.HasPrefix(ev.ID, "adopt-lane-question-"))) || strings.HasPrefix(ev.ID, "expired-wait-adopt-") {
				superseded = append(superseded, ev.ID)
			}
		}
		// Resolve old launch reservations as superseded so consumers of the
		// audit log do not keep counting them as in-flight spawns.
		decs, err := d.store.Decisions().List(ctx, store.DecisionFilter{RunID: r.ID})
		if err != nil {
			return err
		}
		completed := make(map[string]bool)
		type pendingLaunch struct {
			laneID string
			args   controller.DecisionArgs
		}
		var pending []pendingLaunch
		for i := range decs {
			var args controller.DecisionArgs
			if json.Unmarshal(decs[i].Args, &args) != nil {
				continue
			}
			if strings.HasPrefix(args.ActionKey, "intent:phase.") || strings.HasPrefix(args.ActionKey, "intent:integrate-launch:") {
				args.ActionKey = strings.TrimPrefix(args.ActionKey, "intent:")
				pending = append(pending, pendingLaunch{args: args, laneID: decs[i].LaneID})
			} else {
				completed[args.ActionKey] = true
			}
		}
		for i := range pending {
			args := pending[i].args
			if completed[args.ActionKey] {
				continue
			}
			args.TriggerEventID = ""
			raw, err := json.Marshal(args)
			if err != nil {
				return err
			}
			if _, err := d.store.Decisions().Append(ctx, store.Decision{RunID: r.ID, Actor: controller.ActorController, LaneID: pending[i].laneID, Tool: controller.ToolAdoptLaunchSupersede, Reason: "host state supersedes unfinished launch", Args: raw}); err != nil {
				return err
			}
			completed[args.ActionKey] = true
		}
		args, err := json.Marshal(controller.DecisionArgs{ActionKey: "adopt-generation:" + actionGeneration, EventIDs: superseded})
		if err != nil {
			return err
		}
		if _, err := d.store.Decisions().Append(ctx, store.Decision{RunID: r.ID, Actor: controller.ActorController, Tool: controller.ToolAdoptSupersede, Reason: "new host adoption generation", Args: args}); err != nil {
			return err
		}
	}

	workflow.Version = 0
	saved, err := d.store.Recipes().Save(ctx, workflow)
	if err != nil {
		return err
	}
	workflow = saved
	for i := range items {
		item := items[i]
		if err := d.store.Backlog().Add(ctx, item); err != nil {
			if !errors.Is(err, store.ErrAlreadyExists) {
				return err
			}
			if !resume {
				if err := d.store.Backlog().Update(ctx, item, controller.ActorController, "adopt host task"); err != nil {
					return err
				}
			} else if item.NoMerge {
				if existingItem, gErr := d.store.Backlog().Get(ctx, r.ID, item.ID); gErr == nil && existingItem != nil && !existingItem.NoMerge {
					existingItem.NoMerge = true
					if existingItem.Description == "" && item.Description != "" {
						existingItem.Description = item.Description
					}
					if err := d.store.Backlog().Update(ctx, *existingItem, controller.ActorController, "mark no-merge from host brief"); err != nil {
						return err
					}
				}
			}
		}
	}
	for i := range lanes {
		ln := &lanes[i]
		if resume {
			stored, err := d.store.Lanes().Get(ctx, r.ID, ln.ID)
			if err == nil {
				*ln = *stored
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		ln.RecipeVersion = workflow.Version
		if err := d.store.Lanes().Upsert(ctx, *ln); err != nil {
			return err
		}
		if tracker, ok := d.driver.(interface{ TrackLane(lane.Lane) }); ok && !ln.Status.Terminal() {
			tracker.TrackLane(*ln)
		}
	}
	for i := range r.Lanes {
		imported := &r.Lanes[i]
		if integratedLane(*imported) || finishedAnalysisLane(*imported) {
			continue
		}
		waitingOn, until := imported.expectedHostWait(d.clock.Now().UTC())
		if waitingOn == "" {
			continue
		}
		var deadline *time.Time
		if !until.IsZero() {
			deadline = &until
		}
		wait := store.Directive{ID: "wait-adopt-" + imported.ID, RunID: r.ID, Kind: store.DirectiveInstruction, Source: store.DirectiveSourceDispatcher, Scope: imported.ID, Ref: controller.NamedWaitRefPrefix + imported.ID, Text: "waiting on " + waitingOn, ExpiresAt: deadline, Status: store.DirectiveActive}
		if err := d.store.Directives().Add(ctx, wait); err != nil {
			if !errors.Is(err, store.ErrAlreadyExists) {
				return err
			}
			if !resume {
				if err := d.store.Directives().Replace(ctx, wait); err != nil {
					return err
				}
			}
		}
	}

	if err := d.store.Runs().UpdateObjective(ctx, r.ID, fullObjectiveWithHandover(r), generation); err != nil {
		return err
	}
	var boxes []mailbox.Mailbox
	if d.Mailboxes != nil {
		boxes = d.Mailboxes(r.ID)
	}
	syncer := newRunLedgerSync(r, d.clock, lanes)
	syncer.ledgerScript = d.ledgerScript
	runStore := &adoptedStore{
		Store: d.store,
		runs: &adoptedRunStore{
			RunStore: d.store.Runs(),
			syncer:   syncer,
		},
		lanes: &adoptedLaneStore{
			LaneStore:   d.store.Lanes(),
			syncer:      syncer,
			controllers: d,
		},
		backlog: &adoptedBacklogStore{
			BacklogStore: d.store.Backlog(),
			controllers:  d,
		},
	}
	var ctrl *controller.Controller
	var cr *controlledRun
	var dispatch controller.Judge
	if d.judges != nil {
		dispatch = d.judges(r.ID, func(ctx context.Context, runID, askID, text string) error {
			if runID != r.ID {
				return fmt.Errorf("owner answer run %q does not match adopted run %q", runID, r.ID)
			}
			return ctrl.AnswerOwnerAskTextFromJudge(ctx, askID, text)
		})
	}
	if setter, ok := dispatch.(interface{ SetMailboxes(...mailbox.Mailbox) }); ok && len(boxes) > 0 {
		setter.SetMailboxes(boxes...)
	}
	if setter, ok := dispatch.(interface {
		SetRunCloseout(func(context.Context, string, dispatcher.RunCloseoutParams) (string, error))
	}); ok {
		setter.SetRunCloseout(d.CloseoutRun)
	}
	adoptedAt := parseAdoptCutoff(generation, d.clock)
	for i := range boxes {
		if setter, ok := boxes[i].(mailbox.AdoptCutoffSetter); ok {
			setter.SetAdoptCutoff(adoptedAt, runStore)
			_ = setter.SyncAdoptCutoff(ctx)
		}
	}
	runDriver := d.driver
	if runDriver != nil {
		runDriver = &adoptedRunDriver{LaneDriver: runDriver, run: r, store: runStore, syncer: syncer, clock: d.clock}
		if ds, ok := dispatch.(interface{ SetDriver(lane.LaneDriver) }); ok {
			ds.SetDriver(runDriver)
		} else if dispatch != nil {
			slog.DebugContext(ctx, "orca/adopt: judge does not implement SetDriver; adoptedRunDriver not attached to judge", "run_id", r.ID)
		}
	}
	var integrationMu sync.Mutex
	integrationCache := map[string]integrationResult{}
	verify := func(ctx context.Context, ln lane.Lane) (bool, string, error) {
		integrationMu.Lock()
		defer integrationMu.Unlock()
		key := ln.ID + ":" + ln.PRHead
		if result, ok := integrationCache[key]; ok {
			return result.merged, result.proof, result.err
		}
		merged, proof, err := d.verifyIntegration(ctx, syncer, ln)
		integrationCache[key] = integrationResult{merged: merged, proof: proof, err: err}
		return merged, proof, err
	}
	ctrl = controller.New(controller.Config{
		BeforeStep: func(ctx context.Context) (bool, error) {
			integrationMu.Lock()
			clear(integrationCache)
			integrationMu.Unlock()
			return d.prepareStep(ctx, cr)
		},
		IsolateLaneErrors: true,
		StepError: func(err error) {
			if cr == nil || err == nil {
				return
			}
			cr.statusMu.Lock()
			cr.downReason = err.Error()
			cr.notice = fmt.Sprintf("⚠ controller lane failure: %s", err)
			cr.statusMu.Unlock()
		},
		RunID:              r.ID,
		AdoptionGeneration: actionGeneration,
		HolderID:           holder,
		PRProbe:            d.runPRProbe(syncer),
		HostIntegration:    verify,
		JudgeLaneMail:      true,
		MaxCostPerHourUSD:  d.MaxCostPerHourUSD,
		NoPR:               r.NoPR,
		HoldAdvance:        holdAdvanceLanes(r),
		HoldAdvanceCheck:   func(id string) bool { return fileExists(filepath.Join(r.Dir, id+".hold-advance")) },
		ImportedHostState:  true,
		HostRun:            true,
	}, runStore, runDriver, d.clock, d.judge(holder, dispatch), boxes...)
	runCtx, cancel := context.WithCancel(d.ctx)
	cr = &controlledRun{
		controller: ctrl,
		dispatch:   dispatch,
		driver:     runDriver,
		syncer:     syncer,
		cancel:     cancel,
		done:       make(chan struct{}),
		run:        r,
		holder:     holder,
	}
	d.mu.Lock()
	s.cr = cr
	d.mu.Unlock()
	cleanupFailedStart := func() {
		cancel()
		if closer, ok := dispatch.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
		close(cr.done)
	}
	if err := d.observeFirst(ctx, r, dispatch); err != nil {
		cleanupFailedStart()
		return err
	}
	// A host lane can already have handed back a completed SWE report before
	// adoption. Surface the same merge judgment used by a live report instead
	// of treating the imported done status as permission to merge or advance.
	for i := range r.Lanes {
		preview := previewLaneAt(r, r.Lanes[i], d.clock.Now().UTC())
		if preview.Move != "judge" {
			continue
		}
		prefix, summary := "adopt-merge-judgment-", "host merge judgment: "
		if strings.HasPrefix(preview.Reason, "lane question:") {
			prefix, summary = "adopt-lane-question-", ""
		}
		eventID := prefix + r.Lanes[i].ID
		if actionGeneration != "" {
			eventID += ":adopt:" + actionGeneration
		}
		_, err := d.store.Events().Append(ctx, store.Event{
			ID:      eventID,
			RunID:   r.ID,
			LaneID:  r.Lanes[i].ID,
			Kind:    store.EventTimer,
			Class:   store.EventClassJudgment,
			Source:  "adopt",
			Summary: summary + preview.Reason,
		})
		if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			cleanupFailedStart()
			return err
		}
	}
	if !r.HasNextWork() || ObjectiveIsDone(r.Objective) {
		expireHoldDirectivesIfRunDone(ctx, runStore, r.ID)
	}
	// Fail synchronously if another process still holds the controller lease.
	if err := ctrl.Step(ctx); err != nil {
		if !controller.IsTransient(err) {
			cleanupFailedStart()
			return err
		}
		slog.Warn("adopted controller initial step failed; retrying", "run", r.ID, "error", err)
	}
	if planned, err := d.planNextWorkIfDrained(ctx, r.ID, "adopt"); err != nil {
		slog.Warn("plan next work on adopt failed", "run", r.ID, "error", err)
	} else if planned {
		_ = ctrl.Step(ctx)
	}
	if err := ctrl.StartWatches(runCtx); err != nil {
		cleanupFailedStart()
		_ = ctrl.ReleaseLease(ctx)
		return err
	}
	d.mu.Lock()
	d.runs[r.ID] = cr
	d.mu.Unlock()
	started = true
	go d.supervise(runCtx, cr)
	return nil
}

func holdAdvanceLanes(r Run) map[string]bool {
	if len(r.Lanes) == 0 {
		return nil
	}
	out := make(map[string]bool)
	for i := range r.Lanes {
		if r.Lanes[i].HoldAdvance {
			out[r.Lanes[i].ID] = true
		}
	}
	return out
}

func (d *Controllers) observeFirst(ctx context.Context, r Run, dispatch controller.Judge) error {
	holds := make(map[string]string)
	for i := range r.Lanes {
		if !r.Lanes[i].HoldAdvance {
			continue
		}
		p := filepath.Join(r.Dir, r.Lanes[i].ID+".hold-advance")
		holds[r.Lanes[i].ID] = readText(p)
	}
	obs := controller.ContractObservation{Objective: r.FullObjective(), Context: r.Context, IntegrateRule: r.IntegrateRule, NoPR: r.NoPR, HoldAdvance: holds}
	if observer, ok := dispatch.(controller.ContractObserver); ok {
		return observer.ObserveRunContract(ctx, r.ID, obs)
	}
	// Test and embedded judges that do not expose the richer observer still get
	// a durable gate record; no lane or host state is touched by this fallback.
	args, _ := json.Marshal(obs)
	_, err := d.store.Decisions().Append(ctx, store.Decision{RunID: r.ID, Category: store.DecisionJudgment, Actor: "dispatcher", Tool: "adopt.observe", Reason: "read host run contract before lifecycle actions", Result: "observe-first pass complete", Args: args, Vetted: true, CreatedAt: d.clock.Now().UTC()})
	return err
}

const maxControllerRestarts = 5

func (d *Controllers) supervise(ctx context.Context, cr *controlledRun) {
	defer close(cr.done)
	defer cr.cancel()
	for {
		err := cr.controller.Run(ctx)
		if errors.Is(err, controller.ErrControllerStopped) {
			cr.statusMu.Lock()
			cr.downReason = controller.ErrControllerStopped.Error()
			cr.notice = "⚠ controller stopped"
			cr.stopped = true
			cr.statusMu.Unlock()
			return
		}
		if err == nil || ctx.Err() != nil {
			return
		}
		if controller.IsUnrecoverable(err) {
			cr.statusMu.Lock()
			cr.downReason = err.Error()
			cr.notice = fmt.Sprintf("⚠ controller stopped: %s", err)
			cr.stopped = true
			cr.statusMu.Unlock()
			slog.Error("adopted controller stopped", "run", cr.run.ID, "error", err)
			return
		}
		cr.statusMu.Lock()
		cr.downReason = err.Error()
		cr.restarts++
		restart := cr.restarts
		cr.notice = fmt.Sprintf("⚠ controller down: %s", err)
		cr.statusMu.Unlock()
		slog.Warn("adopted controller restarting", "run", cr.run.ID, "attempt", restart, "error", err)
		if restart > maxControllerRestarts {
			cr.statusMu.Lock()
			cr.stopped = true
			cr.notice = fmt.Sprintf("⚠ controller stopped after %d restart attempts: %s", maxControllerRestarts, err)
			cr.statusMu.Unlock()
			return
		}
		// Controller.Run owns the heartbeat while it is active. Keep renewing
		// during the supervisor's backoff so a transient stop cannot surrender
		// the adopted run before the next restart.
		stopLease := d.keepLease(ctx, cr.run.ID, cr.holder)
		timer := d.clock.NewTimer(time.Duration(restart) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			stopLease()
			return
		case <-timer.C():
		}
		stopLease()
		cr.statusMu.Lock()
		cr.downReason = ""
		cr.notice = fmt.Sprintf("controller restarted (attempt %d)", restart)
		cr.statusMu.Unlock()
	}
}

func markerGeneration(marker string) string {
	s, ok := strings.CutPrefix(strings.TrimSpace(marker), markerPrefix)
	if !ok {
		return ""
	}
	if open := strings.IndexByte(s, '('); open >= 0 {
		return strings.TrimSpace(s[:open])
	}
	return strings.TrimSpace(s)
}

func parseAdoptCutoff(marker string, clk clock.Clock) time.Time {
	gen := markerGeneration(marker)
	if gen != "" {
		if t, err := time.Parse(time.RFC3339Nano, gen); err == nil {
			return t.UTC()
		}
		if t, err := time.Parse(time.RFC3339, gen); err == nil {
			return t.UTC()
		}
	}
	if clk != nil {
		return clk.Now().UTC()
	}
	return time.Now().UTC()
}

func expireHoldDirectivesIfRunDone(ctx context.Context, st store.Store, runID string) {
	if st == nil || strings.TrimSpace(runID) == "" {
		return
	}
	lanes, err := st.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		return
	}
	items, err := st.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
	if err != nil {
		return
	}
	if dispatcher.CheckRunTerminal(lanes, items, false) != nil {
		return
	}
	dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err != nil {
		return
	}
	for i := range dirs {
		d := &dirs[i]
		if d.Kind != store.DirectiveStanding && d.Kind != store.DirectiveInstruction && d.Kind != store.DirectiveNote {
			continue
		}
		if dispatcher.IsHoldDirectiveText(d.Text) {
			_ = st.Directives().UpdateStatus(ctx, runID, d.ID, store.DirectiveExpired)
			if st.Memories() != nil {
				_ = st.Memories().Delete(ctx, "", d.ID)
			}
		}
	}
}

func sameAdoptionGeneration(storedPlanRef, currentMarker string) bool {
	if currentMarker == "" || storedPlanRef == "" {
		return false
	}
	if storedPlanRef == currentMarker {
		return true
	}
	g1, g2 := markerGeneration(storedPlanRef), markerGeneration(currentMarker)
	return g1 != "" && g1 == g2
}

// Stop stops r's controller, checkpoints its lanes, and releases its SQLite
// run lease (used by /release).
func (d *Controllers) Stop(ctx context.Context, r Run) error {
	return d.stop(ctx, r, true)
}

func (d *Controllers) stop(ctx context.Context, r Run, releaseLease bool) error {
	d.mu.Lock()
	s := d.starting[r.ID]
	d.mu.Unlock()
	if s != nil {
		<-s.done
	}
	d.mu.Lock()
	cr := d.runs[r.ID]
	d.mu.Unlock()
	if cr == nil {
		return nil
	}
	cr.controller.Kill()
	<-cr.done
	cr.cancel()
	if closer, ok := cr.dispatch.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	current, err := d.store.Lanes().List(ctx, store.LaneFilter{RunID: r.ID})
	if err != nil {
		return err
	}
	if cr.syncer != nil {
		if err := cr.syncer.SyncAll(ctx, current); err != nil {
			slog.Warn("sync adopted run lanes on stop failed", "run", r.ID, "error", err)
		}
	}
	if err := checkpoint(r.Dir, current); err != nil {
		return err
	}
	if releaseLease {
		if err := cr.controller.ReleaseLease(ctx); err != nil {
			return err
		}
	}
	d.mu.Lock()
	if d.runs[r.ID] == cr {
		delete(d.runs, r.ID)
	}
	d.mu.Unlock()
	return nil
}

// judge runs the dispatcher and exposes unresolved escalations to the owner.
func (d *Controllers) judge(holder string, dispatch controller.Judge) controller.Judge {
	return controller.JudgeFunc(func(ctx context.Context, runID string, events []store.Event) error {
		_, _ = d.RefreshObjective(ctx, runID)
		var err error
		if dispatch != nil {
			var turnsBefore int
			if d.store != nil {
				if turns, tErr := d.store.Threads().ListTurns(ctx, "dispatcher-"+runID); tErr == nil {
					turnsBefore = len(turns)
				}
			}
			stop := d.keepLease(ctx, runID, holder)
			err = dispatch.Judge(ctx, runID, events)
			stop()
			if err != nil {
				if d.store != nil {
					var turnsAfter int
					if turns, tErr := d.store.Threads().ListTurns(ctx, "dispatcher-"+runID); tErr == nil {
						turnsAfter = len(turns)
					}
					if turnsAfter == turnsBefore {
						_, _ = d.store.Decisions().Append(ctx, store.Decision{
							RunID:     runID,
							Category:  store.DecisionJudgment,
							Actor:     "dispatcher",
							Tool:      controller.ToolJudgmentDispatch,
							Reason:    "dispatcher turn failed",
							Result:    "dispatcher failed: " + err.Error(),
							Vetted:    false,
							CreatedAt: d.clock.Now().UTC(),
						})
					}
				}
			}
		}
		d.syncRunLanes(ctx, runID)
		if _, pErr := d.planNextWorkIfDrained(ctx, runID, "backlog_drained"); pErr != nil {
			slog.Warn("plan next work after judge failed", "run", runID, "error", pErr)
		}
		if err != nil {
			slog.Error("adopted run dispatcher failed; asking the owner about its escalations", "run", runID, "error", err)
		}
		return d.askOwnerOnEscalation(ctx, runID, events)
	})
}

func adoptNormalizeContractions(s string) string {
	return strings.NewReplacer(
		"don't", "dont",
		"can't", "cant",
		"won't", "wont",
		"shouldn't", "shouldnt",
		"didn't", "didnt",
		"isn't", "isnt",
		"wasn't", "wasnt",
	).Replace(s)
}

func adoptContainsWholeWord(text, word string) bool {
	if word == "" {
		return false
	}
	for start := 0; start < len(text); {
		idx := strings.Index(text[start:], word)
		if idx < 0 {
			return false
		}
		pos := start + idx
		end := pos + len(word)
		leftOK := pos == 0 || !isWordByte(text[pos-1])
		rightOK := end == len(text) || !isWordByte(text[end])
		if leftOK && rightOK {
			return true
		}
		start = pos + 1
	}
	return false
}

func isWordByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func (d *Controllers) syncRunLanes(ctx context.Context, runID string) {
	cr := d.lookupRun(runID)
	if cr == nil || cr.syncer == nil {
		return
	}
	_, _ = cr.syncer.RefreshObjectiveIfChanged(ctx, d.store.Runs())
	lanes, err := d.store.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		return
	}
	if err := cr.syncer.SyncAll(ctx, lanes); err != nil {
		slog.Warn("sync adopted run lanes failed", "run", runID, "error", err)
	}
}

// RefreshObjective re-reads OBJECTIVE.md from the adopted run directory when
// its stat signature changes and updates store.Run.Objective if the content changed.
func (d *Controllers) RefreshObjective(ctx context.Context, runID string) (bool, error) {
	cr := d.lookupRun(runID)
	if cr == nil || cr.syncer == nil {
		return false, nil
	}
	return cr.syncer.RefreshObjectiveIfChanged(ctx, d.store.Runs())
}

func (d *Controllers) runPRProbe(syncer *runLedgerSync) controller.PRProbe {
	if d.PRProbe == nil {
		return nil
	}
	var cacheMu sync.Mutex
	var cachedDir string
	var cachedHasGit bool
	var cachedSet bool
	checkGit := func(dir string) bool {
		cacheMu.Lock()
		defer cacheMu.Unlock()
		if cachedSet && cachedDir == dir {
			return cachedHasGit
		}
		cachedDir = dir
		cachedHasGit = hasGitCheckout(dir)
		cachedSet = true
		return cachedHasGit
	}
	return func(ctx context.Context, runID, branch string, pr int) (controller.PRStatus, error) {
		if syncer != nil {
			curRun := syncer.CurrentRun()
			if strings.TrimSpace(curRun.Repo) == "" && strings.TrimSpace(curRun.RepoID) == "" && !checkGit(curRun.Dir) {
				var storeRepo string
				if d.store != nil && runID != "" {
					if sr, err := d.store.Runs().Get(ctx, runID); err == nil && sr != nil {
						storeRepo = strings.TrimSpace(sr.Repository)
					}
				}
				if storeRepo == "" {
					return controller.PRStatus{}, nil
				}
			}
		}
		return d.PRProbe(ctx, runID, branch, pr)
	}
}

type integrationResult struct {
	err    error
	proof  string
	merged bool
}

// verifyIntegration deliberately ignores report outcomes and host PR-state prose.
// Only a recorded merge SHA or commit evidence on the configured target counts.
func (d *Controllers) verifyIntegration(ctx context.Context, syncer *runLedgerSync, ln lane.Lane) (bool, string, error) {
	r := syncer.CurrentRun()
	r.TargetBranch = cmp.Or(strings.TrimSpace(r.TargetBranch), recipe.Default().MergePolicy.TargetBranch)
	b, err := os.ReadFile(filepath.Join(r.Dir, "state.json"))
	if err != nil {
		return false, "", err
	}
	var host ledger
	if err := json.Unmarshal(b, &host); err != nil {
		return false, "", err
	}
	for i := range host.Tasks {
		if host.Tasks[i].ID == ln.ID && validGitSHA(strings.TrimSpace(host.Tasks[i].MergeSHA)) {
			return true, strings.TrimSpace(host.Tasks[i].MergeSHA), nil
		}
	}
	r.Lanes = []Lane{{ID: ln.ID, Branch: ln.Branch, Head: ln.PRHead, PR: strconv.Itoa(ln.PR), Status: "running"}}
	applyTargetIntegrationEvidence(ctx, &r, d.Mirror)
	if r.Lanes[0].Integrated {
		return true, ln.PRHead, nil
	}
	return false, fmt.Sprintf("PR #%d remains open or unverified on target %s: %s", ln.PR, r.TargetBranch, cmp.Or(r.IntegrationError, r.Lanes[0].IntegrationReason)), nil
}

const maxGitCheckoutSearchDepth = 4

func hasGitCheckout(dir string) bool {
	cur := strings.TrimSpace(dir)
	if cur == "" {
		return false
	}
	cur = filepath.Clean(cur)
	for depth := 0; depth <= maxGitCheckoutSearchDepth; depth++ {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
	return false
}

func (d *Controllers) planNextWorkIfDrained(ctx context.Context, runID, trigger string) (bool, error) {
	cr := d.lookupRun(runID)
	d.mu.Lock()
	plannerFn := d.Planner
	d.mu.Unlock()
	if cr == nil || cr.syncer == nil {
		return false, nil
	}
	_, _ = cr.syncer.RefreshObjectiveIfChanged(ctx, d.store.Runs())
	lanes, err := d.store.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		return false, err
	}
	items, err := d.store.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
	if err != nil {
		return false, err
	}
	if dispatcher.CheckRunTerminal(lanes, items, false) != nil {
		return false, nil
	}
	curRun := cr.syncer.CurrentRun()
	needsCloseout := shouldAutoCloseoutRun(curRun, lanes, items)
	if !needsCloseout && (!curRun.HasNextWork() || ObjectiveIsDone(curRun.Objective)) {
		expireHoldDirectivesIfRunDone(ctx, d.store, runID)
		return false, nil
	}
	now := d.clock.Now().UTC()
	if paused, _, _, _, _ := store.EnforceCostGuard(ctx, d.store, runID, now, d.MaxCostPerHourUSD); paused {
		return false, nil
	}
	if settings, sErr := d.store.Runs().GetSettings(ctx, runID); sErr == nil && settings.Paused {
		return false, nil
	}
	var doneSummary []string
	for i := range items {
		it := &items[i]
		doneSummary = append(doneSummary, fmt.Sprintf("%s:%s", it.ID, it.Status))
	}
	dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err != nil {
		return false, err
	}
	for i := range dirs {
		dir := &dirs[i]
		if strings.HasPrefix(dir.Ref, ownerAskRef) && (dir.ExpiresAt == nil || now.Before(*dir.ExpiresAt)) {
			return false, nil
		}
	}
	if needsCloseout {
		if cr.syncer.MarkClosedOut() {
			if _, cErr := d.CloseoutRun(ctx, runID, dispatcher.RunCloseoutParams{
				Reason: "all lanes terminal (" + trigger + "); execute remaining handover close-out steps",
			}); cErr != nil {
				cr.syncer.ClearClosedOut()
				return false, cErr
			}
			return true, nil
		}
		return false, nil
	}
	if plannerFn == nil {
		return false, nil
	}
	planKey := curRun.FullObjective() + "|" + strings.Join(doneSummary, ",")
	if !cr.syncer.MarkPlanned(planKey) {
		return false, nil
	}
	var reason strings.Builder
	fmt.Fprintf(&reason, "Adopted run %s backlog drained (%s); turn the remaining Next: / open work in OBJECTIVE.md into backlog items, or ask the owner one question if a decision is required.\n\n", runID, trigger)
	fmt.Fprintf(&reason, "OBJECTIVE.md:\n%s\n", curRun.FullObjective())
	if curRun.Next != "" {
		fmt.Fprintf(&reason, "\nNext: %s\n", curRun.Next)
	}
	if curRun.PlanPath != "" {
		fmt.Fprintf(&reason, "Plan path: %s\n", curRun.PlanPath)
	}
	planRef := ""
	if curRun.PlanPath != "" && fileExists(curRun.PlanPath) {
		planRef = curRun.PlanPath
	}
	stop := func() {}
	if cr.holder != "" {
		stop = d.keepLease(ctx, runID, cr.holder)
	}
	err = plannerFn(ctx, runID, reason.String(), planRef)
	stop()
	_, _, _, _, _ = store.EnforceCostGuard(ctx, d.store, runID, d.clock.Now().UTC(), d.MaxCostPerHourUSD)
	if err != nil {
		if ctx.Err() != nil {
			cr.syncer.ClearPlanned(planKey)
		} else {
			_, _ = d.store.Events().Append(context.WithoutCancel(ctx), store.Event{
				RunID:   runID,
				Kind:    store.EventTimer,
				Class:   store.EventClassJudgment,
				Source:  "planner",
				Summary: "planner next-work handoff failed: " + err.Error(),
			})
		}
		return false, err
	}
	afterItems, err := d.store.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
	if err != nil {
		return true, nil
	}
	addedOpen := false
	for i := range afterItems {
		if afterItems[i].Status == store.BacklogOpen {
			addedOpen = true
			break
		}
	}
	if addedOpen {
		_, _ = d.store.Events().Append(ctx, store.Event{
			RunID:   runID,
			Kind:    store.EventTimer,
			Class:   store.EventClassJudgment,
			Source:  "planner",
			Urgent:  true,
			Summary: "planner created next backlog items from OBJECTIVE.md",
		})
		d.Wake(runID)
	}
	return true, nil
}

func (d *Controllers) keepLease(ctx context.Context, runID, holder string) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	ticker := d.clock.NewTicker(controller.DefaultHeartbeatInterval)
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C():
				_, _ = d.store.Runs().HeartbeatLease(ctx, runID, holder, controller.DefaultLeaseTTL)
			}
		}
	}()
	return func() { close(stop); <-done }
}

// askOwnerOnEscalation creates one owner question per unresolved escalation.
func (d *Controllers) askOwnerOnEscalation(ctx context.Context, runID string, events []store.Event) error {
	var laneIDs []string
	for i := range events {
		if id := events[i].LaneID; id != "" && !slices.Contains(laneIDs, id) {
			laneIDs = append(laneIDs, id)
		}
	}
	if len(laneIDs) == 0 {
		return nil
	}
	now := d.clock.Now().UTC()
	var errs []error
	for _, id := range laneIDs {
		ln, err := d.store.Lanes().Get(ctx, runID, id)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				errs = append(errs, err)
			}
			continue
		}
		if ln.Status != lane.StatusBlocked {
			continue
		}
		decs, err := d.store.Decisions().List(ctx, store.DecisionFilter{RunID: runID, LaneID: id})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var esc *store.Decision
		for j := len(decs) - 1; j >= 0 && esc == nil; j-- {
			if decs[j].Tool == controller.ToolLaneEscalate {
				esc = &decs[j]
			}
		}
		if esc == nil {
			continue
		}
		errs = append(errs, d.addEscalationAsk(ctx, runID, ln, esc, now))
	}
	return errors.Join(errs...)
}

const ownerAskRef = "owner_ask:"

func (d *Controllers) addEscalationAsk(ctx context.Context, runID string, ln *lane.Lane, esc *store.Decision, now time.Time) error {
	hold := "hold: keep " + ln.ID + " blocked"
	ask := controller.OwnerAsk{
		ID:       fmt.Sprintf("escalation-%s-%d", ln.ID, esc.Seq),
		RunID:    runID,
		LaneID:   ln.ID,
		Question: fmt.Sprintf("Lane %s escalated (%s). How should it continue?", ln.ID, esc.Reason),
		Options:  []string{fmt.Sprintf("rework: start swe round %d", ln.Round+1), "stop: drop " + ln.ID, hold},
		Default:  hold,
		Deadline: now.Add(escalationAskTTL),
	}
	if d.Mailboxes != nil {
		notifier := dispatcher.New(dispatcher.Config{Mailboxes: d.Mailboxes(runID)}, d.store, d.driver, d.clock, nil)
		_, err := notifier.RegisterOwnerAsk(ctx, runID, dispatcher.OwnerAskParams{ID: ask.ID, LaneID: ask.LaneID, Question: ask.Question, Options: ask.Options, Default: ask.Default, Deadline: ask.Deadline.Format(time.RFC3339)})
		return err
	}
	// Without a mailbox there is no delivery retry path, but the fallback must
	// still preserve the one-open-ask-per-lane invariant.
	dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err != nil {
		return err
	}
	for i := range dirs {
		if dirs[i].Scope == ln.ID && strings.HasPrefix(dirs[i].Ref, ownerAskRef) {
			return nil
		}
	}
	// Keep the ask scoped to the blocked lane so its default cannot change lane state.
	if err := ask.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(ask)
	if err != nil {
		return err
	}
	err = d.store.Directives().Add(ctx, store.Directive{
		ID:        "ask-" + ask.ID,
		RunID:     runID,
		Kind:      store.DirectiveInstruction,
		Source:    store.DirectiveSourceDispatcher,
		Scope:     ln.ID,
		Ref:       ownerAskRef + ask.ID,
		Text:      string(raw),
		ExpiresAt: &ask.Deadline,
		Status:    store.DirectiveActive,
	})
	if errors.Is(err, store.ErrAlreadyExists) {
		return nil
	}
	return err
}

// Close stops controllers and checkpoints their lanes while keeping both the
// handover lease and the SQLite run lease so a restarted or re-opened chat
// continues holding adopted host runs until /release.
func (d *Controllers) Close(ctx context.Context) error {
	d.mu.Lock()
	var starts []*startup
	for _, s := range d.starting {
		starts = append(starts, s)
	}
	d.mu.Unlock()
	for _, s := range starts {
		<-s.done
	}
	d.mu.Lock()
	var runs []Run
	for _, cr := range d.runs {
		runs = append(runs, cr.run)
	}
	d.mu.Unlock()
	var errs []error
	for i := range runs {
		errs = append(errs, d.stop(ctx, runs[i], false))
	}
	return errors.Join(errs...)
}

func controllerState(r Run) (recipe.Recipe, []lane.Lane, []store.BacklogItem, error) {
	b, err := os.ReadFile(filepath.Join(r.Dir, "state.json"))
	if err != nil {
		return recipe.Recipe{}, nil, nil, err
	}
	var st struct {
		Tasks []struct {
			Sessions map[string]string `json:"sessions"`
			// Terminals is nebula-swarm's phase -> round -> terminal id.
			Terminals   map[string]map[string]string `json:"terminals"`
			ID          string                       `json:"id"`
			PRHead      string                       `json:"pr_head"`
			ApprovalSHA string                       `json:"approval_sha"`
			MergeSHA    string                       `json:"merge_sha"`
			Checks      string                       `json:"checks"`
			BaseCommit  string                       `json:"fork_sha"`
			Worktree    string                       `json:"worktree"`
			Brief       string                       `json:"brief"`
			Priority    store.Priority               `json:"priority"`
			DependsOn   []string                     `json:"depends_on"`
		} `json:"tasks"`
		Config struct {
			MergePolicy *recipe.MergePolicy `json:"merge_policy"`
			MergeRule   string              `json:"merge_rule"`
			// Merge is nebula-swarm's merge authority: none or a merge method.
			Merge  string      `json:"merge"`
			Base   string      `json:"base"`
			Target string      `json:"target"`
			Phases []hostPhase `json:"phases"`
			NoPR   bool        `json:"no_pr"`
		} `json:"config"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return recipe.Recipe{}, nil, nil, err
	}
	r.NoPR = r.NoPR || st.Config.NoPR
	defaultRecipe := recipe.Default()
	workflow := defaultRecipe.Clone()
	workflow.RunID = r.ID
	workflow.Name = "adopted"
	objectiveText := stripMarker(readText(filepath.Join(r.Dir, "OBJECTIVE.md")))
	contextText := stripMarker(readText(filepath.Join(r.Dir, "CONTEXT.md")))
	ruleText := r.IntegrateRule
	if strings.TrimSpace(ruleText) == "" {
		ruleText = extractMergeRuleText(st.Config.MergeRule, objectiveText+"\n"+contextText)
	}
	r.IntegrateRule = ruleText
	if st.Config.MergePolicy != nil {
		workflow.MergePolicy = *st.Config.MergePolicy
	} else if strings.EqualFold(st.Config.Merge, "self") || strings.EqualFold(st.Config.Merge, "lane") || hasSelfMergeRule(ruleText) {
		workflow.MergePolicy.Mode = recipe.MergeModeSelf
	} else if r.Format == FormatNebulaSwarm && (st.Config.Merge == "" || st.Config.Merge == "none") {
		// A nebula-swarm run without merge authority leaves its PRs to a person.
		workflow.MergePolicy.Mode = recipe.MergeModeManual
	}
	if st.Config.Target != "" {
		workflow.MergePolicy.TargetBranch = st.Config.Target
	}
	if r.NoPR {
		workflow = recipe.WithoutIntegrate(workflow)
		workflow.MergePolicy.Mode = recipe.MergeModeNone
	}
	if !workflow.MergePolicy.Mode.Valid() {
		return workflow, nil, nil, fmt.Errorf("invalid adopted merge mode %q", workflow.MergePolicy.Mode)
	}
	hasExplicitClean := false
	for _, spec := range st.Config.Phases {
		switch strings.ToLower(strings.TrimSpace(spec.Name)) {
		case recipe.PhaseClean, "simplify":
			hasExplicitClean = true
		}
	}
	if len(st.Config.Phases) > 0 {
		workflow.Phases = nil
		for _, spec := range st.Config.Phases {
			canonical, isPolish := normalizeHostPhaseName(spec.Name, hasExplicitClean)
			ph, ok := defaultRecipe.Phase(canonical)
			if !ok {
				return workflow, nil, nil, fmt.Errorf("unsupported host phase %q", spec.Name)
			}
			if len(spec.Chain) > 0 {
				ph.Agents = nil
				for _, a := range spec.Chain {
					ph.Agents = append(ph.Agents, a.agent())
				}
			} else if spec.Model != "" {
				ph.Agents = []recipe.AgentSpec{spec.hostAgent.agent()}
			}
			promptFallback := spec.Prompt
			if isPolish && strings.TrimSpace(promptFallback) == "" {
				promptFallback = "Run /pr-polish-nebula."
			}
			if r.Format == FormatNebulaSwarm && (ph.Name == recipe.PhaseClean || ph.Name == recipe.PhaseReview) {
				ph.BriefTemplate = nebulaSwarmPhaseBrief(r.Dir, ph.Name, promptFallback)
			} else if strings.TrimSpace(promptFallback) != "" {
				ph.BriefTemplate = strings.TrimSpace(promptFallback)
			}
			workflow.Phases = append(workflow.Phases, ph)
		}
	}
	if workflow.MergePolicy.Mode == recipe.MergeModeSelf && mergesAfterCleanOrPolish(ruleText) {
		hasReviewLane := false
		for i := range r.Lanes {
			norm, _ := normalizeHostPhaseName(r.Lanes[i].Phase, hasExplicitClean)
			if norm == recipe.PhaseReview {
				hasReviewLane = true
				break
			}
		}
		if !hasReviewLane {
			if _, hasClean := workflow.Phase(recipe.PhaseClean); hasClean {
				workflow.Phases = slices.DeleteFunc(workflow.Phases, func(ph recipe.Phase) bool {
					return ph.Name == recipe.PhaseReview
				})
				if strings.Contains(strings.ToLower(ruleText), "pr-polish") {
					for i := range workflow.Phases {
						if workflow.Phases[i].Name == recipe.PhaseClean && strings.Contains(workflow.Phases[i].BriefTemplate, "/simplify-branch") {
							workflow.Phases[i].BriefTemplate = "Run /pr-polish-nebula."
						}
					}
				}
			}
		}
	}
	if workflow.MergePolicy.Mode != recipe.MergeModeController {
		var phases []recipe.Phase
		for _, ph := range workflow.Phases {
			if ph.Name != recipe.PhaseIntegrate {
				phases = append(phases, ph)
			}
		}
		workflow.Phases = phases
	} else if _, ok := workflow.Phase(recipe.PhaseIntegrate); !ok {
		ph, _ := defaultRecipe.Phase(recipe.PhaseIntegrate)
		workflow.Phases = append(workflow.Phases, ph)
	}
	if _, hasReview := workflow.Phase(recipe.PhaseReview); !hasReview {
		workflow.MergePolicy.RequireReviewPass = false
	}
	workflow.Transitions = nil
	for i := range workflow.Phases {
		ph := &workflow.Phases[i]
		to := recipe.TargetComplete
		if i+1 < len(workflow.Phases) {
			to = workflow.Phases[i+1].Name
		}
		workflow.Transitions = append(workflow.Transitions, recipe.Transition{From: ph.Name, To: to, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: to == recipe.PhaseIntegrate && workflow.MergePolicy.RequireCIGreen}})
		if ph.Name == recipe.PhaseReview || ph.Name == recipe.PhaseIntegrate {
			rework := recipe.Condition{Verdict: recipe.OutcomeNeedsSWE}
			if ph.Name == recipe.PhaseReview {
				// Keep adopted runs on the same review -> SWE round budget as
				// lanes spawned by orca. The ledger's imported round is the
				// current round and must be used by this condition.
				rework.MaxRound = 2
			}
			workflow.Transitions = append(workflow.Transitions, recipe.Transition{From: ph.Name, To: recipe.PhaseSWE, On: rework, AdvanceRound: true})
			if ph.Name == recipe.PhaseReview {
				workflow.Transitions = append(workflow.Transitions, recipe.Transition{From: ph.Name, To: recipe.TargetEscalate, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 3}})
			}
		}
		if ph.Name == recipe.PhaseIntegrate {
			ph.BriefTemplate += fmt.Sprintf("\nRun merge policy: mode=%s; target=%s; script=%s; require_review_pass=%t; require_ci_green=%t. %s", workflow.MergePolicy.Mode, workflow.MergePolicy.TargetBranch, workflow.MergePolicy.Script, workflow.MergePolicy.RequireReviewPass, workflow.MergePolicy.RequireCIGreen, st.Config.MergeRule)
			if r.NoPR {
				ph.BriefTemplate += "\nThis run uses branches without PRs; integrate the reviewed branch into the run target."
				ph.ExitContract.RequiresPR = false
			}
		}
	}
	var lanes []lane.Lane
	var items []store.BacklogItem
	for i := range r.Lanes {
		imported := &r.Lanes[i]
		if err := validatePathComponent("lane id", imported.ID); err != nil {
			return workflow, nil, nil, err
		}
		if imported.Phase != "" {
			if err := validatePathComponent("lane phase", imported.Phase); err != nil {
				return workflow, nil, nil, err
			}
		}
		status := lane.Status(imported.Status)
		if imported.Status == nsNeedsHuman {
			// A nebula-swarm lane parked for its owner; checkpoint writes it back.
			status = lane.StatusBlocked
		}
		terminal := integratedLane(*imported)
		if terminal {
			// Imported merge/close facts are authoritative even when a host
			// ledger's status or hold marker is stale. Terminal lanes are never
			// reopened by adoption.
			status = lane.StatusDone
		}
		// Host-owned blocks and draft PRs are imported as blocked so every
		// controller tick treats them as judgment-only. An owner/dispatcher
		// answer can explicitly move the lane back to running.
		hostHoldReason := strings.TrimSpace(imported.HostHold)
		if hostHoldReason == "" && !finishedAnalysisLane(*imported) && strings.EqualFold(previewOutcome(imported.Report), string(lane.OutcomeBlocked)) {
			hostHoldReason = strings.TrimSpace(imported.ReportBlocker)
		}
		if hostHoldReason == "" && imported.PRDraft {
			hostHoldReason = "PR is draft"
		}
		if hostHoldReason == "" && strings.EqualFold(imported.Status, "blocked") {
			hostHoldReason = "blocked"
		}
		if terminal {
			hostHoldReason = ""
		}
		if hostHoldReason != "" {
			status = lane.StatusBlocked
		}
		if !status.Valid() {
			return workflow, nil, nil, fmt.Errorf("lane %s: unsupported host status %q", imported.ID, imported.Status)
		}
		ln := lane.Lane{ItemID: imported.ID, ID: imported.ID, RunID: r.ID, Title: imported.Title, Branch: imported.Branch, Status: status, RecipeName: workflow.Name, RecipeVersion: 1, BaseBranch: st.Config.Base, Attempt: 1, TerminalState: imported.TerminalState, HostHoldReason: hostHoldReason,
			PRHead: imported.Head, MergeSHA: imported.MergeSHA, Checks: imported.Checks}
		ln.PR, _ = strconv.Atoi(imported.PR)
		for j := range st.Tasks {
			task := &st.Tasks[j]

			if task.ID == ln.ID {
				mission, err := hostMission(r.Dir, task.ID, task.Brief)
				if err != nil {
					return workflow, nil, nil, err
				}
				priority := task.Priority
				if !priority.Valid() {
					priority = store.PriorityP2
				}
				status := store.BacklogInProgress
				switch ln.Status {
				case lane.StatusDone:
					status = store.BacklogDone
				case lane.StatusFailed:
					status = store.BacklogDropped
				case lane.StatusPlanned:
					status = store.BacklogOpen
				case lane.StatusBlocked:
					status = store.BacklogBlocked
				}
				noMerge := store.IsNoMergeText(mission) || store.IsNoMergeText(ln.Title) || store.IsNoMergeText(task.Brief)
				items = append(items, store.BacklogItem{ID: ln.ID, RunID: r.ID, LaneID: ln.ID, Title: ln.Title, Description: mission, Priority: priority, Status: status, DependsOn: task.DependsOn, NoMerge: noMerge})
				ln.PRHead, ln.ApprovalSHA, ln.MergeSHA, ln.Checks, ln.BaseCommit, ln.Worktree = firstNonEmpty(task.PRHead, ln.PRHead), task.ApprovalSHA, firstNonEmpty(task.MergeSHA, ln.MergeSHA), firstNonEmpty(task.Checks, ln.Checks), task.BaseCommit, task.Worktree
				for phase, rounds := range task.Terminals {
					normPhase, _ := normalizeHostPhaseName(phase, hasExplicitClean)
					for round, id := range rounds {
						n, err := strconv.Atoi(round)
						if err != nil || id == "" {
							continue
						}
						if ln.Terminals == nil {
							ln.Terminals = map[string]string{}
						}
						ln.Terminals[lane.PhaseRoundKey(normPhase, n)] = id
					}
				}
				for key, terminal := range task.Sessions {
					if id, ok := strings.CutPrefix(terminal, "nebula:"); ok {
						if ln.Terminals == nil {
							ln.Terminals = map[string]string{}
						}
						ln.Terminals[key] = id
					}
				}
			}
		}
		normImportedPhase, _ := normalizeHostPhaseName(imported.Phase, hasExplicitClean)
		ln.RecordAttempt(normImportedPhase, imported.Round, imported.Session, imported.Terminal)
		lanes = append(lanes, ln)
	}
	return workflow, lanes, items, nil
}

func hostMission(dir, id, brief string) (string, error) {
	if err := validatePathComponent("task id", id); err != nil {
		return "", err
	}
	paths := []string{filepath.Join(dir, "missions", id+".md"), filepath.Join(dir, "briefs", id+".mission.md"),
		// nebula-swarm keeps the lane's assignment as its swe brief or prompt.
		filepath.Join(dir, id+".swe.brief"), filepath.Join(dir, id+".swe.prompt")}
	if brief != "" {
		resolved, err := resolveBriefPath(dir, brief)
		if err != nil {
			return "", err
		}
		paths = append(paths, resolved)
	}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err == nil {
			return inlineRunFileCitations(dir, id, string(b)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if strings.TrimSpace(brief) != "" {
		return inlineRunFileCitations(dir, id, brief), nil
	}
	if obj := stripMarker(readText(filepath.Join(dir, "OBJECTIVE.md"))); obj != "" && containsWord(obj, id) {
		return obj, nil
	}
	return "", nil
}

// validatePathComponent keeps ledger-controlled names from escaping the run
// directory when they are used to construct mission and spawn-file paths.
func validatePathComponent(label, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("%s %q must be a single path component inside the run directory", label, value)
	}
	return nil
}

// resolveBriefPath permits imported task briefs and close-out targets only when
// their lexical and resolved paths (including every existing parent directory
// and symlink component) remain inside the host run directory. Ledger data is
// untrusted: an absolute path, symlink escape, or traversal must never escape dir.
func resolveBriefPath(dir, brief string) (string, error) {
	if filepath.IsAbs(brief) || strings.HasPrefix(brief, "/") || strings.HasPrefix(brief, `\`) {
		return "", fmt.Errorf("brief path %q must be inside the run directory", brief)
	}
	clean := filepath.Clean(brief)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("brief path %q escapes the run directory", brief)
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve run directory for brief path %q: %w", brief, err)
	}
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve run directory for brief path %q: %w", brief, err)
	}
	parts := strings.Split(filepath.ToSlash(clean), "/")
	cur := resolvedBase
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", fmt.Errorf("brief path %q escapes the run directory", brief)
		}
		next := filepath.Join(cur, part)
		info, lerr := os.Lstat(next)
		if lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) {
				rem := append([]string{cur}, parts[i:]...)
				return filepath.Join(rem...), nil
			}
			return "", fmt.Errorf("resolve brief path %q: %w", brief, lerr)
		}
		resolvedNext, evalErr := filepath.EvalSymlinks(next)
		if evalErr != nil {
			return "", fmt.Errorf("brief path %q escapes the run directory: %w", brief, evalErr)
		}
		rel, relErr := filepath.Rel(resolvedBase, resolvedNext)
		if relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("brief path %q escapes the run directory", brief)
		}
		if i+1 < len(parts) && !info.IsDir() {
			tInfo, tErr := os.Stat(resolvedNext)
			if tErr != nil || !tInfo.IsDir() {
				return "", fmt.Errorf("brief path %q parent %q is not a directory", brief, part)
			}
		}
		cur = resolvedNext
	}
	return cur, nil
}

type hostAgent struct {
	Kind   string `json:"kind"`
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}
type hostPhase struct {
	hostAgent
	Name   string      `json:"name"`
	Prompt string      `json:"prompt"`
	Chain  []hostAgent `json:"chain"`
}

func (p *hostPhase) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		*p = hostPhase{Name: strings.TrimSpace(name)}
		return nil
	}
	type rawHostPhase hostPhase
	var raw rawHostPhase
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = hostPhase(raw)
	return nil
}

func normalizeHostPhaseName(name string, hasExplicitClean bool) (string, bool) {
	trimmed := strings.TrimSpace(name)
	switch strings.ToLower(trimmed) {
	case "":
		return "", false
	case recipe.PhaseSWE, "implement", "code":
		return recipe.PhaseSWE, false
	case recipe.PhaseClean, "simplify":
		return recipe.PhaseClean, false
	case recipe.PhaseReview:
		return recipe.PhaseReview, false
	case recipe.PhaseIntegrate, "merge":
		return recipe.PhaseIntegrate, false
	case "pr-polish", "pr_polish", "polish":
		if hasExplicitClean {
			return recipe.PhaseReview, true
		}
		return recipe.PhaseClean, true
	default:
		return trimmed, false
	}
}

var explicitMergeRuleLineRE = regexp.MustCompile(`(?i)^\s*(?:[-*+]\s*)?(?:merge(?:\s+rule|\s+rules|\s+policy|\s+mode|_rule|_policy|_mode)?|self[-_\s]?merge)\s*[:=]\s*(.+)$`)

func isMergeSectionHeading(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "#") {
		return false
	}
	heading := strings.ToLower(strings.TrimSpace(strings.TrimLeft(trimmed, "#")))
	heading = strings.TrimSuffix(heading, ":")
	switch heading {
	case "merge", "merge rule", "merge rules", "merge policy", "merge mode":
		return true
	default:
		return false
	}
}

func extractMergeRuleText(configMergeRule, objectiveText string) string {
	var parts []string
	if trimmed := strings.TrimSpace(configMergeRule); trimmed != "" {
		parts = append(parts, trimmed)
	}
	inMergeSection := false
	for _, line := range strings.Split(objectiveText, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			inMergeSection = isMergeSectionHeading(trimmed)
			continue
		}
		if m := explicitMergeRuleLineRE.FindStringSubmatch(trimmed); len(m) == 2 {
			parts = append(parts, trimmed)
			continue
		}
		if inMergeSection {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, "\n")
}

func splitMergeRuleClauses(s string) []string {
	r := strings.NewReplacer(
		"\n", "|",
		".", "|",
		";", "|",
		"!", "|",
		",", "|",
		" — ", "|",
		" – ", "|",
		" and ", "|",
		" then ", "|",
		" but ", "|",
	)
	return strings.Split(r.Replace(s), "|")
}

func isNegatedMergeRuleClause(clause string) bool {
	norm := adoptNormalizeContractions(clause)
	for _, neg := range []string{"not", "no", "never", "dont", "cannot", "cant", "forbid", "disable", "disabled", "off", "false", "none", "manual"} {
		if adoptContainsWholeWord(norm, neg) {
			return true
		}
	}
	return false
}

func hasSelfMergeRule(text string) bool {
	lower := strings.ToLower(text)
	for _, clause := range splitMergeRuleClauses(lower) {
		clause = strings.TrimSpace(clause)
		if clause == "" || isNegatedMergeRuleClause(clause) {
			continue
		}
		for _, phrase := range []string{
			"lanes self-merge",
			"lane self-merges",
			"merge themselves",
			"merges itself",
			"merge itself",
			"merges its own pr",
			"merge its own pr",
			"lanes merge their own",
			"lane merges its own",
			"lane merges once",
			"lanes merge once",
			"merges once pr-polish",
			"merge once pr-polish",
			"merges after pr-polish",
			"merge after pr-polish",
			"merges once clean",
			"merge once clean",
			"merges after clean",
			"merge after clean",
			"merge_mode=self",
			"merge mode: self",
			"merge: self",
			"merge rule: self-merge",
			"merge policy: self-merge",
		} {
			if strings.Contains(clause, phrase) {
				return true
			}
		}
	}
	return false
}

func mergesAfterCleanOrPolish(text string) bool {
	lower := strings.ToLower(text)
	for _, clause := range splitMergeRuleClauses(lower) {
		clause = strings.TrimSpace(clause)
		if clause == "" || isNegatedMergeRuleClause(clause) {
			continue
		}
		for _, phrase := range []string{
			"merges once pr-polish",
			"merge once pr-polish",
			"merges after pr-polish",
			"merge after pr-polish",
			"merges once clean",
			"merge once clean",
			"merges after clean",
			"merge after clean",
		} {
			if strings.Contains(clause, phrase) {
				return true
			}
		}
	}
	return false
}

func (a hostAgent) agent() recipe.AgentSpec {
	kind := cmp.Or(strings.TrimSpace(a.Kind), strings.TrimSpace(a.Agent))
	if kind == "" {
		kind = dispatcher.ResolveWorkerHarness(a.Model, "claude")
	}
	return recipe.AgentSpec{Harness: kind, Model: a.Model, Effort: a.Effort}
}

func nebulaSwarmPhaseBrief(dir, phase, specPrompt string) string {
	if p := strings.TrimSpace(specPrompt); p != "" {
		return p
	}
	if b := strings.TrimSpace(readText(filepath.Join(dir, phase+".brief"))); b != "" {
		return b
	}
	switch phase {
	case recipe.PhaseClean:
		return "Run /simplify-branch. If that skill command is unavailable, read .claude/skills/simplify-branch/SKILL.md and follow it."
	case recipe.PhaseReview:
		return "Run /pr-polish-nebula."
	default:
		return ""
	}
}

//nolint:govet // field alignment
type adoptedRunDriver struct {
	lane.LaneDriver
	clock  clock.Clock
	store  store.Store
	syncer *runLedgerSync
	run    Run
}

func (d *adoptedRunDriver) PhaseSkill(phase string) string {
	if d.run.Format == FormatNebulaSwarm && (phase == recipe.PhaseClean || phase == recipe.PhaseReview) {
		return ""
	}
	return d.LaneDriver.PhaseSkill(phase)
}

func (d *adoptedRunDriver) ReportInstructions(runID, laneID, phase string, round int) string {
	if d.run.Format != FormatNebulaSwarm {
		return d.LaneDriver.ReportInstructions(runID, laneID, phase, round)
	}
	r := max(round, 1)
	reportFile := filepath.Join(d.run.Dir, "reports", fmt.Sprintf("%s.%s.r%d.md", laneID, phase, r))
	return fmt.Sprintf(
		"Hand back: push your branch before reporting. Write the phase report for lane %s phase %s round %d to R=\"$(git rev-parse --git-dir)/phase-report.$NEBULA_TERMINAL_ID.md\" (and/or %s) with headers:\n"+
			"Outcome: done | no-change | needs-swe | blocked | needs-input\n"+
			"HEAD: <pushed commit sha>\n"+
			"PR: #<number>\n"+
			"Verdict: pass | needs-changes\n"+
			"Then file it with:\n"+
			"nebula phase report --outcome <outcome> [--pr <n>] --file \"$R\"\n"+
			"The phase is not finished until the phase report is filed.",
		laneID, phase, r, reportFile,
	)
}

func (d *adoptedRunDriver) SpawnSession(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	repoID, repo := d.resolveRepository(ctx, req.RepositoryID, req.Repository)
	if repoID == "" && repo == "" {
		return lane.SpawnResult{}, fmt.Errorf("spawn lane %s for adopted run %s: repository is not configured", req.LaneID, cmp.Or(req.RunID, d.run.ID))
	}
	req.RepositoryID = repoID
	req.Repository = cmp.Or(repo, repoID)
	if req.RunDir == "" {
		req.RunDir = d.run.Dir
	}
	req.Brief = d.expandRunBrief(ctx, req.LaneID, req.Phase, req.Brief)
	return d.LaneDriver.SpawnSession(ctx, req)
}

func (d *adoptedRunDriver) StartPhaseTerminal(ctx context.Context, req lane.StartPhaseRequest) (lane.StartPhaseResult, error) {
	repoID, repo := d.resolveRepository(ctx, req.RepositoryID, req.Repository)
	if strings.TrimSpace(req.SessionID) == "" && repoID == "" && repo == "" {
		return lane.StartPhaseResult{}, fmt.Errorf("start phase %s on lane %s for adopted run %s: repository is not configured", req.Phase, req.LaneID, cmp.Or(req.RunID, d.run.ID))
	}
	req.RepositoryID = repoID
	req.Repository = cmp.Or(repo, repoID)
	if req.RunDir == "" {
		req.RunDir = d.run.Dir
	}
	req.Brief = d.expandRunBrief(ctx, req.LaneID, req.Phase, req.Brief)
	return d.LaneDriver.StartPhaseTerminal(ctx, req)
}

func populateRunRepo(r *Run) {
	if r == nil {
		return
	}
	sanitizeRunRepo(r)
	if r.Dir == "" {
		return
	}
	t1RepoID, t1Repo, originRepo, _ := readTier1AndOriginRepo(r.Dir)
	if t1Repo != "" || t1RepoID != "" {
		if t1Repo != "" {
			r.Repo = t1Repo
		}
		if t1RepoID != "" {
			r.RepoID = t1RepoID
		}
		return
	}
	if r.Repo == "" && r.RepoID == "" && originRepo != "" {
		r.Repo = originRepo
	}
}

func resolveStoreRepository(ctx context.Context, st store.Store, runID, ignoreOriginRepo string) string {
	if st == nil || st.Runs() == nil {
		return ""
	}
	check := func(id string, skipOrigin bool) string {
		if strings.TrimSpace(id) == "" {
			return ""
		}
		sr, err := st.Runs().Get(ctx, id)
		if err != nil || sr == nil {
			return ""
		}
		repo := strings.TrimSpace(sr.Repository)
		if repo == "" || isLocalRepoPath(repo) {
			return ""
		}
		if skipOrigin && ignoreOriginRepo != "" && repo == ignoreOriginRepo {
			return ""
		}
		if isRepoSlug(repo) || isRepoID(repo) {
			return repo
		}
		return ""
	}
	if v := check(runID, true); v != "" {
		return v
	}
	if runID != "front-desk" {
		if v := check("front-desk", false); v != "" {
			return v
		}
	}
	return ""
}

func (d *adoptedRunDriver) resolveRepository(ctx context.Context, reqRepoID, reqRepo string) (repoID, repo string) {
	tmp := d.run
	sanitizeRunRepo(&tmp)
	populateRunRepo(&tmp)
	_, _, originRepo, hasTier1 := readTier1AndOriginRepo(tmp.Dir)
	if hasTier1 {
		applyRepoValue(&tmp, reqRepoID)
		applyRepoValue(&tmp, reqRepo)
		if tmp.Repo == "" && tmp.RepoID != "" {
			tmp.Repo = tmp.RepoID
		}
		return tmp.RepoID, tmp.Repo
	}

	var tier2 Run
	if originRepo == "" || strings.TrimSpace(reqRepoID) != originRepo {
		applyRepoValue(&tier2, reqRepoID)
	}
	if originRepo == "" || strings.TrimSpace(reqRepo) != originRepo {
		applyRepoValue(&tier2, reqRepo)
	}
	if tier2.Repo == "" && tier2.RepoID == "" {
		if storeRepo := resolveStoreRepository(ctx, d.store, tmp.ID, originRepo); storeRepo != "" {
			applyRepoValue(&tier2, storeRepo)
		}
	}
	if tier2.Repo != "" || tier2.RepoID != "" {
		if tier2.Repo == "" && tier2.RepoID != "" {
			tier2.Repo = tier2.RepoID
		}
		return tier2.RepoID, tier2.Repo
	}

	if tmp.Repo == "" && tmp.RepoID != "" {
		tmp.Repo = tmp.RepoID
	}
	return tmp.RepoID, tmp.Repo
}

func (d *adoptedRunDriver) expandRunBrief(ctx context.Context, laneID, phase, brief string) string {
	body, tail := splitReportTail(brief)
	out := strings.ReplaceAll(body, "@LANE@", laneID)
	if d.run.Format == FormatNebulaSwarm && validatePathComponent("lane id", laneID) == nil && validatePathComponent("phase", phase) == nil {
		for _, name := range []string{laneID + "." + phase + ".brief", laneID + "." + phase + ".prompt"} {
			if custom := strings.TrimSpace(readText(filepath.Join(d.run.Dir, name))); custom != "" && !strings.Contains(out, custom) {
				out = custom + "\n\n" + out
				break
			}
		}
	}
	if d.store != nil {
		if ln, err := d.store.Lanes().Get(ctx, d.run.ID, laneID); err == nil && ln != nil {
			out = strings.ReplaceAll(out, "@BRANCH@", ln.Branch)
			if d.run.Format == FormatNebulaSwarm && phase == recipe.PhaseReview && ln.PR > 0 {
				prTag := fmt.Sprintf("PR #%d", ln.PR)
				if !strings.Contains(out, prTag) {
					out = fmt.Sprintf("Review lane %s (%s).\n\n%s", laneID, prTag, out)
				}
			}
		}
	}
	out = inlineRunFileCitations(d.run.Dir, laneID, out)
	return out + tail
}

func splitReportTail(brief string) (body, tail string) {
	for _, marker := range []string{"\n\nHand back:", "\n\nFile the phase report for lane "} {
		if idx := strings.LastIndex(brief, marker); idx >= 0 {
			return brief[:idx], brief[idx:]
		}
	}
	return brief, ""
}

const maxInlinedRunFileBytes = 16384

func clipRunFileContent(content string, maxBytes int) string {
	s := strings.TrimSpace(content)
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if nl := strings.LastIndexByte(s[:cut], '\n'); nl >= maxBytes/2 {
		cut = nl
	}
	return fmt.Sprintf("%s\n... (truncated %d bytes)", strings.TrimRight(s[:cut], " \t\r\n"), len(s)-cut)
}

func inlineRunFileCitations(dir, laneID, brief string) string {
	if dir == "" || strings.TrimSpace(brief) == "" {
		return brief
	}
	candidates := []string{
		"OBJECTIVE.md",
		"PROGRESS.md",
		"ESCALATIONS.md",
		"ledger.md",
	}
	if validatePathComponent("lane id", laneID) == nil {
		candidates = append(candidates,
			filepath.Join("missions", laneID+".md"),
			filepath.Join("briefs", laneID+".mission.md"),
			laneID+".swe.brief",
			laneID+".swe.prompt",
		)
	}

	out := brief
	lowerBrief := strings.ToLower(brief)
	for _, rel := range candidates {
		baseName := filepath.Base(rel)
		if !strings.Contains(lowerBrief, strings.ToLower(rel)) && !strings.Contains(lowerBrief, strings.ToLower(baseName)) {
			continue
		}
		resolved, err := resolveBriefPath(dir, rel)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() {
			continue
		}
		content := clipRunFileContent(stripMarker(readText(resolved)), maxInlinedRunFileBytes)
		header := fmt.Sprintf("Inlined from %s:", rel)
		if content == "" || strings.Contains(out, header) || strings.Contains(out, content) {
			continue
		}
		out += fmt.Sprintf("\n\n%s\n%s", header, content)
	}
	return out
}

// An adopted expected wait acknowledges the imported report only. Fresh
// handbacks must still pass through, even while the host deadline is pending.
func (d *adoptedRunDriver) hostAnsweredReport(ctx context.Context, runID, laneID string, rep *lane.PhaseReport) bool {
	if rep == nil || (rep.Outcome != lane.OutcomeNeedsInput && rep.Outcome != lane.OutcomeBlocked) || d.store == nil {
		return false
	}
	var original string
	for i := range d.run.Lanes {
		if d.run.Lanes[i].ID == laneID {
			original = d.run.Lanes[i].reportText
			break
		}
	}
	if original == "" || strings.TrimSpace(rep.Raw) != original {
		return false
	}
	wait, err := d.store.Directives().Get(ctx, runID, "wait-adopt-"+laneID)
	return err == nil && wait.Scope == laneID && wait.IsActive(d.clock.Now().UTC())
}

func (d *adoptedRunDriver) Watch(ctx context.Context, runID string) <-chan lane.Event {
	in := d.LaneDriver.Watch(ctx, runID)
	out := make(chan lane.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-in:
				if !ok {
					return
				}
				if event.Kind == lane.EventPhaseReported && d.hostAnsweredReport(ctx, runID, event.LaneID, event.Report) {
					continue
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func (d *adoptedRunDriver) ReadReport(ctx context.Context, runID, laneID, terminalID string) (*lane.PhaseReport, error) {
	rep, err := d.LaneDriver.ReadReport(ctx, runID, laneID, terminalID)
	if err == nil && rep != nil && rep.Outcome.Valid() {
		if d.hostAnsweredReport(ctx, runID, laneID, rep) {
			return nil, nil
		}
		return rep, nil
	}
	if fileRep := d.readRunReportFile(ctx, runID, laneID, terminalID); fileRep != nil {
		if d.hostAnsweredReport(ctx, runID, laneID, fileRep) {
			return nil, nil
		}
		return fileRep, nil
	}
	return rep, err
}

func (d *adoptedRunDriver) readRunReportFile(ctx context.Context, runID, laneID, terminalID string) *lane.PhaseReport {
	if validatePathComponent("lane id", laneID) != nil {
		return nil
	}
	var phase, worktree string
	round := 1
	if d.store != nil {
		if ln, err := d.store.Lanes().Get(ctx, runID, laneID); err == nil && ln != nil {
			phase = ln.Phase
			round = max(ln.Round, 1)
			worktree = ln.Worktree
			for k, tid := range ln.Terminals {
				if tid == terminalID {
					p, r := lane.SplitPhaseRound(k)
					if p != "" {
						phase, round = p, max(r, 1)
					}
					break
				}
			}
		}
	}
	if phase == "" {
		for i := range d.run.Lanes {
			if d.run.Lanes[i].ID == laneID {
				phase = d.run.Lanes[i].Phase
				round = max(d.run.Lanes[i].Round, 1)
				break
			}
		}
	}
	var candidates []string
	if worktree != "" && terminalID != "" && validatePathComponent("terminal id", terminalID) == nil {
		candidates = append(candidates, filepath.Join(worktree, ".git", "phase-report."+terminalID+".md"))
	}
	if phase != "" && validatePathComponent("phase", phase) == nil {
		candidates = append(candidates,
			filepath.Join(d.run.Dir, "reports", fmt.Sprintf("%s.%s.r%d.md", laneID, phase, round)),
			filepath.Join(d.run.Dir, fmt.Sprintf("%s.%s.r%d.md", laneID, phase, round)),
			filepath.Join(d.run.Dir, "reports", fmt.Sprintf("%s.%s.md", laneID, phase)),
			filepath.Join(d.run.Dir, fmt.Sprintf("%s.%s.md", laneID, phase)),
			filepath.Join(d.run.Dir, "reports", fmt.Sprintf("%s.%s.done", laneID, phase)),
			filepath.Join(d.run.Dir, fmt.Sprintf("%s.%s.done", laneID, phase)),
		)
	}
	for _, p := range candidates {
		raw := strings.TrimSpace(readText(p))
		if raw == "" {
			continue
		}
		rep, err := lane.ParsePhaseReport(raw)
		if err != nil || rep == nil || !rep.Outcome.Valid() {
			continue
		}
		rep.LaneID = cmp.Or(rep.LaneID, laneID)
		rep.TerminalID = cmp.Or(rep.TerminalID, terminalID)
		rep.Phase = cmp.Or(rep.Phase, phase)
		if rep.Round == 0 {
			rep.Round = round
		}
		return rep
	}
	return nil
}

func (d *adoptedRunDriver) ReleaseSession(ctx context.Context, sessionID string, force ...bool) error {
	if rel, ok := d.LaneDriver.(lane.SessionReleaser); ok {
		if err := rel.ReleaseSession(ctx, sessionID, force...); err != nil {
			return err
		}
		if d.syncer != nil {
			d.syncer.MarkSessionReleased(sessionID)
		}
	}
	return nil
}

func (d *adoptedRunDriver) UpdatePost(ctx context.Context, postID, content string) error {
	if pub, ok := d.LaneDriver.(lane.PostPublisher); ok {
		return pub.UpdatePost(ctx, postID, content)
	}
	return fmt.Errorf("republish post %s: driver does not support post updates", postID)
}

func (d *adoptedRunDriver) ReportOnlyNoWork(ctx context.Context, ln lane.Lane) (bool, error) {
	if rc, ok := d.LaneDriver.(lane.ReportOnlyChecker); ok {
		return rc.ReportOnlyNoWork(ctx, ln)
	}
	return false, nil
}

func (d *adoptedRunDriver) TerminalAgentState(ctx context.Context, terminalID string) (string, error) {
	if tr, ok := d.LaneDriver.(lane.TerminalStateReader); ok {
		return tr.TerminalAgentState(ctx, terminalID)
	}
	return "", nil
}

func (d *adoptedRunDriver) LaneGitState(ctx context.Context, ln lane.Lane) (lane.GitState, error) {
	if gr, ok := d.LaneDriver.(lane.GitStateReader); ok {
		return gr.LaneGitState(ctx, ln)
	}
	return lane.GitState{}, nil
}

func (d *adoptedRunDriver) LaneSessionHealth(ctx context.Context, ln lane.Lane) (lane.SessionHealth, error) {
	if hr, ok := d.LaneDriver.(lane.SessionHealthReader); ok {
		return hr.LaneSessionHealth(ctx, ln)
	}
	return lane.SessionHealth{}, nil
}

func (d *adoptedRunDriver) TrackLane(ln lane.Lane) {
	if tl, ok := d.LaneDriver.(interface{ TrackLane(lane.Lane) }); ok {
		tl.TrackLane(ln)
	}
}

type adoptedStore struct {
	store.Store
	runs    store.RunStore
	lanes   store.LaneStore
	backlog store.BacklogStore
}

func (s *adoptedStore) Runs() store.RunStore {
	if s.runs != nil {
		return s.runs
	}
	return s.Store.Runs()
}

func (s *adoptedStore) Lanes() store.LaneStore { return s.lanes }

func (s *adoptedStore) Backlog() store.BacklogStore {
	if s.backlog != nil {
		return s.backlog
	}
	return s.Store.Backlog()
}

type adoptedRunStore struct {
	store.RunStore
	syncer *runLedgerSync
}

func (s *adoptedRunStore) Get(ctx context.Context, runID string) (*store.Run, error) {
	if s.syncer != nil && runID == s.syncer.runID {
		_, _ = s.syncer.RefreshObjectiveIfChanged(ctx, s.RunStore)
	}
	sr, err := s.RunStore.Get(ctx, runID)
	if err != nil || sr == nil {
		return sr, err
	}
	if isLocalRepoPath(sr.Repository) {
		sr.Repository = ""
	}
	if s.syncer != nil && runID == s.syncer.runID {
		cur := s.syncer.CurrentRun()
		_, _, _, hasTier1 := readTier1AndOriginRepo(cur.Dir)
		if hasTier1 {
			sr.Repository = cmp.Or(cur.Repo, cur.RepoID)
		} else if sr.Repository == "" {
			sr.Repository = cmp.Or(cur.Repo, cur.RepoID)
		}
	}
	return sr, nil
}

func (s *adoptedRunStore) List(ctx context.Context) ([]store.Run, error) {
	if s.syncer != nil {
		_, _ = s.syncer.RefreshObjectiveIfChanged(ctx, s.RunStore)
	}
	return s.RunStore.List(ctx)
}

type adoptedLaneStore struct {
	store.LaneStore
	syncer      *runLedgerSync
	controllers *Controllers
}

// Upsert checkpoints the lane live to the host ledger and, when the lane reaches
// a terminal status with no active backlog item, triggers planNextWorkIfDrained
// (controller.stepIntegrate marks merged lanes done directly without calling judge).
func (s *adoptedLaneStore) Upsert(ctx context.Context, ln lane.Lane) error {
	if s.syncer != nil && ln.RunID == s.syncer.runID {
		if err := s.syncer.SyncLane(ctx, ln); err != nil {
			slog.Warn("live checkpoint adopted lane failed", "run", ln.RunID, "lane", ln.ID, "error", err)
		}
	}
	if err := s.LaneStore.Upsert(ctx, ln); err != nil {
		return err
	}
	if ln.Status.Terminal() && s.controllers != nil {
		itemID := cmp.Or(ln.ItemID, ln.ID)
		hasActiveItem := false
		if itemID != "" {
			if item, err := s.controllers.store.Backlog().Get(ctx, ln.RunID, itemID); err == nil && !item.Status.Terminal() {
				hasActiveItem = true
			}
		}
		if !hasActiveItem {
			if _, err := s.controllers.planNextWorkIfDrained(ctx, ln.RunID, "lane_completed"); err != nil {
				slog.Warn("plan next work after lane completion failed", "run", ln.RunID, "lane", ln.ID, "error", err)
			}
		}
	}
	return nil
}

type adoptedBacklogStore struct {
	store.BacklogStore
	controllers *Controllers
}

// Close triggers planNextWorkIfDrained after closing a backlog item so merges
// handled directly by controller.stepIntegrate hand off remaining OBJECTIVE.md work.
func (s *adoptedBacklogStore) Close(ctx context.Context, runID, itemID string, status store.BacklogStatus, actor, reason string) error {
	if err := s.BacklogStore.Close(ctx, runID, itemID, status, actor, reason); err != nil {
		return err
	}
	if s.controllers != nil {
		if _, err := s.controllers.planNextWorkIfDrained(ctx, runID, "backlog_drained"); err != nil {
			slog.Warn("plan next work after backlog close failed", "run", runID, "item", itemID, "error", err)
		}
	}
	return nil
}

// runLedgerSync mirrors controller lane transitions and OBJECTIVE.md updates
// to an adopted host run directory.
//
// Lock ordering: closeoutMu serializes CloseoutRun execution per run; ioMu
// serializes disk and subprocess I/O in SyncLane; mu guards in-memory state
// (run, known, lastObjective, lastPlannedKey, stat signatures). Callers may
// acquire ioMu or mu while holding closeoutMu, and mu while holding ioMu,
// never the reverse, and mu is never held across file or subprocess I/O.
type runLedgerSync struct {
	clock            clock.Clock
	known            map[string]lane.Lane
	releasedSessions map[string]bool
	ledgerScript     *string
	runID            string
	resolvedScript   string
	lastObjective    string
	lastPlannedKey   string
	run              Run
	objModNano       int64
	objSize          int64
	progModNano      int64
	progSize         int64
	statReady        bool
	scriptResolved   bool
	closedOut        bool
	closeoutMu       sync.Mutex
	ioMu             sync.Mutex
	mu               sync.Mutex
}

func fullObjectiveWithHandover(r Run) string {
	full := r.FullObjective()
	if r.Dir == "" || extractCloseoutPostID(full) != "" {
		return full
	}
	handover := stripMarker(readText(filepath.Join(r.Dir, "HANDOVER.md")))
	if postID := extractCloseoutPostID(r.Progress, handover); postID != "" {
		if full != "" {
			full += "\n"
		}
		full += "Post: " + postID
	}
	return full
}

func fileStatSignature(path string) (int64, int64) {
	info, err := os.Stat(path)
	if err != nil {
		return -1, -1
	}
	return info.ModTime().UnixNano(), info.Size()
}

func newRunLedgerSync(r Run, clk clock.Clock, initial []lane.Lane) *runLedgerSync {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	known := make(map[string]lane.Lane, len(initial))
	for i := range initial {
		known[initial[i].ID] = initial[i].Clone()
	}
	objMod, objSize := fileStatSignature(filepath.Join(r.Dir, "OBJECTIVE.md"))
	progMod, progSize := fileStatSignature(filepath.Join(r.Dir, "PROGRESS.md"))
	return &runLedgerSync{
		runID:            r.ID,
		run:              r,
		clock:            clk,
		known:            known,
		releasedSessions: make(map[string]bool),
		lastObjective:    fullObjectiveWithHandover(r),
		objModNano:       objMod,
		objSize:          objSize,
		progModNano:      progMod,
		progSize:         progSize,
		statReady:        r.Dir != "",
	}
}

func (s *runLedgerSync) MarkSessionReleased(sessionID string) bool {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releasedSessions == nil {
		s.releasedSessions = make(map[string]bool)
	}
	if s.releasedSessions[sid] {
		return false
	}
	s.releasedSessions[sid] = true
	return true
}

func (s *runLedgerSync) ReleasedSessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.releasedSessions))
	for sid := range s.releasedSessions {
		out = append(out, sid)
	}
	slices.Sort(out)
	return out
}

func (s *runLedgerSync) MarkClosedOut() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closedOut {
		return false
	}
	s.closedOut = true
	return true
}

func (s *runLedgerSync) ClearClosedOut() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closedOut = false
}

func (s *runLedgerSync) CurrentRun() Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run
}

func (s *runLedgerSync) MarkPlanned(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPlannedKey == key {
		return false
	}
	s.lastPlannedKey = key
	return true
}

func (s *runLedgerSync) ClearPlanned(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPlannedKey == key {
		s.lastPlannedKey = ""
	}
}

func (s *runLedgerSync) RefreshObjectiveIfChanged(ctx context.Context, runs store.RunStore) (bool, error) {
	s.mu.Lock()
	dir := s.run.Dir
	ready := s.statReady
	prevObjMod, prevObjSize := s.objModNano, s.objSize
	prevProgMod, prevProgSize := s.progModNano, s.progSize
	s.mu.Unlock()
	if dir == "" || runs == nil {
		return false, nil
	}
	objMod, objSize := fileStatSignature(filepath.Join(dir, "OBJECTIVE.md"))
	progMod, progSize := fileStatSignature(filepath.Join(dir, "PROGRESS.md"))
	if ready && objMod == prevObjMod && objSize == prevObjSize && progMod == prevProgMod && progSize == prevProgSize {
		return false, nil
	}
	return s.RefreshObjective(ctx, runs)
}

func (s *runLedgerSync) RefreshObjective(ctx context.Context, runs store.RunStore) (bool, error) {
	s.mu.Lock()
	dir := s.run.Dir
	runID := s.run.ID
	goal := s.run.Goal
	prevObj := s.lastObjective
	s.mu.Unlock()
	if dir == "" || runs == nil {
		return false, nil
	}
	objPath := filepath.Join(dir, "OBJECTIVE.md")
	progPath := filepath.Join(dir, "PROGRESS.md")
	objMod, objSize := fileStatSignature(objPath)
	progMod, progSize := fileStatSignature(progPath)
	objText := stripMarker(readText(objPath))
	progText := clip(stripMarker(readText(progPath)), 1500)
	planPath := extractPlanPath(dir, objText, progText)
	nextSec := extractNextSection(objText)
	fullObj := fullObjectiveWithHandover(Run{
		Dir:       dir,
		Objective: objText,
		Progress:  progText,
		Goal:      goal,
	})
	s.mu.Lock()
	s.objModNano, s.objSize = objMod, objSize
	s.progModNano, s.progSize = progMod, progSize
	s.statReady = true
	updated := s.run
	updated.Objective = objText
	updated.Progress = progText
	if updated.Goal == "" {
		updated.Goal = goal
	}
	updated.PlanPath = planPath
	updated.Next = nextSec
	if fullObj == "" || fullObj == prevObj {
		s.mu.Unlock()
		return false, nil
	}
	s.run = updated
	s.lastObjective = fullObj
	s.mu.Unlock()

	cur, err := runs.Get(ctx, runID)
	if err != nil {
		return false, err
	}
	if err := runs.UpdateObjective(ctx, runID, fullObj, cur.PlanRef); err != nil {
		return false, err
	}
	return true, nil
}

func lanePersistedDiff(a, b lane.Lane) bool {
	return a.Phase != b.Phase ||
		a.Round != b.Round ||
		a.Status != b.Status ||
		a.SessionID != b.SessionID ||
		a.TerminalID != b.TerminalID ||
		a.PR != b.PR ||
		a.PRHead != b.PRHead ||
		a.ApprovalSHA != b.ApprovalSHA ||
		a.MergeSHA != b.MergeSHA ||
		a.Checks != b.Checks ||
		a.BaseCommit != b.BaseCommit
}

func (s *runLedgerSync) resolveLedgerScript() *string {
	s.mu.Lock()
	if s.ledgerScript != nil {
		override := s.ledgerScript
		s.mu.Unlock()
		return override
	}
	if s.scriptResolved {
		script := s.resolvedScript
		s.mu.Unlock()
		return &script
	}
	s.mu.Unlock()
	found := FindLedgerScript()
	s.mu.Lock()
	if !s.scriptResolved {
		s.resolvedScript = found
		s.scriptResolved = true
	}
	script := s.resolvedScript
	s.mu.Unlock()
	return &script
}

func (s *runLedgerSync) SyncLane(ctx context.Context, ln lane.Lane) error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()

	s.mu.Lock()
	prev, ok := s.known[ln.ID]
	run := s.run
	s.mu.Unlock()
	if ok && !lanePersistedDiff(prev, ln) {
		return nil
	}
	var prevPtr *lane.Lane
	if ok {
		cp := prev.Clone()
		prevPtr = &cp
	}
	if run.Format == FormatNebulaSwarm {
		cfgPhases, _, _, _ := readNebulaSwarmLedgerMeta(run.Dir)
		usedHelper, err := writeNebulaSwarmLaneWithHelper(ctx, run.Dir, ln, s.resolveLedgerScript())
		if err != nil {
			slog.Warn("nebula-swarm ledger.py helper failed; falling back to direct checkpoint", "dir", run.Dir, "lane", ln.ID, "error", err)
		}
		if err != nil || !usedHelper {
			if cErr := checkpoint(run.Dir, []lane.Lane{ln}); cErr != nil {
				return errors.Join(err, cErr)
			}
		}
		if err := appendNebulaSwarmEvents(run.Dir, s.clock.Now(), prevPtr, ln, cfgPhases); err != nil {
			return err
		}
	} else {
		if err := checkpoint(run.Dir, []lane.Lane{ln}); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.known[ln.ID] = ln.Clone()
	s.mu.Unlock()
	return nil
}

func (s *runLedgerSync) SyncAll(ctx context.Context, lanes []lane.Lane) error {
	var errs []error
	for i := range lanes {
		errs = append(errs, s.SyncLane(ctx, lanes[i]))
	}
	return errors.Join(errs...)
}

// lockLedgerFile acquires an exclusive flock on state.json.lock, retrying
// briefly under transient contention before returning an error.
func lockLedgerFile(lock *os.File) error {
	return lockLedgerFileWith(lock, nil)
}

func lockLedgerFileWith(lock *os.File, onContention func()) error {
	deadline := time.Now().Add(2 * time.Second)
	notified := false
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("lock host ledger: %w", err)
		}
		if !notified && onContention != nil {
			notified = true
			onContention()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("lock host ledger: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func checkpointSubagentSwarm(dir string, task map[string]json.RawMessage, ln *lane.Lane) error {
	fields := map[string]any{"phase": ln.Phase, "round": ln.Round, "status": ln.Status, "nebula_session": ln.SessionID, "nebula_terminal": ln.TerminalID, "pr": ln.PR, "pr_head": ln.PRHead, "approval_sha": ln.ApprovalSHA, "merge_sha": ln.MergeSHA, "checks": ln.Checks, "fork_sha": ln.BaseCommit}
	for k, v := range fields {
		task[k], _ = json.Marshal(v)
	}
	var sessions map[string]string
	if raw := task["sessions"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &sessions); err != nil {
			return err
		}
	}
	if sessions == nil {
		sessions = map[string]string{}
	}
	for k, v := range ln.Terminals {
		sessions[k] = "nebula:" + v
	}
	task["sessions"], _ = json.Marshal(sessions)
	if ln.Phase != "" && ln.TerminalID != "" {
		spawnPath := filepath.Join(dir, ln.ID+"."+ln.Phase+".spawn.json")
		sp := map[string]json.RawMessage{}
		if raw, err := os.ReadFile(spawnPath); err == nil {
			if err := json.Unmarshal(raw, &sp); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		sp["session_id"], _ = json.Marshal(ln.SessionID)
		sp["terminal_id"], _ = json.Marshal(ln.TerminalID)
		spawn, _ := json.Marshal(sp)
		if err := writeAtomic(spawnPath, spawn); err != nil {
			return err
		}
	}
	return nil
}

// checkpoint writes controller state back to the host ledger and spawn files.
func checkpoint(dir string, lanes []lane.Lane) error {
	lock, err := os.OpenFile(filepath.Join(dir, "state.json.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockLedgerFile(lock); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	path := filepath.Join(dir, "state.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	var tasks []map[string]json.RawMessage
	if err := json.Unmarshal(doc["tasks"], &tasks); err != nil {
		return err
	}
	var cfgPhases []string
	if rawCfg := doc["config"]; len(rawCfg) > 0 {
		var cfg struct {
			Phases []hostPhase `json:"phases"`
		}
		if json.Unmarshal(rawCfg, &cfg) == nil {
			for _, p := range cfg.Phases {
				if p.Name != "" {
					cfgPhases = append(cfgPhases, p.Name)
				}
			}
		}
	}
	nebulaSwarm := detectFormat(dir, b) == FormatNebulaSwarm
	for i := range lanes {
		ln := &lanes[i]
		if err := validatePathComponent("lane id", ln.ID); err != nil {
			return err
		}
		if ln.Phase != "" {
			if err := validatePathComponent("lane phase", ln.Phase); err != nil {
				return err
			}
		}
		found := false
		for _, task := range tasks {
			var id string
			if err := json.Unmarshal(task["id"], &id); err != nil {
				return err
			}
			if id != ln.ID {
				continue
			}
			found = true
			if nebulaSwarm {
				if err := checkpointNebulaSwarm(task, ln, cfgPhases); err != nil {
					return err
				}
				continue
			}
			if err := checkpointSubagentSwarm(dir, task, ln); err != nil {
				return err
			}
		}
		if !found {
			task := map[string]json.RawMessage{}
			task["id"], _ = json.Marshal(ln.ID)
			task["title"], _ = json.Marshal(cmp.Or(ln.Title, ln.ID))
			task["branch"], _ = json.Marshal(ln.Branch)
			if nebulaSwarm {
				task["priority"], _ = json.Marshal("p1")
				task["depends_on"], _ = json.Marshal([]string{})
				task["verdict"], _ = json.Marshal("")
				task["terminal_state"], _ = json.Marshal("")
				task["terminals"], _ = json.Marshal(map[string]map[string]string{})
				if err := checkpointNebulaSwarm(task, ln, cfgPhases); err != nil {
					return err
				}
			} else {
				if err := checkpointSubagentSwarm(dir, task, ln); err != nil {
					return err
				}
			}
			tasks = append(tasks, task)
		}
	}
	doc["tasks"], _ = json.Marshal(tasks)
	b, err = json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

// nsNeedsHuman is nebula-swarm's status of a lane parked for its owner.
const nsNeedsHuman = "needs-human"

// checkpointNebulaSwarm writes a lane back in nebula-swarm's ledger fields:
// its session, the terminal of its phase and round, and its PR state.
func checkpointNebulaSwarm(task map[string]json.RawMessage, ln *lane.Lane, cfgPhases []string) error {
	var prev, prevPhase, prevVerdict string
	_ = json.Unmarshal(task["status"], &prev)
	_ = json.Unmarshal(task["phase"], &prevPhase)
	_ = json.Unmarshal(task["verdict"], &prevVerdict)
	status := string(ln.Status)
	if prev == nsNeedsHuman && ln.Status == lane.StatusBlocked {
		status = nsNeedsHuman
	}
	effPhase := ledgerPhase(ln.Phase, cfgPhases, prevPhase)
	round := max(ln.Round, 1)
	fields := map[string]any{"phase": effPhase, "round": round, "status": status, "session_id": ln.SessionID, "pr": ln.PR, "pr_head": ln.PRHead, "approval_sha": ln.ApprovalSHA, "merge_sha": ln.MergeSHA, "checks": ln.Checks, "fork_sha": ln.BaseCommit}
	if ln.ApprovalSHA != "" && prevVerdict == "" {
		fields["verdict"] = "pass"
	}
	if ln.Status.Terminal() {
		fields["terminal_state"] = ""
	}
	for k, v := range fields {
		task[k], _ = json.Marshal(v)
	}
	terminals := map[string]map[string]string{}
	if raw := task["terminals"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &terminals); err != nil {
			return err
		}
	}
	if effPhase != "" && effPhase == ln.Phase && ln.TerminalID != "" {
		if terminals[effPhase] == nil {
			terminals[effPhase] = map[string]string{}
		}
		terminals[effPhase][strconv.Itoa(round)] = ln.TerminalID
	}
	task["terminals"], _ = json.Marshal(terminals)
	return nil
}

// CloseoutRun closes out an adopted run once every lane and backlog item is
// terminal: validates that all file writes stay inside the run directory and
// target files named in OBJECTIVE.md, releases remaining lane sessions, appends
// to the plan's log file when named in OBJECTIVE.md, republishes any Nebula post
// named in OBJECTIVE.md, writes FINAL.md, updates PROGRESS.md and OBJECTIVE.md's
// Next: line, and records one-line summaries for the owner with a /release offer.
func (d *Controllers) CloseoutRun(ctx context.Context, runID string, params dispatcher.RunCloseoutParams) (string, error) {
	cr := d.lookupRun(runID)
	if cr == nil || cr.syncer == nil {
		return "", fmt.Errorf("adopted run %q is not active", runID)
	}
	cr.syncer.closeoutMu.Lock()
	defer cr.syncer.closeoutMu.Unlock()

	curRun := cr.syncer.CurrentRun()
	if strings.TrimSpace(curRun.Dir) == "" {
		return "", fmt.Errorf("adopted run %q has no run directory", runID)
	}

	lanes, err := d.store.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		return "", err
	}
	slices.SortFunc(lanes, func(a, b lane.Lane) int {
		return strings.Compare(a.ID, b.ID)
	})
	items, err := d.store.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
	if err != nil {
		return "", err
	}
	if err := dispatcher.CheckRunTerminal(lanes, items, true); err != nil {
		return "", err
	}

	objPath := filepath.Join(curRun.Dir, "OBJECTIVE.md")
	progPath := filepath.Join(curRun.Dir, "PROGRESS.md")
	handoverPath := filepath.Join(curRun.Dir, "HANDOVER.md")
	rawObj := stripMarker(readText(objPath))
	if strings.TrimSpace(rawObj) == "" {
		rawObj = curRun.Objective
	}
	rawProg := stripMarker(readText(progPath))
	if strings.TrimSpace(rawProg) == "" {
		rawProg = curRun.Progress
	}
	handoverText := stripMarker(readText(handoverPath))
	var storedObj, storedPlanRef string
	if sr, gErr := d.store.Runs().Get(ctx, runID); gErr == nil && sr != nil {
		storedObj = sr.Objective
		storedPlanRef = sr.PlanRef
	}

	if strings.TrimSpace(params.PlanFile) != "" {
		if _, vErr := ValidateCloseoutWrite(curRun.Dir, rawObj, params.PlanFile); vErr != nil {
			return "", vErr
		}
	}
	fileKeys := make([]string, 0, len(params.Files))
	resolvedFiles := make(map[string]string, len(params.Files))
	for k := range params.Files {
		resolvedTarget, vErr := ValidateCloseoutWrite(curRun.Dir, rawObj, k)
		if vErr != nil {
			return "", vErr
		}
		resolvedFiles[k] = resolvedTarget
		fileKeys = append(fileKeys, k)
	}
	slices.Sort(fileKeys)

	if err := dispatcher.ValidateCloseoutPostID(params.PostID, rawObj, storedObj, rawProg, handoverText, storedPlanRef); err != nil {
		return "", err
	}

	now := d.clock.Now().UTC()
	ts := now.Format(time.RFC3339)

	var prRefs []string
	var prLines []string
	doneLanes := 0
	failedLanes := 0
	for i := range lanes {
		ln := lanes[i]
		if ln.Status == lane.StatusDone {
			doneLanes++
		} else {
			failedLanes++
		}
		isMerged := ln.MergeSHA != "" || (ln.Status == lane.StatusDone && ln.PR > 0)
		if isMerged && ln.PR > 0 {
			prRefs = append(prRefs, fmt.Sprintf("#%d", ln.PR))
		}
		switch {
		case isMerged && ln.PR > 0 && ln.MergeSHA != "":
			prLines = append(prLines, fmt.Sprintf("- %s: #%d (%s) — %s", ln.ID, ln.PR, ln.MergeSHA, cmp.Or(ln.Title, ln.ID)))
		case isMerged && ln.PR > 0:
			prLines = append(prLines, fmt.Sprintf("- %s: #%d — %s", ln.ID, ln.PR, cmp.Or(ln.Title, ln.ID)))
		case ln.PR > 0:
			prLines = append(prLines, fmt.Sprintf("- %s: %s (#%d, unmerged) — %s", ln.ID, ln.Status, ln.PR, cmp.Or(ln.Title, ln.ID)))
		default:
			prLines = append(prLines, fmt.Sprintf("- %s: %s — %s", ln.ID, ln.Status, cmp.Or(ln.Title, ln.ID)))
		}
	}
	defaultOutcome := "done"
	if failedLanes > 0 {
		if doneLanes > 0 {
			defaultOutcome = "partial"
		} else {
			defaultOutcome = "failed"
		}
	}
	outcome := cmp.Or(strings.TrimSpace(params.Outcome), defaultOutcome)
	prSummary := strings.Join(prRefs, ", ")
	if prSummary == "" {
		prSummary = fmt.Sprintf("%d terminal lane(s)", len(lanes))
	}

	var stepSummaries []string
	recordStep := func(summary string) {
		stepSummaries = append(stepSummaries, summary)
	}

	// 1. Release any remaining lane sessions.
	sessionSet := make(map[string]bool)
	for i := range lanes {
		if sid := strings.TrimSpace(lanes[i].SessionID); sid != "" {
			sessionSet[sid] = true
		}
		for _, sid := range lanes[i].Sessions {
			if trimmed := strings.TrimSpace(sid); trimmed != "" {
				sessionSet[trimmed] = true
			}
		}
	}
	for i := range curRun.Lanes {
		if sid := strings.TrimSpace(curRun.Lanes[i].Session); sid != "" {
			sessionSet[sid] = true
		}
	}
	if orchSess := strings.TrimSpace(curRun.DrivenBy); orchSess != "" {
		delete(sessionSet, orchSess)
	}
	var allSessions []string
	for sid := range sessionSet {
		allSessions = append(allSessions, sid)
	}
	slices.Sort(allSessions)
	alreadyReleased := make(map[string]bool)
	for _, sid := range cr.syncer.ReleasedSessions() {
		alreadyReleased[sid] = true
	}
	var releaseFailed []string
	if releaser, ok := d.driver.(lane.SessionReleaser); ok {
		for _, sid := range allSessions {
			if alreadyReleased[sid] {
				continue
			}
			if rErr := releaser.ReleaseSession(ctx, sid, true); rErr != nil {
				slog.Warn("release lane session during closeout", "run", runID, "session", sid, "error", rErr)
				releaseFailed = append(releaseFailed, sid)
				continue
			}
			cr.syncer.MarkSessionReleased(sid)
		}
	} else {
		for _, sid := range allSessions {
			if !alreadyReleased[sid] {
				releaseFailed = append(releaseFailed, sid)
			}
		}
	}
	releasedList := cr.syncer.ReleasedSessions()
	if len(releasedList) > 0 && len(releaseFailed) > 0 {
		recordStep(fmt.Sprintf("released lane sessions (%s); failed to release (%s)", strings.Join(releasedList, ", "), strings.Join(releaseFailed, ", ")))
	} else if len(releasedList) > 0 {
		recordStep(fmt.Sprintf("released lane sessions (%s)", strings.Join(releasedList, ", ")))
	} else if len(releaseFailed) > 0 {
		recordStep(fmt.Sprintf("failed to release lane sessions (%s)", strings.Join(releaseFailed, ", ")))
	}

	// 2. Apply any additional validated file writes named in OBJECTIVE.md.
	for _, relKey := range fileKeys {
		resolvedTarget := resolvedFiles[relKey]
		if filepath.ToSlash(filepath.Clean(relKey)) == "FINAL.md" {
			if strings.TrimSpace(params.FinalMarkdown) == "" {
				params.FinalMarkdown = params.Files[relKey]
			}
			continue
		}
		existingContent := stripMarker(readText(resolvedTarget))
		if trimmedAdd := strings.TrimSpace(params.Files[relKey]); trimmedAdd != "" && !strings.Contains(existingContent, trimmedAdd) {
			if _, wErr := WriteCloseoutFile(curRun.Dir, rawObj, relKey, params.Files[relKey], true); wErr != nil {
				return "", wErr
			}
		}
		recordStep(fmt.Sprintf("updated %s in run directory", relKey))
	}
	if len(fileKeys) > 0 {
		if refreshedObj := stripMarker(readText(objPath)); strings.TrimSpace(refreshedObj) != "" {
			rawObj = refreshedObj
		}
		if refreshedProg := stripMarker(readText(progPath)); strings.TrimSpace(refreshedProg) != "" {
			rawProg = refreshedProg
		}
	}

	// 3. Append to plan log file (e.g. PLAN.md) if named in OBJECTIVE.md.
	planRel := strings.TrimSpace(params.PlanFile)
	if planRel == "" {
		planRel = extractCloseoutPlanFile(curRun.Dir, rawObj)
	}
	if planRel != "" {
		planAbs, vErr := ValidateCloseoutWrite(curRun.Dir, rawObj, planRel)
		if vErr != nil {
			return "", vErr
		}
		logLine := strings.TrimSpace(params.PlanLogLine)
		dedupeKey := logLine
		if logLine == "" {
			dedupeKey = fmt.Sprintf("%s %s: merged %s", runID, outcome, prSummary)
			logLine = fmt.Sprintf("- %s %s", ts, dedupeKey)
		}
		existingPlan := readText(planAbs)
		if !strings.Contains(existingPlan, dedupeKey) {
			if _, wErr := WriteCloseoutFile(curRun.Dir, rawObj, planRel, logLine, true); wErr != nil {
				return "", wErr
			}
		}
		recordStep(fmt.Sprintf("appended close-out log entry to %s (%s)", planRel, prSummary))
	}

	// 4. Build FINAL.md content and republish Nebula post before marking OBJECTIVE.md done.
	var finalContent string
	if explicitFinal := strings.TrimSpace(params.FinalMarkdown); explicitFinal != "" {
		finalContent = explicitFinal + "\n"
	} else {
		var finalBuilder strings.Builder
		fmt.Fprintf(&finalBuilder, "# Final Report: %s\n\n", runID)
		fmt.Fprintf(&finalBuilder, "Outcome: %s\n", outcome)
		fmt.Fprintf(&finalBuilder, "Completed: %s\n", ts)
		if curRun.Goal != "" {
			fmt.Fprintf(&finalBuilder, "Goal: %s\n", curRun.Goal)
		}
		finalBuilder.WriteString("\n## Merged PRs\n")
		for _, line := range prLines {
			finalBuilder.WriteString(line + "\n")
		}
		if len(releaseFailed) > 0 {
			fmt.Fprintf(&finalBuilder, "\n## Unreleased Sessions\n- %d session(s) failed to release: %s\n", len(releaseFailed), strings.Join(releaseFailed, ", "))
		}
		if extra := strings.TrimSpace(params.Summary); extra != "" {
			finalBuilder.WriteString("\n## Summary\n")
			finalBuilder.WriteString(extra + "\n")
		} else {
			fmt.Fprintf(&finalBuilder, "\n## Summary\nAll lanes terminal (%s). Remaining orchestrator handover steps completed; run `/release` when ready.\n", prSummary)
		}
		finalContent = finalBuilder.String()
	}

	handoverText = stripMarker(readText(handoverPath))
	postID := cmp.Or(strings.TrimSpace(params.PostID), extractCloseoutPostID(rawObj, storedObj, rawProg, handoverText, storedPlanRef))
	if postID != "" {
		if vErr := dispatcher.ValidateCloseoutPostID(postID, rawObj, storedObj, rawProg, handoverText, storedPlanRef); vErr != nil {
			return "", vErr
		}
		postBody := strings.TrimSpace(params.PostContent)
		if postBody == "" {
			if planRel != "" {
				if planText := strings.TrimSpace(readText(filepath.Join(curRun.Dir, planRel))); planText != "" {
					postBody = planText + "\n\n" + finalContent
				}
			}
			if postBody == "" {
				postBody = finalContent
			}
		}
		postDrv := cmp.Or(cr.driver, d.driver)
		pub, ok := postDrv.(lane.PostPublisher)
		if !ok {
			return "", fmt.Errorf("republish post %s: driver does not support post updates", postID)
		}
		if pErr := pub.UpdatePost(ctx, postID, postBody); pErr != nil {
			if !strings.HasPrefix(pErr.Error(), "republish post ") {
				return "", fmt.Errorf("republish post %s: %w", postID, pErr)
			}
			return "", pErr
		}
		recordStep(fmt.Sprintf("republished Nebula post %s with close-out summary (%s)", postID, prSummary))
	}

	// 5. Write FINAL.md inside the run directory.
	if _, wErr := WriteCloseoutFile(curRun.Dir, rawObj, "FINAL.md", finalContent, false); wErr != nil {
		return "", wErr
	}
	recordStep(fmt.Sprintf("wrote FINAL.md (outcome=%s, merged %s)", outcome, prSummary))

	// 6. Update PROGRESS.md and OBJECTIVE.md Next: line.
	nextLine := strings.TrimSpace(params.Next)
	if nextLine == "" {
		if failedLanes > 0 {
			nextLine = fmt.Sprintf("Next: none. Run DONE %s — lanes terminal (%s, outcome=%s); FINAL.md written. Run /release when ready.", ts, prSummary, outcome)
		} else {
			nextLine = fmt.Sprintf("Next: none. Run DONE %s — all lanes merged (%s); FINAL.md written. Run /release when ready.", ts, prSummary)
		}
	} else if !ObjectiveIsDone(nextLine) {
		nextLine = strings.TrimRight(nextLine, ".") + fmt.Sprintf(" — Run DONE %s (FINAL.md written; /release ready).", ts)
	}
	updatedObj := replaceObjectiveNextLine(rawObj, nextLine)
	if _, wErr := WriteCloseoutFile(curRun.Dir, rawObj, "OBJECTIVE.md", updatedObj, false); wErr != nil {
		return "", wErr
	}
	progAppend := strings.TrimSpace(params.ProgressMarkdown)
	progDedupe := progAppend
	if progAppend == "" {
		progDedupe = fmt.Sprintf("- Outcome: %s (merged %s)", outcome, prSummary)
		progAppend = fmt.Sprintf("\n## Close-out (%s)\n%s\n- Wrote FINAL.md and updated OBJECTIVE.md Next:\n- Run `/release` to hand back the run.\n", ts, progDedupe)
	}
	if !strings.Contains(rawProg, progDedupe) {
		if _, wErr := WriteCloseoutFile(curRun.Dir, rawObj, "PROGRESS.md", progAppend, true); wErr != nil {
			return "", wErr
		}
	}
	_, _ = cr.syncer.RefreshObjective(ctx, d.store.Runs())
	expireHoldDirectivesIfRunDone(ctx, d.store, runID)
	cr.syncer.MarkClosedOut()
	recordStep(fmt.Sprintf("updated OBJECTIVE.md Next: and PROGRESS.md (run DONE, merged %s)", prSummary))

	// 7. Offer /release to the owner.
	finalMsg := fmt.Sprintf("run %s close-out complete (outcome=%s, merged %s, FINAL.md written); run /release to hand back the run", runID, outcome, prSummary)
	if len(releaseFailed) > 0 {
		finalMsg = fmt.Sprintf("run %s close-out complete with %d unreleased session(s) (%s; outcome=%s, merged %s, FINAL.md written); run /release to hand back the run", runID, len(releaseFailed), strings.Join(releaseFailed, ", "), outcome, prSummary)
	}
	recordStep(finalMsg)
	if d.store != nil {
		existingResults := make(map[string]bool)
		if prevDecs, lErr := d.store.Decisions().List(context.WithoutCancel(ctx), store.DecisionFilter{RunID: runID}); lErr == nil {
			for i := range prevDecs {
				if prevDecs[i].Tool == dispatcher.ToolRunCloseout {
					existingResults[prevDecs[i].Result] = true
				}
			}
		}
		reason := cmp.Or(strings.TrimSpace(params.Reason), "close out adopted run")
		for _, summary := range stepSummaries {
			if existingResults[summary] {
				continue
			}
			existingResults[summary] = true
			_, _ = d.store.Decisions().Append(context.WithoutCancel(ctx), store.Decision{
				RunID:     runID,
				Category:  store.DecisionJudgment,
				Actor:     dispatcher.ActorDispatcher,
				Tool:      dispatcher.ToolRunCloseout,
				Reason:    reason,
				Result:    summary,
				Output:    summary,
				Vetted:    true,
				CreatedAt: now,
			})
		}
	}
	return strings.Join(stepSummaries, "; "), nil
}
