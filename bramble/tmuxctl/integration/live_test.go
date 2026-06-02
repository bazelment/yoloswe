//go:build integration

// Package integration exercises tmuxctl against a real tmux server running on
// an isolated socket (tmux -L). Guarded by the integration build tag and by
// tmux availability so it never runs in hermetic CI.
package integration

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/bramble/tmuxctl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startIsolatedTmux launches a `cat` window on a private tmux server and returns
// the controller, the window target, and a cleanup func. `cat` echoes whatever
// is sent to it, so we can assert that input round-trips into the pane.
func startIsolatedTmux(t *testing.T) (tmuxctl.Controller, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	// Absolute socket path under the test's temp dir keeps parallel tests
	// isolated and is sandbox-safe (a bare -L name resolves under $TMUX_TMPDIR,
	// which may be unwritable in a hermetic sandbox).
	socketPath := filepath.Join(t.TempDir(), "tmux.sock")

	// new-session detached, running `cat` so the pane echoes its stdin.
	out, err := exec.Command("tmux", "-S", socketPath, "new-session", "-d",
		"-P", "-F", "#{window_id}", "cat").Output()
	require.NoError(t, err, "start isolated tmux session")
	target := strings.TrimSpace(string(out))
	require.NotEmpty(t, target)

	// Keep the window listed even if the pane process (cat) exits — e.g. when a
	// detached pane sees EOF on stdin under a sandbox — so list/read assertions
	// remain deterministic.
	_ = exec.Command("tmux", "-S", socketPath, "set-option", "-t", target,
		"remain-on-exit", "on").Run()

	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", socketPath, "kill-server").Run()
	})
	return tmuxctl.NewWithSocketPath(socketPath), target
}

func TestLivePasteAndEnterRoundTrips(t *testing.T) {
	t.Parallel()
	ctl, target := startIsolatedTmux(t)
	ctx := context.Background()

	require.NoError(t, ctl.Paste(ctx, target, "PINGME"))
	require.NoError(t, ctl.SendSpecial(ctx, target, tmuxctl.KeyEnter))

	// cat echoes the line back into the pane; assert it appears. Eventually,
	// not sleep, so we don't depend on a fixed echo latency.
	require.Eventually(t, func() bool {
		lines, err := ctl.Capture(ctx, target, 50)
		if err != nil {
			return false
		}
		for _, l := range lines {
			if strings.Contains(l, "PINGME") {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "pasted text should echo in the pane")
}

func TestLiveMultiLinePasteIsNotSubmittedPerLine(t *testing.T) {
	t.Parallel()
	ctl, target := startIsolatedTmux(t)
	ctx := context.Background()

	// Bracketed paste should deliver all three lines as one paste, not three
	// Enter-submits. We then send one Enter to flush cat's line buffer.
	require.NoError(t, ctl.Paste(ctx, target, "alpha\nbravo\ncharlie"))
	require.NoError(t, ctl.SendSpecial(ctx, target, tmuxctl.KeyEnter))

	require.Eventually(t, func() bool {
		lines, err := ctl.Capture(ctx, target, 50)
		if err != nil {
			return false
		}
		joined := strings.Join(lines, "\n")
		return strings.Contains(joined, "alpha") &&
			strings.Contains(joined, "bravo") &&
			strings.Contains(joined, "charlie")
	}, 5*time.Second, 50*time.Millisecond, "all pasted lines should appear")
}

func TestLiveListWindows(t *testing.T) {
	t.Parallel()
	ctl, target := startIsolatedTmux(t)
	ctx := context.Background()

	windows, err := ctl.ListWindows(ctx, "")
	require.NoError(t, err)
	require.NotEmpty(t, windows)
	found := false
	for _, w := range windows {
		if w.ID == target {
			found = true
		}
	}
	assert.True(t, found, "started window %s should be listed", target)
}
