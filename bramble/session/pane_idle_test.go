package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codexPane renders codex's footer chrome, including its working status line.
func codexPane(working bool, transcript ...string) []string {
	lines := append([]string{}, transcript...)
	if working {
		lines = append(lines, "  ◦ Working (2m 29s • esc to interrupt)")
	}
	return append(lines,
		"  › Ask Codex to do anything",
		"  gpt-5.4 · /tmp/wt",
	)
}

// cursorPane renders cursor-agent's footer chrome.
func cursorPane(working bool, transcript ...string) []string {
	return cursorPaneMode(working, false, transcript...)
}

// cursorPaneMode includes cursor's plan-mode line for codetalk subagents.
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

// TestCursorPaneWorkingIsNotIdle pins the cursor trap: "Add a follow-up" is
// present whether working or idle; only "ctrl+c to stop" marks a live turn.
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

// TestEveryTmuxProviderHasAProbe pins Claude's fallback probe: hooks are
// authoritative when they arrive, but stranded windows need a pane verdict so
// their parents' mail can drain.
// The probe only adds an idle that would otherwise never come; a hook that
// fired already moved the session on.
func TestEveryTmuxProviderHasAProbe(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{ProviderClaude, ProviderCodex, ProviderCursor} {
		assert.True(t, providerHasIdleProbe(provider), "provider %q", provider)
		assert.NotNil(t, newPaneIdleTracker(provider), "provider %q", provider)
	}

	// Codex needs correction after its hook fires early. Claude also needs it:
	// native team messages and monitor events can start a new turn without going
	// through Bramble's pane writer, so the previous turn's idle status is stale.
	assert.True(t, newPaneIdleTracker(ProviderCodex).correctsStaleIdle())
	assert.True(t, newPaneIdleTracker(ProviderClaude).correctsStaleIdle(),
		"claude can start a turn outside Bramble while its recorded status is still idle")
}

// TestTrackerNeedsConsecutiveObservations stops one half-painted frame from
// reporting a working lane as finished.
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

// TestCursorPlanModeWorkingIsNotIdle pins the plan-mode footer: a fixed
// trailing-lines window can miss "ctrl+c to stop", so the marker must be read on
// the composer line itself.
// That keeps footer height from changing the verdict.
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

func TestCodexPaneWorkingIsNotIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCodex, codexPane(true, "  running a subagent review"))
	require.True(t, known, "the composer line is present")
	assert.False(t, idle, "a running turn must never read as idle")
}

func TestCodexPaneIdleIsIdle(t *testing.T) {
	t.Parallel()

	idle, known := paneShowsIdle(ProviderCodex, codexPane(false, "  done"))
	require.True(t, known)
	assert.True(t, idle)
}

// TestCodexFooterWorkingMarkerNotOnComposer: the working hint is on its own
// line above the composer, not on "Ask Codex to do anything".
func TestCodexFooterWorkingMarkerNotOnComposer(t *testing.T) {
	t.Parallel()

	working, known := paneShowsWorking(ProviderCodex, codexPane(true))
	require.True(t, known)
	assert.True(t, working)

	probe := paneIdleProbes[ProviderCodex]
	prompt, ok := findPromptLine(codexPane(true), probe.promptMarkers)
	require.True(t, ok)
	assert.False(t, containsAny(prompt, probe.workingInFooter))
}

// TestCodexTranscriptDoesNotReadAsWorking keeps scrollback that quotes the
// working line from resurrecting an idle session.
func TestCodexTranscriptDoesNotReadAsWorking(t *testing.T) {
	t.Parallel()

	transcript := []string{"  old: Working (8s • esc to interrupt)"}
	for i := 0; i < 12; i++ {
		transcript = append(transcript, "  line of output")
	}
	working, known := paneShowsWorking(ProviderCodex, codexPane(false, transcript...))
	require.True(t, known)
	assert.False(t, working, "a quoted line in scrollback must not read as working")
}

func TestCodexPrematureIdleReturnsToRunning(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCodex)
	pane := codexPane(true, "  still going")

	// Confirmed, like the idle direction: one frame is not a state change.
	require.Equal(t, paneIdleActionNone, decidePaneIdlePoll(tr, StatusIdle, pane),
		"one observation is not enough to resurrect a session")
	assert.Equal(t, paneIdleActionMarkRunning, decidePaneIdlePoll(tr, StatusIdle, pane),
		"two in a row means the turn really is still running")
}

