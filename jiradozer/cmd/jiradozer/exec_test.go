package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/wt"
)

func execTestApp(t *testing.T) *cliapp.App {
	return &cliapp.App{Logger: testMainLogger(t)}
}

// Running under an orchestrator would silence every failure on the fleet: the
// variable tells a child to suppress its own report because a parent will
// speak for it, and exec has no parent.
func TestExecRefusesToRunUnderAnOrchestrator(t *testing.T) {
	t.Setenv(jiradozer.OrchestratedEnvVar, "1")

	err := runExec(context.Background(), execTestApp(t), execArgs{issueID: "INF-1", repo: "kernel"})
	require.Error(t, err)
	require.Contains(t, err.Error(), jiradozer.OrchestratedEnvVar)
}

func TestExecRequiresExactlyOneTaskSource(t *testing.T) {
	t.Setenv(jiradozer.OrchestratedEnvVar, "")

	err := runExec(context.Background(), execTestApp(t), execArgs{repo: "kernel"})
	require.ErrorContains(t, err, "exactly one of --issue or --description")

	err = runExec(context.Background(), execTestApp(t), execArgs{
		repo: "kernel", issueID: "INF-1", description: "do a thing"})
	require.ErrorContains(t, err, "exactly one of --issue or --description")
}

// --repo cannot be inferred: a dispatched worker starts under tmux with cwd
// $HOME, which is neither a wt worktree nor a git repo. Failing here with a
// clear message beats failing later inside wt.
func TestResolveExecWTManagerRequiresRepo(t *testing.T) {
	_, err := resolveExecWTManager("")
	require.ErrorContains(t, err, "--repo is required")

	t.Setenv("WT_ROOT", t.TempDir())
	mgr, err := resolveExecWTManager("kernel")
	require.NoError(t, err)
	require.NotNil(t, mgr)
}

// The lock label is the ONLY cross-host claim. A flock lease is per-box, so
// without this check two machines happily take the same issue.
func TestCheckNotClaimedBlocksAForeignClaim(t *testing.T) {
	claimed := &tracker.Issue{
		Identifier: "INF-1",
		Labels:     []string{"ming-work", jiradozer.LockLabel},
	}

	x := &execRun{}
	err := x.checkNotClaimed(claimed)
	require.Error(t, err)
	require.Contains(t, err.Error(), jiradozer.LockLabel)
	require.Contains(t, err.Error(), "--force", "the message must say how to override a stale claim")

	// --force is the documented escape hatch for a claim left by a dead run.
	forced := &execRun{args: execArgs{force: true}}
	require.NoError(t, forced.checkNotClaimed(claimed))

	unclaimed := &execRun{}
	require.NoError(t, unclaimed.checkNotClaimed(&tracker.Issue{
		Identifier: "INF-2", Labels: []string{"ming-work"}}))
}

// claimRecordingTracker records label mutations in the order they happen, so a
// test can assert WHEN the claim was taken relative to the worktree — the
// ordering is the whole point, and a tracker that only remembers the final set
// of labels cannot see it.
type claimRecordingTracker struct {
	issue      *tracker.Issue
	fetchErr   error
	addLabelEr error
	// onAddLabel observes the run's state at the exact instant the claim goes
	// out, which is the only way to assert what a hard kill in that gap would
	// have left behind.
	onAddLabel func()
	calls      []string
}

func (c *claimRecordingTracker) FetchIssue(context.Context, string) (*tracker.Issue, error) {
	if c.fetchErr != nil {
		return nil, c.fetchErr
	}
	c.calls = append(c.calls, "fetch")
	return c.issue, nil
}

func (c *claimRecordingTracker) ListIssues(context.Context, tracker.IssueFilter) ([]*tracker.Issue, error) {
	return nil, nil
}

func (c *claimRecordingTracker) FetchComments(context.Context, string, time.Time) ([]tracker.Comment, error) {
	return nil, nil
}

func (c *claimRecordingTracker) FetchWorkflowStates(context.Context, string) ([]tracker.WorkflowState, error) {
	return nil, nil
}

func (c *claimRecordingTracker) PostComment(context.Context, string, string) (tracker.Comment, error) {
	return tracker.Comment{}, nil
}

func (c *claimRecordingTracker) UpdateIssueState(context.Context, string, string) error { return nil }

func (c *claimRecordingTracker) AddLabel(_ context.Context, _ string, label string) error {
	if c.onAddLabel != nil {
		c.onAddLabel()
	}
	if c.addLabelEr != nil {
		return c.addLabelEr
	}
	c.calls = append(c.calls, "add:"+label)
	return nil
}

