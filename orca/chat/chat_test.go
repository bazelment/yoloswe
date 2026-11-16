package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/concierge"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/bazelment/yoloswe/orca/router"
)

type fixedRouter struct{}

func (fixedRouter) Route(context.Context, string) (router.Decision, error) {
	return router.Decision{Handler: router.Planner, Confidence: .99}, nil
}
func TestPlanChangeReachesPlanner(t *testing.T) {
	received := make(chan string, 1)
	m := New(context.Background(), "run", nil, fixedRouter{})
	m.Dispatch = func(_ context.Context, d router.Decision, text string, emit func(string)) error {
		if d.Handler == router.Planner {
			received <- text
		}
		return nil
	}
	tm := newTestModel(t, &m, teatest.WithInitialTermSize(100, 30))
	tm.Type("change the plan to ship chat first")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case got := <-received:
		if got != "change the plan to ship chat first" {
			t.Fatal(got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("planner did not receive plan change")
	}
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	tm.WaitFinished(t, teatest.WithFinalTimeout(time.Second))
}

type routeFunc func(context.Context, string) (router.Decision, error)

func (f routeFunc) Route(ctx context.Context, s string) (router.Decision, error) { return f(ctx, s) }
func waitText(t *testing.T, tm *teatest.TestModel, texts ...string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		s := string(b)
		san := SanitizeText(s)
		probe := newFakeTerminal(160, 50)
		probe.write(s)
		screen := strings.Join(probe.nonEmptyLines(), "\n")
		for _, text := range texts {
			if !strings.Contains(s, text) && !strings.Contains(san, text) && (probe.incompleteEsc || !strings.Contains(screen, text)) {
				return false
			}
		}
		return true
	}, teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(5*time.Millisecond))
}
func submit(tm *teatest.TestModel, s string) {
	tm.Type(s)
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
}
func finish(t *testing.T, tm *teatest.TestModel) {
	t.Helper()
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	tm.WaitFinished(t, teatest.WithFinalTimeout(time.Second))
}
func TestIsStartRequestOnlyForExplicitAffirmativeRequests(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]bool{
		"start a run to fix the flaky test":         true,
		"Please start a run to fix the flaky test.": true,
		"ok, start work on the docs task":           true,
		"new goal: refresh the README":              true,
		"Fix the docs. Start a run for it":          true,
		"don't start a run yet":                     false,
		"Don’t start a run yet":                     false,
		"do not start work on this":                 false,
		"how do I start work?":                      false,
		"how do I start a run":                      false,
		"can you start a run to fix the test":       false,
		"start a run if CI is green":                false,
		"wait, start a run later":                   false,
		"explain how to start a run":                false,
		"fix the flaky test":                        false,
		"I never said start a run":                  false,
		"":                                          false,
	} {
		if got := IsStartRequest(text); got != want {
			t.Errorf("IsStartRequest(%q) = %v, want %v", text, got, want)
		}
	}
	for text, want := range map[string]bool{"yes": true, "Yes, please!": true, "go ahead.": true, "yes but not now": false, "no": false} {
		if got := IsConfirmation(text); got != want {
			t.Errorf("IsConfirmation(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestUnclearMessageReachesConcierge(t *testing.T) {
	m := New(t.Context(), "run", func(_ context.Context, s string) (string, error) { return "Which plan?", nil }, routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Concierge}, nil
	}))
	var out strings.Builder
	if err := Run(t.Context(), strings.NewReader("change it\n"), &out, m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "→ concierge: change it") || !strings.Contains(out.String(), "Which plan?") {
		t.Fatal(out.String())
	}
}
func TestRoutingDoesNotBlockUpdateAndStreams(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	r := routeFunc(func(context.Context, string) (router.Decision, error) {
		<-release
		return router.Decision{Handler: router.Concierge, Confidence: 1}, nil
	})
	m := New(context.Background(), "run", nil, r)
	m.Dispatch = func(_ context.Context, _ router.Decision, _ string, emit func(string)) error {
		emit("first chunk")
		emit(" second chunk")
		return nil
	}
	tm := newTestModel(t, &m)
	submit(tm, "hello")
	go tm.Type("still responsive")
	waitText(t, tm, "still responsive")
	// Releasing the router proves input was processed before classification completed.
	release <- struct{}{}
	waitText(t, tm, "first chunk second chunk")
	finish(t, tm)
}
func TestUTF8Backspace(t *testing.T) {
	m := New(context.Background(), "run", nil, nil)
	tm := newTestModel(t, &m)
	tm.Type("é猫🙂")
	tm.Send(tea.KeyPressMsg{Code: tea.KeyBackspace})
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	got := tm.FinalModel(t, teatest.WithFinalTimeout(time.Second)).(Model)
	if got.input != "é猫" {
		t.Fatalf("input %q", got.input)
	}
}
func TestPipeIsNotTerminal(t *testing.T) {
	r, w, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	defer w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()
	if isTerminal(r) {
		t.Fatal("pipe detected as terminal")
	}
}
func TestStoreStatusSlashAndInlineAnswer(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(ctx, store.Run{ID: "run", Objective: "ship chat", PlanRef: "plan.md"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(ctx, store.Lane{RunID: "run", ID: "lane", Phase: "review", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Directives().Add(ctx, store.Directive{ID: "ask", RunID: "run", Ref: "owner_ask:decision-1", Scope: "lane", Text: `{"id":"decision-1","question":"Ship today?"}`, Kind: store.DirectiveInstruction, Source: store.DirectiveSourceDispatcher, Status: store.DirectiveActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Decisions().Append(ctx, store.Decision{RunID: "run", Actor: "router", Category: store.DecisionJudgment, Tool: "routing", TraceID: "trace-1", Output: "logged-route"}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "run")
	r := routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Dispatcher, Confidence: 1}, nil
	})
	m := New(ctx, "run", nil, r)
	m.Backend = b
	m.Dispatch = b.Dispatch
	tm := newTestModel(t, &m, teatest.WithInitialTermSize(150, 50))
	waitText(t, tm, "Ship today?")
	for _, s := range []struct{ cmd, want string }{{"/status", "goal: ship chat"}, {"/lanes", `review`}, {"/asks", "Ship today?"}, {"/plan", "plan.md"}, {"/log trace-1", "logged-route"}, {"/memory", "owner"}, {"/pause", "run pause"}, {"/resume", "run resume"}} {
		submit(tm, s.cmd)
		waitText(t, tm, s.want)
	}
	submit(tm, "yes")
	waitText(t, tm, "→ dispatcher: yes", "Saved for the dispatcher of run run")
	finish(t, tm)
	evs, err := st.Events().List(ctx, store.EventFilter{RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("events: %+v", evs)
	}
	var msg mailbox.Message
	if err := json.Unmarshal(evs[0].Payload, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Body != "yes" || !msg.Owner {
		t.Fatal(msg)
	}
	dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: "run", ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	control := ""
	for i := range dirs {
		if dirs[i].Ref == "chat:control" {
			control = dirs[i].Text
		}
	}
	if control != "resume" {
		t.Fatal(control)
	}
}

func TestOwnerDirectiveSpawnFailureStaysVisible(t *testing.T) {
	ctx := t.Context()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(ctx, store.Run{ID: "run", Objective: "measure memory"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{RunID: "run", ID: "mem-overhead", Title: "memory overhead", Status: store.BacklogOpen, Priority: store.PriorityP1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Directives().Add(ctx, store.Directive{ID: "dir-1", RunID: "run", Source: store.DirectiveSourceConcierge, Kind: store.DirectiveInstruction, Text: "start mem-overhead", Status: store.DirectiveActive}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "run")
	b.TrackPendingOutcome("run")
	th, err := st.Threads().StartThread(ctx, store.Thread{ID: "dispatcher", RunID: "run", Role: store.ThreadRoleDispatcher, Status: store.ThreadStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := st.Threads().StartTurn(ctx, store.Turn{ID: "turn", ThreadID: th.ID, RunID: "run", Status: store.TurnStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Threads().CompleteTurn(ctx, th.ID, turn.ID, store.TurnStatusCompleted, model.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Decisions().Append(ctx, store.Decision{RunID: "run", Actor: "dispatcher", TurnID: turn.ID, Tool: "lane.spawn", Vetted: true, Result: "error: repo unavailable", Args: json.RawMessage(`{"item_id":"mem-overhead"}`)}); err != nil {
		t.Fatal(err)
	}
	snap, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Outcomes) != 1 || !strings.Contains(snap.Outcomes[0], "mem-overhead spawn failed: repo unavailable") {
		t.Fatalf("outcomes = %v", snap.Outcomes)
	}
	if !strings.Contains(snap.Status, "📋 1 queued: mem-overhead (spawn failed)") {
		t.Fatalf("status = %q", snap.Status)
	}
}

func TestThreeHandlersDispatch(t *testing.T) {
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "run"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Runs().AcquireLease(t.Context(), "run", "controller", time.Hour); err != nil {
		t.Fatal(err)
	}
	streamed := ""
	var acknowledgements []string
	b := NewStoreBackend(st, "run")
	b.Stream = func(_ context.Context, s string, emit func(string)) error { streamed = s; emit("answer"); return nil }
	for _, h := range []router.Handler{router.Concierge, router.Planner, router.Dispatcher} {
		if err := b.Dispatch(t.Context(), router.Decision{Handler: h}, string(h), func(s string) { acknowledgements = append(acknowledgements, s) }); err != nil {
			t.Fatal(err)
		}
	}
	if streamed != "concierge" {
		t.Fatal(streamed)
	}
	dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	found := map[store.DirectiveKind]string{}
	for _, d := range dirs {
		found[d.Kind] = d.Text
	}
	if found[store.DirectivePlanImport] != "planner" || found[store.DirectiveInstruction] != "dispatcher" {
		t.Fatal(dirs)
	}
	if !slices.Contains(acknowledgements, "Queued for the planner: planner") || !slices.Contains(acknowledgements, "Queued for the dispatcher: dispatcher") {
		t.Fatalf("acknowledgements = %v", acknowledgements)
	}
	events, err := st.Events().List(t.Context(), store.EventFilter{RunID: "run"})
	if err != nil || len(events) != 1 {
		t.Fatalf("%+v %v", events, err)
	}
}

// closeCounter counts Close calls on a store a backend opened.
type closeCounter struct {
	store.Store
	closed *atomic.Int32
}

func (c closeCounter) Close() error { c.closed.Add(1); return nil }

func TestFrontDeskInstructionGoesToTheOneLiveControllerOrIsRefused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	desk := store.NewMemoryStore(nil)
	defer desk.Close()
	if err := desk.Runs().Create(ctx, store.Run{ID: "front-desk"}); err != nil {
		t.Fatal(err)
	}
	runs := map[string]store.Store{}
	var listed []store.Run
	for _, id := range []string{"run-live", "run-idle", "run-other-live"} {
		st := store.NewMemoryStore(nil)
		defer st.Close()
		if err := st.Runs().Create(ctx, store.Run{ID: id}); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(id, "live") {
			if _, err := st.Runs().AcquireLease(ctx, id, "controller", time.Hour); err != nil {
				t.Fatal(err)
			}
		}
		run, err := st.Runs().Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		runs[id] = st
		listed = append(listed, *run)
	}
	var closed atomic.Int32
	b := NewStoreBackend(desk, "front-desk")
	b.FrontDesk = "front-desk"
	b.OpenRun = func(_ context.Context, id string) (store.Store, error) { return closeCounter{runs[id], &closed}, nil }
	var offer []store.Run
	b.ListRuns = func(context.Context) ([]store.Run, error) { return offer, nil }
	send := func() string {
		var ack string
		if err := b.Dispatch(ctx, router.Decision{Handler: router.Planner}, "drop the docs task", func(s string) { ack = s }); err != nil {
			t.Fatal(err)
		}
		return ack
	}
	directives := func(st store.Store, id string) int {
		dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: id})
		if err != nil {
			t.Fatal(err)
		}
		return len(dirs)
	}

	offer = listed[1:2] // only an idle run
	if got := send(); !strings.HasPrefix(got, "Not queued: no controller is running for this chat") {
		t.Fatalf("idle-only ack = %q", got)
	}
	offer = listed
	if got := send(); got != "Not queued: controllers are running runs run-live, run-other-live; attach to one with /runs <id>, then send it again: drop the docs task" {
		t.Fatalf("two-controller ack = %q", got)
	}
	offer = listed[:2]
	if got := send(); got != "Queued for the planner of run run-live, whose controller is running: drop the docs task" {
		t.Fatalf("one-controller ack = %q", got)
	}
	if directives(desk, "front-desk") != 0 || directives(runs["run-live"], "run-live") != 1 || directives(runs["run-idle"], "run-idle") != 0 || closed.Load() != 1 {
		t.Fatalf("delivery: desk=%d live=%d idle=%d closed=%d", directives(desk, "front-desk"), directives(runs["run-live"], "run-live"), directives(runs["run-idle"], "run-idle"), closed.Load())
	}
}

func TestOpenQuestionReplyReachesDispatcherSnapshot(t *testing.T) {
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "run"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Directives().Add(t.Context(), store.Directive{ID: "ask", RunID: "run", Ref: "owner_ask:ask-1", Text: `{"id":"ask-1","question":"A or B?"}`, Kind: store.DirectiveInstruction, Status: store.DirectiveActive}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "run")
	r := routeFunc(func(_ context.Context, input string) (router.Decision, error) {
		if input != "go with B\nOpen questions:\nask-1: A or B?" {
			t.Errorf("input=%q", input)
		}
		return router.Decision{Handler: router.Dispatcher}, nil
	})
	m := New(t.Context(), "run", nil, r)
	m.Backend = b
	m.Dispatch = b.Dispatch
	var out strings.Builder
	if err := Run(t.Context(), strings.NewReader("go with B\n"), &out, m); err != nil {
		t.Fatal(err)
	}
	snap, err := dispatcher.BuildSnapshot(t.Context(), st, nil, "run", nil, dispatcher.SnapshotConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snap.Text, "owner_ask:ask-1") || !strings.Contains(snap.Text, "A or B?") {
		t.Fatal(snap.Text)
	}
	events, err := st.Events().List(t.Context(), store.EventFilter{RunID: "run"})
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	var msg mailbox.Message
	if err := json.Unmarshal(events[0].Payload, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.InReplyTo != "" {
		t.Fatalf("reply=%+v, want no inferred identity", msg)
	}
	dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: "run", ActiveOnly: true})
	if err != nil || len(dirs) != 2 {
		t.Fatalf("directives=%+v err=%v", dirs, err)
	}
}
func TestPauseViaConciergeTool(t *testing.T) {
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "run"}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "run")
	client := model.NewFakeClient(
		model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("pause", concierge.ToolRunControl, json.RawMessage(`{"action":"pause"}`)))},
		model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("run paused"))},
	)
	c := concierge.New(concierge.Config{DisableCitations: true, RunControl: func(ctx context.Context, _ string, cmd string) (string, error) { return b.Command(ctx, cmd) }}, st, nil, nil, client)
	b.Stream = func(ctx context.Context, text string, emit func(string)) error {
		res, err := c.Ask(ctx, "run", text)
		if err == nil {
			emit(res.Answer)
		}
		return err
	}
	r := routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Handler: router.Concierge, Fallback: true}, nil
	})
	m := New(t.Context(), "run", nil, r)
	m.Dispatch = b.Dispatch
	var out strings.Builder
	if err := Run(t.Context(), strings.NewReader("pause the run\n"), &out, m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "→ concierge: pause the run (via fallback)") {
		t.Fatal(out.String())
	}
	dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: "run", ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0].Ref != "chat:control" || dirs[0].Text != "pause" {
		t.Fatal(dirs)
	}
}

