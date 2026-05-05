package jiradozer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	mw.tailerAlive.Store(true)

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

// TestRunWatchdog_DoesNotCancelDuringReviewWait verifies that once the
// subprocess emits "waiting for approval" and the tailer flips
// inReview=true, runWatchdog suppresses its idle check. The workflow
// legitimately blocks in PollForFeedback during human review and the
// prior step's idle_timeout must not cancel that wait.
func TestRunWatchdog_DoesNotCancelDuringReviewWait(t *testing.T) {
	t.Parallel()

	cancelled := atomic.Bool{}
	mw := &managedWorkflow{
		issue:       &tracker.Issue{ID: "1", Identifier: "ENG-1"},
		cancel:      func() { cancelled.Store(true) },
		currentStep: "plan",
	}
	mw.lastOutputAt.Store(time.Now().Add(-time.Hour).UnixNano())
	mw.tailerAlive.Store(true)
	mw.inReview.Store(true)

	cfg := testOrchestratorConfig()
	cfg.Plan.IdleTimeout = 50 * time.Millisecond
	o := &Orchestrator{logger: slog.New(&recordingHandler{}), config: cfg}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		o.runWatchdog(mw, 5*time.Millisecond, stop)
		close(done)
	}()

	// Many ticks pass; cancel must remain false because we're in review.
	time.Sleep(80 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWatchdog did not exit on stop")
	}
	require.False(t, cancelled.Load(),
		"watchdog must not cancel during human review wait")
}

// TestMaybeEmitTransition_TogglesReviewFlag verifies that the tailer
// flips mw.inReview on the right log lines: true on "waiting for
// approval", false on "feedback:" or the next "step:" line. The
// watchdog reads this to suppress idle cancellation during human review.
func TestMaybeEmitTransition_TogglesReviewFlag(t *testing.T) {
	t.Parallel()

	o := &Orchestrator{logger: slog.New(&recordingHandler{}), config: testOrchestratorConfig()}
	mw := &managedWorkflow{issue: &tracker.Issue{ID: "1", Identifier: "ENG-1"}}

	// Initially not in review.
	require.False(t, mw.inReview.Load())

	// "waiting for approval" → in review.
	o.maybeEmitTransition(mw,
		"I0504 22:02:13.836103 1350798 workflow.go:424] waiting for approval step=plan_review issue=ENG-1\n", true)
	require.True(t, mw.inReview.Load(), "waiting for approval must set inReview=true")

	// "feedback: approved" → leaves review.
	o.maybeEmitTransition(mw,
		"I0504 22:03:59.142546 1350798 workflow.go:437] feedback: approved step=plan_review\n", true)
	require.False(t, mw.inReview.Load(), "feedback line must clear inReview")

	// Re-enter review, then exit via next "step:" line.
	o.maybeEmitTransition(mw,
		"I0504 22:04:00.000000 1350798 workflow.go:424] waiting for approval step=build_review issue=ENG-1\n", true)
	require.True(t, mw.inReview.Load())
	o.maybeEmitTransition(mw,
		"I0504 22:04:01.672857 1350798 workflow.go:339] step: validate issue=ENG-1\n", true)
	require.False(t, mw.inReview.Load(), "next step: line must clear inReview")
}

// TestIdleTimeoutForStep_PreStepFallsBackToMax verifies that when
// currentStep is empty (the startup window before the first "step:"
// line is parsed) idleTimeoutForStep returns the max across configured
// steps so a silent startup hang is still caught conservatively.
func TestIdleTimeoutForStep_PreStepFallsBackToMax(t *testing.T) {
	t.Parallel()

	cfg := testOrchestratorConfig()
	cfg.Plan.IdleTimeout = 5 * time.Minute
	cfg.Build.IdleTimeout = 22 * time.Minute
	cfg.Validate.IdleTimeout = 10 * time.Minute

	o := &Orchestrator{logger: slog.New(&recordingHandler{}), config: cfg}

	// Empty step name: fall back to max across all steps.
	require.Equal(t, 22*time.Minute, o.idleTimeoutForStep(""),
		"unknown step must fall back to max configured timeout")
	// Known step still wins exactly.
	require.Equal(t, 5*time.Minute, o.idleTimeoutForStep("plan"))

	// All-zero config: still return zero, preserving the existing
	// "watchdog disabled" semantics.
	zeroCfg := testOrchestratorConfig()
	zo := &Orchestrator{logger: slog.New(&recordingHandler{}), config: zeroCfg}
	require.Equal(t, time.Duration(0), zo.idleTimeoutForStep(""),
		"all-zero IdleTimeout must keep the watchdog disabled")
}

// errOnceSeeker wraps an underlying ReadSeeker, returning a non-EOF error
// the first time the wrapped Read encounters EOF, then delegating cleanly
// after a Seek. This drives the tailer's reopen + offset-resume path
// deterministically using a real seekable underlying source so offset
// tracking is actually exercised.
type errOnceSeeker struct {
	r        *strings.Reader
	errored  bool
	seekHits int
}

func (s *errOnceSeeker) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err == io.EOF && !s.errored {
		s.errored = true
		// Convert the first EOF into a transient read error so the
		// tailer takes the reopen path. Subsequent reads (after the
		// caller closes us and reopens) will see a fresh seeker.
		return n, errors.New("simulated transient read error")
	}
	return n, err
}

