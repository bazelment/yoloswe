package chat

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/concierge"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/router"
	"github.com/bazelment/yoloswe/orca/store"
)

// ErrPlannerFallbackToQueue signals that PlannerStream could not run a live or
// replayed planner chat turn (e.g. when a test replay only recorded router
// exchanges) so Dispatch should fall back to queuing a plan_import directive.
var ErrPlannerFallbackToQueue = errors.New("orca/chat: planner fallback to queue")

// StoreBackend attaches chat to the same durable inbox and directives as orca run.
//
// The backend owns its attached store: Attach swaps the store and run under
// one lock and closes the store it replaces, so the poll loop, View and
// commands never see a half-switched or closed store.
//
//nolint:govet
type StoreBackend struct {
	Stream func(context.Context, string, func(string)) error
	// PlannerStream streams a conversational answer from the planner for runID
	// in st. When nil (or when it returns ErrPlannerFallbackToQueue), Dispatch
	// falls back to queuing a plan_import directive.
	PlannerStream func(ctx context.Context, st store.Store, runID, text string, emit func(string)) error
	Now           func() time.Time
	ListRuns      func(context.Context) ([]store.Run, error)
	// OpenRun opens the store of a run ListRuns returned; /runs <id> attaches
	// to it.
	OpenRun func(context.Context, string) (store.Store, error)
	// OnAttach runs under the attachment lock whenever another run is attached.
	OnAttach func(store.Store, string)
	// OnHoldChange runs when /adopt or /release changes the held runs.
	OnHoldChange func()
	// Adoption hands host swarms to this chat; its lease holder is stable
	// across chat restarts, so /release finds the adopted run after one.
	// When nil, AdoptionUnavailable says why /adopt and /release are off.
	Adoption            *adopt.Service
	AdoptionUnavailable string
	// HostSwarms lists this machine's host swarms for /runs; nil falls back
	// to Adoption.
	HostSwarms func(context.Context) ([]adopt.Run, error)
	// HostProbe reads live lane terminal and PR state for LiveSwarms.
	HostProbe HostProbe
	// FrontDesk is the run of a chat opened without one. No controller runs
	// it, so a Planner or Dispatcher instruction sent while attached to it
	// goes to a run whose controller this chat holds, else to the one run a
	// live controller holds, or is refused.
	FrontDesk string
	// Restart validates the startup binary and prepares an in-place re-exec
	// for /restart.
	Restart func(context.Context, string) (string, error)

	mu            sync.RWMutex
	store         store.Store
	runID         string
	memory        concierge.MemoryEditor
	lastDirective lastDirective

	cacheMu            sync.Mutex
	prCache            map[string]cachedPR
	usageCache         map[string]runUsageCache
	seenTurns          map[string]bool
	seenDecs           map[string]bool
	seenAsks           map[string]bool
	pendingOutcomeRuns map[string]bool
	exchanges          []chatExchange
}

// ProposalAskIDPrefix prefixes pending action proposals stored in the open-question table.
const ProposalAskIDPrefix = "proposal-"

const proposalTTL = 15 * time.Minute

type chatExchange struct {
	owner string
	orca  string
}

const (
	// prCacheTTL bounds how long a successful GitHub PR status probe is reused
	// across periodic status snapshots while a lane remains in the same phase/round.
	prCacheTTL = 30 * time.Second
	// snapshotPRProbeTimeout bounds a single PR probe during status refresh.
	snapshotPRProbeTimeout = 5 * time.Second
)

//nolint:govet // cache entry grouping
type cachedPR struct {
	at     time.Time
	phase  string
	round  int
	status PRStatus
}

//nolint:govet // cache entry grouping
type runUsageCache struct {
	day      time.Time
	store    store.Store
	termCost map[string]float64
	afterSeq int64
	total    float64
}

// lastDirective identifies the directive /undo revokes.
type lastDirective struct{ run, id string }

// NewStoreBackend returns a backend attached to runID in st. Close, or an
// Attach to another store, closes st.
func NewStoreBackend(st store.Store, runID string) *StoreBackend {
	b := &StoreBackend{store: st, runID: runID}
	b.seedSeenDispatcherRun(context.Background(), st, runID)
	return b
}

// RunID returns the attached run.
func (b *StoreBackend) RunID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.runID
}

// WithRun calls fn with the attached store and run, holding the attachment
// so it is neither switched nor closed while fn runs.
func (b *StoreBackend) WithRun(fn func(store.Store, string) error) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return fn(b.store, b.runID)
}

// Attach makes runID in st the attached run and closes the previous store.
func (b *StoreBackend) Attach(st store.Store, runID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	old := b.store
	b.store, b.runID, b.lastDirective = st, runID, lastDirective{}
	b.cacheMu.Lock()
	b.exchanges = nil
	b.cacheMu.Unlock()
	b.seedSeenDispatcherRun(context.Background(), st, runID)
	if b.OnAttach != nil {
		b.OnAttach(st, runID)
	}
	if old == nil || old == st {
		return nil
	}
	return old.Close()
}

// Close closes the attached store.
func (b *StoreBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.store == nil {
		return nil
	}
	err := b.store.Close()
	b.store = nil
	return err
}

func (b *StoreBackend) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *StoreBackend) frontDesk() bool { return b.FrontDesk != "" && b.runID == b.FrontDesk }

// HeldRuns returns the attached run followed by the adopted runs driven in
// the attached store.
func (b *StoreBackend) HeldRuns(ctx context.Context) ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.heldRuns(ctx)
}

// RestartHeld restarts controllers for every adopted run this chat holds.
func (b *StoreBackend) RestartHeld(ctx context.Context) ([]adopt.Run, error) {
	if b.Adoption == nil {
		return nil, nil
	}
	ids, err := b.Adoption.Adopted()
	if err != nil {
		return nil, err
	}
	var restarted []adopt.Run
	var errs []error
	for _, id := range ids {
		r, adoptErr := b.Adoption.Adopt(ctx, id)
		if adoptErr != nil {
			errs = append(errs, adoptErr)
			continue
		}
		restarted = append(restarted, r)
	}
	return restarted, errors.Join(errs...)
}

// ReadHostRun reads the host swarm run directory of id when Adoption or HostSwarms is set.
func (b *StoreBackend) ReadHostRun(id string) (adopt.Run, error) {
	if b.Adoption != nil {
		dir, err := b.Adoption.Importer.Find(id)
		if err != nil {
			return adopt.Run{}, err
		}
		return adopt.Read(dir)
	}
	if b.HostSwarms != nil {
		runs, err := b.HostSwarms(context.Background())
		if err != nil {
			return adopt.Run{}, err
		}
		for i := range runs {
			if runs[i].ID == id {
				return runs[i], nil
			}
		}
	}
	return adopt.Run{}, adopt.ErrRunNotFound
}

// heldRuns returns the attached run followed by the adopted runs driven in
// the attached store. The caller holds b.mu.
func (b *StoreBackend) heldRuns(ctx context.Context) ([]string, error) {
	runs, _, err := b.heldRunsWithRecords(ctx)
	return runs, err
}

func (b *StoreBackend) heldRunsWithRecords(ctx context.Context) ([]string, map[string]*store.Run, error) {
	runs := []string{b.runID}
	byID := map[string]*store.Run{}
	if b.Adoption == nil {
		return runs, byID, nil
	}
	ids, err := b.Adoption.Adopted()
	for _, id := range ids {
		if id == b.runID {
			continue
		}
		if r, getErr := b.store.Runs().Get(ctx, id); getErr == nil {
			runs = append(runs, id)
			byID[id] = r
		}
	}
	return runs, byID, err
}

// openAsk is an unanswered owner or lane question.
type openAsk struct {
	run, scope string
	age        string
	ask        controller.OwnerAsk
}

func (b *StoreBackend) asks(ctx context.Context, runs []string) ([]openAsk, error) {
	now := b.now()
	var out []openAsk
	for _, run := range runs {
		allLaneQs, err := mailbox.OpenLaneQuestions(ctx, b.store, run, now)
		if err != nil {
			return nil, err
		}
		mailCreated := make(map[string]time.Time, len(allLaneQs)*2)
		for i := range allLaneQs {
			q := &allLaneQs[i]
			if q.ID != "" && !q.CreatedAt.IsZero() {
				mailCreated[q.ID] = q.CreatedAt
			}
			if q.EventID != "" && !q.CreatedAt.IsZero() {
				mailCreated[q.EventID] = q.CreatedAt
			}
		}

		dirs, err := b.store.Directives().List(ctx, store.DirectiveFilter{RunID: run, ActiveOnly: true})
		if err != nil {
			return nil, err
		}
		for i := range dirs {
			d := &dirs[i]
			if !strings.HasPrefix(d.Ref, "owner_ask:") || (d.ExpiresAt != nil && !now.Before(*d.ExpiresAt)) {
				continue
			}
			a := openAsk{run: run, scope: d.Scope}
			if err := json.Unmarshal([]byte(d.Text), &a.ask); err != nil {
				return nil, fmt.Errorf("owner ask %s in run %s: %w", d.ID, run, err)
			}
			if mailbox.IsQuestionObsoleteOrClosed(ctx, b.store, run, cmp.Or(a.ask.LaneID, d.Scope), a.ask.SourceMessageID, a.ask.SourceEventID, a.ask.Question) {
				_ = b.store.Directives().UpdateStatus(ctx, run, d.ID, store.DirectiveHandled)
				continue
			}
			if srcID := cmp.Or(a.ask.SourceMessageID, a.ask.SourceEventID); srcID != "" {
				if ts, ok := mailCreated[srcID]; ok && !ts.IsZero() {
					a.age = mailbox.FormatAge(now, ts)
				} else if ts, ok := mailCreated[a.ask.SourceEventID]; ok && !ts.IsZero() {
					a.age = mailbox.FormatAge(now, ts)
				} else if !d.CreatedAt.IsZero() {
					a.age = mailbox.FormatAge(now, d.CreatedAt)
				}
			}
			out = append(out, a)
		}

		for i := range allLaneQs {
			q := allLaneQs[i]
			if q.EscalatedAskID != "" {
				continue
			}
			out = append(out, openAsk{
				run:   run,
				scope: q.LaneID,
				age:   cmp.Or(q.Age, "0s"),
				ask: controller.OwnerAsk{
					ID:       q.ID,
					RunID:    run,
					LaneID:   q.LaneID,
					Question: q.Question,
				},
			})
		}
	}
	return out, nil
}

