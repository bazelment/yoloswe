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
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/store"
)

var swarmNow = time.Date(2026, 11, 14, 4, 42, 0, 0, time.UTC)

// writeHostSwarm writes a host swarm run directory whose files were last
// touched at last.
func writeHostSwarm(t *testing.T, root, id, state string, last time.Time) {
	t.Helper()
	dir := filepath.Join(root, ".shared", "subagent-swarm", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, last, last); err != nil {
		t.Fatal(err)
	}
}

func TestRunsListsOrcaRunsAndHostSwarms(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeHostSwarm(t, root, "20261113-054825-orca-v2",
		`{"config":{"goal":"ship orca v2"},"tasks":[{"id":"chatux","phase":"build","status":"running"},{"id":"rename","phase":"review","status":"running","pr":401},{"id":"docs","phase":"build","status":"done"}]}`,
		swarmNow.Add(-4*time.Minute))
	writeHostSwarm(t, root, "20261112-090000-docs",
		`{"config":{"goal":"refresh docs"},"tasks":[]}`,
		swarmNow.Add(-26*time.Hour))
	b := &StoreBackend{
		Now: func() time.Time { return swarmNow },
		ListRuns: func(context.Context) ([]store.Run, error) {
			return []store.Run{{ID: "run-1", Objective: "fix the flaky test"}}, nil
		},
		HostSwarms: adopt.Importer{Roots: []string{root}}.Discover,
	}
	got, err := b.Command(t.Context(), "/runs")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"orca runs (1; /runs <id> attaches):",
		"  run-1: fix the flaky test",
		"host swarms (2; /adopt <id> takes one over):",
		"  20261113-054825-orca-v2: ship orca v2 | lanes: 3 (done:1, running:2; build:2, review:1) | last activity: 2026-11-14T04:38:00Z (4m0s ago) | orchestrator: claude | format: subagent-swarm-nebula | active",
		"  20261112-090000-docs: refresh docs | lanes: 0 | last activity: 2026-11-13T02:42:00Z (26h0m0s ago) | orchestrator: claude | format: subagent-swarm-nebula | finished",
	}, "\n")
	if got != want {
		t.Fatalf("/runs =\n%s\nwant\n%s", got, want)
	}

	detail := b.Swarms(t.Context()).Render(swarmNow, true)
	for _, line := range []string{
		"    - chatux: phase=build status=running round=0 pr=- ",
		"    - rename: phase=review status=running round=0 pr=#401 ",
	} {
		if !slices.Contains(strings.Split(detail, "\n"), line) {
			t.Fatalf("detail missing %q:\n%s", line, detail)
		}
	}

	filtered := b.Swarms(t.Context()).Filter("orca-v2").Render(swarmNow, true)
	if !strings.Contains(filtered, "20261113-054825-orca-v2") || strings.Contains(filtered, "20261112-090000-docs") {
		t.Fatalf("filtered swarms =\n%s", filtered)
	}
}

func TestRunsSaysWhenNeitherGroupHasRuns(t *testing.T) {
	t.Parallel()
	b := &StoreBackend{AdoptionUnavailable: "watch_lanes.sh not found"}
	got, err := b.Command(t.Context(), "/runs")
	if err != nil {
		t.Fatal(err)
	}
	if want := "orca runs: none\nhost swarms: not listed: watch_lanes.sh not found"; got != want {
		t.Fatalf("/runs = %q, want %q", got, want)
	}
}

