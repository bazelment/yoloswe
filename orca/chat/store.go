package chat

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/concierge"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/router"
	"github.com/bazelment/yoloswe/orca/store"
)

// StoreBackend attaches chat to the same durable inbox and directives as orca run.
//
// The backend owns its attached store: Attach swaps the store and run under
// one lock and closes the store it replaces, so the poll loop, View and
// commands never see a half-switched or closed store.
//
//nolint:govet
type StoreBackend struct {
	Stream   func(context.Context, string, func(string)) error
	Now      func() time.Time
	ListRuns func(context.Context) ([]store.Run, error)
	// OpenRun opens the store of a run ListRuns returned; /runs <id> attaches
	// to it.
	OpenRun func(context.Context, string) (store.Store, error)
	// OnAttach runs under the attachment lock whenever another run is attached.
	OnAttach func(store.Store, string)
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

	mu            sync.RWMutex
	store         store.Store
	runID         string
	memory        concierge.MemoryEditor
	lastDirective lastDirective
}

// lastDirective identifies the directive /undo revokes.
type lastDirective struct{ run, id string }

// NewStoreBackend returns a backend attached to runID in st. Close, or an
// Attach to another store, closes st.
func NewStoreBackend(st store.Store, runID string) *StoreBackend {
	return &StoreBackend{store: st, runID: runID}
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

// heldRuns returns the attached run followed by the adopted runs driven in
// the attached store. The caller holds b.mu.
func (b *StoreBackend) heldRuns(ctx context.Context) ([]string, error) {
	runs := []string{b.runID}
	if b.Adoption == nil {
		return runs, nil
	}
	ids, err := b.Adoption.Adopted()
	for _, id := range ids {
		if id == b.runID {
			continue
		}
		if _, getErr := b.store.Runs().Get(ctx, id); getErr == nil {
			runs = append(runs, id)
		}
	}
	return runs, err
}

// openAsk is an unanswered owner question.
type openAsk struct {
	run, scope string
	ask        controller.OwnerAsk
}

func (b *StoreBackend) asks(ctx context.Context, runs []string) ([]openAsk, error) {
	var out []openAsk
	for _, run := range runs {
		dirs, err := b.store.Directives().List(ctx, store.DirectiveFilter{RunID: run, ActiveOnly: true})
		if err != nil {
			return nil, err
		}
		for i := range dirs {
			d := &dirs[i]
			if !strings.HasPrefix(d.Ref, "owner_ask:") || (d.ExpiresAt != nil && !b.now().Before(*d.ExpiresAt)) {
				continue
			}
			a := openAsk{run: run, scope: d.Scope}
			if err := json.Unmarshal([]byte(d.Text), &a.ask); err != nil {
				return nil, fmt.Errorf("owner ask %s in run %s: %w", d.ID, run, err)
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// question renders an owner ask with its run, lane, choices, and default.
func (b *StoreBackend) question(a *openAsk) string {
	q := a.ask.ID + ": " + a.ask.Question
	if a.run != b.runID {
		q = "[" + a.run + "] " + q
	}
	var details []string
	if l := cmp.Or(a.ask.LaneID, a.scope); l != "" {
		details = append(details, "lane "+l)
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

func (b *StoreBackend) Snapshot(ctx context.Context) (Snapshot, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, err := b.view(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return v.snapshot(b), nil
}

// runView is what the attached run and the runs it holds look like now.
//
//nolint:govet // field order follows the doc
type runView struct {
	run    *store.Run
	held   []string
	lanes  []heldLane
	asks   []openAsk
	cost   float64
	desk   bool
	phases map[string]int
}

// view reads the attached run's view; the caller holds b.mu.
func (b *StoreBackend) view(ctx context.Context) (runView, error) {
	run, err := b.store.Runs().Get(ctx, b.runID)
	if err != nil {
		return runView{}, err
	}
	v := runView{run: run, desk: b.frontDesk(), phases: map[string]int{}}
	if v.held, err = b.heldRuns(ctx); err != nil {
		return runView{}, err
	}
	if v.lanes, err = b.lanes(ctx, v.held); err != nil {
		return runView{}, err
	}
	if v.asks, err = b.asks(ctx, v.held); err != nil {
		return runView{}, err
	}
	start := b.now().UTC().Truncate(24 * time.Hour)
	for _, id := range v.held {
		decs, err := b.store.Decisions().List(ctx, store.DecisionFilter{RunID: id, SinceTime: &start})
		if err != nil {
			return runView{}, err
		}
		for i := range decs {
			v.cost += decs[i].CostUSD
		}
		threads, err := b.store.Threads().ListThreads(ctx, store.ThreadFilter{RunID: id, Role: store.ThreadRoleConcierge})
		if err != nil {
			return runView{}, err
		}
		for i := range threads {
			turns, err := b.store.Threads().ListTurns(ctx, threads[i].ID)
			if err != nil {
				return runView{}, err
			}
			for j := range turns {
				if !turns[j].StartedAt.Before(start) {
					v.cost += turns[j].CostUSD
				}
			}
		}
	}
	for i := range v.lanes {
		v.phases[cmp.Or(v.lanes[i].lane.Phase, "-")]++
	}
	return v, nil
}

// goal names the attached run's goal, or says the chat is at the front desk.
func (v *runView) goal() string {
	if v.desk {
		return "front desk: no run yet"
	}
	return "goal: " + cmp.Or(strings.TrimSpace(v.run.Objective), "(none)")
}

func (v *runView) snapshot(b *StoreBackend) Snapshot {
	status := v.goal()
	if len(v.held) > 1 {
		status += " | adopted: " + strings.Join(v.held[1:], ", ")
	}
	status += " | lanes: " + countLine(len(v.lanes), v.phases)
	s := Snapshot{Status: status + fmt.Sprintf(" | open questions: %d | cost today: $%.2f", len(v.asks), v.cost)}
	for i := range v.asks {
		s.Questions = append(s.Questions, b.question(&v.asks[i]))
	}
	return s
}

// statusReport is /status: the run and its goal, lanes by phase, open
// questions, held runs and today's cost. The caller holds b.mu.
func (b *StoreBackend) statusReport(ctx context.Context) (string, error) {
	v, err := b.view(ctx)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s | run: %s\n", v.goal(), b.runID)
	fmt.Fprintf(&out, "lanes: %s\n", countLine(len(v.lanes), v.phases))
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
	fmt.Fprintf(&out, "cost today: $%.2f", v.cost)
	return out.String(), nil
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
	if id == "" {
		return false
	}
	idChar := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' }
	for i := 0; ; {
		j := strings.Index(text[i:], id)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(id)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (start == 0 || !idChar(before)) && (end == len(text) || !idChar(after)) {
			return true
		}
		i = start + 1
	}
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
func (b *StoreBackend) ownerMessage(ctx context.Context, st store.Store, runID, text string) error {
	payload, err := json.Marshal(mailbox.Message{Owner: true, From: "owner", Body: text})
	if err != nil {
		return err
	}
	if _, err := st.Events().Append(ctx, store.Event{RunID: runID, Kind: store.EventOwnerMessage, Source: "owner", Summary: text, Payload: payload, Urgent: true}); err != nil {
		return err
	}
	b.Wake(runID)
	return nil
}

func (b *StoreBackend) live(run *store.Run) bool {
	return run.Lease != nil && !run.Lease.Expired(b.now())
}

func (b *StoreBackend) Dispatch(ctx context.Context, d router.Decision, text string, emit func(string)) error {
	if d.Handler == router.Concierge {
		if b.Stream == nil {
			return fmt.Errorf("concierge unavailable")
		}
		return b.Stream(ctx, text, emit)
	}
	if d.Handler != router.Planner && d.Handler != router.Dispatcher {
		return fmt.Errorf("unsupported handler %q", d.Handler)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	runID, ack, err := b.target(ctx, text)
	switch {
	case err != nil || ack != "":
	case runID == "":
		ack, err = b.deliverFromFrontDesk(ctx, d.Handler, text)
	default:
		var live bool
		live, err = b.deliver(ctx, b.store, runID, d.Handler, text)
		switch {
		case runID != b.runID:
			ack = fmt.Sprintf("Queued for the %s of adopted run %s: %s", d.Handler, runID, text)
		case live:
			ack = fmt.Sprintf("Queued for the %s: %s", d.Handler, text)
		default:
			ack = fmt.Sprintf("Saved for the %s of run %s, but no controller is running it; it will act when one starts: %s", d.Handler, b.runID, text)
		}
	}
	if err == nil && emit != nil {
		emit(ack)
	}
	return err
}

// deliver writes an owner instruction for handler to runID in st and reports
// whether a controller holds the run's lease and so will act on it now.
func (b *StoreBackend) deliver(ctx context.Context, st store.Store, runID string, handler router.Handler, text string) (bool, error) {
	switch handler {
	case router.Planner:
		if err := b.directive(ctx, st, runID, store.DirectivePlanImport, text, ""); err != nil {
			return false, err
		}
	case router.Dispatcher:
		// Chat routing chooses the Dispatcher, but cannot infer which open ask
		// (if any) this message answers. Let the Dispatcher decide from its
		// snapshot and always preserve the owner message as a fresh instruction.
		if err := b.directive(ctx, st, runID, store.DirectiveInstruction, text, ""); err != nil {
			return false, err
		}
		if err := b.ownerMessage(ctx, st, runID, text); err != nil {
			return false, err
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

// deliverFromFrontDesk routes a front-desk instruction to a live held run.
func (b *StoreBackend) deliverFromFrontDesk(ctx context.Context, handler router.Handler, text string) (string, error) {
	target, refusal, err := b.frontDeskTarget(ctx, text)
	if err != nil || refusal != "" {
		return refusal, err
	}
	st := b.store
	if _, err := st.Runs().Get(ctx, target); err != nil {
		if b.OpenRun == nil {
			return "", fmt.Errorf("run %s is not reachable from this chat", target)
		}
		if st, err = b.OpenRun(ctx, target); err != nil {
			return "", err
		}
		defer func() { _ = st.Close() }()
	}
	if live, err := b.deliver(ctx, st, target, handler, text); err != nil || !live {
		// The lease lapsed between listing and writing.
		return fmt.Sprintf("Saved for the %s of run %s, but its controller stopped; it will act when one starts: %s", handler, target, text), err
	}
	return fmt.Sprintf("Queued for the %s of run %s, whose controller is running: %s", handler, target, text), nil
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
	l := b.Swarms(ctx)
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
		return "released " + target, nil
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
		v, e := b.view(ctx)
		if e != nil {
			return "", e
		}
		s := v.snapshot(b)
		if len(s.Questions) == 0 {
			return "no open questions", nil
		}
		return strings.Join(s.Questions, "\n"), nil
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
		dirs, e := b.store.Directives().List(ctx, store.DirectiveFilter{RunID: b.runID, ActiveOnly: true})
		if e != nil {
			return "", e
		}
		for i := range dirs {
			d := dirs[i]
			if d.Ref == "chat:control" {
				if e := b.store.Directives().UpdateStatus(ctx, b.runID, d.ID, store.DirectiveRevoked); e != nil {
					return "", e
				}
			}
		}
		action := strings.TrimPrefix(strings.ToLower(fields[0]), "/")
		e = b.directive(ctx, b.store, b.runID, store.DirectiveInstruction, action, "chat:control")
		return "run " + action, e
	case "/undo":
		if last := b.lastDirective; last.id != "" {
			b.lastDirective = lastDirective{}
			return "directive revoked", b.store.Directives().UpdateStatus(ctx, last.run, last.id, store.DirectiveRevoked)
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
