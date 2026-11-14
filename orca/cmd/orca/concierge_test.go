package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bazelment/yoloswe/orca/adopt"
	chatui "github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

// loopingModel never stops calling tools, like a model that cannot find what
// it is looking for.
func loopingModel(calls *atomic.Int32) model.ModelClient {
	return modelClientFunc(func(_ context.Context, req model.Request) (*model.Response, error) {
		n := calls.Add(1)
		return &model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(model.NewToolUseBlock(
				fmt.Sprintf("call-%d", n), ToolConciergeStatus, json.RawMessage(`{}`))),
		}, nil
	})
}

func TestConciergeBudgetStopAnswersWithWhatItFound(t *testing.T) {
	t.Parallel()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: frontDeskRunID}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	c := NewConcierge(frontDeskRunID, st, nil, loopingModel(&calls), DefaultConciergeModel)

	answer, err := c.Ask(t.Context(), "which swarms on this machine are still active, and what is each one doing?")
	if err != nil {
		t.Fatalf("Ask: %v (answer %q)", err, answer)
	}
	if got := calls.Load(); got > DefaultConciergeMaxTurn {
		t.Fatalf("model calls = %d, want at most the %d-turn budget", got, DefaultConciergeMaxTurn)
	}
	for _, want := range []string{"turn budget", "Run: " + frontDeskRunID} {
		if !strings.Contains(answer, want) {
			t.Fatalf("answer %q missing %q", answer, want)
		}
	}
}

func TestConciergeBudgetStopForcesAnAnswerWithoutTools(t *testing.T) {
	t.Parallel()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: frontDeskRunID}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var final model.Request
	client := modelClientFunc(func(_ context.Context, req model.Request) (*model.Response, error) {
		n := calls.Add(1)
		if len(req.Tools) == 0 {
			final = req
			return &model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("No orca run yet; I could not check host swarms."))}, nil
		}
		return &model.Response{
			StopReason: model.StopReasonToolUse,
			Message:    model.NewAssistantMessage(model.NewToolUseBlock(fmt.Sprintf("call-%d", n), ToolConciergeStatus, json.RawMessage(`{}`))),
		}, nil
	})
	c := NewConcierge(frontDeskRunID, st, nil, client, DefaultConciergeModel)

	answer, err := c.Ask(t.Context(), "what is running?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if answer != "No orca run yet; I could not check host swarms." || calls.Load() != DefaultConciergeMaxTurn {
		t.Fatalf("answer %q after %d calls", answer, calls.Load())
	}
	prompt := final.Messages[len(final.Messages)-1].Text()
	for _, want := range []string{"Owner question: what is running?", "[status] Run: " + frontDeskRunID, budgetPrompt} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("forced-answer prompt missing %q:\n%s", want, prompt)
		}
	}
	// The next turn starts from a well-formed, text-only history.
	if _, err := c.Ask(t.Context(), "thanks"); err != nil {
		t.Fatal(err)
	}
	for _, m := range c.history[:2] {
		if len(m.ToolCalls()) != 0 {
			t.Fatalf("history kept a dangling tool call: %+v", c.history)
		}
	}
}

// scriptedModel answers each request with the next response.
func scriptedModel(responses ...model.Response) model.ModelClient {
	var next atomic.Int32
	return modelClientFunc(func(context.Context, model.Request) (*model.Response, error) {
		i := int(next.Add(1)) - 1
		if i >= len(responses) {
			return nil, fmt.Errorf("unexpected model call %d", i+1)
		}
		return &responses[i], nil
	})
}

func toolUse(id, name, input string) model.Response {
	return model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock(id, name, json.RawMessage(input)))}
}

func textAnswer(s string) model.Response {
	return model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock(s))}
}

func TestConciergeLogsEveryToolCall(t *testing.T) {
	t.Parallel()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "run", Objective: "ship chat"}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	c := NewConcierge("run", st, nil, scriptedModel(
		toolUse("a", ToolConciergeStatus, `{"include_metrics": true}`),
		toolUse("b", ToolConciergeLaneShow, `{"lane_id":"lane-missing"}`),
		textAnswer("run ships chat"),
	), DefaultConciergeModel)
	c.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	if _, err := c.Ask(t.Context(), "where are we?"); err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		if line["msg"] == "tool call" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("tool call log lines = %d, want 2:\n%s", len(lines), logs.String())
	}
	status, show := lines[0], lines[1]
	if status["level"] != "INFO" || status["actor"] != ActorConcierge || status["tool"] != ToolConciergeStatus ||
		status["args"] != `{"include_metrics": true}` || status["result_bytes"].(float64) <= 0 || status["error"] != nil {
		t.Fatalf("status log = %v", status)
	}
	if show["tool"] != ToolConciergeLaneShow || show["args"] != `{"lane_id":"lane-missing"}` ||
		!strings.Contains(fmt.Sprint(show["error"]), "not found") {
		t.Fatalf("lane.show log = %v", show)
	}
}

