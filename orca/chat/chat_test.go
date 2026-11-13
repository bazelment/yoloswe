package chat

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/store"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/bazelment/yoloswe/orca/router"
)

type fixedRouter struct{}

func (fixedRouter) Route(context.Context, string) (router.Decision, error) {
	return router.Decision{Destination: router.Planner, Confidence: .99}, nil
}
func TestPlanChangeReachesPlanner(t *testing.T) {
	received := make(chan string, 1)
	m := New(context.Background(), "run", nil, fixedRouter{})
	m.Dispatch = func(_ context.Context, d router.Decision, text string, emit func(string)) error {
		if d.Destination == router.Planner {
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
func TestClarificationReroutesOriginal(t *testing.T) {
	seen := make(chan string, 2)
	called := make(chan string, 1)
	r := routeFunc(func(_ context.Context, s string) (router.Decision, error) {
		seen <- s
		if s == "change it" {
			return router.Decision{NeedsClarification: true, ClarifyingQuestion: "Which plan?"}, nil
		}
		return router.Decision{Destination: router.Planner, Confidence: .99}, nil
	})
	m := New(context.Background(), "run", func(_ context.Context, s string) (string, error) {
		t.Error("ambiguous original reached concierge")
		return "", nil
	}, r)
	m.Dispatch = func(_ context.Context, _ router.Decision, s string, _ func(string)) error { called <- s; return nil }
	tm := teatest.NewTestModel(t, m)
	submit(tm, "change it")
	waitText(t, tm, "Which plan?")
	submit(tm, "the launch plan")
	select {
	case s := <-called:
		if s != "change it\nClarification: the launch plan" {
			t.Fatal(s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clarification lost")
	}
	finish(t, tm)
}
func TestRoutingDoesNotBlockUpdateAndStreams(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	r := routeFunc(func(context.Context, string) (router.Decision, error) {
		<-release
		return router.Decision{Destination: router.Concierge, Confidence: 1}, nil
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
		return router.Decision{Destination: router.AnswerOpenQuestion, Confidence: 1}, nil
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
	waitText(t, tm, "Answer queued")
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
	if msg.InReplyTo != "decision-1" || msg.LaneID != "lane" || msg.Body != "yes" {
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

func TestMultipartDispatchesEveryDestination(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(ctx, store.Run{ID: "run"}); err != nil {
		t.Fatal(err)
	}
	b := &StoreBackend{Store: st, RunID: "run", Stream: func(_ context.Context, s string, emit func(string)) error { emit("answer: " + s); return nil }}
	parts := []router.Decision{{Destination: router.Concierge, Text: "hello"}, {Destination: router.Planner, Text: "ship chat first"}, {Destination: router.Memory, Text: "use Bazel", IsStandingRule: true}, {Destination: router.Dispatcher, Text: "add chat tests"}, {Destination: router.Controller, Text: "pause"}}
	m := New(ctx, "run", nil, routeFunc(func(context.Context, string) (router.Decision, error) {
		return router.Decision{Parts: parts, Confidence: 1}, nil
	}))
	m.Dispatch = b.Dispatch
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 50))
	submit(tm, "hello and ship chat first and use Bazel and add chat tests and pause")
	waitText(t, tm, "run pause")
	finish(t, tm)
	dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for i := range dirs {
		found[string(dirs[i].Kind)+":"+dirs[i].Text] = true
	}
	for _, s := range []string{"plan_import:ship chat first", "instruction:add chat tests", "instruction:pause"} {
		if !found[s] {
			t.Errorf("missing %s in %+v", s, dirs)
		}
	}
	mem, err := st.Memories().List(ctx, store.MemoryFilter{RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	saved := false
	for i := range mem {
		if mem[i].Body == "use Bazel" {
			saved = true
		}
	}
	if !saved {
		t.Fatal("memory missing")
	}
	evs, err := st.Events().List(ctx, store.EventFilter{RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Summary != "add chat tests" {
		t.Fatal(evs)
	}
}
