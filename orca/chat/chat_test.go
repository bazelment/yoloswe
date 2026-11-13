package chat

import (
	"context"
	"encoding/json"
	"os"
	"strings"
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
	b := &StoreBackend{Store: st, RunID: "run"}
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
	streamed := ""
	b := &StoreBackend{Store: st, RunID: "run", Stream: func(_ context.Context, s string, emit func(string)) error { streamed = s; emit("answer"); return nil }}
	for _, h := range []router.Handler{router.Concierge, router.Planner, router.Dispatcher} {
		if err := b.Dispatch(t.Context(), router.Decision{Handler: h}, string(h), func(string) {}); err != nil {
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
	events, err := st.Events().List(t.Context(), store.EventFilter{RunID: "run"})
	if err != nil || len(events) != 1 {
		t.Fatalf("%+v %v", events, err)
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
	b := &StoreBackend{Store: st, RunID: "run"}
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
	if msg.InReplyTo != "ask-1" {
		t.Fatalf("reply=%+v", msg)
	}
	dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: "run", ActiveOnly: true})
	if err != nil || len(dirs) != 1 {
		t.Fatalf("directives=%+v err=%v", dirs, err)
	}
}
func TestPauseViaConciergeTool(t *testing.T) {
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "run"}); err != nil {
		t.Fatal(err)
	}
	b := &StoreBackend{Store: st, RunID: "run"}
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
