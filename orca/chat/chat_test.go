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
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 30))
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
func waitText(t *testing.T, tm *teatest.TestModel, text string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return strings.Contains(string(b), text) }, teatest.WithDuration(5*time.Second))
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
	tm := teatest.NewTestModel(t, m)
	submit(tm, "hello")
	go tm.Type("still responsive")
	waitText(t, tm, "still responsive")
	// Releasing the router proves input was processed before classification completed.
	release <- struct{}{}
	waitText(t, tm, "first chunk second chunk")
	finish(t, tm)
}
func TestUTF8Backspace(t *testing.T) {
	tm := teatest.NewTestModel(t, New(context.Background(), "run", nil, nil))
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
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(150, 50))
	waitText(t, tm, "Ship today?")
	for _, s := range []struct{ cmd, want string }{{"/status", "goal: ship chat"}, {"/lanes", `review`}, {"/asks", "Ship today?"}, {"/plan", "plan.md"}, {"/log trace-1", "logged-route"}, {"/memory", "owner"}, {"/pause", "run pause"}, {"/resume", "run resume"}} {
		submit(tm, s.cmd)
		waitText(t, tm, s.want)
	}
	submit(tm, "yes")
	waitText(t, tm, "→ dispatcher: yes")
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
	if v := m.View().Content; !strings.HasPrefix(v, "orca chat — run run-b\n") {
		t.Fatalf("view header = %q", v)
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