// TestClaudeExternalTurnStartReturnsToRunning covers work that starts without
// Bramble typing into the pane. Claude team messages and monitor events can
// begin a turn natively, so no send-input path is available to mark it running.
// The pane monitor must correct the stale idle status once the working chrome
// has remained stable for Claude's full confirmation window.
func TestClaudeExternalTurnStartReturnsToRunning(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderClaude)
	require.True(t, tr.correctsStaleIdle(),
		"an idle Claude session must still be polled for externally started work")
	pane := claudePane("✽ Smooshing… (1m 31s · thinking)")

	need := tr.confirmationsNeeded()
	for i := 1; i < need; i++ {
		require.Equal(t, paneIdleActionNone, decidePaneIdlePoll(tr, StatusIdle, pane),
			"working observation %d of %d must not flap the state", i, need)
	}
	assert.Equal(t, paneIdleActionMarkRunning, decidePaneIdlePoll(tr, StatusIdle, pane),
		"a sustained working pane must correct the stale idle status")
}

// TestStrayWorkingFrameDoesNotResurrect is why the correction is confirmed. It
// used to fire on a single frame while going idle needed two, and because every
// resurrection re-arms idle reporting, a pane flapping around the marker sent
// the parent one report per flap.
func TestStrayWorkingFrameDoesNotResurrect(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCodex)
	require.Equal(t, paneIdleActionNone,
		decidePaneIdlePoll(tr, StatusIdle, codexPane(true, "  a half-painted frame")))
	// The next frame shows it really was idle, so the streak dies with it.
	require.Equal(t, paneIdleActionNone,
		decidePaneIdlePoll(tr, StatusIdle, codexPane(false, "  all done")))
	assert.Equal(t, paneIdleActionNone,
		decidePaneIdlePoll(tr, StatusIdle, codexPane(true, "  another stray")),
		"the count restarted, so a lone frame still cannot resurrect")
}

func TestCodexGenuinelyIdleStaysIdle(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCodex)
	action := decidePaneIdlePoll(tr, StatusIdle, codexPane(false, "  all done"))
	assert.Equal(t, paneIdleActionNone, action)
}

func TestTerminalSessionNeverResurrectedByProbe(t *testing.T) {
	t.Parallel()

	for _, status := range []SessionStatus{StatusCompleted, StatusFailed, StatusStopped} {
		tr := newPaneIdleTracker(ProviderCodex)
		action := decidePaneIdlePoll(tr, status, codexPane(true))
		assert.Equal(t, paneIdleActionNone, action, "status %s", status)
	}
}

// TestOnlyProvidersWhoseIdleCanBecomeStaleArePolledWhileIdle keeps idle-pane
// polling limited to providers with a state transition to recover. Cursor has
// no completion hook and receives turns through Bramble, so it has no missing
// running edge. Claude can start turns from native team and monitor messages.
func TestOnlyProvidersWhoseIdleCanBecomeStaleArePolledWhileIdle(t *testing.T) {
	t.Parallel()

	assert.True(t, newPaneIdleTracker(ProviderCodex).correctsStaleIdle(),
		"codex's notify hook fires early; its pane must still be read when idle")
	assert.False(t, newPaneIdleTracker(ProviderCursor).correctsStaleIdle(),
		"cursor has no hook to correct; polling it while idle is pure cost")
	assert.True(t, newPaneIdleTracker(ProviderClaude).correctsStaleIdle(),
		"claude can start native work without Bramble observing the running edge")
}

func TestPaneIdlePollingEligibility(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		provider string
		status   SessionStatus
		want     bool
	}{
		{"running claude", ProviderClaude, StatusRunning, true},
		{"idle claude with an externally started turn", ProviderClaude, StatusIdle, true},
		{"idle codex after an early hook", ProviderCodex, StatusIdle, true},
		{"idle cursor has no missed running edge", ProviderCursor, StatusIdle, false},
		{"terminal claude", ProviderClaude, StatusCompleted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, shouldPollPaneIdle(newPaneIdleTracker(tc.provider), tc.status))
		})
	}
}