func TestConciergeSwarmsToolListsHostSwarms(t *testing.T) {
	t.Parallel()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: frontDeskRunID}); err != nil {
		t.Fatal(err)
	}
	c := NewConcierge(frontDeskRunID, st, nil, scriptedModel(
		toolUse("a", ToolSwarms, `{}`),
		textAnswer("swarm-a is building chatux."),
	), DefaultConciergeModel)
	c.Swarms = func(context.Context) chatui.SwarmList {
		return chatui.SwarmList{Host: []adopt.Run{{
			ID: "swarm-a", Goal: "ship orca v2", Orchestrator: "claude", LastActivity: "2026-11-14T04:38:00Z",
			Lanes: []adopt.Lane{{ID: "chatux", Phase: "build", Status: "running", PR: "404"}},
		}}}
	}
	answer, err := c.Ask(t.Context(), "which swarms on this machine are still active, and what is each one doing?")
	if err != nil || answer != "swarm-a is building chatux." {
		t.Fatalf("answer %q err %v", answer, err)
	}
	items, err := st.Threads().ListItems(t.Context(), c.threadID, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var result string
	for i := range items {
		if items[i].Kind == store.ItemKindToolResult && items[i].Name == ToolSwarms {
			result = items[i].Content
		}
	}
	for _, want := range []string{
		"orca runs: none",
		"  swarm-a: ship orca v2 | lanes: 1 (build:1) | last activity: 2026-11-14T04:38:00Z",
		"    - chatux: phase=build status=running round=0 pr=#404",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("swarms result missing %q:\n%s", want, result)
		}
	}
}

