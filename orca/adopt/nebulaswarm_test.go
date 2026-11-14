package adopt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

const nsRun = "20261114-trial-m1"

// writeNebulaSwarmRun writes a nebula-swarm run directory (sw_init layout) at
// <root>/<id>: lane m1-base finished with its PR merged, lane m2-default in
// its clean phase with an open PR. orch is the ledger's orchestrator session.
func writeNebulaSwarmRun(t *testing.T, root, id, orch string, last time.Time) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"state.json": `{"config":{"goal":"trial m1: slim prompts","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"` + orch + `","merge":"none",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high","prompt":"","chain":[{"kind":"claude","model":"opus","effort":"high"}]},
{"name":"clean","kind":"codex","model":"gpt-5.6-sol","effort":"high","prompt":"","chain":[{"kind":"codex","model":"gpt-5.6-sol","effort":"high"}]},
{"name":"review","kind":"claude","model":"sonnet","effort":"high","prompt":"","chain":[{"kind":"claude","model":"sonnet","effort":"high"}]}]},
"tasks":[
{"id":"m1-base","title":"base prompts","branch":"swarm-lane/` + id + `/m1-base","priority":"p1","status":"done","phase":"review","round":1,"session_id":"sess-m1","terminals":{"swe":{"1":"term-m1-swe"},"review":{"1":"term-m1-review"}},"pr":5604,"pr_head":"aaa","approval_sha":"aaa","merge_sha":"mmm","terminal_state":"","verdict":"pass"},
{"id":"m2-default","title":"default prompts","branch":"swarm-lane/` + id + `/m2-default","priority":"p1","status":"running","phase":"clean","round":1,"session_id":"sess-m2","terminals":{"swe":{"1":"term-m2-swe"},"clean":{"1":"term-m2-clean"}},"pr":5705,"pr_head":"bbb","terminal_state":"working","verdict":""}]}
`,
		"OBJECTIVE.md":                 "# " + id + "\n\nGoal: trial m1: slim prompts.\n",
		"PROGRESS.md":                  "Merged: m1-base #5604.\nNext: m2-default review after clean; then merge train.\n",
		"ESCALATIONS.md":               "",
		"env.sh":                       "export RUN=" + dir + "\n",
		".repo-slug":                   "sycamore-labs/nebula\n",
		"m2-default.clean.prompt":      "clean the branch\n",
		"reports/m1-base.review.r1.md": "Outcome: done\nHEAD: aaa\nVerdict: pass aaa\n",
		"reports/m2-default.swe.r1.md": "Outcome: done\nHEAD: bbb\nPR: 5705\n",
		"EVENTS": "2026-11-14T05:20:00Z\tm1-base\tmerged\t#5604 mmm\n" +
			"2026-11-14T05:30:00Z\tm2-default\tphase\tswe -> clean\n" +
			"2026-11-14T05:35:00Z\tm2-default\tstarted\tclean term-m2-clean\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		write(t, p, body)
		if err := os.Chtimes(p, last, last); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDiscoversNebulaSwarmRun(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".swarm")
	last := time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC)
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", last)

	runs, err := Importer{Roots: []string{root}}.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want the one nebula-swarm run", runs)
	}
	r := runs[0]
	if r.ID != nsRun || r.Dir != dir || r.Format != FormatNebulaSwarm || r.Goal != "trial m1: slim prompts" ||
		r.DrivenBy != "sess-orch" || r.State != RunActive || r.Repo != "sycamore-labs/nebula" {
		t.Fatalf("run = %+v", r)
	}
	if len(r.Lanes) != 2 {
		t.Fatalf("lanes = %+v", r.Lanes)
	}
	m1, m2 := r.Lanes[0], r.Lanes[1]
	if m1.ID != "m1-base" || m1.Status != "done" || m1.PR != "5604" || m1.Session != "sess-m1" || m1.Terminal != "term-m1-review" ||
		m1.ReportPath != filepath.Join(dir, "reports", "m1-base.review.r1.md") || m1.Report != "Outcome: done" ||
		m1.LastEvent != "2026-11-14T05:20:00Z merged #5604 mmm" {
		t.Fatalf("m1-base = %+v", m1)
	}
	if m2.ID != "m2-default" || m2.Phase != "clean" || m2.Status != "running" || m2.Round != 1 || m2.PR != "5705" ||
		m2.Session != "sess-m2" || m2.Terminal != "term-m2-clean" || m2.TerminalState != "working" ||
		m2.LastEvent != "2026-11-14T05:35:00Z started clean term-m2-clean" ||
		m2.Next != "Next: m2-default review after clean; then merge train." {
		t.Fatalf("m2-default = %+v", m2)
	}
}

