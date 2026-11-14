package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/router"
	"github.com/bazelment/yoloswe/orca/store"
)

const (
	adoptedRun = "20261113-054825-host"
	chatRun    = "orca-takeover"
)

type hostLabeler struct{}

func (hostLabeler) RunOrchestrator(context.Context, string) (string, error)  { return "claude", nil }
func (hostLabeler) SetRunOrchestrator(context.Context, string, string) error { return nil }

type idleWatcher struct{}

func (idleWatcher) Running(context.Context, string) ([]int, error) { return nil, nil }
func (idleWatcher) Stop(context.Context, []int) error              { return nil }
func (idleWatcher) Start(context.Context, string) error            { return nil }

//nolint:govet // test fixture grouping
type adoptedFixture struct {
	backend *StoreBackend
	store   store.Store
	driver  *lane.FakeDriver
	clock   *clock.FakeClock
	ctrls   *adopt.Controllers
	root    string
}

// newHostRun writes a host swarm run directory with an escalation-ready
// review lane and a finished lane, and returns the swarm root.
func newHostRun(t *testing.T, ids ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, id := range ids {
		dir := filepath.Join(root, ".shared", "subagent-swarm", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"state.json": `{"config":{"goal":"host goal","phases":[{"name":"swe"},{"name":"review"}]},"tasks":[` +
				`{"id":"orca-bare","title":"Bare orca","phase":"review","status":"running","round":3,"pr":401},` +
				`{"id":"rename","title":"Rename","phase":"swe","status":"done","round":1,"pr":398}]}`,
			"orca-bare.review.spawn.json": `{"session_id":"s-bare","terminal_id":"t-bare"}`,
			"OBJECTIVE.md":                "objective\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// newAdoptedFixture is a chat whose own run has no controller, holding the
// adopted host run driven by a real adopted controller. judges may be nil.
func newAdoptedFixture(t *testing.T, judges adopt.JudgeFactory, adoptIDs ...string) *adoptedFixture {
	t.Helper()
	if len(adoptIDs) == 0 {
		adoptIDs = []string{adoptedRun}
	}
	root := newHostRun(t, adoptIDs...)
	clk := clock.NewFakeClock(clock.NewRealClock().Now().UTC())
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Runs().Create(t.Context(), store.Run{ID: chatRun, Objective: "Owner chat for run " + chatRun}); err != nil {
		t.Fatal(err)
	}
	drv := lane.NewFakeDriver()
	ctrls := adopt.NewControllers(t.Context(), st, drv, clk, judges)
	t.Cleanup(func() { _ = ctrls.Close(context.Background()) })
	svc := &adopt.Service{
		Importer: adopt.Importer{Roots: []string{root}},
		Labeler:  hostLabeler{},
		Watcher:  idleWatcher{},
		Holder:   adopt.ChatHolder(chatRun),
		Driver:   ctrls,
	}
	b := NewStoreBackend(st, chatRun)
	b.Adoption, b.Now = svc, clk.Now
	for _, id := range adoptIDs {
		out, err := b.Command(t.Context(), "/adopt "+id)
		if err != nil || !strings.HasPrefix(out, "adopted "+id) {
			t.Fatalf("/adopt %s = %q, %v", id, out, err)
		}
	}
	return &adoptedFixture{backend: b, store: st, driver: drv, clock: clk, ctrls: ctrls, root: root}
}

func TestLanesListsAdoptedRunLanes(t *testing.T) {
	t.Parallel()
	f := newAdoptedFixture(t, nil)
	out, err := f.backend.Command(t.Context(), "/lanes")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("/lanes = %q, want a header and two lane rows", out)
	}
	for i, want := range [][]string{
		{"RUN", "LANE", "PHASE", "ROUND", "STATUS", "PR"},
		{adoptedRun, "orca-bare", "review", "3", "running", "#401"},
		{adoptedRun, "rename", "swe", "1", "done", "#398"},
	} {
		if got := strings.Fields(lines[i]); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("/lanes row %d = %q, want %q\n%s", i, got, want, out)
		}
	}
	s, err := f.backend.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Status, "lanes: 2 (") || !strings.Contains(s.Status, "adopted: "+adoptedRun) {
		t.Fatalf("status = %q, want the adopted run's two lanes counted", s.Status)
	}
}

