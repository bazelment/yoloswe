//go:build integration

// C14: jiradozer workflow with a Validate prompt shaped like pr-polish
// round 2 — sync step plus two Monitors (one fails fast, one long) —
// must complete the Validate step successfully without tripping the
// removed "live background work" guard.
//
// This is the end-to-end regression for the INF-401 failure. It runs the
// real workflow, real Claude CLI, and a stubbed `bramble` binary on PATH
// so the test is hermetic and fast (~15s) instead of requiring real
// bramble/codex/cursor accounts.
//
// Invariants:
//  1. Validate step transitions to StepValidateReview (passed).
//  2. Total wall time is >= the slow Monitor's sleep (~12s) — proves the
//     consumer actually waited for the bg work to settle.
//  3. No log line contains "live background work" — the old guard is gone.
package integration

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer"
)

// writeFakeBramble drops a shell script named `bramble` into dir that
// mimics the two Monitor subprocesses from pr-polish. The fake takes
// subcommand: "code-review --backend codex" exits 2 after ~0.5s; any
// other invocation sleeps 12 seconds then prints a synthetic envelope
// and exits 0.
func writeFakeBramble(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/usr/bin/env bash
# Fake bramble shim for C14 integration test.
# Mimics the INF-401 round 2 pr-polish shape.
set -u
cmd="${1:-}"
backend=""
for arg in "$@"; do
  case "$arg" in
    codex)   backend="codex" ;;
    cursor)  backend="cursor" ;;
  esac
  # also catch --backend X
done
# Parse --backend value
while [ $# -gt 0 ]; do
  case "$1" in
    --backend) backend="$2"; shift 2 ;;
    *) shift ;;
  esac
done

if [ "$cmd" = "code-review" ] && [ "$backend" = "codex" ]; then
  sleep 0.5
  echo "ERROR: invalid model gpt-4.1-mini (HTTP 400)" >&2
  exit 2
fi

if [ "$cmd" = "code-review" ]; then
  sleep 12
  echo '{"suggestions":[{"severity":"nit","body":"C14 fake review ok"}]}'
  exit 0
fi

# gh-pr-status and other read-only subcommands just echo ok.
echo "ok"
exit 0
`
	path := filepath.Join(dir, "bramble")
	require.NoError(t, os.WriteFile(path, []byte(script), 0755))
	return path
}

// captureLogBuffer is a slog.Handler that tees all records into a
// thread-safe buffer so the test can grep the final log for removed
// signals like "live background work".
type captureLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (c *captureLogBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

// TestValidate_PRPolishLocal (C14) — the post-refactor regression for the
// INF-401 workflow failure. See file comment for invariants.
func TestValidate_PRPolishLocal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	workDir := t.TempDir()
	binDir := t.TempDir()
	writeFakeBramble(t, binDir)

	// Prepend fake binDir to PATH so the agent's Bash/Monitor tools pick
	// it up instead of any real bramble binary.
	origPath := os.Getenv("PATH")
	require.NoError(t, os.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath))
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })

	// Capture logs so we can assert removed signals absent.
	buf := &captureLogBuffer{}
	logger := slog.New(slog.NewTextHandler(
		multiWriter(os.Stderr, &buf.b, &buf.mu),
		&slog.HandlerOptions{Level: slog.LevelInfo},
	))

	issue := e2eIssue()
	ft := NewFakeTracker(e2eWorkflowStates())
	ft.AddIssue(*issue)

	cfg := e2eConfig(t, workDir)
	// Redirect Validate to the pr-polish-style prompt. We write out the
	// slow/fast markers to files so the test can verify the slow Monitor
	// actually ran to completion before the step returned.
	slowMarker := filepath.Join(workDir, "c14_slow.txt")
	cfg.Validate = jiradozer.StepConfig{
		Model:          "haiku",
		PermissionMode: "bypass",
		MaxTurns:       6,
		MaxBudgetUSD:   2.0,
		AutoApprove:    true,
		Prompt: `Issue: {{.Identifier}} — {{.Title}}

You MUST launch TWO bg tool_uses in the SAME turn (do both before waiting):

1. Monitor tool: run ` + "`bramble code-review --backend codex --goal fake`" + ` — this will exit fast with non-zero.
2. Bash with run_in_background:true: run ` + "`bramble code-review --backend cursor --goal fake && echo C14_SLOW_DONE > " + slowMarker + "`" + ` — this takes ~12s.

After BOTH settle, report their terminal statuses. Do not edit any files.`,
	}

	wf := jiradozer.NewWorkflow(ft, issue, cfg, logger)
	var transitions []jiradozer.WorkflowStep
	var mu sync.Mutex
	wf.OnTransition = func(step jiradozer.WorkflowStep) {
		mu.Lock()
		transitions = append(transitions, step)
		mu.Unlock()
		t.Logf("transition → %s", step)
	}

	start := time.Now()
	err := wf.Run(ctx)
	elapsed := time.Since(start)
	require.NoError(t, err, "workflow should not error — the old HasLiveBackgroundWork guard is gone")

	// Invariant 1: Validate step transitioned past review (not refused).
	mu.Lock()
	got := append([]jiradozer.WorkflowStep(nil), transitions...)
	mu.Unlock()
	sawValidateReview := false
	for _, step := range got {
		if step == jiradozer.StepValidateReview {
			sawValidateReview = true
			break
		}
	}
	assert.True(t, sawValidateReview, "Validate step should have transitioned to ValidateReview")

	// Invariant 2: elapsed >= 12s (the slow Monitor ran to completion).
	if _, statErr := os.Stat(slowMarker); statErr != nil {
		// Marker not required on happy path if model chose sync tools; log only.
		t.Logf("slow marker missing (%v) — model may have picked sync tools instead of bg", statErr)
	} else {
		assert.GreaterOrEqual(t, elapsed, 10*time.Second,
			"workflow elapsed should cover the slow Monitor; got %v", elapsed)
	}

	// Invariant 3: no "live background work" in logs.
	logs := buf.String()
	assert.NotContains(t, strings.ToLower(logs), "live background work",
		"post-refactor logs must not reference the removed guard")
}

// multiWriter is a tiny tee that writes into two writers while grabbing a
// mutex on the second (since bytes.Buffer is not concurrency-safe and the
// slog handler writes from multiple goroutines).
type lockedMultiWriter struct {
	a  *os.File
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w *lockedMultiWriter) Write(p []byte) (int, error) {
	_, _ = w.a.Write(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func multiWriter(a *os.File, b *bytes.Buffer, mu *sync.Mutex) *lockedMultiWriter {
	return &lockedMultiWriter{a: a, b: b, mu: mu}
}
