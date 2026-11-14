//go:build integration

// Package integration runs the adoption handover against the installed
// subagent-swarm-nebula watch_lanes.sh. It skips when the script is absent:
//
//	bazel test //orca/adopt/integration:integration_test --test_env=ORCA_WATCH_LANES=$HOME/.claude/skills/subagent-swarm-nebula/scripts/watch_lanes.sh
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/adopt"
)

type labeler struct{ label string }

func (l *labeler) RunOrchestrator(context.Context, string) (string, error) { return l.label, nil }
func (l *labeler) SetRunOrchestrator(_ context.Context, _ string, v string) error {
	l.label = v
	return nil
}

func TestHandoverWithRealWatchLanes(t *testing.T) {
	script := adopt.FindWatchScript()
	if script == "" {
		t.Skip("watch_lanes.sh not installed; set ORCA_WATCH_LANES")
	}
	root := t.TempDir()
	dir := filepath.Join(root, ".shared", "subagent-swarm", "20261113-000000-real")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"config":{"goal":"g","phases":[{"name":"swe"}]},"tasks":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sw := adopt.ScriptWatcher{Script: script, StopTimeout: 5 * time.Second}

	// The host orchestrator's armed watcher.
	armed := exec.Command("bash", script, dir, "5")
	armed.Env = append(os.Environ(), "SNL_RUN_ID=")
	if err := armed.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = armed.Wait() }()
	t.Cleanup(func() { _ = armed.Process.Kill() })
	waitPIDs(t, sw, dir, 1)
	owner, err := os.ReadFile(filepath.Join(dir, ".watch-owner"))
	if err != nil || strings.TrimSpace(string(owner)) != strconv.Itoa(armed.Process.Pid) {
		t.Fatalf(".watch-owner = %q, %v; want %d", owner, err, armed.Process.Pid)
	}

	l := &labeler{label: "claude"}
	s := &adopt.Service{Importer: adopt.Importer{Roots: []string{root}}, Labeler: l, Watcher: sw, Holder: adopt.ChatHolder("it")}
	if _, err := s.Adopt(t.Context(), filepath.Base(dir)); err != nil {
		t.Fatal(err)
	}
	if pids, err := sw.Running(t.Context(), dir); err != nil || len(pids) != 0 {
		t.Fatalf("after adopt: watchers %v, %v", pids, err)
	}
	if err := s.Release(t.Context(), filepath.Base(dir)); err != nil {
		t.Fatal(err)
	}
	bridge := waitPIDs(t, sw, dir, 1)
	t.Cleanup(func() { _ = sw.Stop(context.Background(), bridge) })
	args, err := os.ReadFile("/proc/" + strconv.Itoa(bridge[0]) + "/cmdline")
	if err != nil || !strings.Contains(string(args), "--bridge") {
		t.Fatalf("restarted watcher argv %q, %v; want a --bridge", args, err)
	}
	if l.label != "claude" {
		t.Fatalf("label %q after release", l.label)
	}
}

func waitPIDs(t *testing.T, sw adopt.ScriptWatcher, dir string, n int) []int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		pids, err := sw.Running(t.Context(), dir)
		if err == nil && len(pids) == n {
			return pids
		}
		if time.Now().After(deadline) {
			t.Fatalf("watchers %v, %v; want %d", pids, err, n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
