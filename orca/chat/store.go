package chat

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/concierge"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/router"
	"github.com/bazelment/yoloswe/orca/store"
)

// StoreBackend attaches chat to the same durable inbox and directives as orca run.
//
//nolint:govet
type StoreBackend struct {
	Store           store.Store
	RunID           string
	Stream          func(context.Context, string, func(string)) error
	Now             func() time.Time
	memory          concierge.MemoryEditor
	lastDirective   string
	Adoption        adopt.Service
	AdoptionEnabled bool
	// AdoptionRunID is the host swarm currently handed to this chat. It is
	// deliberately separate from RunID: chat may continue on its own run while
	// operating an adopted swarm.
	AdoptionRunID string
}

func (b *StoreBackend) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}
func (b *StoreBackend) asks(ctx context.Context) ([]store.Directive, error) {
	dirs, err := b.Store.Directives().List(ctx, store.DirectiveFilter{RunID: b.RunID, ActiveOnly: true})
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
	run, err := b.Store.Runs().Get(ctx, b.RunID)
	if err != nil {
		return Snapshot{}, err
	}
	lanes, err := b.Store.Lanes().List(ctx, store.LaneFilter{RunID: b.RunID})
	if err != nil {
		return Snapshot{}, err
	}
	asks, err := b.asks(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	start := b.now().UTC().Truncate(24 * time.Hour)
	decs, err := b.Store.Decisions().List(ctx, store.DecisionFilter{RunID: b.RunID, SinceTime: &start})
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
func (b *StoreBackend) directive(ctx context.Context, kind store.DirectiveKind, text, ref string) (string, error) {
	id := "chat-" + rand.Text()
	err := b.Store.Directives().Add(ctx, store.Directive{ID: id, RunID: b.RunID, Kind: kind, Source: store.DirectiveSourceOwner, Text: text, Ref: ref, Status: store.DirectiveActive})
	if err == nil {
		b.lastDirective = id
	}
	return id, err
}
func (b *StoreBackend) Dispatch(ctx context.Context, d router.Decision, text string, emit func(string)) error {
	switch d.Handler {
	case router.Concierge:
		if b.Stream == nil {
			return fmt.Errorf("concierge unavailable")
		}
		return b.Stream(ctx, text, emit)
	case router.Planner:
		_, err := b.directive(ctx, store.DirectivePlanImport, text, "")
		if err == nil && emit != nil {
			emit("Queued for the planner: " + text)
		}
		return err
	case router.Dispatcher:
		msg := mailbox.Message{Owner: true, From: "owner", Body: text}
		// Chat routing chooses the Dispatcher, but cannot infer which open ask
		// (if any) this message answers. Let the Dispatcher decide from its
		// snapshot and always preserve the owner message as a fresh instruction.
		if _, err := b.directive(ctx, store.DirectiveInstruction, text, ""); err != nil {
			return err
		}
		_, err := mailbox.AppendEvent(ctx, b.Store.Events(), b.RunID, msg)
		if err == nil && emit != nil {
			emit("Queued for the dispatcher: " + text)
		}
		return err
	}
	return fmt.Errorf("unsupported handler %q", d.Handler)
}
func (b *StoreBackend) Command(ctx context.Context, text string) (string, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", nil
	}
	switch strings.ToLower(fields[0]) {
	case "/adopt":
		if !b.AdoptionEnabled {
			return "adoption unavailable: labeler and process adapters are not configured", nil
		}
		if len(fields) == 1 {
			rs, e := b.Adoption.List()
			if e != nil {
				return "", e
			}
			var out []string
			for _, r := range rs {
				out = append(out, fmt.Sprintf("%s: %s | lanes=%d | last=%s | orchestrator=%s", r.ID, r.Goal, len(r.Lanes), r.LastActivity, r.Orchestrator))
			}
			return strings.Join(out, "\n"), nil
		}
		b.Adoption.Handover.HolderID = b.RunID
		r, e := b.Adoption.Adopt(fields[1])
		if e != nil {
			return "", e
		}
		b.AdoptionRunID = r.ID
		return "adopted " + r.ID, nil
	case "/release":
		if !b.AdoptionEnabled {
			return "adoption unavailable: labeler and process adapters are not configured", nil
		}
		b.Adoption.Handover.HolderID = b.RunID
		target := b.AdoptionRunID
		if target == "" {
			rs, e := b.Adoption.List()
			if e != nil {
				return "", e
			}
			if e := b.Adoption.ReleaseOwned(rs); e != nil {
				return "", e
			}
			return "released adopted swarm", nil
		}
		if target == "" {
			return "no adopted swarm", nil
		}
		if e := b.Adoption.Release(target); e != nil {
			return "", e
		}
		b.AdoptionRunID = ""
		return "released " + target, nil
	case "/status":
		s, e := b.Snapshot(ctx)
		return s.Status, e
	case "/lanes":
		v, e := b.Store.Lanes().List(ctx, store.LaneFilter{RunID: b.RunID})
		return pretty(v, e)
	case "/asks":
		s, e := b.Snapshot(ctx)
		return strings.Join(s.Questions, "\n"), e
	case "/memory":
		v, e := b.Store.Memories().List(ctx, store.MemoryFilter{RunID: b.RunID})
		return pretty(v, e)
	case "/plan":
		r, e := b.Store.Runs().Get(ctx, b.RunID)
		if e != nil {
			return "", e
		}
		return fmt.Sprintf("%s\n%s", r.Objective, r.PlanRef), nil
	case "/log":
		if len(fields) != 2 {
			return "", fmt.Errorf("usage: /log <trace>")
		}
		decs, e := b.Store.Decisions().List(ctx, store.DecisionFilter{RunID: b.RunID})
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
		dirs, e := b.Store.Directives().List(ctx, store.DirectiveFilter{RunID: b.RunID, ActiveOnly: true})
		if e != nil {
			return "", e
		}
		for i := range dirs {
			d := dirs[i]
			if d.Ref == "chat:control" {
				if e := b.Store.Directives().UpdateStatus(ctx, b.RunID, d.ID, store.DirectiveRevoked); e != nil {
					return "", e
				}
			}
		}
		action := strings.TrimPrefix(strings.ToLower(fields[0]), "/")
		_, e = b.directive(ctx, store.DirectiveInstruction, action, "chat:control")
		return "run " + action, e
	case "/undo":
		if b.lastDirective != "" {
			e := b.Store.Directives().UpdateStatus(ctx, b.RunID, b.lastDirective, store.DirectiveRevoked)
			b.lastDirective = ""
			return "directive revoked", e
		}
		return b.memory.Undo(ctx, b.Store)
	default:
		return "commands: /status /lanes /asks /memory /plan /log <trace> /pause /resume /stop /undo /adopt [run-id] /release", nil
	}
}
func pretty(v any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	return string(raw), err
}