// closeCounting records Close on a store.
type closeCounting struct {
	store.Store
	closed atomic.Int32
}

func (c *closeCounting) Close() error { c.closed.Add(1); return c.Store.Close() }

func newRunStore(t *testing.T, run store.Run, leased bool) *closeCounting {
	t.Helper()
	st := &closeCounting{Store: store.NewMemoryStore(nil)}
	if err := st.Runs().Create(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if leased {
		if _, err := st.Runs().AcquireLease(t.Context(), run.ID, "controller", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// TestRunsSwitchUnderPollAndView switches runs while the poll loop and View
// read the backend; under the race detector it fails if the switch is not
// owned by the backend's lock.
func TestRunsSwitchUnderPollAndView(t *testing.T) {
	t.Parallel()
	first := newRunStore(t, store.Run{ID: "run-a", Objective: "first"}, true)
	second := newRunStore(t, store.Run{ID: "run-b", Objective: "second"}, true)
	b := NewStoreBackend(first, "run-a")
	b.ListRuns = func(context.Context) ([]store.Run, error) { return []store.Run{{ID: "run-a"}, {ID: "run-b"}}, nil }
	b.OpenRun = func(_ context.Context, id string) (store.Store, error) {
		if id != "run-b" {
			t.Errorf("OpenRun(%q)", id)
		}
		return second, nil
	}
	var attached []string
	b.OnAttach = func(_ store.Store, id string) { attached = append(attached, id) }
	m := New(t.Context(), "run-a", nil, nil)
	m.Backend = b

	stop, polled, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		once := sync.OnceFunc(func() { close(polled) })
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			_ = m.View()
			_ = m.headerLine()
			if msg, ok := m.refresh()().(snapshotMsg); !ok || msg.err != nil {
				done <- fmt.Errorf("poll snapshot: %+v", msg)
				return
			}
			once()
		}
	}()
	<-polled
	got, err := b.Command(t.Context(), "/runs run-b")
	close(stop)
	if pollErr := <-done; pollErr != nil {
		t.Fatal(pollErr)
	}
	if err != nil || got != "attached to run run-b" {
		t.Fatalf("/runs run-b = %q, %v", got, err)
	}
	if first.closed.Load() != 1 || second.closed.Load() != 0 {
		t.Fatalf("closed first=%d second=%d, want 1 0", first.closed.Load(), second.closed.Load())
	}
	if strings.Join(attached, ",") != "run-b" || b.RunID() != "run-b" {
		t.Fatalf("attached=%v RunID=%q", attached, b.RunID())
	}
	if h := m.headerLine(); h != "orca chat — run run-b" {
		t.Fatalf("headerLine = %q, want orca chat — run run-b", h)
	}
	if v := m.View().Content; strings.Contains(v, "orca chat — run") {
		t.Fatalf("view re-prints header: %q", v)
	}
	if s, err := b.Snapshot(t.Context()); err != nil || !strings.HasPrefix(s.Status, "goal: second") {
		t.Fatalf("snapshot after switch = %+v, %v", s, err)
	}
	if err := b.Close(); err != nil || second.closed.Load() != 1 {
		t.Fatalf("Close = %v, second closed %d", err, second.closed.Load())
	}
}

func TestRunsRejectsRunsDiscoveryDoesNotOffer(t *testing.T) {
	t.Parallel()
	first := newRunStore(t, store.Run{ID: "run-a", Objective: "first"}, true)
	b := NewStoreBackend(first, "run-a")
	b.ListRuns = func(context.Context) ([]store.Run, error) { return []store.Run{{ID: "run-a"}}, nil }
	b.OpenRun = func(_ context.Context, id string) (store.Store, error) {
		t.Errorf("OpenRun(%q) for a run discovery did not offer", id)
		return nil, fmt.Errorf("unexpected")
	}
	if _, err := b.Command(t.Context(), "/runs run-elsewhere"); err == nil || err.Error() != `no run "run-elsewhere" for this repository; /runs lists them` {
		t.Fatalf("err = %v", err)
	}
	if b.RunID() != "run-a" || first.closed.Load() != 0 {
		t.Fatalf("RunID=%q closed=%d", b.RunID(), first.closed.Load())
	}
}

func TestQueuedAcknowledgementNeedsLiveControllerLease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		leased     bool
	}{
		{"leased", "Queued for the dispatcher: retry lint", true},
		{"no-controller", "Saved for the dispatcher of run run, but no controller is running it; it will act when one starts: retry lint", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := NewStoreBackend(newRunStore(t, store.Run{ID: "run"}, tc.leased), "run")
			defer b.Close()
			var got []string
			if err := b.Dispatch(t.Context(), router.Decision{Handler: router.Dispatcher}, "retry lint", func(s string) { got = append(got, s) }); err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\n") != tc.want {
				t.Fatalf("ack = %q, want %q", got, tc.want)
			}
		})
	}
}

