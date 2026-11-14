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
	"time"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/concierge"
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
	// goes to the one run a live controller holds, or is refused.
	FrontDesk string

	mu            sync.RWMutex
	store         store.Store
	runID         string
	memory        concierge.MemoryEditor
	lastDirective string
}

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
	b.store, b.runID, b.lastDirective = st, runID, ""
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
func (b *StoreBackend) asks(ctx context.Context) ([]store.Directive, error) {
	dirs, err := b.store.Directives().List(ctx, store.DirectiveFilter{RunID: b.runID, ActiveOnly: true})
	if err != nil {
		return nil, err
	}
	out := []store.Directive{}
	for i := range dirs {
		d := dirs[i]
		if strings.HasPrefix(d.Ref, "owner_ask:") && (d.ExpiresAt == nil || b.now().Before(*d.ExpiresAt)) {
			out = append(out, d)
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
	lanes, err := b.store.Lanes().List(ctx, store.LaneFilter{RunID: b.runID})
	if err != nil {
		return Snapshot{}, err
	}
	asks, err := b.asks(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	start := b.now().UTC().Truncate(24 * time.Hour)
	decs, err := b.store.Decisions().List(ctx, store.DecisionFilter{RunID: b.runID, SinceTime: &start})
	if err != nil {
		return Snapshot{}, err
	}
	cost := 0.0
	for i := range decs {
		d := decs[i]
		cost += d.CostUSD
	}
	counts := map[string]int{}
	for i := range lanes {
		l := lanes[i]
		counts[l.Phase]++
	}
	phases := []string{}
	for k, n := range counts {
		phases = append(phases, fmt.Sprintf("%s:%d", k, n))
	}
	sort.Strings(phases)
	s := Snapshot{Status: fmt.Sprintf("goal: %s | lanes: %d (%s) | open questions: %d | cost today: $%.2f", run.Objective, len(lanes), strings.Join(phases, ", "), len(asks), cost)}
	for i := range asks {
		d := asks[i]
		var q struct{ ID, Question string }
		if err := json.Unmarshal([]byte(d.Text), &q); err != nil {
			return Snapshot{}, err
		}
		s.Questions = append(s.Questions, q.ID+": "+q.Question)
	}
	return s, nil
}

// directive adds an owner directive to runID in st. /undo revokes the last
// one written to the attached run.
func (b *StoreBackend) directive(ctx context.Context, st store.Store, runID string, kind store.DirectiveKind, text, ref string) error {
	id := "chat-" + rand.Text()
	err := st.Directives().Add(ctx, store.Directive{ID: id, RunID: runID, Kind: kind, Source: store.DirectiveSourceOwner, Text: text, Ref: ref, Status: store.DirectiveActive})
	if err == nil && st == b.store && runID == b.runID {
		b.lastDirective = id
	}
	return err
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
	var ack string
	var err error
	if b.FrontDesk != "" && b.runID == b.FrontDesk {
		ack, err = b.deliverFromFrontDesk(ctx, d.Handler, text)
	} else {
		var live bool
		if live, err = b.deliver(ctx, b.store, b.runID, d.Handler, text); live {
			ack = fmt.Sprintf("Queued for the %s: %s", d.Handler, text)
		} else {
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
		msg := mailbox.Message{Owner: true, From: "owner", Body: text}
		// Chat routing chooses the Dispatcher, but cannot infer which open ask
		// (if any) this message answers. Let the Dispatcher decide from its
		// snapshot and always preserve the owner message as a fresh instruction.
		if err := b.directive(ctx, st, runID, store.DirectiveInstruction, text, ""); err != nil {
			return false, err
		}
		if _, err := mailbox.AppendEvent(ctx, st.Events(), runID, msg); err != nil {
			return false, err
		}
	}
	run, err := st.Runs().Get(ctx, runID)
	if err != nil {
		return false, err
	}
	return b.live(run), nil
}

// deliverFromFrontDesk sends an instruction given at the front desk to the
// one run a live controller holds: a run this chat adopted (driven in the
// attached store) or one ListRuns offers. With none, or several, it saves
// nothing and says so.
func (b *StoreBackend) deliverFromFrontDesk(ctx context.Context, handler router.Handler, text string) (string, error) {
	var candidates []store.Run
	if b.Adoption != nil {
		ids, err := b.Adoption.Adopted()
		if err != nil {
			return "", err
		}
		for _, id := range ids {
			if run, err := b.store.Runs().Get(ctx, id); err == nil {
				candidates = append(candidates, *run)
			}
		}
	}
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
		v, e := b.store.Lanes().List(ctx, store.LaneFilter{RunID: b.runID})
		return pretty(v, e)
	case "/asks":
		s, e := b.snapshot(ctx)
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
		if b.lastDirective != "" {
			e := b.store.Directives().UpdateStatus(ctx, b.runID, b.lastDirective, store.DirectiveRevoked)
			b.lastDirective = ""
			return "directive revoked", e
		}
		return b.memory.Undo(ctx, b.store)
	default:
		return "commands: /runs [run-id] /status /lanes /asks /memory /plan /log <trace> /pause /resume /stop /undo /adopt [run-id] /release [run-id]", nil
	}
}
func pretty(v any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	return string(raw), err
}
