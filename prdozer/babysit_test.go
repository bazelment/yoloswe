package prdozer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/wt"
)

// busyProbeBlob is a heavily loaded box, so scoring prefers the other one.
const busyProbeBlob = `__NPROC__
8
__LOAD__
14.0 13.0 12.0 9/100 1
__DF__
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/root        506771172 345397612 161357176      69% /
__TMUX__
90
__LEASES__
__GH__
ok
__BIN__
/usr/bin/prdozer
__END__
`

// noPrdozerProbeBlob is a reachable box missing the binary.
const noPrdozerProbeBlob = `__NPROC__
8
__LOAD__
0.1 0.1 0.1 1/100 1
__DF__
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/root        506771172 345397612 161357176      69% /
__TMUX__
1
__LEASES__
__GH__
ok
__BIN__
MISSING
__END__
`

// writeFleetFixture creates a two-box fleet registry under home.
func writeFleetFixture(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "magent", "fleet")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	write := func(name, hostname, dns string) {
		body := fmt.Sprintf(`{"cloud":"aws","cron_style":"legacy","hostname":%q,`+
			`"public_dns":%q,"registered":"2026-07-26","roles":"","ssh_user":"ubuntu","sync_offset":""}`,
			hostname, dns)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("self.json", "self-box", "self.example")
	write("other.json", "other-box", "other.example")
}

// fakeGit is a no-op GitRunner for tests that never reach real git.
type fakeGit struct{}

func (fakeGit) Run(_ context.Context, _ []string, _ string) (*wt.CmdResult, error) {
	return &wt.CmdResult{}, nil
}

