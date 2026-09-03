package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeTarget is a DeliveryTarget backed by a map, so notifier behaviour can be
// driven through every status and runner type without live managers or tmux.
type fakeTarget struct { //nolint:govet // fieldalignment: readability over packing
	mu         sync.Mutex
	sessions   map[SessionID]SessionInfo
	tmuxTarget string
	captured   map[SessionID][]string
	captureErr error
	// captureDelay stands in for how long a real pane capture takes. It is what
	// makes the courier's event handling slow enough to test what happens to the
	// events arriving behind it.
	captureDelay  time.Duration
	captureCount  int
	markedRunning []SessionID
	markedIdle    []SessionID
}

func newFakeTarget() *fakeTarget {
	return &fakeTarget{
		sessions:   make(map[SessionID]SessionInfo),
		captured:   make(map[SessionID][]string),
		tmuxTarget: "@7",
	}
}

func (f *fakeTarget) set(id SessionID, status SessionStatus, runner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prev := f.sessions[id]
	prev.ID = id
	prev.Status = status
	prev.RunnerType = runner
	f.sessions[id] = prev
}

// setChild registers a session as a subagent of parent, keeping whatever
// result paths a test has already attached.
func (f *fakeTarget) setChild(id, parent SessionID, status SessionStatus, runner string) {
	f.set(id, status, runner)
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	info.ParentSessionID = parent
	f.sessions[id] = info
}

// annotate attaches result metadata to an existing session.
func (f *fakeTarget) annotate(id SessionID, fn func(*SessionInfo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	fn(&info)
	f.sessions[id] = info
}

// setBackend records which agent CLI backs a session. The yield checks are
// provider-specific — only claude has a readable composer — so a test has to
// say which CLI it means.
func (f *fakeTarget) setBackend(id SessionID, backend, model string) {
	f.annotate(id, func(i *SessionInfo) {
		i.Backend = backend
		i.Model = model
	})
}

func (f *fakeTarget) SessionInfo(id SessionID) (SessionInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.sessions[id]
	return info, ok
}

func (f *fakeTarget) ResolveTmuxTarget(id SessionID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tmuxTarget == "" {
		return "", fmt.Errorf("session %s is not a tmux session", id)
	}
	return f.tmuxTarget, nil
}

func (f *fakeTarget) CapturePaneText(id SessionID, _ int) ([]string, error) {
	f.mu.Lock()
	f.captureCount++
	delay := f.captureDelay
	err := f.captureErr
	lines := f.captured[id]
	f.mu.Unlock()
	if delay > 0 {
		// Outside the lock: this stands in for a real pane capture, which
		// shells out to tmux and holds nothing of the courier's.
		time.Sleep(delay)
	}
	if err != nil {
		return nil, err
	}
	return lines, nil
}

// appendPane mirrors text into every session's pane buffer. A shared buffer is
// enough and keeps the fake from needing to know which session a paste targeted.
// setPane REPLACES every session's pane buffer, for tests that need the pane to
// stop saying what it said before — appendPane only ever adds, so an earlier
// working marker would still be found in the tail.
func (f *fakeTarget) setPane(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.sessions {
		f.captured[id] = strings.Split(text, "\n")
	}
}

func (f *fakeTarget) appendPane(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.sessions {
		f.captured[id] = append(f.captured[id], strings.Split(text, "\n")...)
	}
}

func (f *fakeTarget) MarkIdle(id SessionID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	if info.Status == StatusRunning {
		info.Status = StatusIdle
		f.sessions[id] = info
	}
	f.markedIdle = append(f.markedIdle, id)
}

func (f *fakeTarget) MarkRunning(id SessionID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	f.markedRunning = append(f.markedRunning, id)
	if info.Status != StatusIdle {
		return false
	}
	info.Status = StatusRunning
	f.sessions[id] = info
	return true
}

// mustInfo returns a registered session, for tests that hand a SessionInfo
// straight to a notifier method.
func (f *fakeTarget) mustInfo(id SessionID) SessionInfo {
	info, _ := f.SessionInfo(id)
	return info
}

// fakePanes records tmux writes in order so a test can assert that a paste was
// followed by the Enter that submits it.
type fakePanes struct { //nolint:govet // fieldalignment: readability over packing
	mu       sync.Mutex
	writes   []string
	pasteErr error
	// echo, when set, mirrors a pasted line into the pane a test reads back,
	// standing in for a TUI that accepted the paste.
	echo func(string)
	// onSubmit, when set, stands in for a TUI accepting Enter: the composer
	// clears and the text moves up into the transcript.
	onSubmit func()
	// enterErr, when set, stands in for a tmux send-keys that failed: the text
	// is in the composer and no turn was started.
	enterErr error
}

func (p *fakePanes) Paste(_ context.Context, target, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pasteErr != nil {
		return p.pasteErr
	}
	p.writes = append(p.writes, "paste("+target+"): "+text)
	if p.echo != nil {
		p.echo(text)
	}
	return nil
}

func (p *fakePanes) SendEnter(_ context.Context, target string) error {
	p.mu.Lock()
	onSubmit := p.onSubmit
	enterErr := p.enterErr
	p.writes = append(p.writes, "enter("+target+")")
	p.mu.Unlock()
	if enterErr != nil {
		return enterErr
	}
	if onSubmit != nil {
		onSubmit()
	}
	return nil
}

// pasteCount reports how many pastes reached the pane. Counting the writes
// rather than the echoes: a test whose echo is unset still needs to know
// whether the notifier pasted once or twice.
func (p *fakePanes) pasteCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, w := range p.writes {
		if strings.HasPrefix(w, "paste(") {
			n++
		}
	}
	return n
}

