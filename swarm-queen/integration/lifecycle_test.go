//go:build integration

// Package integration drives swarm-queen the way /subagent-swarm actually runs:
// real bramble sessions, in real tmux windows, on real git worktrees.
//
// Everything below is deliberately end-to-end. The bugs this harness exists to
// prevent -- an empty branch behind a .done, a stale approval, a worktree
// removed out from under a live agent -- are all invisible to a unit test with
// a fake bramble, because each one is a disagreement between what an agent
// SAYS and what the system actually contains.
//
// These tests create and destroy real resources. They are gated behind the
// integration tag and tagged manual+local in BUILD.bazel so `bazel test //...`
// never runs them.
package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/lifecycle"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

const (
	// testRepo is the bramble repository these tests spawn into.
	testRepo = "yoloswe"
	// spawnSettle is how long a fresh session gets to appear in list-sessions.
	spawnSettle = 90 * time.Second
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return c
}

func client(t *testing.T) *bramble.Client {
	t.Helper()
	c, err := bramble.New()
	if err != nil {
		t.Skipf("no reachable bramble TUI: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Skipf("bramble not answering: %v", err)
	}
	return c
}

// repoRoot is the main worktree these tests create branches under.
func repoRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("SWARM_QUEEN_TEST_REPO")
	if root == "" {
		root = os.ExpandEnv("$HOME/worktrees/yoloswe/main")
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skipf("no repo at %s (set SWARM_QUEEN_TEST_REPO): %v", root, err)
	}
	return root
}

// uniqueSuffix keeps concurrent runs and reruns from colliding on branch names.
func uniqueSuffix() string { return fmt.Sprintf("%d", time.Now().UnixNano()%1e7) }

// seedRun creates a run directory with a ledger, as `ledger.py init` would.
func seedRun(t *testing.T, phases []state.Phase, lanes ...*state.Lane) (string, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	store := state.NewStore(dir)
	if err := store.Create(state.Config{
		Goal: "swarm-queen integration", Base: "main", Target: "main", Phases: phases,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) error {
		st.Lanes = append(st.Lanes, lanes...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return dir, store
}

// reaper tears down everything a test created, even on failure. Leaking a tmux
// window or a worktree from a test would be the same defect the harness exists
// to prevent.
type reaper struct {
	t        *testing.T
	client   *bramble.Client
	repo     string
	sessions []string
	branches []string
	trees    []string
}

func (r *reaper) session(id string) { r.sessions = append(r.sessions, id) }
func (r *reaper) branch(b string)   { r.branches = append(r.branches, b) }
func (r *reaper) tree(p string)     { r.trees = append(r.trees, p) }

func (r *reaper) cleanup() {
	c := context.Background()
	tmux := lifecycle.ExecTmux{}
	self, _ := lifecycle.ResolveSelf(c, tmux)

	// Kill sessions before removing their worktrees: an agent left running
	// against a deleted path keeps acting.
	live, _ := r.client.ListSessions(c)
	for _, want := range r.sessions {
		for i := range live {
			s := &live[i]
			if s.ID != want || !s.HasPane() {
				continue
			}
			if err := lifecycle.SafeKillWindow(c, tmux, s.TmuxTarget, self, want); err != nil {
				r.t.Logf("cleanup: kill %s (%s): %v", want, s.TmuxTarget, err)
			}
		}
	}
	git := reconcile.ExecGit{}
	for _, p := range r.trees {
		if _, err := git.Run(c, r.repo, "worktree", "remove", "--force", p); err != nil {
			r.t.Logf("cleanup: remove worktree %s: %v", p, err)
		}
	}
	for _, b := range r.branches {
		if _, err := git.Run(c, r.repo, "branch", "-D", b); err != nil {
			r.t.Logf("cleanup: delete branch %s: %v", b, err)
		}
	}
	// Prune stale administrative entries left by force-removed worktrees.
	if _, err := git.Run(c, r.repo, "worktree", "prune"); err != nil {
		r.t.Logf("cleanup: worktree prune: %v", err)
	}
}

func newReaper(t *testing.T, c *bramble.Client, repo string) *reaper {
	r := &reaper{t: t, client: c, repo: repo}
	t.Cleanup(r.cleanup)
	return r
}

// waitForSession polls until the session appears WITH a pane, or fails with
// what was last seen.
//
// Waiting for the pane, not merely for the row, is the point: bramble registers
// a session before tmux has assigned its window, so the first snapshot after a
// spawn routinely reports tmux_target="". Treating that as "it did not start"
// is a false negative -- and treating it as "no pane, therefore dead" is the
// same mistake in the opposite direction.
func waitForSession(t *testing.T, c *bramble.Client, id string, d time.Duration) bramble.Session {
	t.Helper()
	deadline := time.Now().Add(d)
	var last bramble.Session
	var found bool
	for time.Now().Before(deadline) {
		sessions, err := c.ListSessions(context.Background())
		if err == nil {
			for i := range sessions {
				if sessions[i].ID != id {
					continue
				}
				found = true
				last = sessions[i]
				if last.HasPane() {
					return last
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	if found {
		t.Fatalf("session %q appeared but never got a tmux pane within %s (status=%s)",
			id, d, last.Status)
	}
	t.Fatalf("session %q never appeared within %s", id, d)
	return bramble.Session{}
}

// runCmd executes a command and returns combined output, so a failure message
// carries what the tool actually printed rather than just an exit code.
func runCmd(c context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(c, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// doctorBinary locates the built swarm-queen binary, skipping when absent so the
// suite does not fail on a missing build artifact.
func doctorBinary(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		"../../bazel-bin/swarm-queen/cmd/swarm-queen/swarm-queen_/swarm-queen",
		os.ExpandEnv("$HOME/worktrees/yoloswe/feat/swarm-agent/bazel-bin/swarm-queen/cmd/swarm-queen/swarm-queen_/swarm-queen"),
	} {
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	t.Skip("swarm-queen binary not built; run `bazel build //swarm-queen/cmd/swarm-queen`")
	return ""
}

// runCmdStdout returns only stdout, discarding stderr, so a test can assert what
// a caller sees when stderr is redirected away.
func runCmdStdout(c context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(c, name, args...)
	out, err := cmd.Output()
	return string(out), err
}