// nsSnapshot is every regular file of a run directory (reports/ included),
// leaving out the bridge log a release appends to.
func nsSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == stepBridgeMarker+".log" {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func nsService(root string, w Watcher, label *fakeLabeler) *Service {
	return &Service{
		Importer: Importer{Roots: []string{root}},
		Labeler:  label,
		Watcher:  w,
		Holder:   ChatHolder("chat-1"),
		Now:      func() time.Time { return time.Date(2026, 11, 14, 5, 40, 0, 0, time.UTC) },
	}
}

// fakeSwarmScript stands in for swarm_nebula.sh: its sw_step waits like a
// real tick does.
const fakeSwarmScript = "sw_step() { sleep 600; }\n"

// TestAdoptNebulaSwarmStopsAndRestartsStepLoop drives the handover against
// real processes: a host sw_step loop runs in the run's directory, adopt
// imports the lanes with their sessions and stops the loop, release puts the
// files back and starts a step-loop bridge.
func TestAdoptNebulaSwarmStopsAndRestartsStepLoop(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	script := filepath.Join(t.TempDir(), "swarm_nebula.sh")
	write(t, script, fakeSwarmScript)
	loops := StepLoopWatcher{Script: script, StopTimeout: 5 * time.Second}
	fw := FormatWatcher{Nebula: loops, SubagentMissing: "watch_lanes.sh not found", StopTimeout: 5 * time.Second,
		Panes: func(context.Context) ([]Pane, error) {
			// The host loop's parent (this test) plays the orchestrator's tmux pane.
			return []Pane{{PID: os.Getpid(), Target: "0:1.0", Window: "claude", Title: "slim-swarm-prompts"}}, nil
		}}

	host := exec.Command("bash", "-c", `source "$2"; sw_step "$1"; :`, "host-loop", dir, script)
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = host.Wait(); close(exited) }()
	t.Cleanup(func() { _ = host.Process.Kill() })
	var pids []int
	waitFor(t, func() bool { pids, _ = fw.Running(t.Context(), dir); return len(pids) == 2 })
	if pids[0] != host.Process.Pid {
		t.Fatalf("loop pids %v, want the host shell %d and its sw_step child", pids, host.Process.Pid)
	}

	runs, err := Importer{Roots: []string{root}, Loops: fw}.Discover(t.Context())
	if err != nil || len(runs) != 1 {
		t.Fatalf("discover = %+v, %v", runs, err)
	}
	if want := fmt.Sprintf("pids %d,%d in tmux 0:1.0 (window \"claude\", title \"slim-swarm-prompts\")", pids[0], pids[1]); runs[0].Loop != want {
		t.Fatalf("loop = %q, want %q", runs[0].Loop, want)
	}
	// The ledger's session wins over the pane; a host-run ledger names none.
	if runs[0].DrivenBy != "sess-orch" {
		t.Fatalf("driven by %q", runs[0].DrivenBy)
	}
	write(t, filepath.Join(dir, "state.json"), strings.Replace(readText(filepath.Join(dir, "state.json")), `"orchestrator_session_id":"sess-orch"`, `"orchestrator_session_id":null`, 1))
	if runs, err = (Importer{Roots: []string{root}, Loops: fw}).Discover(t.Context()); err != nil || runs[0].DrivenBy != "tmux 0:1.0" {
		t.Fatalf("host-run ledger driven by %+v, %v; want its loop's tmux pane", runs, err)
	}

	before := nsSnapshot(t, dir)
	label := &fakeLabeler{label: "claude"}
	s := nsService(root, fw, label)
	r, err := s.Adopt(t.Context(), nsRun)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("host step loop still running after adopt")
	}
	gotLanes := map[string][3]string{}
	for _, l := range r.Lanes {
		gotLanes[l.ID] = [3]string{l.Session, l.Terminal, l.PR}
	}
	if want := map[string][3]string{"m1-base": {"sess-m1", "term-m1-review", "5604"}, "m2-default": {"sess-m2", "term-m2-clean", "5705"}}; !reflect.DeepEqual(gotLanes, want) {
		t.Fatalf("adopted lanes = %v, want %v", gotLanes, want)
	}
	j, err := readJournal(filepath.Join(dir, leaseFile))
	if err != nil || j.State != stateAdopted || j.Import == nil || len(j.Import.Lanes) != 2 || len(j.WatcherPIDs) != 2 || label.label != "orca" {
		t.Fatalf("journal %+v err %v label %q", j, err, label.label)
	}
	for _, n := range handoverFiles {
		if b := readText(filepath.Join(dir, n)); !strings.HasPrefix(b, "ORCHESTRATOR: orca since 2026-11-14T05:40:00Z (orca-chat:chat-1)") {
			t.Fatalf("%s lacks the stand-down line: %q", n, b)
		}
	}
	if pids, err := loops.Running(t.Context(), dir); err != nil || len(pids) != 0 {
		t.Fatalf("step loop after adopt: %v %v", pids, err)
	}

	if err := s.Release(t.Context(), nsRun); err != nil {
		t.Fatal(err)
	}
	var bridge []int
	waitFor(t, func() bool { bridge, _ = loops.Running(t.Context(), dir); return len(bridge) == 2 })
	t.Cleanup(func() { _ = loops.Stop(context.Background(), bridge) })
	if args := readText(fmt.Sprintf("/proc/%d/cmdline", bridge[0])); !strings.Contains(args, stepBridgeMarker) {
		t.Fatalf("restarted loop %d is not the step bridge: %q", bridge[0], args)
	}
	if after := nsSnapshot(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatalf("release left the run changed:\n got %v\nwant %v", after, before)
	}
	if label.label != "claude" {
		t.Fatalf("label after release = %q", label.label)
	}
	if err := loops.Stop(t.Context(), bridge); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptFakeStepLoopStoppedRecordedAndRestarted(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	marker := filepath.Join(t.TempDir(), "stepped")
	loops := StepLoopWatcher{StopTimeout: 5 * time.Second}
	fw := FormatWatcher{Nebula: loops, SubagentMissing: "watch_lanes.sh not found", StopTimeout: 5 * time.Second}

	loopCmd := fmt.Sprintf(`while (( SECONDS < 600 )); do echo "$1" >> %q; sleep 180; done`, marker)
	host := exec.Command("bash", "-c", loopCmd, "fake-step-loop", dir)
	host.Env = append(os.Environ(), "SECRET_API_KEY=do-not-persist-in-lease")
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = host.Wait(); close(exited) }()
	t.Cleanup(func() { _ = host.Process.Kill() })
	var initialPIDs []int
	waitFor(t, func() bool {
		initialPIDs, _ = fw.Running(t.Context(), dir)
		return fileExists(marker) && len(initialPIDs) == 2
	})

	before := nsSnapshot(t, dir)
	label := &fakeLabeler{label: "claude"}
	s := nsService(root, fw, label)
	if _, err := s.Adopt(t.Context(), nsRun); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("fake step loop still running after adopt")
	}
	for _, pid := range initialPIDs {
		if procAlive(pid) {
			t.Fatalf("loop or sleep child pid %d still alive after adopt", pid)
		}
	}
	leasePath := filepath.Join(dir, leaseFile)
	j, err := readJournal(leasePath)
	if err != nil || len(j.WatcherPIDs) != 2 || j.StoppedLoop == nil || !strings.Contains(j.StoppedLoop.Command, dir) {
		t.Fatalf("journal = %+v, stopped_loop = %+v, err = %v", j, j.StoppedLoop, err)
	}
	if rawLease := readText(leasePath); strings.Contains(rawLease, "do-not-persist-in-lease") {
		t.Fatalf("lease file leaked process environment secret:\n%s", rawLease)
	}
	if info, err := os.Stat(leasePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lease file mode = %v, err = %v; want 0600", info, err)
	}

	if err := s.Release(t.Context(), nsRun); err != nil {
		t.Fatal(err)
	}
	var restarted []int
	waitFor(t, func() bool { restarted, _ = loops.Running(t.Context(), dir); return len(restarted) == 2 })
	t.Cleanup(func() { _ = loops.Stop(context.Background(), restarted) })
	if after := nsSnapshot(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatalf("release left the run changed:\n got %v\nwant %v", after, before)
	}
	if err := loops.Stop(t.Context(), restarted); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptRefusesWhenStepLoopNotStoppable(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	loops := StepLoopWatcher{
		StopTimeout: 500 * time.Millisecond,
		Signal:      func(int, syscall.Signal) error { return syscall.EPERM },
	}
	fw := FormatWatcher{Nebula: loops, SubagentMissing: "watch_lanes.sh not found", StopTimeout: 500 * time.Millisecond}

	host := exec.Command("bash", "-c", `while (( SECONDS < 600 )); do sleep 180; done`, "fake-step-loop", dir)
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if pids, err := (StepLoopWatcher{StopTimeout: 2 * time.Second}).Running(context.Background(), dir); err == nil && len(pids) > 0 {
			_ = (StepLoopWatcher{StopTimeout: 2 * time.Second}).Stop(context.Background(), pids)
		}
		_ = host.Process.Kill()
		_ = host.Wait()
	})
	var pids []int
	waitFor(t, func() bool { pids, _ = fw.Running(t.Context(), dir); return len(pids) == 2 })

	before := nsSnapshot(t, dir)
	label := &fakeLabeler{label: "claude"}
	s := nsService(root, fw, label)
	if _, err := s.Adopt(t.Context(), nsRun); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("adopt = %v, want EPERM refusal when step loop is not stoppable", err)
	}
	if after := nsSnapshot(t, dir); !reflect.DeepEqual(after, before) || label.label != "claude" {
		t.Fatalf("refused adopt changed state: label=%q\n got %v\nwant %v", label.label, after, before)
	}
	if afterPIDs, err := fw.Running(t.Context(), dir); err != nil || len(afterPIDs) != 2 {
		t.Fatalf("loop pids after refused adopt = %v, err = %v", afterPIDs, err)
	}
}