// question renders an owner or lane ask with its run, lane, age, choices, and default.
func (b *StoreBackend) question(a *openAsk) string {
	q := a.ask.ID + ": " + a.ask.Question
	if a.run != b.runID {
		q = "[" + a.run + "] " + q
	}
	var details []string
	if l := cmp.Or(a.ask.LaneID, a.scope); l != "" {
		details = append(details, "lane "+l)
	}
	if a.age != "" {
		details = append(details, "age "+a.age)
	}
	if len(a.ask.Options) > 0 {
		details = append(details, "choices: "+strings.Join(a.ask.Options, " | "))
	}
	if a.ask.Default != "" {
		details = append(details, "default: "+a.ask.Default)
	}
	if len(details) > 0 {
		q += " (" + strings.Join(details, "; ") + ")"
	}
	return q
}

//nolint:govet // row grouping
type heldLane struct {
	run  string
	lane store.Lane
}

func (b *StoreBackend) lanes(ctx context.Context, runs []string) ([]heldLane, error) {
	var out []heldLane
	for _, run := range runs {
		lanes, err := b.store.Lanes().List(ctx, store.LaneFilter{RunID: run})
		if err != nil {
			return nil, err
		}
		for i := range lanes {
			out = append(out, heldLane{run: run, lane: lanes[i]})
		}
	}
	return out, nil
}

// Snapshot reads store state and open questions under b.mu.RLock, then
// releases b.mu before reading host-run metadata or probing GitHub PRs so
// external probes never block Attach, Command, RunID, or Questions.
func (b *StoreBackend) Snapshot(ctx context.Context) (Snapshot, error) {
	b.mu.RLock()
	st := b.store
	v, groups, err := b.view(ctx, true)
	var questions []string
	if err == nil {
		questions = make([]string, len(v.asks))
		for i := range v.asks {
			questions[i] = b.question(&v.asks[i])
		}
	}
	b.mu.RUnlock()
	if err != nil {
		return Snapshot{}, err
	}
	v.liveLanes = b.renderLiveLanes(ctx, groups)
	snap := v.snapshot(questions)
	// Lock ordering: reacquire b.mu.RLock() after external live-lane probes and
	// verify b.store == st so Attach/Close (which take b.mu.Lock()) cannot swap
	// or close the store while pending outcomes are read.
	b.mu.RLock()
	if b.store == st && st != nil {
		snap.Outcomes = b.collectPendingDispatcherOutcomes(ctx, st, v.held, v.asks)
	}
	b.mu.RUnlock()
	return snap, nil
}

// Questions returns the formatted open owner questions for all held runs.
func (b *StoreBackend) Questions(ctx context.Context) ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.questions(ctx)
}

// questions reads and formats open owner questions for all held runs; the caller holds b.mu.
func (b *StoreBackend) questions(ctx context.Context) ([]string, error) {
	held, err := b.heldRuns(ctx)
	if err != nil {
		return nil, err
	}
	asks, err := b.asks(ctx, held)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(asks))
	for i := range asks {
		out[i] = b.question(&asks[i])
	}
	return out, nil
}

// runView is what the attached run and the runs it holds look like now.
//
//nolint:govet // field order follows the doc
type runView struct {
	run       *store.Run
	held      []string
	lanes     []heldLane
	liveLanes []string
	asks      []openAsk
	cost      float64
	costState CostGuardState
	bgActive  bool
	bgRole    string
	pending   string
	desk      bool
	statuses  map[string]int
	phases    map[string]int
}

//nolint:govet // snapshot live-lane input grouping
type liveRunGroup struct {
	now           time.Time
	probe         HostProbe
	decisionStart map[string]time.Time
	runID         string
	repo          string
	lanes         []store.Lane
}

// view reads the attached run's view; the caller holds b.mu.
func (b *StoreBackend) view(ctx context.Context, includeLive bool) (runView, []liveRunGroup, error) {
	run, err := b.store.Runs().Get(ctx, b.runID)
	if err != nil {
		return runView{}, nil, err
	}
	v := runView{run: run, desk: b.frontDesk(), statuses: map[string]int{}, phases: map[string]int{}}
	var runsByID map[string]*store.Run
	if v.held, runsByID, err = b.heldRunsWithRecords(ctx); err != nil {
		return runView{}, nil, err
	}
	if b.Adoption != nil {
		if refresher, ok := b.Adoption.Driver.(interface {
			RefreshObjective(context.Context, string) (bool, error)
		}); ok {
			for _, id := range v.held {
				if changed, _ := refresher.RefreshObjective(ctx, id); changed {
					if r, getErr := b.store.Runs().Get(ctx, id); getErr == nil {
						runsByID[id] = r
					}
				}
			}
			if run, err = b.store.Runs().Get(ctx, b.runID); err == nil {
				v.run = run
			}
		}
	}
	runsByID[b.runID] = v.run
	if v.lanes, err = b.lanes(ctx, v.held); err != nil {
		return runView{}, nil, err
	}
	if v.asks, err = b.asks(ctx, v.held); err != nil {
		return runView{}, nil, err
	}
	v.pending = b.pendingWorkSummary(ctx, v.held)
	liveByRun := map[string][]store.Lane{}
	for i := range v.lanes {
		v.statuses[cmp.Or(string(v.lanes[i].lane.Status), "-")]++
		if !v.lanes[i].lane.Status.Terminal() {
			v.phases[cmp.Or(v.lanes[i].lane.Phase, "-")]++
			if includeLive {
				liveByRun[v.lanes[i].run] = append(liveByRun[v.lanes[i].run], v.lanes[i].lane)
			}
		}
	}
	now := b.now()
	start := now.UTC().Truncate(24 * time.Hour)
	var groups []liveRunGroup
	costRuns := slices.Clone(v.held)
	if b.FrontDesk != "" && !slices.Contains(costRuns, b.FrontDesk) {
		if _, fErr := b.store.Runs().Get(ctx, b.FrontDesk); fErr == nil {
			costRuns = append(costRuns, b.FrontDesk)
		}
	}
	for _, id := range costRuns {
		runLanes := liveByRun[id]
		filter := store.DecisionFilter{RunID: id, SinceTime: &start}
		if len(runLanes) > 0 {
			filter.SinceTime = nil
		}
		decs, err := b.store.Decisions().List(ctx, filter)
		if err != nil {
			return runView{}, nil, err
		}
		turnDecisionCost := map[string]float64{}
		var decisionStarts map[string]time.Time
		if len(runLanes) > 0 {
			decisionStarts = make(map[string]time.Time, len(runLanes))
		}
		for i := range decs {
			d := &decs[i]
			if d.CreatedAt.IsZero() || !d.CreatedAt.Before(start) {
				if d.Tool != "metrics.snapshot" {
					c := decisionCostUSD(d)
					v.cost += c
					if d.TurnID != "" {
						turnDecisionCost[d.TurnID] += c
					}
				}
			}
			if decisionStarts != nil && d.LaneID != "" && !d.CreatedAt.IsZero() && isPhaseStartDecision(d.Tool) {
				if d.CreatedAt.After(decisionStarts[d.LaneID]) {
					decisionStarts[d.LaneID] = d.CreatedAt
				}
			}
		}
		threads, err := b.store.Threads().ListThreads(ctx, store.ThreadFilter{RunID: id})
		if err != nil {
			return runView{}, nil, err
		}
		for i := range threads {
			role := string(threads[i].Role)
			isBgRole := threads[i].Role == store.ThreadRoleDispatcher || threads[i].Role == store.ThreadRolePlanner || role == "controller"
			if isBgRole && threads[i].Status == store.ThreadStatusRunning {
				v.bgActive = true
				v.bgRole = cmp.Or(v.bgRole, role)
			}
			turns, err := b.store.Threads().ListTurns(ctx, threads[i].ID)
			if err != nil {
				return runView{}, nil, err
			}
			for j := range turns {
				if isBgRole && turns[j].Status == store.TurnStatusRunning {
					v.bgActive = true
					v.bgRole = cmp.Or(v.bgRole, role)
				}
				if turns[j].StartedAt.IsZero() || !turns[j].StartedAt.Before(start) {
					tc := turnCostUSD(&turns[j], role)
					decCost := turnDecisionCost[turns[j].ID]
					if rem := tc - decCost; rem > 0 {
						v.cost += rem
					}
				}
			}
			if len(turns) == 0 && (threads[i].UpdatedAt.IsZero() || !threads[i].UpdatedAt.Before(start)) {
				v.cost += threadCostUSD(&threads[i])
			}
		}
		evCost, err := b.eventCostToday(ctx, b.store, id, start)
		if err != nil {
			return runView{}, nil, err
		}
		v.cost += evCost
		if settings, sErr := b.store.Runs().GetSettings(ctx, id); sErr == nil {
			if !settings.CostGuardDisabled && settings.MaxCostPerHourUSD > 0 {
				hourly, hErr := store.HourlyRunCostUSD(ctx, b.store, id, now)
				if hErr != nil {
					return runView{}, nil, hErr
				}
				if st := ClassifyCostGuard(hourly, settings.MaxCostPerHourUSD); st > v.costState {
					v.costState = st
				}
			}
			if settings.Paused {
				for i := range decs {
					if decs[i].Tool == "cost.guard" && (decs[i].CreatedAt.IsZero() || !decs[i].CreatedAt.Before(start)) {
						v.costState = CostStateOverLimit
						break
					}
				}
			}
		}
		if len(runLanes) > 0 {
			var repo string
			if r := runsByID[id]; r != nil {
				repo = r.Repository
			}
			groups = append(groups, liveRunGroup{
				now:           now,
				probe:         b.HostProbe,
				decisionStart: decisionStarts,
				runID:         id,
				repo:          repo,
				lanes:         runLanes,
			})
		}
	}
	return v, groups, nil
}

func decisionCostUSD(d *store.Decision) float64 {
	if d.CostUSD > 0 {
		return d.CostUSD
	}
	return store.EstimateUsageCostUSD(cmp.Or(d.Model, d.Actor), d.InputTokens, d.OutputTokens)
}

func turnCostUSD(t *store.Turn, role string) float64 {
	if t.CostUSD > 0 {
		return t.CostUSD
	}
	return store.EstimateUsageCostUSD(role, t.InputTokens, t.OutputTokens)
}

func threadCostUSD(th *store.Thread) float64 {
	if th.CostUSD > 0 {
		return th.CostUSD
	}
	return store.EstimateUsageCostUSD(string(th.Role), th.InputTokens, th.OutputTokens)
}

