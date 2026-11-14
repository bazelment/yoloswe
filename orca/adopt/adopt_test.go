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
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/recipe"
)

const testRun = "20261113-000000-host"

var errInjected = errors.New("injected")

type fakeLabeler struct {
	failSet error
	label   string
	mu      sync.Mutex
}

func (f *fakeLabeler) RunOrchestrator(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.label, nil
}

func (f *fakeLabeler) SetRunOrchestrator(_ context.Context, _ string, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSet != nil {
		return f.failSet
	}
	f.label = label
	return nil
}

// fakeWatcher models one host watcher process. A failing Stop still kills it,
// the harder case for an undo.
type fakeWatcher struct {
	failStop, failStart error
	mu                  sync.Mutex
	running             bool
}

func (f *fakeWatcher) Running(context.Context, string) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return []int{4242}, nil
	}
	return nil, nil
}

func (f *fakeWatcher) Stop(context.Context, []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
	return f.failStop
}

func (f *fakeWatcher) Start(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStart != nil {
		return f.failStart
	}
	f.running = true
	return nil
}

type world struct {
	label   *fakeLabeler
	watcher *fakeWatcher
	root    string
	dir     string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".shared", "subagent-swarm", testRun)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"host goal","phases":[{"name":"swe"}]},"tasks":[{"id":"a","title":"A","phase":"swe","status":"running","round":1,"sessions":{}}]}`)
	write(t, filepath.Join(dir, "OBJECTIVE.md"), "objective\n")
	// PROGRESS.md is absent: the handover must create and later remove it.
	return &world{root: root, dir: dir, label: &fakeLabeler{label: "claude"}, watcher: &fakeWatcher{running: true}}
}

// service is a fresh chat process: nothing carries over but the holder id.
func (w *world) service() *Service {
	return &Service{
		Importer: Importer{Roots: []string{w.root}},
		Labeler:  w.label,
		Watcher:  w.watcher,
		Holder:   ChatHolder("chat-1"),
		Now:      func() time.Time { return time.Date(2026, 11, 13, 1, 2, 3, 0, time.UTC) },
	}
}

// snapshot is everything the handover may change: every file in the run
// directory byte for byte, the Nebula label and whether a watcher runs.
type snapshot struct {
	files   map[string]string
	label   string
	running bool
}

func (w *world) snapshot(t *testing.T) snapshot {
	t.Helper()
	ents, err := os.ReadDir(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	s := snapshot{files: map[string]string{}, label: w.label.label, running: w.watcher.running}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(w.dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s.files[e.Name()] = string(b)
	}
	return s
}

func assertSame(t *testing.T, what string, got, want snapshot) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s changed state:\n got %+v\nwant %+v", what, got, want)
	}
}

func assertAdopted(t *testing.T, w *world) {
	t.Helper()
	s := w.snapshot(t)
	if s.label != "orca" || s.running {
		t.Fatalf("adopted label=%q watcher running=%v", s.label, s.running)
	}
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		if !strings.HasPrefix(s.files[n], "ORCHESTRATOR: orca since 2026-11-13T01:02:03Z (orca-chat:chat-1)") {
			t.Fatalf("%s lacks the stand-down line: %q", n, s.files[n])
		}
	}
	if _, ok := s.files[leaseFile]; !ok {
		t.Fatal("lease missing")
	}
}

func TestRestartBetweenAdoptAndRelease(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	before := w.snapshot(t)
	r, err := w.service().Adopt(t.Context(), testRun)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != testRun || len(r.Lanes) != 1 {
		t.Fatalf("imported %+v", r)
	}
	assertAdopted(t, w)

	// The chat restarts: a new process knows only its holder id.
	restarted := w.service()
	ids, err := restarted.Adopted()
	if err != nil || !reflect.DeepEqual(ids, []string{testRun}) {
		t.Fatalf("Adopted() = %v, %v", ids, err)
	}
	other := w.service()
	other.Holder = ChatHolder("chat-2")
	adopted := w.snapshot(t)
	if _, err := other.Adopt(t.Context(), testRun); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("other holder adopt = %v", err)
	}
	if err := other.Release(t.Context(), testRun); !errors.Is(err, ErrLeaseNotHeld) {
		t.Fatalf("other holder release = %v", err)
	}
	assertSame(t, "other holder", w.snapshot(t), adopted)

	if err := restarted.Release(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	assertSame(t, "adopt then release across a restart", w.snapshot(t), before)
}

func TestAdoptFailureAtEachStepLeavesPriorState(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"import", "marker", "label", "watcher"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			s := w.service()
			switch step {
			case "label":
				w.label.failSet = errInjected
			case "watcher":
				w.watcher.failStop = errInjected
			default:
				s.fault = func(got string) error {
					if got == "adopt:"+step {
						return errInjected
					}
					return nil
				}
			}
			before := w.snapshot(t)
			if _, err := s.Adopt(t.Context(), testRun); !errors.Is(err, errInjected) {
				t.Fatalf("adopt = %v, want injected failure", err)
			}
			assertSame(t, "failed adopt at "+step, w.snapshot(t), before)
		})
	}
}

func TestReleaseFailureAtEachStepLeavesAdoptedState(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"import", "marker", "label", "watcher"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			original := w.snapshot(t)
			s := w.service()
			if _, err := s.Adopt(t.Context(), testRun); err != nil {
				t.Fatal(err)
			}
			switch step {
			case "label":
				w.label.failSet = errInjected
			case "watcher":
				w.watcher.failStart = errInjected
			default:
				s.fault = func(got string) error {
					if got == "release:"+step {
						return errInjected
					}
					return nil
				}
			}
			adopted := w.snapshot(t)
			if err := s.Release(t.Context(), testRun); !errors.Is(err, errInjected) {
				t.Fatalf("release = %v, want injected failure", err)
			}
			assertSame(t, "failed release at "+step, w.snapshot(t), adopted)

			w.label.failSet, w.watcher.failStart, s.fault = nil, nil, nil
			if err := s.Release(t.Context(), testRun); err != nil {
				t.Fatal(err)
			}
			assertSame(t, "release after the failure", w.snapshot(t), original)
		})
	}
}

// A crash leaves the journal mid-protocol; rerunning the command finishes it.
func TestRerunAfterCrashFinishesHandover(t *testing.T) {
	t.Parallel()
	crashAt := func(s *Service, step string) {
		s.fault = func(got string) error {
			if got == step {
				panic("crash")
			}
			return nil
		}
		defer func() { _ = recover() }()
		if strings.HasPrefix(step, "adopt:") {
			_, _ = s.Adopt(context.Background(), testRun)
		} else {
			_ = s.Release(context.Background(), testRun)
		}
	}
	w := newWorld(t)
	original := w.snapshot(t)
	crashAt(w.service(), "adopt:watcher")
	if _, err := w.service().Adopt(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	assertAdopted(t, w)
	crashAt(w.service(), "release:marker")
	if err := w.service().Release(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	assertSame(t, "crash and rerun", w.snapshot(t), original)
}

func TestLeaseHeldByAnotherHolderChangesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	write(t, filepath.Join(w.dir, leaseFile), `{"holder":"orca-chat:other","state":"adopted"}`+"\n")
	before := w.snapshot(t)
	if _, err := w.service().Adopt(t.Context(), testRun); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("adopt = %v", err)
	}
	assertSame(t, "refused adopt", w.snapshot(t), before)
}

func TestImportsCurrentRoundOfTwoLaneRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"g","phases":[{"name":"swe"},{"name":"review"}]},"tasks":[
{"id":"a","title":"A","branch":"ba","phase":"swe","status":"running","round":2,"backend":"nebula","sessions":{"swe":"nebula:term-a1","review":"nebula:term-ar1","swe2":"nebula:term-a2"},"nebula_session":"sess-a2","nebula_terminal":"term-a2"},
{"id":"b","title":"B","branch":"bb","phase":"review","status":"running","round":2,"pr":22,"sessions":{"review":"nebula:term-br1"}}]}`)
	base := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	files := []struct {
		name, body string
		age        int
	}{
		{"a.swe.spawn.json", `{"session_id":"sess-a2","terminal_id":"term-a2"}`, 0},
		{"a.review.spawn.json", `{"session_id":"sess-a1","terminal_id":"term-ar1"}`, 2},
		{"a.swe.md", "Outcome: done\nPR: 11\n", 0},
		{"a.swe.md.prev-20261112T000000Z", "Outcome: needs-swe\nPR: 9\n", 3},
		{"b.review.spawn.json", `{"session_id":"sess-b2","terminal_id":"term-b2"}`, 0},
		{"b.swe.spawn.json", `{"session_id":"sess-b1","terminal_id":"term-b1"}`, 3},
		{"b.review.md.prev-20261112T000000Z", "Outcome: needs-swe\nPR: 22\n", 2},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		write(t, p, f.body)
		mt := base.Add(-time.Duration(f.age) * time.Hour)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Lane{
		{ID: "a", Title: "A", Branch: "ba", Phase: "swe", Status: "running", Round: 2, Session: "sess-a2", Terminal: "term-a2", ReportPath: filepath.Join(dir, "a.swe.md"), PR: "11", Report: "Outcome: done", Next: "next phase: review"},
		{ID: "b", Title: "B", Branch: "bb", Phase: "review", Status: "running", Round: 2, Session: "sess-b2", Terminal: "term-b2", PR: "22", Next: "next: merge"},
	}
	if !reflect.DeepEqual(r.Lanes, want) {
		t.Fatalf("lanes:\n got %+v\nwant %+v", r.Lanes, want)
	}
}

