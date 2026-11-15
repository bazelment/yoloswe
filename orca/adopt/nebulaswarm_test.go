package adopt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
	"github.com/bazelment/yoloswe/orca/mailbox"
	mailboxnebula "github.com/bazelment/yoloswe/orca/mailbox/nebula"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/planner"
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
{"id":"m2-default","title":"default prompts","branch":"swarm-lane/` + id + `/m2-default","priority":"p1","status":"running","phase":"clean","round":1,"session_id":"sess-m2","terminals":{"swe":{"1":"term-m2-swe"},"clean":{"1":"term-m2-clean"}},"pr":5705,"pr_head":"bbb","merge_sha":"","terminal_state":"working","verdict":""}]}
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
	// When the journal is in stateReleasing, HeldByOrca is authoritative and
	// returns false even while the stand-down marker is still on line 1.
	write(t, filepath.Join(standDownDir, "OBJECTIVE.md"), "ORCHESTRATOR: orca since 2026-11-14T05:40:00Z (orca-chat:front-desk) — stand down; do not dispatch or merge.\n# Goal\n")
	write(t, filepath.Join(standDownDir, ".orca-lease.json"), `{"holder":"orca-chat:front-desk","state":"releasing"}`)
	if holder, held := HeldByOrca(standDownDir); held {
		t.Fatalf("HeldByOrca(releasing journal with marker) = (%q, true), want false", holder)
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

	// Another chat's Controllers instance on the same store must fail with
	// store.ErrLeaseHeld while the first chat holds the non-expired run lease.
	competing := NewControllers(t.Context(), st, drv, clk, nil)
	rOther := r
	rOther.Holder = "orca-chat:other-chat"
	if err := competing.Start(t.Context(), rOther); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("competing Controllers.Start = %v, want store.ErrLeaseHeld", err)
	}
}

func TestNebulaSwarmLiveWriteBackOnMergeAndTransition(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"fallback_checkpoint", "ledger_py_helper"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), ".swarm")
			dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
			// Enable merge authority on the run so review -> integrate -> merged runs under orca.
			statePath := filepath.Join(dir, "state.json")
			write(t, statePath, strings.Replace(readText(statePath), `"merge":"none"`, `"merge":"squash"`, 1))

			var helperLogPath string
			var scriptOverride string
			if mode == "ledger_py_helper" {
				helperLogPath = filepath.Join(t.TempDir(), "ledger-calls.log")
				scriptOverride = filepath.Join(t.TempDir(), "fake_ledger.py")
				write(t, scriptOverride, fmt.Sprintf(`#!/usr/bin/env python3
import json, pathlib, sys
log_path = pathlib.Path(%q)
with log_path.open("a") as f:
    f.write(" ".join(sys.argv[1:]) + "\n")
cmd = sys.argv[1]
run_dir = pathlib.Path(sys.argv[2])
state_file = run_dir / "state.json"
doc = json.loads(state_file.read_text())
args = sys.argv[3:]
kv = {}
i = 0
while i < len(args):
    kv[args[i]] = args[i+1]
    i += 2
lane_id = kv["--id"]
if cmd == "add":
    doc.setdefault("tasks", []).append({"id": lane_id, "title": kv.get("--title", lane_id), "branch": kv.get("--branch", "")})
elif cmd == "set":
    for t in doc.get("tasks", []):
        if t.get("id") != lane_id:
            continue
        if "--status" in kv:
            t["status"] = kv["--status"]
        if "--phase" in kv:
            t["phase"] = kv["--phase"]
        if "--round" in kv:
            t["round"] = int(kv["--round"])
        if "--pr" in kv:
            t["pr"] = int(kv["--pr"])
        if "--pr-head" in kv:
            t["pr_head"] = kv["--pr-head"]
        if "--approval-sha" in kv:
            t["approval_sha"] = kv["--approval-sha"]
        if "--verdict" in kv:
            t["verdict"] = kv["--verdict"]
        if "--merge-sha" in kv:
            t["merge_sha"] = kv["--merge-sha"]
        if "--terminal-state" in kv:
            t["terminal_state"] = kv["--terminal-state"]
        if "--terminal-id" in kv and t.get("phase"):
            terms = t.setdefault("terminals", {})
            terms.setdefault(t["phase"], {})[str(t.get("round", 1))] = kv["--terminal-id"]
state_file.write_text(json.dumps(doc, indent=2) + "\n")
`, helperLogPath))
			}

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
			controllers.ledgerScript = &scriptOverride
			var prMerged atomic.Bool
			controllers.PRProbe = func(_ context.Context, _, _ string, pr int) (controller.PRStatus, error) {
				if pr == 5705 && prMerged.Load() {
					return controller.PRStatus{
						State:    "MERGED",
						MergeSHA: "999aaaa",
						Number:   5705,
					}, nil
				}
				return controller.PRStatus{State: "OPEN", Number: pr}, nil
			}

			label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
			s := nsService(root, loop, label)
			s.Driver = controllers
			r, err := s.Adopt(t.Context(), nsRun)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

			// Advance clean -> review while orca holds the run.
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

			// Without releasing the run, state.json and EVENTS must already reflect the transition to review.
			midRun, err := Read(dir)
			if err != nil {
				t.Fatal(err)
			}
			if m2 := midRun.Lanes[1]; m2.Phase != "review" || m2.Status != "running" {
				t.Fatalf("live state.json after clean->review = %+v, want phase=review status=running", m2)
			}
			if events := readText(filepath.Join(dir, "EVENTS")); !strings.Contains(events, "\tm2-default\tphase\tclean -> review\n") {
				t.Fatalf("EVENTS missing clean -> review transition:\n%s", events)
			}

			gotReview, err := st.Lanes().Get(t.Context(), nsRun, "m2-default")
			if err != nil {
				t.Fatal(err)
			}
			// Finish review with pass, then let PRProbe reconcile the merged PR (#5705 -> 999aaaa).
			prMerged.Store(true)
			write(t, filepath.Join(dir, "reports", "m2-default.review.r1.md"), "Outcome: done\nHEAD: fb6c5bae\nPR: 5705\nVerdict: pass\n")
			drv.Emit(lane.Event{
				RunID:      nsRun,
				LaneID:     "m2-default",
				TerminalID: gotReview.TerminalID,
				Kind:       lane.EventTurnEndedNoReport,
				Text:       "review passed and PR merged",
			})

			waitFor(t, func() bool {
				got, err := st.Lanes().Get(t.Context(), nsRun, "m2-default")
				return err == nil && got.Status == lane.StatusDone && got.MergeSHA == "999aaaa"
			})

			// While orca still holds the run (before /release), state.json and EVENTS must reflect the merge.
			rawState := readText(statePath)
			if !strings.Contains(rawState, `"merge_sha": "999aaaa"`) && !strings.Contains(rawState, `"merge_sha":"999aaaa"`) {
				t.Fatalf("state.json missing merge_sha 999aaaa while held:\n%s", rawState)
			}
			afterMerge, err := Read(dir)
			if err != nil {
				t.Fatal(err)
			}
			if m2 := afterMerge.Lanes[1]; m2.Status != "done" || m2.Phase != "review" {
				t.Fatalf("m2-default after merge = %+v, want status=done phase=review", m2)
			}
			events := readText(filepath.Join(dir, "EVENTS"))
			if !strings.Contains(events, "\tm2-default\tmerged\t#5705 999aaaa\n") {
				t.Fatalf("EVENTS missing merged line for m2-default:\n%s", events)
			}
			if mode == "ledger_py_helper" {
				calls := readText(helperLogPath)
				if !strings.Contains(calls, "--merge-sha 999aaaa") {
					t.Fatalf("expected ledger.py helper to be invoked with --merge-sha 999aaaa, got:\n%s", calls)
				}
			}
		})
	}
}

func TestAdoptedRunObjectiveNextWorkTriggersPlannerAndRefreshes(t *testing.T) {
	t.Parallel()

	t.Run("creates_backlog_item_and_refreshes_objective", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), ".swarm")
		last := time.Date(2026, 11, 14, 18, 52, 0, 0, time.UTC)
		dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", last)

		// All imported lanes are already done (empty/drained open backlog), while OBJECTIVE.md has Next: work.
		write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"M1 trial of slim-prompt trial mode ... 2 real code lanes","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"sess-orch","merge":"squash",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high"},{"name":"review","kind":"claude","model":"sonnet","effort":"high"}]},
