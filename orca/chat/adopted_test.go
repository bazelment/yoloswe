package chat

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
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
	if !strings.Contains(s.Status, "lanes: 2 (done:1, running:1; review:1)") || !strings.Contains(s.Status, "adopted: "+adoptedRun) {
		t.Fatalf("status = %q, want the adopted run's two lanes counted by status and active phase", s.Status)
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

func TestInstructionNamingAdoptedRunReachesItsDispatcher(t *testing.T) {
	t.Parallel()
	f := newAdoptedFixture(t, nil)
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
	// The adopted CLI dispatcher reads owner messages from the run's events.
	if got := ownerMessages(t, f.store, adoptedRun); len(got) != 1 || got[0].Summary != text {
		t.Fatalf("adopted run owner messages = %+v, want the owner's", got)
	}
}

func ownerMessages(t *testing.T, st store.Store, runID string) []store.Event {
	t.Helper()
	events, err := st.Events().List(t.Context(), store.EventFilter{RunID: runID, Kinds: []store.EventKind{store.EventOwnerMessage}})
	if err != nil {
		t.Fatal(err)
	}
	return events
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

// Planner instructions follow an explicitly named held run or the single live run.
func TestPlannerInstructionNamingAdoptedRunGoesThere(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text, want    string
		ownController bool
	}{
		{"for run " + adoptedRun + ": drop the docs task", adoptedRun, true},
		{"drop the docs task", chatRun, true},
		{"drop the docs task", adoptedRun, false},
	} {
		f := newAdoptedFixture(t, nil)
		if tc.ownController {
			if _, err := f.store.Runs().AcquireLease(t.Context(), chatRun, "orca-run", time.Hour); err != nil {
				t.Fatal(err)
			}
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
				t.Fatalf("%q (ownController=%t): plan imports on %s = %+v, want on %s only", tc.text, tc.ownController, run, dirs, tc.want)
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
		name, run, text, want, err, unattached string
		held, stop, listed                     []string
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
		{name: "frontDeskTarget single live run in store", text: "hold", want: "run-live", held: []string{adoptedRun}, stop: []string{adoptedRun}, listed: []string{"run-live"}},
		{name: "frontDeskTarget named live run in store", text: "for run-b: hold", want: "run-b", held: []string{adoptedRun}, stop: []string{adoptedRun}, listed: []string{"run-a", "run-b"}},
		{name: "frontDeskTarget live run outside attached store", text: "hold", err: "run run-external is not held by this chat; /runs run-external first", held: []string{adoptedRun}, stop: []string{adoptedRun}, unattached: "run-external"},
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
			var offered []store.Run
			for _, id := range tc.listed {
				if err := f.store.Runs().Create(t.Context(), store.Run{ID: id}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.Runs().AcquireLease(t.Context(), id, "orca-run", time.Hour); err != nil {
					t.Fatal(err)
				}
				r, err := f.store.Runs().Get(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				offered = append(offered, *r)
			}
			if tc.unattached != "" {
				offered = append(offered, store.Run{ID: tc.unattached, Lease: &store.Lease{HolderID: "orca-run", ExpiresAt: f.clock.Now().Add(time.Hour)}})
			}
			if len(offered) > 0 {
				f.backend.ListRuns = func(context.Context) ([]store.Run, error) { return offered, nil }
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
	if view := m.View().Content; !strings.Contains(view, "/status  /lanes  /asks") || !strings.Contains(view, "/adopt [run-id]  /release [run-id]  /restart") {
		t.Fatalf("\"/\" shows no command list:\n%s", view)
	}
	if helpOut, err := f.backend.Command(t.Context(), "/help"); err != nil || !strings.Contains(helpOut, "/restart") {
		t.Fatalf("/help = %q, %v; want /restart listed", helpOut, err)
	}
	if _, err := f.backend.Command(t.Context(), "/restart /tmp/other"); err == nil || strings.Contains(err.Error(), "usage: /restart") {
		t.Fatalf("/restart <path> = %v, want target validation error", err)
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
	m.input = "/rest"
	if m = press(m, tab); m.input != "/restart " {
		t.Fatalf("/rest + Tab = %q, want \"/restart \"", m.input)
	}
	m.input = "/s"
	if m = press(m, tab); m.input != "/st" || !slices.Equal(m.hints, []string{"/status", "/stop"}) {
		t.Fatalf("/s + Tab = %q hints %q", m.input, m.hints)
	}
}

func TestCostLineSumsTurnAndDecisionCostsAcrossFrontDeskAndHeldRuns(t *testing.T) {
	t.Parallel()
	f := newAdoptedFixture(t, nil)
	f.backend.FrontDesk = chatRun
	ctx := t.Context()

	// 1. Front-desk concierge thread with a prior-day turn ($5.00, must be excluded from today)
	// and a today turn with explicit CostUSD ($0.12).
	if _, err := f.store.Threads().StartThread(ctx, store.Thread{
		ID:    "concierge-" + chatRun,
		RunID: chatRun,
		Role:  store.ThreadRoleConcierge,
	}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(-24 * time.Hour)
	if _, err := f.store.Threads().StartTurn(ctx, store.Turn{
		ID:       "c-turn-yesterday",
		ThreadID: "concierge-" + chatRun,
		RunID:    chatRun,
		Input:    "yesterday status?",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Threads().CompleteTurn(ctx, "concierge-"+chatRun, "c-turn-yesterday", store.TurnStatusCompleted, model.Usage{CostUSD: 5.00}, ""); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(24 * time.Hour)
	if _, err := f.store.Threads().StartTurn(ctx, store.Turn{
		ID:       "c-turn-1",
		ThreadID: "concierge-" + chatRun,
		RunID:    chatRun,
		Input:    "status?",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Threads().CompleteTurn(ctx, "concierge-"+chatRun, "c-turn-1", store.TurnStatusCompleted, model.Usage{CostUSD: 0.12}, ""); err != nil {
		t.Fatal(err)
	}

	// 2. Adopted run dispatcher turn with CostUSD ($0.25) and a decision on the same turn ($0.10, included in the $0.25 turn cost).
	if _, err := f.store.Threads().StartThread(ctx, store.Thread{
		ID:    "dispatcher-" + adoptedRun,
		RunID: adoptedRun,
		Role:  store.ThreadRoleDispatcher,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Threads().StartTurn(ctx, store.Turn{
		ID:       "d-turn-1",
		ThreadID: "dispatcher-" + adoptedRun,
		RunID:    adoptedRun,
		Input:    "batch 1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Threads().CompleteTurn(ctx, "dispatcher-"+adoptedRun, "d-turn-1", store.TurnStatusCompleted, model.Usage{CostUSD: 0.25}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Decisions().Append(ctx, store.Decision{
		RunID:     adoptedRun,
		TurnID:    "d-turn-1",
		Category:  store.DecisionJudgment,
		Actor:     "dispatcher",
		Tool:      "lane.spawn",
		CostUSD:   0.10,
		CreatedAt: f.clock.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// 3. Adopted run dispatcher turn with CostUSD == 0 but token usage (10,000 input + 2,000 output on sonnet = $0.03 + $0.03 = $0.06).
	if _, err := f.store.Threads().StartTurn(ctx, store.Turn{
		ID:       "d-turn-2",
		ThreadID: "dispatcher-" + adoptedRun,
		RunID:    adoptedRun,
		Input:    "batch 2",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Threads().CompleteTurn(ctx, "dispatcher-"+adoptedRun, "d-turn-2", store.TurnStatusCompleted, model.Usage{InputTokens: 10_000, OutputTokens: 2_000}, ""); err != nil {
		t.Fatal(err)
	}

	// Total expected: $0.12 + $0.25 + $0.06 = $0.43.
	snap, err := f.backend.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snap.Status, "cost today: $0.43") {
		t.Fatalf("Snapshot.Status = %q, want cost today: $0.43", snap.Status)
	}
	statusOut, err := f.backend.Command(ctx, "/status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusOut, "cost today: $0.43") {
		t.Fatalf("/status = %q, want cost today: $0.43", statusOut)
	}
}

func TestStatusListsStandingRules(t *testing.T) {
	t.Parallel()
	f := newAdoptedFixture(t, nil)
	if err := f.store.Directives().Add(t.Context(), store.Directive{
		ID:     "rule-store-cap",
		RunID:  adoptedRun,
		Kind:   store.DirectiveStanding,
		Source: store.DirectiveSourceOwner,
		Text:   "Run at most 2 store lanes concurrently.",
		Status: store.DirectiveActive,
	}); err != nil {
		t.Fatal(err)
	}
	out, err := f.backend.Command(t.Context(), "/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"standing rules: 1",
		"[" + adoptedRun + "] rule-store-cap: Run at most 2 store lanes concurrently.",
		"forget rule-store-cap",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("/status missing %q:\n%s", want, out)
		}
	}

	// Updating OBJECTIVE.md on disk is picked up when /status runs (via RefreshObjectives).
	objPath := filepath.Join(f.root, ".shared", "subagent-swarm", adoptedRun, "OBJECTIVE.md")
	if err := os.WriteFile(objPath, []byte("Goal: updated host goal\nNext: staff m4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.backend.Command(t.Context(), "/status"); err != nil {
		t.Fatal(err)
	}
	gotRun, err := f.store.Runs().Get(t.Context(), adoptedRun)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotRun.Objective, "updated host goal") || !strings.Contains(gotRun.Objective, "Next: staff m4") {
		t.Fatalf("expected /status to refresh adopted run objective from disk, got %q", gotRun.Objective)
	}
}
