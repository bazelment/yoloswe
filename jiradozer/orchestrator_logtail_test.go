package jiradozer

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// recordingHandler captures slog records into a slice so tests can assert
// what was emitted on the parent logger.
type recordingHandler struct {
	records []map[string]any
	mu      sync.Mutex
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := map[string]any{"msg": r.Message, "level": r.Level.String()}
	r.Attrs(func(a slog.Attr) bool {
		rec[a.Key] = a.Value.Any()
		return true
	})
	h.records = append(h.records, rec)
	return nil
}

func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *recordingHandler) findAll(msg string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, r := range h.records {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

func TestMaybeEmitTransition_AllowList(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		line   string
		expect string // expected msg on parent logger; empty = nothing emitted
	}{
		{
			"step start",
			"I0504 22:00:54.425221 1350798 workflow.go:339] step: plan issue=ENG-1 feedback=false resume=false\n",
			"subprocess step",
		},
		{
			"step completed",
			"I0504 22:02:11.817171 1350798 workflow.go:353] step completed step=plan issue=ENG-1 session_id= duration=1m17.39s\n",
			"subprocess step completed",
		},
		{
			"waiting for approval",
			"I0504 22:02:13.836103 1350798 workflow.go:424] waiting for approval step=plan_review issue=ENG-1\n",
			"subprocess waiting for approval",
		},
		{
			"feedback approved",
			"I0504 22:03:59.142546 1350798 workflow.go:437] feedback: approved step=plan_review\n",
			"subprocess feedback",
		},
		{
			// agent text is high-volume and intentionally NOT re-emitted.
			"non-allow-listed slog line drops",
			`D0504 22:17:24.278133 1350798 agent.go:146] agent text step=create_pr text="chunk"` + "\n",
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &recordingHandler{}
			o := &Orchestrator{logger: slog.New(h), config: testOrchestratorConfig()}
			mw := &managedWorkflow{issue: &tracker.Issue{ID: "1", Identifier: "ENG-1"}}

			o.maybeEmitTransition(mw, tc.line, true)

			if tc.expect == "" {
				h.mu.Lock()
				require.Empty(t, h.records, "expected no parent-log emission")
				h.mu.Unlock()
				return
			}
			require.Len(t, h.findAll(tc.expect), 1, "expected exactly one %q emission", tc.expect)
		})
	}

	// PR URL re-emit: appears in many lines around create_pr; allowPRURL=false
	// after the first hit gates further emissions.
	h := &recordingHandler{}
	o := &Orchestrator{logger: slog.New(h), config: testOrchestratorConfig()}
	mw := &managedWorkflow{issue: &tracker.Issue{ID: "1", Identifier: "ENG-1"}}
	prLine := "I0504 22:17:54.349691 1350798 agent.go:146] agent text step=create_pr text=https://github.com/owner/repo/pull/42\n"
	o.maybeEmitTransition(mw, prLine, true)
	o.maybeEmitTransition(mw, prLine, false)
	urls := h.findAll("subprocess pr_url")
	require.Len(t, urls, 1, "expected exactly one PR URL emission across two lines")
	require.Equal(t, "https://github.com/owner/repo/pull/42", urls[0]["url"])
}

// TestMaybeEmitTransition_RecordsCurrentStep verifies that seeing a "step:"
// line populates currentStep so the watchdog can resolve the right timeout
// from config for the active step.
func TestMaybeEmitTransition_RecordsCurrentStep(t *testing.T) {
	t.Parallel()

	cfg := testOrchestratorConfig()
	cfg.Plan.IdleTimeout = 7 * time.Minute
	cfg.Build.IdleTimeout = 22 * time.Minute

	o := &Orchestrator{logger: slog.New(&recordingHandler{}), config: cfg}
	mw := &managedWorkflow{issue: &tracker.Issue{ID: "1", Identifier: "ENG-1"}}

	o.maybeEmitTransition(mw, "I0504 22:00:54.425221 1350798 workflow.go:339] step: plan issue=ENG-1\n", true)
	mw.stepMu.Lock()
	require.Equal(t, "plan", mw.currentStep)
	mw.stepMu.Unlock()
	require.Equal(t, 7*time.Minute, o.idleTimeoutForStep(mw.currentStep))

	o.maybeEmitTransition(mw, "I0504 22:04:01.672857 1350798 workflow.go:339] step: build issue=ENG-1\n", true)
	mw.stepMu.Lock()
	require.Equal(t, "build", mw.currentStep)
	mw.stepMu.Unlock()
	require.Equal(t, 22*time.Minute, o.idleTimeoutForStep(mw.currentStep))
}