func TestPlanDispatch_SelfHostRunsInProcess(t *testing.T) {
	// Never SSH to yourself: if the best box is the one you are on, run
	// in-process.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFleetFixture(t, home)

	ssh := &fakeSSH{out: map[string]string{
		"ubuntu@self.example":  awsProbeBlob,
		"ubuntu@other.example": busyProbeBlob,
	}}
	plan, err := PlanDispatch(context.Background(), ssh, DispatchOptions{
		OwnerRepo: "o/r",
		PRNumber:  1,
		Probe:     ProbeOptions{SelfDNS: "self.example"},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "self-box", plan.Chosen.Host, "the idle box wins")
	assert.True(t, plan.RanLocal, "a self-host choice must run in-process, not over SSH")
}

func TestPlanDispatch_ProducesRunnableCommandAndScores(t *testing.T) {
	// --dry-run shares this decision path with the real dispatch: a dry run
	// that computes a different answer than the real thing is worse than none.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFleetFixture(t, home)

	ssh := &fakeSSH{out: map[string]string{
		"ubuntu@self.example":  busyProbeBlob,
		"ubuntu@other.example": awsProbeBlob,
	}}
	plan, err := PlanDispatch(context.Background(), ssh, DispatchOptions{
		OwnerRepo:    "sycamore-labs/kernel",
		PRNumber:     8123,
		RegistryPath: "~/magent/prdozer/registry.yaml",
		Probe:        ProbeOptions{SelfDNS: "self.example"},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "other-box", plan.Chosen.Host)
	assert.False(t, plan.RanLocal)
	assert.Len(t, plan.Scores, 2, "the full score table must be available to print")

	assert.Contains(t, plan.Command, "ssh ")
	assert.Contains(t, plan.Command, "ubuntu@other.example")
	assert.Contains(t, plan.Command, "babysit-local")
	assert.Contains(t, plan.Command, "8123")
	assert.NotContains(t, plan.Command, "flock",
		"the worker takes the lease itself; flock(1) around tmux does not hold")
}

func TestPlanDispatch_PinnedHostIsHonoured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFleetFixture(t, home)

	ssh := &fakeSSH{out: map[string]string{
		"ubuntu@self.example":  awsProbeBlob,
		"ubuntu@other.example": busyProbeBlob,
	}}
	// "other-box" is the busier machine, so only an explicit pin selects it.
	plan, err := PlanDispatch(context.Background(), ssh, DispatchOptions{
		OwnerRepo: "o/r", PRNumber: 1, Host: "other-box",
		Probe: ProbeOptions{SelfDNS: "self.example"},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "other-box", plan.Chosen.Host)
}

func TestPlanDispatch_UnknownPinnedHostErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFleetFixture(t, home)

	_, err := PlanDispatch(context.Background(), &fakeSSH{}, DispatchOptions{
		OwnerRepo: "o/r", PRNumber: 1, Host: "no-such-box",
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the fleet registry")
}

func TestPlanDispatch_NoEligibleHostStillReturnsScores(t *testing.T) {
	// "No eligible host" is only actionable if the caller can print why each
	// candidate was rejected.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFleetFixture(t, home)

	ssh := &fakeSSH{out: map[string]string{
		"ubuntu@self.example":  noPrdozerProbeBlob,
		"ubuntu@other.example": noPrdozerProbeBlob,
	}}
	plan, err := PlanDispatch(context.Background(), ssh, DispatchOptions{OwnerRepo: "o/r", PRNumber: 1}, nil)
	require.Error(t, err)
	assert.Len(t, plan.Scores, 2, "the score table must survive the error for printing")
	assert.Contains(t, err.Error(), "PATH")
}

func TestBabysitter_RefusesUnusableRepo(t *testing.T) {
	t.Parallel()
	b := NewBabysitter(newFakeGH(), &fakeGit{}, nil, nil, BabysitOptions{
		OwnerRepo: "sycamore-labs/sycaweave",
		PRNumber:  1,
		// No worktree_root: the repo has no local clone.
		Entry: RepoEntry{Flow: "pr-polish"},
	})
	state, err := b.Run(context.Background())
	require.Error(t, err)
	assert.Equal(t, TerminalFailed, state)
	assert.Contains(t, err.Error(), "worktree_root")
}

func TestBabysitter_RefusesWhenLeaseHeld(t *testing.T) {
	// A duplicate dispatch must not produce a second babysitter on one PR.
	home := t.TempDir()
	t.Setenv("HOME", home)

	held, err := AcquireLease("o/r", 42)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Release() })

	// A USABLE entry, because Run checks the layout before it ever reaches the
	// lease: a bare temp dir fails that first check, and the test would then
	// pass on the wrong error while the lease went unexercised.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "main", ".git"), 0o755))

	b := NewBabysitter(newFakeGH(), &fakeGit{}, nil, nil, BabysitOptions{
		OwnerRepo: "o/r",
		PRNumber:  42,
		Entry: RepoEntry{
			WorktreeRoot: root, Layout: LayoutWT, BaseBranch: "main", Flow: "pr-polish",
		},
	})
	_, err = b.Run(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLeaseHeld)
}

func TestWatcherConfig_AutoMergeOnButPolicyDecides(t *testing.T) {
	t.Parallel()
	// AutoMerge is enabled so the loop can REACH a merge; the policy is what
	// decides whether anything lands. With "notify" nothing merges.
	b := NewBabysitter(nil, nil, nil, nil, BabysitOptions{
		PRNumber: 7,
		Entry: RepoEntry{
			MergePolicy: MergePolicyNotify,
			BaseBranch:  "main",
			Model:       "opus",
		},
	})
	cfg := b.watcherConfig(&RunContext{WorktreePath: "/tmp/wt"})
	assert.True(t, cfg.Polish.AutoMerge)
	assert.Equal(t, MergePolicyNotify, cfg.Polish.MergePolicy)
	assert.Equal(t, "/tmp/wt", cfg.WorkDir)
	assert.Equal(t, "opus", cfg.Agent.Model)
	assert.Equal(t, []int{7}, cfg.Source.PRs)
}

func TestTerminalFor(t *testing.T) {
	t.Parallel()
	pr := DiscoveredPR{Number: 42}
	cases := []struct {
		action   LastAction
		want     TerminalState
		terminal bool
	}{
		{LastActionMerged, TerminalMerged, true},
		{LastActionClosed, TerminalClosed, true},
		{LastActionNeedsHuman, TerminalNeedsHuman, true},
		// Non-terminal: the loop keeps going.
		{LastActionPolished, "", false},
		{LastActionReworked, "", false},
		{LastActionArmed, "", false},
		{LastActionIdle, "", false},
		{LastActionFailed, "", false},
		// Stalled is non-terminal BY DESIGN, and is the entry most likely to be
		// "fixed" into a terminal one: it shares a name with TickResult.Stalled,
		// which is terminal. They mean opposite things — this action says one
		// invocation was cut off mid-round and the cooldown brake should handle
		// it, while the flag says a whole streak produced no round and the run is
		// over. Ending the run here would hand a human every PR the brake would
		// have recovered on its own.
		{LastActionStalled, "", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.action), func(t *testing.T) {
			t.Parallel()
			got, _, done := terminalFor(TickResult{Action: tc.action}, pr)
			assert.Equal(t, tc.terminal, done)
			if tc.terminal {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// A divergence stop and an approval block are both LastActionNeedsHuman but
// call for opposite responses, so the message must tell them apart — and must
// quote the streak the guard tripped on, not the run's cumulative round count.
func TestTerminalFor_DivergedSaysWhy(t *testing.T) {
	t.Parallel()
	pr := DiscoveredPR{Number: 42}

	state, msg, done := terminalFor(TickResult{
		Action: LastActionNeedsHuman, Diverged: true,
		RoundsSinceImprovement: 3, PolishRounds: 17,
	}, pr)
	require.True(t, done)
	assert.Equal(t, TerminalNeedsHuman, state)
	assert.Contains(t, msg, "stopped improving")
	assert.Contains(t, msg, "3 polish rounds produced no better result",
		"must report the flat streak, not the 17 rounds the run did in total")
	assert.Contains(t, msg, "17 rounds total")

	_, plain, done := terminalFor(TickResult{Action: LastActionNeedsHuman}, pr)
	require.True(t, done)
	assert.NotContains(t, plain, "stopped improving",
		"a PR blocked on an approval must not be reported as diverging")
}

// A stall, a divergence and an approval block are all LastActionNeedsHuman and
// all call for different responses — so, like divergence, a stall must say so.
//
// The generic message is actively misleading here: it sends an operator looking
// for a reviewer or a merge policy when the actual fault is the agent backend
// never returning a round (kernel#8374).
func TestTerminalFor_StalledSaysWhy(t *testing.T) {
	t.Parallel()
	pr := DiscoveredPR{Number: 42}

	state, msg, done := terminalFor(TickResult{
		Action: LastActionNeedsHuman, Stalled: true,
		InvocationsSinceRound: 6, StallError: "turn forced complete after grace period",
	}, pr)
	require.True(t, done)
	assert.Equal(t, TerminalNeedsHuman, state)
	assert.Contains(t, msg, "stalled")
	assert.Contains(t, msg, "6 polish invocations produced no completed round")
	assert.Contains(t, msg, "turn forced complete after grace period",
		"must name the cause; a polish stall has no LastMergeError to fall back on")
	assert.NotContains(t, msg, "stopped improving",
		"a stall is not a divergence: no round ever returned to be scored")

	_, plain, done := terminalFor(TickResult{Action: LastActionNeedsHuman}, pr)
	require.True(t, done)
	assert.NotContains(t, plain, "stalled",
		"a PR blocked on an approval must not be reported as a stall")
}

func TestTerminalFor_ArmedIsNotTerminal(t *testing.T) {
	t.Parallel()
	// --auto only ARMS the merge queue; the PR has not landed. Treating this
	// as terminal would report "merged" for a PR still sitting in the queue.
	_, _, done := terminalFor(TickResult{Action: LastActionArmed}, DiscoveredPR{Number: 1})
	assert.False(t, done, "an armed auto-merge must keep polling until .merged is true")
}

// poll_interval bounds how OFTEN prdozer looks at a PR. Sleeping the full
// interval AFTER each tick instead adds the wait to however long the tick took,
// so a long polish round is penalized twice.
//
// kernel#8374: 4.1h of agent work spread over 11.5h wall-clock, with gaps of 96,
// 81, 73, 62, 59, 55, 55, 51, 43, 33, 33, 29 and 20 minutes against a 20-minute
// interval. ~64% of that run was waiting.
func TestNextPollDelay_MeasuresFromTickStart(t *testing.T) {
	t.Parallel()
	interval := 20 * time.Minute

	// A tick shorter than the interval waits only the remainder.
	assert.Equal(t, 15*time.Minute, nextPollDelay(interval, 5*time.Minute),
		"a 5-minute tick on a 20-minute interval leaves 15 minutes, not a fresh 20")

	// A tick that already outlasted the interval is overdue: go now.
	assert.Equal(t, time.Duration(0), nextPollDelay(interval, 29*time.Minute),
		"the next look is overdue once the tick outlasts the interval")

	// Exactly at the boundary is also due.
	assert.Equal(t, time.Duration(0), nextPollDelay(interval, interval))
}

// The busy-loop guard. A tick that finished inside its budget keeps the full
// remaining pacing no matter what it did — including a polish round, whose
// pushed commit re-arms self_review and would otherwise chain rounds
// back-to-back on agent budget until the divergence guard trips. Pacing is the
// only other brake on that loop.
func TestNextPollDelay_FastTicksKeepPacing(t *testing.T) {
	t.Parallel()
	interval := 20 * time.Minute
	got := nextPollDelay(interval, 2*time.Second)
	assert.Greater(t, got, 19*time.Minute,
		"a fast tick must not spin: pacing is the API and agent budget's only protection")
}

// The zero-wait path is reached after ticks that outlasted the interval — the
// long, expensive ones, after which a shutdown is most likely pending. Skipping
// the pause must not also skip the cancellation check.
func TestWaitForNextPoll_ZeroWaitStillHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(t, waitForNextPoll(ctx, 0),
		"an overdue tick on a cancelled babysit must stop, not start another tick")
	assert.False(t, waitForNextPoll(ctx, -time.Minute),
		"a negative delay is the same overdue case")
}

func TestWaitForNextPoll_ZeroWaitProceedsWhenLive(t *testing.T) {
	t.Parallel()
	assert.True(t, waitForNextPoll(context.Background(), 0),
		"an overdue tick on a live babysit runs immediately")
}

func TestWaitForNextPoll_CancelInterruptsALongWait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- waitForNextPoll(ctx, time.Hour) }()
	cancel()
	select {
	case ok := <-done:
		assert.False(t, ok, "cancelling mid-wait ends the loop instead of sleeping out the hour")
	case <-time.After(5 * time.Second):
		t.Fatal("waitForNextPoll did not return after its context was cancelled")
	}
}

func TestWaitForNextPoll_ElapsedWaitProceeds(t *testing.T) {
	t.Parallel()
	assert.True(t, waitForNextPoll(context.Background(), time.Millisecond),
		"a wait that simply elapses means the next tick is due")
}

// Guards one thing: that the loop's zero-wait path is still an interruption
// point. It says nothing about the interval math — see
// TestBabysitLoop_SpacesTicksFromTickStart for that.
//
// The interval is a nanosecond, so every tick outlasts it and the loop takes
// the zero-wait path on every pass. A loop that skipped the cancellation check
// there would spin forever on a cancelled context, so this hangs — hence the
// hard deadline — rather than silently passing.
func TestBabysitLoop_ZeroWaitPathStopsOnCancel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gh := newFakeGH() // no responses registered: every tick errors and keeps polling

	runLog, err := NewRunLog(RunMeta{Repo: "o/r", PRNumber: 42, RunID: "cancel-test"})
	require.NoError(t, err)

	b := NewBabysitter(gh, nil, nil, nil, BabysitOptions{
		OwnerRepo:    "o/r",
		PRNumber:     42,
		PollInterval: time.Nanosecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	type result struct {
		state  TerminalState
		detail string
	}
	done := make(chan result, 1)
	go func() {
		state, detail := b.loop(ctx, &RunContext{WorktreePath: t.TempDir()}, runLog, DiscoveredPR{Number: 42})
		done <- result{state, detail}
	}()

	select {
	case got := <-done:
		assert.Equal(t, TerminalRunning, got.state)
		assert.Equal(t, "context cancelled", got.detail,
			"an overdue tick must still observe cancellation before starting the next one")
	case <-time.After(10 * time.Second):
		t.Fatal("loop did not stop on a cancelled context: the zero-wait path is spinning")
	}
}

// pacedGH times how the loop spaces its ticks. Every Run call is one tick's
// first gh call — the snapshot bails there because nothing parses — so the
// recorded times ARE the tick start times. The first call blocks for tickCost
// to stand in for a slow tick, which is the whole point: the interval must be
// measured across that cost, not added to it.
type pacedGH struct {
	secondTick chan struct{}
	starts     []time.Time
	tickCost   time.Duration
	mu         sync.Mutex
}

func (p *pacedGH) Run(_ context.Context, _ []string, _ string) (*wt.CmdResult, error) {
	p.mu.Lock()
	p.starts = append(p.starts, time.Now())
	n := len(p.starts)
	p.mu.Unlock()
	switch n {
	case 1:
		time.Sleep(p.tickCost) // simulated tick work, not synchronization
	case 2:
		close(p.secondTick)
	}
	return &wt.CmdResult{}, nil
}

func (p *pacedGH) tickStarts() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.starts...)
}

// The drift regression, pinned at the loop rather than on nextPollDelay alone:
// a tick that costs 1.5s on a 2s interval must be followed by the next tick 2s
// after it STARTED, not 3.5s later.
//
// This is what fails if tickStart moves below the Tick call, if time.Since is
// dropped, or if the loop goes back to sleeping the full interval afterwards —
// none of which the helper unit tests or the cancellation test can see.
//
// The assertion window is the midpoint between the two behaviours, so it has
// 750ms of slack in each direction.
func TestBabysitLoop_SpacesTicksFromTickStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const (
		interval = 2 * time.Second
		tickCost = 1500 * time.Millisecond
	)
	gh := &pacedGH{tickCost: tickCost, secondTick: make(chan struct{})}

	runLog, err := NewRunLog(RunMeta{Repo: "o/r", PRNumber: 42, RunID: "drift-test"})
	require.NoError(t, err)

	b := NewBabysitter(gh, nil, nil, nil, BabysitOptions{
		OwnerRepo:    "o/r",
		PRNumber:     42,
		PollInterval: interval,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.loop(ctx, &RunContext{WorktreePath: t.TempDir()}, runLog, DiscoveredPR{Number: 42})

	select {
	case <-gh.secondTick:
	case <-time.After(30 * time.Second):
		t.Fatal("the loop never started a second tick")
	}
	cancel()

	starts := gh.tickStarts()
	require.GreaterOrEqual(t, len(starts), 2)
	gap := starts[1].Sub(starts[0])
	assert.Less(t, gap, interval+tickCost/2,
		"the interval is measured from the tick's start, so a 1.5s tick on a 2s interval "+
			"must not push the next tick out to 3.5s")
	assert.GreaterOrEqual(t, gap, interval-tickCost/2,
		"pacing still applies: a tick that finished inside its budget waits out the remainder")
}

// endsTheRun (watcher.go) gates the merge brake on "does this tick end the
// run", and terminalFor is what actually ends it. They are separate functions
// in separate files, so pin them against each other over the full action set:
// a new terminal action added to one and not the other would silently let the
// brake arm a cooldown on a run that is already over.
func TestEndsTheRun_MatchesTerminalFor(t *testing.T) {
	t.Parallel()
	for _, action := range []LastAction{
		LastActionMerged, LastActionClosed, LastActionNeedsHuman,
		LastActionPolished, LastActionReworked, LastActionArmed,
		LastActionIdle, LastActionFailed, LastActionStalled,
		LastActionTransient, LastActionDryRun,
	} {
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()
			_, _, done := terminalFor(TickResult{Action: action}, DiscoveredPR{Number: 42})
			assert.Equal(t, done, endsTheRun(action),
				"endsTheRun must agree with terminalFor for %q", action)
		})
	}
}