func (c *claimRecordingTracker) RemoveLabel(_ context.Context, _ string, label string) error {
	c.calls = append(c.calls, "remove:"+label)
	return nil
}

// execRunForClaimOrdering wires run() so it reaches createWorktree and fails
// there. The manager is built on an empty root, so wt refuses before running a
// single git command — deterministic, and it needs no repository.
func execRunForClaimOrdering(t *testing.T, trk *claimRecordingTracker) *execRun {
	t.Helper()
	return &execRun{
		app:    execTestApp(t),
		logger: testMainLogger(t),
		// WorkDir carries the production default, not "". A fixture that leaves
		// it empty makes every "was a worktree created" assertion below pass
		// vacuously — which is how a `kept := cfg.WorkDir != ""` test survived
		// being wrong.
		cfg:   &jiradozer.Config{WorkDir: ".", BaseBranch: "main"},
		wtMgr: wt.NewManager(t.TempDir(), "kernel"),
		args:  execArgs{issueID: "INF-1", repo: "kernel"},
		runID: "r1",
		newTracker: func(*jiradozer.Config, string) (tracker.IssueTracker, error) {
			return trk, nil
		},
	}
}

// The lock label is the only claim another box can see. Taking it AFTER the
// worktree left a window as long as a full checkout in which a second dispatch
// read an unclaimed issue and started a duplicate run; it must be taken between
// the claim check and the checkout instead.
func TestTheIssueIsClaimedBeforeTheWorktreeIsCreated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	trk := &claimRecordingTracker{issue: &tracker.Issue{ID: "id-1", Identifier: "INF-1"}}
	x := execRunForClaimOrdering(t, trk)

	err := x.run(context.Background())

	require.Error(t, err, "the checkout must fail so the test observes only the pre-worktree path")
	require.NotContains(t, err.Error(), "already claimed")
	require.Equal(t, []string{"fetch", "add:" + jiradozer.LockLabel, "remove:" + jiradozer.LockLabel}, trk.calls,
		"the claim must land between reading the issue and creating the worktree")
}

// The label must not be attachable before the run-log exists.
//
// Both orders leave a window under a hard kill, but only one of them is
// recoverable. Label first, and a SIGKILL in the gap leaves an issue claimed
// with no record anywhere naming who took it — every later dispatch refuses it
// until a human passes --force. Run-log first, and the same kill leaves a
// record with a stale heartbeat and no label: `runs` and `gc` can both see it,
// and nothing is blocked meanwhile.
func TestTheRunIsRecordedBeforeTheIssueIsClaimed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	trk := &claimRecordingTracker{issue: &tracker.Issue{ID: "id-1", Identifier: "INF-1"}}
	x := execRunForClaimOrdering(t, trk)
	// AddLabel is the observation point: whatever the run-log looks like at the
	// instant the claim goes out is what a kill in the gap would have left.
	var runDirAtClaim string
	trk.onAddLabel = func() {
		if x.rl != nil {
			runDirAtClaim = x.rl.Dir()
		}
	}

	require.Error(t, x.run(context.Background()),
		"the checkout must fail so the test observes only the pre-worktree path")

	require.NotEmpty(t, runDirAtClaim, "the run-log must already exist when the label is attached")
	onDisk, err := jiradozer.LoadRunMeta(runDirAtClaim)
	require.NoError(t, err, "the record has to be READABLE at that instant, not merely allocated")
	assert.Equal(t, "INF-1", onDisk.IssueIdentifier)
}

// A claim that silently failed is WORSE than the old ordering: the run builds
// its worktree looking claimed while no other box can see any claim at all. The
// pre-worktree AddLabel is therefore the one tracker write in exec that is
// fatal.
func TestAFailedClaimStopsTheRunBeforeAnyWorktreeExists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	trk := &claimRecordingTracker{
		issue:      &tracker.Issue{ID: "id-1", Identifier: "INF-1"},
		addLabelEr: errors.New("tracker unavailable"),
	}
	x := execRunForClaimOrdering(t, trk)

	err := x.run(context.Background())

	require.ErrorContains(t, err, "claim INF-1")
	require.ErrorContains(t, err, "tracker unavailable")
	assert.Empty(t, x.worktreePath, "the run must stop before any checkout exists")
	assert.Equal(t, []string{"fetch"}, trk.calls,
		"a label that never attached must not then be removed: that would strip another host's claim")
	assert.Equal(t, jiradozer.RunStateFailed, x.rl.Meta().State,
		"the run-log outlives the failed claim, so it must be settled rather than left running")
}