func (p *fakePanes) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.writes...)
}

// echoPanes stands in for a CLI that echoes a pasted message into its composer.
// It writes real claude chrome because the composer check reads only a locatable
// composer, not a bare line.
func echoPanes(target *fakeTarget) *fakePanes {
	p := &fakePanes{}
	p.echo = func(text string) { target.appendPane(claudeComposerPane(text)) }
	// Submitting scrolls the composer contents up into the transcript, leaving
	// a fresh empty composer behind.
	p.onSubmit = func() { target.appendPane(claudeComposerPane("")) }
	return p
}

// claudeComposerPane renders a composer holding body as claude draws it.
func claudeComposerPane(body string) string {
	return strings.Join([]string{
		"────────────────────────────────────────────",
		"❯ " + body,
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}, "\n")
}

// newTestNotifier builds a notifier whose legacy sweep points at a temp dir, so
// a test can never reclaim the developer's real ~/.bramble/deliveries.
func newTestNotifier(t *testing.T, target DeliveryTarget, panes PaneWriter) *Notifier {
	t.Helper()
	n, err := NewNotifier(target, panes, NotifierConfig{LegacyDeliveryDir: t.TempDir()})
	require.NoError(t, err)
	return n
}

// claudeChild registers an idle claude parent with a tmux child of its own and
// returns the child, ready to hand to NotifyParent.
func claudeChild(target *fakeTarget) SessionInfo {
	target.set("parent", StatusIdle, RunnerTypeTmux)
	target.setBackend("parent", "claude", "opus")
	target.setChild("child", "parent", StatusIdle, RunnerTypeTmux)
	target.setPane(claudeComposerPane(""))
	return target.mustInfo("child")
}

func TestAnIdleChildNudgesItsParentOnce(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Equal(t, []string{
		"paste(@7): " + nudgeText,
		"enter(@7)",
	}, panes.recorded(), "a hint is one paste and one Enter, nothing else")
	require.Equal(t, []SessionID{"parent"}, target.markedRunning,
		"submitting a prompt starts a turn, which nothing else reports for a tmux session")
}

// The live failure this change exists to remove: three sessions on the author's
// machine were wedged for hours because a leftover draft blocked every delivery
// and the courier kept retrying. A hint must simply stay quiet.
func TestADraftInTheComposerSilencesTheNudge(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.setPane(claudeComposerPane("half a thought the user is still typing"))
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(),
		"a human draft must never be appended to, and there is nothing to queue")
	require.Empty(t, target.markedRunning, "no turn was started")
}

// TestAStrandedNudgeIsSubmittedNotDropped pins the fix for issue #346: a
// previous nudge that pasted its line but never got the Enter that would have
// submitted it (a killed process, a dropped tmux write) leaves nudgeText
// sitting alone in the composer. Every later nudge attempt runs straight into
// the draft guard and reads it as a human's half-written line, so without
// recognizing its own words the wedge is permanent — nothing else will ever
// submit that line, and nothing else will ever nudge this parent again either
// (claimNudge is per-attempt, not a queue).
func TestAStrandedNudgeIsSubmittedNotDropped(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.setPane(claudeComposerPane(nudgeText))
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Equal(t, []string{"enter(@7)"}, panes.recorded(),
		"the stranded line is submitted with a bare Enter, never pasted again")
	require.Equal(t, []SessionID{"parent"}, target.markedRunning,
		"submitting the stranded nudge starts a turn like any other delivery")
}

