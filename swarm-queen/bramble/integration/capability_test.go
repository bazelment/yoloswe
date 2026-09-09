//go:build integration

// Package integration asserts the live bramble tool surface.
//
// These are capability self-tests: they fail when bramble drifts from what
// swarm-queen assumes, so drift breaks a test run instead of a live swarm. Every
// assertion here was established by invocation against a running TUI, and each
// corresponds to something a real orchestrator got wrong.
package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func client(t *testing.T) *bramble.Client {
	t.Helper()
	c, err := bramble.New()
	if err != nil {
		t.Skipf("no reachable bramble TUI: %v", err)
	}
	return c
}

// The socket path carries the TUI's PID. The skill's documented preflight uses
// an un-suffixed path that does not exist, so `test -S "$BRAMBLE_SOCK"` fails on
// step 1 of every documented run.
func TestSocketPathResolvesToALiveSocket(t *testing.T) {
	sock, err := bramble.SocketPath()
	if err != nil {
		t.Skipf("no bramble socket: %v", err)
	}
	if !strings.Contains(sock, "-") {
		t.Errorf("socket %q lacks the expected pid suffix", sock)
	}
	if strings.Contains(sock, "-control-") {
		t.Errorf("socket %q is the control socket, not the command socket", sock)
	}
	if err := client(t).Ping(ctx(t)); err != nil {
		t.Errorf("ping over %s: %v", sock, err)
	}
}

// The response is a dict wrapping a list, and rows key on worktree_name.
func TestListSessionsShape(t *testing.T) {
	sessions, err := client(t).ListSessions(ctx(t))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Skip("no live sessions to assert against")
	}
	for _, s := range sessions {
		if s.ID == "" || s.WorktreeName == "" {
			t.Errorf("row missing required identity fields: %+v", s)
		}
		if s.Status == "running" && !s.HasPane() {
			t.Errorf("running session %s has no tmux_target", s.ID)
		}
	}
}

// There is no `bramble kill-session`. Reaping is three manual layers (process,
// worktree, tmux window); if this ever starts passing, lifecycle/ can be
// simplified.
func TestKillSessionStillDoesNotExist(t *testing.T) {
	out, err := exec.CommandContext(ctx(t), "bramble", "kill-session").CombinedOutput()
	if err == nil {
		t.Error("bramble kill-session now exists — simplify the reap path")
	}
	if !strings.Contains(string(out), "unknown command") {
		t.Logf("unexpected failure mode: %s", out)
	}
}

// send-key exists and is the correct way to submit a composer, rather than raw
// tmux send-keys. One mined run spent 29 hours fighting the composer before
// discovering this.
func TestSendKeyExists(t *testing.T) {
	out, err := exec.CommandContext(ctx(t), "bramble", "send-key", "--help").CombinedOutput()
	if err != nil {
		t.Errorf("bramble send-key --help failed: %v: %s", err, out)
	}
}