"tasks":[
{"id":"m2-gaps2","title":"m2 gaps2","branch":"swarm-lane/20261114-trial-m1/m2-gaps2","priority":"p1","status":"done","phase":"review","round":1,"session_id":"sess-m2","terminals":{"review":{"1":"term-m2-review"}},"pr":5786,"pr_head":"bbb","approval_sha":"bbb","merge_sha":"999aaaa","terminal_state":"","verdict":"pass"}]}
`)
		write(t, filepath.Join(dir, "PLAN.md"), "# Slim Prompt Trial Plan\n\n- M1: slim prompt trial\n- M2: hand interventions 6-7\n- M4: lane_metrics day vs baseline\n")
		objectiveBody := "# 20261114-trial-m1\n\n" +
			"Goal: Validate slim-prompt trial mode through M4 and eliminate hand interventions.\n" +
			"Plan: PLAN.md\n\n" +
			"Milestones:\n" +
			"- [x] M1: slim-prompt trial (PR #5604, #5786 merged)\n" +
			"- [ ] 6. Fix adopt write-back to state.json and EVENTS\n" +
			"- [ ] 7. Fix adopt planner handoff on drained backlog\n" +
			"- [ ] M4: lane_metrics day vs baseline\n\n" +
			"Next: when #5786 merges, staff a lane for 6-7 if not covered; then M4 (lane_metrics day vs baseline), update PLAN.md log and the Nebula post\n"
		write(t, filepath.Join(dir, "OBJECTIVE.md"), objectiveBody)

		clk := clock.NewFakeClock(last)
		st := store.NewMemoryStore(clk)
		t.Cleanup(func() { _ = st.Close() })
		drv := lane.NewFakeDriver()

		recArgs, err := json.Marshal(planner.Reconciliation{
			Summary:   "Added m2-items-6-7 from OBJECTIVE.md Next: work.",
			Objective: "Shortened multi-line summary\nLine 2 that must not overwrite adopted OBJECTIVE.md",
			BacklogDiff: planner.BacklogDiff{
				Add: []planner.BacklogAddItem{
					{
						ID:          "m2-items-6-7",
						Title:       "Cover hand interventions 6-7 from OBJECTIVE.md",
						Description: "Staff a lane for items 6-7 after #5786 merged, before M4.",
						Priority:    "p1",
						Order:       1,
					},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		readPlanArgs, err := json.Marshal(map[string]string{
			"plan_ref": filepath.Join(dir, "PLAN.md"),
		})
		if err != nil {
			t.Fatal(err)
		}
		fakeClient := model.NewFakeClient(
			model.Response{
				ID:         "msg_read_plan",
				Model:      planner.DefaultModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("tu_read_plan", planner.ToolReadPlan, readPlanArgs),
				),
			},
			model.Response{
				ID:         "msg_plan_next",
				Model:      planner.DefaultModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("tu_plan_1", planner.ToolSubmitReconciliation, recArgs),
				),
			},
			model.Response{
				ID:         "msg_plan_next_done",
				Model:      planner.DefaultModel,
				StopReason: model.StopReasonEndTurn,
				Message:    model.NewAssistantMessage(model.NewTextBlock("Added m2-items-6-7 to backlog.")),
			},
		)
		pln := planner.New(planner.Config{}, st, drv, clk, fakeClient)

		controllers := NewControllers(t.Context(), st, drv, clk, nil)
		controllers.Planner = pln.OnPlannerRequest
		label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
		s := nsService(root, loop, label)
		s.Driver = controllers

		r, err := s.Adopt(t.Context(), nsRun)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

		// 1. Verify the full objective (including milestones, plan path, and Next:) was imported into the store.
		storedRun, err := st.Runs().Get(t.Context(), nsRun)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"Validate slim-prompt trial mode through M4",
			"6. Fix adopt write-back to state.json and EVENTS",
			"7. Fix adopt planner handoff on drained backlog",
			"Next: when #5786 merges, staff a lane for 6-7 if not covered",
			"PLAN.md",
		} {
			if !strings.Contains(storedRun.Objective, want) {
				t.Fatalf("storedRun.Objective missing %q:\n%s", want, storedRun.Objective)
			}
		}

		// 2. Verify the planner received the full objective, read PLAN.md via read.plan, and created the backlog item.
		if len(fakeClient.Requests) < 2 {
			t.Fatalf("expected >= 2 planner model calls, got %d", len(fakeClient.Requests))
		}
		plannerPrompt := fakeClient.Requests[0].Messages[0].Text()
		for _, want := range []string{
			"when #5786 merges, staff a lane for 6-7 if not covered",
			"6. Fix adopt write-back to state.json and EVENTS",
			"PLAN.md",
		} {
			if !strings.Contains(plannerPrompt, want) {
				t.Fatalf("planner prompt missing %q:\n%s", want, plannerPrompt)
			}
		}
		readPlanResult := fakeClient.Requests[1].Messages[2].Content[0]
		if readPlanResult.IsError || !strings.Contains(readPlanResult.Content, "Slim Prompt Trial Plan") {
			t.Fatalf("expected read.plan on resolved PLAN.md to succeed, got is_error=%v content=%q", readPlanResult.IsError, readPlanResult.Content)
		}
		if strings.Contains(storedRun.Objective, "Shortened multi-line summary") {
			t.Fatalf("expected adopted run Objective to be preserved against multi-line planner replacement, got:\n%s", storedRun.Objective)
		}
		item, err := st.Backlog().Get(t.Context(), nsRun, "m2-items-6-7")
		if err != nil || item.Status != store.BacklogOpen {
			t.Fatalf("expected open backlog item m2-items-6-7 created by planner, got %+v, err=%v", item, err)
		}

		// 3. Modify OBJECTIVE.md on disk and verify the stored objective refreshes.
		updatedObj := strings.Replace(objectiveBody, "M4: lane_metrics day vs baseline", "M4: lane_metrics day vs baseline (with p95 latency)", 1)
		write(t, filepath.Join(dir, "OBJECTIVE.md"), updatedObj)
		if _, err := controllers.RefreshObjective(t.Context(), nsRun); err != nil {
			t.Fatal(err)
		}
		refreshedRun, err := st.Runs().Get(t.Context(), nsRun)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(refreshedRun.Objective, "with p95 latency") {
			t.Fatalf("refreshedRun.Objective did not pick up OBJECTIVE.md update:\n%s", refreshedRun.Objective)
		}
	})

	t.Run("asks_owner_question_when_drained_after_merge", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), ".swarm")
		last := time.Date(2026, 11, 14, 18, 52, 0, 0, time.UTC)
		dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", last)
		write(t, filepath.Join(dir, "OBJECTIVE.md"),
			"# 20261114-trial-m1\n\n"+
				"Goal: Finish M2 and decide whether to run M4 on staging or prod.\n"+
				"Next: when #5705 merges, ask owner whether M4 should run against staging or prod.\n")

		clk := clock.NewFakeClock(last)
		st := store.NewMemoryStore(clk)
		t.Cleanup(func() { _ = st.Close() })
		drv := lane.NewFakeDriver()

		recArgs, err := json.Marshal(planner.Reconciliation{
			Summary: "Needs owner decision for M4 target environment.",
			Conflicts: []planner.ConflictPoint{
				{
					ID:          "ask-m4-env",
					Description: "Next step in OBJECTIVE.md requires choosing M4 target environment.",
					Question:    "Should M4 lane_metrics run against staging or prod?",
					Options:     []string{"staging", "prod"},
					Default:     "staging",
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		fakeClient := model.NewFakeClient(
			model.Response{
				ID:         "msg_plan_ask",
				Model:      planner.DefaultModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("tu_plan_ask", planner.ToolSubmitReconciliation, recArgs),
				),
			},
			model.Response{
				ID:         "msg_plan_ask_done",
				Model:      planner.DefaultModel,
				StopReason: model.StopReasonEndTurn,
				Message:    model.NewAssistantMessage(model.NewTextBlock("Asked owner about staging vs prod.")),
			},
		)
		pln := planner.New(planner.Config{}, st, drv, clk, fakeClient)

		var prMerged atomic.Bool
		controllers := NewControllers(t.Context(), st, drv, clk, nil)
		controllers.Planner = pln.OnPlannerRequest
		controllers.PRProbe = func(_ context.Context, _, _ string, pr int) (controller.PRStatus, error) {
			if pr == 5705 && prMerged.Load() {
				return controller.PRStatus{State: "MERGED", MergeSHA: "999aaaa", Number: 5705}, nil
			}
			return controller.PRStatus{State: "OPEN", Number: pr}, nil
		}
		label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
		s := nsService(root, loop, label)
		s.Driver = controllers

		r, err := s.Adopt(t.Context(), nsRun)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

		// While m2-default is still running, the planner has not been called yet.
		if len(fakeClient.Requests) != 0 {
			t.Fatalf("expected 0 planner calls while m2-default is still running, got %d", len(fakeClient.Requests))
		}

		// Merge #5705 so m2-default finishes and the backlog drains.
		prMerged.Store(true)
		drv.Emit(lane.Event{
			RunID:      nsRun,
			LaneID:     "m2-default",
			TerminalID: "term-m2-clean",
			Kind:       lane.EventTurnEndedNoReport,
			Text:       "PR #5705 merged",
		})

		waitFor(t, func() bool {
			dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: nsRun, ActiveOnly: true})
			if err != nil {
				return false
			}
			for i := range dirs {
				if strings.HasPrefix(dirs[i].Ref, "owner_ask:") && strings.Contains(dirs[i].Text, "staging or prod") {
					return true
				}
			}
			return false
		})
	})

	t.Run("planner_error_does_not_retry_until_objective_changes", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), ".swarm")
		last := time.Date(2026, 11, 14, 18, 52, 0, 0, time.UTC)
		dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", last)
		write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"M1 trial","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"sess-orch","merge":"squash",
"phases":[{"name":"swe","kind":"claude"},{"name":"review","kind":"claude"}]},
"tasks":[{"id":"m2-gaps2","title":"m2 gaps2","branch":"swarm-lane/20261114-trial-m1/m2-gaps2","priority":"p1","status":"done","phase":"review","round":1,"session_id":"sess-m2","terminals":{"review":{"1":"term-m2-review"}},"pr":5786,"pr_head":"bbb","approval_sha":"bbb","merge_sha":"999aaaa","terminal_state":"","verdict":"pass"}]}`)
		write(t, filepath.Join(dir, "OBJECTIVE.md"), "# 20261114-trial-m1\n\nGoal: Complete M1-M4.\nNext: staff items 6-7.\n")

		clk := clock.NewFakeClock(last)
		st := store.NewMemoryStore(clk)
		t.Cleanup(func() { _ = st.Close() })
		drv := lane.NewFakeDriver()

		var plannerCalls atomic.Int32
		controllers := NewControllers(t.Context(), st, drv, clk, nil)
		controllers.Planner = func(_ context.Context, _, _, _ string) error {
			plannerCalls.Add(1)
			return errors.New("gateway 502")
		}
		label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
		s := nsService(root, loop, label)
		s.Driver = controllers
		r, err := s.Adopt(t.Context(), nsRun)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

		if got := plannerCalls.Load(); got != 1 {
			t.Fatalf("plannerCalls on adopt = %d, want 1", got)
		}
		// Repeated planNextWorkIfDrained calls with unchanged OBJECTIVE.md must not re-trigger the failing planner.
		if _, err := controllers.planNextWorkIfDrained(t.Context(), nsRun, "backlog_drained"); err != nil {
			t.Fatal(err)
		}
		if got := plannerCalls.Load(); got != 1 {
			t.Fatalf("plannerCalls after unchanged planNextWorkIfDrained = %d, want 1 (no unbounded retry)", got)
		}
		// Updating OBJECTIVE.md changes the planKey and allows a fresh planner invocation.
		write(t, filepath.Join(dir, "OBJECTIVE.md"), "# 20261114-trial-m1\n\nGoal: Complete M1-M4.\nNext: staff items 6-7 and M4.\n")
		_, _ = controllers.planNextWorkIfDrained(t.Context(), nsRun, "backlog_drained")
		if got := plannerCalls.Load(); got != 2 {
			t.Fatalf("plannerCalls after OBJECTIVE.md change = %d, want 2", got)
		}
	})
}