func (b *StoreBackend) eventCostToday(ctx context.Context, st store.Store, runID string, start time.Time) (float64, error) {
	b.cacheMu.Lock()
	var state runUsageCache
	if b.usageCache != nil {
		if prev, ok := b.usageCache[runID]; ok && prev.store == st && prev.day.Equal(start) {
			state = prev
		}
	}
	if state.termCost == nil {
		state = runUsageCache{day: start, store: st, termCost: map[string]float64{}}
	}
	afterSeq := state.afterSeq
	b.cacheMu.Unlock()

	evs, err := st.Events().List(ctx, store.EventFilter{RunID: runID, AfterSeq: afterSeq})
	if err != nil {
		return 0, err
	}
	if len(evs) == 0 {
		return state.total, nil
	}

	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.usageCache == nil {
		b.usageCache = map[string]runUsageCache{}
	}
	cur, ok := b.usageCache[runID]
	if !ok || cur.store != st || !cur.day.Equal(start) || cur.termCost == nil {
		cur = runUsageCache{day: start, store: st, termCost: map[string]float64{}}
	}
	for i := range evs {
		if evs[i].Seq > cur.afterSeq {
			cur.afterSeq = evs[i].Seq
		}
		if (!evs[i].CreatedAt.IsZero() && evs[i].CreatedAt.Before(start)) || !strings.HasPrefix(evs[i].Source, "nebula:usage:") || len(evs[i].Payload) == 0 {
			continue
		}
		var p struct {
			Usage *struct {
				EstimatedCostUSD float64 `json:"estimatedCostUsd"`
			} `json:"usage"`
			TerminalID string `json:"terminalId"`
		}
		if json.Unmarshal(evs[i].Payload, &p) == nil && p.Usage != nil && p.Usage.EstimatedCostUSD > 0 {
			key := cmp.Or(p.TerminalID, evs[i].Source)
			if p.Usage.EstimatedCostUSD > cur.termCost[key] {
				cur.total += p.Usage.EstimatedCostUSD - cur.termCost[key]
				cur.termCost[key] = p.Usage.EstimatedCostUSD
			}
		}
	}
	b.usageCache[runID] = cur
	return cur.total, nil
}

func (b *StoreBackend) renderLiveLanes(ctx context.Context, groups []liveRunGroup) []string {
	if len(groups) == 0 {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, snapshotPRProbeTimeout)
	defer cancel()
	// Each live lane writes to its own preallocated items[i] slot under wg; sem
	// has capacity 4 so goroutines acquire a token before probing and release it
	// before wg.Done(), and wg.Wait() completes before items is joined.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	groupItems := make([][]string, len(groups))
	for gi := range groups {
		g := &groups[gi]
		hr, _ := b.ReadHostRun(g.runID)
		hostLanes := make(map[string]adopt.Lane, len(hr.Lanes))
		for i := range hr.Lanes {
			hostLanes[hr.Lanes[i].ID] = hr.Lanes[i]
		}
		repo := cmp.Or(g.repo, hr.Repo)
		items := make([]string, len(g.lanes))
		groupItems[gi] = items
		for i := range g.lanes {
			l := g.lanes[i]
			hl := hostLanes[l.ID]
			decStart := g.decisionStart[l.ID]
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				phase := cmp.Or(l.Phase, string(l.Status), "-")
				desc := fmt.Sprintf("%s %s r%d", l.ID, phase, max(l.Round, 1))
				if pr := b.cachedLanePRDesc(probeCtx, g.probe, g.now, repo, hr.Dir, &l); pr != "" {
					desc += " " + pr
				}
				if start := resolveLanePhaseStart(decStart, hl, &l); !start.IsZero() {
					desc += " " + phaseElapsed(g.now.Sub(start))
				}
				items[idx] = desc
			}(i)
		}
	}
	wg.Wait()
	out := make([]string, len(groups))
	for gi := range groups {
		joined := strings.Join(groupItems[gi], ", ")
		if len(groups) > 1 {
			joined = groups[gi].runID + ": " + joined
		}
		out[gi] = joined
	}
	return out
}

func (b *StoreBackend) cachedLanePRDesc(ctx context.Context, probe HostProbe, now time.Time, repo, dir string, l *store.Lane) string {
	if l.PR <= 0 {
		return ""
	}
	var state string
	if probe.PR != nil {
		num := strconv.Itoa(l.PR)
		key := repo + "\x00" + dir + "\x00" + num
		var cached cachedPR
		var hasCached, fresh bool
		b.cacheMu.Lock()
		if b.prCache != nil {
			cached, hasCached = b.prCache[key]
			if hasCached && !now.Before(cached.at) && now.Sub(cached.at) < prCacheTTL && cached.phase == l.Phase && cached.round == l.Round {
				fresh = true
			}
		}
		b.cacheMu.Unlock()
		if fresh {
			if cached.status.State != "" {
				state = cached.status.String()
			}
		} else {
			probeCtx, cancel := context.WithTimeout(ctx, snapshotPRProbeTimeout)
			pr, err := probe.PR(probeCtx, repo, dir, num)
			cancel()
			nextStatus := cached.status
			if err == nil && pr.State != "" {
				nextStatus = pr
			}
			b.cacheMu.Lock()
			if b.prCache == nil {
				b.prCache = make(map[string]cachedPR)
			}
			b.prCache[key] = cachedPR{at: now, phase: l.Phase, round: l.Round, status: nextStatus}
			b.cacheMu.Unlock()
			if nextStatus.State != "" {
				state = nextStatus.String()
			}
		}
	}
	if state == "" {
		switch {
		case l.MergeSHA != "":
			state = "merged"
		case l.Checks != "":
			state = l.Checks
		}
	}
	if state != "" {
		return fmt.Sprintf("PR #%d (%s)", l.PR, state)
	}
	return fmt.Sprintf("PR #%d", l.PR)
}

func isPhaseStartDecision(tool string) bool {
	switch tool {
	case "lane.launch", "lane.advance", "lane.rework", "lane.retry", "lane.start", "lane.resume":
		return true
	}
	return false
}

func resolveLanePhaseStart(decisionStart time.Time, hl adopt.Lane, l *store.Lane) time.Time {
	if !decisionStart.IsZero() {
		return decisionStart
	}
	if first, _, _ := strings.Cut(strings.TrimSpace(hl.LastEvent), " "); first != "" {
		if t, err := time.Parse(time.RFC3339, first); err == nil {
			return t
		}
	}
	if !l.CreatedAt.IsZero() {
		return l.CreatedAt
	}
	return l.UpdatedAt
}

func phaseElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Round(time.Minute)/time.Minute))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour).Round(time.Minute) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// goal names the attached run's goal, the held runs, or says the chat is at the front desk.
func (v *runView) goal() string {
	if v.desk {
		if len(v.held) > 1 {
			return "adopted: " + strings.Join(v.held[1:], ", ")
		}
		return "front desk: no run yet"
	}
	return "goal: " + cmp.Or(strings.TrimSpace(v.run.Objective), "(none)")
}

func (v *runView) snapshot(questions []string) Snapshot {
	lead := v.goal()
	if !v.desk && len(v.held) > 1 {
		lead += " | adopted: " + strings.Join(v.held[1:], ", ")
	}
	totals := fmt.Sprintf("lanes: %s | open questions: %d | cost today: $%.2f",
		laneCounts(len(v.lanes), v.statuses, v.phases), len(v.asks), v.cost)
	if v.pending != "" {
		totals += " | " + v.pending
	}
	status := lead + " | " + totals
	if len(v.liveLanes) > 0 {
		status = lead + " | " + strings.Join(v.liveLanes, " | ") + "\n" + totals
	}
	return Snapshot{
		Status:           status,
		Questions:        questions,
		BackgroundActive: v.bgActive,
		BackgroundRole:   v.bgRole,
		CostState:        v.costState,
	}
}

// pendingWorkSummary keeps owner-requested work visible in the pinned footer.
// Open backlog items are queued work; a failed lane.spawn is called out on the
// item itself so it cannot disappear between dispatcher turns.
func (b *StoreBackend) pendingWorkSummary(ctx context.Context, runs []string) string {
	var items []string
	seenItems := map[string]bool{}
	for _, runID := range runs {
		backlog, err := b.store.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
		if err == nil {
			failed := b.failedSpawnItems(ctx, runID)
			for i := range backlog {
				if backlog[i].Status != store.BacklogOpen {
					continue
				}
				label := backlog[i].ID
				if failed[backlog[i].ID] {
					label += " (spawn failed)"
				}
				items = append(items, label)
				seenItems[backlog[i].ID] = true
			}
		}
		dirs, err := b.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
		if err == nil {
			for i := range dirs {
				d := &dirs[i]
				if d.Source != store.DirectiveSourceOwner && d.Source != store.DirectiveSourceConcierge {
					continue
				}
				if d.Kind == store.DirectiveStanding || strings.HasPrefix(d.Ref, "chat:") {
					continue
				}
				label := strings.TrimSpace(d.Text)
				if label == "" {
					label = strings.TrimSpace(d.Ref)
				}
				if label != "" {
					skip := false
					for id := range seenItems {
						if Mentions(label, id) {
							skip = true
							break
						}
					}
					if skip {
						continue
					}
					items = append(items, oneLineSummary(label))
				}
			}
		}
	}
	if len(items) == 0 {
		return ""
	}
	const maxVisible = 3
	total := len(items)
	visible := items
	if len(visible) > maxVisible {
		visible = visible[:maxVisible]
	}
	more := total - len(visible)
	if more > 0 {
		return fmt.Sprintf("📋 %d queued: %s, +%d more", total, strings.Join(visible, ", "), more)
	}
	return fmt.Sprintf("📋 %d queued: %s", total, strings.Join(visible, ", "))
}

func (b *StoreBackend) failedSpawnItems(ctx context.Context, runID string) map[string]bool {
	out := map[string]bool{}
	decs, err := b.store.Decisions().List(ctx, store.DecisionFilter{RunID: runID, Actor: "dispatcher"})
	if err != nil {
		return out
	}
	for i := range decs {
		d := &decs[i]
		if d.Tool != "lane.spawn" || !spawnDecisionFailed(d) {
			continue
		}
		var p struct {
			ItemID string `json:"item_id"`
		}
		if json.Unmarshal(d.Args, &p) != nil || strings.TrimSpace(p.ItemID) == "" {
			continue
		}
		out[p.ItemID] = true
		for j := i + 1; j < len(decs); j++ {
			later := &decs[j]
			if later.Tool != "lane.spawn" {
				continue
			}
			var laterParams struct {
				ItemID string `json:"item_id"`
			}
			if json.Unmarshal(later.Args, &laterParams) != nil || laterParams.ItemID != p.ItemID {
				continue
			}
			if !spawnDecisionFailed(later) {
				delete(out, p.ItemID)
			}
			break
		}
	}
	return out
}