// selfLoginGH answers the login lookup and nothing else.
type selfLoginGH struct {
	mu sync.Mutex
}

func (g *selfLoginGH) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if strings.Contains(strings.Join(args, " "), "api user") {
		return &wt.CmdResult{Stdout: "mzhaom\n"}, nil
	}
	// Anything else: an empty-but-valid payload keeps the caller moving.
	return &wt.CmdResult{Stdout: "{}"}, nil
}

// A scope stop is the fourth kind of LastActionNeedsHuman, and the one the
// generic message inverts outright: every round SUCCEEDED. "Blocked on a review
// approval" sends the operator to find a reviewer, when what the PR needs is
// someone to decide what to cut — the same misdiagnosis Diverged and Stalled
// were added to prevent.
func TestTerminalFor_RatchetedSaysWhy(t *testing.T) {
	t.Parallel()
	pr := DiscoveredPR{Number: 42}

	state, msg, done := terminalFor(TickResult{
		Action: LastActionNeedsHuman, Ratcheted: true,
		PolishCommits: 23, PolishCommitLimit: 12, PolishRounds: 7,
	}, pr)
	require.True(t, done)
	assert.Equal(t, TerminalNeedsHuman, state)
	assert.Contains(t, msg, "scope cap")
	assert.Contains(t, msg, "23 commits", "the operator needs the number that tripped the guard")
	assert.Contains(t, msg, "limit 12")
	assert.NotContains(t, msg, "review approval",
		"nothing here is waiting on a reviewer")

	_, plain, done := terminalFor(TickResult{Action: LastActionNeedsHuman}, pr)
	require.True(t, done)
	assert.NotContains(t, plain, "scope cap",
		"a PR blocked on an approval must not be reported as over-scoped")
}