func TestLanesWithoutAdoptedRunSaysSo(t *testing.T) {
	t.Parallel()
	st := store.NewMemoryStore(nil)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Runs().Create(t.Context(), store.Run{ID: chatRun}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, chatRun)
	out, err := b.Command(t.Context(), "/lanes")
	if err != nil {
		t.Fatal(err)
	}
	if out != "no lanes in run "+chatRun+"; /adopt <run-id> takes over a host swarm" {
		t.Fatalf("/lanes = %q", out)
	}
	if out, err := b.Command(t.Context(), "/asks"); err != nil || out != "no open questions" {
		t.Fatalf("/asks = %q, %v", out, err)
	}
}

// recordingJudge stands in for an adopted run's dispatcher.
type recordingJudge struct{ batches chan []store.Event }

func (j recordingJudge) Judge(_ context.Context, _ string, events []store.Event) error {
	j.batches <- events
	return nil
}

func TestInstructionNamingAdoptedRunReachesItsDispatcher(t *testing.T) {
	t.Parallel()
	judged := map[string]recordingJudge{}
	f := newAdoptedFixture(t, func(runID string, _ func(context.Context, string, string, string) error) controller.Judge {
		judged[runID] = recordingJudge{batches: make(chan []store.Event, 8)}
		return judged[runID]
	})
	text := "Owner decision for run " + adoptedRun + ": start swe round 4 on orca-bare"
	var ack string
	if err := f.backend.Dispatch(t.Context(), router.Decision{Handler: router.Dispatcher}, text, func(s string) { ack += s }); err != nil {
		t.Fatal(err)
	}
	if ack != "Queued for the dispatcher of adopted run "+adoptedRun+": "+text {
		t.Fatalf("ack = %q", ack)
	}
	if got := instructions(t, f.store, adoptedRun); len(got) != 1 || got[0] != text {
		t.Fatalf("adopted run instructions = %q, want the owner's", got)
	}
	if got := instructions(t, f.store, chatRun); len(got) != 0 {
		t.Fatalf("chat run instructions = %q, want none", got)
	}
	select {
	case batch := <-judged[adoptedRun].batches:
		if len(batch) != 1 || batch[0].Kind != store.EventOwnerMessage || batch[0].Summary != text {
			t.Fatalf("dispatcher batch = %+v, want the owner message", batch)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("adopted run's dispatcher never saw the owner instruction")
	}
}

func TestInstructionTargetAmongHeldRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text, want string
		ownController    bool
	}{
		{"own run has no controller", "start the next round", adoptedRun, false},
		{"own run has a controller", "start the next round", chatRun, true},
		{"names an adopted lane", "start swe round 4 on orca-bare", adoptedRun, true},
		{"names the chat run", "pause " + chatRun, chatRun, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAdoptedFixture(t, nil)
			if tc.ownController {
				if _, err := f.store.Runs().AcquireLease(t.Context(), chatRun, "orca-run", time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.backend.Dispatch(t.Context(), router.Decision{Handler: router.Dispatcher}, tc.text, nil); err != nil {
				t.Fatal(err)
			}
			if got := instructions(t, f.store, tc.want); len(got) != 1 || got[0] != tc.text {
				t.Fatalf("instructions on %s = %q, want %q", tc.want, got, tc.text)
			}
		})
	}
}