// A --description run's label sits on a per-run local tracker nothing else can
// read, so it excludes nothing and must not be able to kill a live run.
func TestALocalIssueLabelFailureDoesNotKillTheRun(t *testing.T) {
	trk := &claimRecordingTracker{
		issue:      &tracker.Issue{ID: "id-1", Identifier: "local-1"},
		addLabelEr: errors.New("tracker unavailable"),
	}
	x := &execRun{logger: testMainLogger(t), tracker: trk, issue: trk.issue}

	require.Error(t, x.claim(context.Background()),
		"claim reports the failure; whether it is fatal is the caller's decision")
}

// A label nobody is working blocks every later dispatch until a human passes
// --force, so an aborted run must hand the issue back.
func TestAClaimIsReleasedWhenTheWorktreeNeverComesUp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	trk := &claimRecordingTracker{issue: &tracker.Issue{ID: "id-1", Identifier: "INF-1"}}
	x := execRunForClaimOrdering(t, trk)

	require.Error(t, x.run(context.Background()))

	require.Equal(t, "remove:"+jiradozer.LockLabel, trk.calls[len(trk.calls)-1],
		"an aborted run must leave the issue unclaimed")
}

// The release has to survive the context that carried the failure: by the time
// an abort unwinds, the run context is usually already cancelled, and this is
// the one write that must still land. finish() therefore builds its own.
func TestAClaimIsReleasedEvenOnACancelledContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	trk := &claimRecordingTracker{issue: &tracker.Issue{ID: "id-1", Identifier: "INF-1"}}
	x := execRunForClaimOrdering(t, trk)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, x.run(ctx))

	assert.Equal(t, "remove:"+jiradozer.LockLabel, trk.calls[len(trk.calls)-1],
		"a cancelled run context must not take the release down with it")
}

// Releasing is authorised by having CLAIMED, not by having a tracker. finish()
// now runs on paths where the AddLabel itself failed, and a failed add cannot
// be told apart from another host having claimed first — so an unconditional
// release would let a run that never held the lock strip the lock of the run
// that does.
func TestARunThatNeverClaimedDoesNotReleaseAnyoneElsesClaim(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	trk := &claimRecordingTracker{issue: &tracker.Issue{ID: "id-1", Identifier: "INF-1"}}
	x := newFinishTestRun(t)
	x.tracker = trk
	x.issue = trk.issue
	// claimed stays false: this run reached finish() without ever labelling.

	x.finish(errors.New("claim INF-1: tracker unavailable"))

	assert.Empty(t, trk.calls, "no RemoveLabel may be issued for a claim this run never took")
}

// A claim left on an issue nobody is working is invisible except in the log, so
// a failed release must be loud rather than a warning nobody reads.
func TestAFailedClaimReleaseIsReportedNotSwallowed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var logged bytes.Buffer
	x := newFinishTestRun(t)
	x.logger = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelError}))
	x.tracker = &failingRemoveLabelTracker{}
	x.issue = &tracker.Issue{ID: "id-1", Identifier: "INF-1"}
	x.claimed = true

	x.finish(nil)

	out := logged.String()
	assert.Contains(t, out, "INF-1", "a human needs to know which issue stays claimed")
	assert.Contains(t, out, jiradozer.LockLabel)
	assert.Contains(t, out, "tracker unavailable")
}

type failingRemoveLabelTracker struct{ claimRecordingTracker }

func (failingRemoveLabelTracker) RemoveLabel(context.Context, string, string) error {
	return errors.New("tracker unavailable")
}

// A lease is held INSIDE the worker for its whole lifetime, so a second worker
// on the same box cannot take the same task.
func TestLeaseExcludesASecondWorkerOnThisBox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	first, err := jiradozer.AcquireLease("INF-1")
	require.NoError(t, err)

	_, err = jiradozer.AcquireLease("INF-1")
	require.ErrorIs(t, err, jiradozer.ErrLeaseHeld)

	// A different task is unaffected.
	other, err := jiradozer.AcquireLease("INF-2")
	require.NoError(t, err)
	require.NoError(t, other.Release())

	require.NoError(t, first.Release())

	// Release leaves the lock FILE behind on purpose — removing it races a
	// process about to flock it. So the file existing must not block a retake;
	// only a held lock does. Counting files instead of held locks once made
	// every host exclude itself permanently.
	retaken, err := jiradozer.AcquireLease("INF-1")
	require.NoError(t, err, "a leftover lock file must not look like a held lease")
	require.NoError(t, retaken.Release())
}