// statusReport is /status: the run and its goal, lanes by status and phase,
// open questions, held runs and today's cost. The caller holds b.mu.
func (b *StoreBackend) statusReport(ctx context.Context) (string, error) {
	v, _, err := b.view(ctx, false)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s | run: %s\n", v.goal(), b.runID)
	fmt.Fprintf(&out, "lanes: %s\n", laneCounts(len(v.lanes), v.statuses, v.phases))
	fmt.Fprintf(&out, "open questions: %d\n", len(v.asks))
	for i := range v.asks {
		fmt.Fprintf(&out, "  %s\n", b.question(&v.asks[i]))
	}
	switch {
	case len(v.held) > 1:
		fmt.Fprintf(&out, "held runs: %s (attached); adopted: %s\n", b.runID, strings.Join(v.held[1:], ", "))
	case v.desk:
		out.WriteString("held runs: none (/runs lists runs and host swarms)\n")
	default:
		fmt.Fprintf(&out, "held runs: %s (attached)\n", b.runID)
	}
	if rules := b.standingRules(ctx, v.held); len(rules) > 0 {
		fmt.Fprintf(&out, "standing rules: %d\n", len(rules))
		for i := range rules {
			fmt.Fprintf(&out, "  %s\n", rules[i])
		}
	}
	fmt.Fprintf(&out, "cost today: $%.2f", v.cost)
	return out.String(), nil
}

func (b *StoreBackend) standingRules(ctx context.Context, held []string) []string {
	now := b.now().UTC()
	seenGlobal := map[string]bool{}
	var out []string
	for _, runID := range held {
		prefix := ""
		if len(held) > 1 {
			prefix = "[" + runID + "] "
		}
		for _, r := range concierge.ListStandingRules(ctx, b.store, runID, now) {
			if r.RunID == "" {
				if seenGlobal[r.ID] {
					continue
				}
				seenGlobal[r.ID] = true
			}
			out = append(out, prefix+r.FormatLine(false))
		}
	}
	return out
}

// splitAdopted partitions adopted runs into those with a live controller and those without.
func (b *StoreBackend) splitAdopted(adopted []string) (live, dead []string) {
	if b.Adoption == nil {
		return adopted, nil
	}
	active, ok := b.Adoption.Driver.(interface{ Active(string) bool })
	if !ok {
		return adopted, nil
	}
	for _, id := range adopted {
		if active.Active(id) {
			live = append(live, id)
		} else {
			dead = append(dead, id)
		}
	}
	return live, dead
}

// liveHeld returns the non-front-desk attached run and live adopted runs, plus
// any adopted runs without a live controller. The caller holds b.mu.
func (b *StoreBackend) liveHeld(ctx context.Context) (runs, dead []string, err error) {
	held, err := b.heldRuns(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Journal ownership can outlive the process that owns the controller. Do
	// not acknowledge an instruction unless an adopted controller is live.
	runs, dead = b.splitAdopted(held[1:])
	if !b.frontDesk() {
		runs = append([]string{b.runID}, runs...)
	}
	return runs, dead, nil
}

func errNoLiveController(id string) error {
	return fmt.Errorf("adopted run %s has no live controller; name it after it is active", id)
}

// target selects the held run an owner instruction addresses. The caller holds b.mu.
func (b *StoreBackend) target(ctx context.Context, text string) (runID, refusal string, err error) {
	runs, dead, err := b.liveHeld(ctx)
	if err != nil {
		return "", "", err
	}
	for _, id := range dead {
		if Mentions(text, id) {
			return "", "", errNoLiveController(id)
		}
	}
	switch len(runs) {
	case 0:
		return "", "", nil
	case 1:
		return runs[0], "", nil
	}
	if named := named(runs, text); len(named) == 1 {
		return named[0], "", nil
	}
	lanes, err := b.lanes(ctx, runs)
	if err != nil {
		return "", "", err
	}
	var byLane []string
	for i := range lanes {
		if l := &lanes[i]; Mentions(text, l.lane.ID) && !slices.Contains(byLane, l.run) {
			byLane = append(byLane, l.run)
		}
	}
	if len(byLane) == 1 {
		return byLane[0], "", nil
	}
	asks, err := b.asks(ctx, runs)
	if err != nil {
		return "", "", err
	}
	var asking []string
	for i := range asks {
		if run := asks[i].run; !slices.Contains(asking, run) {
			asking = append(asking, run)
		}
	}
	if len(asking) == 1 {
		return asking[0], "", nil
	}
	if b.frontDesk() {
		return "", fmt.Sprintf("Not queued: this chat holds runs %s; name the run in the instruction, then send it again: %s", strings.Join(runs, ", "), text), nil
	}
	if len(runs) == 2 {
		run, err := b.store.Runs().Get(ctx, b.runID)
		if err != nil {
			return "", "", err
		}
		if !b.live(run) {
			return runs[1], "", nil
		}
	}
	return b.runID, "", nil
}

// named returns the runs text mentions.
func named(runs []string, text string) []string {
	return slices.DeleteFunc(slices.Clone(runs), func(id string) bool { return !Mentions(text, id) })
}

// OwnerRun resolves the run for a concierge directive, validating explicit ids.
func (b *StoreBackend) OwnerRun(ctx context.Context, run, text string) (string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if run = strings.TrimSpace(run); run != "" {
		runs, dead, err := b.liveHeld(ctx)
		if err != nil {
			return "", err
		}
		if slices.Contains(dead, run) {
			return "", errNoLiveController(run)
		}
		if !slices.Contains(runs, run) {
			return "", fmt.Errorf("run %s is not held by this chat (held: %s); /adopt or /runs it first", run, cmp.Or(strings.Join(runs, ", "), "none"))
		}
		return run, nil
	}
	target, refusal, err := b.target(ctx, text)
	switch {
	case err != nil:
		return "", err
	case refusal != "":
		return "", errors.New(refusal)
	case target != "":
		return target, nil
	}
	target, refusal, err = b.frontDeskTarget(ctx, text)
	switch {
	case err != nil:
		return "", err
	case refusal != "":
		return "", errors.New(refusal)
	}
	if _, err := b.store.Runs().Get(ctx, target); err != nil {
		return "", fmt.Errorf("run %s is not held by this chat; /runs %s first", target, target)
	}
	return target, nil
}

// Wake asks an adopted controller driving runID to act on new events now.
func (b *StoreBackend) Wake(runID string) {
	if b.Adoption == nil {
		return
	}
	if w, ok := b.Adoption.Driver.(interface{ Wake(string) }); ok {
		w.Wake(runID)
	}
}

// Mentions reports whether text contains id as a whole word.
func Mentions(text, id string) bool {
	return store.Mentions(text, id)
}

// directive adds an owner directive to runID in st. /undo revokes the last
// one written to the attached store.
func (b *StoreBackend) directive(ctx context.Context, st store.Store, runID string, kind store.DirectiveKind, text, ref string) error {
	id := "chat-" + rand.Text()
	err := st.Directives().Add(ctx, store.Directive{ID: id, RunID: runID, Kind: kind, Source: store.DirectiveSourceOwner, Text: text, Ref: ref, Status: store.DirectiveActive})
	if err == nil && st == b.store {
		b.lastDirective = lastDirective{run: runID, id: id}
	}
	return err
}

// ownerMessage queues an urgent dispatcher event and wakes an adopted
// controller driving runID.
func (b *StoreBackend) ownerMessage(ctx context.Context, st store.Store, runID, body, summary string) error {
	payload, err := json.Marshal(mailbox.Message{Owner: true, From: "owner", Body: body})
	if err != nil {
		return err
	}
	if _, err := st.Events().Append(ctx, store.Event{RunID: runID, Kind: store.EventOwnerMessage, Source: "owner", Summary: cmp.Or(summary, body), Payload: payload, Urgent: true}); err != nil {
		return err
	}
	b.Wake(runID)
	return nil
}

// RecordProposal records a pending action proposal in the open-question store
// so the router's Open questions block includes it and a subsequent owner
// confirmation ("yes fund it", "yes", "do it") routes to the dispatcher with
// the proposal's self-contained spec.
func (b *StoreBackend) RecordProposal(ctx context.Context, runID, summary, spec string) (string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.recordProposalLocked(ctx, b.store, runID, summary, spec)
}

func (b *StoreBackend) recordProposalLocked(ctx context.Context, st store.Store, runID, summary, spec string) (string, error) {
	if st == nil {
		return "", nil
	}
	spec = strings.TrimSpace(spec)
	summary = strings.TrimSpace(summary)
	if spec == "" {
		spec = summary
	}
	if spec == "" {
		return "", nil
	}
	if summary != "" && !strings.Contains(spec, summary) {
		spec = summary + ": " + spec
	}
	spec = truncateProposalSpec(spec, 4096)
	targetRun := strings.TrimSpace(runID)
	if targetRun == "" {
		targetRun = b.proposalTargetRun(ctx, spec)
	}
	if targetRun == "" {
		return "", nil
	}
	if dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: targetRun, ActiveOnly: true}); err == nil {
		for i := range dirs {
			if strings.HasPrefix(dirs[i].Ref, "owner_ask:"+ProposalAskIDPrefix) {
				_ = st.Directives().UpdateStatus(ctx, targetRun, dirs[i].ID, store.DirectiveHandled)
			}
		}
	}
	askID := ProposalAskIDPrefix + strings.ToLower(rand.Text()[:8])
	ask := controller.OwnerAsk{
		ID:       askID,
		RunID:    targetRun,
		Question: spec,
	}
	raw, err := json.Marshal(ask)
	if err != nil {
		return "", err
	}
	if err := st.Directives().Add(ctx, store.Directive{
		ID:        "ask-" + askID,
		RunID:     targetRun,
		Kind:      store.DirectiveInstruction,
		Source:    store.DirectiveSourceConcierge,
		Text:      string(raw),
		Ref:       "owner_ask:" + askID,
		ExpiresAt: func() *time.Time { deadline := b.now().Add(proposalTTL); return &deadline }(),
		Status:    store.DirectiveActive,
	}); err != nil {
		return "", err
	}
	b.cacheMu.Lock()
	if b.seenAsks == nil {
		b.seenAsks = map[string]bool{}
	}
	b.seenAsks[seenAskKey(targetRun, askID)] = true
	b.cacheMu.Unlock()
	return askID, nil
}

