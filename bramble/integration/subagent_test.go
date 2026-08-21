//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
)

// stubModel routes to the scripted stand-in: bramble picks a backend from the
// model ID, and any gpt-* model means "run the codex binary".
const stubModel = "gpt-5.5"

// stubCursorModel routes to the cursor stand-in the same way: a composer-*
// model means "run the binary named agent". It is the hookless backend, so a
// session on it is only ever seen to finish by reading its pane.
const stubCursorModel = "composer-3"

// reportMarker is the prefix of every report bramble generates for a parent.
const reportMarker = "[bramble] subagent"

// deliveredReportMarker counts reports the recipient actually took a turn on.
//
// Counting reportMarker alone over-counts badly: the text appears once where it
// was pasted, once in the stand-in's echo of the submitted line, and once in its
// reply. Only the reply means the prompt was submitted and a turn ran, which is
// what "the parent was told" has to mean.
const deliveredReportMarker = "STUB-REPLY " + reportMarker

// TestSubagentLineageIsRecorded pins the link a subagent's whole return path
// hangs on: without a recorded parent nothing knows where to report.
func TestSubagentLineageIsRecorded(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubModel, string(parent), "CHILD-BOOT")
	dumpPanesOnFailure(t, h, parent, child)

	var found bool
	for _, s := range h.sessions() {
		if s.ID != string(child) {
			continue
		}
		found = true
		assert.Equal(t, string(parent), s.ParentSessionID)
		assert.Equal(t, stubModel, s.Model)
	}
	require.True(t, found, "child session missing from list-sessions")

	// A subagent with no branch of its own works on its parent's tree.
	for _, s := range h.sessions() {
		if s.ID == string(child) {
			assert.Equal(t, "main", s.WorktreeName)
		}
	}
}

// TestTopLevelSessionHasNoParent guards the other direction: an ordinary
// session must not acquire a parent it never asked for and start mailing it.
func TestTopLevelSessionHasNoParent(t *testing.T) {
	h := newHarness(t, true)

	solo := h.spawn("builder", stubModel, "", "SOLO-BOOT")
	h.awaitStatus(solo, "idle")

	for _, s := range h.sessions() {
		if s.ID == string(solo) {
			assert.Empty(t, s.ParentSessionID)
		}
	}
}

// TestSubagentReportsToParentOnItsOwn is the headline behaviour: a subagent
// finishes and its parent is told, with a path to the full output.
//
// This is what makes a non-Claude backend usable as a subagent at all. Codex
// has no system prompt, no MCP and no tool restrictions through this wrapper,
// so it cannot be reliably instructed to call back — bramble generates the
// report from the session's own state instead.
func TestSubagentReportsToParentOnItsOwn(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubModel, string(parent), "CHILD-ANSWERS-THIS")
	dumpPanesOnFailure(t, h, parent, child)

	h.awaitPane(parent, reportMarker, "parent was never told its subagent finished")

	report := h.pane(parent)
	assert.Contains(t, report, string(child), "the report should name the child")
	assert.Contains(t, report, "result:", "the report should point at the child's output")
	// Pointer, not payload: the child's actual answer belongs in the file, not
	// pasted into the parent's context.
	assert.NotContains(t, report, "STUB-REPLY CHILD-ANSWERS-THIS",
		"the report should carry a path, not the child's transcript")
}

// TestSubagentNotifyHookMarksItIdle covers the hook that everything else waits
// on. Without it a codex window sits at "running" forever after answering:
// nothing drains its queue, and its parent hears only when the window dies.
func TestSubagentNotifyHookMarksItIdle(t *testing.T) {
	h := newHarness(t, true)

	child := h.spawn("codetalk", stubModel, "", "ANSWER-ME")
	dumpPanesOnFailure(t, h, child)

	h.awaitPane(child, "STUB-REPLY ANSWER-ME", "the agent never answered")
	h.awaitStatus(child, "idle")
}