func TestFrontDeskInstructionGoesToHeldAdoptedRun(t *testing.T) {
	t.Parallel()
	f := newAdoptedFixture(t, nil)
	f.backend.FrontDesk = chatRun
	// A run this chat does not hold is also live; the held one wins.
	other := store.Run{ID: "run-elsewhere", Lease: &store.Lease{HolderID: "orca-run", ExpiresAt: f.clock.Now().Add(time.Hour)}}
	f.backend.ListRuns = func(context.Context) ([]store.Run, error) { return []store.Run{other}, nil }
	for _, h := range []router.Handler{router.Planner, router.Dispatcher} {
		var ack string
		if err := f.backend.Dispatch(t.Context(), router.Decision{Handler: h}, "drop the docs task", func(s string) { ack = s }); err != nil {
			t.Fatal(err)
		}
		if want := "Queued for the " + string(h) + " of adopted run " + adoptedRun + ": drop the docs task"; ack != want {
			t.Fatalf("ack = %q, want %q", ack, want)
		}
	}
	dirs, err := f.store.Directives().List(t.Context(), store.DirectiveFilter{RunID: adoptedRun, ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var owner int
	for i := range dirs {
		if dirs[i].Source == store.DirectiveSourceOwner {
			owner++
		}
	}
	if owner != 2 || len(instructions(t, f.store, chatRun)) != 0 {
		t.Fatalf("adopted run owner directives = %d, want planner and dispatcher; front desk must get none", owner)
	}
}

// Planner instructions follow an explicitly named held run.
func TestPlannerInstructionNamingAdoptedRunGoesThere(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ text, want string }{
		{"for run " + adoptedRun + ": drop the docs task", adoptedRun},
		{"drop the docs task", chatRun},
	} {
		f := newAdoptedFixture(t, nil)
		if _, err := f.store.Runs().AcquireLease(t.Context(), chatRun, "orca-run", time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := f.backend.Dispatch(t.Context(), router.Decision{Handler: router.Planner}, tc.text, nil); err != nil {
			t.Fatal(err)
		}
		for _, run := range []string{adoptedRun, chatRun} {
			dirs, err := f.store.Directives().List(t.Context(), store.DirectiveFilter{RunID: run, Kinds: []store.DirectiveKind{store.DirectivePlanImport}})
			if err != nil {
				t.Fatal(err)
			}
			if want := run == tc.want; (len(dirs) == 1 && dirs[0].Text == tc.text) != want || len(dirs) > 1 {
				t.Fatalf("%q: plan imports on %s = %+v, want on %s only", tc.text, run, dirs, tc.want)
			}
		}
	}
	f := newAdoptedFixture(t, nil)
	if err := f.ctrls.Stop(t.Context(), adopt.Run{ID: adoptedRun, Dir: filepath.Join(f.root, ".shared", "subagent-swarm", adoptedRun)}); err != nil {
		t.Fatal(err)
	}
	if err := f.backend.Dispatch(t.Context(), router.Decision{Handler: router.Planner}, "for run "+adoptedRun+": drop the docs task", nil); err == nil || !strings.Contains(err.Error(), "adopted run "+adoptedRun+" has no live controller") {
		t.Fatalf("inactive adopted run planner dispatch = %v, want no live controller error", err)
	}
}

// OwnerRun resolves concierge directive targets at the front desk.
func TestOwnerRunPicksTheRunAnInstructionIsAbout(t *testing.T) {
	t.Parallel()
	const second = "20261113-054825-other"
	for _, tc := range []struct {
		name, run, text, want, err string
		held, stop                 []string
	}{
		{name: "tool names a held run", run: second, text: "hold", want: second, held: []string{adoptedRun, second}},
		{name: "text names a held run", text: "for run " + second + ": hold", want: second, held: []string{adoptedRun, second}},
		{name: "one held run", text: "hold", want: adoptedRun, held: []string{adoptedRun}},
		{name: "tool names an unheld run", run: "run-elsewhere", text: "hold", err: "run run-elsewhere is not held by this chat", held: []string{adoptedRun}},
		{name: "tool names synthetic front desk", run: chatRun, text: "hold", err: "run " + chatRun + " is not held by this chat", held: []string{adoptedRun}},
		{name: "tool names inactive adopted run", run: second, text: "hold", err: "adopted run " + second + " has no live controller", held: []string{adoptedRun, second}, stop: []string{second}},
		{name: "text names inactive adopted run while another is live", text: "for run " + second + ": hold", err: "adopted run " + second + " has no live controller", held: []string{adoptedRun, second}, stop: []string{second}},
		{name: "no live controller at front desk", text: "hold", err: "no controller is running for this chat", held: []string{adoptedRun}, stop: []string{adoptedRun}},
		{name: "several held runs, none named", text: "hold", err: "name the run", held: []string{adoptedRun, second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAdoptedFixture(t, nil, tc.held...)
			f.backend.FrontDesk = chatRun
			for _, id := range tc.stop {
				if err := f.ctrls.Stop(t.Context(), adopt.Run{ID: id, Dir: filepath.Join(f.root, ".shared", "subagent-swarm", id)}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := f.backend.OwnerRun(t.Context(), tc.run, tc.text)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("OwnerRun = %q, %v; want error %q", got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("OwnerRun = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestFrontDeskInstructionNamingOneOfSeveralLiveRuns(t *testing.T) {
	t.Parallel()
	clk := clock.NewFakeClock(clock.NewRealClock().Now().UTC())
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	for _, id := range []string{"front", "run-a", "run-b", "run-idle"} {
		if err := st.Runs().Create(t.Context(), store.Run{ID: id}); err != nil {
			t.Fatal(err)
		}
		if id == "front" || id == "run-idle" {
			continue
		}
		if _, err := st.Runs().AcquireLease(t.Context(), id, "orca-run", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	b := NewStoreBackend(st, "front")
	b.FrontDesk, b.Now = "front", clk.Now
	b.ListRuns = func(ctx context.Context) ([]store.Run, error) { return st.Runs().List(ctx) }
	var ack string
	if err := b.Dispatch(t.Context(), router.Decision{Handler: router.Dispatcher}, "for run-b: hold", func(s string) { ack = s }); err != nil {
		t.Fatal(err)
	}
	if want := "Queued for the dispatcher of run run-b, whose controller is running: for run-b: hold"; ack != want {
		t.Fatalf("ack = %q, want %q", ack, want)
	}
	if got := instructions(t, st, "run-b"); len(got) != 1 {
		t.Fatalf("run-b instructions = %q", got)
	}
	if err := b.Dispatch(t.Context(), router.Decision{Handler: router.Dispatcher}, "for run-idle: hold", func(s string) { ack = s }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ack, "Not queued: run run-idle has no live controller") {
		t.Fatalf("idle run ack = %q, want refusal", ack)
	}
	if got := instructions(t, st, "run-idle"); len(got) != 0 {
		t.Fatalf("run-idle instructions = %q, want none", got)
	}
}

func instructions(t *testing.T, st store.Store, runID string) []string {
	t.Helper()
	dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: runID, ActiveOnly: true, Kinds: []store.DirectiveKind{store.DirectiveInstruction}})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := range dirs {
		if dirs[i].Source == store.DirectiveSourceOwner {
			out = append(out, dirs[i].Text)
		}
	}
	return out
}

// failingJudge is a dispatcher whose every turn fails.
type failingJudge struct{ batches chan []store.Event }

func (j failingJudge) Judge(_ context.Context, _ string, events []store.Event) error {
	j.batches <- events
	return errors.New("model unavailable")
}

func TestControllerEscalationBecomesChatQuestion(t *testing.T) {
	t.Parallel()
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprint("dispatcher fails=", failing), func(t *testing.T) {
			t.Parallel()
			var judges adopt.JudgeFactory
			fj := failingJudge{batches: make(chan []store.Event, 8)}
			if failing {
				judges = func(string, func(context.Context, string, string, string) error) controller.Judge { return fj }
			}
			testEscalationBecomesChatQuestion(t, judges)
			if failing {
				// The controller outlives the failed turn and judges the answer.
				timeout := time.After(10 * time.Second)
				for answered := false; !answered; {
					select {
					case batch := <-fj.batches:
						answered = len(batch) == 1 && batch[0].Summary == "rework"
					case <-timeout:
						t.Fatal("controller stopped judging after the failed dispatcher turn")
					}
				}
			}
		})
	}
}

func testEscalationBecomesChatQuestion(t *testing.T, judges adopt.JudgeFactory) {
	t.Helper()
	f := newAdoptedFixture(t, judges)
	// Review asks for another SWE round past the cap: the controller escalates.
	f.driver.Emit(lane.Event{Kind: lane.EventPhaseReported, RunID: adoptedRun, LaneID: "orca-bare", TerminalID: "t-bare", Phase: "review", Report: &lane.PhaseReport{
		LaneID: "orca-bare", TerminalID: "t-bare", Phase: "review", Round: 3, Outcome: "needs-swe", HeadSHA: "abc123", PR: 401, Summary: "still broken",
	}})
	var s Snapshot
	deadline := time.Now().Add(10 * time.Second)
	for len(s.Questions) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("escalation never reached chat; status %q", s.Status)
		}
		// Let the judgment debounce elapse in fake time.
		f.clock.Advance(10 * time.Second)
		var err error
		if s, err = f.backend.Snapshot(t.Context()); err != nil {
			t.Fatal(err)
		}
		runtime.Gosched()
	}
	if len(s.Questions) != 1 || !strings.Contains(s.Status, "open questions: 1") {
		t.Fatalf("questions = %q status = %q, want one", s.Questions, s.Status)
	}
	q := s.Questions[0]
	for _, want := range []string{
		"[" + adoptedRun + "]",
		"Lane orca-bare escalated (escalated from review on verdict needs-swe at round 3)",
		"lane orca-bare",
		"choices: rework: start swe round 4 | stop: drop orca-bare | hold: keep orca-bare blocked",
		"default: hold: keep orca-bare blocked",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("question %q lacks %q", q, want)
		}
	}
	if out, err := f.backend.Command(t.Context(), "/asks"); err != nil || out != q {
		t.Fatalf("/asks = %q, %v", out, err)
	}
	// The owner's answer goes to the run that asked.
	if err := f.backend.Dispatch(t.Context(), router.Decision{Handler: router.Dispatcher}, "rework", nil); err != nil {
		t.Fatal(err)
	}
	if got := instructions(t, f.store, adoptedRun); len(got) != 1 || got[0] != "rework" {
		t.Fatalf("adopted run instructions = %q", got)
	}
}

func TestSlashCompletion(t *testing.T) {
	t.Parallel()
	f := newAdoptedFixture(t, nil, adoptedRun, "20261113-070000-other")
	m := New(t.Context(), chatRun, nil, nil)
	m.Backend = f.backend
	press := func(m Model, k tea.KeyPressMsg) Model {
		t.Helper()
		next, cmd := m.Update(k)
		m = next.(Model)
		for cmd != nil {
			next, cmd = m.Update(cmd())
			m = next.(Model)
		}
		return m
	}
	typed := func(m Model, s string) Model {
		for _, r := range s {
			m = press(m, tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		return m
	}
	tab := tea.KeyPressMsg{Code: tea.KeyTab}

	m = typed(m, "/")
	if view := m.View().Content; !strings.Contains(view, "/status  /lanes  /asks") || !strings.Contains(view, "/adopt [run-id]  /release [run-id]") {
		t.Fatalf("\"/\" shows no command list:\n%s", view)
	}
	m = press(typed(m, "ad"), tab)
	if m.input != "/adopt " {
		t.Fatalf("/ad + Tab = %q, want \"/adopt \"", m.input)
	}
	m = press(m, tab)
	if m.input != "/adopt 20261113-0" || !slices.Equal(m.hints, []string{adoptedRun, "20261113-070000-other"}) {
		t.Fatalf("/adopt + Tab = %q hints %q, want both run ids", m.input, m.hints)
	}
	if view := m.View().Content; !strings.Contains(view, adoptedRun) || !strings.Contains(view, "20261113-070000-other") {
		t.Fatalf("run ids not shown:\n%s", view)
	}
	m = press(typed(m, "7"), tab)
	if m.input != "/adopt 20261113-070000-other" {
		t.Fatalf("unique run id completion = %q", m.input)
	}
	m.input = "/rel"
	if m = press(press(m, tab), tab); m.input != "/release 20261113-0" {
		t.Fatalf("/rel + Tab + Tab = %q", m.input)
	}
	m.input = "/s"
	if m = press(m, tab); m.input != "/st" || !slices.Equal(m.hints, []string{"/status", "/stop"}) {
		t.Fatalf("/s + Tab = %q hints %q", m.input, m.hints)
	}
}
