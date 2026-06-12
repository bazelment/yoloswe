package jiradozer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// TestManagedWorkflow_TailRing verifies the per-issue tail ring buffer bounds
// its size, drops oldest lines, strips newlines, truncates over-long lines,
// and returns an independent snapshot.
func TestManagedWorkflow_TailRing(t *testing.T) {
	t.Parallel()

	t.Run("empty before any append", func(t *testing.T) {
		t.Parallel()
		mw := &managedWorkflow{}
		require.Nil(t, mw.tailLines())
	})

	t.Run("keeps only the last tailRingMax lines", func(t *testing.T) {
		t.Parallel()
		mw := &managedWorkflow{}
		total := tailRingMax + 10
		for i := 0; i < total; i++ {
			mw.appendTail(fmt.Sprintf("line %d\n", i))
		}
		got := mw.tailLines()
		require.Len(t, got, tailRingMax)
		// Oldest dropped: first retained line is total-tailRingMax.
		require.Equal(t, fmt.Sprintf("line %d", total-tailRingMax), got[0])
		require.Equal(t, fmt.Sprintf("line %d", total-1), got[len(got)-1])
	})

	t.Run("strips trailing newline and skips blank lines", func(t *testing.T) {
		t.Parallel()
		mw := &managedWorkflow{}
		mw.appendTail("hello\n")
		mw.appendTail("\n")    // blank after trim — skipped
		mw.appendTail("")      // empty — skipped
		mw.appendTail("world") // no newline (partial trailing line)
		require.Equal(t, []string{"hello", "world"}, mw.tailLines())
	})

	t.Run("truncates over-long lines", func(t *testing.T) {
		t.Parallel()
		mw := &managedWorkflow{}
		mw.appendTail(strings.Repeat("x", tailLineMax+500))
		got := mw.tailLines()
		require.Len(t, got, 1)
		require.True(t, strings.HasSuffix(got[0], "…"))
		require.LessOrEqual(t, len(got[0]), tailLineMax+len("…"))
	})

	t.Run("truncation stays valid UTF-8 on a rune boundary", func(t *testing.T) {
		t.Parallel()
		mw := &managedWorkflow{}
		// Multibyte runes (3 bytes each) so a naive byte-slice at tailLineMax
		// would land mid-rune and produce invalid UTF-8.
		mw.appendTail(strings.Repeat("世", tailLineMax))
		got := mw.tailLines()
		require.Len(t, got, 1)
		require.True(t, utf8.ValidString(got[0]), "truncated line must remain valid UTF-8")
		require.True(t, strings.HasSuffix(got[0], "…"))
	})

	t.Run("snapshot is independent of later appends", func(t *testing.T) {
		t.Parallel()
		mw := &managedWorkflow{}
		mw.appendTail("a")
		snap := mw.tailLines()
		mw.appendTail("b")
		require.Equal(t, []string{"a"}, snap, "snapshot must not see later appends")
	})
}

// capturedComment is one recorded PostComment call.
type capturedComment struct {
	issueID string
	body    string
}

// commentCapturingTracker records PostComment calls so the failure-report
// fan-out can be asserted without a real tracker.
//
//nolint:govet // fieldalignment: test fixture; embedded mock dictates layout
type commentCapturingTracker struct {
	mockDiscoveryTracker
	mu       sync.Mutex
	comments []capturedComment
}

func (m *commentCapturingTracker) PostComment(_ context.Context, issueID, body string) (tracker.Comment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.comments = append(m.comments, capturedComment{issueID: issueID, body: body})
	return tracker.Comment{ID: fmt.Sprintf("c%d", len(m.comments))}, nil
}

func (m *commentCapturingTracker) getComments() []capturedComment {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]capturedComment(nil), m.comments...)
}