// TestQueuedDeliveryWaitsForIdle is the reason --queue exists. Typing into a
// live turn lands the text in the recipient's *next* prompt, stripped of the
// context that made it make sense.
func TestQueuedDeliveryWaitsForIdle(t *testing.T) {
	h := newHarness(t, true)

	target := h.spawn("builder", stubModel, "", "FIRST-TURN")
	h.awaitStatus(target, "idle")
	dumpPanesOnFailure(t, h, target)

	// Put the session mid-turn, then queue behind it. SLOW-TURN keeps the stub
	// busy only briefly, so the assertion below races it deliberately: what
	// matters is that the queued text is not written *while* it is running.
	_, err := h.send("", target, "SLOW-TURN", false)
	require.NoError(t, err)

	result, err := h.send("", target, "QUEUED-BEHIND", true)
	require.NoError(t, err)

	if result.Queued {
		// The interesting case: held back rather than typed into a live turn.
		assert.Equal(t, 1, h.deliveryQueueLen(), "a queued delivery should be persisted")
		h.awaitPane(target, "STUB-REPLY QUEUED-BEHIND", "the queued message never landed")
	} else {
		// The session had already gone idle, so it was written immediately —
		// still correct, just not the path this test is aiming at.
		h.awaitPane(target, "STUB-REPLY QUEUED-BEHIND", "an immediate delivery never landed")
	}

	require.Eventually(t, func() bool { return h.deliveryQueueLen() == 0 },
		settleTimeout, pollInterval, "the queue should drain once delivered")
}

// TestQueuedDeliveryToTerminalSessionIsRefused stops a caller from queueing a
// message nothing will ever deliver.
func TestQueuedDeliveryToTerminalSessionIsRefused(t *testing.T) {
	h := newHarness(t, true)

	target := h.spawn("builder", stubModel, "", "BOOT")
	h.awaitStatus(target, "idle")

	// Killing the window ends the session.
	_, err := h.tmux("kill-window", "-t", h.tmuxTargetOf(target))
	require.NoError(t, err)
	h.awaitStatus(target, "completed", "failed", "stopped")

	_, err = h.send("", target, "TOO-LATE", true)
	require.Error(t, err, "a terminal session must refuse mail")
	assert.Equal(t, 0, h.deliveryQueueLen(), "nothing should be queued for a dead session")
}

// TestDeliveryReachesPaneInCopyMode is a regression test for a silent failure
// that is invisible from inside bramble.
//
// A pane someone scrolled back in sits in tmux copy mode, where the pager
// consumes every key — including the Enter that submits a delivered prompt.
// tmux reports success for both the paste and the key, so the message lands in
// the composer and is never read. Bramble leaves copy mode before writing.
func TestDeliveryReachesPaneInCopyMode(t *testing.T) {
	h := newHarness(t, true)

	target := h.spawn("builder", stubModel, "", "BOOT")
	h.awaitStatus(target, "idle")
	h.awaitPane(target, "STUB-REPLY BOOT", "the agent never answered its opening prompt")
	dumpPanesOnFailure(t, h, target)

	// Scroll the pane back, exactly as a human watching the session would.
	tmuxTarget := h.tmuxTargetOf(target)
	_, err := h.tmux("copy-mode", "-t", tmuxTarget)
	require.NoError(t, err)
	inMode, err := h.tmux("display-message", "-p", "-t", tmuxTarget, "#{pane_in_mode}")
	require.NoError(t, err)
	require.Equal(t, "1", strings.TrimSpace(inMode), "pane should be in copy mode")

	_, err = h.send("", target, "THROUGH-COPY-MODE", true)
	require.NoError(t, err)

	h.awaitPane(target, "STUB-REPLY THROUGH-COPY-MODE",
		"delivery to a pane in copy mode was swallowed")
}

// TestTwoWayConversationKeepsReporting is the whole feature end to end, and the
// case that stayed broken longest.
//
// A tmux session's status comes entirely from outside — its agent reports
// idleness and nothing reports the opposite — so a session bramble typed into
// stayed "idle" through the turn, and the notify that ended that turn was
// dropped by SetSessionIdle's guard. No state change meant no second report:
// every conversation went silent after one round.
func TestTwoWayConversationKeepsReporting(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubModel, string(parent), "ROUND-ONE")
	dumpPanesOnFailure(t, h, parent, child)

	// Round 1: the child answers and its parent is told.
	h.awaitPane(child, "STUB-REPLY ROUND-ONE", "the child never answered round one")
	require.Eventually(t, func() bool {
		return h.countInPane(parent, deliveredReportMarker) >= 1
	}, settleTimeout, pollInterval, "no report for round one")

	// The parent replies. Submitting must move the child off "idle", or the
	// notify that ends this turn is discarded and nothing more is ever said.
	_, err := h.send(parent, child, "ROUND-TWO", true)
	require.NoError(t, err)
	h.awaitPane(child, "STUB-REPLY ROUND-TWO", "the parent's reply never reached the child")

	// Round 2: the answer to the reply is reported too.
	h.awaitPaneCond(parent, func() bool {
		return h.countInPane(parent, deliveredReportMarker) >= 2
	}, "round two was never reported — the conversation went silent after one exchange")
}