func TestAdoptNebulaSwarmFailureAtEachStepLeavesPriorState(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"import", "marker", "label", "watcher"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
			label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: true}
			s := nsService(root, loop, label)
			switch step {
			case "label":
				label.failSet = errInjected
			case "watcher":
				loop.failStop = errInjected
			default:
				s.fault = func(got string) error {
					if got == "adopt:"+step {
						return errInjected
					}
					return nil
				}
			}
			before := nsSnapshot(t, dir)
			if _, err := s.Adopt(t.Context(), nsRun); !errors.Is(err, errInjected) {
				t.Fatalf("adopt = %v, want injected failure", err)
			}
			if after := nsSnapshot(t, dir); !reflect.DeepEqual(after, before) || label.label != "claude" || !loop.running {
				t.Fatalf("failed adopt at %s changed state: label %q loop running %v\n got %v\nwant %v", step, label.label, loop.running, after, before)
			}
		})
	}
}

// The adopted controller writes lane state back in nebula-swarm's fields.
func TestNebulaSwarmCheckpointKeepsLedgerFormat(t *testing.T) {
	t.Parallel()
	dir := writeNebulaSwarmRun(t, t.TempDir(), nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	r, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	workflow, lanes, _, err := controllerState(r)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.MergePolicy.Mode != recipe.MergeModeManual {
		t.Fatalf("merge none imported as %q", workflow.MergePolicy.Mode)
	}
	for i := range lanes {
		if lanes[i].ID == "m2-default" {
			lanes[i].RecordAttempt("review", 1, "sess-m2", "term-m2-review")
			lanes[i].Status = "blocked"
		}
	}
	if err := checkpoint(dir, lanes); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m2 := got.Lanes[1]; m2.Phase != "review" || m2.Terminal != "term-m2-review" || m2.Session != "sess-m2" || m2.Status != "blocked" || m2.PR != "5705" {
		t.Fatalf("m2-default after checkpoint = %+v", m2)
	}
	if b := readText(filepath.Join(dir, "state.json")); strings.Contains(b, "nebula_session") || strings.Contains(b, `"sessions"`) {
		t.Fatalf("checkpoint wrote subagent-swarm fields into a nebula-swarm ledger:\n%s", b)
	}
	if DetectFormat(dir) != FormatNebulaSwarm {
		t.Fatal("checkpointed ledger no longer reads as nebula-swarm")
	}
}

type fakeOrchestrators map[string]string

func (f fakeOrchestrators) RunOrchestratorSession(_ context.Context, runID string) (string, error) {
	return f[runID], nil
}

// A run whose orchestrator session moved on to a newer run is superseded,
// even while its ledger still shows a running lane.
func TestRunWhoseOrchestratorMovedOnIsSuperseded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeNebulaSwarmRun(t, root, "20261113-slim-prompts", "sess-orch", time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC))
	writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	writeNebulaSwarmRun(t, root, "20261112-other", "sess-other", time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
	// A subagent-swarm-nebula run records no session; Nebula names it.
	sub := filepath.Join(root, ".shared", "subagent-swarm", "20261113-legacy")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sub, "state.json"), `{"config":{"goal":"legacy"},"tasks":[{"id":"x","status":"done","phase":"review","pr":5591}]}`)
	mt := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(sub, "state.json"), mt, mt); err != nil {
		t.Fatal(err)
	}

	runs, err := Importer{Roots: []string{root}, Orchestrators: fakeOrchestrators{"20261113-legacy": "sess-orch"}}.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][3]string{}
	for _, r := range runs {
		got[r.ID] = [3]string{r.State, r.SupersededBy, r.DrivenBy}
	}
	want := map[string][3]string{
		"20261112-other":        {RunActive, "", "sess-other"},
		"20261113-legacy":       {RunSuperseded, nsRun, "sess-orch"},
		"20261113-slim-prompts": {RunSuperseded, nsRun, "sess-orch"},
		nsRun:                   {RunActive, "", "sess-orch"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("run states:\n got %v\nwant %v", got, want)
	}
}

