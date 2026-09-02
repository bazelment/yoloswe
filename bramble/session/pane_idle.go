package session

import (
	"regexp"
	"strings"
)

// paneIdleProbe tells, from tmux pane text, whether an agent CLI is waiting for
// input. It covers hookless providers and providers whose hook can report idle
// before the pane is actually ready.
//
// Reading the pane is treated as a weak signal: the probe must positively
// recognize provider chrome before judging, and otherwise reports unknown.
type paneIdleProbe struct {
	// judge replaces substring matching for CLIs whose chrome needs structure.
	judge func(lines []string) (working, known bool)
	// promptMarkers identify the composer line itself. Until one appears the
	// pane says nothing useful — the CLI may still be booting.
	promptMarkers []string
	// workingOnPrompt means a turn is in flight when found on the composer line.
	// This catches Cursor's "ctrl+c to stop" without relying on a fixed footer.
	workingOnPrompt []string
	// workingInFooter means a turn is in flight within paneIdleTailLines of the
	// bottom; the tail bound keeps quoted scrollback markers out of reach.
	workingInFooter []string
	// confirmations overrides paneIdleConfirmations for noisy chrome.
	confirmations int
	// correctsStaleIdle keeps polling an idle session whose pane can become
	// working without Bramble observing an idle-to-running edge. This covers a
	// premature completion hook and work started by the provider itself.
	correctsStaleIdle bool
}

// paneIdleProbes lists only providers whose chrome is understood. A wrong idle
// verdict is written into the session status a polling orchestrator reads, so
// unknown providers are not guessed from the pane.
var paneIdleProbes = map[string]paneIdleProbe{
	// "Add a follow-up" is not idle; Cursor shows it while working too.
	ProviderCursor: {
		promptMarkers:   []string{"Add a follow-up"},
		workingOnPrompt: []string{"ctrl+c to stop"},
	},
	// Codex reports turn ends through a notify hook, but that hook can fire
	// before the pane shows idle — while "Working (… • esc to interrupt)" is
	// still on screen. The probe corrects that premature idle back to running.
	ProviderCodex: {
		promptMarkers:     []string{"Ask Codex to do anything"},
		workingInFooter:   []string{"esc to interrupt"},
		correctsStaleIdle: true,
	},
	// Claude normally reports idle through its Stop hook. Its pane probe is both
	// the fallback when that hook never arrives and the running-edge detector for
	// native team messages and monitor events. Those start turns without going
	// through Bramble's pane writer, so its recorded status can remain idle while
	// Claude is working. Its chrome needs claudePaneJudge because "no spinner" is
	// not enough to call the pane idle.
	ProviderClaude: {
		judge:             claudePaneJudge,
		correctsStaleIdle: true,
		// Five, not two. Claude's working chrome is often absent from any given
		// frame, so agreement has to be sustained before it means anything.
		confirmations: 5,
	},
}

// claudeCompletionPastRe matches claude's finished-turn line, e.g.
// "✻ Baked for 3m 48s". Match the verb as non-space rather than \w because
// Go's \w is ASCII-only and claude's verbs need not be.
var claudeCompletionPastRe = regexp.MustCompile(`^[✻✢✽✹]\s+\S+ for\s+\d`)

// isClaudeSeparator matches either claude rule, including the input rule whose
// box drawing is split by the mode indicator.
func isClaudeSeparator(trimmed string) bool {
	return separatorRe.MatchString(trimmed) ||
		(strings.HasPrefix(trimmed, "─") && strings.Contains(trimmed, "▪"))
}

// claudeComposerIdx locates claude-code's live composer by position: the first
// non-empty line above the lowest status separator, with the next separator
// above it bounding content when present.
//
// Do not scan by glyph alone. Claude echoes submitted prompts with the same `❯`,
// so position relative to the status separator is the only anchor that separates
// the live composer from transcript scrollback.
//
// contentEndIdx is -1 when the upper rule is missing, often because a multi-line
// composer absorbed it. Callers must treat that as "content region unknown",
// not "no content".
func statusSepIdx(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if isClaudeSeparator(strings.TrimSpace(lines[i])) {
			return i
		}
	}
	return -1
}