// TestPaneIdleTrackerComesFromTheStoredModel pins the re-adopt input:
// monitorTrackedTmuxWindow has only the stored model string, so that string must
// still produce the pane-idle tracker for stranded sessions.
func TestPaneIdleTrackerComesFromTheStoredModel(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer m.Close()

	assert.NotNil(t, m.newPaneIdleTrackerForModel("composer-3", ""),
		"a stored cursor model must still produce a pane-idle tracker")
	assert.NotNil(t, m.newPaneIdleTrackerForModel("sonnet", ""),
		"claude needs the fallback too: a window whose hook cannot reach bramble has no other way to be seen idle")
	assert.Nil(t, m.newPaneIdleTrackerForModel("not-a-model", ""),
		"an unresolvable model is not grounds for guessing at a pane's chrome")
}

// TestPaneIdleTrackerUsesTheSessionBackend pins third-party model IDs: when the
// model registry cannot resolve a provider, the explicit backend must still
// create the tracker for hookless sessions.
func TestPaneIdleTrackerUsesTheSessionBackend(t *testing.T) {
	t.Parallel()

	m := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer m.Close()

	const thirdPartyModel = "stealth/ox-alpha"

	assert.Nil(t, m.newPaneIdleTrackerForModel(thirdPartyModel, ""),
		"precondition: the model alone does not resolve, which is why the backend has to travel with it")
	assert.NotNil(t, m.newPaneIdleTrackerForModel(thirdPartyModel, ProviderCursor),
		"an explicit backend names the provider the model cannot")
	assert.NotNil(t, m.newPaneIdleTrackerForModel(thirdPartyModel, ProviderClaude),
		"an explicit backend names the provider for claude too")
}

// TestTrackerDoesNotCarryObservationsAcrossATurn pins the boundary the pane
// cannot show: an idle frame before delivery must not count toward the turn that
// delivery just started.
func TestTrackerDoesNotCarryObservationsAcrossATurn(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	tr.forTurn(1)
	require.False(t, tr.observe(cursorPane(false)), "one observation is never enough")

	tr.forTurn(2)
	assert.False(t, tr.observe(cursorPane(false)),
		"the frame before the delivery must not be counted towards the new turn")
	assert.True(t, tr.observe(cursorPane(false)), "two fresh observations agree")
}

// TestTrackerRearmsForEveryTurn keeps short turns from leaving the tracker
// permanently past its firing count.
func TestTrackerRearmsForEveryTurn(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderCursor)
	tr.forTurn(1)
	require.False(t, tr.observe(cursorPane(false)))
	require.True(t, tr.observe(cursorPane(false)), "the first turn is seen to end")
	require.False(t, tr.observe(cursorPane(false)), "it fires once per run of observations")

	// The turn bump is the only signal for a turn that starts and ends between polls.
	tr.forTurn(2)
	assert.False(t, tr.observe(cursorPane(false)))
	assert.True(t, tr.observe(cursorPane(false)), "the second turn was never seen to end")
}

// claudePane renders the live claude-code shape, measured on 2026-08-25:
//
//	<transcript>
//	<state>            <- the line that decides idle vs working
//	────────────────   <- content boundary
//	❯ <composer>
//	────────────────   <- status separator
//	<info line>
//	<permissions line>
//
// Both rules are plain runs of ─; requiring mode markers made every measured
// real pane unreadable. The composer is always drawn, so it cannot decide idle.
// state is the nearest content line; tests that want "idle" pass a completion
// line because production panes with prior turns have transcript content.
// This helper intentionally omits the old synthetic mode marker.
func claudePane(state string, transcript ...string) []string {
	return claudePaneComposer("❯ ", state, transcript...)
}

// claudePaneComposer is claudePane with the composer line spelled out, for the
// cases that turn on what the composer itself holds.
func claudePaneComposer(composer, state string, transcript ...string) []string {
	lines := append([]string{}, transcript...)
	if state != "" {
		lines = append(lines, state)
	}
	return append(lines,
		"────────────────────────────────────────────",
		composer,
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	)
}