// proposalTargetRun selects the run a pending proposal belongs to; caller holds b.mu.
func (b *StoreBackend) proposalTargetRun(ctx context.Context, text string) string {
	held, err := b.heldRuns(ctx)
	if err != nil || len(held) == 0 {
		return ""
	}
	if namedHeld := named(held, text); len(namedHeld) == 1 {
		return namedHeld[0]
	}
	if lanes, lErr := b.lanes(ctx, held); lErr == nil {
		var byLane []string
		for i := range lanes {
			if l := &lanes[i]; Mentions(text, l.lane.ID) && !slices.Contains(byLane, l.run) {
				byLane = append(byLane, l.run)
			}
		}
		if len(byLane) == 1 {
			return byLane[0]
		}
	}
	if len(held) > 1 {
		if len(held) == 2 {
			if b.frontDesk() {
				return held[1]
			}
			if run, rErr := b.store.Runs().Get(ctx, b.runID); rErr == nil && !b.live(run) {
				return held[1]
			}
		}
		// An unqualified offer is unsafe when more than one held run could
		// receive it. Require the offer to name a unique run or lane.
		return ""
	}
	if b.frontDesk() {
		// An empty front-desk chat asking "Shall I start a run to ...?" is
		// handled by concierge run.start confirmation, not a dispatcher proposal.
		if strings.Contains(strings.ToLower(text), "start a run") {
			return ""
		}
		tRun, refusal, tErr := b.frontDeskTarget(ctx, text)
		if tErr == nil && refusal == "" && tRun != "" {
			if _, gErr := b.store.Runs().Get(ctx, tRun); gErr == nil {
				return tRun
			}
		}
		return ""
	}
	if strings.Contains(strings.ToLower(text), "start a run") {
		if run, rErr := b.store.Runs().Get(ctx, b.runID); rErr != nil || !b.live(run) {
			return ""
		}
	}
	return b.runID
}

func (b *StoreBackend) latestProposalID(ctx context.Context) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.store == nil {
		return ""
	}
	held, err := b.heldRuns(ctx)
	if err != nil {
		return ""
	}
	var latest string
	for _, rID := range held {
		dirs, dErr := b.store.Directives().List(ctx, store.DirectiveFilter{RunID: rID, ActiveOnly: true})
		if dErr != nil {
			continue
		}
		for i := range dirs {
			if after, ok := strings.CutPrefix(dirs[i].Ref, "owner_ask:"+ProposalAskIDPrefix); ok {
				latest = ProposalAskIDPrefix + after
			}
		}
	}
	return latest
}