// gc keys reclamation SOLELY on the recorded PR URL, and exec always keeps its
// worktree. A run that finishes without recording one therefore leaks its
// worktree permanently — the exact leak `jiradozer gc` exists to prevent.
func TestFinishRecordsThePRSoGCCanReclaimTheWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	x := newFinishTestRun(t)
	x.lookupPR = func(_ context.Context, branch, dir string) (*wt.PRInfo, error) {
		require.Equal(t, "jiradozer/INF-1", branch, "the PR is looked up by this run's branch")
		require.Equal(t, x.cfg.WorkDir, dir, "the lookup must run inside the worktree")
		return &wt.PRInfo{URL: "https://github.com/o/r/pull/42", Number: 42}, nil
	}

	x.finish(nil)

	m := x.rl.Meta()
	assert.Equal(t, "https://github.com/o/r/pull/42", m.PRURL)
	assert.Equal(t, 42, m.PRNumber)
	assert.True(t, m.WorktreeKept)
	assert.Equal(t, jiradozer.RunStateDone, m.State)

	// The record on disk is what a sweeper on this box actually reads.
	onDisk, err := jiradozer.LoadRunMeta(x.rl.Dir())
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/o/r/pull/42", onDisk.PRURL)
}

// A run that failed before create_pr legitimately has no PR. That must settle
// the run-log normally rather than abort the exit path.
func TestFinishToleratesARunWithNoPR(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	x := newFinishTestRun(t)
	x.lookupPR = func(context.Context, string, string) (*wt.PRInfo, error) {
		return nil, errors.New("no pull requests found for branch")
	}

	x.finish(errors.New("build: agent exited 1"))

	m := x.rl.Meta()
	assert.Empty(t, m.PRURL)
	assert.Equal(t, jiradozer.RunStateFailed, m.State)
	assert.Contains(t, m.Error, "agent exited 1")
	assert.True(t, m.WorktreeKept, "a failed run keeps its worktree; the work exists nowhere else")
}

// work_dir is the configured BASE directory, and LoadConfig defaults it to
// ".", so it is non-empty on every real run from config load onward. Reading it
// as "a checkout exists" makes a run that died before wt.New report a kept
// worktree at "." and sends a human after a directory that was never created —
// and, worse, invites `gh` to answer about whatever repository the process
// happened to be started in.
func TestFinishDoesNotClaimAWorktreeItNeverCreated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	x := newFinishTestRun(t)
	x.cfg.WorkDir = "." // the production default
	x.worktreePath = "" // createWorktree never ran
	x.lookupPR = func(context.Context, string, string) (*wt.PRInfo, error) {
		t.Fatal("no checkout exists, so there is nowhere to ask gh from")
		return nil, nil
	}

	x.finish(errors.New("create worktree for jiradozer/INF-1: not a git repository"))

	m := x.rl.Meta()
	assert.False(t, m.WorktreeKept, "nothing was kept; work_dir is not evidence of a checkout")
	assert.Empty(t, m.WorktreeKeptReason)
	assert.Equal(t, jiradozer.RunStateFailed, m.State)
}

// A cancellation is a stop, not a failure: recording it as failed would make
// every Ctrl-C look like a broken run to a fleet-wide listing.
func TestFinishRecordsACancellationAsCancelled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	x := newFinishTestRun(t)
	x.finish(fmt.Errorf("plan: %w", context.Canceled))

	assert.Equal(t, jiradozer.RunStateCancelled, x.rl.Meta().State)
}

func newFinishTestRun(t *testing.T) *execRun {
	t.Helper()
	runID, err := jiradozer.NewRunID()
	require.NoError(t, err)
	rl, err := jiradozer.NewRunLog(jiradozer.RunMeta{
		RunID:           runID,
		IssueIdentifier: "INF-1",
		Repo:            "kernel",
		Branch:          "jiradozer/INF-1",
		WorktreePath:    t.TempDir(),
		State:           jiradozer.RunStateRunning,
	})
	require.NoError(t, err)
	// A run that reaches finish() normally has a checkout, and createWorktree
	// points both fields at it — so the fixture does too.
	checkout := t.TempDir()
	return &execRun{
		app:          execTestApp(t),
		logger:       testMainLogger(t),
		cfg:          &jiradozer.Config{WorkDir: checkout},
		worktreePath: checkout,
		rl:           rl,
		runID:        runID,
		branch:       "jiradozer/INF-1",
		lookupPR: func(context.Context, string, string) (*wt.PRInfo, error) {
			return nil, errors.New("no pull requests found for branch")
		},
	}
}

// Phase is what makes a dispatched worker's progress readable from another box:
// `jiradozer fleet runs` reads meta.json over ssh and can see neither this
// host's log nor its terminal.
func TestRecordPhaseMirrorsWorkflowStepsIntoTheRunLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	x := newFinishTestRun(t)
	require.Empty(t, x.rl.Meta().Phase)

	x.recordPhase(jiradozer.StepBuilding)

	// The step's own name, not its numeric value: a WorkflowStep is an int, so
	// a plain conversion would write an unprintable rune into the listing.
	assert.Equal(t, jiradozer.StepBuilding.String(), x.rl.Meta().Phase)
	assert.NotEmpty(t, x.rl.Meta().Phase)

	onDisk, err := jiradozer.LoadRunMeta(x.rl.Dir())
	require.NoError(t, err)
	assert.Equal(t, jiradozer.StepBuilding.String(), onDisk.Phase,
		"a remote reader sees meta.json, never this process's memory")
}