// TestClaudeJudgeSeesAWrappedComposer pins capture depth to the composer walk:
// wrapped deliveries push the upper rule away, and too shallow a capture makes
// the judge return known=false forever.
func TestClaudeJudgeSeesAWrappedComposer(t *testing.T) {
	t.Parallel()

	const composer = "\u276f hello"
	for _, rows := range []int{1, 8, 16} {
		lines := claudePaneComposer(composer, "\u273b Baking\u2026 (1m 55s)")
		var wrapped []string
		for _, l := range lines {
			wrapped = append(wrapped, l)
			if l == composer {
				for i := 1; i < rows; i++ {
					wrapped = append(wrapped, "  wrapped continuation text")
				}
			}
		}
		if len(wrapped) > paneIdleCaptureLines {
			wrapped = wrapped[len(wrapped)-paneIdleCaptureLines:]
		}

		working, known := claudePaneJudge(wrapped)
		require.True(t, known, "composer wrapped onto %d rows must stay legible within a %d-line capture", rows, paneIdleCaptureLines)
		require.True(t, working, "spinner above a %d-row composer must still read as working", rows)
	}
}

// TestClaudePaneJudge covers each visible shape. Markerless frames must stay
// unknown because live monitoring usually misses claude's sub-second spinner.
// Reading unknown as idle would report a running turn as finished.
func TestClaudePaneJudge(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		lines   []string
		working bool
		known   bool
	}{
		// The composer is present in idle and working cases, so it says nothing.
		{"turn just finished", claudePane("✻ Worked for 36m 36s"), false, true},
		{"completion with a non-ASCII verb", claudePane("✻ Sautéed for 6m 16s"), false, true},
		{"completion with a trailing clause", claudePane("✻ Baked for 3m 48s · 1 shell still running"), false, true},
		{"a draft in the composer is not a turn in flight", claudePaneComposer("❯ half a thought", "✻ Worked for 12s"), false, true},
		{"a completion pushed up by a recap is still found", claudePane("recap tail (disable recaps in /config)", "✻ Cooked for 2m 44s", "※ recap: you asked me to…"), false, true},

		// Positive working markers.
		{"spinner", claudePane("* Frosting… (2m 30s)"), true, true},
		{"braille spinner", claudePane("⠋ Thinking…"), true, true},
		{"tool line", claudePane("● Bash(git status)"), true, true},

		// Ambiguous: agent output, no marker either way. Must be unknown.
		{"agent prose mid-turn", claudePane("Let me check the delivery path."), false, false},
		{"wrapped output", claudePane("  ...and that is why it failed."), false, false},

		// Not claude's pane at all.
		{"no separator", []string{"$ ", "some shell"}, false, false},
		{"empty pane", []string{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			working, known := paneShowsWorking(ProviderClaude, tc.lines)
			assert.Equal(t, tc.known, known, "known")
			if tc.known {
				assert.Equal(t, tc.working, working, "working")
			}
		})
	}
}

// TestClaudeAmbiguousFrameResetsTheStreak keeps markerless working frames from
// accumulating toward idle.
func TestClaudeAmbiguousFrameResetsTheStreak(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderClaude)
	need := tr.confirmationsNeeded()
	require.Greater(t, need, paneIdleConfirmations,
		"claude needs more agreement than a provider whose working chrome is always on screen")

	for i := 0; i < need-1; i++ {
		require.False(t, tr.observe(claudePane("✻ Worked for 36m 36s")), "observation %d", i+1)
	}
	require.False(t, tr.observe(claudePane("still working on it")))

	assert.False(t, tr.observe(claudePane("✻ Worked for 36m 36s")), "the streak restarted")
	for i := 0; i < need-2; i++ {
		assert.False(t, tr.observe(claudePane("✻ Worked for 36m 36s")))
	}
	assert.True(t, tr.observe(claudePane("✻ Worked for 36m 36s")), "a full fresh streak fires")
}

// TestClaudeNeedsAFullStreakToGoIdle: a single idle-looking frame is not
// enough, which is what keeps a half-painted repaint from releasing mail.
func TestClaudeNeedsAFullStreakToGoIdle(t *testing.T) {
	t.Parallel()

	tr := newPaneIdleTracker(ProviderClaude)
	need := tr.confirmationsNeeded()

	for i := 0; i < need-1; i++ {
		assert.False(t, tr.observe(claudePane("✻ Worked for 36m 36s")), "observation %d of %d", i+1, need)
	}
	assert.True(t, tr.observe(claudePane("✻ Worked for 36m 36s")), "the %dth consecutive idle frame fires", need)
}