func TestStepLoopProcFilterAndRestartPaths(t *testing.T) {
	procRoot := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	runDir := filepath.Join(home, ".swarm", "20261114-trial-m1")

	writeFakeProc := func(pid, ppid int, state string, args, env []string) {
		pdir := filepath.Join(procRoot, fmt.Sprint(pid))
		if err := os.MkdirAll(pdir, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(pdir, "cmdline"), strings.Join(args, "\x00")+"\x00")
		write(t, filepath.Join(pdir, "stat"), fmt.Sprintf("%d (proc) %s %d 1 1 0\n", pid, state, ppid))
		if len(env) > 0 {
			write(t, filepath.Join(pdir, "environ"), strings.Join(env, "\x00")+"\x00")
		}
	}

	// PID 100: bash shell loop referencing ~/.swarm/20261114-trial-m1 via env.
	writeFakeProc(100, 1, "S", []string{"bash"}, []string{
		"RUN=~/.swarm/20261114-trial-m1",
		"BASH_FUNC_sw_step%%=() { :; }",
		"__CLAUDE_CC_COMMAND_TEXT=while true; do sw_step \"$RUN\"; sleep 180; done",
	})
	// PID 101: sleep child of PID 100.
	writeFakeProc(101, 100, "S", []string{"sleep", "180"}, nil)
	// PID 200: unrelated process mentioning the run dir in cmdline (must not be picked).
	writeFakeProc(200, 1, "S", []string{"tail", "-f", runDir + "/EVENTS"}, nil)
	// PID 201: unrelated process mentioning the run dir in environ (must not be picked).
	writeFakeProc(201, 1, "S", []string{"python3", "worker.py"}, []string{"RUN=" + runDir})
	// PID 300: zombie loop process (must be ignored by procTable).
	writeFakeProc(300, 1, "Z", []string{"bash", "-c", "sw_step " + runDir}, nil)

	w := StepLoopWatcher{ProcRoot: procRoot}
	pids, err := w.Running(t.Context(), runDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pids, []int{100, 101}) {
		t.Fatalf("Running pids = %v, want [100 101] (excluding unrelated 200, 201 and zombie 300)", pids)
	}

	stopped := w.Inspect(pids)
	if stopped == nil || !stopped.IsStepLoop || !strings.Contains(stopped.Command, "sw_step") {
		t.Fatalf("Inspect = %+v", stopped)
	}

	// Home-relative candidates (~, $HOME, ${HOME}) and env mentions.
	cands := runDirCandidates(runDir)
	for _, want := range []string{"~/.swarm/20261114-trial-m1", "$HOME/.swarm/20261114-trial-m1", "${HOME}/.swarm/20261114-trial-m1"} {
		if !reflect.DeepEqual(mentionsDir([]string{"RUN=" + want}, want), true) || !strings.Contains(strings.Join(cands, "|"), want) {
			t.Fatalf("runDirCandidates(%q) = %v, missing %q", runDir, cands, want)
		}
	}
	if mentionsDir([]string{"PWD=" + runDir, "OLDPWD=" + runDir, "ORCA_SWARM_ROOTS=" + runDir}, runDir) {
		t.Fatal("mentionsDir should ignore PWD, OLDPWD, and ORCA_ entries")
	}

	// Restart error when custom command cannot be started reports the manual restart command.
	custom := &StoppedLoop{
		Command: "custom-step-loop --run " + runDir,
		Args:    []string{"/nonexistent-binary-for-test", "--run", runDir},
	}
	if err := w.Restart(t.Context(), t.TempDir(), custom); err == nil || !strings.Contains(err.Error(), "restart manually: custom-step-loop --run "+runDir) {
		t.Fatalf("StepLoopWatcher.Restart error = %v, want manual restart hint", err)
	}
	fw := FormatWatcher{NebulaMissing: "swarm_nebula.sh missing"}
	if err := fw.Restart(t.Context(), t.TempDir(), custom); err == nil || !strings.Contains(err.Error(), "restart manually: custom-step-loop --run "+runDir) {
		t.Fatalf("FormatWatcher.Restart error = %v, want manual restart hint", err)
	}
}