func claudeComposerIdx(lines []string) (composerIdx, contentEndIdx int) {
	sepIdx := statusSepIdx(lines)
	if sepIdx < 0 {
		return -1, -1
	}
	// Walk up from the status separator to the upper rule. The composer may wrap,
	// so its line is the first line in that block, not the nearest one.
	composerIdx = -1
	seen := 0
	sawUpperRule := false
	for i := sepIdx - 1; i >= 0 && seen < claudeComposerMaxLines; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if isClaudeSeparator(trimmed) {
			if composerIdx < 0 {
				// Two rules with nothing between them: no composer drawn.
				return -1, i
			}
			sawUpperRule = true
			break
		}
		seen++
		composerIdx = i
	}
	if composerIdx < 0 {
		return -1, -1
	}
	if !sawUpperRule {
		// Without the upper rule, the topmost reached line is not a proven
		// composer. Fail closed as unfound; trusting it can either deliver into an
		// oversized draft or latch onto a transcript prompt that never clears.
		return -1, -1
	}
	contentEndIdx = -1
	for i := composerIdx - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if isClaudeSeparator(trimmed) {
			contentEndIdx = i
		}
		break
	}
	return composerIdx, contentEndIdx
}

// claudePaneJudge reads claude-code's pane to decide whether a turn is in flight.
// It does not use ParseClaudeStatusBar's IsIdle: the live composer is visible in
// both states, so that parser can call a working pane idle, which is written
// into the status a polling orchestrator reads.
//
// Claude's reliable signal is the nearest sparkle line in the bounded content
// tail: gerund/ellipsis means working, past tense plus "for <duration>" means
// done. A `●` tool line counts as work only when no completion sparkle sits
// below it.
//
// Ambiguity reports known=false. "No marker" must never mean idle; the spinner
// can be absent from a single frame, and observe resets the streak on unknowns.
func claudePaneJudge(lines []string) (working, known bool) {
	composerIdx, contentEndIdx := claudeComposerIdx(lines)
	if composerIdx < 0 {
		return false, false // no composer: not claude's prompt, or still booting
	}
	if contentEndIdx < 0 {
		// No rule above the composer: a multi-line composer absorbed it, so the
		// content region cannot be delimited and a transcript line would be
		// indistinguishable from live chrome. Refuse to guess.
		return false, false
	}

	// Stop at this turn's submitted prompt. Completion lines above it belong to
	// previous turns and would otherwise make a newly started live turn look idle.
	seen := 0
	for i := contentEndIdx - 1; i >= 0 && seen < claudePaneContentTailLines; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, claudePromptGlyph) {
			// This turn's own submitted prompt. Nothing above it can speak for
			// the turn now running, and this turn has produced no verdict yet.
			return false, false
		}
		seen++
		if working, known := claudeLineVerdict(line); known {
			return working, true
		}
	}

	// Nothing decisive in the tail. Never guess idle here.
	return false, false
}

// paneCaptureLines is how deep every pane read in this file goes. The walks
// below are sized against it, so it is the one number to change if a CLI's
// chrome grows taller.
const paneCaptureLines = 40

// claudeComposerMaxLines bounds the composer walk so a missing upper rule cannot
// turn arbitrary transcript into a composer. It is sized against paneCaptureLines
// rather than a typical composer: long drafts can be ordinary, and too small a
// bound manufactures the fail-closed path in composerDraft.
const claudeComposerMaxLines = paneCaptureLines - 6

// claudePaneContentTailLines finds nearby sparkle lines while staying below
// quoted transcript history.
const claudePaneContentTailLines = 4

// claudeLineVerdict reports what one content line says about the turn, and
// whether it says anything at all.
func claudeLineVerdict(line string) (working, known bool) {
	if spinnerRe.MatchString(line) {
		return true, true
	}
	if completionRe.MatchString(line) {
		// Same glyph class, opposite meanings — the tense decides.
		if claudeCompletionPastRe.MatchString(line) {
			return false, true // "✻ Baked for 3m 48s": the turn is over
		}
		if strings.Contains(line, "…") {
			return true, true // "✽ Baking… (1m 55s)": still running
		}
		return false, false // neither shape: say nothing
	}
	if strings.HasPrefix(line, "●") {
		// A tool line means work only when nothing below it has already
		// reported the turn finished; the caller reaches this first only when
		// that is the case.
		return true, true
	}
	return false, false
}

// paneIdleConfirmations is how many consecutive polls must agree before a
// session is called idle. Two, so a single half-painted frame cannot report a
// turn that is still running as finished.
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

// paneShowsWorking judges whether a captured pane shows a turn in flight.
// known is false when the pane does not yet look like the CLI's prompt.
func paneShowsWorking(provider string, lines []string) (working, known bool) {
	probe, ok := paneIdleProbes[provider]
	if !ok {
		return false, false
	}
	if probe.judge != nil {
		return probe.judge(lines)
	}

	prompt, ok := findPromptLine(lines, probe.promptMarkers)
	if !ok {
		return false, false
	}
	if len(probe.workingOnPrompt) > 0 && containsAny(prompt, probe.workingOnPrompt) {
		return true, true
	}
	if len(probe.workingInFooter) > 0 && footerShowsWorking(lines, probe.promptMarkers, probe.workingInFooter) {
		return true, true
	}
	return false, true
}