// TestAHumanDraftThatMerelyContainsTheNudgeStaysProtected pins the boundary of
// the ownership comparison: it is exact-match on the whole composer body, not
// a substring or prefix check, so a human line that happens to quote or
// reference bramble's hint text is still a human's line.
func TestAHumanDraftThatMerelyContainsTheNudgeStaysProtected(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.setPane(claudeComposerPane(nudgeText + " -- I'm mid-thought, don't submit this"))
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(), "a human draft is never submitted, even one that echoes the hint text")
	require.Empty(t, target.markedRunning, "no turn was started")
}

// TestAHumanContinuationBelowAStrandedNudgeStaysProtected pins the case an
// exact match on the composer's top row alone gets wrong. claude wraps a
// composer over several rows and claudeComposerIdx returns the TOP one (see
// TestWrappedComposerStillReadsAsADraft), so a human who types a follow-up
// under a stranded nudge produces a block whose first row is byte-for-byte
// nudgeText. Submitting on that match would send the human's unfinished line
// riding along with it — the exact harm the draft guard exists to prevent, and
// strictly worse than the dropped nudge it was trying to fix.
func TestAHumanContinuationBelowAStrandedNudgeStaysProtected(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.setPane(strings.Join([]string{
		"────────────────────────────────────────────",
		"❯ " + nudgeText,
		"and my own follow-up that I have not finished typing yet",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}, "\n"))
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(),
		"a wrapped composer whose first row matches is still carrying a human's rows below it")
	require.Empty(t, target.markedRunning, "no turn was started")
}

// TestAnEchoedNudgeInAChromelessPaneIsNotResubmitted pins the other way a
// weaker locator can be wrong about ownership. With no status rule in the
// capture — a pane scrolled back, or one showing only transcript — the tail
// scan finds any "❯" line, including the transcript's echo of a nudge that was
// already submitted and answered. Pressing Enter there submits an empty
// composer and marks a turn that never starts, so the parent reads busy with
// nothing behind it.
func TestAnEchoedNudgeInAChromelessPaneIsNotResubmitted(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.setPane(strings.Join([]string{
		"some earlier transcript output",
		"❯ " + nudgeText,
	}, "\n"))
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(),
		"a transcript echo is not a composer bramble has proven it can submit")
	require.Empty(t, target.markedRunning, "no turn was started")
}

func TestAWorkingPaneSilencesTheNudge(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	// A spinner above the composer is claude's own "turn in flight" chrome. It
	// needs the full frame: the judge locates the composer first and refuses to
	// read a bare line it cannot place.
	target.setPane(strings.Join([]string{
		"● Read(delivery.go)",
		"────────────────────────────────────────────",
		"❯ ",
		"────────────────────────────────────────────",
		"  ~/wt/branch  main  Opus 4.6  ctx:43%  tokens:20k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}, "\n"))
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(), "a turn in flight is not interrupted")
}

// Coalescing is the only bookkeeping the notifier keeps, and it is what turns a
// wave of finishing lanes into one line instead of one per lane.
func TestManyChildrenFinishingProduceOneNudge(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("parent", StatusIdle, RunnerTypeTmux)
	target.setBackend("parent", "claude", "opus")
	target.setPane(claudeComposerPane(""))
	panes := &fakePanes{}
	n := newTestNotifier(t, target, panes)

	// Hold the claim the way a real in-flight nudge would, then report a wave.
	require.True(t, n.claimNudge("parent"))
	for i := range 5 {
		id := SessionID(fmt.Sprintf("child-%d", i))
		target.setChild(id, "parent", StatusIdle, RunnerTypeTmux)
		n.NotifyParent(t.Context(), target.mustInfo(id))
	}
	require.Empty(t, panes.recorded(), "a hint already in flight absorbs the rest of the wave")

	n.releaseNudge("parent")
	n.NotifyParent(t.Context(), target.mustInfo("child-0"))
	require.Equal(t, 1, panes.pasteCount(), "once the claim clears, one hint goes out")
}

// The hint carries no child, status, or path on purpose: anything it carried
// could be read after the fact and be wrong. This is what makes issue #330's
// "a replay and a real failure look identical" unrepresentable.
func TestTheNudgeCarriesNoState(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	claudeChild(target)
	target.annotate("child", func(i *SessionInfo) {
		i.Status = StatusFailed
		i.ErrorMsg = "lane died holding uncommitted work"
		i.ResearchFilePath = "/home/u/.bramble/research/child.md"
	})
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), target.mustInfo("child"))

	// Before the loop: every assertion below lives inside it, so without this a
	// run that hinted nothing at all would pass while proving nothing.
	require.NotEmpty(t, panes.recorded(), "a failed child must still hint")
	for _, w := range panes.recorded() {
		require.NotContains(t, w, "child", "a hint names no session")
		require.NotContains(t, w, "research", "a hint points at no file")
		require.NotContains(t, w, "died", "a hint reports no status")
	}
}