func (b *StoreBackend) recordExchange(ownerText, orcaText string) {
	ownerText = strings.TrimSpace(ownerText)
	orcaText = strings.TrimSpace(orcaText)
	if ownerText == "" && orcaText == "" {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if ownerText == "" && len(b.exchanges) > 0 && b.exchanges[len(b.exchanges)-1].orca == "" {
		b.exchanges[len(b.exchanges)-1].orca = orcaText
		return
	}
	b.exchanges = append(b.exchanges, chatExchange{owner: ownerText, orca: orcaText})
	if len(b.exchanges) > 6 {
		b.exchanges = slices.Clone(b.exchanges[len(b.exchanges)-6:])
	}
}

func (b *StoreBackend) recentExchanges(limit int) []chatExchange {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if len(b.exchanges) == 0 || limit <= 0 {
		return nil
	}
	start := max(len(b.exchanges)-limit, 0)
	return slices.Clone(b.exchanges[start:])
}

func truncateExchangePart(s string, maxBytes int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if len(s) <= maxBytes {
		return s
	}
	for n := maxBytes; n > 0; n-- {
		if utf8.RuneStart(s[n]) {
			return s[:n] + "..."
		}
	}
	return s[:maxBytes] + "..."
}

func (b *StoreBackend) conversationWindow(ctx context.Context, st store.Store, runID string) string {
	ex := b.recentExchanges(3)
	if len(ex) == 0 && st != nil {
		// Fall back to persisted concierge turns if no in-memory exchange is cached.
		var threadRuns []string
		if b.runID != "" {
			threadRuns = append(threadRuns, b.runID)
		}
		if runID != "" && !slices.Contains(threadRuns, runID) {
			threadRuns = append(threadRuns, runID)
		}
		for _, rID := range threadRuns {
			threads, err := st.Threads().ListThreads(ctx, store.ThreadFilter{RunID: rID, Role: store.ThreadRoleConcierge})
			if err != nil {
				continue
			}
			for i := range threads {
				turns, tErr := st.Threads().ListTurns(ctx, threads[i].ID)
				if tErr != nil {
					continue
				}
				for j := range turns {
					tr := &turns[j]
					if tr.Status != store.TurnStatusCompleted {
						continue
					}
					items, _ := st.Threads().ListItems(ctx, tr.ThreadID, tr.ID, 0, 0)
					var reply string
					for k := range items {
						if items[k].Kind == store.ItemKindAssistantMessage && strings.TrimSpace(items[k].Content) != "" {
							reply = strings.TrimSpace(items[k].Content)
						}
					}
					if strings.TrimSpace(tr.Input) != "" || reply != "" {
						ex = append(ex, chatExchange{owner: tr.Input, orca: reply})
					}
				}
			}
		}
		if len(ex) > 3 {
			ex = ex[len(ex)-3:]
		}
	}
	if len(ex) == 0 {
		return ""
	}
	var parts []string
	for i := range ex {
		ownerPart := truncateExchangePart(ex[i].owner, 160)
		orcaPart := truncateExchangePart(ex[i].orca, 320)
		switch {
		case ownerPart != "" && orcaPart != "":
			parts = append(parts, fmt.Sprintf("[turn %d] owner: %s -> orca: %s", i+1, ownerPart, orcaPart))
		case ownerPart != "":
			parts = append(parts, fmt.Sprintf("[turn %d] owner: %s", i+1, ownerPart))
		case orcaPart != "":
			parts = append(parts, fmt.Sprintf("[turn %d] orca: %s", i+1, orcaPart))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Recent chat context: " + strings.Join(parts, " | ")
}

func (b *StoreBackend) withConversationWindow(text, convWindow string) string {
	if convWindow == "" || strings.Contains(text, convWindow) {
		return text
	}
	return text + " (" + convWindow + ")"
}

func (b *StoreBackend) activeProposal(ctx context.Context, st store.Store, runID string) (*store.Directive, controller.OwnerAsk) {
	if st == nil {
		return nil, controller.OwnerAsk{}
	}
	candidates := []string{runID}
	if b.runID != "" && !slices.Contains(candidates, b.runID) {
		candidates = append(candidates, b.runID)
	}
	now := b.now()
	var bestDir *store.Directive
	var bestAsk controller.OwnerAsk
	for _, rID := range candidates {
		if rID == "" {
			continue
		}
		dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: rID, ActiveOnly: true})
		if err != nil {
			continue
		}
		for i := range dirs {
			d := dirs[i]
			if !strings.HasPrefix(d.Ref, "owner_ask:"+ProposalAskIDPrefix) {
				continue
			}
			if d.ExpiresAt != nil && !now.Before(*d.ExpiresAt) {
				continue
			}
			var ask controller.OwnerAsk
			if err := json.Unmarshal([]byte(d.Text), &ask); err != nil {
				continue
			}
			bestDir = &d
			bestAsk = ask
		}
	}
	return bestDir, bestAsk
}

func matchesProposal(text string) bool {
	// Only the explicit short replies are answers to a pending offer. Do not
	// consume negations, questions, or arbitrary pronoun-bearing messages as
	// approvals of an action.
	return IsOfferReply(text)
}

func truncateProposalSpec(spec string, maxBytes int) string {
	if maxBytes <= 0 || len(spec) <= maxBytes {
		return spec
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(spec[:cut]) {
		cut--
	}
	return spec[:cut]
}

func (b *StoreBackend) bareReplyUnresolved(ctx context.Context, st store.Store, runID, text string) bool {
	propDir, _ := b.activeProposal(ctx, st, runID)
	hasAsk := b.hasNonProposalOpenAsk(ctx, st, runID)
	convWin := b.conversationWindow(ctx, st, runID)
	return propDir == nil && !hasAsk && convWin == "" && IsBareConfirmationOrChitChat(text) && HasReferentPronoun(text)
}

func (b *StoreBackend) hasNonProposalOpenAsk(ctx context.Context, st store.Store, runID string) bool {
	if st == nil || runID == "" {
		return false
	}
	now := b.now()
	if qs, err := mailbox.OpenLaneQuestions(ctx, st, runID, now); err == nil && len(qs) > 0 {
		return true
	}
	dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err != nil {
		return false
	}
	for i := range dirs {
		ref := dirs[i].Ref
		if strings.HasPrefix(ref, "owner_ask:") && !strings.HasPrefix(ref, "owner_ask:"+ProposalAskIDPrefix) {
			return true
		}
	}
	return false
}

func (b *StoreBackend) live(run *store.Run) bool {
	return run.Lease != nil && !run.Lease.Expired(b.now())
}

func (b *StoreBackend) Dispatch(ctx context.Context, d router.Decision, text string, emit func(string)) error {
	if d.Handler == router.Concierge {
		if b.Stream == nil {
			return fmt.Errorf("concierge unavailable")
		}
		beforeProp := b.latestProposalID(ctx)
		var replyBuf strings.Builder
		wrapEmit := func(s string) {
			replyBuf.WriteString(s)
			if emit != nil {
				emit(s)
			}
		}
		err := b.Stream(ctx, text, wrapEmit)
		if err == nil {
			reply := strings.TrimSpace(replyBuf.String())
			b.recordExchange(text, reply)
			if ContainsOffer(reply) && b.latestProposalID(ctx) == beforeProp {
				_, _ = b.RecordProposal(ctx, "", "", reply)
			}
		}
		return err
	}
	if d.Handler != router.Planner && d.Handler != router.Dispatcher {
		return fmt.Errorf("unsupported handler %q", d.Handler)
	}
	if d.Handler == router.Planner && b.PlannerStream != nil && router.IsPlannerChatQuestion(text) {
		handled, pErr := b.dispatchPlannerStream(ctx, text, emit)
		if handled || pErr != nil {
			return pErr
		}
	}
	b.mu.RLock()
	st := b.store
	runID, ack, err := b.target(ctx, text)
	var (
		queuedDispatcher    bool
		unresolvedBareReply bool
		stepStore           store.Store
		closeStepStore      bool
	)
	switch {
	case err != nil || ack != "":
	case runID == "":
		frontDesk := b.deliverFromFrontDesk(ctx, d.Handler, text)
		runID, stepStore, closeStepStore, ack, unresolvedBareReply, err = frontDesk.target, frontDesk.store, frontDesk.closeStore, frontDesk.ack, frontDesk.unresolvedBareReply, frontDesk.err
		queuedDispatcher = err == nil && d.Handler == router.Dispatcher && runID != ""
	default:
		if d.Handler == router.Dispatcher {
			b.seedSeenDispatcherRun(ctx, st, runID)
			unresolvedBareReply = b.bareReplyUnresolved(ctx, st, runID, text)
		}
		var live bool
		live, err = b.deliver(ctx, st, runID, d.Handler, text)
		queuedDispatcher = err == nil && d.Handler == router.Dispatcher && (live || runID != b.runID)
		stepStore = st
		switch {
		case runID != b.runID:
			ack = fmt.Sprintf("Queued for the %s of adopted run %s: %s", d.Handler, runID, text)
		case live:
			ack = fmt.Sprintf("Queued for the %s: %s", d.Handler, text)
		default:
			ack = fmt.Sprintf("Saved for the %s of run %s, but no controller is running it; it will act when one starts: %s", d.Handler, b.runID, text)
		}
	}
	b.mu.RUnlock()
	if closeStepStore && stepStore != nil {
		defer func() { _ = stepStore.Close() }()
	}
	if queuedDispatcher && stepStore != nil {
		b.stepAdopted(ctx, runID)
		var outcomes []string
		if closeStepStore {
			outcomes = b.collectDispatcherOutcomes(ctx, stepStore, []string{runID}, false)
		} else {
			b.mu.RLock()
			if b.store == stepStore {
				outcomes = b.collectDispatcherOutcomes(ctx, stepStore, []string{runID}, false)
			}
			b.mu.RUnlock()
		}
		if unresolvedBareReply && (len(outcomes) == 0 || (len(outcomes) == 1 && outcomes[0] == "dispatcher: no action taken")) {
			outcomes = []string{fmt.Sprintf("dispatcher: What does %q refer to? No pending proposal or recent context resolves \"it\".", text)}
		}
		if len(outcomes) > 0 {
			ack += "\n" + strings.Join(outcomes, "\n")
		} else if !closeStepStore {
			b.markPendingOutcomeRun(runID)
		}
	} else if unresolvedBareReply {
		ack += fmt.Sprintf("\ndispatcher: What does %q refer to? No pending proposal or recent context resolves \"it\".", text)
	}
	if err == nil && emit != nil {
		emit(ack)
	}
	return err
}

func (b *StoreBackend) dispatchPlannerStream(ctx context.Context, text string, emit func(string)) (bool, error) {
	b.mu.RLock()
	st := b.store
	runID, refusal, err := b.target(ctx, text)
	var (
		stepStore      = st
		closeStepStore bool
	)
	if err == nil && refusal == "" && runID == "" {
		target, fdRefusal, fdErr := b.frontDeskTarget(ctx, text)
		switch {
		case fdErr != nil || fdRefusal != "":
			refusal, err = fdRefusal, fdErr
		case target != "":
			runID = target
			if _, gErr := st.Runs().Get(ctx, target); gErr != nil {
				if b.OpenRun == nil {
					err = fmt.Errorf("run %s is not reachable from this chat", target)
				} else if stepStore, err = b.OpenRun(ctx, target); err == nil {
					closeStepStore = true
				}
			}
		}
	}
	b.mu.RUnlock()
	if closeStepStore && stepStore != nil {
		defer func() { _ = stepStore.Close() }()
	}
	if err != nil {
		return true, err
	}
	if refusal != "" {
		if emit != nil {
			emit(refusal)
		}
		return true, nil
	}
	if runID == "" || stepStore == nil {
		return false, nil
	}
	pErr := b.PlannerStream(ctx, stepStore, runID, text, emit)
	if errors.Is(pErr, ErrPlannerFallbackToQueue) {
		return false, nil
	}
	return true, pErr
}

// deliver writes an owner instruction for handler to runID in st and reports
// whether a controller holds the run's lease and so will act on it now.
func (b *StoreBackend) deliver(ctx context.Context, st store.Store, runID string, handler router.Handler, text string) (bool, error) {
	propDir, propAsk := b.activeProposal(ctx, st, runID)
	convWindow := b.conversationWindow(ctx, st, runID)
	switch handler {
	case router.Planner:
		delivered := text
		if propDir != nil && matchesProposal(text) {
			_ = st.Directives().UpdateStatus(ctx, propDir.RunID, propDir.ID, store.DirectiveHandled)
			if IsRejection(text) {
				run, err := st.Runs().Get(ctx, runID)
				if err != nil {
					return false, err
				}
				return b.live(run), nil
			}
			delivered = fmt.Sprintf("%s (owner: %s)", strings.TrimSpace(propAsk.Question), text)
			if convWindow != "" {
				delivered = b.withConversationWindow(delivered, convWindow)
			}
		} else if convWindow != "" && (IsOfferReply(text) || HasReferentPronoun(text)) {
			delivered = b.withConversationWindow(text, convWindow)
		}
		if err := b.directive(ctx, st, runID, store.DirectivePlanImport, delivered, ""); err != nil {
			return false, err
		}
	case router.Dispatcher:
		switch {
		case propDir != nil && matchesProposal(text):
			_ = st.Directives().UpdateStatus(ctx, propDir.RunID, propDir.ID, store.DirectiveHandled)
			if IsRejection(text) {
				summary := fmt.Sprintf("Owner rejected proposal %q (owner: %s)", strings.TrimSpace(propAsk.Question), text)
				if err := b.ownerMessage(ctx, st, runID, text, b.withConversationWindow(summary, convWindow)); err != nil {
					return false, err
				}
			} else {
				delivered := fmt.Sprintf("%s (owner: %s)", strings.TrimSpace(propAsk.Question), text)
				if err := b.directive(ctx, st, runID, store.DirectiveInstruction, delivered, ""); err != nil {
					return false, err
				}
				if err := b.ownerMessage(ctx, st, runID, delivered, b.withConversationWindow(delivered, convWindow)); err != nil {
					return false, err
				}
			}
		case IsBareConfirmationOrChitChat(text):
			// A bare confirmation, rejection, or chit-chat ("yes", "no", "ok", "thanks", "yes fund it")
			// with no resolved proposal must not become a standing DirectiveInstruction.
			hasOpenAsk := b.hasNonProposalOpenAsk(ctx, st, runID)
			summary := text
			if convWindow != "" {
				summary = b.withConversationWindow(text, convWindow)
			} else if !hasOpenAsk {
				summary = fmt.Sprintf("%s (unresolved confirmation: no pending proposal, open question, or recent chat context resolves what %q refers to; ask in chat via owner.ask what \"it\" is and do not record a standing directive)", text, text)
			}
			if err := b.ownerMessage(ctx, st, runID, text, summary); err != nil {
				return false, err
			}
		default:
			if err := b.directive(ctx, st, runID, store.DirectiveInstruction, text, ""); err != nil {
				return false, err
			}
			if err := b.ownerMessage(ctx, st, runID, text, b.withConversationWindow(text, convWindow)); err != nil {
				return false, err
			}
		}
	}
	run, err := st.Runs().Get(ctx, runID)
	if err != nil {
		return false, err
	}
	return b.live(run), nil
}

// frontDeskTarget selects a live run from ListRuns for a front-desk instruction.
func (b *StoreBackend) frontDeskTarget(ctx context.Context, text string) (runID, refusal string, err error) {
	var candidates []store.Run
	if b.ListRuns != nil {
		listed, err := b.ListRuns(ctx)
		if err != nil {
			return "", "", err
		}
		candidates = append(candidates, listed...)
	}
	var live, dead []string
	for i := range candidates {
		r := &candidates[i]
		if r.ID == b.runID {
			continue
		}
		if b.live(r) {
			if !slices.Contains(live, r.ID) {
				live = append(live, r.ID)
			}
		} else if Mentions(text, r.ID) && !slices.Contains(dead, r.ID) {
			dead = append(dead, r.ID)
		}
	}
	if len(dead) > 0 {
		return "", fmt.Sprintf("Not queued: run %s has no live controller, so nothing would act on it and it was not saved. Start a run, or attach to one with /runs <id>, then send it again: %s", dead[0], text), nil
	}
	if named := named(live, text); len(named) > 0 {
		live = named
	}
	switch len(live) {
	case 0:
		return "", fmt.Sprintf("Not queued: no controller is running for this chat, so nothing would act on it and it was not saved. Start a run, or attach to one with /runs <id>, then send it again: %s", text), nil
	case 1:
		return live[0], "", nil
	default:
		return "", fmt.Sprintf("Not queued: controllers are running runs %s; attach to one with /runs <id>, then send it again: %s", strings.Join(live, ", "), text), nil
	}
}

// frontDeskDelivery carries the selected run and its delivery result.
type frontDeskDelivery struct {
	store               store.Store
	err                 error
	target, ack         string
	closeStore          bool
	unresolvedBareReply bool
}

// deliverFromFrontDesk routes a front-desk instruction to a live held run.
func (b *StoreBackend) deliverFromFrontDesk(ctx context.Context, handler router.Handler, text string) frontDeskDelivery {
	target, refusal, err := b.frontDeskTarget(ctx, text)
	if err != nil || refusal != "" {
		return frontDeskDelivery{ack: refusal, err: err}
	}
	var (
		st                  store.Store
		closeStore          bool
		unresolvedBareReply bool
	)
	st = b.store
	if _, err := st.Runs().Get(ctx, target); err != nil {
		if b.OpenRun == nil {
			return frontDeskDelivery{err: fmt.Errorf("run %s is not reachable from this chat", target)}
		}
		if st, err = b.OpenRun(ctx, target); err != nil {
			return frontDeskDelivery{err: err}
		}
		closeStore = true
	}
	if handler == router.Dispatcher {
		b.seedSeenDispatcherRun(ctx, st, target)
		unresolvedBareReply = b.bareReplyUnresolved(ctx, st, target, text)
	}
	if live, err := b.deliver(ctx, st, target, handler, text); err != nil || !live {
		if closeStore && st != nil {
			_ = st.Close()
		}
		// The lease lapsed between listing and writing.
		return frontDeskDelivery{ack: fmt.Sprintf("Saved for the %s of run %s, but its controller stopped; it will act when one starts: %s", handler, target, text), unresolvedBareReply: unresolvedBareReply, err: err}
	}
	ack := fmt.Sprintf("Queued for the %s of run %s, whose controller is running: %s", handler, target, text)
	return frontDeskDelivery{target: target, store: st, closeStore: closeStore, ack: ack, unresolvedBareReply: unresolvedBareReply}
}

func (b *StoreBackend) stepAdopted(ctx context.Context, runID string) {
	if b.Adoption == nil || runID == "" {
		return
	}
	stepper, ok := b.Adoption.Driver.(interface {
		Step(context.Context, string) error
	})
	if !ok {
		return
	}
	_ = stepper.Step(ctx, runID)
}

func seenTurnKey(runID, turnID string) string {
	return runID + "\x00" + turnID
}

func seenDecKey(runID string, seq int64) string {
	return runID + "\x00" + strconv.FormatInt(seq, 10)
}

func seenAskKey(runID, askID string) string {
	return runID + "\x00" + askID
}

func (b *StoreBackend) markPendingOutcomeRun(runID string) {
	if runID == "" {
		return
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.pendingOutcomeRuns == nil {
		b.pendingOutcomeRuns = map[string]bool{}
	}
	b.pendingOutcomeRuns[runID] = true
}

// TrackPendingOutcome marks a directive submitted by any owner-facing path for
// outcome collection on the next snapshot.
func (b *StoreBackend) TrackPendingOutcome(runID string) {
	b.markPendingOutcomeRun(strings.TrimSpace(runID))
}

func (b *StoreBackend) seedSeenDispatcherRun(ctx context.Context, st store.Store, runID string) {
	if st == nil || runID == "" {
		return
	}
	threads, _ := st.Threads().ListThreads(ctx, store.ThreadFilter{RunID: runID, Role: store.ThreadRoleDispatcher})
	decs, _ := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID, Actor: "dispatcher"})
	ctrlDecs, _ := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID, Actor: "controller"})
	dirs, _ := st.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	laneQs, _ := mailbox.OpenLaneQuestions(ctx, st, runID, b.now())
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.seenTurns == nil {
		b.seenTurns = map[string]bool{}
	}
	if b.seenDecs == nil {
		b.seenDecs = map[string]bool{}
	}
	if b.seenAsks == nil {
		b.seenAsks = map[string]bool{}
	}
	for i := range threads {
		turns, _ := st.Threads().ListTurns(ctx, threads[i].ID)
		for j := range turns {
			if turns[j].Status != store.TurnStatusRunning {
				b.seenTurns[seenTurnKey(runID, turns[j].ID)] = true
			}
		}
	}
	for i := range decs {
		b.seenDecs[seenDecKey(runID, decs[i].Seq)] = true
	}
	for i := range ctrlDecs {
		b.seenDecs[seenDecKey(runID, ctrlDecs[i].Seq)] = true
	}
	for i := range dirs {
		if after, ok := strings.CutPrefix(dirs[i].Ref, "owner_ask:"); ok && after != "" {
			b.seenAsks[seenAskKey(runID, after)] = true
		}
	}
	for i := range laneQs {
		if laneQs[i].ID != "" {
			b.seenAsks[seenAskKey(runID, laneQs[i].ID)] = true
		}
	}
}