func TestStatusShowsRunGoalLanesQuestionsAndCost(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(ctx, store.Run{ID: "run", Objective: "ship chat"}); err != nil {
		t.Fatal(err)
	}
	for _, l := range []store.Lane{
		{RunID: "run", ID: "lane-a", Phase: "build", Status: "running"},
		{RunID: "run", ID: "lane-b", Phase: "review", Status: "running"},
		{RunID: "run", ID: "lane-c", Phase: "build", Status: "running"},
	} {
		if err := st.Lanes().Upsert(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Directives().Add(ctx, store.Directive{ID: "ask", RunID: "run", Ref: "owner_ask:q-1", Scope: "lane-b", Text: `{"id":"q-1","question":"Merge now?"}`, Kind: store.DirectiveInstruction, Source: store.DirectiveSourceDispatcher, Status: store.DirectiveActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Decisions().Append(ctx, store.Decision{RunID: "run", Actor: "dispatcher", Category: store.DecisionJudgment, Tool: "lane.launch", CostUSD: 0.25}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "run")
	got, err := b.Command(ctx, "/status")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"goal: ship chat | run: run",
		"lanes: 3 (running:3; build:2, review:1)",
		"open questions: 1",
		"  q-1: Merge now? (lane lane-b)",
		"held runs: run (attached)",
		"cost today: $0.25",
	}, "\n")
	if got != want {
		t.Fatalf("/status =\n%s\nwant\n%s", got, want)
	}
}

func TestStatusLineCountsLanesByStatusAndPhase(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(ctx, store.Run{ID: "20261114-trial-m1", Objective: "trial m1"}); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		status := store.LaneStatus("done")
		if i >= 4 {
			status = "running"
		}
		if err := st.Lanes().Upsert(ctx, store.Lane{
			RunID:  "20261114-trial-m1",
			ID:     "lane-" + string(rune('a'+i)),
			Phase:  "review",
			Status: status,
		}); err != nil {
			t.Fatal(err)
		}
	}
	b := NewStoreBackend(st, "20261114-trial-m1")
	s, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Status, "lanes: 6 (done:4, running:2; review:6)") {
		t.Fatalf("status line = %q, want lanes counted by status (done:4, running:2) and phase (review:6)", s.Status)
	}
}

func TestStatusAtFrontDeskSaysNoRunYet(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(ctx, store.Run{ID: "front-desk"}); err != nil {
		t.Fatal(err)
	}
	b := NewStoreBackend(st, "front-desk")
	b.FrontDesk = "front-desk"
	got, err := b.Command(ctx, "/status")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"front desk: no run yet | run: front-desk",
		"lanes: 0",
		"open questions: 0",
		"held runs: none (/runs lists runs and host swarms)",
		"cost today: $0.00",
	}, "\n")
	if got != want {
		t.Fatalf("/status =\n%s\nwant\n%s", got, want)
	}
	s, err := b.Snapshot(ctx)
	if err != nil || !strings.HasPrefix(s.Status, "front desk: no run yet | lanes: 0 |") {
		t.Fatalf("status line %q err %v", s.Status, err)
	}
}

// commandRecorder is a Backend that records the commands chat sends.
type commandRecorder struct{ got chan string }

func (commandRecorder) Snapshot(context.Context) (Snapshot, error) { return Snapshot{}, nil }
func (r commandRecorder) Command(_ context.Context, text string) (string, error) {
	r.got <- text
	return "ok", nil
}

