package adopt

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

// regressionMailbox models the question-id idempotency of the Nebula mailbox.
type regressionMailbox struct {
	seen      map[string]bool
	calls     []mailbox.Question
	delivered []mailbox.Question
}

func (*regressionMailbox) SendLane(context.Context, string, string) error    { return nil }
func (*regressionMailbox) Watch(context.Context) (<-chan store.Event, error) { return nil, nil }
func (m *regressionMailbox) AskOwner(_ context.Context, q mailbox.Question) (string, error) {
	m.calls = append(m.calls, q)
	if q.ID == "" || !m.seen[q.ID] {
		m.delivered = append(m.delivered, q)
		m.seen[q.ID] = true
	}
	return q.ID, nil
}

//nolint:govet // fixture fields follow the controller's dependencies.
type regressionEnv struct {
	t           *testing.T
	ctx         context.Context
	clk         *clock.FakeClock
	st          store.Store
	box         *regressionMailbox
	judge       *controller.FakeJudge
	controllers *Controllers
	cr          *controlledRun
}

func newRegressionEnv(t *testing.T) *regressionEnv {
	t.Helper()
	clk := clock.NewFakeClock(time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	box := &regressionMailbox{seen: map[string]bool{}}
	judge := controller.NewFakeJudge()
	background, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := NewControllers(background, st, lane.NewFakeDriver(), clk, func(string) controller.Judge { return judge })
	d.Mailboxes = func(string) []mailbox.Mailbox { return []mailbox.Mailbox{box} }
	emptyScript := ""
	d.ledgerScript = &emptyScript
	d.MaxCostPerHourUSD = 5
	dir := t.TempDir()
	write(t, filepath.Join(dir, leaseFile), `{"holder":"fixture","marker":"ORCHESTRATOR: orca since 2026-11-16T12:00:00Z"}`)
	write(t, filepath.Join(dir, "OBJECTIVE.md"), "Goal: regression fixture\nNext: keep working\n")
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"regression fixture","repo":"fixture/repo","phases":[{"name":"swe"},{"name":"review"},{"name":"integrate"}]},"tasks":[]}`)
	r := Run{Format: FormatNebulaSwarm, ID: "regression", Dir: dir, Goal: "regression fixture", Repo: "fixture/repo", Phases: []string{"swe", "review", "integrate"}}
	if err := d.Start(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	cr := d.runs[r.ID]
	// Quiesce only the loop, preserving the production controller and its config.
	cancel()
	awaitRegression(t, cr.done)
	workflow := recipe.Default().Clone()
	workflow.RunID, workflow.Name = r.ID, "regression"
	if _, err := st.Recipes().Save(t.Context(), workflow); err != nil {
		t.Fatal(err)
	}
	return &regressionEnv{t: t, ctx: t.Context(), clk: clk, st: st, box: box, judge: judge, controllers: d, cr: cr}
}
func awaitRegression(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("controller did not return")
	}
}
func (e *regressionEnv) step() error { return e.controllers.Step(e.ctx, "regression") }
func (e *regressionEnv) mustStep() {
	e.t.Helper()
	if err := e.step(); err != nil {
		e.t.Errorf("Step: %v", err)
	}
}
func (e *regressionEnv) settings(s store.RunSettings) {
	e.t.Helper()
	if err := e.st.Runs().UpdateSettings(e.ctx, "regression", s); err != nil {
		e.t.Fatal(err)
	}
}
func (e *regressionEnv) control(id, text string) {
	e.t.Helper()
	if err := e.st.Directives().Add(e.ctx, store.Directive{ID: id, RunID: "regression", Kind: store.DirectiveInstruction, Ref: "chat:control", Text: text, Status: store.DirectiveActive}); err != nil {
		e.t.Fatal(err)
	}
}
func (e *regressionEnv) event(ev store.Event) store.Event {
	e.t.Helper()
	ev.RunID = "regression"
	saved, err := e.st.Events().Append(e.ctx, ev)
	if err != nil {
		e.t.Fatal(err)
	}
	return saved
}
func (e *regressionEnv) cost() {
	e.t.Helper()
	if _, err := e.st.Decisions().Append(e.ctx, store.Decision{RunID: "regression", Actor: "dispatcher", Tool: "dispatcher.turn", Reason: "prior turn", Result: "ok", CostUSD: 6, Vetted: true}); err != nil {
		e.t.Fatal(err)
	}
}
func (e *regressionEnv) mailCount(want int) {
	e.t.Helper()
	e.t.Logf("owner mail: AskOwner calls=%d delivered=%d want=%d", len(e.box.calls), len(e.box.delivered), want)
	if len(e.box.calls) != want || len(e.box.delivered) != want {
		e.t.Errorf("owner mail calls/delivered=%d/%d, want %d/%d", len(e.box.calls), len(e.box.delivered), want, want)
	}
}

func TestAdoptedRegression(t *testing.T) {
	t.Run("a_cost_guard_five_steps", func(t *testing.T) {
		e := newRegressionEnv(t)
		e.cost()
		e.event(store.Event{Kind: store.EventMail, Urgent: true, Summary: "urgent pending event"})
		for range 5 {
			e.mustStep()
		}
		e.mailCount(1)
		if n := e.judge.BatchCount(); n != 0 {
			t.Errorf("judge calls=%d, want 0", n)
		}
	})
	t.Run("b_cost_drops_then_new_breach", func(t *testing.T) {
		e := newRegressionEnv(t)
		e.cost()
		e.event(store.Event{Kind: store.EventMail, Urgent: true, Summary: "urgent pending event"})
		e.mustStep()
		e.clk.Advance(time.Hour + time.Second)
		e.settings(store.RunSettings{Paused: false})
		e.mustStep()
		e.cost()
		e.event(store.Event{Kind: store.EventMail, Urgent: true, Summary: "second breach"})
		for range 5 {
			e.mustStep()
		}
		e.mailCount(2)
	})
	t.Run("e_stop_visible_preserves_adoption", func(t *testing.T) {
		e := newRegressionEnv(t)
		e.control("stop", "stop")
		if err := e.step(); !errors.Is(err, controller.ErrControllerStopped) {
			t.Errorf("Step=%v, want stopped", err)
		}
		done := make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		e.cr.done = done
		e.cr.cancel = cancel
		go e.controllers.supervise(ctx, e.cr)
		// Bound a failing baseline without sleeps or leaving its loop running.
		select {
		case <-done:
		case <-time.After(time.Second):
			cancel()
			awaitRegression(t, done)
		}
		if !strings.Contains(e.controllers.ControllerStatus("regression")+e.controllers.ControllerNotice("regression"), "stopped") {
			t.Error("stop is not visible in controller status/notice")
		}
		run, err := e.st.Runs().Get(e.ctx, "regression")
		if err != nil || run.Lease == nil {
			t.Errorf("stop released adoption: run=%+v err=%v", run, err)
		}
	})
	t.Run("l_unpause_while_over_limit_is_second_breach", func(t *testing.T) {
		e := newRegressionEnv(t)
		e.cost()
		e.event(store.Event{Kind: store.EventMail, Urgent: true, Summary: "urgent"})
		for range 5 {
			e.mustStep()
		}
		e.clk.Advance(time.Second)
		e.settings(store.RunSettings{Paused: false})
		for range 5 {
			e.mustStep()
		}
		e.mailCount(2)
		settings, err := e.st.Runs().GetSettings(e.ctx, "regression")
		if err != nil || !settings.Paused {
			t.Errorf("guard did not re-pause: %+v %v", settings, err)
		}
	})
	t.Run("m_stop_wins_over_pause", func(t *testing.T) {
		e := newRegressionEnv(t)
		e.settings(store.RunSettings{Paused: true})
		e.control("pause", "pause")
		e.control("stop", "stop")
		if err := e.step(); !errors.Is(err, controller.ErrControllerStopped) {
			t.Errorf("Step=%v, want stopped", err)
		}
		result := make(chan error, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { result <- e.cr.controller.Run(ctx) }()
		select {
		case err := <-result:
			if !errors.Is(err, controller.ErrControllerStopped) {
				t.Errorf("Run=%v, want stopped", err)
			}
		case <-time.After(time.Second):
			cancel()
			<-result
			t.Error("paused Run ignored stop")
		}
	})
}