type modelClientFunc func(context.Context, model.Request) (*model.Response, error)

func (f modelClientFunc) CreateMessage(ctx context.Context, req model.Request) (*model.Response, error) {
	return f(ctx, req)
}

func TestOfferConfirmationReachesDispatcherWithOfferSpec(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const offerSpec = "I'd fund this as a small lane (argon-medium, 15m) to measure sandbox memory overhead."

	var dispatcherInputs []string
	var f *adoptedFixture
	f = newAdoptedFixture(t, func(runID string, answer func(context.Context, string, string, string) error) controller.Judge {
		return controller.JudgeFunc(func(ctx context.Context, rID string, events []store.Event) error {
			var calls int
			client := modelClientFunc(func(_ context.Context, req model.Request) (*model.Response, error) {
				calls++
				if calls == 1 {
					var snapText string
					for _, msg := range req.Messages {
						snapText += msg.Text() + "\n"
					}
					dispatcherInputs = append(dispatcherInputs, snapText)
					brief := "spawn argon-medium"
					if strings.Contains(snapText, offerSpec) {
						brief = offerSpec
					}
					args, _ := json.Marshal(dispatcher.LaneSpawnParams{
						ItemID: "argon-medium",
						Brief:  brief,
						Reason: "owner confirmed concierge offer",
					})
					return &model.Response{
						StopReason: model.StopReasonToolUse,
						Message:    model.NewAssistantMessage(model.NewToolUseBlock("spawn-1", dispatcher.ToolLaneSpawn, args)),
					}, nil
				}
				return &model.Response{
					StopReason: model.StopReasonEndTurn,
					Message:    model.NewAssistantMessage(model.NewTextBlock("Spawned argon-medium.")),
				}, nil
			})
			d := dispatcher.New(dispatcher.Config{OnOwnerAnswer: answer}, f.store, f.driver, f.clock, client)
			return d.Judge(ctx, rID, events)
		})
	})
	if err := f.store.Backlog().Add(ctx, store.BacklogItem{
		ID:       "argon-medium",
		RunID:    adoptedRun,
		Title:    "Measure diskless sandbox memory overhead",
		Priority: store.PriorityP1,
		Status:   store.BacklogOpen,
	}); err != nil {
		t.Fatal(err)
	}

	f.backend.Stream = func(_ context.Context, _ string, emit func(string)) error {
		emit(offerSpec)
		return nil
	}

	var routed []router.Handler
	r := routeFunc(func(_ context.Context, input string) (router.Decision, error) {
		var h router.Handler
		switch {
		case strings.HasPrefix(input, "yes fund it") && strings.Contains(input, "Open questions:"):
			h = router.Dispatcher
		default:
			h = router.Concierge
		}
		routed = append(routed, h)
		return router.Decision{Handler: h, Confidence: 0.9}, nil
	})
	m := New(ctx, chatRun, nil, r)
	m.Backend = f.backend
	m.Dispatch = f.backend.Dispatch
	var out strings.Builder
	if err := Run(ctx, strings.NewReader("how much memory does the diskless sandbox use?\nyes fund it\n"), &out, m); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(routed, []router.Handler{router.Concierge, router.Dispatcher}) {
		t.Fatalf("routed handlers = %v, want [concierge dispatcher]\nchat output:\n%s", routed, out.String())
	}
	if len(dispatcherInputs) == 0 || !strings.Contains(dispatcherInputs[0], offerSpec) {
		t.Fatalf("dispatcher input = %q, want offer spec %q", dispatcherInputs, offerSpec)
	}
	if len(f.driver.Spawns) != 1 || !strings.Contains(f.driver.Spawns[0].Brief, offerSpec) {
		t.Fatalf("spawns = %+v, want 1 spawn whose Brief contains %q", f.driver.Spawns, offerSpec)
	}
	dirs, err := f.store.Directives().List(ctx, store.DirectiveFilter{RunID: adoptedRun})
	if err != nil {
		t.Fatal(err)
	}
	var sawDeliveredSpec, sawHandledProposal bool
	for _, d := range dirs {
		if strings.TrimSpace(d.Text) == "yes fund it" {
			t.Fatalf("found bare confirmation directive row: %+v", d)
		}
		if d.Kind == store.DirectiveInstruction && !store.IsInternalDirective(d) && strings.Contains(d.Text, offerSpec) && strings.Contains(d.Text, "owner: yes fund it") {
			sawDeliveredSpec = true
		}
		if strings.HasPrefix(d.Ref, "owner_ask:"+ProposalAskIDPrefix) && d.Status == store.DirectiveHandled {
			sawHandledProposal = true
		}
	}
	if !sawDeliveredSpec {
		t.Fatalf("expected instruction directive with offer spec and quoted owner reply, got: %+v", dirs)
	}
	if !sawHandledProposal {
		t.Fatalf("expected proposal directive to be marked handled, got: %+v", dirs)
	}
}