func TestAdoptedRunPlannerHeartbeatsLeaseWhileRunning(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".swarm")
	start := time.Date(2026, 11, 14, 18, 52, 0, 0, time.UTC)
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", start)
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"M1 trial","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"sess-orch","merge":"squash",
"phases":[{"name":"swe","kind":"claude"},{"name":"review","kind":"claude"}]},
"tasks":[{"id":"m2-gaps2","title":"m2 gaps2","branch":"swarm-lane/20261114-trial-m1/m2-gaps2","priority":"p1","status":"done","phase":"review","round":1,"session_id":"sess-m2","terminals":{"review":{"1":"term-m2-review"}},"pr":5786,"pr_head":"bbb","approval_sha":"bbb","merge_sha":"999aaaa","terminal_state":"","verdict":"pass"}]}`)
	write(t, filepath.Join(dir, "OBJECTIVE.md"), "# 20261114-trial-m1\n\nGoal: Complete M1-M4.\nNext: when #5786 merges, staff a lane for items 6-7.\n")

	clk := clock.NewFakeClock(start)
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := lane.NewFakeDriver()

	controllers := NewControllers(t.Context(), st, drv, clk, nil)
	var plannerCalled atomic.Bool
	controllers.Planner = func(ctx context.Context, runID, _, _ string) error {
		plannerCalled.Store(true)
		// Advance the clock past DefaultLeaseTTL in heartbeat steps while inside the blocking Planner call.
		for range 4 {
			clk.Advance(controller.DefaultHeartbeatInterval)
			wantExpiry := clk.Now().Add(controller.DefaultLeaseTTL)
			// Wait for the keepLease goroutine to process the ticker fire and extend ExpiresAt.
			waitFor(t, func() bool {
				got, err := st.Runs().Get(ctx, runID)
				return err == nil && got.Lease != nil && got.Lease.ExpiresAt.Equal(wantExpiry)
			})
		}
		// While the planner is still running after > DefaultLeaseTTL, a competing holder must be rejected.
		if _, err := st.Runs().AcquireLease(ctx, runID, "competing-holder", controller.DefaultLeaseTTL); !errors.Is(err, store.ErrLeaseHeld) {
			t.Errorf("competing AcquireLease during slow planner call = %v, want ErrLeaseHeld", err)
		}
		return nil
	}

	label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
	s := nsService(root, loop, label)
	s.Driver = controllers
	r, err := s.Adopt(t.Context(), nsRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })
	if !plannerCalled.Load() {
		t.Fatal("expected Planner to be called on adopt of drained run with Next: work")
	}
}

func TestAdoptObjectiveAndCheckpointHelpers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	planFile := filepath.Join(dir, "docs", "my-plan.md")
	if err := os.MkdirAll(filepath.Dir(planFile), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, planFile, "# Plan\n")

	// 1. OBJECTIVE.md > 8,000 bytes is preserved in full without truncation.
	padding := strings.Repeat("Detailed milestone context line.\n", 300) // > 9,600 bytes
	longObj := "# Goal\n" + padding + "Plan: docs/my-plan.md\nNext: staff m4-metrics after m2 merges\n"
	write(t, filepath.Join(dir, "OBJECTIVE.md"), longObj)
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"short config goal","phases":[{"name":"swe"},{"name":"review"}]},"tasks":[]}`)

	r, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Objective) <= 8000 {
		t.Fatalf("expected unclipped Objective > 8000 bytes, got %d bytes", len(r.Objective))
	}
	if r.PlanPath != planFile {
		t.Fatalf("PlanPath = %q, want %q", r.PlanPath, planFile)
	}
	if r.Next != "staff m4-metrics after m2 merges" {
		t.Fatalf("Next = %q, want %q", r.Next, "staff m4-metrics after m2 merges")
	}
	if !r.HasNextWork() {
		t.Fatal("expected HasNextWork() = true")
	}
	if full := r.FullObjective(); !strings.Contains(full, "Next: staff m4-metrics after m2 merges") || !strings.Contains(full, planFile) {
		t.Fatalf("FullObjective missing expected sections:\n%s", full)
	}
	if gotRef := planner.ExtractObjectivePlanRef(r.FullObjective()); gotRef != planFile {
		t.Fatalf("ExtractObjectivePlanRef(FullObjective) = %q, want resolved %q", gotRef, planFile)
	}
	if !planner.MatchesObjectivePlanRef(r.FullObjective(), planFile) || !planner.MatchesObjectivePlanRef(r.FullObjective(), "docs/my-plan.md") {
		t.Fatalf("MatchesObjectivePlanRef rejected valid plan ref in FullObjective:\n%s", r.FullObjective())
	}

	// 1b. Unconfined Plan: paths (/etc/passwd or ../escape) are rejected by extractPlanPath and stripped by FullObjective.
	if got := extractPlanPath(dir, "Plan: /etc/passwd\n"); got != "" {
		t.Fatalf("extractPlanPath(/etc/passwd) = %q, want empty", got)
	}
	if got := extractPlanPath(dir, "Plan: ../../escape.md\n"); got != "" {
		t.Fatalf("extractPlanPath(../../escape.md) = %q, want empty", got)
	}
	if got := extractPlanPath(dir, "Plan: post:286c9d4f-9da3-4787-aa66-bd97aa21f947\n"); got != "post:286c9d4f-9da3-4787-aa66-bd97aa21f947" {
		t.Fatalf("extractPlanPath(nebula post) = %q", got)
	}
	if got := extractPlanPath(dir, "See Plan: foo.md\n- Plan: bar.md\nNext: update Plan: log\n"); got != "" {
		t.Fatalf("extractPlanPath matched prose/list lines: %q", got)
	}
	unconfinedRun := Run{
		Dir:       dir,
		Objective: "# Goal\nPlan: /etc/passwd\n- Plan: /etc/passwd\n* Plan: ../outside.md\nPlan: ../outside.md\nPlan: sub/future-plan.md\nNext: staff m4\n",
	}
	if full := unconfinedRun.FullObjective(); strings.Contains(full, "/etc/passwd") || strings.Contains(full, "../outside.md") || !strings.Contains(full, "Plan: sub/future-plan.md") {
		t.Fatalf("FullObjective failed to strip unconfined Plan: lines or dropped safe relative Plan: line:\n%s", full)
	}
	if got := extractPlanPath(dir, "Plan: sub/future-plan.md\nSee docs/my-plan.md for background.\n"); got != filepath.Join(dir, "sub", "future-plan.md") {
		t.Fatalf("extractPlanPath(nonexistent safe relative plan with incidental .md) = %q, want %q", got, filepath.Join(dir, "sub", "future-plan.md"))
	}
	if refs := planner.ExtractObjectivePlanRefs("Plan: docs/a.md\nPlan path: docs/b.md\nPlan file: post:286c9d4f-9da3-4787-aa66-bd97aa21f947\n- Plan: /etc/passwd\nPlan: ../escape.md\n"); len(refs) != 3 {
		t.Fatalf("ExtractObjectivePlanRefs returned %v, want 3 refs", refs)
	}
	adoptedRun := store.Run{
		ID:        "run-adopted",
		Objective: "# Run\nGoal: Full goal\nPlan: docs/my-plan.md\n- [x] M1\n- [ ] M2\nNext: staff M2\n",
		PlanRef:   "ORCHESTRATOR: orca since 2026-11-14T18:18:00Z (orca-chat:front-desk)",
	}
	preservedObj, preservedRef := planner.PreserveAdoptedObjective(adoptedRun, "Short summary", "docs/other.md")
	if preservedObj != adoptedRun.Objective || preservedRef != adoptedRun.PlanRef {
		t.Fatalf("PreserveAdoptedObjective(adopted) = (%q, %q), want original objective and marker", preservedObj, preservedRef)
	}
	nativeRun := store.Run{ID: "run-native", Objective: "Old goal", PlanRef: "docs/old.md"}
	nativeObj, nativeRef := planner.PreserveAdoptedObjective(nativeRun, "New goal", "docs/new.md")
	if nativeObj != "New goal" || nativeRef != "docs/new.md" {
		t.Fatalf("PreserveAdoptedObjective(native) = (%q, %q), want (New goal, docs/new.md)", nativeObj, nativeRef)
	}

	// 2. HasNextWork returns false for "none" / "done" / "n/a" / "complete" and true for unchecked checkboxes and multi-line Next:.
	for _, doneNext := range []string{"none", "done.", "N/A", "complete", "completed"} {
		rDone := Run{Objective: "Goal: finished\nNext: " + doneNext + "\n", Next: extractNextSection("Goal: finished\nNext: " + doneNext + "\n")}
		if rDone.HasNextWork() {
			t.Fatalf("expected HasNextWork() = false for Next: %s, got true (Next=%q)", doneNext, rDone.Next)
		}
	}
	for _, activeNext := range []string{"Complete the M3 review", "Finished X; start M4", "(a) staff M4", ".github workflow fix"} {
		obj := "Goal: active\nNext: " + activeNext + "\n"
		rActive := Run{Objective: obj, Next: extractNextSection(obj)}
		if !rActive.HasNextWork() {
			t.Fatalf("expected HasNextWork() = true for Next: %s, got false (Next=%q)", activeNext, rActive.Next)
		}
	}
	multiNext := extractNextSection("Goal: g\nNext:\n- step 1\n- step 2\n\nMilestones:\n- [x] M1\n")
	if multiNext != "- step 1\n- step 2" {
		t.Fatalf("extractNextSection(multi-line) = %q, want %q", multiNext, "- step 1\n- step 2")
	}
	for _, marker := range []string{"- [ ] M2 open", "* [ ] M2 bullet", "+ [ ] M2 plus", "1. [ ] M2 numbered", "2) [ ] M2 paren"} {
		rCheckbox := Run{Objective: "# Run\n- [x] M1\n" + marker + "\n"}
		if !rCheckbox.HasNextWork() {
			t.Fatalf("expected HasNextWork() = true for unchecked item %q", marker)
		}
	}

	// 3. ledgerPhase maps synthetic phase (e.g. "integrate") to fallback or last configured phase.
	if got := ledgerPhase("integrate", []string{"swe", "review"}, "review"); got != "review" {
		t.Fatalf("ledgerPhase(integrate, [swe,review], review) = %q, want review", got)
	}
	if got := ledgerPhase("integrate", []string{"swe", "review"}, ""); got != "review" {
		t.Fatalf("ledgerPhase(integrate, [swe,review], '') = %q, want review", got)
	}

	// 4. checkpoint !found branch for subagent-swarm writes sessions and spawn.json, and lockLedgerFileWith retries under brief contention.
	lockFile, err := os.OpenFile(filepath.Join(dir, "state.json.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockLedgerFile(lockFile); err != nil {
		t.Fatal(err)
	}
	contender, err := os.OpenFile(filepath.Join(dir, "state.json.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	if err := lockLedgerFileWith(contender, func() { _ = lockFile.Close() }); err != nil {
		t.Fatalf("lockLedgerFileWith under brief contention failed: %v", err)
	}
	_ = contender.Close()
	newLane := lane.Lane{
		ID:         "new-lane",
		RunID:      "run-1",
		Title:      "New lane",
		Branch:     "swarm-lane/run-1/new-lane",
		Phase:      "swe",
		Round:      1,
		Status:     lane.StatusRunning,
		SessionID:  "sess-new",
		TerminalID: "term-new",
		Terminals:  map[string]string{"swe": "term-new"},
	}
	if err := checkpoint(dir, []lane.Lane{newLane}); err != nil {
		t.Fatalf("checkpoint with new lane failed: %v", err)
	}
	if !fileExists(filepath.Join(dir, "new-lane.swe.spawn.json")) {
		t.Fatal("expected checkpoint(!found) for subagent-swarm to write new-lane.swe.spawn.json")
	}
	if stateAfter := readText(filepath.Join(dir, "state.json")); !strings.Contains(stateAfter, `"nebula:term-new"`) {
		t.Fatalf("expected checkpoint(!found) to record sessions map, got:\n%s", stateAfter)
	}

	// 5. checkpoint !found branch for nebula-swarm seeds priority, depends_on, verdict, terminal_state, and terminals,
	// and appendNebulaSwarmEvents emits started, verdict, status, phase, and merged TSV lines.
	nsDir := t.TempDir()
	write(t, filepath.Join(nsDir, "state.json"), `{"config":{"goal":"ns goal","orchestrator_session_id":"sess-orch","phases":[{"name":"swe","kind":"claude"},{"name":"review","kind":"claude"}]},"tasks":[]}`)
	nsLane := lane.Lane{
		ID:         "m2-new",
		RunID:      "run-ns",
		Title:      "M2 new lane",
		Branch:     "swarm-lane/run-ns/m2-new",
		Phase:      "swe",
		Round:      1,
		Status:     lane.StatusRunning,
		SessionID:  "sess-ns-1",
		TerminalID: "term-ns-1",
	}
	if err := checkpoint(nsDir, []lane.Lane{nsLane}); err != nil {
		t.Fatalf("checkpoint(nebula-swarm !found): %v", err)
	}
	nsState := readText(filepath.Join(nsDir, "state.json"))
	for _, wantField := range []string{`"priority": "p1"`, `"depends_on": []`, `"verdict": ""`, `"terminal_state": ""`, `"term-ns-1"`} {
		if !strings.Contains(nsState, wantField) {
			t.Fatalf("nebula-swarm checkpoint(!found) missing %s:\n%s", wantField, nsState)
		}
	}
	t0 := time.Date(2026, 11, 14, 19, 0, 0, 0, time.UTC)
	if err := appendNebulaSwarmEvents(nsDir, t0, nil, nsLane, []string{"swe", "review"}); err != nil {
		t.Fatalf("appendNebulaSwarmEvents(started): %v", err)
	}
	blockedLane := nsLane
	blockedLane.Status = lane.StatusBlocked
	if err := appendNebulaSwarmEvents(nsDir, t0.Add(10*time.Second), &nsLane, blockedLane, []string{"swe", "review"}); err != nil {
		t.Fatalf("appendNebulaSwarmEvents(status): %v", err)
	}
	revLane := nsLane
	revLane.Phase = "review"
	revLane.Status = lane.StatusDone
	revLane.TerminalID = "term-ns-2"
	revLane.PR = 5799
	revLane.ApprovalSHA = "abc1234"
	revLane.MergeSHA = "deadbeef"
	if err := appendNebulaSwarmEvents(nsDir, t0.Add(time.Minute), &blockedLane, revLane, []string{"swe", "review"}); err != nil {
		t.Fatalf("appendNebulaSwarmEvents(review/merged): %v", err)
	}
	eventsLog := readText(filepath.Join(nsDir, "EVENTS"))
	for _, wantEvent := range []string{
		"m2-new\tstarted\tswe term-ns-1",
		"m2-new\tstatus\trunning -> blocked",
		"m2-new\tphase\tswe -> review",
		"m2-new\tstarted\treview term-ns-2",
		"m2-new\tverdict\tpass",
		"m2-new\tmerged\t#5799 deadbeef",
	} {
		if !strings.Contains(eventsLog, wantEvent) {
			t.Fatalf("EVENTS missing %q:\n%s", wantEvent, eventsLog)
		}
	}
}

func TestAdoptedNebulaSwarmSpawnUsesConfigRepo(t *testing.T) {
	t.Parallel()
	const wantRepoID = "2a19e6e7-939e-44b2-a80f-8980030efc50"
	const wrongRepoID = "76672137-0000-0000-0000-000000000000"

	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"trial m1: slim prompts","base":"main","target":"main","repo":"`+wantRepoID+`","orchestrator_session_id":"sess-orch","merge":"none",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high","prompt":"","chain":[{"kind":"claude","model":"opus","effort":"high"}]}]},
"tasks":[{"id":"m2-gaps3","title":"gaps 3","branch":"swarm-lane/`+nsRun+`/m2-gaps3","priority":"p1","status":"planned","phase":"swe","round":1,"session_id":"","terminals":{}}]}`)
	// Keep .repo-slug present so both RepositoryID and Repository slug are set,
	// proving that an exact repositoryId match short-circuits even when gitUrl
	// points to an internal mirror with a different slug.

	var gotRepoID atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-repositories":
			_, _ = io.WriteString(w, `{"repositories":[{"id":"`+wrongRepoID+`","fullName":"build-perf-sweeping/house-design"},{"id":"`+wantRepoID+`","fullName":"sycamore-labs/nebula"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions":
			var body struct {
				RepositoryID string `json:"repositoryId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotRepoID.Store(body.RepositoryID)
			// Even if gitUrl points to an internal mirror or renamed slug, an exact
			// repositoryId match short-circuits without rejecting the session.
			_, _ = io.WriteString(w, `{"session":{"id":"sess-gaps3","status":"ready","repositoryId":"`+body.RepositoryID+`","gitUrl":"ssh://git@git.internal/mirrors/nebula.git/"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions/sess-gaps3/terminals":
			_, _ = io.WriteString(w, `{"id":"term-gaps3-swe"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	clk := clock.NewFakeClock(time.Date(2026, 11, 14, 19, 1, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv, err := nebula.New(nebula.Config{
		APIURL: srv.URL,
		Token:  "test-token",
		Clock:  clk,
		Events: st.Events(),
	})
	if err != nil {
		t.Fatal(err)
	}

	controllers := NewControllers(t.Context(), st, drv, clk, nil)
	label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
	s := nsService(root, loop, label)
	s.Driver = controllers
	r, err := s.Adopt(t.Context(), nsRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

	controllers.mu.Lock()
	ctrl := controllers.runs[nsRun].controller
	controllers.mu.Unlock()
	if err := ctrl.StartLane(t.Context(), "m2-gaps3"); err != nil {
		t.Fatalf("StartLane: %v", err)
	}
	if got, _ := gotRepoID.Load().(string); got != wantRepoID {
		t.Fatalf("create session repositoryId = %q, want %q", got, wantRepoID)
	}
}

func TestAdoptedSpawnRefusedWhenRepoMissing(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"trial m1: slim prompts","base":"main","target":"main","repo":"","orchestrator_session_id":"sess-orch","merge":"none",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high","prompt":"","chain":[{"kind":"claude","model":"opus","effort":"high"}]}]},
"tasks":[{"id":"m2-gaps3","title":"gaps 3","branch":"swarm-lane/`+nsRun+`/m2-gaps3","priority":"p1","status":"planned","phase":"swe","round":1,"session_id":"","terminals":{}}]}`)
	os.Remove(filepath.Join(dir, ".repo-slug"))

	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-repositories":
			_, _ = io.WriteString(w, `{"repositories":[{"id":"76672137-0000-0000-0000-000000000000","fullName":"build-perf-sweeping/house-design"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions":
			creates.Add(1)
			_, _ = io.WriteString(w, `{"session":{"id":"sess-unexpected","status":"ready"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	clk := clock.NewFakeClock(time.Date(2026, 11, 14, 19, 1, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv, err := nebula.New(nebula.Config{
		APIURL: srv.URL,
		Token:  "test-token",
		Clock:  clk,
		Events: st.Events(),
	})
	if err != nil {
		t.Fatal(err)
	}

	controllers := NewControllers(t.Context(), st, drv, clk, nil)
	label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
	s := nsService(root, loop, label)
	s.Driver = controllers
	_, err = s.Adopt(t.Context(), nsRun)
	if err == nil || !strings.Contains(err.Error(), "repository is not configured") {
		t.Fatalf("Adopt with missing repository = %v, want repository is not configured error", err)
	}
	if got := creates.Load(); got != 0 {
		t.Fatalf("POST /api/code-sessions called %d times, want 0", got)
	}
}

func TestAdoptedSpawnReleasesSessionInWrongRepo(t *testing.T) {
	t.Parallel()
	const wantRepoID = "2a19e6e7-939e-44b2-a80f-8980030efc50"
	const wrongRepoID = "76672137-0000-0000-0000-000000000000"

	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"trial m1: slim prompts","base":"main","target":"main","repo":"`+wantRepoID+`","orchestrator_session_id":"sess-orch","merge":"none",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high","prompt":"","chain":[{"kind":"claude","model":"opus","effort":"high"}]}]},
"tasks":[{"id":"m2-gaps3","title":"gaps 3","branch":"swarm-lane/`+nsRun+`/m2-gaps3","priority":"p1","status":"planned","phase":"swe","round":1,"session_id":"","terminals":{}}]}`)

	var released atomic.Value
	var termStarts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-repositories":
			_, _ = io.WriteString(w, `{"repositories":[{"id":"`+wrongRepoID+`","fullName":"build-perf-sweeping/house-design"},{"id":"`+wantRepoID+`","fullName":"sycamore-labs/nebula"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"session":{"id":"018a40a9","status":"ready","repositoryId":"`+wrongRepoID+`","gitUrl":"https://github.com/build-perf-sweeping/house-design.git"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions/018a40a9/release":
			released.Store("018a40a9")
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions/018a40a9/terminals":
			termStarts.Add(1)
			_, _ = io.WriteString(w, `{"id":"term-wrong"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	clk := clock.NewFakeClock(time.Date(2026, 11, 14, 19, 1, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv, err := nebula.New(nebula.Config{
		APIURL: srv.URL,
		Token:  "test-token",
		Clock:  clk,
		Events: st.Events(),
	})
	if err != nil {
		t.Fatal(err)
	}

	controllers := NewControllers(t.Context(), st, drv, clk, nil)
	label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
	s := nsService(root, loop, label)
	s.Driver = controllers
	_, err = s.Adopt(t.Context(), nsRun)
	if err == nil {
		t.Fatal("Adopt succeeded on wrong-repo session; want error")
	}
	if got, _ := released.Load().(string); got != "018a40a9" {
		t.Fatalf("released session = %q, want 018a40a9", got)
	}
	if got := termStarts.Load(); got != 0 {
		t.Fatalf("terminal starts on wrong-repo session = %d, want 0", got)
	}

	// Also verify that if Nebula omits repositoryId on the created session and
	// only returns a gitUrl for the wrong repository, verifySessionRepository
	// resolves gitUrl via /api/code-repositories, releases the session, and fails.
	released.Store("")
	srvOmittedID := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-repositories":
			_, _ = io.WriteString(w, `{"repositories":[{"id":"`+wrongRepoID+`","fullName":"build-perf-sweeping/house-design"},{"id":"`+wantRepoID+`","fullName":"sycamore-labs/nebula"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"session":{"id":"018a40aa","status":"ready","gitUrl":"git@github.com:build-perf-sweeping/house-design.git"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions/018a40aa/release":
			released.Store("018a40aa")
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srvOmittedID.Close()

	drvOmitted, err := nebula.New(nebula.Config{
		APIURL: srvOmittedID.URL,
		Token:  "test-token",
		Clock:  clk,
		Events: st.Events(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = drvOmitted.SpawnSession(t.Context(), lane.SpawnRequest{
		RunID:        nsRun,
		RepositoryID: wantRepoID,
		LaneID:       "m2-gaps3",
		Phase:        "swe",
		Round:        1,
		Harness:      "claude",
		Brief:        "test",
	})
	if err == nil || !strings.Contains(err.Error(), "does not match expected") {
		t.Fatalf("SpawnSession with omitted repositoryId and wrong gitUrl = %v, want mismatch error", err)
	}
	if got, _ := released.Load().(string); got != "018a40aa" {
		t.Fatalf("released session when repositoryId omitted = %q, want 018a40aa", got)
	}

	// Also verify that if Nebula omits both repositoryId and gitUrl on create
	// and GetSession, verifySessionRepository fails closed, releases the session,
	// and refuses the spawn.
	released.Store("")
	srvOmittedBoth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"session":{"id":"018a40ab","status":"ready"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-sessions/018a40ab":
			_, _ = io.WriteString(w, `{"session":{"id":"018a40ab","status":"ready"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions/018a40ab/release":
			released.Store("018a40ab")
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srvOmittedBoth.Close()

	drvOmittedBoth, err := nebula.New(nebula.Config{
		APIURL: srvOmittedBoth.URL,
		Token:  "test-token",
		Clock:  clk,
		Events: st.Events(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = drvOmittedBoth.SpawnSession(t.Context(), lane.SpawnRequest{
		RunID:        nsRun,
		RepositoryID: wantRepoID,
		LaneID:       "m2-gaps3",
		Phase:        "swe",
		Round:        1,
		Harness:      "claude",
		Brief:        "test",
	})
	if err == nil || !strings.Contains(err.Error(), "omitted repositoryId and gitUrl") {
		t.Fatalf("SpawnSession with both repositoryId and gitUrl omitted = %v, want omitted repositoryId and gitUrl error", err)
	}
	if got, _ := released.Load().(string); got != "018a40ab" {
		t.Fatalf("released session when both fields omitted = %q, want 018a40ab", got)
	}
}

func TestAdoptedBriefCitingObjectiveInlinesItems(t *testing.T) {
	t.Parallel()
	const wantRepoID = "2a19e6e7-939e-44b2-a80f-8980030efc50"

	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"trial m1: slim prompts","base":"main","target":"main","repo":"`+wantRepoID+`","orchestrator_session_id":"sess-orch","merge":"none",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high","prompt":"","chain":[{"kind":"claude","model":"opus","effort":"high"}]}]},
"tasks":[{"id":"m2-gaps3","title":"gaps 3","branch":"swarm-lane/`+nsRun+`/m2-gaps3","priority":"p1","status":"planned","phase":"swe","round":1,"session_id":"","terminals":{}}]}`)
	objectiveText := "# 20261114-trial-m1\n\n## m2-gaps3\n- Item A: strip redundant prompt preambles.\n- Item B: verify token count drops below 1200.\n"
	write(t, filepath.Join(dir, "OBJECTIVE.md"), objectiveText)
	write(t, filepath.Join(dir, "NOTES.md"), "host-only scratch notes that must not be inlined unless allowlisted\n")
	write(t, filepath.Join(dir, "m2-gaps3.swe.prompt"), "Implement m2-gaps3. Read OBJECTIVE.md for the exact items. Do not inline NOTES.md.\n")

	clk := clock.NewFakeClock(time.Date(2026, 11, 14, 19, 1, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := lane.NewFakeDriver()

	var disp *dispatcher.Dispatcher
	controllers := NewControllers(t.Context(), st, drv, clk, func(_ string, answer func(context.Context, string, string, string) error) controller.Judge {
		fakeClient := model.NewFakeClient(
			model.Response{
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(model.NewToolUseBlock(
					"call-spawn",
					dispatcher.ToolLaneSpawn,
					json.RawMessage(`{"item_id":"m2-gaps4","lane_id":"m2-gaps4","brief":"Implement m2-gaps4. Read OBJECTIVE.md for the exact items. Check NOTES.md in the repo."}`),
				)),
			},
			model.Response{
				StopReason: model.StopReasonEndTurn,
				Message:    model.NewAssistantMessage(model.NewTextBlock("spawned m2-gaps4")),
			},
		)
		disp = dispatcher.New(dispatcher.Config{}, st, drv, clk, fakeClient)
		disp.SetOwnerAnswer(answer)
		return disp
	})
	label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
	s := nsService(root, loop, label)
	s.Driver = controllers
	r, err := s.Adopt(t.Context(), nsRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

	if len(drv.Spawns) != 1 || drv.Spawns[0].RepositoryID != wantRepoID {
		t.Fatalf("Spawns = %+v, want 1 spawn with RepositoryID=%q", drv.Spawns, wantRepoID)
	}
	gotBrief := drv.Spawns[0].Brief
	for _, want := range []string{
		"Item A: strip redundant prompt preambles.",
		"Item B: verify token count drops below 1200.",
	} {
		if !strings.Contains(gotBrief, want) {
			t.Fatalf("brief citing OBJECTIVE.md missing %q:\n%s", want, gotBrief)
		}
	}
	if strings.Contains(gotBrief, "host-only scratch notes") {
		t.Fatalf("brief unexpectedly inlined unallowlisted NOTES.md:\n%s", gotBrief)
	}

	// Exercise the real Dispatcher (wired via SetDriver in NewControllers)
	// spawning a new backlog item whose brief cites OBJECTIVE.md.
	if err := st.Backlog().Add(t.Context(), store.BacklogItem{
		ID:       "m2-gaps4",
		RunID:    nsRun,
		Title:    "gaps 4",
		Priority: store.PriorityP1,
		Status:   store.BacklogOpen,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Events().Append(t.Context(), store.Event{
		RunID:   nsRun,
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "spawn m2-gaps4",
	}); err != nil {
		t.Fatal(err)
	}
	controllers.Wake(nsRun)
	waitFor(t, func() bool {
		got, err := st.Lanes().Get(t.Context(), nsRun, "m2-gaps4")
		return err == nil && got.Status == lane.StatusRunning
	})
	if len(drv.Spawns) != 2 || drv.Spawns[1].LaneID != "m2-gaps4" || drv.Spawns[1].RepositoryID != wantRepoID {
		t.Fatalf("Dispatcher spawn = %+v, want m2-gaps4 with RepositoryID=%q", drv.Spawns, wantRepoID)
	}
	dispBrief := drv.Spawns[1].Brief
	if gotCount := strings.Count(dispBrief, "Inlined from OBJECTIVE.md:"); gotCount != 1 {
		t.Fatalf("Dispatcher brief Inlined from OBJECTIVE.md count = %d, want 1:\n%s", gotCount, dispBrief)
	}
	if !strings.Contains(dispBrief, "Item A: strip redundant prompt preambles.") {
		t.Fatalf("Dispatcher brief missing OBJECTIVE.md items:\n%s", dispBrief)
	}
	if strings.Contains(dispBrief, "host-only scratch notes") {
		t.Fatalf("Dispatcher brief unexpectedly inlined NOTES.md:\n%s", dispBrief)
	}
}

func TestAdoptedWrongRepoLaneReportRespawnsInsteadOfSteering(t *testing.T) {
	t.Parallel()
	const wantRepoID = "2a19e6e7-939e-44b2-a80f-8980030efc50"

	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 5, 35, 0, 0, time.UTC))
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"trial m1: slim prompts","base":"main","target":"main","repo":"`+wantRepoID+`","orchestrator_session_id":"sess-orch","merge":"none",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high","prompt":"","chain":[{"kind":"claude","model":"opus","effort":"high"}]}]},
"tasks":[{"id":"m2-gaps3","title":"gaps 3","branch":"swarm-lane/`+nsRun+`/m2-gaps3","priority":"p1","status":"running","phase":"swe","round":1,"session_id":"018a40a9","terminals":{"swe":{"1":"term-bad"}},"terminal_state":"working","verdict":""}]}`)
	os.Remove(filepath.Join(dir, ".repo-slug"))

	clk := clock.NewFakeClock(time.Date(2026, 11, 14, 19, 12, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := lane.NewFakeDriver()

	controllers := NewControllers(t.Context(), st, drv, clk, nil)
	label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
	s := nsService(root, loop, label)
	s.Driver = controllers
	r, err := s.Adopt(t.Context(), nsRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

	// First verify that normal blocked/needs-input reports mentioning branches,
	// cross-repo dependencies, or the run's own repo slug do NOT trigger a
	// false-positive wrong-repo respawn.
	drv.Emit(lane.Event{
		RunID:      nsRun,
		LaneID:     "m2-gaps3",
		SessionID:  "018a40a9",
		TerminalID: "term-bad",
		Kind:       lane.EventPhaseReported,
		Report: &lane.PhaseReport{
			LaneID:     "m2-gaps3",
			TerminalID: "term-bad",
			Phase:      "swe",
			Round:      1,
			Outcome:    lane.OutcomeNeedsInput,
			Summary:    "checked out on main, checked out to feature/foo, and checked out to sycamore-labs/nebula; waiting on PR in another repository.\nShould we use X instead of Y?",
			Raw:        "Outcome: needs-input\nchecked out on main, checked out to feature/foo, and checked out to sycamore-labs/nebula; waiting on PR in another repository.\nShould we use X instead of Y?",
		},
	})
	waitFor(t, func() bool {
		got, err := st.Lanes().Get(t.Context(), nsRun, "m2-gaps3")
		return err == nil && got.Status == lane.StatusBlocked
	})
	if len(drv.Releases) != 0 || len(drv.Spawns) != 0 {
		t.Fatalf("false-positive wrong-repo respawn on benign report: Releases=%v Spawns=%v", drv.Releases, drv.Spawns)
	}

	// Emit the exact needs-input report from a lane spawned in the wrong repo.
	drv.Emit(lane.Event{
		RunID:      nsRun,
		LaneID:     "m2-gaps3",
		SessionID:  "018a40a9",
		TerminalID: "term-bad",
		Kind:       lane.EventPhaseReported,
		Report: &lane.PhaseReport{
			LaneID:     "m2-gaps3",
			TerminalID: "term-bad",
			Phase:      "swe",
			Round:      1,
			Outcome:    lane.OutcomeNeedsInput,
			Summary:    "the lane is checked out to build-perf-sweeping/house-design instead of sycamore-labs/nebula; OBJECTIVE.md is also unavailable.",
			Raw:        "Outcome: needs-input\nthe lane is checked out to build-perf-sweeping/house-design instead of sycamore-labs/nebula; OBJECTIVE.md is also unavailable.",
		},
	})

	waitFor(t, func() bool {
		got, err := st.Lanes().Get(t.Context(), nsRun, "m2-gaps3")
		return err == nil && got.Status == lane.StatusRunning && got.SessionID != "018a40a9"
	})
	if drv.Releases[0] != "018a40a9" {
		t.Fatalf("released session = %q, want 018a40a9", drv.Releases[0])
	}
	if len(drv.Spawns) != 1 || drv.Spawns[0].RepositoryID != wantRepoID {
		t.Fatalf("respawn Spawns = %+v, want RepositoryID=%q", drv.Spawns, wantRepoID)
	}
	if len(drv.Steers) != 0 {
		t.Fatalf("unexpected steer sends = %+v, want 0", drv.Steers)
	}
	gotLane, err := st.Lanes().Get(t.Context(), nsRun, "m2-gaps3")
	if err != nil {
		t.Fatal(err)
	}
	if gotLane.Status != lane.StatusRunning || gotLane.SessionID == "018a40a9" {
		t.Fatalf("m2-gaps3 after wrong-repo respawn = %+v, want running on new session", gotLane)
	}

	// A second wrong-repo report in the same phase/round must NOT trigger an
	// unbounded respawn loop; it caps at 1 automatic respawn and blocks the lane.
	secondSession := gotLane.SessionID
	secondTerm := gotLane.TerminalID
	drv.Emit(lane.Event{
		RunID:      nsRun,
		LaneID:     "m2-gaps3",
		SessionID:  secondSession,
		TerminalID: secondTerm,
		Kind:       lane.EventPhaseReported,
		Report: &lane.PhaseReport{
			LaneID:     "m2-gaps3",
			TerminalID: secondTerm,
			Phase:      "swe",
			Round:      1,
			Outcome:    lane.OutcomeNeedsInput,
			Summary:    "still in the wrong repository",
			Raw:        "Outcome: needs-input\nstill in the wrong repository",
		},
	})
	waitFor(t, func() bool {
		got, err := st.Lanes().Get(t.Context(), nsRun, "m2-gaps3")
		return err == nil && got.Status == lane.StatusBlocked
	})
	if len(drv.Spawns) != 1 || len(drv.Releases) != 1 {
		t.Fatalf("expected wrong-repo respawn cap of 1 per phase/round, got Spawns=%d Releases=%d", len(drv.Spawns), len(drv.Releases))
	}
}

func TestAdoptedSubagentSwarmEnvRepoUsedOnSpawn(t *testing.T) {
	t.Parallel()
	const wantRepoID = "2a19e6e7-939e-44b2-a80f-8980030efc50"

	w := newWorld(t)
	write(t, filepath.Join(w.dir, "env.sh"), "export SNL_REPO_ID=\""+wantRepoID+"\"\nexport SNL_REPO_URL=\"https://github.com/sycamore-labs/nebula.git\"\n")
	write(t, filepath.Join(w.dir, "state.json"), `{"config":{"goal":"host goal","phases":[{"name":"swe"}]},"tasks":[{"id":"a","title":"A","phase":"swe","status":"planned","round":1,"sessions":{}}]}`)

	clk := clock.NewFakeClock(time.Date(2026, 11, 14, 19, 1, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := lane.NewFakeDriver()

	controllers := NewControllers(t.Context(), st, drv, clk, nil)
	w.watcher.running = false
	s := w.service()
	s.Driver = controllers
	r, err := s.Adopt(t.Context(), testRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

	if len(drv.Spawns) != 1 || drv.Spawns[0].RepositoryID != wantRepoID || drv.Spawns[0].Repository != "sycamore-labs/nebula" {
		t.Fatalf("Spawns = %+v, want RepositoryID=%q Repository=sycamore-labs/nebula", drv.Spawns, wantRepoID)
	}
}

func TestNormalizeRepositorySlugTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw      string
		wantSlug string
		wantIs   bool
	}{
		{"sycamore-labs/nebula", "sycamore-labs/nebula", true},
		{"https://github.com/sycamore-labs/nebula.git", "sycamore-labs/nebula", true},
		{"https://token@github.com/sycamore-labs/nebula.GIT/", "sycamore-labs/nebula", true},
		{"ssh://git@gitlab.example.com/sycamore-labs/nebula.git/", "sycamore-labs/nebula", true},
		{"git@github.com:sycamore-labs/nebula.git", "sycamore-labs/nebula", true},
		{"2a19e6e7-939e-44b2-a80f-8980030efc50", "2a19e6e7-939e-44b2-a80f-8980030efc50", false},
		{"main", "main", false},
		{"a/b/c", "a/b/c", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := lane.NormalizeRepositorySlug(tc.raw); got != tc.wantSlug {
			t.Errorf("NormalizeRepositorySlug(%q) = %q, want %q", tc.raw, got, tc.wantSlug)
		}
		if got := lane.IsRepositorySlug(tc.raw); got != tc.wantIs {
			t.Errorf("IsRepositorySlug(%q) = %v, want %v", tc.raw, got, tc.wantIs)
		}
	}
}

func TestClipRunFileContent(t *testing.T) {
	t.Parallel()
	if got := clipRunFileContent("  short text\n", 100); got != "short text" {
		t.Fatalf("under-limit clipRunFileContent = %q, want %q", got, "short text")
	}
	// Line-boundary cut in the second half of maxBytes.
	lined := "line one\nline two\nline three that exceeds the limit"
	gotLined := clipRunFileContent(lined, 22)
	if !strings.HasPrefix(gotLined, "line one\nline two\n... (truncated ") {
		t.Fatalf("newline-cut clipRunFileContent = %q", gotLined)
	}
	// Multibyte UTF-8 rune boundary safety when no newline is in the second half.
	multibyte := "éééééééééé"
	gotUTF8 := clipRunFileContent(multibyte, 5)
	if !strings.HasPrefix(gotUTF8, "éé\n... (truncated ") {
		t.Fatalf("utf8-safe clipRunFileContent = %q", gotUTF8)
	}
}

func TestAdoptedSelfMergeRecipeHasNoControllerMergePhase(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".swarm")
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", time.Date(2026, 11, 14, 20, 30, 0, 0, time.UTC))
	// Fixture run whose OBJECTIVE.md states the run's rule: a lane merges once
	// pr-polish finishes, and whose active lane is in clean r2 (#5811).
	write(t, filepath.Join(dir, "OBJECTIVE.md"), "# 20261114-trial-m1\n\nGoal: trial m1: slim prompts.\nMerge rule: a lane merges once pr-polish finishes.\n")
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"trial m1: slim prompts","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"sess-orch","merge":"squash",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high","prompt":""},
{"name":"clean","kind":"codex","model":"gpt-5.6-sol","effort":"high","prompt":""},
{"name":"review","kind":"claude","model":"sonnet","effort":"high","prompt":""}]},
"tasks":[
{"id":"m3-polish","title":"polish prompts","branch":"swarm-lane/20261114-trial-m1/m3-polish","priority":"p1","status":"running","phase":"clean","round":2,"session_id":"sess-m3","terminals":{"swe":{"1":"term-m3-swe"},"clean":{"2":"term-m3-clean"}},"pr":5811,"pr_head":"ccc","terminal_state":"working","verdict":""}]}`)

	r, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	workflow, lanes, _, err := controllerState(r)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.MergePolicy.Mode != recipe.MergeModeSelf {
		t.Fatalf("workflow.MergePolicy.Mode = %q, want %q", workflow.MergePolicy.Mode, recipe.MergeModeSelf)
	}
	if _, hasIntegrate := workflow.Phase(recipe.PhaseIntegrate); hasIntegrate {
		t.Fatalf("self-merge adopted recipe must not have controller merge phase %q: phases=%+v", recipe.PhaseIntegrate, workflow.Phases)
	}
	if _, hasReview := workflow.Phase(recipe.PhaseReview); hasReview {
		t.Fatalf("adopted recipe where a lane merges once pr-polish finishes must not have review phase: phases=%+v", workflow.Phases)
	}
	cleanPhase, hasClean := workflow.Phase(recipe.PhaseClean)
	if !hasClean || !strings.Contains(cleanPhase.BriefTemplate, "/pr-polish-nebula") {
		t.Fatalf("clean phase = %+v, want /pr-polish-nebula brief", cleanPhase)
	}
	mech, _ := workflow.MatchTransitions(recipe.PhaseClean, recipe.OutcomeDone, false, 2)
	if mech == nil || mech.To != recipe.TargetComplete {
		t.Fatalf("clean -> done transition = %+v, want To=%q", mech, recipe.TargetComplete)
	}
	if len(lanes) != 1 || lanes[0].Phase != recipe.PhaseClean || lanes[0].Round != 2 {
		t.Fatalf("imported lanes = %+v, want m3-polish in clean r2", lanes)
	}

	// Also verify string-array config.phases ["swe", "pr-polish"] with merge="self".
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"trial m1","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"sess-orch","merge":"self",
"phases":["swe","pr-polish"]},
"tasks":[{"id":"m3-polish","title":"polish prompts","branch":"swarm-lane/20261114-trial-m1/m3-polish","priority":"p1","status":"running","phase":"pr-polish","round":1,"session_id":"sess-m3","terminals":{"pr-polish":{"1":"term-m3-polish"}},"pr":5811}]}`)
	r2, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	wf2, lanes2, _, err := controllerState(r2)
	if err != nil {
		t.Fatal(err)
	}
	if wf2.MergePolicy.Mode != recipe.MergeModeSelf {
		t.Fatalf("wf2.MergePolicy.Mode = %q, want %q", wf2.MergePolicy.Mode, recipe.MergeModeSelf)
	}
	if _, hasIntegrate := wf2.Phase(recipe.PhaseIntegrate); hasIntegrate {
		t.Fatalf("wf2 must not have integrate phase: %+v", wf2.Phases)
	}
	if len(lanes2) != 1 || lanes2[0].Phase != recipe.PhaseClean || lanes2[0].TerminalID != "term-m3-polish" {
		t.Fatalf("lanes2 = %+v, want normalized clean phase with term-m3-polish", lanes2)
	}

	// Negative check: prose mentioning "no review", negated "do not self-merge",
	// a goal mentioning "add self-merge support", "Self-merge: disabled", or
	// unlabelled free-text prose in OBJECTIVE.md must NOT flip merge policy to
	// self or drop the review phase.
	for _, negObj := range []string{
		"# 20261114-trial-m1\n\nGoal: ensure no review comments are dropped.\nMerge rule: lanes must not self-merge; controller merges after review.\n",
		"# 20261114-trial-m1\n\nGoal: add self-merge support to the recipe compiler.\n",
		"# 20261114-trial-m1\n\nSelf-merge: disabled\n",
		"# 20261114-trial-m1\n\nIn this run, a lane merges once pr-polish finishes.\n",
	} {
		write(t, filepath.Join(dir, "OBJECTIVE.md"), negObj)
		write(t, filepath.Join(dir, "clean.brief"), "Note: self-merge is discussed in docs.")
		write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"add self-merge support to the recipe compiler","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"sess-orch","merge":"squash",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high","prompt":""},
{"name":"clean","kind":"codex","model":"gpt-5.6-sol","effort":"high","prompt":""},
{"name":"review","kind":"claude","model":"sonnet","effort":"high","prompt":""}]},
"tasks":[]}`)
		r3, err := Read(dir)
		if err != nil {
			t.Fatal(err)
		}
		wf3, _, _, err := controllerState(r3)
		if err != nil {
			t.Fatal(err)
		}
		if wf3.MergePolicy.Mode != recipe.MergeModeController {
			t.Fatalf("wf3.MergePolicy.Mode = %q for %q, want %q", wf3.MergePolicy.Mode, negObj, recipe.MergeModeController)
		}
		if _, hasReview := wf3.Phase(recipe.PhaseReview); !hasReview {
			t.Fatalf("wf3 dropped review phase for %q: %+v", negObj, wf3.Phases)
		}
		if _, hasIntegrate := wf3.Phase(recipe.PhaseIntegrate); !hasIntegrate {
			t.Fatalf("wf3 missing controller integrate phase for %q: %+v", negObj, wf3.Phases)
		}
	}

	// Self-merge rule variants: "Merge mode: self", "Merge: self", "merge_mode=self",
	// and explicit "Merge rule:" / "## Merge rule" with "without controller".
	for _, tc := range []struct {
		name         string
		objective    string
		wantNoReview bool
	}{
		{
			name:      "Merge mode: self",
			objective: "# 20261114-trial-m1\n\nMerge mode: self\n",
		},
		{
			name:      "Merge: self",
			objective: "# 20261114-trial-m1\n\nMerge: self\n",
		},
		{
			name:      "merge_mode=self",
			objective: "# 20261114-trial-m1\n\nmerge_mode=self\n",
		},
		{
			name:         "explicit merge rule without controller",
			objective:    "# 20261114-trial-m1\n\nMerge rule: a lane merges once pr-polish finishes without controller intervention.\n",
			wantNoReview: true,
		},
		{
			name:         "merge rule markdown section",
			objective:    "# 20261114-trial-m1\n\n## Merge rule\nA lane merges once pr-polish finishes.\n",
			wantNoReview: true,
		},
	} {
		write(t, filepath.Join(dir, "OBJECTIVE.md"), tc.objective)
		rVar, err := Read(dir)
		if err != nil {
			t.Fatalf("%s: Read: %v", tc.name, err)
		}
		wfVar, _, _, err := controllerState(rVar)
		if err != nil {
			t.Fatalf("%s: controllerState: %v", tc.name, err)
		}
		if wfVar.MergePolicy.Mode != recipe.MergeModeSelf {
			t.Fatalf("%s: MergePolicy.Mode = %q, want %q", tc.name, wfVar.MergePolicy.Mode, recipe.MergeModeSelf)
		}
		if _, hasIntegrate := wfVar.Phase(recipe.PhaseIntegrate); hasIntegrate {
			t.Fatalf("%s: unexpected integrate phase: %+v", tc.name, wfVar.Phases)
		}
		if _, hasReview := wfVar.Phase(recipe.PhaseReview); tc.wantNoReview && hasReview {
			t.Fatalf("%s: unexpected review phase: %+v", tc.name, wfVar.Phases)
		}
	}
}

func TestAdoptStartAndHoldIntentParityWithDispatcher(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text      string
		wantStart bool
		wantHold  bool
	}{
		{"yes: clear both hold directives and start M4", true, false},
		{"hold is lifted, proceed with M4", true, false},
		{"do not start M4 yet", false, true},
		{"don't spawn any new lanes", false, true},
		{"pause until I say resume", false, true},
		{"cancel X and don't start Y", false, true},
		{"Why did it say \"start M4\" when I asked to hold?", false, false},
		{"can we start M4 or what is the status?", false, false},
		{"the M4 requirement is unclear", false, false},
	} {
		gotDispStart := dispatcher.OwnerTextAskedToStart(tc.text)
		if gotDispStart != tc.wantStart {
			t.Fatalf("start intent(%q): dispatcher=%v, want %v", tc.text, gotDispStart, tc.wantStart)
		}
		gotDispHold := dispatcher.IsHoldDirectiveText(tc.text)
		if gotDispHold != tc.wantHold {
			t.Fatalf("hold intent(%q): dispatcher=%v, want %v", tc.text, gotDispHold, tc.wantHold)
		}
	}
}

func TestAdoptOldHostMailsZeroDispatcherTurns(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".swarm")
	adoptTime := time.Date(2026, 11, 15, 2, 2, 0, 0, time.UTC)
	dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", adoptTime.Add(-3*time.Hour))
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"trial m1: slim prompts","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"sess-orch","merge":"squash",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high"},{"name":"review","kind":"claude","model":"sonnet","effort":"high"}]},
"tasks":[
{"id":"m1-base","title":"base prompts","branch":"swarm-lane/`+nsRun+`/m1-base","priority":"p1","status":"done","phase":"review","round":1,"session_id":"sess-m1","terminals":{"review":{"1":"term-m1-review"}},"pr":5604,"pr_head":"aaa","approval_sha":"aaa","merge_sha":"mmm","verdict":"pass"},
{"id":"m2-gaps3","title":"gaps 3","branch":"swarm-lane/`+nsRun+`/m2-gaps3","priority":"p1","status":"done","phase":"review","round":1,"session_id":"sess-m2","terminals":{"review":{"1":"term-m2-review"}},"pr":5811,"pr_head":"bbb","approval_sha":"bbb","merge_sha":"nnn","verdict":"pass"}]}
`)
	write(t, filepath.Join(dir, "OBJECTIVE.md"), "# 20261114-trial-m1\n\nGoal: trial m1.\n- [ ] Open: hand interventions 6-7\n\nNext: none. Run DONE 22:25Z — all lanes merged (#5604, #5811). PLAN.md log + post updated. No loop armed.\n")

	var mailPolls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/mail" {
			mailPolls.Add(1)
			_, _ = io.WriteString(w, `{"messages":[
				{"id":"mail-1","lane":"m2-gaps3","question":true,"status":"withdrawn","body":"m2-gaps3 question 1 (withdrawn 04547e4e)","createdAt":"2026-11-14T19:10:00Z"},
				{"id":"mail-2","lane":"m2-gaps3","question":true,"body":"m2-gaps3 question 2 before #5811 merged","createdAt":"2026-11-14T20:15:00Z"},
				{"id":"mail-3","from":"deploy-bot","body":"deploy notice: staging red on commit 111","createdAt":"2026-11-14T21:00:00Z"},
				{"id":"mail-4","from":"deploy-bot","body":"deploy notice: staging red on commit 222","createdAt":"2026-11-14T21:30:00Z"},
				{"id":"mail-5","from":"deploy-bot","body":"deploy notice: staging red on commit 333","createdAt":"2026-11-14T22:00:00Z"},
				{"id":"mail-6","from":"host","body":"unread-mail reminder: check host inbox","createdAt":"2026-11-14T22:20:00Z"}
			],"nextCursor":"cur-6"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	clk := clock.NewFakeClock(adoptTime)
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := lane.NewFakeDriver()
	if err := st.Directives().Add(t.Context(), store.Directive{
		ID:        "hold-stale-1",
		RunID:     nsRun,
		Kind:      store.DirectiveStanding,
		Source:    store.DirectiveSourceOwner,
		Scope:     "run",
		Text:      "Do not start M4 until I confirm",
		Status:    store.DirectiveActive,
		CreatedAt: adoptTime.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	var dispatcherTurns atomic.Int32
	var plannerTurns atomic.Int32
	controllers := NewControllers(t.Context(), st, drv, clk, func(string, func(context.Context, string, string, string) error) controller.Judge {
		return controller.JudgeFunc(func(_ context.Context, _ string, _ []store.Event) error {
			dispatcherTurns.Add(1)
			return nil
		})
	})
	controllers.Planner = func(_ context.Context, _, _, _ string) error {
		plannerTurns.Add(1)
		return nil
	}
	controllers.Mailboxes = func(runID string) []mailbox.Mailbox {
		box, err := mailboxnebula.New(mailboxnebula.Config{
			BaseURL:      srv.URL,
			Token:        "test-token",
			RunID:        runID,
			Events:       st.Events(),
			Host:         true,
			PollInterval: 10 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		return []mailbox.Mailbox{box}
	}

	label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
	s := nsService(root, loop, label)
	s.Now = func() time.Time { return adoptTime }
	s.Driver = controllers
	r, err := s.Adopt(t.Context(), nsRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

	waitFor(t, func() bool { return mailPolls.Load() >= 2 })
	if err := controllers.Step(t.Context(), nsRun); err != nil {
		t.Fatal(err)
	}

	if got := dispatcherTurns.Load(); got != 0 {
		t.Fatalf("dispatcher turns for 6 old host mails on adopt = %d, want 0", got)
	}
	if got := plannerTurns.Load(); got != 0 {
		t.Fatalf("planner turns on done run = %d, want 0", got)
	}
	decs, err := st.Decisions().List(t.Context(), store.DecisionFilter{RunID: nsRun})
	if err != nil {
		t.Fatal(err)
	}
	var summaryCount int
	for _, d := range decs {
		if d.Tool == "mail.adopt_summary" {
			summaryCount++
		}
	}
	if summaryCount != 1 {
		t.Fatalf("mail.adopt_summary decisions = %d, want 1 (decisions=%+v)", summaryCount, decs)
	}
	holdDir, err := st.Directives().Get(t.Context(), nsRun, "hold-stale-1")
	if err != nil {
		t.Fatal(err)
	}
	if holdDir.Status != store.DirectiveExpired {
		t.Fatalf("stale hold directive status on done run = %q, want %q", holdDir.Status, store.DirectiveExpired)
	}

	// Verify that an open question from a live (running) lane and a message with
	// missing createdAt are NOT suppressed by SetAdoptCutoff.
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{
		ID:     "m3-live",
		RunID:  "run-live-adopt",
		Status: lane.StatusRunning,
		Phase:  "swe",
		Round:  1,
	}); err != nil {
		t.Fatal(err)
	}
	var livePolls atomic.Int32
	srvLive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/mail" {
			livePolls.Add(1)
			_, _ = io.WriteString(w, `{"messages":[
				{"id":"mail-live-q","lane":"m3-live","question":true,"body":"open question from running lane before adopt","createdAt":"2026-11-14T23:00:00Z"},
				{"id":"mail-no-ts","from":"host","body":"timestamp-less host message"}
			],"nextCursor":"cur-live"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srvLive.Close()
	boxLive, err := mailboxnebula.New(mailboxnebula.Config{
		BaseURL:      srvLive.URL,
		Token:        "test-token",
		RunID:        "run-live-adopt",
		Events:       st.Events(),
		Host:         true,
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	boxLive.SetAdoptCutoff(adoptTime, st)
	watchCtx, cancelWatch := context.WithCancel(t.Context())
	defer cancelWatch()
	chLive, err := boxLive.Watch(watchCtx)
	if err != nil {
		t.Fatal(err)
	}
	var gotSummaries []string
	for i := 0; i < 2; i++ {
		select {
		case ev := <-chLive:
			if ev.Source == mailbox.AdoptMailSummarySource {
				t.Fatalf("unexpected adopt mail summary event on live question / timestamp-less mail: %+v", ev)
			}
			gotSummaries = append(gotSummaries, ev.Summary)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for live question / timestamp-less mail event %d", i+1)
		}
	}
	joined := strings.Join(gotSummaries, "\n")
	if !strings.Contains(joined, "open question from running lane before adopt") || !strings.Contains(joined, "timestamp-less host message") {
		t.Fatalf("live mail events = %q, want both open question and timestamp-less message", joined)
	}
}

func TestAdoptedNebulaSwarmCloseoutWhenAllLanesTerminal(t *testing.T) {
	t.Parallel()

	t.Run("all_lanes_terminal_on_adopt_writes_final_updates_objective_post_and_releases_sessions", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), ".swarm")
		last := time.Date(2026, 11, 15, 6, 56, 35, 0, time.UTC)
		dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", last)

		write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"finish trial-m1 rollout and publish final post","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"sess-orch","merge":"squash",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high"},{"name":"review","kind":"claude","model":"sonnet","effort":"high"}]},
"tasks":[
{"id":"m1-base","title":"M1 base","branch":"swarm-lane/20261114-trial-m1/m1-base","priority":"p1","status":"done","phase":"review","round":1,"session_id":"sess-m1","terminals":{"review":{"1":"term-m1-review"}},"pr":5604,"pr_head":"abc","approval_sha":"abc","merge_sha":"abc5604","terminal_state":"","verdict":"pass"},
{"id":"m5-finish","title":"M5 finish","branch":"swarm-lane/20261114-trial-m1/m5-finish","priority":"p1","status":"done","phase":"review","round":1,"session_id":"sess-m5","terminals":{"review":{"1":"term-m5-review"}},"pr":5899,"pr_head":"def","approval_sha":"def","merge_sha":"def5899","terminal_state":"","verdict":"pass"}]}
`)
		write(t, filepath.Join(dir, "PLAN.md"), "# Plan\n\n## Log\n- 05:00Z m1-base merged (#5604)\n")
		write(t, filepath.Join(dir, "OBJECTIVE.md"), `# Objective: trial-m1

Goal: finish trial-m1 rollout and publish final post

Plan: PLAN.md (post ac9da4cb)

Next: when #5899 merges, add a line to PLAN.md log, republish the plan post (ac9da4cb), write FINAL.md and update PROGRESS.md / OBJECTIVE.md, and release the lane sessions.
`)
		write(t, filepath.Join(dir, "PROGRESS.md"), `# Progress (05:13Z handover)
Remaining orchestrator steps after #5899 merges:
- check the merge
- add a line to the plan's PLAN.md log
- republish the plan post (ac9da4cb)
- write FINAL.md and update PROGRESS.md / OBJECTIVE.md, and publish them
- release the lane sessions
`)

		clk := clock.NewFakeClock(last)
		st := store.NewMemoryStore(clk)
		t.Cleanup(func() { _ = st.Close() })
		drv := lane.NewFakeDriver()
		controllers := NewControllers(t.Context(), st, drv, clk, nil)
		label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
		s := nsService(root, loop, label)
		s.Driver = controllers

		r, err := s.Adopt(t.Context(), nsRun)
		if err != nil {
			t.Fatalf("Adopt: %v", err)
		}
		t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

		// 1. FINAL.md written with merged PRs and outcome.
		finalBytes, err := os.ReadFile(filepath.Join(dir, "FINAL.md"))
		if err != nil {
			t.Fatalf("expected FINAL.md to be written on all-lanes-terminal closeout: %v", err)
		}
		finalStr := string(finalBytes)
		for _, want := range []string{"Outcome: done", "#5604", "abc5604", "#5899", "def5899"} {
			if !strings.Contains(finalStr, want) {
				t.Fatalf("FINAL.md missing %q:\n%s", want, finalStr)
			}
		}

		// 2. PLAN.md log appended.
		planStr := readText(filepath.Join(dir, "PLAN.md"))
		for _, want := range []string{"#5604", "#5899", "20261114-trial-m1"} {
			if !strings.Contains(planStr, want) {
				t.Fatalf("PLAN.md missing close-out log %q:\n%s", want, planStr)
			}
		}

		// 3. OBJECTIVE.md Next: updated to done (and stand-down marker preserved).
		objRaw := readText(filepath.Join(dir, "OBJECTIVE.md"))
		if !strings.HasPrefix(objRaw, markerPrefix) {
			t.Fatalf("OBJECTIVE.md lost stand-down marker prefix:\n%s", objRaw)
		}
		if !ObjectiveIsDone(stripMarker(objRaw)) {
			t.Fatalf("expected OBJECTIVE.md Next: to be marked done:\n%s", objRaw)
		}
		storedRun, err := st.Runs().Get(t.Context(), nsRun)
		if err != nil {
			t.Fatal(err)
		}
		if !ObjectiveIsDone(storedRun.Objective) {
			t.Fatalf("expected stored Run.Objective to be marked done:\n%s", storedRun.Objective)
		}

		// 4. Fake Nebula client sees the post update with the post ID ac9da4cb.
		if len(drv.PostUpdates) != 1 {
			t.Fatalf("PostUpdates = %+v, want 1 update", drv.PostUpdates)
		}
		if drv.PostUpdates[0].PostID != "ac9da4cb" {
			t.Fatalf("PostUpdates[0].PostID = %q, want ac9da4cb", drv.PostUpdates[0].PostID)
		}
		if !strings.Contains(drv.PostUpdates[0].Content, "#5899") || !strings.Contains(drv.PostUpdates[0].Content, "Outcome: done") {
			t.Fatalf("PostUpdates[0].Content missing merged PR or outcome:\n%s", drv.PostUpdates[0].Content)
		}

		// 5. Lane sessions released (excluding orchestrator session sess-orch).
		if !slices.Contains(drv.Releases, "sess-m1") || !slices.Contains(drv.Releases, "sess-m5") {
			t.Fatalf("Releases = %v, want sess-m1 and sess-m5", drv.Releases)
		}
		if slices.Contains(drv.Releases, "sess-orch") {
			t.Fatalf("Releases = %v, must not release orchestrator session sess-orch", drv.Releases)
		}

		// 6. Owner sees one-line summary decisions for each close-out step and /release offer.
		decs, err := st.Decisions().List(t.Context(), store.DecisionFilter{RunID: nsRun})
		if err != nil {
			t.Fatal(err)
		}
		var summaries []string
		for _, d := range decs {
			if d.Tool == dispatcher.ToolRunCloseout {
				summaries = append(summaries, d.Result)
			}
		}
		joinedDecs := strings.Join(summaries, "\n")
		for _, want := range []string{
			"released lane sessions",
			"PLAN.md",
			"FINAL.md",
			"OBJECTIVE.md Next:",
			"ac9da4cb",
			"/release",
		} {
			if !strings.Contains(joinedDecs, want) {
				t.Fatalf("close-out decisions missing %q:\n%s", want, joinedDecs)
			}
		}

		// 7. Writes outside the run dir or to unnamed files are refused.
		outsideTarget := filepath.Join(t.TempDir(), "escape.md")
		for _, badPath := range []string{
			"../outside.md",
			"../../escape.md",
			outsideTarget,
			"UNLISTED.md",
			"notes/secret.md",
		} {
			if _, err := ValidateCloseoutWrite(dir, stripMarker(objRaw), badPath); err == nil {
				t.Fatalf("ValidateCloseoutWrite(%q) succeeded, want refusal", badPath)
			}
			if _, err := WriteCloseoutFile(dir, stripMarker(objRaw), badPath, "bad", false); err == nil {
				t.Fatalf("WriteCloseoutFile(%q) succeeded, want refusal", badPath)
			}
			if _, err := controllers.CloseoutRun(t.Context(), nsRun, dispatcher.RunCloseoutParams{
				Files: map[string]string{badPath: "bad"},
			}); err == nil {
				t.Fatalf("CloseoutRun with bad file %q succeeded, want refusal", badPath)
			}
		}
		if _, err := os.Stat(outsideTarget); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("outsideTarget %s was created!", outsideTarget)
		}
		if _, err := os.Stat(filepath.Join(dir, "UNLISTED.md")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("UNLISTED.md was created in run dir!")
		}

		// 8. Symlink parent traversal with a nonexistent target file is refused.
		outsideDir := t.TempDir()
		if err := os.Symlink(outsideDir, filepath.Join(dir, "symdir")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		objWithSym := stripMarker(objRaw) + "\nAlso mentions symdir/escape.md and dangling.md."
		if err := os.Symlink(filepath.Join(outsideDir, "dangling-target.md"), filepath.Join(dir, "dangling.md")); err != nil {
			t.Fatalf("Symlink dangling: %v", err)
		}
		for _, symPath := range []string{"symdir/escape.md", "dangling.md"} {
			if _, err := ValidateCloseoutWrite(dir, objWithSym, symPath); err == nil {
				t.Fatalf("ValidateCloseoutWrite(%q) succeeded across symlink escape, want refusal", symPath)
			}
			if _, err := WriteCloseoutFile(dir, objWithSym, symPath, "bad", false); err == nil {
				t.Fatalf("WriteCloseoutFile(%q) succeeded across symlink escape, want refusal", symPath)
			}
		}
		if _, err := os.Stat(filepath.Join(outsideDir, "escape.md")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink escape created %s outside run dir!", filepath.Join(outsideDir, "escape.md"))
		}
	})

	t.Run("post_republish_failure_retries_cleanly_and_is_idempotent", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), ".swarm")
		last := time.Date(2026, 11, 15, 7, 0, 0, 0, time.UTC)
		dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", last)

		write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"finish trial-m1 rollout","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"sess-orch","merge":"squash",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high"},{"name":"review","kind":"claude","model":"sonnet","effort":"high"}]},
"tasks":[
{"id":"m1-base","title":"M1 base","branch":"swarm-lane/20261114-trial-m1/m1-base","priority":"p1","status":"done","phase":"review","round":1,"session_id":"sess-m1","terminals":{"review":{"1":"term-m1-review"}},"pr":5604,"pr_head":"abc","approval_sha":"abc","merge_sha":"abc5604","terminal_state":"","verdict":"pass"},
{"id":"m5-finish","title":"M5 finish","branch":"swarm-lane/20261114-trial-m1/m5-finish","priority":"p1","status":"done","phase":"review","round":1,"session_id":"sess-m5","terminals":{"review":{"1":"term-m5-review"}},"pr":5899,"pr_head":"def","approval_sha":"def","merge_sha":"def5899","terminal_state":"","verdict":"pass"}]}
`)
		write(t, filepath.Join(dir, "PLAN.md"), "# Plan\n\n## Log\n- 05:00Z m1-base merged (#5604)\n")
		// Objective already has Next: Done, while requesting FINAL.md and post republish.
		write(t, filepath.Join(dir, "OBJECTIVE.md"), `# Objective: trial-m1

Goal: finish trial-m1 rollout

Plan: PLAN.md (post ac9da4cb)

Next: Done — write FINAL.md, update PLAN.md log, and republish post ac9da4cb.
Constraints: preserve this line directly after Next.
`)
		write(t, filepath.Join(dir, "PROGRESS.md"), "# Progress\n- m1-base and m5-finish merged.\n")

		var postAttempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && r.URL.Path == "/api/posts/ac9da4cb" {
				attempt := postAttempts.Add(1)
				if attempt == 1 {
					http.Error(w, `{"error":"temporary upstream error"}`, http.StatusBadGateway)
					return
				}
				b, _ := io.ReadAll(r.Body)
				var payload struct {
					Content  string `json:"content"`
					Markdown string `json:"markdown"`
				}
				_ = json.Unmarshal(b, &payload)
				if payload.Content == "" || payload.Content != payload.Markdown {
					http.Error(w, "content/markdown mismatch", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		nebDrv, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "test-token"})
		if err != nil {
			t.Fatal(err)
		}
		fakeDrv := lane.NewFakeDriver()
		drv := &nebulaPostFakeDriver{FakeDriver: fakeDrv, pub: nebDrv}
		clk := clock.NewFakeClock(last)
		st := store.NewMemoryStore(clk)
		t.Cleanup(func() { _ = st.Close() })
		controllers := NewControllers(t.Context(), st, drv, clk, nil)
		label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
		s := nsService(root, loop, label)
		s.Driver = controllers

		// Adopt attempts auto close-out; attempt 1 fails at UpdatePost.
		r, err := s.Adopt(t.Context(), nsRun)
		if err != nil {
			t.Fatalf("Adopt: %v", err)
		}
		t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

		if postAttempts.Load() != 1 {
			t.Fatalf("postAttempts after first attempt = %d, want 1", postAttempts.Load())
		}
		if fileExists(filepath.Join(dir, "FINAL.md")) {
			t.Fatalf("FINAL.md must not be written when UpdatePost fails")
		}

		// Advance clock and retry via planNextWorkIfDrained: attempt 2 succeeds.
		clk.Advance(2 * time.Minute)
		closed, err := controllers.planNextWorkIfDrained(t.Context(), nsRun, "retry")
		if err != nil || !closed {
			t.Fatalf("retry planNextWorkIfDrained = (%v, %v), want (true, nil)", closed, err)
		}
		if postAttempts.Load() != 2 {
			t.Fatalf("postAttempts after retry = %d, want 2", postAttempts.Load())
		}
		if !fileExists(filepath.Join(dir, "FINAL.md")) {
			t.Fatalf("expected FINAL.md after retry succeeded")
		}

		// Re-running CloseoutRun again with a later clock must be idempotent (no duplicate PLAN.md or PROGRESS.md entries).
		clk.Advance(5 * time.Minute)
		if _, err := controllers.CloseoutRun(t.Context(), nsRun, dispatcher.RunCloseoutParams{}); err != nil {
			t.Fatalf("second CloseoutRun: %v", err)
		}
		planStr := readText(filepath.Join(dir, "PLAN.md"))
		if count := strings.Count(planStr, "20261114-trial-m1 done: merged #5604, #5899"); count != 1 {
			t.Fatalf("PLAN.md close-out log count = %d, want 1:\n%s", count, planStr)
		}
		progStr := readText(filepath.Join(dir, "PROGRESS.md"))
		if count := strings.Count(progStr, "## Close-out ("); count != 1 {
			t.Fatalf("PROGRESS.md Close-out section count = %d, want 1:\n%s", count, progStr)
		}
		objStr := stripMarker(readText(filepath.Join(dir, "OBJECTIVE.md")))
		if !strings.Contains(objStr, "Constraints: preserve this line directly after Next.") {
			t.Fatalf("OBJECTIVE.md lost line directly following Next:\n%s", objStr)
		}
	})

	t.Run("failed_lane_and_failed_session_release_report_partial_and_unmerged", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), ".swarm")
		last := time.Date(2026, 11, 15, 7, 10, 0, 0, time.UTC)
		dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", last)

		write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"trial partial closeout","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"sess-orch","merge":"squash",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high"}]},
"tasks":[
{"id":"m1-base","title":"M1 base","branch":"swarm-lane/20261114-trial-m1/m1-base","priority":"p1","status":"done","phase":"swe","round":1,"session_id":"sess-m1","terminals":{"swe":{"1":"term-m1"}},"pr":5604,"pr_head":"abc","approval_sha":"abc","merge_sha":"abc5604","terminal_state":"","verdict":"pass"},
{"id":"m2-broken","title":"M2 broken","branch":"swarm-lane/20261114-trial-m1/m2-broken","priority":"p1","status":"failed","phase":"swe","round":1,"session_id":"sess-m2","terminals":{"swe":{"1":"term-m2"}},"pr":5900,"pr_head":"bad","approval_sha":"","merge_sha":"","terminal_state":"","verdict":""}]}
`)
		write(t, filepath.Join(dir, "PLAN.md"), "# Plan\n")
		write(t, filepath.Join(dir, "OBJECTIVE.md"), "Goal: trial partial closeout\nPlan: PLAN.md\nNext: close out run and write FINAL.md\n")

		clk := clock.NewFakeClock(last)
		st := store.NewMemoryStore(clk)
		t.Cleanup(func() { _ = st.Close() })
		drv := &failingReleaseDriver{FakeDriver: lane.NewFakeDriver(), failSession: "sess-m2"}
		controllers := NewControllers(t.Context(), st, drv, clk, nil)
		label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
		s := nsService(root, loop, label)
		s.Driver = controllers

		r, err := s.Adopt(t.Context(), nsRun)
		if err != nil {
			t.Fatalf("Adopt: %v", err)
		}
		t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

		finalStr := readText(filepath.Join(dir, "FINAL.md"))
		if !strings.Contains(finalStr, "Outcome: partial") {
			t.Fatalf("FINAL.md should report Outcome: partial when a lane failed:\n%s", finalStr)
		}
		if !strings.Contains(finalStr, "m2-broken: failed (#5900, unmerged)") {
			t.Fatalf("FINAL.md should mark failed lane PR #5900 as unmerged:\n%s", finalStr)
		}
		if !strings.Contains(finalStr, "1 session(s) failed to release: sess-m2") {
			t.Fatalf("FINAL.md should list unreleased sessions:\n%s", finalStr)
		}
		planStr := readText(filepath.Join(dir, "PLAN.md"))
		if strings.Contains(planStr, "#5900") {
			t.Fatalf("PLAN.md must not list unmerged PR #5900 as merged:\n%s", planStr)
		}
		decs, err := st.Decisions().List(t.Context(), store.DecisionFilter{RunID: nsRun})
		if err != nil {
			t.Fatal(err)
		}
		var summaries []string
		for _, d := range decs {
			if d.Tool == dispatcher.ToolRunCloseout {
				summaries = append(summaries, d.Result)
			}
		}
		joined := strings.Join(summaries, "\n")
		if !strings.Contains(joined, "released lane sessions (sess-m1); failed to release (sess-m2)") {
			t.Fatalf("decisions should report actual released and failed sessions, got:\n%s", joined)
		}
	})

	t.Run("last_lane_merges_while_adopted_closes_out_via_nebula_client", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), ".swarm")
		last := time.Date(2026, 11, 15, 6, 50, 0, 0, time.UTC)
		dir := writeNebulaSwarmRun(t, root, nsRun, "sess-orch", last)

		write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"finish trial-m1 rollout and publish final post","base":"main","target":"main","repo":"repo-1","orchestrator_session_id":"sess-orch","merge":"squash",
"phases":[{"name":"swe","kind":"claude","model":"opus","effort":"high"},{"name":"review","kind":"claude","model":"sonnet","effort":"high"}]},
"tasks":[
{"id":"m1-base","title":"M1 base","branch":"swarm-lane/20261114-trial-m1/m1-base","priority":"p1","status":"done","phase":"review","round":1,"session_id":"sess-m1","terminals":{"review":{"1":"term-m1-review"}},"pr":5604,"pr_head":"abc","approval_sha":"abc","merge_sha":"abc5604","terminal_state":"","verdict":"pass"},
{"id":"m5-finish","title":"M5 finish","branch":"swarm-lane/20261114-trial-m1/m5-finish","priority":"p1","status":"running","phase":"review","round":1,"session_id":"sess-m5","terminals":{"review":{"1":"term-m5-review"}},"pr":5899,"pr_head":"def","approval_sha":"","merge_sha":"","terminal_state":"working","verdict":""}]}
`)
		write(t, filepath.Join(dir, "PLAN.md"), "# Plan\n\n## Log\n- 05:00Z m1-base merged (#5604)\n")
		write(t, filepath.Join(dir, "OBJECTIVE.md"), `# Objective: trial-m1

Goal: finish trial-m1 rollout and publish final post

Plan: PLAN.md (post ac9da4cb)

Next: when #5899 merges, add a line to PLAN.md log, republish the plan post (ac9da4cb), write FINAL.md and update PROGRESS.md / OBJECTIVE.md, and release the lane sessions.
`)
		write(t, filepath.Join(dir, "PROGRESS.md"), `# Progress (05:13Z handover)
Remaining orchestrator steps after #5899 merges:
- check the merge
- add a line to the plan's PLAN.md log
- republish the plan post (ac9da4cb)
- write FINAL.md and update PROGRESS.md / OBJECTIVE.md, and publish them
- release the lane sessions
`)

		var postIDGot, postBodyGot string
		var srvMu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/posts/") {
				b, _ := io.ReadAll(r.Body)
				var payload struct {
					Content string `json:"content"`
				}
				_ = json.Unmarshal(b, &payload)
				srvMu.Lock()
				postIDGot = strings.TrimPrefix(r.URL.Path, "/api/posts/")
				postBodyGot = payload.Content
				srvMu.Unlock()
				_, _ = io.WriteString(w, `{"ok":true}`)
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		nebDrv, err := nebula.New(nebula.Config{
			APIURL: srv.URL,
			Token:  "test-token",
		})
		if err != nil {
			t.Fatal(err)
		}
		fakeDrv := lane.NewFakeDriver()
		drv := &nebulaPostFakeDriver{FakeDriver: fakeDrv, pub: nebDrv}

		clk := clock.NewFakeClock(last)
		st := store.NewMemoryStore(clk)
		t.Cleanup(func() { _ = st.Close() })
		controllers := NewControllers(t.Context(), st, drv, clk, nil)
		controllers.PRProbe = func(_ context.Context, _, _ string, pr int) (controller.PRStatus, error) {
			if pr == 5899 {
				return controller.PRStatus{
					State:    "MERGED",
					MergeSHA: "def5899",
					Number:   5899,
				}, nil
			}
			return controller.PRStatus{State: "OPEN", Number: pr}, nil
		}
		label, loop := &fakeLabeler{label: "claude"}, &fakeWatcher{running: false}
		s := nsService(root, loop, label)
		s.Driver = controllers

		r, err := s.Adopt(t.Context(), nsRun)
		if err != nil {
			t.Fatalf("Adopt: %v", err)
		}
		t.Cleanup(func() { _ = controllers.Stop(context.Background(), r) })

		// Before m5-finish completes, FINAL.md must not exist yet.
		if _, err := os.Stat(filepath.Join(dir, "FINAL.md")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("FINAL.md should not exist while m5-finish is still running")
		}

		// Now m5-finish writes its review pass report and PRProbe sees #5899 merged.
		write(t, filepath.Join(dir, "reports", "m5-finish.review.r1.md"), "Outcome: done\nHEAD: def\nPR: 5899\nVerdict: pass\n")
		fakeDrv.Emit(lane.Event{
			RunID:      nsRun,
			LaneID:     "m5-finish",
			TerminalID: "term-m5-review",
			Kind:       lane.EventTurnEndedNoReport,
			Text:       "review passed and PR #5899 merged",
		})
		waitFor(t, func() bool {
			return fileExists(filepath.Join(dir, "FINAL.md"))
		})

		finalStr := readText(filepath.Join(dir, "FINAL.md"))
		if !strings.Contains(finalStr, "#5899") || !strings.Contains(finalStr, "Outcome: done") {
			t.Fatalf("FINAL.md after dynamic last-lane merge:\n%s", finalStr)
		}
		srvMu.Lock()
		gotPostID := postIDGot
		gotPostBody := postBodyGot
		srvMu.Unlock()
		if gotPostID != "ac9da4cb" || !strings.Contains(gotPostBody, "#5899") {
			t.Fatalf("nebula post update = (%q, %q), want ac9da4cb with #5899", gotPostID, gotPostBody)
		}
		if !slices.Contains(fakeDrv.Releases, "sess-m1") || !slices.Contains(fakeDrv.Releases, "sess-m5") {
			t.Fatalf("released sessions = %v, want sess-m1 and sess-m5", fakeDrv.Releases)
		}
	})
}

type nebulaPostFakeDriver struct {
	*lane.FakeDriver
	pub lane.PostPublisher
}

func (d *nebulaPostFakeDriver) UpdatePost(ctx context.Context, postID, content string) error {
	return d.pub.UpdatePost(ctx, postID, content)
}

type failingReleaseDriver struct {
	*lane.FakeDriver
	failSession string
}

func (d *failingReleaseDriver) ReleaseSession(ctx context.Context, sessionID string, force ...bool) error {
	if sessionID == d.failSession {
		return errors.New("simulated release error")
	}
	return d.FakeDriver.ReleaseSession(ctx, sessionID, force...)
}

func TestCloseoutUnitHelpers(t *testing.T) {
	t.Parallel()

	t.Run("replaceObjectiveNextLine_preserves_siblings_and_earlier_Next", func(t *testing.T) {
		t.Parallel()
		obj := "# Objective\nNext: earlier note\n\nNext: finish m5\n  - bullet child\nPlan: PLAN.md\nOwner: alice\n"
		got := replaceObjectiveNextLine(obj, "done (FINAL.md written; run /release)")
		if !strings.Contains(got, "Next: earlier note") {
			t.Fatalf("earlier Next: line lost:\n%s", got)
		}
		if !strings.Contains(got, "Next: done (FINAL.md written; run /release)") {
			t.Fatalf("last Next: line not replaced:\n%s", got)
		}
		if strings.Contains(got, "bullet child") {
			t.Fatalf("indented child of replaced Next: should be removed:\n%s", got)
		}
		if !strings.Contains(got, "Plan: PLAN.md") || !strings.Contains(got, "Owner: alice") {
			t.Fatalf("non-indented sibling lines after Next: must be preserved:\n%s", got)
		}

		headingObj := "# Objective\n\n## Next\n- step 1\n- step 2\n\n## Milestones\n- [x] M1\n"
		gotHeading := replaceObjectiveNextLine(headingObj, "done")
		if !strings.Contains(gotHeading, "Next: done\n") || !strings.Contains(gotHeading, "## Milestones\n- [x] M1") || strings.Contains(gotHeading, "step 1") {
			t.Fatalf("## Next section replacement failed:\n%s", gotHeading)
		}
	})

	t.Run("shouldAutoCloseoutRun_negative_guards", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		doneLane := []lane.Lane{{ID: "m1", Status: lane.StatusDone}}

		// Stray mention in PROGRESS.md history while Next: has non-closeout work.
		objOtherWork := "Goal: test\nNext: spawn m2-api lane\n\n## Handover (PROGRESS.md)\nPreviously wrote FINAL.md in trial-0.\n"
		if shouldAutoCloseoutRun(Run{ID: "r1", Dir: dir, Objective: objOtherWork}, doneLane, nil) {
			t.Fatal("shouldAutoCloseoutRun must be false when Next: does not call for closeout")
		}

		// Unchecked milestone - [ ] blocks auto-closeout.
		objUnchecked := "Goal: test\n- [x] M1\n- [ ] M2\nNext: write FINAL.md\n"
		if shouldAutoCloseoutRun(Run{ID: "r1", Dir: dir, Objective: objUnchecked}, doneLane, nil) {
			t.Fatal("shouldAutoCloseoutRun must be false when an unchecked milestone remains")
		}

		// Explicit owner decision gate blocks auto-closeout.
		objAskOwner := "Goal: test\nNext: ask owner before writing FINAL.md\n"
		if shouldAutoCloseoutRun(Run{ID: "r1", Dir: dir, Objective: objAskOwner}, doneLane, nil) {
			t.Fatal("shouldAutoCloseoutRun must be false when Next: says ask owner")
		}

		// Positive control: when Next: calls for writing FINAL.md and all milestones are checked, it returns true.
		objReady := "Goal: test\n- [x] M1\nNext: write FINAL.md\n"
		if !shouldAutoCloseoutRun(Run{ID: "r1", Dir: dir, Objective: objReady}, doneLane, nil) {
			t.Fatal("shouldAutoCloseoutRun must be true when Next: calls for FINAL.md and all lanes/milestones are done")
		}

		// Historical HANDOVER.md / PROGRESS.md mention when Next: is done must not trigger auto-closeout.
		write(t, filepath.Join(dir, "HANDOVER.md"), "# Notes\nPreviously wrote FINAL.md and republished post in trial-0.\n")
		objDoneNoHandover := "Goal: test\nNext: none\n"
		progHistorical := "# Handover\n- Previously republished FINAL.md in trial-0\n"
		if shouldAutoCloseoutRun(Run{ID: "r1", Dir: dir, Objective: objDoneNoHandover, Progress: progHistorical}, doneLane, nil) {
			t.Fatal("shouldAutoCloseoutRun must be false for historical HANDOVER.md / PROGRESS.md mentions when Next: is none")
		}
	})

	t.Run("WriteCloseoutFile_preserves_stand_down_marker_in_PROGRESS", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		progPath := filepath.Join(dir, "PROGRESS.md")
		write(t, progPath, markerPrefix+"2026-11-15T05:13:00Z (orca-1). Claude sessions: do not act on this run; run /release in orca to hand back.\n# Progress\n- 05:00Z lane m1 done\n")
		if _, err := WriteCloseoutFile(dir, "Goal: test\n", "PROGRESS.md", "# Progress\n- 06:56Z closed out\n", false); err != nil {
			t.Fatalf("WriteCloseoutFile: %v", err)
		}
		got := readText(progPath)
		if !strings.HasPrefix(got, markerPrefix) || !strings.Contains(got, "- 06:56Z closed out") {
			t.Fatalf("WriteCloseoutFile must preserve stand-down marker header on overwrite:\n%s", got)
		}
	})

	t.Run("findPostIDInText_and_nebula_UpdatePost_validation", func(t *testing.T) {
		t.Parallel()
		if got := findPostIDInText("Plan: PLAN.md\nNext: write FINAL.md"); got != "" {
			t.Fatalf("findPostIDInText without post id = %q, want empty", got)
		}
		if got := findPostIDInText("Plan: PLAN.md (post ac9da4cb-1234-5678-9abc-def012345678)"); got != "ac9da4cb-1234-5678-9abc-def012345678" {
			t.Fatalf("findPostIDInText UUID = %q", got)
		}

		nebDrv, err := nebula.New(nebula.Config{
			APIURL: "http://127.0.0.1:1",
			Token:  "test-token",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := nebDrv.UpdatePost(t.Context(), "../evil", "body"); err == nil {
			t.Fatal("expected UpdatePost to reject path traversal in postID")
		}
		if err := nebDrv.UpdatePost(t.Context(), "   ", "body"); err == nil {
			t.Fatal("expected UpdatePost to reject blank postID")
		}
	})
}
