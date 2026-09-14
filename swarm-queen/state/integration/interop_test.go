//go:build integration

// Package integration proves that swarm-queen and the skill's ledger.py actually
// cooperate on a shared state.json.
//
// These assertions cannot be made in a unit test: they need the real ledger.py.
// They matter because the two tools agree only by convention -- flock is
// advisory, and the schema is a hand-maintained contract. A silent divergence
// here reproduces exactly the failure this harness exists to prevent: a ledger
// that describes a swarm which no longer exists.
package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// ledgerPy locates the skill's ledger.py, skipping if it is unavailable.
func ledgerPy(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		"../../../.claude/skills/subagent-swarm/scripts/ledger.py",
		os.ExpandEnv("$HOME/.claude/skills/subagent-swarm/scripts/ledger.py"),
	} {
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	t.Skip("ledger.py not found")
	return ""
}

// run invokes ledger.py. The repo blocks bare `python3` via a hook, so use the
// absolute interpreter.
func run(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("/usr/bin/python3", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ledger.py %v: %v\n%s", args, err, out)
	}
}

func seedRun(t *testing.T) (dir, py string) {
	t.Helper()
	dir = t.TempDir()
	py = ledgerPy(t)
	run(t, py, "init", dir, "--goal", "interop", "--phases", "swe:opus,clean:terra",
		"--base", "main", "--target", "t")
	run(t, py, "add", dir, "--id", "lane-a", "--title", "A", "--branch", "b-a", "--priority", "p1")
	return dir, py
}

// Every field of the shared schema must survive Python -> Go.
func TestGoReadsFieldsWrittenByLedgerPy(t *testing.T) {
	dir, py := seedRun(t)
	run(t, py, "set", dir, "--id", "lane-a",
		"--pr", "11968", "--pr-head", "abc123", "--approval-sha", "abc123",
		"--checks", "passing", "--fork-sha", "f00", "--phase-start-sha", "p11",
		"--round", "3")

	st, err := state.NewStore(dir).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	l := st.Lanes[0]
	if l.PR != 11968 || l.PRHead != "abc123" || l.ApprovalSHA != "abc123" ||
		l.Checks != "passing" || l.ForkSHA != "f00" || l.PhaseStartSHA != "p11" || l.Round != 3 {
		t.Errorf("field mismatch across the language boundary: %+v", l)
	}
	if l.ApprovalStale() {
		t.Error("approval pinned at head must not read as stale")
	}
}

// A Go write must survive ledger.py rewriting the whole file.
func TestLedgerPyPreservesGoWrites(t *testing.T) {
	dir, py := seedRun(t)

	if err := state.NewStore(dir).Update(func(st *state.State) error {
		st.Lanes[0].PR = 4242
		st.Lanes[0].PhaseStartSHA = "deadbeef"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// ledger.py rewrites the entire file on any mutation.
	run(t, py, "set", dir, "--id", "lane-a", "--note", "python touched this")

	b, err := os.ReadFile(filepath.Join(dir, state.StateName))
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Tasks []map[string]json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatal(err)
	}
	if got := string(probe.Tasks[0]["pr"]); got != "4242" {
		t.Errorf("pr = %s after a ledger.py write, want 4242 — Go's write was lost", got)
	}
	if got := string(probe.Tasks[0]["phase_start_sha"]); got != `"deadbeef"` {
		t.Errorf("phase_start_sha = %s, want \"deadbeef\"", got)
	}
}

// flock is advisory: it protects nothing unless BOTH tools take the same lock on
// the same path. This asserts they actually block each other.
func TestLockIsHonouredAcrossLanguages(t *testing.T) {
	dir, py := seedRun(t)

	const hold = 3 * time.Second
	released := make(chan struct{})
	acquired := make(chan struct{})

	go func() {
		defer close(released)
		// Update's callback runs while the exclusive lock is held.
		_ = state.NewStore(dir).Update(func(*state.State) error {
			close(acquired)
			time.Sleep(hold)
			return nil
		})
	}()

	<-acquired
	start := time.Now()
	run(t, py, "set", dir, "--id", "lane-a", "--note", "waited for go")
	waited := time.Since(start)
	<-released

	// Allow slack for process startup, but the wait must clearly reflect the hold.
	if waited < hold/2 {
		t.Errorf("ledger.py waited only %s while Go held the exclusive lock for %s — "+
			"the two tools are not sharing a lock", waited, hold)
	}

	st, err := state.NewStore(dir).Read()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range st.Lanes[0].Notes {
		if n == "waited for go" {
			found = true
		}
	}
	if !found {
		t.Error("the python write was lost rather than serialised")
	}
}