// stubGH answers `gh auth status` so a test can drive the real wt.Manager
// without a GitHub credential. createWorktree calls wt.New WITHOUT SkipFetch,
// so FetchOrigin reaches CheckGitHubAuth and any test that gets past the
// checkout would otherwise shell out to the real `gh`.
type stubGH struct{}

func (stubGH) Run(context.Context, []string, string) (*wt.CmdResult, error) {
	return &wt.CmdResult{}, nil
}

// runToCompletionAndLoadTrail drives a whole successful lifecycle and reads
// the trail back off disk, the way a dispatcher on another box does.
//
// Every other exec fixture deliberately fails the checkout, because
// runStepAgent is unexported and a package main test cannot fake the agent.
// Skipping all four phases is the way around that: skipCompletedOrConfigured-
// Phases walks plan→build→validate→ship, finds each one skipped, and
// forceTransitions straight to StepDone without ever invoking an agent.
func runToCompletionAndLoadTrail(t *testing.T) []jiradozer.Event {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	root := t.TempDir()
	repo := "kernel"
	require.NoError(t, os.MkdirAll(filepath.Join(root, repo, ".bare"), 0o755))
	mgr := wt.NewManager(root, repo,
		wt.WithGitRunner(&recordingGit{}),
		wt.WithGHRunner(stubGH{}),
		wt.WithOutput(wt.NewOutput(io.Discard, false)))

	cfg := &jiradozer.Config{WorkDir: ".", BaseBranch: "main"}
	// Set directly rather than through ApplySkipPhases: the field is the input
	// the workflow reads, and naming all four phases is what drives the run to
	// StepDone with no agent.
	cfg.SkipPhases = []string{"plan", "build", "validate", "ship"}

	trk := &claimRecordingTracker{issue: &tracker.Issue{ID: "id-1", Identifier: "INF-1"}}
	x := &execRun{
		app:    execTestApp(t),
		logger: testMainLogger(t),
		cfg:    cfg,
		wtMgr:  mgr,
		args:   execArgs{issueID: "INF-1", repo: repo},
		runID:  "r1",
		newTracker: func(*jiradozer.Config, string) (tracker.IssueTracker, error) {
			return trk, nil
		},
		lookupPR: func(context.Context, string, string) (*wt.PRInfo, error) {
			return nil, errors.New("no pull requests found for branch")
		},
	}

	require.NoError(t, x.run(context.Background()),
		"all four phases are skipped, so the workflow runs to StepDone with no agent")

	events, err := jiradozer.LoadEvents(x.rl.Dir())
	require.NoError(t, err)
	// NotEmpty here, and deliberately: LoadEvents returns (nil, nil) for a
	// missing file, so every assertion that merely ranges over events passes
	// vacuously on a run that wrote nothing at all.
	require.NotEmpty(t, events, "a completed run must leave an event trail, not an empty directory")
	return events
}

// meta.json cannot substitute for the trail: it is a SNAPSHOT — Phase is
// overwritten on every transition and State on every settle — so the sequence
// of what happened, and when, survives nowhere else.
func TestACompletedRunWritesItsEventTrail(t *testing.T) {
	events := runToCompletionAndLoadTrail(t)

	kinds := make([]string, 0, len(events))
	for _, ev := range events {
		assert.False(t, ev.At.IsZero(), "an event with no timestamp cannot order a trail")
		assert.NotEmpty(t, ev.Kind)
		kinds = append(kinds, ev.Kind)
	}

	assert.Contains(t, kinds, jiradozer.EventWorktreeCreated)
	assert.Contains(t, kinds, jiradozer.EventPhase)

	// The trail is ordered and bracketed: a reader replaying it has to see the
	// run open before it closes, whatever happened in between.
	assert.Equal(t, jiradozer.EventRunStarted, kinds[0])
	assert.Equal(t, jiradozer.EventRunFinished, kinds[len(kinds)-1])
}