const fakeClaudeStepEntryScript = `#!/usr/bin/env bash
set -euo pipefail
run_dir="${1:?run_dir}"
step_marker="${2:?step_marker}"
check_orca_lease() {
  local dir="$1"
  local lease="$dir/.orca-lease.json"
  if [[ -f "$lease" ]]; then
    local holder
    holder="$(python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
state = str(d.get("state") or "").strip()
holder = str(d.get("holder") or "").strip()
if state != "releasing" and holder.startswith("orca-"):
    print(holder)
' "$lease")"
    if [[ -n "$holder" ]]; then
      echo "refusing to step: run held by orca ($holder)" >&2
      return 1
    fi
  fi
  for f in "$dir/OBJECTIVE.md" "$dir/PROGRESS.md"; do
    [[ -f "$f" ]] || continue
    local first
    IFS= read -r first < "$f" || true
    if [[ "$first" == "ORCHESTRATOR: orca since "* ]]; then
      echo "refusing to step: $first" >&2
      return 1
    fi
  done
  return 0
}
check_orca_lease "$run_dir"
echo "stepped" >> "$step_marker"
`

func TestFakeClaudeStepEntryPointRefusesWhileOrcaLeaseLive(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	script := filepath.Join(t.TempDir(), "swarm_nebula.sh")
	write(t, script, fakeSwarmScript)
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	stepScript := filepath.Join(t.TempDir(), "fake_step.sh")
	write(t, stepScript, fakeClaudeStepEntryScript)
	if err := os.Chmod(stepScript, 0o755); err != nil {
		t.Fatal(err)
	}
	stepMarker := filepath.Join(t.TempDir(), "steps.log")

	if holder, held := HeldByOrca(dir); held {
		t.Fatalf("HeldByOrca before adopt = (%q, true), want false", holder)
	}
	if out, err := exec.Command(stepScript, dir, stepMarker).CombinedOutput(); err != nil {
		t.Fatalf("step before adopt failed: %v: %s", err, out)
	}
	if got := strings.TrimSpace(readText(stepMarker)); got != "stepped" {
		t.Fatalf("step marker before adopt = %q, want stepped", got)
	}

	label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
	s := nsService(root, loop, label)
	if _, err := s.Adopt(t.Context(), nsRun); err != nil {
		t.Fatal(err)
	}
	if holder, held := HeldByOrca(dir); !held || holder != s.Holder {
		t.Fatalf("HeldByOrca during adopt = (%q, %v), want (%q, true)", holder, held, s.Holder)
	}

	// Production StepLoopWatcher.Start and ScriptWatcher.Start also refuse via HeldByOrca.
	if err := (StepLoopWatcher{Script: script}).Start(t.Context(), dir); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("StepLoopWatcher.Start while held = %v, want ErrLeaseHeld", err)
	}
	if err := (ScriptWatcher{Script: script}).Start(t.Context(), dir); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("ScriptWatcher.Start while held = %v, want ErrLeaseHeld", err)
	}

	// While orca holds the lease, the Claude step entry point refuses.
	out, err := exec.Command(stepScript, dir, stepMarker).CombinedOutput()
	if err == nil {
		t.Fatalf("expected fake Claude step entry point to refuse under live orca lease, got output: %s", out)
	}
	if !strings.Contains(string(out), "refusing to step") || !strings.Contains(string(out), s.Holder) {
		t.Fatalf("refusal output = %q, want holder %q", string(out), s.Holder)
	}
	if got := strings.TrimSpace(readText(stepMarker)); got != "stepped" {
		t.Fatalf("step marker mutated while held by orca: %q", got)
	}

	// Also verify refusal when only the stand-down line is present on line 1,
	// and that a stand-down string buried on line 2 is ignored.
	standDownDir := filepath.Join(t.TempDir(), "stand-down-only")
	if err := os.MkdirAll(standDownDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(standDownDir, "OBJECTIVE.md"), "ORCHESTRATOR: orca since 2026-11-14T05:40:00Z (orca-chat:front-desk) — stand down; do not dispatch or merge.\n# Goal\n")
	if holder, held := HeldByOrca(standDownDir); !held || holder != "orca-chat:front-desk" {
		t.Fatalf("HeldByOrca(standDownDir) = (%q, %v), want (orca-chat:front-desk, true)", holder, held)
	}
	write(t, filepath.Join(standDownDir, "OBJECTIVE.md"), "# Goal\nORCHESTRATOR: orca since 2026-11-14T05:40:00Z (orca-chat:front-desk)\n")
	if holder, held := HeldByOrca(standDownDir); held {
		t.Fatalf("HeldByOrca(buried marker) = (%q, true), want false", holder)
	}

	if err := s.Release(t.Context(), nsRun); err != nil {
		t.Fatal(err)
	}
	if holder, held := HeldByOrca(dir); held {
		t.Fatalf("HeldByOrca after release = (%q, true), want false", holder)
	}
	if out, err := exec.Command(stepScript, dir, stepMarker).CombinedOutput(); err != nil {
		t.Fatalf("step after release failed: %v: %s", err, out)
	}
	if got := strings.Count(readText(stepMarker), "stepped"); got != 2 {
		t.Fatalf("step count after release = %d, want 2", got)
	}
}

