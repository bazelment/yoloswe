//go:build integration

package integration

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubModel routes to the scripted stand-in: bramble picks a backend from the
// model ID, and any gpt-* model means "run the codex binary".
const stubModel = "gpt-5.5"

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
	require.Eventuallyf(t, func() bool {
		return h.countInPane(parent, deliveredReportMarker) >= 2
	}, settleTimeout, pollInterval,
		"round two was never reported — the conversation went silent after one exchange\n--- parent pane ---\n%s",
		h.pane(parent))
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

// --- live backends -----------------------------------------------------------

// TestLiveCodexSubagentTwoWay runs the same conversation against a real codex.
//
// The stub cannot catch what only a real TUI does: announcing idleness before
// its prompt is ready to take a paste, or putting a modal in the way. Gate:
//
//	bazel test //bramble/integration:integration_test \
//	  --test_env=BRAMBLE_IT_CODEX_MODEL=gpt-5.5 --test_output=all
func TestLiveCodexSubagentTwoWay(t *testing.T) {
	model := liveBackend(t, "BRAMBLE_IT_CODEX_MODEL", "codex")
	h := newHarness(t, false)

	parent := h.spawn("builder", parentModel(t), "",
		"You are the PARENT in an automated test. Say exactly: PARENT READY. Then wait.")
	h.awaitStatus(parent, "idle")

	child := h.spawn("codetalk", model, string(parent),
		"Reply with exactly one line and nothing else: R1 word ARTICHOKE. Do not read files. Do not run commands.")
	dumpPanesOnFailure(t, h, parent, child)

	h.awaitPane(child, "ARTICHOKE", "codex never answered round one")
	require.Eventually(t, func() bool {
		return h.countInPane(parent, deliveredReportMarker) >= 1
	}, settleTimeout, pollInterval, "the parent was never told about its codex subagent")

	result, err := h.send(parent, child,
		"R2: reply with exactly one line: R2 confirmed ARTICHOKE", true)
	// A modal in the recipient's TUI (codex's rate-limit prompt, say) blocks
	// delivery. That is correctly an error rather than an Enter into a menu,
	// but it is the developer's box, not a bramble bug.
	if err != nil {
		t.Skipf("codex would not take the delivery (a modal in its pane?): %v", err)
	}
	require.False(t, result.Queued, "the child was idle, so this should have been written at once")

	h.awaitPane(child, "R2 confirmed", "codex never answered round two")
	require.Eventuallyf(t, func() bool {
		return h.countInPane(parent, deliveredReportMarker) >= 2
	}, settleTimeout, pollInterval,
		"round two was never reported\n--- parent pane ---\n%s", h.pane(parent))
}

// parentModel picks the backend for the parent in a live test. It defaults to
// Claude, since the parent only has to sit at a prompt and be written to.
func parentModel(t *testing.T) string {
	t.Helper()
	if m := os.Getenv("BRAMBLE_IT_PARENT_MODEL"); m != "" {
		return m
	}
	requireTool(t, "claude")
	return "sonnet"
}