func (b *StoreBackend) collectPendingDispatcherOutcomes(ctx context.Context, st store.Store, held []string, asks []openAsk) []string {
	b.cacheMu.Lock()
	var runs []string
	for _, id := range held {
		if b.pendingOutcomeRuns[id] {
			runs = append(runs, id)
		}
	}
	b.cacheMu.Unlock()
	var out []string
	for _, runID := range runs {
		res := b.collectDispatcherOutcomes(ctx, st, []string{runID}, true)
		if len(res) > 0 {
			b.cacheMu.Lock()
			delete(b.pendingOutcomeRuns, runID)
			b.cacheMu.Unlock()
		}
		out = append(out, res...)
	}
	for _, runID := range held {
		if slices.Contains(runs, runID) {
			continue
		}
		if res := b.collectControllerNotices(ctx, st, runID, true); len(res) > 0 {
			out = append(out, res...)
		}
	}
	for i := range asks {
		a := &asks[i]
		if strings.HasPrefix(a.ask.ID, "cost-guard-") || strings.HasPrefix(a.ask.ID, ProposalAskIDPrefix) {
			continue
		}
		id := cmp.Or(a.ask.ID, a.ask.Question)
		if id == "" {
			continue
		}
		key := seenAskKey(a.run, id)
		b.cacheMu.Lock()
		if b.seenAsks == nil {
			b.seenAsks = map[string]bool{}
		}
		seen := b.seenAsks[key]
		if !seen {
			b.seenAsks[key] = true
		}
		b.cacheMu.Unlock()
		if seen {
			continue
		}
		laneLabel := cmp.Or(a.ask.LaneID, a.scope, a.run)
		out = append(out, fmt.Sprintf("lane question (%s): %s", laneLabel, oneLineSummary(a.ask.Question)))
	}
	return out
}

func (b *StoreBackend) collectControllerNotices(ctx context.Context, st store.Store, runID string, includeBackgroundEvents bool) []string {
	if st == nil || runID == "" {
		return nil
	}
	ctrlDecs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID, Actor: "controller"})
	if err != nil {
		return nil
	}
	var out []string
	for i := range ctrlDecs {
		d := &ctrlDecs[i]
		notice := formatControllerDecisionNotice(d, includeBackgroundEvents)
		if notice == "" {
			continue
		}
		decKey := seenDecKey(runID, d.Seq)
		b.cacheMu.Lock()
		if b.seenDecs == nil {
			b.seenDecs = map[string]bool{}
		}
		seen := b.seenDecs[decKey]
		if !seen {
			b.seenDecs[decKey] = true
		}
		b.cacheMu.Unlock()
		if seen {
			continue
		}
		out = append(out, notice)
	}
	return out
}

func formatControllerDecisionNotice(d *store.Decision, includeBackgroundEvents bool) string {
	switch d.Tool {
	case mailbox.AdoptMailSummaryTool:
		return oneLineSummary(cmp.Or(d.Result, d.Reason))
	case "cost.guard":
		txt := oneLineSummary(cmp.Or(d.Result, d.Reason))
		if txt == "" {
			return ""
		}
		if !strings.HasPrefix(strings.ToLower(txt), "cost guard") {
			return "cost guard: " + txt
		}
		return txt
	}
	if !includeBackgroundEvents {
		return ""
	}
	detail := oneLineSummary(cmp.Or(d.Result, d.Reason))
	if detail == "" {
		return ""
	}
	switch d.Tool {
	case controller.ToolPhaseAdvance, controller.ToolPhaseRework, controller.ToolPhaseStart:
		if d.LaneID != "" && !strings.Contains(detail, d.LaneID) {
			return fmt.Sprintf("lane %s advanced: %s", d.LaneID, detail)
		}
		return "lane advanced: " + detail
	case controller.ToolLaneComplete:
		merged := strings.Contains(strings.ToLower(d.Reason+" "+d.Result), "merge")
		if merged {
			det := oneLineSummary(cmp.Or(d.Reason, d.Result))
			if d.LaneID != "" && !strings.Contains(det, d.LaneID) {
				return fmt.Sprintf("PR merged (%s): %s", d.LaneID, det)
			}
			return "PR merged: " + det
		}
		if d.LaneID != "" && !strings.Contains(detail, d.LaneID) {
			return fmt.Sprintf("lane %s completed: %s", d.LaneID, detail)
		}
		return "lane completed: " + detail
	}
	return ""
}

func (b *StoreBackend) collectDispatcherOutcomes(ctx context.Context, st store.Store, runs []string, includeBackgroundEvents bool) []string {
	if st == nil || len(runs) == 0 {
		return nil
	}
	var out []string
	for _, runID := range runs {
		out = append(out, b.collectControllerNotices(ctx, st, runID, includeBackgroundEvents)...)
		threads, err := st.Threads().ListThreads(ctx, store.ThreadFilter{RunID: runID, Role: store.ThreadRoleDispatcher})
		if err != nil {
			continue
		}
		decs, _ := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID, Actor: "dispatcher"})
		byTurn := map[string][]store.Decision{}
		for i := range decs {
			if decs[i].TurnID != "" {
				byTurn[decs[i].TurnID] = append(byTurn[decs[i].TurnID], decs[i])
			}
		}
		for i := range threads {
			turns, tErr := st.Threads().ListTurns(ctx, threads[i].ID)
			if tErr != nil {
				continue
			}
			for j := range turns {
				tr := &turns[j]
				if tr.Status == store.TurnStatusRunning {
					continue
				}
				turnKey := seenTurnKey(runID, tr.ID)
				b.cacheMu.Lock()
				if b.seenTurns == nil {
					b.seenTurns = map[string]bool{}
				}
				seen := b.seenTurns[turnKey]
				if !seen {
					b.seenTurns[turnKey] = true
					if b.seenDecs == nil {
						b.seenDecs = map[string]bool{}
					}
					turnDecs := byTurn[tr.ID]
					for k := range turnDecs {
						b.seenDecs[seenDecKey(runID, turnDecs[k].Seq)] = true
					}
				}
				b.cacheMu.Unlock()
				if seen {
					continue
				}
				if line := formatDispatcherTurnOutcome(ctx, st, tr, byTurn[tr.ID]); line != "" {
					out = append(out, line)
					b.recordExchange("", line)
					if ContainsOffer(line) {
						_, _ = b.recordProposalLocked(ctx, st, runID, "", strings.TrimPrefix(line, "dispatcher: "))
					}
				}
			}
		}
		for i := range decs {
			d := &decs[i]
			if d.TurnID != "" {
				continue
			}
			decKey := seenDecKey(runID, d.Seq)
			b.cacheMu.Lock()
			if b.seenDecs == nil {
				b.seenDecs = map[string]bool{}
			}
			seen := b.seenDecs[decKey]
			if !seen {
				b.seenDecs[decKey] = true
			}
			b.cacheMu.Unlock()
			if seen {
				continue
			}
			if !d.Vetted {
				res := oneLineSummary(cmp.Or(d.Result, d.Reason, d.Tool))
				if strings.HasPrefix(strings.ToLower(res), "dispatcher failed:") {
					out = append(out, res)
				} else {
					out = append(out, "dispatcher failed: "+res)
				}
			} else if d.Tool != "noop" && d.Tool != "judgment.dispatch" {
				out = append(out, fmt.Sprintf("dispatcher: %s (%s)", d.Tool, oneLineSummary(cmp.Or(d.Result, d.Reason))))
			}
		}
	}
	return out
}

func formatDispatcherTurnOutcome(ctx context.Context, st store.Store, tr *store.Turn, decs []store.Decision) string {
	items, _ := st.Threads().ListItems(ctx, tr.ThreadID, tr.ID, 0, 0)
	var assistantText string
	for i := range items {
		if items[i].Kind == store.ItemKindAssistantMessage {
			if txt := strings.TrimSpace(items[i].Content); txt != "" {
				assistantText = txt
			}
		}
	}
	if tr.Status == store.TurnStatusFailed || strings.TrimSpace(tr.Error) != "" {
		return "dispatcher failed: " + oneLineSummary(cmp.Or(tr.Error, assistantText, "turn failed"))
	}
	var vetted, rejected []string
	for i := range decs {
		d := &decs[i]
		if d.Tool == "noop" || d.Tool == "judgment.dispatch" {
			continue
		}
		detail := oneLineSummary(cmp.Or(d.Result, d.Reason))
		if d.Tool == "lane.spawn" {
			if line := formatSpawnOutcome(d, detail); line != "" {
				if d.Vetted {
					vetted = append(vetted, line)
				} else {
					rejected = append(rejected, line)
				}
				continue
			}
		}
		entry := d.Tool
		if detail != "" {
			entry += " (" + detail + ")"
		}
		if d.Vetted {
			vetted = append(vetted, entry)
		} else {
			rejected = append(rejected, entry)
		}
	}
	msg := oneLineSummary(assistantText)
	switch {
	case len(vetted) > 0:
		acts := strings.Join(vetted, "; ")
		if msg != "" && msg != acts {
			return "dispatcher: " + acts + " — " + msg
		}
		return "dispatcher: " + acts
	case len(rejected) > 0:
		rejs := "rejected " + strings.Join(rejected, "; ")
		if msg != "" {
			return "dispatcher: " + rejs + " — " + msg
		}
		return "dispatcher: " + rejs
	case msg != "":
		return "dispatcher: " + msg
	default:
		return "dispatcher: no action taken"
	}
}