// TestClaudeWorkingFrameIsNeverIdle: the direct statement of the bug this probe
// must not introduce.
func TestClaudeWorkingFrameIsNeverIdle(t *testing.T) {
	t.Parallel()

	for _, working := range []string{"* Frosting… (2m 30s)", "● Bash(git status)", "⠹ Thinking…"} {
		idle, known := paneShowsIdle(ProviderClaude, claudePane(working))
		require.True(t, known, "%q", working)
		assert.False(t, idle, "a running turn must never read as idle: %q", working)
	}
}

// TestWorkingClaudePaneIsNeverReadAsIdle pins the repo fixture where
// ParseClaudeStatusBar reads a working pane as idle because the composer is
// always on screen. The pane-idle judge must not repeat that failure.
// Repeating it would report the turn finished while it is still running.
func TestWorkingClaudePaneIsNeverReadAsIdle(t *testing.T) {
	t.Parallel()

	// Verbatim from tmux_test.go's "working with completion indicator" case.
	live := []string{
		"● Bash(pytest tests/)",
		"  ⎿  Running…",
		"✢ Fluttering… (4m 16s)",
		"",
		"───────── ▪▪▪ ─",
		"❯ ",
		"───────────────────────────────────────",
		"  ~/project  main  Opus 4.6  ctx:19%  tokens:67k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
		"",
	}

	require.True(t, ParseClaudeStatusBar(live).IsIdle,
		"precondition: the status-bar parser still reads this working pane as idle")

	working, known := claudePaneJudge(live)
	assert.True(t, known, "a pane with a tool line in flight is readable")
	assert.True(t, working, "a working claude pane must never be judged idle")

	// The tracker must never fire idle on it, however many frames agree.
	tr := &paneIdleTracker{provider: ProviderClaude}
	for i := 0; i < 3*tr.confirmationsNeeded(); i++ {
		require.False(t, tr.observe(live), "frame %d released mail into a live turn", i+1)
	}
}

// TestStaleTranscriptPromptIsNotADraft pins composer anchoring: submitted
// prompts use the same glyph, so a bottom-up scan for the lowest glyph can latch
// onto transcript history and hold every delivery forever.
func TestStaleTranscriptPromptIsNotADraft(t *testing.T) {
	t.Parallel()

	// A dialog makes transcript history the lowest glyph in the capture.
	pane := append(claudePaneComposer("❯ ", ""),
		"❯ an earlier prompt the user already sent",
		"  [press esc to dismiss]",
	)

	draft, known := composerDraft(ProviderClaude, pane)
	require.True(t, known, "the composer is readable")
	assert.False(t, draft,
		"an empty composer is not a draft just because a redrawn transcript prompt sits below it")
}

// TestPaneIdleAndWorkingStreaksAreIndependent keeps a pane-observed idle streak
// from blocking codex's later premature-idle correction.
func TestPaneIdleAndWorkingStreaksAreIndependent(t *testing.T) {
	t.Parallel()

	tr := &paneIdleTracker{provider: ProviderCodex}
	need := tr.confirmationsNeeded()

	for i := 1; i < need; i++ {
		require.False(t, tr.observe(codexPane(false)), "idle frame %d", i)
	}
	require.True(t, tr.observe(codexPane(false)), "the idle streak fires")

	// Working frames must still be able to resurrect after a pane-driven idle.
	for i := 1; i < need; i++ {
		require.False(t, tr.observeWorking(codexPane(true)), "working frame %d", i)
	}
	assert.True(t, tr.observeWorking(codexPane(true)),
		"a premature idle must still be correctable after a pane-driven idle")
}

