package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/adopt"
	chatui "github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/store"
)

const heldRun = "20261113-slim-prompts"

type fixedLabeler struct{}

func (fixedLabeler) RunOrchestrator(context.Context, string) (string, error)  { return "claude", nil }
func (fixedLabeler) SetRunOrchestrator(context.Context, string, string) error { return nil }

type noWatcher struct{}

func (noWatcher) Running(context.Context, string) ([]int, error) { return nil, nil }
func (noWatcher) Stop(context.Context, []int) error              { return nil }
func (noWatcher) Start(context.Context, string) error            { return nil }

// eventJudge records events delivered to an adopted dispatcher.
type eventJudge struct{ events chan store.Event }

func (j eventJudge) Judge(_ context.Context, _ string, events []store.Event) error {
	for i := range events {
		j.events <- events[i]
	}
	return nil
}

// frontDeskWithAdoptedRuns builds a front desk with live adopted controllers.
func frontDeskWithAdoptedRuns(t *testing.T, ids ...string) (*chatui.StoreBackend, store.Store, map[string]eventJudge) {
	t.Helper()
	root := t.TempDir()
	for _, id := range ids {
		dir := filepath.Join(root, ".shared", "subagent-swarm", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"slim the prompts","phases":[{"name":"swe"}]},"tasks":[{"id":"done-lane","title":"Done","phase":"swe","status":"done","round":1}]}`)
	}
	clk := clock.NewFakeClock(clock.NewRealClock().Now().UTC())
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Runs().Create(t.Context(), store.Run{ID: frontDeskRunID}); err != nil {
		t.Fatal(err)
	}
	judges := map[string]eventJudge{}
	for _, id := range ids {
		judges[id] = eventJudge{events: make(chan store.Event, 64)}
	}
	ctrls := adopt.NewControllers(t.Context(), st, lane.NewFakeDriver(), clk, func(runID string, _ func(context.Context, string, string, string) error) controller.Judge {
		return judges[runID]
	})
	t.Cleanup(func() { _ = ctrls.Close(context.Background()) })
	b := chatui.NewStoreBackend(st, frontDeskRunID)
	b.FrontDesk, b.Now = frontDeskRunID, clk.Now
	b.Adoption = &adopt.Service{
		Importer: adopt.Importer{Roots: []string{root}},
		Labeler:  fixedLabeler{},
		Watcher:  noWatcher{},
		Holder:   adopt.ChatHolder(frontDeskRunID),
		Driver:   ctrls,
	}
	for _, id := range ids {
		if out, err := b.Command(t.Context(), "/adopt "+id); err != nil || !strings.HasPrefix(out, "adopted "+id) {
			t.Fatalf("/adopt %s = %q, %v", id, out, err)
		}
	}
	return b, st, judges
}

func conciergeDirectives(t *testing.T, st store.Store, runID string) []store.Directive {
	t.Helper()
	dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Directive
	for i := range dirs {
		if dirs[i].Source == store.DirectiveSourceConcierge {
			out = append(out, dirs[i])
		}
	}
	return out
}

// A concierge directive is stored on and wakes the run it targets.
func TestConciergeDirectiveLandsOnTheRunItIsAbout(t *testing.T) {
	t.Parallel()
	const hold = "hold: start no new lanes until I say otherwise"
	for _, tc := range []struct {
		name, owner, args, answer string
		held                      []string
	}{
		{
			name:   "run_id names a held run",
			owner:  "hold, start no new lanes until I say otherwise",
			args:   `{"kind":"instruction","text":"` + hold + `","run_id":"` + heldRun + `"}`,
			answer: "I submitted the hold directive.",
			held:   []string{heldRun, "20261113-other"},
		},
		{
			name:   "owner text names a held run",
			owner:  "for run " + heldRun + ": hold, start no new lanes until I say otherwise",
			args:   `{"kind":"instruction","text":"` + hold + `"}`,
			answer: "I submitted a directive scoped to run " + heldRun + ".",
			held:   []string{heldRun, "20261113-other"},
		},
		{
			name:   "no run named, one held run",
			owner:  "hold, start no new lanes until I say otherwise",
			args:   `{"kind":"instruction","text":"` + hold + `"}`,
			answer: "Done.",
			held:   []string{heldRun},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, st, judges := frontDeskWithAdoptedRuns(t, tc.held...)
			c := NewConcierge(frontDeskRunID, st, nil, scriptedModel(
				toolUse("dir", ToolDirectiveSubmit, tc.args),
				textAnswer(tc.answer),
			), DefaultConciergeModel)
			wireChatConcierge(c, b)
			var streamed strings.Builder
			if err := c.AskStream(t.Context(), tc.owner, func(s string) { streamed.WriteString(s + "\n") }); err != nil {
				t.Fatal(err)
			}
			dirs := conciergeDirectives(t, st, heldRun)
			if len(dirs) != 1 || dirs[0].Text != hold || dirs[0].Kind != store.DirectiveInstruction {
				t.Fatalf("directives on %s = %+v, want the hold", heldRun, dirs)
			}
			for _, other := range append([]string{frontDeskRunID}, tc.held[1:]...) {
				if got := conciergeDirectives(t, st, other); len(got) != 0 {
					t.Fatalf("directives on %s = %+v, want none", other, got)
				}
			}
			if !chatui.Mentions(streamed.String(), heldRun) {
				t.Fatalf("reply %q does not name run %s", streamed.String(), heldRun)
			}
			deadline := time.After(10 * time.Second)
			for {
				select {
				case ev := <-judges[heldRun].events:
					if ev.Kind == store.EventOwnerMessage && ev.Source == ActorConcierge && strings.Contains(ev.Summary, hold) {
						if ev.RunID != heldRun {
							t.Fatalf("owner event run = %q, want %q", ev.RunID, heldRun)
						}
						return
					}
				case <-deadline:
					t.Fatalf("the dispatcher of %s never saw the hold", heldRun)
				}
			}
		})
	}
}

// The reply identifies the stored run even when the model omits it.
func TestConciergeDirectiveReplyQuotesStoredRun(t *testing.T) {
	t.Parallel()
	b, st, _ := frontDeskWithAdoptedRuns(t, heldRun)
	c := NewConcierge(frontDeskRunID, st, nil, scriptedModel(
		toolUse("dir", ToolDirectiveSubmit, `{"kind":"instruction","text":"hold"}`),
		textAnswer("I submitted the directive."),
	), DefaultConciergeModel)
	wireChatConcierge(c, b)
	answer, err := c.Ask(t.Context(), "hold everything")
	if err != nil {
		t.Fatal(err)
	}
	dirs := conciergeDirectives(t, st, heldRun)
	if len(dirs) != 1 {
		t.Fatalf("directives on %s = %+v", heldRun, dirs)
	}
	if want := "I submitted the directive.\nDirective " + dirs[0].ID + " is saved on run " + heldRun + "."; answer != want {
		t.Fatalf("answer = %q, want %q", answer, want)
	}
}

// An unheld explicit run is rejected without writing a directive.
func TestConciergeDirectiveRefusesUnheldRun(t *testing.T) {
	t.Parallel()
	b, st, _ := frontDeskWithAdoptedRuns(t, heldRun)
	c := NewConcierge(frontDeskRunID, st, nil, scriptedModel(
		toolUse("dir", ToolDirectiveSubmit, `{"kind":"instruction","text":"hold","run_id":"run-elsewhere"}`),
		textAnswer("I could not save it: this chat does not hold run-elsewhere."),
	), DefaultConciergeModel)
	wireChatConcierge(c, b)
	if _, err := c.Ask(t.Context(), "hold run-elsewhere"); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{frontDeskRunID, heldRun, "run-elsewhere"} {
		if got := conciergeDirectives(t, st, run); len(got) != 0 {
			t.Fatalf("directives on %s = %+v, want none", run, got)
		}
	}
}