func formatSpawnOutcome(d *store.Decision, detail string) string {
	var p struct {
		ItemID string `json:"item_id"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(d.Args, &p)
	item := strings.TrimSpace(p.ItemID)
	if item == "" {
		item = strings.TrimSpace(d.LaneID)
	}
	if item == "" {
		return ""
	}
	if spawnDecisionFailed(d) {
		reason := strings.TrimSpace(strings.TrimPrefix(detail, "error:"))
		return fmt.Sprintf("✗ %s spawn failed: %s", item, cmp.Or(reason, detail))
	}
	if detail == "" {
		detail = "spawned"
	}
	if p.Model != "" && !strings.Contains(detail, p.Model) {
		detail += ", model=" + p.Model
	}
	return fmt.Sprintf("▶ %s spawned (%s)", item, detail)
}

func spawnDecisionFailed(d *store.Decision) bool {
	if d == nil || d.Tool != "lane.spawn" {
		return false
	}
	if !d.Vetted {
		return true
	}
	result := strings.ToLower(strings.TrimSpace(d.Result))
	return strings.HasPrefix(result, "error:") || strings.HasPrefix(result, "rejected:") || strings.HasPrefix(result, "failed:")
}

func oneLineSummary(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if len(s) <= 220 {
		return s
	}
	for n := 220; n > 0; n-- {
		if utf8.RuneStart(s[n]) {
			return s[:n] + "..."
		}
	}
	return s[:220] + "..."
}

// switchRun attaches to id when it is one of the runs ListRuns offers.
func (b *StoreBackend) switchRun(ctx context.Context, runs []store.Run, id string) (string, error) {
	if b.OpenRun == nil {
		return "run switching unavailable", nil
	}
	if !slices.ContainsFunc(runs, func(r store.Run) bool { return r.ID == id }) {
		return "", fmt.Errorf("no run %q for this repository; /runs lists them", id)
	}
	st, err := b.OpenRun(ctx, id)
	if err != nil {
		return "", err
	}
	if err := b.Attach(st, id); err != nil {
		return "", err
	}
	return "attached to run " + id, nil
}

// Swarms lists the orca runs ListRuns offers and this machine's host swarms.
func (b *StoreBackend) Swarms(ctx context.Context) SwarmList {
	var l SwarmList
	if b.ListRuns != nil {
		l.Orca, l.OrcaErr = b.ListRuns(ctx)
	}
	switch {
	case b.HostSwarms != nil:
		l.Host, l.HostErr = b.HostSwarms(ctx)
	case b.Adoption != nil:
		l.Host, l.HostErr = b.Adoption.List(ctx)
	default:
		l.HostOff = cmp.Or(b.AdoptionUnavailable, "no host swarm discovery configured")
	}
	return l
}

// LiveSwarms is Swarms with each host lane's terminal agent state and PR
// state read live through HostProbe.
func (b *StoreBackend) LiveSwarms(ctx context.Context) SwarmList {
	return b.LiveSwarmsFiltered(ctx, "")
}

// LiveSwarmsFiltered filters Swarms by run before probing live terminal and
// PR state.
func (b *StoreBackend) LiveSwarmsFiltered(ctx context.Context, run string) SwarmList {
	l := b.Swarms(ctx).Filter(run)
	if l.HostOff == "" {
		l.Probe(ctx, b.HostProbe)
	}
	return l
}

// RunIDs lists host runs available to /adopt.
func (b *StoreBackend) RunIDs(ctx context.Context) ([]string, error) {
	if b.Adoption == nil {
		return nil, nil
	}
	rs, err := b.Adoption.List(ctx)
	ids := make([]string, 0, len(rs))
	for i := range rs {
		ids = append(ids, rs[i].ID)
	}
	slices.Sort(ids)
	return ids, err
}

func (b *StoreBackend) Command(ctx context.Context, text string) (string, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", nil
	}
	switch strings.ToLower(fields[0]) {
	case "/runs":
		if len(fields) == 1 {
			return b.Swarms(ctx).Render(b.now(), false), nil
		}
		if b.ListRuns == nil {
			return "no run discovery configured", nil
		}
		runs, err := b.ListRuns(ctx)
		if err != nil {
			return "", err
		}
		return b.switchRun(ctx, runs, fields[1])
	case "/adopt":
		if b.Adoption == nil {
			return "adoption unavailable: " + b.AdoptionUnavailable, nil
		}
		if len(fields) == 1 {
			rs, err := b.Adoption.List(ctx)
			var out []string
			for i := range rs {
				out = append(out, HostSwarmLine(&rs[i], b.now()))
			}
			if err != nil {
				out = append(out, "unreadable runs: "+err.Error())
			}
			if len(out) == 0 {
				return "no host swarm runs found", nil
			}
			return strings.Join(out, "\n"), nil
		}
		r, e := b.Adoption.Adopt(ctx, fields[1])
		if e != nil {
			return "", e
		}
		b.mu.RLock()
		adoptedStore := b.store
		b.mu.RUnlock()
		b.seedSeenDispatcherRun(ctx, adoptedStore, r.ID)
		if b.OnHoldChange != nil {
			b.OnHoldChange()
		}
		return fmt.Sprintf("adopted %s (%d lanes imported)", r.ID, len(r.Lanes)), nil
	case "/release":
		if b.Adoption == nil {
			return "adoption unavailable: " + b.AdoptionUnavailable, nil
		}
		var target string
		if len(fields) > 1 {
			target = fields[1]
		} else {
			ids, e := b.Adoption.Adopted()
			if e != nil {
				return "", e
			}
			switch len(ids) {
			case 0:
				return "no adopted swarm", nil
			case 1:
				target = ids[0]
			default:
				return "several adopted swarms; name one: /release " + strings.Join(ids, " | "), nil
			}
		}
		if e := b.Adoption.Release(ctx, target); e != nil {
			return "", e
		}
		if b.OnHoldChange != nil {
			b.OnHoldChange()
		}
		return "released " + target, nil
	case "/restart":
		if len(fields) > 2 {
			return "", fmt.Errorf("usage: /restart [path]")
		}
		if b.Restart == nil {
			return "", fmt.Errorf("restart unavailable")
		}
		path := ""
		if len(fields) == 2 {
			path = fields[1]
		}
		return b.Restart(ctx, path)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.runCommand(ctx, fields)
}

// runCommand runs a command against the attached run; the caller holds b.mu.
func (b *StoreBackend) runCommand(ctx context.Context, fields []string) (string, error) {
	switch strings.ToLower(fields[0]) {
	case "/status":
		return b.statusReport(ctx)
	case "/lanes":
		return b.lanesTable(ctx)
	case "/asks":
		qs, e := b.questions(ctx)
		if e != nil {
			return "", e
		}
		if len(qs) == 0 {
			return "no open questions", nil
		}
		return strings.Join(qs, "\n"), nil
	case "/memory":
		v, e := b.store.Memories().List(ctx, store.MemoryFilter{RunID: b.runID})
		return pretty(v, e)
	case "/plan":
		r, e := b.store.Runs().Get(ctx, b.runID)
		if e != nil {
			return "", e
		}
		return fmt.Sprintf("%s\n%s", r.Objective, r.PlanRef), nil
	case "/log":
		if len(fields) != 2 {
			return "", fmt.Errorf("usage: /log <trace>")
		}
		decs, e := b.store.Decisions().List(ctx, store.DecisionFilter{RunID: b.runID})
		if e != nil {
			return "", e
		}
		out := []store.Decision{}
		for i := range decs {
			d := decs[i]
			if d.TraceID == fields[1] {
				out = append(out, d)
			}
		}
		return pretty(out, nil)
	case "/pause", "/resume", "/stop":
		targetRun := b.runID
		if b.frontDesk() {
			tRun, refusal, tErr := b.target(ctx, "")
			if tErr != nil {
				return "", tErr
			}
			if refusal != "" {
				return refusal, nil
			}
			if tRun != "" {
				targetRun = tRun
			}
		}
		dirs, e := b.store.Directives().List(ctx, store.DirectiveFilter{RunID: targetRun, ActiveOnly: true})
		if e != nil {
			return "", e
		}
		for i := range dirs {
			d := dirs[i]
			if d.Ref == "chat:control" {
				if e := b.store.Directives().UpdateStatus(ctx, targetRun, d.ID, store.DirectiveRevoked); e != nil {
					return "", e
				}
			}
		}
		action := strings.TrimPrefix(strings.ToLower(fields[0]), "/")
		e = b.directive(ctx, b.store, targetRun, store.DirectiveInstruction, action, "chat:control")
		if e == nil {
			b.Wake(targetRun)
		}
		return "run " + action, e
	case "/undo":
		if last := b.lastDirective; last.id != "" {
			if err := b.store.Directives().UpdateStatus(ctx, last.run, last.id, store.DirectiveRevoked); err != nil {
				return "", err
			}
			b.lastDirective = lastDirective{}
			if b.store.Memories() != nil {
				_ = b.store.Memories().Delete(ctx, "", last.id)
			}
			return "directive revoked", nil
		}
		return b.memory.Undo(ctx, b.store)
	default:
		return "commands: " + commandUsage(), nil
	}
}

// lanesTable renders lanes for all held runs; the caller holds b.mu.
func (b *StoreBackend) lanesTable(ctx context.Context) (string, error) {
	runs, err := b.heldRuns(ctx)
	if err != nil {
		return "", err
	}
	lanes, err := b.lanes(ctx, runs)
	if err != nil {
		return "", err
	}
	if len(lanes) == 0 {
		if len(runs) == 1 {
			return "no lanes in run " + b.runID + "; /adopt <run-id> takes over a host swarm", nil
		}
		return "no lanes in runs " + strings.Join(runs, ", "), nil
	}
	var out strings.Builder
	w := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RUN\tLANE\tPHASE\tROUND\tSTATUS\tPR")
	for i := range lanes {
		l := &lanes[i].lane
		pr := "-"
		if l.PR > 0 {
			pr = fmt.Sprintf("#%d", l.PR)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", lanes[i].run, l.ID, cmp.Or(l.Phase, "-"), l.Round, l.Status, pr)
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func pretty(v any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	return string(raw), err
}