// TestSubagentIsReportedOnceNotOnEveryStateChange keeps a finished subagent
// from nagging its parent. A session emits several state changes around one
// turn, and an earlier draft reported on each of them.
//
// The narrower rules — a completed that follows a report is silent, a failure
// never is — are pinned deterministically in the unit tests
// (TestCompletedAfterIdleIsSilent, TestFailureIsReportedEvenAfterAnIdleReport),
// which do not depend on how an agent CLI happens to exit.
func TestSubagentIsReportedOnceNotOnEveryStateChange(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubModel, string(parent), "ONE-AND-DONE")
	dumpPanesOnFailure(t, h, parent, child)

	h.awaitPane(parent, reportMarker, "no report at all")
	h.awaitStatus(child, "idle")

	// Give the watcher room to fire again on any later transition.
	require.Never(t, func() bool {
		return h.countInPane(parent, deliveredReportMarker) > 1
	}, 8*time.Second, pollInterval, "the parent was told more than once about one turn")
}

// TestReadoptedCursorSubagentIsStillSeenToFinish is the restart path, and the
// only test that runs the monitor loop a re-adopted session gets.
//
// A session bramble started is watched by runSession's loop; one bramble
// re-adopts after a restart is watched by monitorTrackedTmuxWindow instead.
// Cursor has no completion hook — its idleness is read off its pane — so a
// re-adopt loop that does not poll the pane leaves the subagent running
// forever: nothing drains its queued mail and its parent is never told it
// finished. That gap shipped once and is invisible to every unit test, because
// neither loop can be driven without a tmux server.
//
// The second turn is what matters. The first proves the child works before the
// restart; the assertion is on the one after it, which only the re-adopted
// loop can observe.
func TestReadoptedCursorSubagentIsStillSeenToFinish(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", stubCursorModel, string(parent), "BEFORE-RESTART")
	dumpPanesOnFailure(t, h, parent, child)

	// Before the restart the child is watched by the loop that started it.
	h.awaitPane(child, "CURSOR-STUB-REPLY BEFORE-RESTART", "the cursor stand-in never answered")
	h.awaitStatus(child, "idle")

	h.restart()

	// The re-adopted child takes another turn. Queued rather than typed
	// directly: this has to go through the courier, which is what needs the
	// child to be seen going idle again.
	h.awaitStatus(child, "idle")
	_, err := h.send(parent, child, "AFTER-RESTART", true)
	require.NoError(t, err)
	h.awaitPane(child, "CURSOR-STUB-REPLY AFTER-RESTART", "the message never reached the re-adopted child")

	// The whole point: the re-adopted loop must see the pane go quiet again.
	// Without it the child stays "running" for the rest of the process.
	h.awaitPaneCond(child, func() bool {
		return h.status(child) == "idle"
	}, "the re-adopted cursor session was never seen to finish its turn — nothing polls its pane")
}

// --- live backends -----------------------------------------------------------