func TestCtrlUClearsInputBeforeTheNextCommand(t *testing.T) {
	t.Parallel()
	rec := commandRecorder{got: make(chan string, 1)}
	m := New(t.Context(), "run", nil, nil)
	m.Backend = rec
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 30))
	tm.Type("which swarms are active")
	tm.Send(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	submit(tm, "/status")
	select {
	case got := <-rec.got:
		if got != "/status" {
			t.Fatalf("command = %q, want /status", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no command sent")
	}
	finish(t, tm)
}

func TestReadlineKeysEditInput(t *testing.T) {
	t.Parallel()
	ctrl := func(c rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: c, Mod: tea.ModCtrl} }
	typeText := func(s string) []tea.KeyPressMsg {
		var out []tea.KeyPressMsg
		for _, r := range s {
			out = append(out, tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		return out
	}
	keys := func(groups ...[]tea.KeyPressMsg) []tea.KeyPressMsg {
		var out []tea.KeyPressMsg
		for _, g := range groups {
			out = append(out, g...)
		}
		return out
	}
	one := func(k tea.KeyPressMsg) []tea.KeyPressMsg { return []tea.KeyPressMsg{k} }
	for _, tc := range []struct {
		name, want string
		keys       []tea.KeyPressMsg
		cursor     int
	}{
		{"ctrl-u kills to start", "", keys(typeText("old text"), one(ctrl('u'))), 0},
		{"ctrl-u keeps text after cursor", "text", keys(typeText("old text"), one(ctrl('a')), one(ctrl('f')), one(ctrl('f')), one(ctrl('f')), one(ctrl('f')), one(ctrl('u'))), 0},
		{"ctrl-w kills previous word", "show the ", keys(typeText("show the lanes"), one(ctrl('w'))), 9},
		{"ctrl-w skips trailing space", "show ", keys(typeText("show the  "), one(ctrl('w'))), 5},
		{"ctrl-a then type inserts at start", "/status now", keys(typeText(" now"), one(ctrl('a')), typeText("/status")), 7},
		{"ctrl-e returns to end", "ab!", keys(typeText("ab"), one(ctrl('a')), one(ctrl('e')), typeText("!")), 3},
		{"ctrl-k kills to end", "show", keys(typeText("show lanes"), one(ctrl('a')), one(ctrl('f')), one(ctrl('f')), one(ctrl('f')), one(ctrl('f')), one(ctrl('k'))), 4},
		{"left and backspace edit mid-line", "é🙂", keys(typeText("é猫🙂"), one(tea.KeyPressMsg{Code: tea.KeyLeft}), one(tea.KeyPressMsg{Code: tea.KeyBackspace})), len("é")},
		{"delete removes under cursor", "ac", keys(typeText("abc"), one(tea.KeyPressMsg{Code: tea.KeyHome}), one(tea.KeyPressMsg{Code: tea.KeyRight}), one(tea.KeyPressMsg{Code: tea.KeyDelete})), 1},
	} {
		m := New(t.Context(), "run", nil, nil)
		for _, k := range tc.keys {
			next, _ := m.Update(k)
			m = next.(Model)
		}
		if m.input != tc.want || m.cursor != tc.cursor {
			t.Errorf("%s: input %q cursor %d, want %q cursor %d", tc.name, m.input, m.cursor, tc.want, tc.cursor)
		}
	}
}

// TestHostSwarmSaysNothingRunsOnlyWhenChecked: the summary claims nothing
// runs only when every lane is finished or its terminal was checked idle and
// the orchestrator has no newer run; otherwise it says what it did not check.
func TestHostSwarmSaysNothingRunsOnlyWhenChecked(t *testing.T) {
	t.Parallel()
	running := adopt.Lane{ID: "m2-default", Phase: "clean", Status: "running", Round: 1, Terminal: "term-m2"}
	done := adopt.Lane{ID: "m1-base", Phase: "review", Status: "done", Round: 1, Terminal: "term-m1", PR: "5604"}
	idle := HostProbe{TerminalState: func(context.Context, string) (string, error) { return "idle", nil }}
	working := HostProbe{TerminalState: func(context.Context, string) (string, error) { return "working", nil }}
	for _, tc := range []struct {
		name  string
		run   adopt.Run
		probe *HostProbe
		want  []string
		never []string
	}{
		{name: "all finished", run: adopt.Run{ID: "r", State: adopt.RunFinished, Lanes: []adopt.Lane{done}}, probe: &idle,
			want:  []string{"    running now: nothing: every lane is finished or its terminal checked idle, and the orchestrator has no newer run", "        pr #5604: GitHub state not checked"},
			never: []string{"not checked: "}},
		{name: "running lane checked idle", run: adopt.Run{ID: "r", State: adopt.RunActive, Lanes: []adopt.Lane{running}}, probe: &idle,
			want: []string{"    running now: nothing: every lane is finished or its terminal checked idle, and the orchestrator has no newer run"}},
		{name: "running lane working", run: adopt.Run{ID: "r", State: adopt.RunActive, Lanes: []adopt.Lane{running, done}}, probe: &working,
			want: []string{"    running now: m2-default (clean, agent working)"}},
		{name: "not probed", run: adopt.Run{ID: "r", State: adopt.RunActive, Lanes: []adopt.Lane{running}},
			want:  []string{"    running now: unknown for m2-default", "    not checked: terminal agent states and PR states (no live probe)"},
			never: []string{"running now: nothing"}},
		{name: "probe off", run: adopt.Run{ID: "r", State: adopt.RunActive, Unchecked: []string{"Nebula run orchestrator (no Nebula client)"}, Lanes: []adopt.Lane{running}},
			probe: &HostProbe{Off: []string{"lane terminal agent states (no Nebula client)"}},
			want:  []string{"    running now: unknown for m2-default", "    not checked: Nebula run orchestrator (no Nebula client); lane terminal agent states (no Nebula client)"},
			never: []string{"running now: nothing"}},
		{name: "superseded", run: adopt.Run{ID: "old", State: adopt.RunSuperseded, SupersededBy: "20261114-trial-m1", DrivenBy: "sess-orch", Lanes: []adopt.Lane{running}}, probe: &idle,
			want:  []string{"    state: superseded by 20261114-trial-m1 (its orchestrator now drives that run) | driven by: sess-orch | step loop: none running", "    running now: nothing here; orchestrator sess-orch moved on to run 20261114-trial-m1, ask about that run"},
			never: []string{"every lane is finished"}},
		{name: "live step loop", run: adopt.Run{ID: "r", State: adopt.RunActive, Loop: "pids 7,8 in tmux 0:1.0", Lanes: []adopt.Lane{running}}, probe: &idle,
			want: []string{"    running now: the step loop (pids 7,8 in tmux 0:1.0)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := SwarmList{Host: []adopt.Run{tc.run}}
			if tc.probe != nil {
				l.Probe(t.Context(), *tc.probe)
			}
			out := l.Render(swarmNow, true)
			lines := strings.Split(out, "\n")
			for _, w := range tc.want {
				if !slices.Contains(lines, w) {
					t.Fatalf("missing %q:\n%s", w, out)
				}
			}
			for _, n := range tc.never {
				if strings.Contains(out, n) {
					t.Fatalf("says %q:\n%s", n, out)
				}
			}
		})
	}
}

func TestSwarmsRecentFirstCompactRowsAndFilter(t *testing.T) {
	t.Parallel()
	var runs []adopt.Run
	for i := range 20 {
		id := "20261110-old-" + string(rune('a'+i))
		runs = append(runs, adopt.Run{
			ID:           id,
			Goal:         "old run " + id,
			State:        adopt.RunFinished,
			LastActivity: swarmNow.Add(-time.Duration(20-i) * time.Hour).Format(time.RFC3339),
			Lanes: []adopt.Lane{
				{ID: "lane-1", Phase: "review", Status: "done", PR: "100"},
				{ID: "lane-2", Phase: "review", Status: "done", PR: "101"},
			},
		})
	}
	runs = append(runs, adopt.Run{
		ID:           "20261114-trial-m1",
		Goal:         "trial m1",
		State:        adopt.RunActive,
		LastActivity: swarmNow.Add(-2 * time.Minute).Format(time.RFC3339),
		Lanes: []adopt.Lane{
			{ID: "m1-a", Phase: "review", Status: "done", PR: "410"},
			{ID: "m1-b", Phase: "review", Status: "running", PR: "411", LastEvent: "reviewing PR #411", Next: "merge"},
		},
	})
	l := SwarmList{Host: runs}
	probedPRs := 0
	l.Probe(t.Context(), HostProbe{
		PR: func(_ context.Context, _, _, num string) (PRStatus, error) {
			probedPRs++
			return PRStatus{State: "OPEN", Checks: "pass (" + num + ")"}, nil
		},
	})
	out := l.Render(swarmNow, true)
	lines := strings.Split(out, "\n")
	if len(lines) < 3 || !strings.Contains(lines[2], "20261114-trial-m1:") {
		t.Fatalf("expected newest active run first, got:\n%s", out)
	}
	for _, r := range runs {
		if !strings.Contains(out, r.ID) {
			t.Fatalf("long swarms list lost run %s:\n%s", r.ID, out)
		}
	}
	if strings.Contains(out, "pr #100") {
		t.Fatalf("expected non-active runs in multi-run list to render compact rows without per-lane bloat:\n%s", out)
	}
	if probedPRs != 2 {
		t.Fatalf("expected Probe on unfiltered list to probe only active run's 2 PRs, probed %d", probedPRs)
	}

	filtered := l.Filter("20261110-old-a")
	filtered.Probe(t.Context(), HostProbe{
		PR: func(_ context.Context, _, _, num string) (PRStatus, error) {
			return PRStatus{State: "MERGED", Checks: num}, nil
		},
	})
	fout := filtered.Render(swarmNow, true)
	if !strings.Contains(fout, "20261110-old-a") || !strings.Contains(fout, "pr #100 (GitHub): merged, checks: 100") || strings.Contains(fout, "20261114-trial-m1") {
		t.Fatalf("filtered run should render full detail for matched run only, got:\n%s", fout)
	}
}