// TestTailSubprocessLog_StreamsAndUpdatesLastOutput verifies that the
// tailer follows a growing file, re-emits transitions, and updates
// lastOutputAt on each line (the watchdog's input signal).
func TestTailSubprocessLog_StreamsAndUpdatesLastOutput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "subprocess.log")
	require.NoError(t, os.WriteFile(logPath, nil, 0o600))

	h := &recordingHandler{}
	o := &Orchestrator{logger: slog.New(h), config: testOrchestratorConfig()}
	mw := &managedWorkflow{issue: &tracker.Issue{ID: "1", Identifier: "ENG-1"}}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		o.tailSubprocessLog(mw, logPath, stop)
		close(done)
	}()

	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })

	_, err = f.WriteString("I0504 22:00:54.425221 1350798 workflow.go:339] step: plan issue=ENG-1\n")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(h.findAll("subprocess step")) >= 1
	}, 2*time.Second, 20*time.Millisecond, "tailer did not pick up step line")

	first := mw.lastOutputAt.Load()
	require.NotZero(t, first)

	time.Sleep(15 * time.Millisecond)
	_, err = f.WriteString("I0504 22:02:11.817171 1350798 workflow.go:353] step completed step=plan issue=ENG-1 duration=1m17s\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return len(h.findAll("subprocess step completed")) >= 1
	}, 2*time.Second, 20*time.Millisecond)
	require.Greater(t, mw.lastOutputAt.Load(), first)

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tailer did not exit after stop")
	}
}

// TestRunWatchdog_CancelsOnIdle drives runWatchdog with a fast tick and
// confirms it cancels when lastOutputAt is older than IdleTimeout.
func TestRunWatchdog_CancelsOnIdle(t *testing.T) {
	t.Parallel()

	cancelled := atomic.Bool{}
	mw := &managedWorkflow{
		issue:       &tracker.Issue{ID: "1", Identifier: "ENG-1"},
		cancel:      func() { cancelled.Store(true) },
		currentStep: "plan",
	}
	mw.lastOutputAt.Store(time.Now().Add(-10 * time.Second).UnixNano())

	cfg := testOrchestratorConfig()
	cfg.Plan.IdleTimeout = 50 * time.Millisecond
	h := &recordingHandler{}
	o := &Orchestrator{logger: slog.New(h), config: cfg}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		o.runWatchdog(mw, 5*time.Millisecond, stop)
		close(done)
	}()
	t.Cleanup(func() { close(stop) })

	require.Eventually(t, cancelled.Load,
		2*time.Second, 5*time.Millisecond, "watchdog did not cancel on idle")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runWatchdog did not return after cancelling")
	}
	require.NotEmpty(t, h.findAll("subprocess hung — cancelling"),
		"expected hang log line")
}

// TestRunWatchdog_DoesNotCancelWhenNoTimeout verifies that with
// IdleTimeout=0 the watchdog never fires.
func TestRunWatchdog_DoesNotCancelWhenNoTimeout(t *testing.T) {
	t.Parallel()

	cancelled := atomic.Bool{}
	mw := &managedWorkflow{
		issue:       &tracker.Issue{ID: "1", Identifier: "ENG-1"},
		cancel:      func() { cancelled.Store(true) },
		currentStep: "plan",
	}
	mw.lastOutputAt.Store(time.Now().Add(-time.Hour).UnixNano())

	// testOrchestratorConfig leaves IdleTimeout zero on every step, so the
	// watchdog has nothing to fire on.
	o := &Orchestrator{logger: slog.New(&recordingHandler{}), config: testOrchestratorConfig()}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		o.runWatchdog(mw, 5*time.Millisecond, stop)
		close(done)
	}()

	// Let the ticker fire many times; cancel must remain false.
	time.Sleep(80 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWatchdog did not exit on stop")
	}
	require.False(t, cancelled.Load(), "watchdog must not cancel when IdleTimeout=0")
}

// TestCleanup_ReleasesLockLabelOnAllTerminationPaths verifies that
// cleanup() removes LockLabel for every terminal step. Pre-Fix-#7 the
// label leaked on every run.
func TestCleanup_ReleasesLockLabelOnAllTerminationPaths(t *testing.T) {
	t.Parallel()

	for _, step := range []WorkflowStep{StepDone, StepCancelled, StepFailed} {
		t.Run(step.String(), func(t *testing.T) {
			mt := &mockClaimTracker{}
			cfg := testOrchestratorConfig()
			wtm := newMockWTManagerWithDir(t)
			o := NewOrchestrator(mt, cfg, wtm, "", testLogger(t))

			mw := &managedWorkflow{
				issue:        &tracker.Issue{ID: "issue-1", Identifier: "ENG-1"},
				worktreePath: "/tmp/wt/ENG-1",
				branch:       "jiradozer/ENG-1",
			}
			o.cleanup(context.Background(), mw, step)

			removed := mt.getRemovedLabels()
			require.Len(t, removed, 1, "expected one RemoveLabel call for %s", step)
			require.Equal(t, LockLabel, removed[0])
		})
	}
}