func TestConversationWindowAttachedOnNonConciergeHandoff(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var batches [][]store.Event
	f := newAdoptedFixture(t, func(_ string, _ func(context.Context, string, string, string) error) controller.Judge {
		return controller.JudgeFunc(func(_ context.Context, _ string, events []store.Event) error {
			batches = append(batches, slices.Clone(events))
			return nil
		})
	})
	replies := []string{
		"Turn 1 reply about alpha.",
		"Turn 2 reply about beta.",
		"Turn 3 reply about gamma.",
		"Turn 4 reply: lane memory-spike is ready on backlog item argon-medium.",
	}
	var turnIdx int
	f.backend.Stream = func(_ context.Context, _ string, emit func(string)) error {
		emit(replies[turnIdx])
		turnIdx++
		return nil
	}
	for i := 0; i < 4; i++ {
		if err := f.backend.Dispatch(ctx, router.Decision{Handler: router.Concierge}, fmt.Sprintf("question %d", i+1), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.backend.Dispatch(ctx, router.Decision{Handler: router.Dispatcher}, "spawn a lane for it now", nil); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("batches = %+v, want 1 event", batches)
	}
	summary := batches[0][0].Summary
	if !strings.Contains(summary, "Recent chat context:") || !strings.Contains(summary, "argon-medium") {
		t.Fatalf("event summary missing recent chat context: %q", summary)
	}
	if strings.Contains(summary, "Turn 1 reply about alpha.") {
		t.Fatalf("expected conversation window to keep only last ~3 exchanges, got: %q", summary)
	}
}

func TestBareConfirmationAndChitChatWriteNoDirectiveAndAskWhenUnresolved(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newAdoptedFixture(t, func(_ string, _ func(context.Context, string, string, string) error) controller.Judge {
		return controller.JudgeFunc(func(_ context.Context, _ string, _ []store.Event) error {
			return nil
		})
	})
	for _, msg := range []string{"yes", "no", "ok", "thanks", "yes fund it"} {
		var ack string
		if err := f.backend.Dispatch(ctx, router.Decision{Handler: router.Dispatcher}, msg, func(s string) { ack += s }); err != nil {
			t.Fatal(err)
		}
		if msg == "yes fund it" && !strings.Contains(ack, `What does "yes fund it" refer to?`) {
			t.Fatalf("unresolved bare confirmation ack = %q, want clarification prompt", ack)
		}
	}
	dirs, err := f.store.Directives().List(ctx, store.DirectiveFilter{RunID: adoptedRun})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if !store.IsInternalDirective(d) {
			t.Fatalf("unexpected durable directive from bare confirmation/chit-chat: %+v", d)
		}
	}
}

