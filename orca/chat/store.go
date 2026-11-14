package chat

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
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
	return b.snapshot(ctx)
}
func (b *StoreBackend) snapshot(ctx context.Context) (Snapshot, error) {
	run, err := b.store.Runs().Get(ctx, b.runID)
	if err != nil {
		return Snapshot{}, err
	}
	runs, err := b.heldRuns(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	lanes, err := b.lanes(ctx, runs)
	if err != nil {
		return Snapshot{}, err
	}
	asks, err := b.asks(ctx, runs)
	if err != nil {
		return Snapshot{}, err
	}
	start := b.now().UTC().Truncate(24 * time.Hour)
	cost := 0.0
	for _, id := range runs {
		decs, err := b.store.Decisions().List(ctx, store.DecisionFilter{RunID: id, SinceTime: &start})
		if err != nil {
			return Snapshot{}, err
		}
		for i := range decs {
			cost += decs[i].CostUSD
		}
	}
	counts := map[string]int{}
	for i := range lanes {
		counts[lanes[i].lane.Phase]++
	}
	phases := []string{}
	for k, n := range counts {
		phases = append(phases, fmt.Sprintf("%s:%d", cmp.Or(k, "-"), n))
	}
	sort.Strings(phases)
	status := "goal: " + run.Objective
	if len(runs) > 1 {
		status += " | adopted: " + strings.Join(runs[1:], ", ")
	}
	status += fmt.Sprintf(" | lanes: %d", len(lanes))
	if len(phases) > 0 {
		status += " (" + strings.Join(phases, ", ") + ")"
	}
	s := Snapshot{Status: status + fmt.Sprintf(" | open questions: %d | cost today: $%.2f", len(asks), cost)}
	for i := range asks {
		s.Questions = append(s.Questions, b.question(&asks[i]))
	}
	return s, nil
}

// target selects the held run an owner instruction for handler addresses.
// Planner instructions stay on the attached run unless it is the front desk.
// At the front desk it returns "" with no refusal when this chat holds no
// live controller, and a refusal when several held runs remain ambiguous.
// The caller holds b.mu.
func (b *StoreBackend) target(ctx context.Context, handler router.Handler, text string) (runID, refusal string, err error) {
	frontDesk := b.frontDesk()
	if handler == router.Planner && !frontDesk {
		return b.runID, "", nil
	}
	held, err := b.heldRuns(ctx)
	if err != nil {
		return "", "", err
	}
	adopted := held[1:]
	// Journal ownership can outlive the process that owns the controller. Do
	// not acknowledge an instruction unless an adopted controller is live.
	if b.Adoption != nil {
		if active, ok := b.Adoption.Driver.(interface{ Active(string) bool }); ok {
			var live []string
			for _, id := range adopted {
				if active.Active(id) {
					live = append(live, id)
				}
			}
			if len(live) == 0 {
				for _, id := range adopted {
					if mentions(text, id) {
						return "", "", fmt.Errorf("adopted run %s has no live controller; name it after it is active", id)
					}
				}
			}
			adopted = live
		}
	}
	runs := adopted
	if !frontDesk {
		runs = append([]string{b.runID}, adopted...)
	}
	switch len(runs) {
	case 0:
		return "", "", nil
	case 1:
		return runs[0], "", nil
	}
	if named := slices.DeleteFunc(slices.Clone(runs), func(id string) bool { return !mentions(text, id) }); len(named) == 1 {
		return named[0], "", nil
	}
	lanes, err := b.lanes(ctx, runs)
	if err != nil {
		return "", "", err
	}
	var byLane []string
	for i := range lanes {
		if l := &lanes[i]; mentions(text, l.lane.ID) && !slices.Contains(byLane, l.run) {
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
	if frontDesk {
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

// mentions reports whether text contains id as a whole word.
func mentions(text, id string) bool {
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
	if b.Adoption != nil {
		if w, ok := b.Adoption.Driver.(interface{ Wake(string) }); ok {
			w.Wake(runID)
		}
	}
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
	runID, ack, err := b.target(ctx, d.Handler, text)
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

// deliverFromFrontDesk sends an instruction given at the front desk, when
// this chat holds no live controller, to the one run ListRuns offers that a
// live controller holds. With none, or several, it saves nothing and says so.
func (b *StoreBackend) deliverFromFrontDesk(ctx context.Context, handler router.Handler, text string) (string, error) {
	var candidates []store.Run
	if b.ListRuns != nil {
		listed, err := b.ListRuns(ctx)
		if err != nil {
			return "", err
		}
		candidates = append(candidates, listed...)
	}
	var live []string
	for i := range candidates {
		if r := &candidates[i]; r.ID != b.runID && b.live(r) && !slices.Contains(live, r.ID) {
			live = append(live, r.ID)
		}
	}
	switch len(live) {
	case 0:
		return fmt.Sprintf("Not queued: no controller is running for this chat, so nothing would act on it and it was not saved. Start a run, or attach to one with /runs <id>, then send it again: %s", text), nil
	case 1:
	default:
		return fmt.Sprintf("Not queued: controllers are running runs %s; attach to one with /runs <id>, then send it again: %s", strings.Join(live, ", "), text), nil
	}
	target, st := live[0], b.store
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

// RunIDs lists host runs available to /adopt.
func (b *StoreBackend) RunIDs(context.Context) ([]string, error) {
	if b.Adoption == nil {
		return nil, nil
	}
	rs, err := b.Adoption.List()
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
		if b.ListRuns == nil {
			return "no run discovery configured", nil
		}
		runs, err := b.ListRuns(ctx)
		if err != nil {
			return "", err
		}
		if len(fields) > 1 {
			return b.switchRun(ctx, runs, fields[1])
		}
		var out strings.Builder
		for i := range runs {
			run := &runs[i]
			fmt.Fprintf(&out, "%s: %s\n", run.ID, cmp.Or(run.Objective, "(no goal)"))
		}
		if out.Len() == 0 {
			return "no runs", nil
		}
		return strings.TrimSpace(out.String()), nil
	case "/adopt":
		if b.Adoption == nil {
			return "adoption unavailable: " + b.AdoptionUnavailable, nil
		}
		if len(fields) == 1 {
			rs, err := b.Adoption.List()
			var out []string
			for _, r := range rs {
				out = append(out, fmt.Sprintf("%s: %s | lanes=%d | last=%s | orchestrator=%s", r.ID, r.Goal, len(r.Lanes), r.LastActivity, r.Orchestrator))
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
		s, e := b.snapshot(ctx)
		return s.Status, e
	case "/lanes":
		return b.lanesTable(ctx)
	case "/asks":
		s, e := b.snapshot(ctx)
		if e == nil && len(s.Questions) == 0 {
			return "no open questions", nil
		}
		return strings.Join(s.Questions, "\n"), e
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
