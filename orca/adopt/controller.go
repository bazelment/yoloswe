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

// JudgeFactory creates the dispatcher session for an adopted run.
type JudgeFactory func(runID string) controller.Judge

// Controllers attaches host runs to orca's deterministic lane lifecycle.
//
//nolint:govet // field grouping follows lifecycle ownership and synchronization.
type Controllers struct {
	ctx       context.Context
	store     store.Store
	driver    lane.LaneDriver
	clock     clock.Clock
	judges    JudgeFactory
	Mailboxes func(runID string) []mailbox.Mailbox
	// PRHold reads the host's standing PR Hold: comment. The CLI owns the
	// external read; adopted decision logic keeps the hold until explicit release.
	PRHold             func(context.Context, Run, Lane) (string, error)
	ledgerScript       *string
	runs               map[string]*controlledRun
	starting           map[string]*startup
	SubscriptionActors []string
	MaxCostPerHourUSD  float64
	// Shadow runs execute the normal controller against an isolated store while
	// recording transport effects. They never acquire leases or checkpoint the
	// host run directory.
	Shadow           bool
	DecideAsOperator bool
	Recorder         *ActionRecorder
	mu               sync.Mutex
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
	funding    []string
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
	paused, _, _, msg, err := store.EnforceCostGuard(ctx, d.store, cr.run.ID, d.clock.Now().UTC(), d.MaxCostPerHourUSD, d.SubscriptionActors...)
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
	if len(cr.funding) == 0 {
		return cr.downReason
	}
	return strings.TrimSpace(cr.downReason + "\nawaiting funding: " + strings.Join(cr.funding, ", "))
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
	if d.Shadow && d.Recorder == nil {
		return errors.New("shadow controller requires an action recorder")
	}
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
	j, err := readJournal(filepath.Join(r.Dir, leaseFile))
	if d.Shadow && errors.Is(err, os.ErrNotExist) {
		j = &journal{}
		err = nil
	}
	if err != nil {
		return err
	}
	generation := j.Marker
	populateRunRepo(&r)
	_, _, originRepo, hasTier1Repo := readTier1AndOriginRepo(r.Dir)
	if !hasTier1Repo {
		if storeRepo := resolveStoreRepository(ctx, d.store, r.ID, originRepo); storeRepo != "" {
			r.Repo = ""
			r.RepoID = ""
			applyRepoValue(&r, storeRepo)
		}
	}
	if err := d.importPRHolds(ctx, &r); err != nil {
		return err
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
	if !d.Shadow {
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
	}
	started := false
	defer func() {
		if !started && !d.Shadow {
			_ = d.store.Runs().ReleaseLease(context.WithoutCancel(ctx), r.ID, holder)
		}
	}()
	storedRun, err := d.store.Runs().Get(ctx, r.ID)
	if err != nil {
		return err
	}
	resume := sameAdoptionGeneration(storedRun.PlanRef, generation)
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
		if !d.Shadow {
			if tracker, ok := d.driver.(interface{ TrackLane(lane.Lane) }); ok && !ln.Status.Terminal() {
				tracker.TrackLane(*ln)
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
	if d.Shadow && d.Recorder != nil {
		for i := range boxes {
			boxes[i] = NewRecordingMailbox(boxes[i], d.Recorder)
		}
	}
	syncer := newRunLedgerSync(r, d.clock, lanes)
	syncer.readOnly = d.Shadow
	syncer.ledgerScript = d.ledgerScript
	runStore := &adoptedStore{
		Store: d.store,
		runs: &adoptedRunStore{
			RunStore: d.store.Runs(),
			syncer:   syncer,
		},
		lanes: &adoptedLaneStore{
			LaneStore: d.store.Lanes(),
			syncer:    syncer,
			// Sole-operator mode still observes the host ledger, but its local
			// lifecycle writes must never reach the adopted run.
			shadow:   d.Shadow,
			operator: d.DecideAsOperator,
		},
	}
	var cr *controlledRun
	var dispatch controller.Judge
	if d.judges != nil {
		dispatch = d.judges(r.ID)
	}
	if shell, ok := dispatch.(*ShellDispatcher); ok {
		shell.Run = r
		if err := shell.Prepare(); err != nil {
			return err
		}
	}
	if setter, ok := dispatch.(interface{ SetStore(store.Store) }); ok {
		setter.SetStore(runStore)
	}
	adoptedAt := parseAdoptCutoff(generation, d.clock)
	for i := range boxes {
		if setter, ok := boxes[i].(mailbox.AdoptCutoffSetter); ok {
			setter.SetAdoptCutoff(adoptedAt, runStore)
			_ = setter.SyncAdoptCutoff(ctx)
		}
	}
	runDriver := d.driver
	if runDriver != nil && d.Shadow && d.Recorder != nil {
		runDriver = NewRecordingDriver(runDriver, d.Recorder)
	}
	// The CLI dispatcher and the skill's watcher own judgment; the controller
	// keeps the lease, heartbeat and host refresh for the adopted run.
	ctrl := controller.New(controller.Config{
		BeforeStep: func(ctx context.Context) (bool, error) {
			return d.prepareStep(ctx, cr)
		},
		HostRefresh: func(ctx context.Context) error {
			return d.refreshHostState(ctx, r.ID)
		},
		RunID:              r.ID,
		HolderID:           holder,
		MaxCostPerHourUSD:  d.MaxCostPerHourUSD,
		SubscriptionActors: d.SubscriptionActors,
		HostRun:            true,
		SkipLease:          d.Shadow,
	}, runStore, runDriver, d.clock, dispatch, boxes...)
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
	if err := ctrl.StartWatches(runCtx); err != nil {
		cleanupFailedStart()
		_ = ctrl.ReleaseLease(ctx)
		return err
	}
	d.mu.Lock()
	d.runs[r.ID] = cr
	d.mu.Unlock()
	started = true
	go d.watchRunFiles(runCtx, cr)
	go d.supervise(runCtx, cr)
	if shell, ok := dispatch.(*ShellDispatcher); ok {
		go shell.Supervise(runCtx)
	}
	return nil
}

// watchRunFiles turns host ledger/report changes into immediate controller
// wakes. The controller still polls as a backstop, but an EVENTS append or a
// report/.done/.needs-swe update is visible without waiting for that tick.
func (d *Controllers) watchRunFiles(ctx context.Context, cr *controlledRun) {
	if cr == nil {
		return
	}
	lastDir := strings.TrimSpace(cr.run.Dir)
	if cr.syncer != nil {
		lastDir = strings.TrimSpace(cr.syncer.CurrentRun().Dir)
	}
	last := runFileSignature(lastDir)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if shell, ok := cr.dispatch.(*ShellDispatcher); ok {
				if err := shell.syncEventFiles(ctx); err != nil {
					slog.Warn("relay adopted owner events", "error", err)
				}
			}
			dir := strings.TrimSpace(cr.run.Dir)
			if cr.syncer != nil {
				dir = strings.TrimSpace(cr.syncer.CurrentRun().Dir)
			}
			now := runFileSignature(dir)
			if dir != lastDir {
				if lastDir != "" {
					cr.controller.Wake()
				}
				lastDir, last = dir, now
				continue
			}
			if now != last {
				if shell, ok := cr.dispatch.(*ShellDispatcher); ok {
					if err := shell.Refresh(); err != nil {
						slog.Warn("refresh shadow workspace", "error", err)
						continue
					}
				}
				last = now
				cr.controller.Wake()
			}
		}
	}
}

func runFileSignature(dir string) string {
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry == nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		if name != "EVENTS" && name != "state.json" && !strings.HasSuffix(name, ".md") &&
			!strings.HasSuffix(name, ".done") && !strings.HasSuffix(name, ".needs-swe") &&
			!strings.HasPrefix(filepath.ToSlash(rel), "reports/") {
			return nil
		}
		if info, statErr := entry.Info(); statErr == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", path, info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	return b.String()
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

// Stop stops r's controller and releases its SQLite run lease (used by
// /release).
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
	cr.cancel()
	<-cr.done
	if closer, ok := cr.dispatch.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	if releaseLease {
		if !d.Shadow {
			if err := cr.controller.ReleaseLease(ctx); err != nil {
				return err
			}
		}
	}
	d.mu.Lock()
	if d.runs[r.ID] == cr {
		delete(d.runs, r.ID)
	}
	d.mu.Unlock()
	return nil
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

// refreshHostState is the adopted handover's read-before-act fence. Host
// ledger phase/status/session changes are imported on every tick, including in
// shadow mode, so a stale local snapshot can never drive a lifecycle action.
func (d *Controllers) refreshHostState(ctx context.Context, runID string) error {
	cr := d.lookupRun(runID)
	if cr == nil || cr.syncer == nil {
		return nil
	}
	hostRun := cr.syncer.CurrentRun()
	fresh, err := Read(hostRun.Dir)
	if err != nil {
		return err
	}
	fresh.ID = runID
	populateRunRepo(&fresh)
	if err := d.importPRHolds(ctx, &fresh); err != nil {
		return err
	}
	workflow, imported, items, err := controllerState(fresh)
	if err != nil {
		return err
	}
	itemStates := make(map[string]store.BacklogStatus, len(items))
	for i := range items {
		itemStates[items[i].ID] = items[i].Status
	}
	var funding []string
	for i := range items {
		item := &items[i]
		if item.Status != store.BacklogOpen {
			continue
		}
		ready := strings.TrimSpace(item.Description) != ""
		for _, dep := range item.DependsOn {
			ready = ready && itemStates[dep].Terminal()
		}
		if !ready {
			funding = append(funding, item.ID)
		}
	}
	cr.statusMu.Lock()
	cr.funding = funding
	cr.statusMu.Unlock()
	if !slices.Equal(hostRun.Phases, fresh.Phases) {
		workflow.Version = 0
		saved, err := d.store.Recipes().Save(ctx, workflow)
		if err != nil {
			return err
		}
		for i := range fresh.Lanes {
			if !lane.Status(fresh.Lanes[i].Status).Terminal() {
				if cur, getErr := d.store.Lanes().Get(ctx, runID, fresh.Lanes[i].ID); getErr == nil {
					cur.RecipeName, cur.RecipeVersion = saved.Name, saved.Version
					_ = d.store.Lanes().Upsert(ctx, *cur)
				}
			}
		}
	}
	for i := range items {
		if err := d.store.Backlog().Add(ctx, items[i]); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return err
		} else if errors.Is(err, store.ErrAlreadyExists) {
			cur, err := d.store.Backlog().Get(ctx, runID, items[i].ID)
			if err != nil {
				return err
			}
			if cur.Description != items[i].Description || !slices.Equal(cur.DependsOn, items[i].DependsOn) || (items[i].Status.Terminal() && cur.Status != items[i].Status) {
				cur.Description, cur.DependsOn = items[i].Description, items[i].DependsOn
				if items[i].Status.Terminal() {
					cur.Status = items[i].Status
				}
				if err := d.store.Backlog().Update(ctx, *cur, controller.ActorController, "host funding updated"); err != nil {
					return err
				}
			}
		}
	}
	for i := range imported {
		host := imported[i]
		if !cr.syncer.HostLaneChanged(host) {
			continue
		}
		cur, getErr := d.store.Lanes().Get(ctx, runID, host.ID)
		if errors.Is(getErr, store.ErrNotFound) {
			if err := d.store.Lanes().Upsert(ctx, host); err != nil {
				return err
			}
			cr.syncer.RememberHostLane(host)
			continue
		}
		if getErr != nil {
			return getErr
		}
		if cur.Status.Terminal() {
			cr.syncer.RememberHostLane(host)
			continue
		}
		// Keep controller-owned report/approval timestamps and notes, while
		// replacing every host-owned lifecycle identity.
		identityChanged := cur.Phase != host.Phase || cur.TerminalID != host.TerminalID
		cur.Phase = host.Phase
		// The host ledger owns lifecycle status. A previous shadow intent must
		// never pin a lane in blocked after the host has moved it on.
		cur.Status = host.Status
		cur.SessionID, cur.TerminalID = host.SessionID, host.TerminalID
		cur.TerminalState, cur.HostHold, cur.HostHoldReason = host.TerminalState, host.HostHold, host.HostHoldReason
		cur.PR, cur.PRHead, cur.MergeSHA, cur.Checks = host.PR, host.PRHead, host.MergeSHA, host.Checks
		cur.Branch, cur.BaseBranch, cur.BaseCommit, cur.Worktree = host.Branch, host.BaseBranch, host.BaseCommit, host.Worktree
		if identityChanged {
			cur.AwaitingInputSince = time.Time{}
			cur.Question = ""
		}
		cur.Round = host.Round
		if cur.RecipeVersion == 0 {
			cur.RecipeVersion = 1
		}
		cur.Terminals, cur.Sessions = host.Terminals, host.Sessions
		if err := d.store.Lanes().Upsert(ctx, *cur); err != nil {
			return err
		}
		cr.syncer.RememberHostLane(host)
	}
	cr.syncer.mu.Lock()
	cr.syncer.run = fresh
	cr.syncer.mu.Unlock()
	return nil
}

func (d *Controllers) importPRHolds(ctx context.Context, r *Run) error {
	if d.PRHold == nil {
		return nil
	}
	for i := range r.Lanes {
		ln := &r.Lanes[i]
		if lane.Status(ln.Status).Terminal() || ln.PR == "" || strings.EqualFold(ln.PRState, "merged") {
			continue
		}
		comment, err := d.PRHold(ctx, *r, *ln)
		if err != nil {
			return fmt.Errorf("read host PR hold for %s: %w", ln.ID, err)
		}
		if hold := hostPRHold(comment, ""); hold != "" {
			ln.HostHold = hold
			ln.HostHoldUntil = hostHoldDeadline(comment)
		}
	}
	return nil
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

// Close stops controllers while keeping both the handover lease and the
// SQLite run lease, so a restarted or re-opened chat continues holding adopted
// host runs until /release.
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
			MergeGate   string              `json:"merge_gate"`
			// Merge is nebula-swarm's merge authority: none or a merge method.
			Merge       string      `json:"merge"`
			Base        string      `json:"base"`
			Target      string      `json:"target"`
			Phases      []hostPhase `json:"phases"`
			TargetLanes int         `json:"target_lanes"`
			NoPR        bool        `json:"no_pr"`
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
	workflow.MergePolicy.RequireCIGreen = true
	if st.Config.TargetLanes > 0 {
		workflow.Limits.MaxConcurrentLanes = st.Config.TargetLanes
	} else {
		live, planned := 0, false
		for i := range r.Lanes {
			imported := &r.Lanes[i]
			planned = planned || imported.Status == string(lane.StatusPlanned)
			if imported.Status != string(lane.StatusDone) && imported.Status != string(lane.StatusFailed) && strings.TrimSpace(imported.Terminal) != "" && controller.TerminalStateLive(imported.TerminalState) {
				live++
			}
		}
		// Leave room for one queued lane beyond the host's live terminals.
		if planned {
			live++
		}
		workflow.Limits.MaxConcurrentLanes = max(workflow.Limits.MaxConcurrentLanes, live)
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
	// An adopted run's declared phase list is authoritative. In particular,
	// merge policy text never removes review or jumps directly to integrate.
	// No-PR is the sole explicit exception because it changes the recipe's
	// handback contract rather than inferring a phase transition.
	if r.NoPR {
		workflow = recipe.WithoutIntegrate(workflow)
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
			ph.BriefTemplate += fmt.Sprintf("\nRun merge policy: mode=%s; target=%s; script=%s; require_review_pass=%t; require_ci_green=%t. %s", workflow.MergePolicy.Mode, workflow.MergePolicy.TargetBranch, workflow.MergePolicy.Script, workflow.MergePolicy.RequireReviewPass, workflow.MergePolicy.RequireCIGreen, adoptedMergeGateText(st.Config.MergeGate, ruleText, objectiveText, contextText))
			ph.BriefTemplate += "\nRequired checks: Lint plus Build and Test green on the PR's exact head before merge; one rerun allowed. Recheck the head before merging once."
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
		mergeSHA := strings.TrimSpace(imported.MergeSHA)
		if !validGitSHA(mergeSHA) {
			mergeSHA = ""
		}
		hostHoldReason := strings.TrimSpace(imported.HostHold)
		if hostHoldReason == "" {
			hostHoldReason = decisionHostHold(imported.LastEvent)
		}
		if hostHoldReason == "" && strings.TrimSpace(imported.WaitingOn) != "" {
			hostHoldReason = "waiting on " + strings.TrimSpace(imported.WaitingOn)
		}
		if hostHoldReason == "" && !finishedAnalysisLane(*imported) && strings.EqualFold(previewOutcome(imported.Report), string(lane.OutcomeBlocked)) {
			hostHoldReason = strings.TrimSpace(imported.ReportBlocker)
		}
		if hostHoldReason == "" && imported.PRDraft {
			hostHoldReason = "PR is draft"
		}
		mergeForbidden := hostHoldReason != "" || imported.PRDraft
		terminal := status.Terminal() || strings.EqualFold(imported.PRState, "merged") || closedLane(*imported) || (!mergeForbidden && integratedLane(*imported))
		if strings.EqualFold(imported.Phase, recipe.PhaseIntegrate) {
			closed := closedLane(*imported)
			mergeSHA = ""
			if !mergeForbidden {
				for j := range st.Tasks {
					if st.Tasks[j].ID == imported.ID {
						if sha := strings.TrimSpace(st.Tasks[j].MergeSHA); validGitSHA(sha) {
							mergeSHA = sha
						}
						break
					}
				}
			}
			if !mergeForbidden && !openPR(*imported) && imported.targetIntegrated && mergeSHA == "" {
				mergeSHA = imported.Head
			}
			// A closed host PR is terminal even though it is not a merge. Merge
			// prose and an integrate done report remain claims until the target
			// mirror (or an independent host merge SHA) proves the merge.
			terminal = terminal || closed || (!mergeForbidden && !openPR(*imported) && (imported.targetIntegrated || mergeSHA != ""))
			if !terminal && status == lane.StatusDone {
				status = lane.StatusRunning
			}
			if !terminal && (integratedLane(*imported) || reportDone(imported.Report)) {
				status = lane.StatusBlocked
			}
		}
		if mergeForbidden || openPR(*imported) {
			mergeSHA = ""
		}
		if terminal && !status.Terminal() {
			// Imported merge/close facts are authoritative even when a host
			// ledger's status or hold marker is stale. Terminal lanes are never
			// reopened by adoption.
			status = lane.StatusDone
		}
		// Host-owned blocks and draft PRs are imported as blocked so every
		// controller tick treats them as judgment-only. An owner/dispatcher
		// answer can explicitly move the lane back to running.
		if terminal && !mergeForbidden {
			hostHoldReason = ""
		}
		if hostHoldReason != "" && !terminal {
			status = lane.StatusBlocked
		}
		if !status.Valid() {
			return workflow, nil, nil, fmt.Errorf("lane %s: unsupported host status %q", imported.ID, imported.Status)
		}
		ln := lane.Lane{ItemID: imported.ID, ID: imported.ID, RunID: r.ID, Title: imported.Title, Branch: imported.Branch, Status: status, RecipeName: workflow.Name, RecipeVersion: 1, BaseBranch: st.Config.Base, Attempt: 1, TerminalState: imported.TerminalState, HostHoldReason: hostHoldReason, HostHold: imported.HoldAdvance,
			PRHead: imported.Head, MergeSHA: mergeSHA, Checks: imported.Checks}
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
				taskMergeSHA := strings.TrimSpace(task.MergeSHA)
				if mergeForbidden || !validGitSHA(taskMergeSHA) {
					taskMergeSHA = ""
				}
				ln.PRHead, ln.ApprovalSHA, ln.MergeSHA, ln.Checks, ln.BaseCommit, ln.Worktree = firstNonEmpty(task.PRHead, ln.PRHead), task.ApprovalSHA, firstNonEmpty(taskMergeSHA, ln.MergeSHA), firstNonEmpty(task.Checks, ln.Checks), task.BaseCommit, task.Worktree
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

// Keep gate prose additive without letting it change the host's merge authority.
func adoptedMergeGateText(configGate, rule, objective, contextText string) string {
	parts := []string{configGate, rule}
	for _, line := range strings.Split(objective+"\n"+contextText, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "merge gate") || strings.Contains(lower, "merge_gate") || strings.Contains(lower, "lint") || strings.Contains(lower, "build and test") || strings.Contains(lower, "soak") || strings.Contains(lower, "before merg") {
			parts = append(parts, line)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
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

type adoptedStore struct {
	store.Store
	runs  store.RunStore
	lanes store.LaneStore
}

func (s *adoptedStore) Runs() store.RunStore {
	if s.runs != nil {
		return s.runs
	}
	return s.Store.Runs()
}

func (s *adoptedStore) Lanes() store.LaneStore { return s.lanes }

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
	syncer   *runLedgerSync
	shadow   bool
	operator bool
}

// hostOverlay restores ephemeral host facts after a SQLite read. Those facts
// belong to the host ledger, so they deliberately do not require a store schema
// migration and are re-read before every controller tick.
func (s *adoptedLaneStore) hostOverlay(ln *lane.Lane) {
	if ln == nil || s.syncer == nil || s.operator {
		return
	}
	r := s.syncer.CurrentRun()
	for i := range r.Lanes {
		host := &r.Lanes[i]
		if host.ID != ln.ID {
			continue
		}
		ln.HostHold = host.HoldAdvance
		ln.HostHoldReason = cmp.Or(host.HostHold, decisionHostHold(host.LastEvent))
		if ln.HostHoldReason == "" && host.WaitingOn != "" {
			ln.HostHoldReason = "waiting on " + host.WaitingOn
		}
		if ln.HostHoldReason == "" && host.PRDraft {
			ln.HostHoldReason = "PR is draft"
		}
		if !ln.Status.Terminal() && s.syncer.HostStatusIsNewer(host.ID, lane.Status(host.Status)) {
			ln.Status = lane.Status(host.Status)
		}
		if !ln.Status.Terminal() && (strings.EqualFold(host.PRState, "merged") || (ln.HostHoldReason == "" && !host.PRDraft && integratedLane(*host))) {
			ln.Status = lane.StatusDone
		}
		return
	}
}

func (s *adoptedLaneStore) Get(ctx context.Context, runID, id string) (*lane.Lane, error) {
	ln, err := s.LaneStore.Get(ctx, runID, id)
	if err == nil && runID == s.syncer.runID {
		s.hostOverlay(ln)
	}
	return ln, err
}

func (s *adoptedLaneStore) List(ctx context.Context, filter store.LaneFilter) ([]lane.Lane, error) {
	lanes, err := s.LaneStore.List(ctx, filter)
	if err == nil {
		for i := range lanes {
			if lanes[i].RunID == s.syncer.runID {
				s.hostOverlay(&lanes[i])
			}
		}
	}
	return lanes, err
}

// Upsert checkpoints the lane live to the host ledger.
func (s *adoptedLaneStore) Upsert(ctx context.Context, ln lane.Lane) error {
	if s.shadow {
		// Shadow lifecycle intents are recorded by the RecordingDriver. Keeping
		// them out of the overlay means the next tick starts from host state.
		return nil
	}
	if s.syncer != nil && ln.RunID == s.syncer.runID {
		if err := s.syncer.SyncLane(ctx, ln); err != nil {
			slog.Warn("live checkpoint adopted lane failed", "run", ln.RunID, "lane", ln.ID, "error", err)
		}
	}
	return s.LaneStore.Upsert(ctx, ln)
}

// runLedgerSync mirrors controller lane transitions and OBJECTIVE.md updates
// to an adopted host run directory.
//
// Lock ordering: ioMu serializes disk and subprocess I/O in SyncLane; mu
// guards in-memory state (run, known, lastObjective, stat signatures). Callers
// may acquire mu while holding ioMu, never the reverse, and mu is never held
// across file or subprocess I/O.
//
//nolint:govet // fields mirror the host ledger and synchronization state.
type runLedgerSync struct {
	clock          clock.Clock
	known          map[string]lane.Lane
	hostKnown      map[string]lane.Lane
	ledgerScript   *string
	readOnly       bool
	runID          string
	resolvedScript string
	lastObjective  string
	run            Run
	objModNano     int64
	objSize        int64
	progModNano    int64
	progSize       int64
	statReady      bool
	scriptResolved bool
	ioMu           sync.Mutex
	mu             sync.Mutex
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
	hostKnown := make(map[string]lane.Lane, len(initial))
	for i := range initial {
		known[initial[i].ID] = initial[i].Clone()
		hostKnown[initial[i].ID] = initial[i].Clone()
	}
	objMod, objSize := fileStatSignature(filepath.Join(r.Dir, "OBJECTIVE.md"))
	progMod, progSize := fileStatSignature(filepath.Join(r.Dir, "PROGRESS.md"))
	return &runLedgerSync{
		runID:         r.ID,
		run:           r,
		clock:         clk,
		known:         known,
		hostKnown:     hostKnown,
		lastObjective: fullObjectiveWithHandover(r),
		objModNano:    objMod,
		objSize:       objSize,
		progModNano:   progMod,
		progSize:      progSize,
		statReady:     r.Dir != "",
	}
}

func (s *runLedgerSync) CurrentRun() Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run
}

// HostLaneChanged reports whether a newly imported ledger row differs from the
// last ledger snapshot. Controller writes in adopted shadow mode update known,
// but they must not make an unchanged host row rewind that local state.
func (s *runLedgerSync) HostLaneChanged(ln lane.Lane) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.hostKnown[ln.ID]
	changed := !ok || hostLaneDiff(prev, ln)
	s.hostKnown[ln.ID] = ln.Clone()
	return changed
}

// HostLaneIsNewer reports whether the current ledger row differs from the
// last host snapshot. It is intentionally read-only; callers record the row
// after deciding whether to overlay it.
func (s *runLedgerSync) HostLaneIsNewer(ln lane.Lane) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.hostKnown[ln.ID]
	return !ok || hostLaneDiff(prev, ln)
}

func (s *runLedgerSync) HostStatusIsNewer(id string, status lane.Status) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.hostKnown[id]
	return !ok || prev.Status != status
}

func (s *runLedgerSync) RememberHostLane(ln lane.Lane) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostKnown[ln.ID] = ln.Clone()
}

func hostLaneDiff(a, b lane.Lane) bool {
	if lanePersistedDiff(a, b) || a.TerminalState != b.TerminalState || a.HostHold != b.HostHold || a.HostHoldReason != b.HostHoldReason {
		return true
	}
	if len(a.Terminals) != len(b.Terminals) || len(a.Sessions) != len(b.Sessions) {
		return true
	}
	for k, v := range a.Terminals {
		if b.Terminals[k] != v {
			return true
		}
	}
	for k, v := range a.Sessions {
		if b.Sessions[k] != v {
			return true
		}
	}
	return false
}

func (s *runLedgerSync) RefreshObjectiveIfChanged(ctx context.Context, runs store.RunStore) (bool, error) {
	if s.readOnly {
		return false, nil
	}
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
	if s.readOnly {
		return true, nil
	}

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
	if s.readOnly {
		s.mu.Lock()
		s.known[ln.ID] = ln.Clone()
		s.mu.Unlock()
		return nil
	}
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
	s.hostKnown[ln.ID] = ln.Clone()
	s.mu.Unlock()
	return nil
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