func TestPlannerRoutedMessageStreamsPlannerAnswerInChat(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := newRunStore(t, store.Run{ID: "run-sandbox", Objective: "diskless sandbox with minimal guest RAM"}, true)
	defer st.Close()
	b := NewStoreBackend(st, "run-sandbox")
	b.PlannerStream = func(callCtx context.Context, targetStore store.Store, runID, text string, emit func(string)) error {
		th, err := targetStore.Threads().StartThread(callCtx, store.Thread{
			ID:     "planner-" + runID + "-1",
			RunID:  runID,
			Role:   store.ThreadRolePlanner,
			Goal:   text,
			Status: store.ThreadStatusIdle,
		})
		if err != nil {
			return err
		}
		turnRec, err := targetStore.Threads().StartTurn(callCtx, store.Turn{
			ID:       th.ID + "-turn-1",
			ThreadID: th.ID,
			RunID:    runID,
			Input:    text,
		})
		if err != nil {
			return err
		}
		askRaw := `{"id":"proposal-dax","run_id":"run-sandbox","question":"Cap guest RAM at 1 GiB and use virtiofs DAX to bypass guest page cache","options":["approve_proposal","reject_proposal"],"default":"approve_proposal"}`
		if err := targetStore.Directives().Add(callCtx, store.Directive{
			ID:     "ask-proposal-dax",
			RunID:  runID,
			Kind:   store.DirectiveInstruction,
			Source: store.DirectiveSourcePlanner,
			Ref:    "owner_ask:proposal-dax",
			Text:   askRaw,
			Status: store.DirectiveActive,
		}); err != nil {
			return err
		}
		usage := model.Usage{InputTokens: 2000, OutputTokens: 400, CostUSD: 0.05}
		if err := targetStore.Threads().CompleteTurn(callCtx, th.ID, turnRec.ID, store.TurnStatusCompleted, usage, ""); err != nil {
			return err
		}
		if emit != nil {
			emit("With virtiofs DAX or EROFS over virtio-pmem, the guest kernel maps host pages directly without duplicating fs cache in guest RAM, keeping overhead under 512 MiB.")
		}
		return nil
	}

	const ask = "I'm not completely against guest ram consumption, but up to 4GB seems to be high, can you research what's the best we can do if memory overhead is minimized, think about how kernel manages fs cache"
	var streamed []string
	if err := b.Dispatch(ctx, router.Decision{Handler: router.Planner, Confidence: 0.95}, ask, func(s string) {
		streamed = append(streamed, s)
	}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(streamed, "\n")
	if strings.HasPrefix(got, "Queued for the planner") {
		t.Fatalf("planner-routed message produced bare ack %q, want streamed planner answer", got)
	}
	if !strings.Contains(got, "virtiofs DAX") {
		t.Fatalf("streamed planner answer = %q, want grounded answer", got)
	}
	snap, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snap.Status, "open questions: 1") || !strings.Contains(snap.Status, "cost today: $0.05") {
		t.Fatalf("Snapshot.Status = %q, want open questions: 1 and cost today: $0.05", snap.Status)
	}
	if len(snap.Questions) != 1 || !strings.Contains(snap.Questions[0], "proposal-dax") {
		t.Fatalf("Snapshot.Questions = %v, want proposal-dax", snap.Questions)
	}
}