// TestEveryTerminalStatusHints covers the arms of hintWorthyStatus that no other
// test reaches.
//
// Idle is well covered and Running is covered negatively, but Failed, Completed
// and Stopped had no positive test — so narrowing the switch to `case
// StatusIdle` left the whole suite green. These are exactly the transitions
// issue #330 is about: a lane whose window died cannot report for itself, so
// the hint is the only thing that fires.
func TestEveryTerminalStatusHints(t *testing.T) {
	t.Parallel()
	for _, status := range []SessionStatus{StatusFailed, StatusCompleted, StatusStopped} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			target := newFakeTarget()
			claudeChild(target)
			target.annotate("child", func(i *SessionInfo) { i.Status = status })
			panes := echoPanes(target)

			newTestNotifier(t, target, panes).NotifyParent(t.Context(), target.mustInfo("child"))

			require.Equal(t, 1, panes.pasteCount(),
				"a child reaching %s is news its parent cannot get elsewhere", status)
		})
	}
}

func TestNoParentMeansNoNudge(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("orphan", StatusIdle, RunnerTypeTmux)
	panes := &fakePanes{}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), target.mustInfo("orphan"))

	require.Empty(t, panes.recorded())
}

func TestATerminalParentIsNotNudged(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.set("parent", StatusCompleted, RunnerTypeTmux)
	panes := &fakePanes{}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(), "a finished parent can never read it")
}

func TestABusyParentIsNotNudged(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.set("parent", StatusRunning, RunnerTypeTmux)
	panes := &fakePanes{}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(), "mid-turn text lands in the next prompt, out of context")
}

// A TUI parent has no pane to type into; its turn loop already surfaces child
// state through the model.
func TestATUIParentIsNotPastedInto(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.set("parent", StatusIdle, RunnerTypeTUI)
	panes := &fakePanes{}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded())
}

// An unreadable pane is silence, not a problem to solve. The notifier proceeds
// because refusing every pane it cannot parse would silence hints for every
// backend without claude's chrome — and a wrong hint costs nothing.
// TestAFailedCaptureYieldsForClaude pins the "unreadable frame" half of the
// yielding contract the Notifier doc states.
//
// A failed capture is not the same as a pane with nothing to say. For claude the
// draft guard is the only thing between this line and a human's half-typed one,
// and tmux paste-buffer appends — so writing while that guard is blind is
// exactly the failure the guard exists to prevent. Yielding costs one poll
// interval, which is what droppable buys.
func TestAFailedCaptureYieldsForClaude(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	target.captureErr = fmt.Errorf("pane is gone")
	panes := &fakePanes{}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, panes.recorded(),
		"a blind draft guard must not be written past")
	require.Empty(t, target.markedRunning, "and no turn was started")
}

// TestAFailedCaptureStillHintsWhereNoGuardApplies is the other half: gemini has
// neither an idle probe nor a readable composer, so no guard would have
// consulted the capture. Yielding there would withhold every hint from those
// providers for no protection — the capture is skipped, not failed.
func TestAFailedCaptureStillHintsWhereNoGuardApplies(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("parent", StatusIdle, RunnerTypeTmux)
	target.setBackend("parent", "gemini", "gemini-3-flash-preview")
	target.setChild("child", "parent", StatusIdle, RunnerTypeTmux)
	target.captureErr = fmt.Errorf("pane is gone")
	panes := &fakePanes{}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), target.mustInfo("child"))

	require.Equal(t, 1, panes.pasteCount(),
		"no guard reads this provider's pane, so a capture error withholds nothing")
}