// paneShowsIdle judges a captured pane. known is false when the pane does not
// yet look like the CLI's prompt, in which case idle is meaningless.
func paneShowsIdle(provider string, lines []string) (idle, known bool) {
	working, known := paneShowsWorking(provider, lines)
	if !known {
		return false, false
	}
	return !working, true
}

// footerShowsWorking reports whether a working marker appears on a non-composer
// line within the tail window.
func footerShowsWorking(lines []string, promptMarkers, workingMarkers []string) bool {
	working := false
	forEachPaneTailLine(lines, func(line string) bool {
		if containsAny(line, promptMarkers) {
			return false
		}
		if containsAny(line, workingMarkers) {
			working = true
			return true
		}
		return false
	})
	return working
}

func forEachPaneTailLine(lines []string, visit func(string) bool) {
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < paneIdleTailLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		if visit(lines[i]) {
			return
		}
	}
}

// findPromptLine returns the composer line, searched upwards from the bottom of
// the pane so the most recent one wins.
func findPromptLine(lines, markers []string) (string, bool) {
	var prompt string
	var found bool
	forEachPaneTailLine(lines, func(line string) bool {
		if containsAny(line, markers) {
			prompt = line
			found = true
			return true
		}
		return false
	})
	// A found flag rather than prompt != "": a matched line is by definition
	// non-empty today, but making emptiness stand in for "no match" is the kind
	// of sentinel that silently becomes wrong when a marker changes.
	return prompt, found
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
	// Idle and working streaks are separate. observe fires only on equality,
	// so a shared counter that just fired idle would increment past the
	// working threshold and never correct a premature idle.
	idleStreak    int
	workingStreak int
	// epoch is the session turn the current streaks were observed during, so
	// observations cannot be carried across a turn boundary. See forTurn.
	epoch uint64
}

// correctsStaleIdle reports whether this provider's pane is worth reading while
// the session is already idle.
func (p *paneIdleTracker) correctsStaleIdle() bool {
	if p == nil {
		return false
	}
	return paneIdleProbes[p.provider].correctsStaleIdle
}

// shouldPollPaneIdle reports whether a monitor should capture a pane for this
// session state. Idle panes are normally skipped, except for providers whose
// status can become stale without Bramble observing a new turn.
func shouldPollPaneIdle(tracker *paneIdleTracker, status SessionStatus) bool {
	return tracker != nil && (status == StatusRunning ||
		(status == StatusIdle && tracker.correctsStaleIdle()))
}

// newPaneIdleTracker returns a tracker for a provider that needs pane evidence,
// or nil when its hook is sufficient on its own.
func newPaneIdleTracker(provider string) *paneIdleTracker {
	if !providerHasIdleProbe(provider) {
		return nil
	}
	return &paneIdleTracker{provider: provider}
}

// forTurn re-arms the tracker on a new turn. The pane may not repaint between a
// delivery and the next poll, so observations must not carry across the turn
// boundary.
func (p *paneIdleTracker) forTurn(epoch uint64) {
	if p == nil || p.epoch == epoch {
		return
	}
	p.epoch = epoch
	p.idleStreak = 0
	p.workingStreak = 0
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
		p.idleStreak = 0
		return false
	}
	p.idleStreak++
	return p.idleStreak == p.confirmationsNeeded()
}

// confirmationsNeeded is per-probe because a false idle tells the orchestrator a
// lane is done while it is still working, while a false working costs only
// polling latency.
func (p *paneIdleTracker) confirmationsNeeded() int {
	if n := paneIdleProbes[p.provider].confirmations; n > 0 {
		return n
	}
	return paneIdleConfirmations
}

// observeWorking is the symmetric correction for a session already marked idle.
// It also requires consecutive frames so one half-painted frame cannot flap state.
func (p *paneIdleTracker) observeWorking(lines []string) bool {
	if p == nil {
		return false
	}
	working, known := paneShowsWorking(p.provider, lines)
	if !known || !working {
		p.workingStreak = 0
		return false
	}
	p.workingStreak++
	return p.workingStreak == p.confirmationsNeeded()
}

// reset forgets both streaks, so a session that went idle and was then given
// more work must be observed afresh in either direction.
func (p *paneIdleTracker) reset() {
	if p != nil {
		p.idleStreak = 0
		p.workingStreak = 0
	}
}

// paneIdleAction is what pollPaneIdle would do for one capture, before any
// tmux I/O. Tests drive this directly with literal pane fixtures.
type paneIdleAction int

const (
	paneIdleActionNone paneIdleAction = iota
	paneIdleActionMarkIdle
	paneIdleActionMarkRunning
)

