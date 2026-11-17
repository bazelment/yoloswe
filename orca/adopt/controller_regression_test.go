package adopt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type regressionDriver struct {
	*lane.FakeDriver
	failure  error
	attempts int
}

func (d *regressionDriver) StartPhaseTerminal(ctx context.Context, req lane.StartPhaseRequest) (lane.StartPhaseResult, error) {
	if req.LaneID == "X" {
		d.attempts++
		if d.failure != nil {
			return lane.StartPhaseResult{}, d.failure
		}
	}
	return d.FakeDriver.StartPhaseTerminal(ctx, req)
}
func (d *regressionDriver) SpawnSession(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	if req.LaneID == "X" {
		d.attempts++
		if d.failure != nil {
			return lane.SpawnResult{}, d.failure
		}
	}
	return d.FakeDriver.SpawnSession(ctx, req)
}

type regressionStore struct {
	store.Store
	notes *regressionNotes
}

func (s *regressionStore) Notes() store.NoteStore { return s.notes }

type regressionNotes struct {
	store.NoteStore
	failure   error
	onFailure func()
}

func (s *regressionNotes) Append(ctx context.Context, note store.RunNote) (store.RunNote, error) {
	if s.failure != nil {
		if s.onFailure != nil {
			s.onFailure()
		}
		return store.RunNote{}, s.failure
	}
	return s.NoteStore.Append(ctx, note)
}

//nolint:govet // fixture fields follow the controller's dependencies.
type regressionEnv struct {
	t           *testing.T
	ctx         context.Context
	clk         *clock.FakeClock
	st          *regressionStore
	drv         *regressionDriver
	box         *regressionMailbox
	judge       *controller.FakeJudge
	controllers *Controllers
	cr          *controlledRun
}