// TestLiveSubagentTwoWay drives the real agent CLIs, one subtest per backend.
//
// The stubbed cases above pin bramble's own logic; these pin the part that only
// a real CLI can tell you. Every bug this feature shipped with was of that kind:
// codex not reporting idle, a session never leaving "running", a paste dropped
// while a TUI finished painting, an Enter eaten by copy mode. None of them are
// visible without the real thing in a real pane.
//
// They run by default and skip only when a backend is missing or logged out, so
// what is exercised depends on the machine rather than on remembering a flag.
// A live run costs real tokens on each backend it finds.
func TestLiveSubagentTwoWay(t *testing.T) {
	for _, backend := range liveBackends {
		t.Run(backend.provider, func(t *testing.T) {
			model := backend.require(t)
			h := newHarness(t, false)

			// The parent is Claude: that is the orchestrator in practice, and
			// it is the side that has to receive and read the report.
			parentModel := liveBackends[0].require(t)
			parent := h.spawn("builder", parentModel, "",
				"You are the PARENT in an automated test. Say exactly: PARENT READY. Then wait. Do not read or edit files.")
			h.awaitReady(parent)

			child := h.spawn("codetalk", model, string(parent),
				"Reply with exactly one line and nothing else: R1 ARTICHOKE. Do not read files. Do not run commands.")
			dumpPanesOnFailure(t, h, parent, child)

			// Round one. Reaching idle at all is the backend-specific part:
			// claude and codex report it through a hook, cursor has neither and
			// is read off its pane.
			h.awaitPaneClearingDialogs(child, "ARTICHOKE", "the subagent never answered round one")
			h.awaitStatus(child, "idle")
			h.awaitPaneCond(parent, func() bool {
				return h.countInPane(parent, reportMarker) >= 1
			}, "the parent was never told about its %s subagent", backend.provider)

			// The report is a pointer, so the path is the part the parent has
			// to be able to use. Asserting only that "result:" appeared would
			// pass on a report naming a file that was never written — which is
			// what a failed pane capture produces.
			resultPath, ok := reportedResultPath(h.pane(parent))
			require.Truef(t, ok, "the report carried no result path\n--- parent pane ---\n%s", h.pane(parent))
			body, err := os.ReadFile(resultPath)
			require.NoErrorf(t, err, "the reported result file is not readable: %s", resultPath)
			assert.Containsf(t, string(body), "ARTICHOKE",
				"the %s subagent's answer is missing from its result file %s", backend.provider, resultPath)

			// Round two: the parent replies, and the answer comes back. This is
			// the leg that stayed broken longest — a delivery has to move the
			// child off idle, or the turn it starts produces no state change and
			// the conversation goes quiet.
			result, err := h.send(parent, child,
				"R2: reply with exactly one line and nothing else: R2 CONFIRMED", true)
			if err != nil {
				// A modal in the recipient's pane blocks delivery, which is
				// correctly an error rather than an Enter into a menu. Answer it
				// and try once more before giving up.
				h.answerStartupDialogs(child, h.pane(child))
				result, err = h.send(parent, child,
					"R2: reply with exactly one line and nothing else: R2 CONFIRMED", true)
				require.NoErrorf(t, err, "could not deliver to the %s subagent", backend.provider)
			}
			require.False(t, result.Queued, "the child was idle, so this should have been written at once")

			h.awaitPaneClearingDialogs(child, "R2 CONFIRMED", "the subagent never answered round two")
			h.awaitPaneCond(parent, func() bool {
				return h.countInPane(parent, reportMarker) >= 2
			}, "round two was never reported for %s — the conversation went quiet after one exchange", backend.provider)
		})
	}
}

// TestLiveQueuedDeliveryWaitsForALiveTurn is the live counterpart to the
// stubbed queue tests, and covers two things nothing else does against a real
// CLI.
//
// First, the queue itself. Every other live assertion delivers to an *idle*
// child, so the deferred path — the reason the courier exists — was only ever
// exercised against a stand-in. Here the child is genuinely mid-turn.
//
// Second, and more valuable: that a real agent working for twenty seconds is
// not mistaken for an idle one. That is the harmful direction. Claude and codex
// report their own turn ends, but cursor's state is read off its pane, and a
// false idle there would release this queued message straight into the running
// turn — precisely what the queue prevents. The unit tests pin that against a
// synthetic pane; only this pins it against the real chrome.
func TestLiveQueuedDeliveryWaitsForALiveTurn(t *testing.T) {
	for _, backend := range liveBackends {
		t.Run(backend.provider, func(t *testing.T) {
			model := backend.require(t)
			h := newHarness(t, false)

			parent := h.spawn("builder", liveBackends[0].require(t), "",
				"You are the PARENT in an automated test. Say exactly: PARENT READY. Then wait.")
			h.awaitReady(parent)

			// A builder, not a codetalk: the child has to be able to run a
			// command to occupy itself for a known length of time.
			child := h.spawn("builder", model, string(parent), longTurnPrompt("LONG-DONE"))
			dumpPanesOnFailure(t, h, parent, child)
			h.awaitWorking(child, "sleep")

			result, err := h.send(parent, child, "QUEUED-MID-TURN: acknowledge with QUEUE-ACK", true)
			require.NoErrorf(t, err, "could not queue for the %s subagent", backend.provider)
			require.Truef(t, result.Queued,
				"a message sent to a %s subagent mid-turn should have been held, not written", backend.provider)
			assert.Equal(t, 1, h.deliveryQueueLen(), "the held message should be persisted")

			// While the turn runs: the session must stay running, and nothing
			// may be typed into it.
			watchFor := (longTurnSeconds - 6) * int(time.Second)
			h.neverDuring(child, time.Duration(watchFor), func() bool {
				return h.status(child) == "idle" || strings.Contains(h.pane(child), "QUEUED-MID-TURN")
			}, "the %s subagent was treated as idle mid-turn, or the queued message was typed into a live turn",
				backend.provider)

			// The turn ends, and only then does the message land.
			h.awaitPaneClearingDialogs(child, "LONG-DONE", "the subagent never finished its long turn")
			h.awaitPaneClearingDialogs(child, "QUEUED-MID-TURN", "the held message never landed after the turn ended")
			require.Eventually(t, func() bool { return h.deliveryQueueLen() == 0 },
				settleTimeout, pollInterval, "the queue should drain once delivered")
		})
	}
}