func TestAFailedPasteIsDroppedNotRetried(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	panes := &fakePanes{pasteErr: fmt.Errorf("tmux said no")}
	n := newTestNotifier(t, target, panes)

	n.NotifyParent(t.Context(), child)

	require.Empty(t, target.markedRunning, "a failed paste starts no turn")
	// Nothing was retained: the claim is released, so the next event is free to
	// try again, but nothing is scheduled to do so on its own.
	require.True(t, n.claimNudge("parent"), "a failed hint leaves no claim behind")
}

// The queues found in practice were hours to days old, and one held ten status
// updates each announcing that it superseded the last. Replaying that history
// is precisely the noise being removed, so it is deleted rather than delivered.
func TestStartupDiscardsQueuesLeftByTheRetiredCourier(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stale := filepath.Join(dir, "babysit-prod-planner-bcc6f26d.json")
	require.NoError(t, os.WriteFile(stale, []byte(`[{"to":"p","text":"stale report"}]`), 0o600))
	keep := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(keep, []byte("unrelated"), 0o600))

	target := newFakeTarget()
	panes := &fakePanes{}
	_, err := NewNotifier(target, panes, NotifierConfig{LegacyDeliveryDir: dir})
	require.NoError(t, err)

	require.NoFileExists(t, stale, "a stale queue is reclaimed, not replayed")
	require.FileExists(t, keep, "only the courier's own .json queues are swept")
	require.Empty(t, panes.recorded(), "nothing from the old queue is delivered")
}

func TestASweepOfAMissingDirectoryIsNotAnError(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	_, err := NewNotifier(target, &fakePanes{}, NotifierConfig{
		LegacyDeliveryDir: filepath.Join(t.TempDir(), "never-existed"),
	})
	require.NoError(t, err)
}

// TestAFanOutNeverHintsPerChild pins what a burst of finishing children costs a
// parent's pane.
//
// No exact count, because none is guaranteed: claimNudge only turns away a hint
// that genuinely overlaps one in flight, and MarkRunning only stops children
// that read the parent after it lands, so a simultaneous release can produce
// anything from 1 to 8. Asserting exactly one failed 3 runs in 8; asserting
// fewer than 8 still failed 1 in 20. Coalescing itself is pinned
// deterministically by TestManyChildrenFinishingProduceOneNudge, which holds
// the claim rather than racing for it.
//
// What must hold here is only the ceiling: hints are never queued, so the pane
// can never accumulate more than the burst that produced them.
func TestAFanOutNeverHintsPerChild(t *testing.T) {
	t.Parallel()
	const children = 8
	target := newFakeTarget()
	child := claudeChild(target)
	panes := echoPanes(target)
	n := newTestNotifier(t, target, panes)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range children {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n.NotifyParent(t.Context(), child)
		}()
	}
	close(start)
	wg.Wait()

	pastes := panes.pasteCount()
	require.NotZero(t, pastes, "the parent is idle, so it must be hinted at least once")
	require.LessOrEqual(t, pastes, children,
		"a hint is never queued, so the pane can never hold more than one line per child")
}

// TestTheTurnIsMarkedBeforeTheEnter pins the ordering, not just the marking.
//
// The recipient can answer and fire its completion notify the instant Enter
// lands. If the turn were marked after, that notify would hit SetSessionIdle's
// compare-and-set while the session still read idle, be dropped, and then this
// would mark it running with nothing alive to end it — busy forever, which is
// the state the polling orchestrator reads as a working lane.
func TestTheTurnIsMarkedBeforeTheEnter(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	panes := echoPanes(target)

	var statusAtEnter SessionStatus
	inner := panes.onSubmit
	panes.onSubmit = func() {
		// Stands in for the notify arriving as Enter is processed.
		statusAtEnter = target.mustInfo("parent").Status
		inner()
	}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Equal(t, StatusRunning, statusAtEnter,
		"a notify landing with the Enter must find the session already running, or it is dropped")
}

// A failed Enter started no turn, so the marking must not stand: the session
// would otherwise read busy forever with nothing to end it.
func TestAFailedEnterRollsTheTurnBack(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	panes := &fakePanes{enterErr: fmt.Errorf("send-keys failed")}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Equal(t, []SessionID{"parent"}, target.markedIdle,
		"a submit that failed must put the session back")
	require.Equal(t, StatusIdle, target.mustInfo("parent").Status)
}