type restartingWatcher struct {
	fakeWatcher
	restartAfter time.Duration
}

func (w *restartingWatcher) Stop(ctx context.Context, pids []int) error {
	if err := w.fakeWatcher.Stop(ctx, pids); err != nil {
		return err
	}
	time.AfterFunc(w.restartAfter, func() {
		w.mu.Lock()
		w.running = true
		w.mu.Unlock()
	})
	return nil
}

func TestAdoptFailsWhenStepLoopRestartsWithinInterval(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	before := nsSnapshot(t, dir)
	label := &fakeLabeler{label: "claude"}
	loop := &restartingWatcher{
		fakeWatcher:  fakeWatcher{running: true},
		restartAfter: 15 * time.Millisecond,
	}
	s := nsService(root, loop, label)
	s.LoopVerifyInterval = 60 * time.Millisecond

	_, err := s.Adopt(t.Context(), nsRun)
	if err == nil || !strings.Contains(err.Error(), "step loop restarted under orca lease") {
		t.Fatalf("Adopt error = %v, want step loop restarted under orca lease", err)
	}
	if after := nsSnapshot(t, dir); !reflect.DeepEqual(after, before) || label.label != "claude" {
		t.Fatalf("failed adopt after loop restart left dirty state: label=%q\n got %v\nwant %v", label.label, after, before)
	}
}