// concurrentSubagents is how many children the fan-out tests spawn. Enough for
// their reports to genuinely overlap without making the suite slow.
const concurrentSubagents = 3

// TestConcurrentSubagentsAllReport covers a fan-out: one parent, several
// subagents working at once, all reporting to the same place.
//
// This is where the queue takes concurrent writes — every child's completion
// races the others into one recipient's queue — and where reports can be lost
// quietly rather than loudly. A dropped report is not a crash: the parent
// simply waits forever for a subagent that already finished.
//
// The parent is deliberately left mid-turn while they finish, so every report
// has to queue rather than being written straight through.
func TestConcurrentSubagentsAllReport(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	// Occupy the parent so the reports pile up behind a live turn.
	_, err := h.send("", parent, "PARENT-BUSY", true)
	require.NoError(t, err)

	children := make([]session.SessionID, 0, concurrentSubagents)
	for i := 0; i < concurrentSubagents; i++ {
		child := h.spawn("codetalk", stubModel, string(parent), fmt.Sprintf("CHILD-%d-WORK", i))
		children = append(children, child)
	}
	dumpPanesOnFailure(t, h, append(children, parent)...)

	for _, child := range children {
		h.awaitStatus(child, "idle")
	}

	// Every child must be named in the parent's pane exactly once. Checking
	// the count as well as the presence is what catches a report delivered
	// twice, which is as wrong as one delivered never.
	for _, child := range children {
		want := deliveredReportMarker + " " + string(child)
		h.awaitPaneCond(parent, func() bool { return h.countInPane(parent, want) >= 1 },
			"the parent was never told about subagent %s", child)
	}
	for _, child := range children {
		assert.Equalf(t, 1, h.countInPane(parent, deliveredReportMarker+" "+string(child)),
			"subagent %s was reported more than once", child)
	}

	require.Eventually(t, func() bool { return h.deliveryQueueLen() == 0 },
		settleTimeout, pollInterval, "every queued report should drain")
}

// TestConcurrentSubagentsQueueDurablyWhileParentIsBusy pins the durability half
// of a fan-out.
//
// Reports that arrive while a parent is mid-turn live on disk until it is free,
// and a queue written from several goroutines at once used to lose some of them
// there. Nothing looked wrong at the time — delivery in the running process
// still worked off the in-memory queue — so the loss only showed up as a
// restart that dropped reports for subagents that had already finished.
//
// The parent is held busy deliberately: without that it answers each report the
// instant it lands and the queue never has more than one thing in it, which is
// not the case being tested.
func TestConcurrentSubagentsQueueDurablyWhileParentIsBusy(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	// Hold the parent mid-turn for long enough that every child finishes while
	// it is still busy.
	_, err := h.send("", parent, "STUB-SLEEP 8", true)
	require.NoError(t, err)
	h.awaitStatus(parent, "running")

	children := make([]session.SessionID, 0, concurrentSubagents)
	for i := 0; i < concurrentSubagents; i++ {
		children = append(children, h.spawn("codetalk", stubModel, string(parent), fmt.Sprintf("CHILD-%d-WORK", i)))
	}
	dumpPanesOnFailure(t, h, append(children, parent)...)
	for _, child := range children {
		h.awaitStatus(child, "idle")
	}

	// While the parent is still busy, every report must be on disk. This is
	// the state a restarted bramble would resume from.
	queued := h.queuedTextFor(parent)
	require.NotEmptyf(t, queued, "no report was queued while the parent was busy\n--- parent pane ---\n%s", h.pane(parent))
	for _, child := range children {
		assert.Containsf(t, queued, string(child),
			"the persisted queue is missing %s; a restart here would drop it", child)
	}

	// And they all still arrive once it frees up.
	for _, child := range children {
		want := deliveredReportMarker + " " + string(child)
		h.awaitPaneCond(parent, func() bool { return h.countInPane(parent, want) >= 1 },
			"subagent %s was queued but never delivered", child)
	}
}