func TestTheEventTrailRecordsEveryPhaseTheRunPassedThrough(t *testing.T) {
	events := runToCompletionAndLoadTrail(t)

	var phases []string
	var finished *jiradozer.Event
	for _, ev := range events {
		switch ev.Kind {
		case jiradozer.EventPhase:
			phases = append(phases, ev.Detail)
		case jiradozer.EventRunFinished:
			finished = &ev
		}
	}

	// meta.Phase holds only the LAST of these. The sequence is exactly what the
	// trail adds over the snapshot, so pin more than one.
	require.NotEmpty(t, phases)
	assert.Contains(t, phases, jiradozer.StepDone.String(),
		"the run reached StepDone, so the trail has to say so")
	// The phase's NAME, not its numeric value: a WorkflowStep is an int, so a
	// plain conversion would write an unprintable rune into the trail.
	assert.Equal(t, jiradozer.StepDone.String(), phases[len(phases)-1])

	// The terminal state belongs in the trail too, or a reader replaying it
	// cannot tell a run that finished from one that was killed mid-flight.
	require.NotNil(t, finished, "the trail must record how the run ended")
	assert.Equal(t, string(jiradozer.RunStateDone), finished.Detail)
}

// These are methods on a half-built execRun, and Append on a nil RunLog panics
// — which would take a run down over a log line.
func TestEventAppendsAreSkippedWhenNoRunLogExists(t *testing.T) {
	trk := &claimRecordingTracker{issue: &tracker.Issue{ID: "id-1", Identifier: "local-1"}}
	x := &execRun{logger: testMainLogger(t), tracker: trk, issue: trk.issue}

	require.NotPanics(t, func() {
		_ = x.claim(context.Background())
		x.recordPhase(jiradozer.StepBuilding)
	}, "a helper firing before the run-log exists must not panic on a nil run-log")
}

// dispatch refuses a duplicate by asking which box holds this task's lease —
// a check that runs BEFORE the worker exists. It can therefore only work if
// both sides derive the same name, which is why this is one function.
func TestLeaseTargetIsDerivedIdenticallyForDispatchAndExec(t *testing.T) {
	assert.Equal(t, "inf-1234", leaseTarget(execArgs{issueID: "INF-1234", taskID: "t-7"}),
		"an issue identifier wins: it is the cross-host claim")
	assert.Equal(t, "t-7", leaseTarget(execArgs{taskID: "t-7", description: "tidy things"}))
	assert.Equal(t, "", leaseTarget(execArgs{}))
}

// The guard only excludes a duplicate when both spellings of one issue reach
// one lock name. The trackers already fold these — GitHub reports
// "owner/repo#42" whichever form it was handed, and the local tracker
// lowercases before it looks for a file — so a lease keyed on the raw CLI
// string let the same issue be dispatched twice under two spellings, with
// nothing catching it until both runs had built worktrees.
func TestOneIssueLeasesOneNameHoweverItIsSpelled(t *testing.T) {
	canonical := leaseTarget(execArgs{issueID: "acme/app#42"})
	assert.Equal(t, "acme/app#42", canonical)
	assert.Equal(t, canonical, leaseTarget(execArgs{issueID: "https://github.com/acme/app/issues/42"}),
		"the URL and the shorthand are the same issue to the tracker")
	assert.Equal(t, canonical, leaseTarget(execArgs{issueID: "  acme/app#42  "}))

	assert.Equal(t, leaseTarget(execArgs{issueID: "LOCAL-1"}), leaseTarget(execArgs{issueID: "local-1"}),
		"the local tracker resolves both to one file, so they must lease one name")
	assert.Equal(t, leaseTarget(execArgs{taskID: "T-7"}), leaseTarget(execArgs{taskID: "t-7"}))

	// Folding must not go so far that two different issues collide.
	assert.NotEqual(t, canonical, leaseTarget(execArgs{issueID: "acme/app#43"}))
	assert.NotEqual(t, canonical, leaseTarget(execArgs{issueID: "acme/other#42"}))
	// A string no tracker can parse is still usable as a lock name, unchanged
	// apart from case — refusing it would disable the guard entirely.
	assert.Equal(t, "some-odd-id", leaseTarget(execArgs{issueID: "SOME-ODD-ID"}))
}

// A --description run with no --task-id used to lease a per-run random name, so
// `dispatch` had nothing to look for and two concurrent dispatches of the same
// task both proceeded — duplicate worktrees, duplicate PRs.
func TestLeaseTargetIsStableForADescriptionRun(t *testing.T) {
	x := execArgs{description: "tidy the helm chart"}
	first := leaseTarget(x)
	assert.NotEmpty(t, first)
	assert.Equal(t, first, leaseTarget(x), "the same task must lease the same name every time")
	assert.Equal(t, first, leaseTarget(execArgs{description: "  tidy the helm chart\n"}),
		"incidental whitespace must not make a task look new")
	assert.NotEqual(t, first, leaseTarget(execArgs{description: "tidy the ingress"}))

	// It becomes a lock FILENAME, so free-form text must not survive verbatim.
	assert.NotContains(t, leaseTarget(execArgs{description: "fix /etc/hosts\nand more"}), "/")
	assert.NotContains(t, leaseTarget(execArgs{description: "fix /etc/hosts\nand more"}), "\n")
}

