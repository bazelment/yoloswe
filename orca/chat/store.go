package chat

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/concierge"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/router"
	"github.com/bazelment/yoloswe/orca/store"
)

// StoreBackend attaches chat to the same durable inbox and directives as orca run.
//
//nolint:govet
type StoreBackend struct {
	Store         store.Store
	RunID         string
	Stream        func(context.Context, string, func(string)) error
	Now           func() time.Time
	memory        concierge.MemoryEditor
	lastDirective string
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
	if len(d.Parts) > 0 {
		for i := range d.Parts {
			part := d.Parts[i]
			if err := b.Dispatch(ctx, part, part.Text, emit); err != nil {
				return err
			}
			emit("\n")
		}
		return nil
	}
	if d.NeedsClarification {
		return fmt.Errorf("cannot dispatch an ambiguous message")
	}
	switch d.Destination {
	case router.Concierge:
		if b.Stream == nil {
			return fmt.Errorf("concierge unavailable")
		}
		return b.Stream(ctx, text, emit)
	case router.AnswerOpenQuestion:
		asks, err := b.asks(ctx)
		if err != nil {
			return err
		}
		var match *store.Directive
		for i := range asks {
			if strings.TrimPrefix(asks[i].Ref, "owner_ask:") == d.QuestionID || (d.QuestionID == "" && len(asks) == 1) {
				match = &asks[i]
			}
		}
		if match == nil {
			return fmt.Errorf("choose an open question with /asks")
		}
		_, err = mailbox.AppendEvent(ctx, b.Store.Events(), b.RunID, mailbox.Message{Owner: true, From: "owner", LaneID: match.Scope, InReplyTo: strings.TrimPrefix(match.Ref, "owner_ask:"), Body: text})
		if err == nil {
			emit("Answer queued for " + strings.TrimPrefix(match.Ref, "owner_ask:"))
		}
		return err
	case router.Planner:
		id, err := b.directive(ctx, store.DirectivePlanImport, text, "")
		if err == nil {
			emit("Plan change queued: " + id)
		}
		return err
	case router.Memory:
		typ := "fact"
		if d.IsStandingRule {
			typ = "rule"
		}
		echo, err := b.memory.Submit(ctx, b.Store, b.RunID, "chat-memory-"+rand.Text(), text, concierge.MemorySubmitParams{Action: "create", Name: text, Description: text, Body: text, Scope: "run", Type: typ})
		if err == nil {
			emit(echo)
		}
		return err
	case router.Dispatcher:
		_, err := b.directive(ctx, store.DirectiveInstruction, text, "")
		if err != nil {
			return err
		}
		_, err = mailbox.AppendEvent(ctx, b.Store.Events(), b.RunID, mailbox.Message{Owner: true, From: "owner", Body: text})
		return err
	case router.Controller:
		command := strings.ToLower(strings.TrimSpace(text))
		if strings.Contains(command, "resume") {
			command = "resume"
		} else if strings.Contains(command, "pause") {
			command = "pause"
		} else if strings.Contains(command, "stop") {
			command = "stop"
		} else {
			return fmt.Errorf("controller requires pause, resume or stop")
		}
		echo, err := b.Command(ctx, "/"+command)
		if err == nil {
			emit(echo)
		}
		return err
	}
	return fmt.Errorf("unsupported destination %q", d.Destination)
}
func (b *StoreBackend) Command(ctx context.Context, text string) (string, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", nil
	}
	switch strings.ToLower(fields[0]) {
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
		return "commands: /status /lanes /asks /memory /plan /log <trace> /pause /resume /stop /undo", nil
	}
}
func pretty(v any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	return string(raw), err
}
