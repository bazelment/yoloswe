package adopt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

// TestAdoptedHostContract uses only synthetic ledgers and a local bare remote.
func TestAdoptedHostContract(t *testing.T) {
	for _, name := range []string{"a capacity from live host", "a blocked live terminal", "b configured target lanes", "c waits release slots", "d red then pending then green", "d headless green report", "d recorded host gate", "e open integrate report", "e squash merge", "e wrong target", "e host merge sha", "e imported done", "e report merge sha", "f draft", "f host hold"} {
		t.Run(name, func(t *testing.T) {
			dir, repo, head := integrationFixture(t, "missing", "main", 42, false, "running")
			write(t, filepath.Join(dir, leaseFile), `{"holder":"fixture","marker":"ORCHESTRATOR: orca since 2026-11-16T12:00:00Z"}`)
			config := map[string]any{"repo": repo, "target": "main", "base": "main", "merge": "squash", "merge_gate": "verdict+ci", "phases": []map[string]string{{"name": "swe"}, {"name": "review"}, {"name": "integrate"}}}
			if name == "e wrong target" {
				config["target"] = "feature"
				base := strings.TrimSpace(gitTest(t, repo, "rev-parse", "main"))
				gitTest(t, repo, "push", "origin", base+":refs/heads/feature")
			}
			tasks := []map[string]any{}
			add := func(id, phase, status, terminal string) {
				tasks = append(tasks, map[string]any{"id": id, "title": id, "phase": phase, "status": status, "round": 1, "session_id": "sess-" + id, "terminals": map[string]map[string]string{phase: {"1": terminal}}, "pr": 42, "pr_head": head, "approval_sha": head, "checks": "green"})
			}
			switch name[0] {
			case 'a', 'b', 'c':
				count := 6
				if name[0] == 'c' {
					count = 4
					config["target_lanes"] = 5
					add("wait-1", "swe", "running", "")
					tasks[len(tasks)-1]["waiting_on"] = "host soak"
					add("wait-2", "swe", "running", "")
					tasks[len(tasks)-1]["waiting_on"] = "owner deadline"
				}
				for i := 0; i < count; i++ {
					id := fmt.Sprintf("live-%d", i)
					status := "running"
					if name == "a blocked live terminal" && i == 0 {
						status = "blocked"
					}
					add(id, "swe", status, "term-"+id)
				}
				add("planned", "swe", "planned", "")
				if name[0] == 'b' {
					config["target_lanes"] = 9
				}
			default:
				phase := "review"
				if name[0] == 'e' {
					phase = "integrate"
				}
				add("lane-1", phase, "running", "term-lane-1")
				if name == "e imported done" {
					tasks[0]["status"] = "done"
				}
				if name[0] == 'd' {
					tasks[0]["checks"] = "red"
				}
				if name == "f draft" {
					tasks[0]["pr_draft"] = true
				}
				if name == "f host hold" {
					tasks[0]["hold"] = "owner approval"
				}
			}
			data, err := json.Marshal(map[string]any{"config": config, "tasks": tasks})
			if err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "state.json"), string(data))
			write(t, filepath.Join(dir, "OBJECTIVE.md"), "Goal: host contract fixture\nMerge gate: retain soak PASS\n")
			write(t, filepath.Join(dir, "CONTEXT.md"), "Merge gate: staging soak PASS\n")
			if name == "d recorded host gate" {
				write(t, filepath.Join(dir, "lane-1.hold-advance"), "Retain this owner approval gate after advancing.")
			}
			if name == "e report merge sha" {
				write(t, filepath.Join(dir, "reports/lane-1.integrate.r1.md"), "Outcome: done\nHead: "+head+"\nMerge SHA: "+head+"\nPR: #42\n")
			}
			r, err := Read(dir)
			if err != nil {
				t.Fatal(err)
			}
			clk := clock.NewFakeClock(time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
			st := store.NewMemoryStore(clk)
			t.Cleanup(func() { _ = st.Close() })
			drv := lane.NewFakeDriver()
			bg, cancel := context.WithCancel(t.Context())
			cancel() // Run is quiescent; each assertion drives exactly one Step.
			d := NewControllers(bg, st, drv, clk, nil)
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			emptyScript := ""
			d.ledgerScript = &emptyScript
			if err := st.Runs().Create(t.Context(), store.Run{ID: r.ID}); err != nil {
				t.Fatal(err)
			}
			if err := st.Runs().UpdateSettings(t.Context(), r.ID, store.RunSettings{Paused: true}); err != nil {
				t.Fatal(err)
			}
			if err := d.Start(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			awaitRegression(t, d.runs[r.ID].done)
			if name == "d recorded host gate" {
				if err := os.Remove(filepath.Join(dir, "lane-1.hold-advance")); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.Runs().UpdateSettings(t.Context(), r.ID, store.RunSettings{}); err != nil {
				t.Fatal(err)
			}
			step := func() {
				t.Helper()
				if err := d.Step(t.Context(), r.ID); err != nil {
					t.Fatal(err)
				}
			}
			get := func(id string) *lane.Lane {
				t.Helper()
				ln, err := st.Lanes().Get(t.Context(), r.ID, id)
				if err != nil {
					t.Fatal(err)
				}
				return ln
			}
			appendEvent := func(id string, kind store.EventKind, payload any) {
				t.Helper()
				b, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.Events().Append(t.Context(), store.Event{ID: id, RunID: r.ID, LaneID: "lane-1", Kind: kind, Payload: b}); err != nil {
					t.Fatal(err)
				}
			}
			workflow, err := st.Recipes().Get(t.Context(), r.ID, "adopted", 0)
			if err != nil {
				t.Fatal(err)
			}
			switch name[0] {
			case 'a', 'b', 'c':
				want := map[byte]int{'a': 7, 'b': 9, 'c': 5}[name[0]]
				if workflow.Limits.MaxConcurrentLanes != want {
					t.Errorf("adopted cap=%d, want %d", workflow.Limits.MaxConcurrentLanes, want)
				}
				// Isolate the slot rule from recipe mapping in case (c).
				if name[0] == 'c' {
					if err := st.Runs().UpdateSettings(t.Context(), r.ID, store.RunSettings{ConcurrencyCap: 5}); err != nil {
						t.Fatal(err)
					}
				}
				if name[0] == 'b' {
					if err := st.Runs().UpdateSettings(t.Context(), r.ID, store.RunSettings{ConcurrencyCap: 6}); err != nil {
						t.Fatal(err)
					}
					step()
					if get("planned").Status != lane.StatusPlanned {
						t.Fatal("owner cap did not override host cap")
					}
					if err := st.Runs().UpdateSettings(t.Context(), r.ID, store.RunSettings{ConcurrencyCap: 7}); err != nil {
						t.Fatal(err)
					}
				}
				step()
				if get("planned").Status != lane.StatusRunning {
					t.Error("planned lane did not start")
				}
			case 'd':
				appendEvent("review-done", store.EventLaneReport, lane.PhaseReport{Phase: "review", Round: 1, Outcome: lane.OutcomeDone, HeadSHA: head, PR: 42})
				step()
				if len(drv.PhaseStarts) != 0 || get("lane-1").MergeSHA != "" {
					t.Fatalf("red CI launched merge: %+v", drv.PhaseStarts)
				}
				if name == "d headless green report" {
					green := true
					appendEvent("headless-report", store.EventLaneReport, controller.LaneReportPayload{PhaseReport: lane.PhaseReport{Phase: "review", Round: 1, Outcome: lane.OutcomeDone, PR: 42}, Checks: "green", CIGreen: &green})
					step()
					if len(drv.PhaseStarts) != 0 || get("lane-1").Checks != "red" {
						t.Fatal("headless report overwrote exact-head red CI")
					}
				}
				appendEvent("pending", store.EventCI, controller.CIPayload{HeadSHA: head, Status: "pending", PR: 42})
				step()
				if len(drv.PhaseStarts) != 0 {
					t.Fatal("pending CI launched merge")
				}
				for _, oldHead := range []string{"", "stale-head"} {
					appendEvent("wrong-green-"+oldHead, store.EventCI, controller.CIPayload{HeadSHA: oldHead, Status: "green", PR: 42})
					step()
					if len(drv.PhaseStarts) != 0 || get("lane-1").PRHead != head {
						t.Fatal("CI for a missing/different head launched merge or changed head")
					}
				}
				appendEvent("generic-green", store.EventCI, controller.CIPayload{HeadSHA: head, Status: "green", PR: 42})
				step()
				if len(drv.PhaseStarts) != 0 {
					t.Fatal("generic green CI launched integrate without named required checks")
				}
				appendEvent("lint-red", store.EventCI, controller.CIPayload{HeadSHA: head, CheckName: "Lint", Status: "red", PR: 42})
				step()
				appendEvent("build-green", store.EventCI, controller.CIPayload{HeadSHA: head, CheckName: "Build and Test", Status: "green", PR: 42})
				step()
				appendEvent("lint-rerun-green", store.EventCI, controller.CIPayload{HeadSHA: head, CheckName: "Lint", Status: "green", PR: 42})
				step()
				step()
				if len(drv.PhaseStarts) != 1 || drv.PhaseStarts[0].Phase != "integrate" {
					t.Fatalf("green CI launches=%+v, want one integrate", drv.PhaseStarts)
				}
				for _, text := range []string{"require_ci_green=true", "Lint", "Build and Test", "exact head", "one rerun", "verdict+ci", "retain soak PASS", "staging soak PASS"} {
					if !strings.Contains(drv.PhaseStarts[0].Brief, text) {
						t.Errorf("integrate brief missing %q", text)
					}
				}
				if name == "d recorded host gate" && !strings.Contains(drv.PhaseStarts[0].Brief, "Retain this owner approval gate") {
					t.Fatal("integrate lost the recorded hold-advance gate after its marker was lifted")
				}
			case 'e':
				appendEvent("integrate-done", store.EventLaneReport, lane.PhaseReport{Phase: "integrate", Round: 1, Outcome: lane.OutcomeDone, HeadSHA: head, PR: 42})
				step()
				ln := get("lane-1")
				if ln.Status == lane.StatusDone || ln.MergeSHA != "" {
					t.Fatalf("open PR #42 marked merged: status=%s merge=%s", ln.Status, ln.MergeSHA)
				}
				events, err := st.Events().List(t.Context(), store.EventFilter{RunID: r.ID})
				if err != nil {
					t.Fatal(err)
				}
				var openNamed bool
				for _, ev := range events {
					openNamed = openNamed || strings.Contains(ev.Summary, "PR #42")
				}
				if !openNamed {
					t.Error("wait/judgment does not name open PR #42")
				}
				switch name {
				case "e squash merge":
					gitTest(t, repo, "checkout", "main")
					gitTest(t, repo, "commit", "--allow-empty", "-m", "integrated lane (#42)")
					gitTest(t, repo, "push", "origin", "main")
				case "e host merge sha":
					recordFixtureMerge(t, dir, "lane-1", head)
				case "e wrong target":
					gitTest(t, repo, "push", "origin", head+":refs/heads/main")
					appendEvent("base-only", store.EventLaneReport, lane.PhaseReport{Phase: "integrate", Round: 1, Outcome: lane.OutcomeDone, HeadSHA: head, PR: 42})
					step()
					if get("lane-1").Status == lane.StatusDone {
						t.Fatal("merge on base/HEAD was treated as a merge on feature target")
					}
					gitTest(t, repo, "push", "origin", head+":refs/heads/feature")
				default:
					gitTest(t, repo, "push", "origin", head+":refs/heads/main")
				}
				step()
				if got := get("lane-1"); got.Status != lane.StatusDone || got.MergeSHA == "" {
					t.Fatalf("mirror merge not completed: %+v", got)
				}
			case 'f':
				appendEvent("held-done", store.EventLaneReport, controller.LaneReportPayload{PhaseReport: lane.PhaseReport{Phase: "review", Round: 1, Outcome: lane.OutcomeDone, HeadSHA: head, PR: 42}, Checks: "green"})
				step()
				if len(drv.PhaseStarts) != 0 || get("lane-1").MergeSHA != "" || get("lane-1").Status == lane.StatusDone {
					t.Fatal("draft or host-held PR advanced or merged")
				}
			}
		})
	}
}

func recordFixtureMerge(t *testing.T, dir, id, sha string) {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readText(filepath.Join(dir, "state.json"))), &doc); err != nil {
		t.Fatal(err)
	}
	var tasks []map[string]json.RawMessage
	if err := json.Unmarshal(doc["tasks"], &tasks); err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		var taskID string
		_ = json.Unmarshal(task["id"], &taskID)
		if taskID == id {
			task["merge_sha"], _ = json.Marshal(sha)
		}
	}
	doc["tasks"], _ = json.Marshal(tasks)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(dir, "state.json"), b); err != nil {
		t.Fatal(err)
	}
}
