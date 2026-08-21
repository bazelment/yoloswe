package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cursorPane renders the footer cursor-agent keeps at the bottom of its pane.
// working=true adds the hint it shows for exactly as long as a turn runs.
func cursorPane(working bool, transcript ...string) []string {
	return cursorPaneMode(working, false, transcript...)
}

// cursorPaneMode renders the footer with or without the extra mode line cursor
// shows in plan mode, which is what a codetalk subagent runs in.
func cursorPaneMode(working, planMode bool, transcript ...string) []string {
	lines := append([]string{}, transcript...)
	prompt := "  → Add a follow-up"
	if working {
		prompt += "                    ctrl+c to stop"
	}
	lines = append(lines, prompt)
	if planMode {
		lines = append(lines, "  Plan (shift+tab to cycle)")
	}
	return append(lines,
		"  Composer 2.5 · 7.6%                    Run Everything",
		"  /tmp/wt · master",
	)
}

// TestCursorPaneWorkingIsNotIdle guards the trap this probe exists to record.
//
// "Add a follow-up" is present the whole time, working or not — reading it as
// an idle marker would release queued mail into a live turn, which is exactly
// what the queue is for. Only "ctrl+c to stop" distinguishes the two.
func TestCursorPaneWorkingIsNotIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCursor, cursorPane(true, "  1", "  2"))
	require.True(t, known, "the footer is present, so the pane is readable")
	assert.False(t, idle, "a running turn must never read as idle")
}

func TestCursorPaneIdleIsIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCursor, cursorPane(false, "  30"))
	require.True(t, known)
	assert.True(t, idle)
}

// TestUnpaintedPaneIsUnknown keeps a still-booting CLI from being called idle
// before it has shown a prompt at all.
func TestUnpaintedPaneIsUnknown(t *testing.T) {
	t.Parallel()

	_, known := paneShowsIdle(ProviderCursor, []string{"", "loading...", ""})
	assert.False(t, known, "a pane with no recognizable chrome tells us nothing")
}

// TestProvidersWithHooksHaveNoProbe: claude and codex report their own turn
// ends, and a second, weaker signal could only contradict them.
func TestProvidersWithHooksHaveNoProbe(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{ProviderClaude, ProviderCodex} {
		assert.False(t, providerHasIdleProbe(provider), "provider %s", provider)
		assert.Nil(t, newPaneIdleTracker(provider), "provider %s", provider)
	}
	assert.True(t, providerHasIdleProbe(ProviderCursor))
	assert.NotNil(t, newPaneIdleTracker(ProviderCursor))
}

// TestTrackerNeedsConsecutiveObservations stops one half-painted frame from
// releasing queued mail.
func TestTrackerNeedsConsecutiveObservations(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	assert.False(t, tr.observe(cursorPane(false)), "one observation is not enough")
	assert.True(t, tr.observe(cursorPane(false)), "two in a row means idle")
}

// TestTrackerStreakResetsWhenWorkResumes: a flicker back to working must
// restart the count, not carry it.
func TestTrackerStreakResetsWhenWorkResumes(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	require.False(t, tr.observe(cursorPane(false)))
	require.False(t, tr.observe(cursorPane(true)), "still working")
	assert.False(t, tr.observe(cursorPane(false)), "the streak restarted")
	assert.True(t, tr.observe(cursorPane(false)))
}

// TestTrackerFiresOnceUntilReset pins the transition being edge-triggered: the
// monitor marks the session idle once, and only new work re-arms it.
func TestTrackerFiresOnceUntilReset(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	require.False(t, tr.observe(cursorPane(false)))
	require.True(t, tr.observe(cursorPane(false)))
	assert.False(t, tr.observe(cursorPane(false)), "no repeat while it stays idle")

	// A delivered message starts a turn; the monitor resets the tracker.
	tr.reset()
	require.False(t, tr.observe(cursorPane(false)))
	assert.True(t, tr.observe(cursorPane(false)), "a fresh idle is reported again")
}