// TestSubagentOnItsOwnWorktreeIsIsolated covers the other half of the worktree
// story. A subagent with no branch of its own works on its parent's tree; with
// `--create-worktree -b <branch>` it gets one of its own, on a new branch off
// the base.
//
// The isolation is the point: two subagents editing one tree would corrupt each
// other's work, so "it got its own worktree" has to be asserted against git,
// not just against the path bramble reports. And the return path has to keep
// working across that boundary — lineage travels by session ID, not by tree,
// but nothing proved it until now.
func TestSubagentOnItsOwnWorktreeIsIsolated(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	const branch = "sub/isolated"
	child, worktree := h.spawnOnNewWorktree("builder", stubModel, string(parent), branch, "main", "CHILD-ON-OWN-TREE")
	dumpPanesOnFailure(t, h, parent, child)

	assert.NotEqualf(t, h.worktreePath, worktree,
		"the subagent was put on its parent's tree instead of a new one")
	require.DirExists(t, worktree)

	// Ask git, not bramble: a path that exists proves nothing about whether a
	// worktree was actually added on the right branch off the right base.
	assert.Equal(t, branch, h.gitIn(worktree, "branch", "--show-current"),
		"the new worktree is not on the requested branch")
	assert.Equal(t,
		h.gitIn(h.worktreePath, "rev-parse", "main"),
		h.gitIn(worktree, "rev-parse", "HEAD"),
		"the new branch is not based on main")

	// Edits on one tree must not appear on the other.
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "child-only.txt"), []byte("x\n"), 0o644))
	assert.NoFileExists(t, filepath.Join(h.worktreePath, "child-only.txt"),
		"a file written on the subagent's tree showed up on its parent's")

	// bramble reports the session against its own worktree, which is what the
	// command centre and `list-sessions --parent` show.
	var found bool
	for _, s := range h.sessions() {
		if s.ID != string(child) {
			continue
		}
		found = true
		assert.Equal(t, string(parent), s.ParentSessionID)
		assert.Equal(t, filepath.Base(branch), s.WorktreeName)
	}
	require.True(t, found, "the subagent is missing from list-sessions")

	// The return path still works across the worktree boundary.
	h.awaitPane(parent, reportMarker, "a subagent on its own worktree never reported to its parent")
	_, err := h.send(parent, child, "CROSS-TREE-FOLLOWUP", true)
	require.NoError(t, err)
	h.awaitPane(child, "STUB-REPLY CROSS-TREE-FOLLOWUP",
		"a message to a subagent on another worktree never landed")
}

// TestSubagentWorktreeIsReusedNotDuplicated: a parent that respawns a subagent
// on the same branch — after a crash, or for a second attempt — should land on
// the existing tree rather than failing.
func TestSubagentWorktreeIsReusedNotDuplicated(t *testing.T) {
	h := newHarness(t, true)

	parent := h.spawn("builder", stubModel, "", "PARENT-BOOT")
	h.awaitStatus(parent, "idle")

	const branch = "sub/reused"
	first, firstPath := h.spawnOnNewWorktree("builder", stubModel, string(parent), branch, "main", "FIRST-ATTEMPT")
	h.awaitStatus(first, "idle")

	second, secondPath := h.spawnOnNewWorktree("builder", stubModel, string(parent), branch, "main", "SECOND-ATTEMPT")
	dumpPanesOnFailure(t, h, parent, first, second)

	assert.Equal(t, firstPath, secondPath,
		"respawning on the same branch should reuse the worktree, not make another")
	assert.NotEqual(t, first, second, "each spawn is still its own session")
	h.awaitPane(second, "STUB-REPLY SECOND-ATTEMPT", "the second subagent never ran")
}