func decidePaneIdlePoll(tracker *paneIdleTracker, status SessionStatus, lines []string) paneIdleAction {
	if tracker == nil {
		return paneIdleActionNone
	}
	if status.IsTerminal() {
		tracker.reset()
		return paneIdleActionNone
	}
	if status == StatusIdle {
		// Confirm working too; a single stray frame must not flap parent reports.
		if tracker.observeWorking(lines) {
			tracker.reset()
			return paneIdleActionMarkRunning
		}
		return paneIdleActionNone
	}
	if status != StatusRunning {
		tracker.reset()
		return paneIdleActionNone
	}
	if tracker.observe(lines) {
		return paneIdleActionMarkIdle
	}
	return paneIdleActionNone
}

// paneIdleCaptureLines is sized against the walks it feeds, not a typical
// footer. claudePaneJudge must find the composer, its upper rule, and
// claudePaneContentTailLines above that; too shallow a capture makes the judge
// unknown forever. Keep it equal to paneCaptureLines because
// claudeComposerMaxLines is sized against the same capture depth.
const paneIdleCaptureLines = paneCaptureLines

// claudePromptGlyph is the composer prompt in claude-code's TUI, U+276F.
const claudePromptGlyph = "❯"

// composerBody strips the prompt glyph off a captured composer line and returns
// what the user or bramble put after it, reporting whether the glyph was there
// at all.
//
// This is the one place the glyph is stripped, because the trims have to be
// Unicode-aware and are easy to get wrong in the same way: claude separates
// `❯` from the body with U+00A0, so an ASCII-only cutset leaves that byte
// behind and reads every EMPTY composer as holding text. judgeComposerLine
// would then report a draft on every idle pane, which silences the notifier
// permanently — it yields whenever it sees a draft.
func composerBody(line string) (body string, hasGlyph bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, claudePromptGlyph) {
		return strings.TrimSpace(trimmed), false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, claudePromptGlyph)), true
}

// composerReadable reports whether bramble can distinguish an empty composer
// from a draft. Only claude has a stable prompt glyph for that.
func composerReadable(provider string) bool {
	return provider == ProviderClaude
}

// composerDraft reports whether claude's composer holds a draft.
//
// The draft's text is deliberately not returned. The courier needed it to notice
// a changed draft and restart its hold; a hint has no hold to restart, and any
// draft at all is reason enough to stay quiet.
func composerDraft(provider string, lines []string) (draft, known bool) {
	if provider != ProviderClaude {
		return false, false
	}
	if composerIdx, _ := claudeComposerIdx(lines); composerIdx >= 0 {
		draft, known = judgeComposerLine(strings.TrimSpace(lines[composerIdx]))
		if !known {
			return true, true
		}
		return draft, known
	}
	// The composer could not be located by the bounded walk.
	if searchedForComposer(lines) {
		// Position still says where the composer is. If that line has the glyph,
		// it is legible even though the upper region was not bounded.
		if line, ok := lineAboveStatusRule(lines); ok {
			if draft, known := judgeComposerLine(line); known {
				return draft, known
			}
		}
		// The composer region exists but cannot be read, often because a long draft
		// filled it. Fail closed: the cost is one dropped hint, while writing here
		// can submit a human draft with this line appended.
		return true, true
	}
	// No status rule means no claude chrome to scope; the tail scan is the only
	// reader left, and there is no lower chrome competing for those rows.
	forEachPaneTailLine(lines, func(line string) bool {
		if !strings.HasPrefix(strings.TrimSpace(line), claudePromptGlyph) {
			return false
		}
		draft, known = judgeComposerLine(line)
		return true
	})
	return draft, known
}

// lineAboveStatusRule returns the first non-empty line above the lowest status
// rule, where claude draws its live composer even if the upper rule is missing.
func lineAboveStatusRule(lines []string) (string, bool) {
	sepIdx := statusSepIdx(lines)
	if sepIdx < 0 {
		return "", false
	}
	for i := sepIdx - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		return lines[i], true
	}
	return "", false
}

// searchedForComposer reports whether claude's status separator is present. If
// not, claiming a draft would hold mail against a pane with no proven composer.
func searchedForComposer(lines []string) bool {
	return statusSepIdx(lines) >= 0
}

// judgeComposerLine reports whether one composer line holds a draft. The
// glyph and the U+00A0 that follows it are handled by composerBody, which every
// composer reader shares.
func judgeComposerLine(line string) (draft, known bool) {
	body, hasGlyph := composerBody(line)
	if !hasGlyph {
		return false, false
	}
	if body == "" {
		return false, true
	}
	// Do not decide bramble provenance here; the prefix is user-controllable.
	// Any text in the composer is a draft to yield to, whoever wrote it.
	return true, true
}
