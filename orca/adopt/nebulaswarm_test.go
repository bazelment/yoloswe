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
	"strings"
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

func TestLaneMergeNotePRsRequiresOwnMergedPhrase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "EVENTS")
	write(t, path,
		"2026-11-16T01:00:00Z\tlane-1\torch-note\t#7 MERGED to main abc1234\n"+
			"2026-11-16T01:01:00Z\tlane-1\tdecision\town lane #8: PR merged into main\n"+
			"2026-11-16T01:02:00Z\tlane-1\tdecision\tblocked until #12 is merged\n"+
			"2026-11-16T01:03:00Z\tlane-1\tdecision\trebased onto #30 (merged)\n"+"2026-11-16T01:04:00Z\tlane-1\tdecision\twill be merged once checks pass\n"+
			"2026-11-16T01:05:00Z\tlane-1\tdecision\town lane #9: PR is OPEN, not merged\n")
	got := laneMergeNotePRs(path)
	want := map[string][]string{"lane-1": {"7", "8"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("laneMergeNotePRs() = %#v, want %#v", got, want)
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
	if _, hasReview := workflow.Phase(recipe.PhaseReview); !hasReview {
		t.Fatalf("adopted recipe where a lane merges once pr-polish finishes must retain the declared review phase: phases=%+v", workflow.Phases)
	}
	cleanPhase, hasClean := workflow.Phase(recipe.PhaseClean)
	if !hasClean || !strings.Contains(cleanPhase.BriefTemplate, "/simplify-branch") {
		t.Fatalf("clean phase = %+v, want /simplify-branch brief", cleanPhase)
	}
	mech, _ := workflow.MatchTransitions(recipe.PhaseClean, recipe.OutcomeDone, false, 2)
	if mech == nil || mech.To != recipe.PhaseReview {
		t.Fatalf("clean -> done transition = %+v, want To=%q", mech, recipe.PhaseReview)
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
		if _, hasIntegrate := wf3.Phase(recipe.PhaseIntegrate); hasIntegrate {
			t.Fatalf("wf3 invented controller integrate phase for %q: %+v", negObj, wf3.Phases)
		}
	}

	// Self-merge rule variants: "Merge mode: self", "Merge: self", "merge_mode=self",
	// and explicit "Merge rule:" / "## Merge rule" with "without controller".
	for _, tc := range []struct {
		name      string
		objective string
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
			name:      "explicit merge rule without controller",
			objective: "# 20261114-trial-m1\n\nMerge rule: a lane merges once pr-polish finishes without controller intervention.\n",
		},
		{
			name:      "merge rule markdown section",
			objective: "# 20261114-trial-m1\n\n## Merge rule\nA lane merges once pr-polish finishes.\n",
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
		if _, hasReview := wfVar.Phase(recipe.PhaseReview); !hasReview {
			t.Fatalf("%s: missing declared review phase: %+v", tc.name, wfVar.Phases)
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
	controllers := NewControllers(t.Context(), st, drv, clk, func(string) controller.Judge {
		return controller.JudgeFunc(func(_ context.Context, _ string, _ []store.Event) error {
			dispatcherTurns.Add(1)
			return nil
		})
	})
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

func TestCloseoutUnitHelpers(t *testing.T) {
	t.Parallel()

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
		if got := findPostIDInText("İ\nPost: ac9da4cb"); got != "ac9da4cb" {
			t.Fatalf("findPostIDInText after non-ASCII rune = %q, want ac9da4cb", got)
		}
		if got := findPostIDInText("- Merged #5899 (def58990) and republish plan post (ac9da4cb)"); got != "ac9da4cb" {
			t.Fatalf("findPostIDInText with earlier merge SHA = %q, want ac9da4cb", got)
		}
		if got := findPostIDInText("republish the plan; m1 merged abc5604"); got != "" {
			t.Fatalf("findPostIDInText matched merge SHA without post marker = %q, want empty", got)
		}

		var reqCount atomic.Int32
		var gotMethod, gotPath, gotAuth, gotMarkdown string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqCount.Add(1)
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			var payload struct {
				Content  string `json:"content"`
				Markdown string `json:"markdown"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			gotMarkdown = payload.Markdown
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		nebDrv, err := nebula.New(nebula.Config{
			APIURL: srv.URL,
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
		if reqCount.Load() != 0 {
			t.Fatalf("expected 0 HTTP requests for rejected postIDs, got %d", reqCount.Load())
		}
		if err := nebDrv.UpdatePost(t.Context(), "ac9da4cb", "# Updated Plan"); err != nil {
			t.Fatalf("UpdatePost(ac9da4cb): %v", err)
		}
		if gotMethod != http.MethodPut || gotPath != "/api/posts/ac9da4cb" || gotAuth != "Bearer test-token" || gotMarkdown != "# Updated Plan" {
			t.Fatalf("UpdatePost request = (%s %s auth=%q md=%q)", gotMethod, gotPath, gotAuth, gotMarkdown)
		}
	})

}

func TestSubagentSwarmLocalBareRepoResolution(t *testing.T) {
	t.Parallel()

	t.Run("local_bare_origin_remote_resolves_slug_when_env_sh_and_store_absent", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		runID := "20261115-diskless-sandbox"
		dir := filepath.Join(root, ".shared", "subagent-swarm", runID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		barePath := filepath.Join(root, "nebula", ".bare")
		if err := os.MkdirAll(barePath, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(barePath, "config"), "[remote \"origin\"]\n\turl = git@github.com:sycamore-labs/nebula.git\n")
		write(t, filepath.Join(dir, "state.json"), fmt.Sprintf(`{
			"config": {
				"goal": "diskless sandbox",
				"repo": "",
				"phases": [{"name": "swe"}]
			},
			"tasks": [
				{"id": "mem-overhead", "title": "mem overhead", "phase": "swe", "status": "planned", "brief":"Synthetic funded mission", "round": 1, "repo_url": %q, "sessions": {}}
			]
		}`, barePath))
		write(t, filepath.Join(dir, "OBJECTIVE.md"), "Goal: diskless sandbox\n")

		r, err := Read(dir)
		if err != nil {
			t.Fatalf("Read(%s): %v", dir, err)
		}
		if r.Repo != "sycamore-labs/nebula" || r.RepoID != "" {
			t.Fatalf("Read(%s) = Repo=%q RepoID=%q, want Repo=sycamore-labs/nebula RepoID=\"\"", dir, r.Repo, r.RepoID)
		}
	})

}