// conciergeReplay writes a replay in which the router sends each message to
// the concierge, which answers it.
func conciergeReplay(t *testing.T, answers ...string) string {
	t.Helper()
	var exchanges []model.RecordedExchange
	for _, a := range answers {
		exchanges = append(exchanges,
			model.RecordedExchange{Response: textAnswer(`{"handler":"concierge","confidence":0.9}`)},
			model.RecordedExchange{Response: textAnswer(a)})
	}
	path := filepath.Join(t.TempDir(), "concierge-replay.json")
	if err := model.SaveRecording(path, exchanges); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBareChatRestartReattachesToFrontDeskStore(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	session := func(question, answer string) string {
		var stdout, stderr bytes.Buffer
		args := []string{"chat", "--store-dir", base, "--repo", "bazelment/yoloswe", "--replay", conciergeReplay(t, answer)}
		if err := Execute(t.Context(), args, strings.NewReader(question+"\n"), &stdout, &stderr); err != nil {
			t.Fatalf("Execute chat: %v\nstderr:\n%s", err, stderr.String())
		}
		if !strings.Contains(stdout.String(), "orca> "+answer) {
			t.Fatalf("stdout:\n%s", stdout.String())
		}
		return stderr.String()
	}
	first := session("remember the lint lane", "noted")
	second := session("what did I ask before?", "the lint lane")

	path, err := store.RunDBPath(base, frontDeskRunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, log := range []string{first, second} {
		if !strings.Contains(log, `"store":"sqlite:`+path+`"`) {
			t.Fatalf("chat did not open the front-desk store %s:\n%s", path, log)
		}
	}
	st, err := store.OpenSQLite(path, clock.NewRealClock())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	threads, err := st.Threads().ListThreads(t.Context(), store.ThreadFilter{RunID: frontDeskRunID, Role: store.ThreadRoleConcierge})
	if err != nil || len(threads) != 1 {
		t.Fatalf("front-desk threads = %+v err %v, want one continued thread", threads, err)
	}
	items, err := st.Threads().ListItems(t.Context(), threads[0].ID, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for i := range items {
		if items[i].Kind == store.ItemKindUserMessage || items[i].Kind == store.ItemKindAssistantMessage {
			said = append(said, items[i].Content)
		}
	}
	if want := "remember the lint lane|noted|what did I ask before?|the lint lane"; strings.Join(said, "|") != want {
		t.Fatalf("conversation = %q, want %q", strings.Join(said, "|"), want)
	}
}

type fakeRunOrchestrators map[string]string

func (f fakeRunOrchestrators) RunOrchestratorSession(_ context.Context, runID string) (string, error) {
	return f[runID], nil
}

// noLoops finds no live step loop or watcher.
type noLoops struct{}

func (noLoops) Running(context.Context, string) ([]int, error)             { return nil, nil }
func (noLoops) Describe(context.Context, []int) (desc string, pane string) { return "", "" }

// TestConciergeSwarmSummaryNamesActivityNextAndPRState: the swarms tool reads
// a nebula-swarm run (testdata/swarm) with fake Nebula terminal states and a
// fake GitHub, and the summary the concierge answers from names each lane's
// current activity, next step and real PR state.
func TestConciergeSwarmSummaryNamesActivityNextAndPRState(t *testing.T) {
	t.Parallel()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: frontDeskRunID}); err != nil {
		t.Fatal(err)
	}
	backend := chatui.NewStoreBackend(st, frontDeskRunID)
	backend.HostSwarms = adopt.Importer{Roots: []string{filepath.Join("testdata", "swarm")}, Orchestrators: fakeRunOrchestrators{}, Loops: noLoops{}}.Discover
	var probedTerminals []string
	var mu sync.Mutex
	backend.HostProbe = chatui.HostProbe{
		TerminalState: func(_ context.Context, id string) (string, error) {
			mu.Lock()
			probedTerminals = append(probedTerminals, id)
			mu.Unlock()
			return map[string]string{"term-m2-clean": "working"}[id], nil
		},
		PR: func(_ context.Context, repo, _, n string) (chatui.PRStatus, error) {
			if repo != "sycamore-labs/nebula" {
				return chatui.PRStatus{}, fmt.Errorf("repo %q", repo)
			}
			switch n {
			case "5604":
				return chatui.PRStatus{State: "MERGED", Checks: "4 passed"}, nil
			case "5705":
				return chatui.PRStatus{State: "OPEN", Checks: "3 passed, 1 pending"}, nil
			}
			return chatui.PRStatus{}, fmt.Errorf("no PR %s", n)
		},
	}
	c := NewConcierge(frontDeskRunID, st, nil, scriptedModel(
		toolUse("a", ToolSwarms, `{}`),
		textAnswer("20261114-trial-m1 is running m2-default (clean)."),
	), DefaultConciergeModel)
	c.Swarms = backend.LiveSwarms
	if _, err := c.Ask(t.Context(), "adopt the swarm driven by claude: what is it doing now?"); err != nil {
		t.Fatal(err)
	}
	items, err := st.Threads().ListItems(t.Context(), c.threadID, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var result string
	for i := range items {
		if items[i].Kind == store.ItemKindToolResult && items[i].Name == ToolSwarms {
			result = items[i].Content
		}
	}
	lines := strings.Split(result, "\n")
	for _, want := range []string{
		"    state: active | driven by: sess-orch | step loop: none running",
		"    - m1-base: phase=review status=done round=1 pr=#5604 base prompts",
		"        now: review round 1; terminal term-m1-review (lane done, not probed); last event: 2026-11-14T05:20:00Z merged #5604 mmm; latest report m1-base.review.r1.md: Outcome: done",
		"        next: nothing (lane done)",
		"        pr #5604 (GitHub): merged, checks: 4 passed",
		"    - m2-default: phase=clean status=running round=1 pr=#5705 default prompts",
		"        now: clean round 1; terminal term-m2-clean agent=working; ledger terminal_state=working; last event: 2026-11-14T05:35:00Z started clean term-m2-clean; latest report m2-default.swe.r1.md: Outcome: done",
		"        next: Next: m2-default review after clean; then merge train.",
		"        pr #5705 (GitHub): open, checks: 3 passed, 1 pending",
		"    progress: Merged: m1-base #5604. / Next: m2-default review after clean; then merge train.",
		"    running now: m2-default (clean, agent working)",
	} {
		if !slices.Contains(lines, want) {
			t.Fatalf("swarms summary missing %q:\n%s", want, result)
		}
	}
	if !strings.Contains(lines[2], "20261114-trial-m1: trial m1: slim swarm prompts | lanes: 2 (clean:1, review:1)") ||
		!strings.HasSuffix(lines[2], "| format: nebula-swarm | active | driven by sess-orch") {
		t.Fatalf("run line = %q", lines[2])
	}
	if strings.Contains(result, "not checked") || strings.Contains(result, "running now: nothing") {
		t.Fatalf("summary with every read done claims unchecked or idle state:\n%s", result)
	}
	// Only the running lane's terminal is probed; the done lane's session is released.
	if !reflect.DeepEqual(probedTerminals, []string{"term-m2-clean"}) {
		t.Fatalf("probed terminals %v", probedTerminals)
	}
}