// The watcher babysit builds must carry the self login.
//
// Without it w.self is "", so Snapshot never marks a comment IsSelf, and
// NewComments — an unconditional polish trigger — fires on prdozer's own
// replies. The run then polishes in response to itself: kernel#7040 read
// UNRES=0 / SUCCESS / APPROVED on every tick and still ran six rounds, with the
// comment count climbing in lockstep with the rounds producing it. The
// orchestrator path has always passed WithSelfLogin; only babysit did not.
//
// Asserted on the constructed watcher rather than through a running loop, and
// that is the whole point of splitting newWatcher out. The bug was an option
// never passed, which is invisible from outside a loop: a loop-level test can
// only see that `gh api user` was called, and that still passes with
// WithSelfLogin deleted. newWatcher is also the loop's only watcher
// construction path (babysit.go), so this covers the loop too.
//
// The other half of the chain — w.self reaching the trigger — is
// TestComputeChangeset_NewComments_IgnoresSelf.
func TestBabysitNewWatcher_CarriesSelfLoginIntoTheWatcher(t *testing.T) {
	t.Parallel()
	gh := &selfLoginGH{}
	b := NewBabysitter(gh, nil, nil, nil, BabysitOptions{OwnerRepo: "o/r", PRNumber: 42})

	w := b.newWatcher(context.Background(), &RunContext{WorktreePath: t.TempDir()}, nil)

	assert.Equal(t, "mzhaom", w.self,
		"without the login every self-reply reads as a new comment and starts another round")
}

// A login lookup that fails must disable the filter, not the run: the same
// best-effort behaviour the orchestrator path already has.
func TestBabysitNewWatcher_LoginFailureLeavesTheRunAlive(t *testing.T) {
	t.Parallel()
	gh := newFakeGH()
	gh.failPrefix("api user", "gh: not authenticated")
	b := NewBabysitter(gh, nil, nil, nil, BabysitOptions{OwnerRepo: "o/r", PRNumber: 42})

	w := b.newWatcher(context.Background(), &RunContext{WorktreePath: t.TempDir()}, nil)

	require.NotNil(t, w, "a failed lookup must not take the run down with it")
	assert.Empty(t, w.self)
}