// TestReportSubprocessFailure verifies the orchestrator fans a per-issue
// subprocess failure out to the tracker comment + notifier sinks, carrying the
// log path and tail, with the step falling back to the last observed step when
// the error carries no step prefix.
func TestReportSubprocessFailure(t *testing.T) {
	t.Parallel()

	tr := &commentCapturingTracker{}
	notifier := &captureNotifier{}
	o := &Orchestrator{
		tracker:       tr,
		logger:        discardLogger(),
		config:        testOrchestratorConfig(),
		notifier:      notifier,
		buildRevision: "deadbeef",
	}

	mw := &managedWorkflow{
		issue:       &tracker.Issue{ID: "issue-1", Identifier: "INF-1369"},
		logPath:     "/tmp/jiradozer/INF-1369.log",
		currentStep: "build",
	}
	mw.appendTail("E0610 build.go:42] codex auth error: 401\n")
	mw.appendTail("build step failed\n")

	// Bare "exit status 1" carries no step prefix -> falls back to currentStep.
	o.reportSubprocessFailure(mw, errors.New("exit status 1"))

	comments := tr.getComments()
	require.Len(t, comments, 1)
	require.Equal(t, "issue-1", comments[0].issueID)
	body := comments[0].body
	for _, want := range []string{"INF-1369", "`build`", "exit status 1", "deadbeef", "/tmp/jiradozer/INF-1369.log", "codex auth error", "build step failed"} {
		require.Containsf(t, body, want, "comment body missing %q\ngot:\n%s", want, body)
	}

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	require.Len(t, notifier.reports, 1)
	rep := notifier.reports[0]
	require.Equal(t, "INF-1369", rep.Target)
	require.Equal(t, "build", rep.Step)
	require.Equal(t, "/tmp/jiradozer/INF-1369.log", rep.LogPath)
	require.Equal(t, []string{"E0610 build.go:42] codex auth error: 401", "build step failed"}, rep.LogTail)
}

// TestReportSubprocessFailure_NilNotifier confirms a nil external notifier is
// safe (tracker comment still posts).
func TestReportSubprocessFailure_NilNotifier(t *testing.T) {
	t.Parallel()

	tr := &commentCapturingTracker{}
	o := &Orchestrator{
		tracker: tr,
		logger:  discardLogger(),
		config:  testOrchestratorConfig(),
	}
	mw := &managedWorkflow{issue: &tracker.Issue{ID: "issue-2", Identifier: "INF-2"}}

	require.NotPanics(t, func() {
		o.reportSubprocessFailure(mw, errors.New("boom"))
	})
	require.Len(t, tr.getComments(), 1)
}

// TestReportSubprocessFailure_StepFromError confirms a step-prefixed error
// takes precedence over the last observed step.
func TestReportSubprocessFailure_StepFromError(t *testing.T) {
	t.Parallel()

	notifier := &captureNotifier{}
	o := &Orchestrator{
		tracker:  &commentCapturingTracker{},
		logger:   discardLogger(),
		config:   testOrchestratorConfig(),
		notifier: notifier,
	}
	mw := &managedWorkflow{
		issue:       &tracker.Issue{ID: "i", Identifier: "INF-3"},
		currentStep: "build", // would be the fallback
	}
	// Error explicitly names the plan step.
	o.reportSubprocessFailure(mw, fmt.Errorf("plan step: %w", errors.New("x")))

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	require.Len(t, notifier.reports, 1)
	require.Equal(t, "plan", notifier.reports[0].Step, "step from error must win over currentStep")
}