// TestIdleClaudeSessionIsActuallyReachable pins the fallback for stranded
// Claude windows. An idle verdict must be reachable on real panes, where the
// topmost content line is often the tail of the last answer rather than the
// completion marker.
// Demanding the marker on that top line reset the streak forever on real panes.
//
// The layouts below are from live panes captured 2026-08-25.
func TestIdleClaudeSessionIsActuallyReachable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"answer tail above the completion line", claudePane(
			"✻ Cogitated for 1m 27s",
			"but if you ever see a dangling ~/.agents/skills on a new box, that means the",
			"Committed as 5aeae0f. I did not push — you'd asked to push last turn for that",
		)},
		{"completion pushed up by a recap block", claudePane(
			"estimate's weakest input. (disable recaps in /config)",
			"✻ Cooked for 2m 44s",
			"※ recap: You asked me to cost the per-tenant-org pattern on code.storage",
		)},
		{"non-ASCII verb", claudePane(
			"✻ Sautéed for 6m 10s",
			"services/python/{api-gateway-service,tenant-user-agent}",
			"apps).",
		)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			working, known := claudePaneJudge(tc.lines)
			require.True(t, known, "a real idle claude pane must produce a verdict")
			assert.False(t, working, "a finished turn is not work in flight")

			// The tracker must actually reach the idle decision.
			tr := &paneIdleTracker{provider: ProviderClaude}
			need := tr.confirmationsNeeded()
			for i := 1; i < need; i++ {
				require.False(t, tr.observe(tc.lines), "frame %d", i)
			}
			assert.True(t, tr.observe(tc.lines),
				"%d agreeing frames must mark a stranded session idle", need)
		})
	}
}

// TestWrappedComposerStillReadsAsADraft pins wrapped composer detection. Reading
// only the nearest line above the status separator sees a continuation without
// the glyph and reports unknown, which means deliver into the draft.
// Live window 6 of the 2026-08-25 survey had this shape.
func TestWrappedComposerStillReadsAsADraft(t *testing.T) {
	t.Parallel()

	// Human text, not a bramble-staged delivery, so ownership is not the issue.
	pane := []string{
		"● Bash(git status)",
		"────────────────────────────────────────────",
		"❯ file the dev deprovisioning bug and then check whether the staging",
		"tenant still has the old role binding attached to it",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}

	draft, known := composerDraft(ProviderClaude, pane)
	require.True(t, known, "a wrapped composer is still a readable composer")
	assert.True(t, draft, "a wrapped draft must hold the delivery, not invite one")
}

// TestOversizedComposerIsHeldNotDelivered pins the bounded tail fallback. On the
// alternate screen, a composer taller than the window may have no upper rule in
// the capture, and unknown must fail closed as hold.
// It also keeps transcript prompts with the same glyph from wedging the queue as
// drafts that never clear.
func TestOversizedComposerIsHeldNotDelivered(t *testing.T) {
	t.Parallel()

	t.Run("a draft wrapping past the top of the capture is held", func(t *testing.T) {
		t.Parallel()
		// The upper rule has scrolled off the visible capture.
		pane := []string{
			"❯ the beginning of a very long draft that fills the window",
			"and the rest of my long draft continues here",
			"more of the draft, still no rule above it",
			"────────────────────────────────────────────",
			"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
			"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
		}
		draft, known := composerDraft(ProviderClaude, pane)
		require.True(t, known, "an unreadable composer must not report unknown — that means deliver")
		assert.True(t, draft, "an oversized draft must hold the delivery")
	})

	t.Run("a stale prompt with no rule above it does not wedge the queue", func(t *testing.T) {
		t.Parallel()
		pane := []string{
			"❯ an earlier submitted prompt",
			"● some tool output",
			"❯ ",
			"────────────────────────────────────────────",
			"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
			"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
		}
		draft, known := composerDraft(ProviderClaude, pane)
		require.True(t, known)
		assert.False(t, draft, "the live composer is empty; a transcript prompt must not hold mail forever")
	})
}

// TestComposerLayerDoesNotJudgeOwnership: whether staged text belongs to
// bramble is not decidable from the pane. Text that looks like bramble's own
// output is user-controllable, so this layer reports any non-empty composer as
// a draft — and the notifier yields on that, which is the safe direction.
func TestComposerLayerDoesNotJudgeOwnership(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		"[bramble] subagent forge-planner-0da3be25 (planner, opus) is idle",
		"file the dev deprovisioning bug",
	} {
		draft, known := composerDraft(ProviderClaude, claudePaneComposer("❯ "+body, "✻ Worked for 12s"))
		require.True(t, known)
		assert.True(t, draft, "any non-empty composer is a draft at this layer: %q", body)
	}
}

