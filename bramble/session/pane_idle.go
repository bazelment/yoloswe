package session

import "strings"

// paneIdleProbe tells, from a tmux pane's text, whether an agent CLI is waiting
// for input.
//
// It exists for backends with no turn-completion hook. Claude gets a Stop hook
// through --settings and codex a notify program through -c notify=[...], so
// both tell bramble when a turn ends. Cursor has neither: no --notify flag, and
// its plugin `stop` hook does not fire from the CLI in either interactive or
// --print mode (checked against cursor-agent 2026.08.11). Without some signal,
// bramble only ever learns such a session finished when its window dies — so
// nothing drains its queued mail and its parent is never told it is done.
//
// Reading the pane is a poor second to a hook and is treated as such: the probe
// must positively recognize the CLI's chrome before it will judge anything, and
// it reports "unknown" rather than guessing.
type paneIdleProbe struct {
	// promptMarkers identify the composer line itself. Until one appears the
	// pane says nothing useful — the CLI may still be booting.
	promptMarkers []string
	// workingMarkers mean a turn is in flight. They are looked for *on the
	// composer line only*, not anywhere in the footer: the footer grows and
	// shrinks (cursor adds a mode line in plan mode), so a fixed window of
	// trailing lines would sometimes miss the hint and read a working session
	// as idle — releasing queued mail into a live turn.
	workingMarkers []string
}

// paneIdleProbes holds a probe per provider that lacks a completion hook.
//
// Deliberately not a fallback for every provider: a wrong "idle" is worse than
// no signal, because it releases queued messages into a live turn. A provider
// is listed only once its chrome has been checked against the real CLI.
var paneIdleProbes = map[string]paneIdleProbe{
	// cursor-agent's composer footer carries "ctrl+c to stop" for exactly as
	// long as a turn is running. "Add a follow-up" is NOT an idle marker — it
	// is shown while working too, which is the trap this table exists to
	// record.
	ProviderCursor: {
		promptMarkers:  []string{"Add a follow-up"},
		workingMarkers: []string{"ctrl+c to stop"},
	},
}

// paneIdleConfirmations is how many consecutive polls must agree before a
// session is called idle. Two, so a single half-painted frame cannot release
// queued mail into a turn that is still running.
const paneIdleConfirmations = 2

// paneIdleTailLines bounds how far up from the bottom the composer line is
// looked for. Generous enough for a footer that grows a mode line or two, tight
// enough that the transcript above — which can quote these very markers back —
// is out of reach.
const paneIdleTailLines = 6

// providerHasIdleProbe reports whether a provider's idleness can be read off
// its pane.
func providerHasIdleProbe(provider string) bool {
	_, ok := paneIdleProbes[provider]
	return ok
}

// paneShowsIdle judges a captured pane. known is false when the pane does not
// yet look like the CLI's prompt, in which case idle is meaningless.
func paneShowsIdle(provider string, lines []string) (idle, known bool) {
	probe, ok := paneIdleProbes[provider]
	if !ok {
		return false, false
	}

	prompt, ok := findPromptLine(lines, probe.promptMarkers)
	if !ok {
		return false, false
	}
	return !containsAny(prompt, probe.workingMarkers), true
}

// findPromptLine returns the composer line, searched upwards from the bottom of
// the pane so the most recent one wins.
func findPromptLine(lines, markers []string) (string, bool) {
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < paneIdleTailLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		if containsAny(lines[i], markers) {
			return lines[i], true
		}
	}
	return "", false
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// paneIdleTracker turns a stream of pane judgements into one idle transition.
type paneIdleTracker struct {
	provider string
	streak   int
	// epoch is the session turn the current streak was observed during, so
	// observations cannot be carried across a turn boundary. See forTurn.
	epoch uint64
}

// newPaneIdleTracker returns a tracker for a provider, or nil when that
// provider reports its own idleness through a hook.
func newPaneIdleTracker(provider string) *paneIdleTracker {
	if !providerHasIdleProbe(provider) {
		return nil
	}
	return &paneIdleTracker{provider: provider}
}

// forTurn re-arms the tracker when the session has been started on a new turn.
//
// The monitor cannot see that boundary in the pane. A delivery is written while
// the recipient is idle and marks it running again between two polls, so
// without this the poll after the write extends the streak the poll before it
// began — a frame the CLI has not repainted yet would then be counted towards
// calling the new turn idle. It is also what re-arms a tracker whose streak has
// already fired: a turn short enough that no poll catches its working chrome
// would otherwise leave the streak counting past the confirmation count
// forever, and the session would never be seen to go idle again.
func (p *paneIdleTracker) forTurn(epoch uint64) {
	if p == nil || p.epoch == epoch {
		return
	}
	p.epoch = epoch
	p.streak = 0
}

// observe feeds one capture in and reports whether the session should now be
// marked idle. It fires once per run of idle observations: the caller marks the
// session idle, and the next turn re-arms it through forTurn.
func (p *paneIdleTracker) observe(lines []string) bool {
	if p == nil {
		return false
	}
	idle, known := paneShowsIdle(p.provider, lines)
	if !known || !idle {
		p.streak = 0
		return false
	}
	p.streak++
	return p.streak == paneIdleConfirmations
}

// reset forgets the current streak, so a session that went idle and was then
// given more work must be observed idle afresh.
func (p *paneIdleTracker) reset() {
	if p != nil {
		p.streak = 0
	}
}

// paneIdleCaptureLines is how much scrollback the monitor pulls per poll. Small
// on purpose: only the footer is read, and this runs every couple of seconds
// for every hookless session.
const paneIdleCaptureLines = 12