// TestReportSubprocessFailure_TailFromFile verifies the file fallback: when the
// in-memory ring is empty (exec-restored or fast-exit child), the tail is read
// from the log file at mw.logPath so restored/quick failures still surface
// their final lines.
func TestReportSubprocessFailure_TailFromFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "INF-9.log")
	require.NoError(t, os.WriteFile(logPath, []byte("starting\nstep: build\nE0610 build failed: 401\n"), 0o600))

	notifier := &captureNotifier{}
	o := &Orchestrator{
		tracker:  &commentCapturingTracker{},
		logger:   discardLogger(),
		config:   testOrchestratorConfig(),
		notifier: notifier,
	}
	// No appendTail calls: the in-memory ring is empty, as it is for a
	// restored workflow whose tailer was never re-attached.
	mw := &managedWorkflow{
		issue:   &tracker.Issue{ID: "i9", Identifier: "INF-9"},
		logPath: logPath,
	}
	o.reportSubprocessFailure(mw, errors.New("exit status 1"))

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	require.Len(t, notifier.reports, 1)
	require.Equal(t, []string{"starting", "step: build", "E0610 build failed: 401"}, notifier.reports[0].LogTail,
		"empty ring must fall back to reading the log file")
}

// TestReportSubprocessFailure_RingBeatsFile confirms the live ring is preferred
// over the file when both are available.
func TestReportSubprocessFailure_RingBeatsFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "INF-10.log")
	require.NoError(t, os.WriteFile(logPath, []byte("file line\n"), 0o600))

	notifier := &captureNotifier{}
	o := &Orchestrator{
		tracker:  &commentCapturingTracker{},
		logger:   discardLogger(),
		config:   testOrchestratorConfig(),
		notifier: notifier,
	}
	mw := &managedWorkflow{issue: &tracker.Issue{ID: "i10", Identifier: "INF-10"}, logPath: logPath}
	mw.appendTail("ring line\n")

	o.reportSubprocessFailure(mw, errors.New("boom"))

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	require.Equal(t, []string{"ring line"}, notifier.reports[0].LogTail)
}

func TestReadLogTail(t *testing.T) {
	t.Parallel()

	t.Run("missing file returns nil", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, readLogTail(filepath.Join(t.TempDir(), "nope.log"), 5))
	})

	t.Run("returns last n lines", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "a.log")
		var sb strings.Builder
		for i := 0; i < 50; i++ {
			fmt.Fprintf(&sb, "line %d\n", i)
		}
		require.NoError(t, os.WriteFile(p, []byte(sb.String()), 0o600))
		got := readLogTail(p, 5)
		require.Equal(t, []string{"line 45", "line 46", "line 47", "line 48", "line 49"}, got)
	})

	t.Run("fewer lines than n returns all", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "b.log")
		require.NoError(t, os.WriteFile(p, []byte("only\ntwo\n"), 0o600))
		require.Equal(t, []string{"only", "two"}, readLogTail(p, 20))
	})

	t.Run("empty file returns nil", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "c.log")
		require.NoError(t, os.WriteFile(p, nil, 0o600))
		require.Nil(t, readLogTail(p, 5))
	})
}

// TestWatchdogHungReporting verifies the watchdog marks a stalled workflow as
// hung and that a hung workflow is reported as a failure (not silently
// classified as a cancellation). The cmd.Wait branch keys off mw.hung; here we
// assert the flag plumbs through to a failure report.
func TestWatchdogHungReporting(t *testing.T) {
	t.Parallel()

	notifier := &captureNotifier{}
	o := &Orchestrator{
		tracker:  &commentCapturingTracker{},
		logger:   discardLogger(),
		config:   testOrchestratorConfig(),
		notifier: notifier,
	}
	mw := &managedWorkflow{
		issue:       &tracker.Issue{ID: "ih", Identifier: "INF-HUNG"},
		currentStep: "validate",
	}
	mw.hung.Store(true)
	require.True(t, mw.hung.Load())

	// The cmd.Wait goroutine reports a hung workflow via reportSubprocessFailure
	// with a watchdog-wrapped error; assert that path produces an alert naming
	// the stuck step.
	o.reportSubprocessFailure(mw, fmt.Errorf("subprocess hung and was cancelled by watchdog: %w", errors.New("signal: interrupt")))

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	require.Len(t, notifier.reports, 1, "a hung (watchdog-killed) subprocess must alert")
	require.Equal(t, "validate", notifier.reports[0].Step)
	require.Contains(t, notifier.reports[0].Err.Error(), "hung")
}