func TestNebulaSwarmReviewReportRecognised(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	write(t, filepath.Join(dir, "m2-default.review.prompt"), "Run /pr-polish-nebula on PR #5705.\n")

	clk := clock.NewFakeClock(time.Date(2026, 11, 14, 6, 36, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := lane.NewFakeDriver()

	controllers := NewControllers(
		t.Context(),
		st,
		drv,
		clk,
		func(string, func(context.Context, string, string, string) error) controller.Judge {
			return controller.NewFakeJudge()
		},
	)
	label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
	s := nsService(root, loop, label)
	s.Driver = controllers
	r, err := s.Adopt(t.Context(), nsRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

	// Complete clean phase via a nebula-swarm disk report (.done file) and turn_ended_no_report
	// so the controller advances m2-default to review round 1.
	write(t, filepath.Join(dir, "reports", "m2-default.clean.done"), "Outcome: done\nHEAD: bbb\nPR: 5705\n")
	drv.Emit(lane.Event{
		RunID:      nsRun,
		LaneID:     "m2-default",
		TerminalID: "term-m2-clean",
		Kind:       lane.EventTurnEndedNoReport,
		Text:       "clean finished",
	})

	waitFor(t, func() bool {
		got, err := st.Lanes().Get(t.Context(), nsRun, "m2-default")
		return err == nil && got.Phase == recipe.PhaseReview && got.Status == lane.StatusRunning
	})
	if len(drv.PhaseStarts) != 1 {
		t.Fatalf("PhaseStarts = %+v, want 1 review start", drv.PhaseStarts)
	}
	reviewStart := drv.PhaseStarts[0]
	for _, want := range []string{
		"Run /pr-polish-nebula on PR #5705.",
		"reports/m2-default.review.r1.md",
		"nebula phase report --outcome",
		"Verdict: pass | needs-changes",
	} {
		if !strings.Contains(reviewStart.Brief, want) {
			t.Fatalf("nebula-swarm review brief missing %q:\n%s", want, reviewStart.Brief)
		}
	}

	gotReviewLane, err := st.Lanes().Get(t.Context(), nsRun, "m2-default")
	if err != nil {
		t.Fatal(err)
	}
	reviewTermID := gotReviewLane.TerminalID

	// Simulate the review terminal (codex / pr-polish) writing the run's report file
	// with Verdict: pass and ending its turn without filing a Nebula API phase report
	// (with turn summary text in Event.Text, which maps to PhaseExitPayload.Error).
	write(t, filepath.Join(dir, "reports", "m2-default.review.r1.md"), "- Lane: m2-default\n- Phase: review\n- Round: 1\n- PR: #5705\n- Head-SHA: fb6c5bae\n- Verdict: pass\n- Rework-class: none\n")
	drv.Emit(lane.Event{
		RunID:      nsRun,
		LaneID:     "m2-default",
		TerminalID: reviewTermID,
		Kind:       lane.EventTurnEndedNoReport,
		Text:       "Polished PR #5705 and pushed fb6c5bae.",
	})

	waitFor(t, func() bool {
		got, err := st.Lanes().Get(t.Context(), nsRun, "m2-default")
		return err == nil && got.Status == lane.StatusDone
	})
	gotLane, err := st.Lanes().Get(t.Context(), nsRun, "m2-default")
	if err != nil {
		t.Fatal(err)
	}
	if gotLane.ApprovalSHA != "fb6c5bae" || gotLane.PR != 5705 {
		t.Fatalf("m2-default after review pass = %+v, want ApprovalSHA=fb6c5bae PR=5705", gotLane)
	}
	if len(drv.PhaseStarts) != 1 {
		t.Fatalf("unexpected retry/spawn after review pass: PhaseStarts=%+v", drv.PhaseStarts)
	}

	// Another Controllers instance on the same store must fail with store.ErrLeaseHeld
	// while the first controller holds the non-expired run lease.
	competing := NewControllers(t.Context(), st, drv, clk, nil)
	if err := competing.Start(t.Context(), r); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("competing Controllers.Start = %v, want store.ErrLeaseHeld", err)
	}
}