func (s *errOnceSeeker) Seek(offset int64, whence int) (int64, error) {
	s.seekHits++
	return s.r.Seek(offset, whence)
}

func (s *errOnceSeeker) Close() error { return nil }

// TestTailSubprocessLog_ReopensAfterReadError drives the reopen + offset
// resume branch of tailSubprocessLog deterministically. The first opener
// call hands back a wrapped seeker that fails with a non-EOF error after
// the first line. The second opener call returns a seeker over the full
// file (two lines), so the tailer must Seek past the first line on
// reopen — otherwise the first line would be re-emitted, flipping
// inReview / currentStep back to stale values.
//
// Not t.Parallel(): mutates the package-level logTailOpener.
func TestTailSubprocessLog_ReopensAfterReadError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "subprocess.log")
	require.NoError(t, os.WriteFile(logPath, nil, 0o600))

	const line1 = "I0504 22:00:54.425221 1350798 workflow.go:339] step: plan issue=ENG-1\n"
	const line2 = "I0504 22:01:54.425221 1350798 workflow.go:339] step: build issue=ENG-1\n"
	full := line1 + line2

	calls := atomic.Int32{}
	original := logTailOpener
	logTailOpener = func(_ string) (io.ReadSeekCloser, error) {
		n := calls.Add(1)
		if n == 1 {
			// First open: only the first line is "available", and we
			// trip a non-EOF error after it.
			return &errOnceSeeker{r: strings.NewReader(line1)}, nil
		}
		// Reopen: full file is now visible. Tailer must Seek past
		// line1 so it does not replay the "step: plan" transition.
		return &readSeekCloserFromString{r: strings.NewReader(full)}, nil
	}
	t.Cleanup(func() { logTailOpener = original })

	h := &recordingHandler{}
	o := &Orchestrator{logger: slog.New(h), config: testOrchestratorConfig()}
	mw := &managedWorkflow{issue: &tracker.Issue{ID: "1", Identifier: "ENG-1"}}
	mw.tailerAlive.Store(true)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		o.tailSubprocessLog(mw, logPath, stop)
		close(done)
	}()

	// Both step lines must reach the parent log.
	require.Eventually(t, func() bool {
		return len(h.findAll("subprocess step")) >= 2
	}, 3*time.Second, 20*time.Millisecond,
		"tailer did not parse both pre-error and post-reopen lines")

	// And the read-error / reopen attempt must have been logged.
	require.NotEmpty(t, h.findAll("log tailer: read error, attempting reopen"))

	require.True(t, mw.tailerAlive.Load(),
		"tailer must remain alive after a single transient read error")
	require.GreaterOrEqual(t, int(calls.Load()), 2,
		"logTailOpener must have been called at least twice (initial + reopen)")

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tailer did not exit after stop")
	}
	require.False(t, mw.tailerAlive.Load(),
		"tailerAlive must clear on graceful exit")

	// Critical regression check: each step transition must have been
	// emitted exactly once. If reopen Seek() were missing, line1 would
	// be re-parsed and we'd see "step: plan" emitted twice.
	steps := h.findAll("subprocess step")
	planHits := 0
	buildHits := 0
	for _, r := range steps {
		switch r["step"] {
		case "plan":
			planHits++
		case "build":
			buildHits++
		}
	}
	require.Equal(t, 1, planHits, "step: plan must be emitted exactly once (no replay)")
	require.Equal(t, 1, buildHits, "step: build must be emitted exactly once")
}

// readSeekCloserFromString is the test variant of os.Open: a seekable,
// closable view over an in-memory string. Used to drive the tailer's
// reopen path with deterministic content while still exercising real
// Seek-after-reopen semantics.
type readSeekCloserFromString struct {
	r *strings.Reader
}

func (r *readSeekCloserFromString) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func (r *readSeekCloserFromString) Seek(offset int64, whence int) (int64, error) {
	return r.r.Seek(offset, whence)
}

func (r *readSeekCloserFromString) Close() error { return nil }

// TestRunWatchdog_DoesNotCancelWhenTailerDead verifies that the watchdog
// suppresses its idle check when the tailer goroutine has exited
// (tailerAlive=false). Otherwise lastOutputAt would never refresh and
// the watchdog would kill a still-healthy subprocess after IdleTimeout.
func TestRunWatchdog_DoesNotCancelWhenTailerDead(t *testing.T) {
	t.Parallel()

	cancelled := atomic.Bool{}
	mw := &managedWorkflow{
		issue:       &tracker.Issue{ID: "1", Identifier: "ENG-1"},
		cancel:      func() { cancelled.Store(true) },
		currentStep: "plan",
	}
	mw.lastOutputAt.Store(time.Now().Add(-time.Hour).UnixNano())
	// Tailer never alive in this test — simulates an early read-error exit.
	mw.tailerAlive.Store(false)

	cfg := testOrchestratorConfig()
	cfg.Plan.IdleTimeout = 50 * time.Millisecond
	o := &Orchestrator{logger: slog.New(&recordingHandler{}), config: cfg}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		o.runWatchdog(mw, 5*time.Millisecond, stop)
		close(done)
	}()

	// Give the watchdog many ticks; it must NOT cancel.
	time.Sleep(80 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWatchdog did not exit on stop")
	}
	require.False(t, cancelled.Load(),
		"watchdog must not cancel when tailer is dead — lastOutputAt is stale")
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
	mw.tailerAlive.Store(true)

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