// A description is the only task identifier here that is free-form, so it is
// the only one two unrelated tasks can collide on. Unscoped, "update the deps"
// against two repos leased the same name and dispatch refused the second as
// already-running somewhere.
func TestADescriptionLeaseIsScopedToItsRepo(t *testing.T) {
	kernel := leaseTarget(execArgs{description: "update the deps", repo: "kernel"})
	web := leaseTarget(execArgs{description: "update the deps", repo: "web"})

	assert.NotEqual(t, kernel, web, "the same words against two repos are two tasks")
	assert.Equal(t, kernel, leaseTarget(execArgs{description: "update the deps", repo: "kernel"}),
		"dispatch and exec must still derive the same name for the same task")

	// The repo boundary must be unambiguous, or two different (repo,
	// description) pairs whose concatenation matches would collide anyway.
	assert.NotEqual(t,
		leaseTarget(execArgs{repo: "ab", description: "cd"}),
		leaseTarget(execArgs{repo: "a", description: "bcd"}))

	// An issue or task id is already unique fleet-wide. Folding the repo in
	// would WEAKEN it: the same issue dispatched at two repos would stop
	// excluding itself, and an issue is claimed once.
	assert.Equal(t,
		leaseTarget(execArgs{issueID: "INF-1", repo: "kernel"}),
		leaseTarget(execArgs{issueID: "INF-1", repo: "web"}))
	assert.Equal(t,
		leaseTarget(execArgs{taskID: "t-7", repo: "kernel"}),
		leaseTarget(execArgs{taskID: "t-7", repo: "web"}))
}

// An alert with no target names nothing an on-call human can act on.
func TestReportTargetIsNeverAnonymous(t *testing.T) {
	assert.Equal(t, "INF-9", (&execRun{args: execArgs{issueID: "INF-9"}}).reportTarget(),
		"a failure before the issue fetch must still name the issue")
	assert.Equal(t, "INF-9", (&execRun{
		issue: &tracker.Issue{Identifier: "INF-9"},
		args:  execArgs{issueID: "inf-9"},
	}).reportTarget(), "the resolved identifier wins once it is known")
	assert.Equal(t, "t-7", (&execRun{args: execArgs{taskID: "t-7"}}).reportTarget())
	assert.Equal(t, "tidy the helm chart",
		(&execRun{args: execArgs{description: "tidy the helm chart"}}).reportTarget())
}

// A GitHub-style identifier must not turn a branch into nested path segments.
func TestSanitizeBranchLeaf(t *testing.T) {
	require.Equal(t, "acme-app-42", sanitizeBranchLeaf("acme/app#42"))
	require.Equal(t, "INF-1234", sanitizeBranchLeaf("INF-1234"))
	require.Equal(t, "a-b", sanitizeBranchLeaf("a b"))
}

// gc's liveness guard reads the lease name off the run-log. If exec does not
// write it, the guard asks about the wrong lock for every --description run and
// answers "not held" about a live worker.
func TestStartRunLogRecordsTheLeaseItHolds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	x := &execRun{
		app:    execTestApp(t),
		logger: testMainLogger(t),
		cfg:    &jiradozer.Config{WorkDir: t.TempDir(), BaseBranch: "main"},
		args:   execArgs{description: "tidy the helm chart", repo: "kernel"},
		runID:  "r1",
		branch: "jiradozer/r1",
	}
	require.NoError(t, x.startRunLog())

	want := leaseTarget(x.args)
	require.NotEmpty(t, want)
	assert.Equal(t, want, x.rl.Meta().LeaseTarget)
	assert.Equal(t, want, x.rl.Meta().LeaseKey(),
		"before a local issue exists the lease name is the only name this run has")

	onDisk, err := jiradozer.LoadRunMeta(x.rl.Dir())
	require.NoError(t, err)
	assert.Equal(t, want, onDisk.LeaseTarget, "gc reads meta.json, not this process's memory")
}