// TestProbeReadsOnlyTheFooter keeps the agent from talking its own session into
// a state change by quoting the marker back.
func TestProbeReadsOnlyTheFooter(t *testing.T) {
	t.Parallel()

	transcript := make([]string, 0, 20)
	for i := 0; i < 12; i++ {
		transcript = append(transcript, "  the hint is: ctrl+c to stop")
	}
	idle, known := paneShowsIdle(ProviderCursor, cursorPane(false, transcript...))
	require.True(t, known)
	assert.True(t, idle, "an old line in the transcript must not read as working")
}

// TestNilTrackerIsInert: providers with hooks get a nil tracker, and the
// monitor calls straight through it.
func TestNilTrackerIsInert(t *testing.T) {
	t.Parallel()

	var tr *paneIdleTracker
	assert.False(t, tr.observe(cursorPane(false)))
	assert.NotPanics(t, func() { tr.reset() })
}

// TestCursorPlanModeWorkingIsNotIdle is the case that a fixed trailing-lines
// window got wrong. A codetalk subagent runs cursor in plan mode, which adds a
// mode line to the footer and pushes "ctrl+c to stop" further from the bottom.
// Reading a window of trailing lines would miss it and call a running turn
// idle — releasing queued mail into it. The hint is looked for on the composer
// line itself, so footer height does not matter.
func TestCursorPlanModeWorkingIsNotIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCursor, cursorPaneMode(true, true, "  thinking"))
	require.True(t, known, "the composer line is present")
	assert.False(t, idle, "a running turn in plan mode must not read as idle")
}

func TestCursorPlanModeIdleIsIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCursor, cursorPaneMode(false, true, "  done"))
	require.True(t, known)
	assert.True(t, idle)
}

// TestPaneIdleTrackerComesFromTheStoredModel pins the input the re-adopt path
// has to work from. monitorTrackedTmuxWindow never sees a resolved agent model
// — only the model string the session was persisted with — so if that string
// does not yield a provider, a cursor session that survives a bramble restart
// gets no idle signal at all and its parent is never told it finished.
func TestPaneIdleTrackerComesFromTheStoredModel(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer m.Close()

	assert.NotNil(t, m.newPaneIdleTrackerForModel("composer-3"),
		"a stored cursor model must still produce a pane-idle tracker")
	assert.Nil(t, m.newPaneIdleTrackerForModel("sonnet"),
		"claude reports its own turn ends; a second signal could only contradict it")
	assert.Nil(t, m.newPaneIdleTrackerForModel("not-a-model"),
		"an unresolvable model is not grounds for guessing at a pane's chrome")
}

// TestTrackerDoesNotCarryObservationsAcrossATurn is the boundary the monitor
// cannot see in the pane. A delivery is written while the recipient is idle and
// marks it running again between two polls, so an idle frame observed before
// the write must not count towards calling the turn that write started idle —
// the CLI has not necessarily repainted yet.
func TestTrackerDoesNotCarryObservationsAcrossATurn(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	tr.forTurn(1)
	require.False(t, tr.observe(cursorPane(false)), "one observation is never enough")

	// A message is delivered; the session is marked running again.
	tr.forTurn(2)
	assert.False(t, tr.observe(cursorPane(false)),
		"the frame before the delivery must not be counted towards the new turn")
	assert.True(t, tr.observe(cursorPane(false)), "two fresh observations agree")
}

// TestTrackerRearmsForEveryTurn keeps a turn too short to be caught working
// from latching the session as never-idle-again: the streak counts past the
// confirmation count, and only a new turn brings it back.
func TestTrackerRearmsForEveryTurn(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	tr.forTurn(1)
	require.False(t, tr.observe(cursorPane(false)))
	require.True(t, tr.observe(cursorPane(false)), "the first turn is seen to end")
	require.False(t, tr.observe(cursorPane(false)), "it fires once per run of observations")

	// A second turn runs and finishes between polls, so no working frame is ever
	// captured — the only signal that it happened is the turn bump.
	tr.forTurn(2)
	assert.False(t, tr.observe(cursorPane(false)))
	assert.True(t, tr.observe(cursorPane(false)), "the second turn was never seen to end")
}