func newRegressionEnv(t *testing.T) *regressionEnv {
	t.Helper()
	clk := clock.NewFakeClock(time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	memory := store.NewMemoryStore(clk)
	st := &regressionStore{Store: memory, notes: &regressionNotes{NoteStore: memory.Notes()}}
	t.Cleanup(func() { _ = st.Close() })
	drv := &regressionDriver{FakeDriver: lane.NewFakeDriver(), failure: errors.New("capacity refused")}
	box := &regressionMailbox{seen: map[string]bool{}}
	judge := controller.NewFakeJudge()
	background, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := NewControllers(background, st, drv, clk, func(string, func(context.Context, string, string, string) error) controller.Judge { return judge })
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
	return &regressionEnv{t: t, ctx: t.Context(), clk: clk, st: st, drv: drv, box: box, judge: judge, controllers: d, cr: cr}
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
func (e *regressionEnv) lane(id, phase string, status lane.Status) {
	e.t.Helper()
	ln := lane.Lane{ID: id, RunID: "regression", RecipeName: "regression", Status: status, Phase: phase, Round: 1, Attempt: 1, PR: 42, PRHead: "head", ApprovalSHA: "head", Checks: "green", LastEventAt: e.clk.Now()}
	if id == "X" && status == lane.StatusPlanned {
		ln.SessionID = "sess-X"
	}
	if status == lane.StatusRunning {
		ln.RecordAttempt(phase, 1, "sess-"+id, "term-"+id)
	}
	if err := e.st.Lanes().Upsert(e.ctx, ln); err != nil {
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
func (e *regressionEnv) report(id string) store.Event {
	e.t.Helper()
	ln, err := e.st.Lanes().Get(e.ctx, "regression", id)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, err := json.Marshal(lane.PhaseReport{Phase: ln.Phase, Round: 1, Outcome: lane.OutcomeDone, HeadSHA: "head", Summary: "completed work"})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.event(store.Event{ID: "report-" + id, LaneID: id, Kind: store.EventLaneReport, Payload: raw})
}
func (e *regressionEnv) cost() {
	e.t.Helper()
	if _, err := e.st.Decisions().Append(e.ctx, store.Decision{RunID: "regression", Actor: "dispatcher", Tool: "dispatcher.turn", Reason: "prior turn", Result: "ok", CostUSD: 6, Vetted: true}); err != nil {
		e.t.Fatal(err)
	}
}
func (e *regressionEnv) decisions(tool string) int {
	e.t.Helper()
	rows, err := e.st.Decisions().List(e.ctx, store.DecisionFilter{RunID: "regression"})
	if err != nil {
		e.t.Fatal(err)
	}
	count := 0
	for _, row := range rows {
		if row.Tool == tool {
			count++
		}
	}
	return count
}
func (e *regressionEnv) cursor() int64 {
	e.t.Helper()
	cur, err := e.st.Events().GetCursor(e.ctx, "regression", controller.DefaultConsumer)
	if err != nil {
		e.t.Fatal(err)
	}
	return cur.Seq
}
func (e *regressionEnv) wantPhase(id, phase string) {
	e.t.Helper()
	ln, err := e.st.Lanes().Get(e.ctx, "regression", id)
	if err != nil {
		e.t.Fatal(err)
	}
	if ln.Phase != phase {
		e.t.Errorf("%s phase=%s, want %s", id, ln.Phase, phase)
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
	for _, setting := range []bool{false, true} {
		name := "c_chat_pause"
		if setting {
			name = "d_settings_pause"
		}
		t.Run(name, func(t *testing.T) {
			e := newRegressionEnv(t)
			e.lane("Y", "review", lane.StatusRunning)
			report := e.report("Y")
			if setting {
				e.settings(store.RunSettings{Paused: true})
			} else {
				e.control("pause", "pause")
			}
			e.mustStep()
			e.wantPhase("Y", "review")
			if len(e.drv.PhaseStarts) != 0 || len(e.drv.Stops) != 0 || e.decisions(controller.ToolPhaseAdvance) != 0 {
				t.Errorf("paused lifecycle actions: starts=%v stops=%v", e.drv.PhaseStarts, e.drv.Stops)
			}
			if e.cursor() >= report.Seq {
				t.Error("paused report was consumed")
			}
			if setting {
				e.settings(store.RunSettings{})
			} else if err := e.st.Directives().UpdateStatus(e.ctx, "regression", "pause", store.DirectiveRevoked); err != nil {
				t.Fatal(err)
			}
			e.mustStep()
			e.wantPhase("Y", "integrate")
			if len(e.drv.PhaseStarts) != 1 {
				t.Errorf("starts=%d, want 1", len(e.drv.PhaseStarts))
			}
		})
	}
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
	t.Run("f_runtime_cap_six", func(t *testing.T) {
		e := newRegressionEnv(t)
		for i := range 5 {
			e.lane(fmt.Sprintf("running%d", i), "swe", lane.StatusRunning)
		}
		e.lane("ready", "swe", lane.StatusPlanned)
		e.settings(store.RunSettings{ConcurrencyCap: 6})
		e.mustStep()
		if len(e.drv.Spawns) != 1 {
			t.Errorf("spawns=%d, want 1", len(e.drv.Spawns))
		}
	})
	// (g) is the existing native runner cost-guard/pause suite, run alongside this package.
	for _, repository := range []bool{false, true} {
		name := "h_lane_failure_backoff"
		if repository {
			name = "j_repository_text_is_lane_local"
		}
		t.Run(name, func(t *testing.T) {
			e := newRegressionEnv(t)
			if repository {
				e.drv.failure = errors.New("repository is not configured")
			}
			e.lane("X", "swe", lane.StatusPlanned)
			e.lane("Y", "swe", lane.StatusRunning)
			e.report("Y")
			for range 3 {
				e.mustStep()
			}
			e.wantPhase("Y", "clean")
			if e.decisions("phase.start.failure") != 1 {
				t.Errorf("failure decisions=%d, want 1", e.decisions("phase.start.failure"))
			}
			if e.judge.BatchCount() != 1 {
				t.Errorf("judgments=%d, want 1", e.judge.BatchCount())
			}
			if !strings.Contains(e.controllers.ControllerStatus("regression"), "X") {
				t.Error("status does not name X")
			}
			if e.drv.attempts != 1 {
				t.Errorf("attempts before backoff=%d, want 1", e.drv.attempts)
			}
			e.clk.Advance(time.Second)
			e.mustStep()
			if e.drv.attempts != 2 {
				t.Errorf("attempts at 1s=%d, want 2", e.drv.attempts)
			}
			e.clk.Advance(time.Second)
			e.mustStep()
			if e.drv.attempts != 2 {
				t.Errorf("attempts at 2s=%d, want 2", e.drv.attempts)
			}
			e.clk.Advance(time.Second)
			e.mustStep()
			if e.drv.attempts != 3 {
				t.Errorf("attempts at 3s=%d, want 3", e.drv.attempts)
			}
			ln, err := e.st.Lanes().Get(e.ctx, "regression", "X")
			if err != nil {
				t.Fatal(err)
			}
			stoppedX := false
			for _, stop := range e.drv.Stops {
				if stop.LaneID == "X" {
					stoppedX = true
				}
			}
			if ln.Status != lane.StatusPlanned || stoppedX {
				t.Errorf("failure blocked/stopped X: status=%s stops=%v", ln.Status, e.drv.Stops)
			}
			if e.decisions("phase.start.failure") != 1 || e.judge.BatchCount() != 1 {
				t.Error("failure was reported more than once")
			}
		})
	}
	t.Run("i_failed_event_does_not_block_next", func(t *testing.T) {
		e := newRegressionEnv(t)
		e.lane("X", "swe", lane.StatusRunning)
		e.lane("Y", "swe", lane.StatusRunning)
		e.report("X")
		e.report("Y")
		e.mustStep()
		e.wantPhase("Y", "clean")
		if e.decisions("event.failure") != 1 {
			t.Errorf("failed-event decisions=%d, want 1", e.decisions("event.failure"))
		}
		e.mustStep()
		if e.drv.attempts != 1 {
			t.Errorf("event attempts before backoff=%d, want 1", e.drv.attempts)
		}
		e.clk.Advance(time.Second)
		e.mustStep()
		if e.drv.attempts != 2 {
			t.Errorf("event attempts after backoff=%d, want 2", e.drv.attempts)
		}
		if e.decisions("event.failure") != 1 || e.judge.BatchCount() != 1 {
			t.Error("event failure was reported more than once")
		}
	})
	t.Run("k_transient_handler_keeps_cursor", func(t *testing.T) {
		e := newRegressionEnv(t)
		e.lane("Y", "swe", lane.StatusRunning)
		first := e.report("Y")
		e.event(store.Event{Kind: store.EventMail, Urgent: true, Summary: "later event"})
		failure := errors.New("temporary store outage")
		e.st.notes.failure = failure
		if err := e.step(); !errors.Is(err, failure) {
			t.Errorf("Step=%v, want transient store error", err)
		}
		if e.cursor() >= first.Seq {
			t.Errorf("cursor=%d skipped event %d", e.cursor(), first.Seq)
		}
		if e.decisions("event.failure") != 0 {
			t.Error("transient failure recorded as failed event")
		}
		e.st.notes.failure = nil
		e.mustStep()
		e.wantPhase("Y", "clean")
	})
	for _, cause := range []error{store.ErrLeaseNotHeld, controller.ErrControllerStopped, context.Canceled} {
		t.Run("k_Run_returns_"+cause.Error(), func(t *testing.T) {
			e := newRegressionEnv(t)
			e.lane("Y", "swe", lane.StatusRunning)
			first := e.report("Y")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			e.st.notes.failure = cause
			if errors.Is(cause, context.Canceled) {
				e.st.notes.onFailure = cancel
			}
			result := make(chan error, 1)
			go func() { result <- e.cr.controller.Run(ctx) }()
			select {
			case err := <-result:
				if !errors.Is(cause, context.Canceled) && !errors.Is(err, cause) {
					t.Errorf("Run=%v, want %v", err, cause)
				}
			case <-time.After(time.Second):
				cancel()
				<-result
				t.Error("Run swallowed terminal handler error")
			}
			if e.cursor() >= first.Seq {
				t.Errorf("cursor=%d skipped event %d", e.cursor(), first.Seq)
			}
		})
	}
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

func TestAdoptedEventRetryRecovery(t *testing.T) {
	e := newRegressionEnv(t)
	e.lane("X", "swe", lane.StatusRunning)
	e.report("X")
	e.mustStep()
	restart := func() {
		t.Helper()
		if err := e.controllers.Start(e.ctx, e.cr.run); err != nil {
			t.Fatal(err)
		}
		e.cr = e.controllers.runs["regression"]
		awaitRegression(t, e.cr.done)
	}
	restart()
	if e.drv.attempts != 1 {
		t.Fatalf("restart bypassed event backoff: attempts=%d", e.drv.attempts)
	}
	e.settings(store.RunSettings{Paused: true})
	e.clk.Advance(time.Second)
	e.mustStep()
	if e.drv.attempts != 1 {
		t.Fatalf("paused retry launched: attempts=%d", e.drv.attempts)
	}
	e.settings(store.RunSettings{})
	e.drv.failure = nil
	e.mustStep()
	e.wantPhase("X", "clean")
	if e.drv.attempts != 2 {
		t.Errorf("recovered attempts=%d, want 2", e.drv.attempts)
	}
	if e.decisions("event.failure") != 1 || e.decisions("event.retry.complete") != 1 || e.judge.BatchCount() != 1 {
		t.Error("retry lost or duplicated its failure/completion/judgment")
	}
	restart()
	e.clk.Advance(time.Second)
	e.mustStep()
	if e.drv.attempts != 2 {
		t.Errorf("completed retry replayed: attempts=%d", e.drv.attempts)
	}
}

func TestAdoptedDriverControlErrorsKeepCursor(t *testing.T) {
	for _, cause := range []error{store.ErrLeaseNotHeld, controller.ErrControllerStopped, context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			e := newRegressionEnv(t)
			e.lane("X", "swe", lane.StatusRunning)
			first := e.report("X")
			e.drv.failure = cause
			if err := e.step(); !errors.Is(err, cause) {
				t.Errorf("Step=%v, want %v", err, cause)
			}
			if e.cursor() >= first.Seq || e.decisions("event.failure") != 0 {
				t.Error("driver control error was discarded as lane-local")
			}
		})
	}
}

func TestAdoptedMissingLaneSessionIsLocal(t *testing.T) {
	for _, status := range []lane.Status{lane.StatusPlanned, lane.StatusRunning} {
		t.Run(string(status), func(t *testing.T) {
			e := newRegressionEnv(t)
			e.drv.failure = fmt.Errorf("%w: unknown lane session", store.ErrNotFound)
			e.lane("X", "swe", status)
			e.lane("Y", "swe", lane.StatusRunning)
			if status == lane.StatusRunning {
				e.report("X")
			}
			e.report("Y")
			e.mustStep()
			e.wantPhase("Y", "clean")
			if e.judge.BatchCount() != 1 {
				t.Errorf("judgments=%d, want 1", e.judge.BatchCount())
			}
		})
	}
}