// TestLocatedButUnreadableComposerHolds pins fail-closed behavior for a located
// composer whose text cannot be parsed. Unknown means deliver, so unreadable
// content must report hold.
// With no upper rule, the composer is unfound instead and the bounded tail
// fallback owns the decision.
func TestLocatedButUnreadableComposerHolds(t *testing.T) {
	t.Parallel()

	pane := []string{
		"✻ Worked for 12s",
		"────────────────────────────────────────────",
		"⏎ some decorated composer shape we do not parse",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}

	// Precondition: located branch, not the tail fallback.
	composerIdx, contentEnd := claudeComposerIdx(pane)
	require.GreaterOrEqual(t, composerIdx, 0, "the composer region must be located")
	require.GreaterOrEqual(t, contentEnd, 0, "bounded above by a rule")

	draft, known := composerDraft(ProviderClaude, pane)
	require.True(t, known, "an unreadable composer must not report unknown — that means deliver")
	assert.True(t, draft, "hold when something unparseable occupies the composer")
}

// TestStaleCompletionLineIsNotThisTurnsVerdict pins the submitted-prompt
// boundary: a previous completion can sit just above this turn's echoed prompt,
// but nothing above that prompt speaks for the turn now running.
//
// Reading the old completion as current would mark a live turn idle and let a
// hint be typed into it.
// forTurn resets the streak at the submitted-prompt boundary, where the spinner
// is often absent from individual frames.
func TestStaleCompletionLineIsNotThisTurnsVerdict(t *testing.T) {
	t.Parallel()

	justSubmitted := []string{
		"  Worktree is ready for your next task.",
		"✻ Worked for 36m 36s",                 // the PREVIOUS turn's completion
		"❯ [bramble] subagent child-1 is idle", // THIS turn's prompt
		"────────────────────────────────────────────",
		"❯ ",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}

	working, known := claudePaneJudge(justSubmitted)
	assert.False(t, known,
		"a turn that has produced no output yet has no verdict; the line above its prompt is a previous turn's")
	assert.False(t, working)

	// The tracker must never reach an idle decision on it.
	tr := &paneIdleTracker{provider: ProviderClaude}
	for i := 0; i < 3*tr.confirmationsNeeded(); i++ {
		require.False(t, tr.observe(justSubmitted),
			"frame %d reported a live turn as finished", i+1)
	}

	// Once the turn ends, its own completion sits below the prompt.
	finished := []string{
		"❯ [bramble] subagent child-1 is idle",
		"● Read(delivery.go)",
		"✻ Worked for 12s",
		"────────────────────────────────────────────",
		"❯ ",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}
	working, known = claudePaneJudge(finished)
	require.True(t, known, "a completed turn below its own prompt is readable")
	assert.False(t, working, "and reads as idle")
}

// TestTallComposerIsHeldNotDelivered pins the draft half of an over-tall
// composer: ordinary long drafts must hold, not deliver.
func TestTallComposerIsHeldNotDelivered(t *testing.T) {
	t.Parallel()

	pane := []string{"────────────────────────────────────────────"}
	for i := 0; i < 12; i++ {
		pane = append(pane, "and the draft continues onto another line")
	}
	pane = append([]string{pane[0]}, pane[1:]...)
	pane[1] = "❯ the beginning of a draft that wraps well past six lines"
	pane = append(pane,
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	)

	draft, known := composerDraft(ProviderClaude, pane)
	require.True(t, known, "unknown means deliver, straight into the draft")
	assert.True(t, draft, "a composer taller than six lines is still a draft")
}

// TestComposerBoundFollowsTheCapture: the walk's bound must be sized against
// the capture it runs over, not against a guess at composer height. At 6 it
// manufactured the unfound case for ordinary panes.
func TestComposerBoundFollowsTheCapture(t *testing.T) {
	t.Parallel()
	assert.Greater(t, claudeComposerMaxLines, paneIdleTailLines,
		"the walk must reach further than the tail scan it replaced")
	assert.Less(t, claudeComposerMaxLines, paneCaptureLines,
		"but never past the capture, or it walks into transcript")
}