func TestControllerStateRejectsBriefOutsideRunDirectory(t *testing.T) {
	t.Parallel()
	for _, brief := range []string{"../outside.md", "/tmp/outside.md"} {
		t.Run(brief, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "outside.md"), []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "state.json"), fmt.Sprintf(`{"config":{"goal":"g"},"tasks":[{"id":"a","title":"A","phase":"swe","status":"running","brief":%q}]}`, brief))
			r, err := Read(dir)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = controllerState(r)
			if err == nil || !strings.Contains(err.Error(), "brief path") || !strings.Contains(err.Error(), "run directory") {
				t.Fatalf("controllerState error = %v, want a clear run-directory brief-path error", err)
			}
		})
	}
}

func TestControllerStateRejectsLedgerPathComponents(t *testing.T) {
	t.Parallel()
	for _, task := range []string{`{"id":"../outside","phase":"swe"}`, `{"id":"a","phase":"../outside"}`} {
		t.Run(task, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			write(t, filepath.Join(dir, "state.json"), fmt.Sprintf(`{"config":{"goal":"g"},"tasks":[%s]}`, task))
			r, err := Read(dir)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = controllerState(r)
			if err == nil || !strings.Contains(err.Error(), "single path component") {
				t.Fatalf("controllerState error = %v, want path-component rejection", err)
			}
		})
	}
}

func TestAdoptedReviewRoundCapMatchesDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"g","phases":[{"name":"swe"},{"name":"review"}]},"tasks":[{"id":"a","title":"A","phase":"review","status":"running","round":3}]}`)
	r, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	workflow, lanes, _, err := controllerState(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(lanes) != 1 || lanes[0].Round != 3 {
		t.Fatalf("imported lanes = %+v, want review round 3", lanes)
	}
	mechanical, judgment := workflow.MatchTransitions(recipe.PhaseReview, recipe.OutcomeNeedsSWE, false, lanes[0].Round)
	if mechanical == nil || mechanical.To != recipe.TargetEscalate || len(judgment) != 0 {
		t.Fatalf("review round 3 needs-swe transition = mechanical=%+v judgment=%+v, want escalation", mechanical, judgment)
	}
}

const fakeWatchScript = `#!/bin/bash
if [ "$1" = --status ]; then
  a=""; b=""
  for f in "$2"/.fake-pid-*; do
    [ -e "$f" ] || continue
    pid=${f##*-}
    kill -0 "$pid" 2>/dev/null || continue
    read -r kind owner < "$f"
    if [ "$kind" = bridge ]; then b="$b $pid"; else a="$a $pid"; fi
  done
  echo "armed:${a:- none}"
  echo "bridges:${b:- none}"
  [ -n "$a" ] && exit 0 || exit 1
fi
kind=armed
if [ "$1" = --bridge ]; then kind=bridge; shift; fi
echo "$kind ${WATCH_BRIDGE_OF:-}" > "$1/.fake-pid-$$"
exec sleep 300
`

// TestScriptWatcherHandsOverRealProcesses runs the handover against real
// processes: adopt stops the armed watcher with a signal, release starts a
// bridge owned by the last armed watcher.
func TestScriptWatcherHandsOverRealProcesses(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	script := filepath.Join(t.TempDir(), "watch_lanes.sh")
	write(t, script, fakeWatchScript)
	sw := ScriptWatcher{Script: script, StopTimeout: 5 * time.Second}

	armed := exec.Command("bash", script, w.dir)
	if err := armed.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = armed.Wait(); close(exited) }()
	t.Cleanup(func() { _ = armed.Process.Kill() })
	write(t, filepath.Join(w.dir, ".watch-owner"), fmt.Sprintf("%d\n", armed.Process.Pid))
	waitFor(t, func() bool { pids, _ := sw.Running(t.Context(), w.dir); return len(pids) == 1 })

	s := w.service()
	s.Watcher = sw
	if _, err := s.Adopt(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("armed watcher still running after adopt")
	}
	if err := s.Release(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	var bridge []int
	waitFor(t, func() bool { bridge, _ = sw.Running(t.Context(), w.dir); return len(bridge) == 1 })
	t.Cleanup(func() { _ = sw.Stop(context.Background(), bridge) })
	owner, err := os.ReadFile(filepath.Join(w.dir, fmt.Sprintf(".fake-pid-%d", bridge[0])))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("bridge %d\n", armed.Process.Pid); string(owner) != want {
		t.Fatalf("bridge identity = %q, want %q", owner, want)
	}
	if err := sw.Stop(t.Context(), bridge); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

type runDriverFuncs struct {
	start func(context.Context, Run) error
	stop  func(context.Context, Run) error
}

func (d runDriverFuncs) Start(ctx context.Context, r Run) error { return d.start(ctx, r) }
func (d runDriverFuncs) Stop(ctx context.Context, r Run) error  { return d.stop(ctx, r) }

func TestFailedReleaseResumesController(t *testing.T) {
	w := newWorld(t)
	svc := w.service()
	starts, stops := 0, 0
	svc.Driver = runDriverFuncs{
		start: func(context.Context, Run) error {
			if w.watcher.running {
				t.Fatal("host watcher still running at controller start")
			}
			starts++
			return nil
		},
		stop: func(context.Context, Run) error { stops++; return nil },
	}
	if _, err := svc.Adopt(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	svc.fault = func(step string) error {
		if step == "release:label" {
			return errInjected
		}
		return nil
	}
	if err := svc.Release(t.Context(), testRun); !errors.Is(err, errInjected) {
		t.Fatalf("release error = %v", err)
	}
	if starts != 2 || stops != 1 || w.watcher.running {
		t.Fatalf("handover after failure: starts=%d stops=%d watcher=%v", starts, stops, w.watcher.running)
	}
	svc.fault = nil
	if err := svc.Release(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	if stops != 2 || !w.watcher.running {
		t.Fatalf("successful handback: stops=%d watcher=%v", stops, w.watcher.running)
	}
}