// LeaseKey returns "" when a record cannot name its lock, and gc reads that as
// "never reclaim" — permanently, since nothing revisits a terminal run. That is
// the right reading of a damaged record, but it is only affordable because a
// record this writer produces is never that shape. exec accepts exactly two run
// shapes, so pinning both here is what keeps the refusal a safety net rather
// than a slow disk leak.
func TestEveryRunShapeRecordsALeaseName(t *testing.T) {
	shapes := map[string]execArgs{
		"issue":       {issueID: "INF-1", repo: "kernel"},
		"description": {description: "tidy the helm chart", repo: "kernel"},
		// --task-id rides along with either, and must not displace the name.
		"description with task id": {description: "tidy it", taskID: "T-7", repo: "kernel"},
	}
	for name, args := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())

			x := &execRun{
				app:    execTestApp(t),
				logger: testMainLogger(t),
				cfg:    &jiradozer.Config{WorkDir: t.TempDir(), BaseBranch: "main"},
				args:   args,
				runID:  "r1",
				branch: "jiradozer/r1",
			}
			require.NoError(t, x.startRunLog())

			onDisk, err := jiradozer.LoadRunMeta(x.rl.Dir())
			require.NoError(t, err)
			require.Equal(t, leaseTarget(args), onDisk.LeaseTarget)
			require.NotEmpty(t, onDisk.LeaseKey(),
				"a run gc cannot ask about is a worktree gc can never reclaim")
		})
	}
}

// The run-log IS gc's ownership namespace: these are ordinary wt worktrees
// sitting beside human-owned ones, so gc's only discovery path is walking
// run-logs, and it skips a run directory with no readable meta.json. A checkout
// created before that first write is therefore a directory nothing can ever
// name — no teardown reaches it, because a hard kill runs no teardown at all.
// The record has to exist BEFORE the checkout can, not after it.
func TestTheRunLogIsWrittenBeforeTheWorktreeCanExist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	trk := &claimRecordingTracker{issue: &tracker.Issue{ID: "id-1", Identifier: "INF-1"}}
	x := execRunForClaimOrdering(t, trk)

	require.Error(t, x.run(context.Background()), "the checkout must fail, so only the pre-worktree path is observed")

	runs, err := jiradozer.ListRuns()
	require.NoError(t, err)
	require.Len(t, runs, 1, "a run whose checkout never came up must still be recorded")
	assert.Equal(t, x.plannedWorktreePath(), runs[0].WorktreePath,
		"gc reclaims by this path; a record without one is a run gc cannot act on")
	assert.NotEmpty(t, runs[0].WorktreePath)
	assert.Equal(t, jiradozer.RunStateFailed, runs[0].State)
	assert.False(t, runs[0].WorktreeKept,
		"nothing was kept: claiming otherwise sends a human after a directory that does not exist")
}

// recordingGit captures the git command lines a wt.Manager issues, so a test
// can drive the real manager without a repository.
type recordingGit struct{ cmds [][]string }

func (g *recordingGit) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	g.cmds = append(g.cmds, args)
	if len(args) > 1 && args[0] == "branch" && args[1] == "--show-current" {
		return &wt.CmdResult{Stdout: "jiradozer/INF-1\n"}, nil
	}
	return &wt.CmdResult{}, nil
}

// The run-log records where the checkout WILL be, before wt.New is called, so
// the prediction has to match what wt actually does. If wt ever changes how it
// derives the path, the prediction would go stale silently and every run-log
// would point gc at a directory that does not exist — so the check drives the
// real manager rather than restating the join.
func TestThePlannedWorktreePathIsWhereWtActuallyPutsIt(t *testing.T) {
	root := t.TempDir()
	repo := "kernel"
	require.NoError(t, os.MkdirAll(filepath.Join(root, repo, ".bare"), 0o755))

	mgr := wt.NewManager(root, repo, wt.WithGitRunner(&recordingGit{}), wt.WithOutput(wt.NewOutput(io.Discard, false)))
	x := &execRun{
		logger: testMainLogger(t),
		cfg:    &jiradozer.Config{BaseBranch: "main"},
		wtMgr:  mgr,
		args:   execArgs{issueID: "INF-1", repo: repo},
		runID:  "r1",
	}
	x.deriveBranch()

	// SkipFetch only to keep the test off the network and off `gh auth`; the
	// path derivation under test happens before either.
	actual, err := mgr.New(context.Background(), x.branch, "main", "", wt.NewOptions{SkipFetch: true})
	require.NoError(t, err)
	assert.Equal(t, actual, x.plannedWorktreePath(),
		"the path recorded before the checkout must be the path the checkout gets")
}

// Before a branch is named there is nothing to predict, and a join against an
// empty branch would name the repository root — a path gc must never be handed.
func TestThereIsNoPlannedWorktreePathBeforeABranchIsNamed(t *testing.T) {
	x := &execRun{
		cfg:   &jiradozer.Config{},
		wtMgr: wt.NewManager(t.TempDir(), "kernel"),
	}

	assert.Empty(t, x.plannedWorktreePath(), "an unnamed run must not claim the repository root")
}