// TestAFailedEnterLeavesAnAlreadyRunningParentAlone pins the ownership half of
// the rollback on the notifier side.
//
// nudge reads parent.Status once, then does two tmux round-trips (a capture and
// a paste) before submitting. The parent can legitimately start a turn of its
// own in that window — a user typing, or another writer — at which point
// MarkRunning no-ops because the parent is already running. An unconditional
// rollback on a failed Enter would then report that live turn as finished,
// which is the exact failure this design exists to remove.
//
// The parent is moved to running from inside the paste, which is where that
// window actually is.
func TestAFailedEnterLeavesAnAlreadyRunningParentAlone(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	panes := &fakePanes{enterErr: fmt.Errorf("send-keys failed")}
	panes.echo = func(string) {
		// The parent starts its own turn between the paste and the Enter.
		target.set("parent", StatusRunning, RunnerTypeTmux)
	}

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), child)

	require.Empty(t, target.markedIdle,
		"a hint that did not start the turn must not end it")
	require.Equal(t, StatusRunning, target.mustInfo("parent").Status,
		"the parent's own turn survives the failed hint")
}

// TestAHintDoesNotHintBack pins termination.
//
// A hint is typed into the parent's pane and submitted, which starts a turn —
// so the parent goes running, then idle again. If that idle were itself
// hint-worthy the pair would volley forever, filling both panes. It is not:
// hints follow a *child's* transition, and a top-level parent has no parent to
// tell. The integration suite caught the raw pane count that made this look
// like a loop; this pins the actual rule.
func TestAHintDoesNotHintBack(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	child := claudeChild(target)
	panes := echoPanes(target)
	n := newTestNotifier(t, target, panes)

	n.NotifyParent(t.Context(), child)
	require.Equal(t, 1, panes.pasteCount(), "the child's finish hints once")

	// The parent is now running because the hint submitted a prompt. Its own
	// return to idle is the transition that would close the loop.
	target.set("parent", StatusIdle, RunnerTypeTmux)
	n.NotifyParent(t.Context(), target.mustInfo("parent"))

	require.Equal(t, 1, panes.pasteCount(),
		"a parent going idle must not hint: it has no parent, and a volley would never stop")
}

// TestAChildStartingWorkIsNotNews pins the status gate.
//
// A hint is typed and submitted, so it starts a real turn in the recipient.
// Hinting on idle→running would therefore spend a turn to report that a lane
// began working — and because the hint moves the recipient idle→running too,
// the same rule would fire again one level up.
func TestAChildStartingWorkIsNotNews(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	claudeChild(target)
	target.set("child", StatusRunning, RunnerTypeTmux)
	panes := echoPanes(target)

	newTestNotifier(t, target, panes).NotifyParent(t.Context(), target.mustInfo("child"))

	require.Empty(t, panes.recorded(), "a lane getting on with its work is not news")
}

// TestAHintDoesNotClimbTheAncestorChain is the case TestAHintDoesNotHintBack
// cannot reach: a parent that is itself somebody's child.
//
// new-session inherits --parent unless --no-parent is passed, so a nested swarm
// is the normal shape rather than an exotic one. Without the status gate, one
// leaf finishing hints its parent, which marks that parent running, which is a
// transition that hints the grandparent — one completion billing a turn to
// every ancestor.
func TestAHintDoesNotClimbTheAncestorChain(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("grandparent", StatusIdle, RunnerTypeTmux)
	target.setBackend("grandparent", "claude", "opus")
	target.setChild("parent", "grandparent", StatusIdle, RunnerTypeTmux)
	target.setBackend("parent", "claude", "opus")
	target.setChild("child", "parent", StatusIdle, RunnerTypeTmux)
	target.setPane(claudeComposerPane(""))
	panes := echoPanes(target)
	n := newTestNotifier(t, target, panes)

	n.NotifyParent(t.Context(), target.mustInfo("child"))
	require.Equal(t, 1, panes.pasteCount(), "the child's finish hints its parent once")

	// The hint just moved the parent idle→running. That transition is what
	// would climb, so feed it back the way Watch does.
	n.NotifyParent(t.Context(), target.mustInfo("parent"))

	require.Equal(t, 1, panes.pasteCount(),
		"a parent starting a turn must not hint the grandparent")
}
