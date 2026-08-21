package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTarget is a DeliveryTarget backed by a map, so courier behaviour can be
// driven through every status and runner type without live managers or tmux.
type fakeTarget struct { //nolint:govet // fieldalignment: readability over packing
	mu            sync.Mutex
	sessions      map[SessionID]SessionInfo
	followUps     []string
	tmuxTarget    string
	followErr     error
	captured      map[SessionID][]string
	captureErr    error
	markedRunning []SessionID
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

func (f *fakeTarget) SessionInfo(id SessionID) (SessionInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.sessions[id]
	return info, ok
}

func (f *fakeTarget) SendFollowUp(id SessionID, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.followErr != nil {
		return f.followErr
	}
	f.followUps = append(f.followUps, message)
	return nil
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
	defer f.mu.Unlock()
	if f.captureErr != nil {
		return nil, f.captureErr
	}
	return f.captured[id], nil
}

// appendPane mirrors text into every session's pane buffer. The courier only
// ever reads back the pane it just wrote to, so a shared buffer is enough and
// keeps the fake from needing to know which session a paste targeted.
func (f *fakeTarget) appendPane(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.sessions {
		f.captured[id] = append(f.captured[id], strings.Split(text, "\n")...)
	}
}

func (f *fakeTarget) MarkRunning(id SessionID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.sessions[id]
	if info.Status == StatusIdle {
		info.Status = StatusRunning
		f.sessions[id] = info
	}
	f.markedRunning = append(f.markedRunning, id)
}

// mustInfo returns a registered session, for tests that hand a SessionInfo
// straight to a courier method.
func (f *fakeTarget) mustInfo(id SessionID) SessionInfo {
	info, _ := f.SessionInfo(id)
	return info
}

func (f *fakeTarget) sentFollowUps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.followUps...)
}

// fakePanes records tmux writes in order so a test can assert that a paste was
// followed by the Enter that submits it.
type fakePanes struct { //nolint:govet // fieldalignment: readability over packing
	mu       sync.Mutex
	writes   []string
	pasteErr error
	// echo, when set, mirrors a pasted line into the pane the courier reads
	// back, standing in for a TUI that accepted the paste.
	echo func(string)
	// onSubmit, when set, stands in for a TUI accepting Enter: the composer
	// clears and the text moves up into the transcript.
	onSubmit func()
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
	p.writes = append(p.writes, "enter("+target+")")
	p.mu.Unlock()
	if onSubmit != nil {
		onSubmit()
	}
	return nil
}

func (p *fakePanes) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.writes...)
}

// echoPanes builds a pane writer whose pastes show up in target's pane, so the
// courier's paste verification sees what it just wrote.
func echoPanes(target *fakeTarget) *fakePanes {
	p := &fakePanes{}
	p.echo = func(text string) { target.appendPane(text) }
	// Submitting scrolls the composer contents up into the transcript. The
	// courier decides that by looking at the pane's tail, so stand in for the
	// fresh empty composer with enough lines to push the submitted text out of
	// it.
	p.onSubmit = func() { target.appendPane("> ") }
	return p
}

func newTestCourier(t *testing.T) (*Courier, *fakeTarget, *fakePanes) {
	t.Helper()
	target := newFakeTarget()
	// By default the fake TUI accepts what is pasted, so paste verification
	// passes. Tests that care about a dropped paste clear echo.
	panes := echoPanes(target)
	c, err := NewCourier(target, panes, t.TempDir())
	require.NoError(t, err)
	return c, target, panes
}

// reportFixture builds the setup every subagent-report test shares: a courier,
// a running parent, and a child of that parent in the given status.
func reportFixture(t *testing.T, childStatus SessionStatus) (*Courier, *fakeTarget, SessionID, SessionID) {
	t.Helper()
	parentID, childID := ids(t)
	c, target, _ := newTestCourier(t)
	target.set(parentID, StatusRunning, RunnerTypeTmux)
	target.setChild(childID, parentID, childStatus, RunnerTypeTmux)
	return c, target, parentID, childID
}

// reportNow reads the child's current state and runs one report pass over it,
// the way the state-change watcher does.
func reportNow(c *Courier, target *fakeTarget, childID SessionID) {
	child, _ := target.SessionInfo(childID)
	c.reportToParent(context.Background(), child)
}

// ids returns session IDs unique to this test. Result files are written to a
// shared directory keyed by session ID, so parallel tests reusing a literal
// "child" would overwrite each other's output.
func ids(t *testing.T) (parent, child SessionID) {
	t.Helper()
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, t.Name())
	return SessionID("parent-" + safe), SessionID("child-" + safe)
}

// TestSendToIdleTUISessionUsesFollowUp pins that a TUI-mode recipient is
// reached through the turn loop, not through tmux — it has no pane to type in.
func TestSendToIdleTUISessionUsesFollowUp(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTUI)

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)
	assert.False(t, queued, "an idle session should be written immediately")
	assert.Equal(t, []string{"hello"}, target.sentFollowUps())
	assert.Empty(t, panes.recorded(), "TUI sessions must not be driven through tmux")
}

// TestSendToIdleTmuxSessionPastesAndSubmits covers the other half of the switch
// that is the courier's reason to exist.
func TestSendToIdleTmuxSessionPastesAndSubmits(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTmux)

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)
	assert.False(t, queued)
	assert.Equal(t, []string{"paste(@7): hello", "enter(@7)"}, panes.recorded())
	assert.Empty(t, target.sentFollowUps())
}

// TestSendWithoutSubmitDoesNotPressEnter keeps the draft case working: text is
// staged in the pane for a human to review rather than submitted.
func TestSendWithoutSubmitDoesNotPressEnter(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTmux)

	_, err := c.Send(context.Background(), "", "s1", "draft", false)
	require.NoError(t, err)
	assert.Equal(t, []string{"paste(@7): draft"}, panes.recorded())
}

// TestSendToBusySessionQueuesInsteadOfInterrupting is the central case. Today's
// send-input would type into a running turn and land out of context; the whole
// queue exists so that cannot happen.
func TestSendToBusySessionQueuesInsteadOfInterrupting(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)
	assert.True(t, queued)
	assert.Empty(t, panes.recorded(), "nothing may be written while the recipient is mid-turn")
	assert.Empty(t, target.sentFollowUps())

	require.Len(t, c.Pending("s1"), 1)
	assert.Equal(t, "hello", c.Pending("s1")[0].Text)
}

// TestQueuedDeliveryLandsOnIdleTransition completes the story: the message is
// held, then written the moment the recipient is actually ready for it.
func TestQueuedDeliveryLandsOnIdleTransition(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)
	require.True(t, queued)

	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")

	assert.Equal(t, []string{"paste(@7): hello", "enter(@7)"}, panes.recorded())
	assert.Empty(t, c.Pending("s1"), "a written delivery must not be written twice")
}

// TestDrainWritesOneDeliveryPerIdleTransition pins the pacing rule. Writing a
// message starts the recipient's next turn, so a second write in the same drain
// would land mid-turn — the very thing being prevented.
func TestDrainWritesOneDeliveryPerIdleTransition(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)

	for _, msg := range []string{"first", "second", "third"} {
		_, err := c.Send(context.Background(), "", "s1", msg, true)
		require.NoError(t, err)
	}
	require.Len(t, c.Pending("s1"), 3)

	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")

	assert.Equal(t, []string{"paste(@7): first", "enter(@7)"}, panes.recorded())
	pending := c.Pending("s1")
	require.Len(t, pending, 2)
	assert.Equal(t, "second", pending[0].Text, "FIFO order must survive a partial drain")
	assert.Equal(t, "third", pending[1].Text)

	// Writing the first delivery started a turn, so the session is no longer
	// idle and a second drain right now is correctly a no-op.
	info, _ := target.SessionInfo("s1")
	require.Equal(t, StatusRunning, info.Status, "a submitted delivery should start a turn")
	c.Drain(context.Background(), "s1")
	assert.Len(t, panes.recorded(), 2, "nothing more may be written mid-turn")

	// That turn ends, and the next delivery goes out.
	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")
	assert.Equal(t, "paste(@7): second", panes.recorded()[2])
	assert.Len(t, c.Pending("s1"), 1)
}

// TestDrainWhileBusyIsANoOp guards the queue against a spurious state change.
func TestDrainWhileBusyIsANoOp(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	c.Drain(context.Background(), "s1")

	assert.Empty(t, panes.recorded())
	assert.Len(t, c.Pending("s1"), 1)
}

// TestFailedWriteKeepsDeliveryQueued makes a transient tmux error a retry
// rather than a silent drop.
func TestFailedWriteKeepsDeliveryQueued(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	panes.pasteErr = errors.New("tmux exploded")
	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")
	require.Len(t, c.Pending("s1"), 1, "a failed write must not consume the delivery")

	panes.pasteErr = nil
	c.Drain(context.Background(), "s1")
	assert.Equal(t, []string{"paste(@7): hello", "enter(@7)"}, panes.recorded())
	assert.Empty(t, c.Pending("s1"))
}

// TestSendToTerminalSessionIsRefused stops a caller from queueing a message
// that nothing will ever deliver.
func TestSendToTerminalSessionIsRefused(t *testing.T) {
	t.Parallel()
	for _, status := range []SessionStatus{StatusCompleted, StatusFailed, StatusStopped} {
		c, target, _ := newTestCourier(t)
		target.set("s1", status, RunnerTypeTmux)

		_, err := c.Send(context.Background(), "", "s1", "hello", true)
		require.Error(t, err, "status %s should be refused", status)
		assert.Contains(t, err.Error(), string(status))
		assert.Empty(t, c.Pending("s1"))
	}
}

// TestSendToUnknownSessionErrors keeps a typo'd ID from silently creating a
// queue nobody reads.
func TestSendToUnknownSessionErrors(t *testing.T) {
	t.Parallel()
	c, _, _ := newTestCourier(t)

	_, err := c.Send(context.Background(), "", "ghost", "hello", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Empty(t, c.Pending("ghost"))
}

// TestDrainDiscardsQueueForTerminalSession stops the on-disk queue leaking when
// a recipient dies with mail still waiting.
func TestDrainDiscardsQueueForTerminalSession(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	target.set("s1", StatusFailed, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")

	assert.Empty(t, c.Pending("s1"))
}

// TestQueueSurvivesReload is why the queue is on disk: a bramble restart must
// not lose a subagent's report.
func TestQueueSurvivesReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("s1", StatusRunning, RunnerTypeTmux)

	c1, err := NewCourier(target, echoPanes(target), dir)
	require.NoError(t, err)
	for _, msg := range []string{"first", "second"} {
		_, err := c1.Send(context.Background(), "", "s1", msg, true)
		require.NoError(t, err)
	}

	panes := echoPanes(target)
	c2, err := NewCourier(target, panes, dir)
	require.NoError(t, err)

	pending := c2.Pending("s1")
	require.Len(t, pending, 2)
	assert.Equal(t, "first", pending[0].Text)
	assert.Equal(t, "second", pending[1].Text)

	target.set("s1", StatusIdle, RunnerTypeTmux)
	c2.Drain(context.Background(), "s1")
	assert.Equal(t, []string{"paste(@7): first", "enter(@7)"}, panes.recorded())
}

// TestEmptyQueueLeavesNoFile keeps the delivery directory from filling with
// empty stubs, one per session that ever received a message.
func TestEmptyQueueLeavesNoFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("s1", StatusRunning, RunnerTypeTmux)

	c, err := NewCourier(target, echoPanes(target), dir)
	require.NoError(t, err)
	_, err = c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	require.Len(t, files, 1)

	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")

	files, err = filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	assert.Empty(t, files)
}

// TestQueueFileNameIsSanitized keeps a hand-passed session ID from escaping the
// delivery directory. Generated IDs are tame, but the ID reaches this code
// straight off a socket.
func TestQueueFileNameIsSanitized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("../../escape", StatusRunning, RunnerTypeTmux)

	c, err := NewCourier(target, &fakePanes{}, dir)
	require.NoError(t, err)
	_, err = c.Send(context.Background(), "", "../../escape", "hello", true)
	require.NoError(t, err)

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	require.Len(t, files, 1, "the queue must stay inside the delivery dir")
	assert.Equal(t, "______escape.json", filepath.Base(files[0]))

	// The path the unsanitized ID would have produced must not exist.
	escaped := filepath.Join(dir, "../../escape.json")
	_, statErr := os.Stat(escaped)
	assert.True(t, os.IsNotExist(statErr), "queue escaped to %s", escaped)
}

// TestPendingReturnsACopy stops a caller mutating the courier's queue.
func TestPendingReturnsACopy(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	got := c.Pending("s1")
	got[0].Text = "tampered"
	assert.Equal(t, "hello", c.Pending("s1")[0].Text)
}

// TestSendToPendingSessionQueues covers a session spawned but not yet running:
// it has no runner type, so there is nowhere to write yet.
func TestSendToPendingSessionQueues(t *testing.T) {
	t.Parallel()
	c, target, panes := newTestCourier(t)
	target.set("s1", StatusPending, "")

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)
	assert.True(t, queued)
	assert.Empty(t, panes.recorded())
}

// TestWatchDrainsOnIdle exercises the real wiring: a live Manager's state
// changes drive the courier with no polling anywhere.
func TestWatchDrainsOnIdle(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	panes := echoPanes(target)
	c, err := NewCourier(target, panes, t.TempDir())
	require.NoError(t, err)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := c.Watch(ctx, mgr)
	defer unsub()

	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err = c.Send(ctx, "", "s1", "hello", true)
	require.NoError(t, err)

	// Flip the fake to idle first, then announce the transition, mirroring the
	// order the Manager itself uses.
	target.set("s1", StatusIdle, RunnerTypeTmux)
	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: "s1", OldStatus: StatusRunning, NewStatus: StatusIdle,
	})

	require.Eventually(t, func() bool { return len(c.Pending("s1")) == 0 },
		5*time.Second, 10*time.Millisecond,
		"queued delivery was not written after the idle transition")
	assert.Equal(t, []string{"paste(@7): hello", "enter(@7)"}, panes.recorded())
}

// TestWatchDiscardsOnTerminal pins the cleanup half of the watcher.
func TestWatchDiscardsOnTerminal(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	c, err := NewCourier(target, echoPanes(target), t.TempDir())
	require.NoError(t, err)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := c.Watch(ctx, mgr)
	defer unsub()

	target.set("s1", StatusRunning, RunnerTypeTmux)
	_, err = c.Send(ctx, "", "s1", "hello", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1)

	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: "s1", OldStatus: StatusRunning, NewStatus: StatusFailed,
	})

	require.Eventually(t, func() bool {
		return len(c.Pending("s1")) == 0
	}, 5*time.Second, 10*time.Millisecond, "queue for a dead session should be reclaimed")
}

// TestNewCourierIgnoresJunkFiles keeps a stray file in the delivery directory
// from failing bramble's startup.
func TestNewCourierIgnoresJunkFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{{{"), 0o644))

	c, err := NewCourier(newFakeTarget(), &fakePanes{}, dir)
	require.NoError(t, err)
	assert.Empty(t, c.Pending("s1"))
}

// --- subagent auto-reporting -------------------------------------------------

// TestChildIdleReportsToParent is the codex case. A non-Claude child cannot be
// reliably told to call back, so bramble reports on its behalf — otherwise a
// parent that spawned a codex subagent would wait forever.
func TestChildIdleReportsToParent(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)
	target.annotate(childID, func(i *SessionInfo) {
		i.Type = SessionTypeCodeTalk
		i.Model = "gpt-5.4-mini"
		i.ResearchFilePath = "/tmp/some-plan/child.md"
		i.Progress = SessionProgressSnapshot{TurnCount: 2, TotalCostUSD: 0.25}
	})

	reportNow(c, target, childID)

	pending := c.Pending(parentID)
	require.Len(t, pending, 1, "the parent should have been told its child is done")
	assert.Equal(t, SessionID(childID), pending[0].From)
	assert.Contains(t, pending[0].Text, "subagent child")
	assert.Contains(t, pending[0].Text, "gpt-5.4-mini")
	assert.Contains(t, pending[0].Text, "result: /tmp/some-plan/child.md")
}

// TestReportIsSentOnlyOncePerStatus keeps a chatty session from nagging its
// parent with the same news after every state change.
func TestReportIsSentOnlyOncePerStatus(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	reportNow(c, target, childID)
	reportNow(c, target, childID)
	reportNow(c, target, childID)

	assert.Len(t, c.Pending(parentID), 1)
}

// TestCompletedAfterIdleIsSilent pins the quiet rule: a tmux window closing
// after the result was already reported carries no new information.
func TestCompletedAfterIdleIsSilent(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	reportNow(c, target, childID)

	target.setChild(childID, parentID, StatusCompleted, RunnerTypeTmux)
	reportNow(c, target, childID)

	assert.Len(t, c.Pending(parentID), 1, "completion after a report adds nothing")
}

// TestFailureIsReportedEvenAfterAnIdleReport is the exception to that rule: a
// failure changes what the parent should do next.
func TestFailureIsReportedEvenAfterAnIdleReport(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	reportNow(c, target, childID)

	target.setChild(childID, parentID, StatusFailed, RunnerTypeTmux)
	target.annotate(childID, func(i *SessionInfo) { i.ErrorMsg = "context window exhausted" })
	reportNow(c, target, childID)

	pending := c.Pending(parentID)
	require.Len(t, pending, 2)
	assert.Contains(t, pending[1].Text, "context window exhausted")
}

// TestCompletedWithoutPriorReportIsAnnounced covers a child that dies before
// ever going idle — the parent still needs to hear about it.
func TestCompletedWithoutPriorReportIsAnnounced(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusCompleted)

	reportNow(c, target, childID)

	require.Len(t, c.Pending(parentID), 1)
}

// TestChildSelfReportSuppressesGeneratedReport keeps bramble from talking over
// a subagent that wrote its own, better summary.
func TestChildSelfReportSuppressesGeneratedReport(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusRunning)

	// The child speaks for itself while still mid-turn.
	_, err := c.Send(context.Background(), childID, parentID, "done: see /tmp/mine.md", true)
	require.NoError(t, err)

	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)

	pending := c.Pending(parentID)
	require.Len(t, pending, 1, "bramble should not repeat what the child already said")
	assert.Equal(t, "done: see /tmp/mine.md", pending[0].Text)
}

// TestUnrelatedSenderDoesNotSuppressReport guards the suppression from firing
// on a message between two sessions that are not parent and child.
func TestUnrelatedSenderDoesNotSuppressReport(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusRunning)
	target.set("stranger", StatusRunning, RunnerTypeTmux)

	_, err := c.Send(context.Background(), "stranger", parentID, "fyi", true)
	require.NoError(t, err)

	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)

	assert.Len(t, c.Pending(parentID), 2)
}

// TestTopLevelSessionReportsToNobody keeps ordinary sessions from generating
// mail for a parent they do not have.
func TestTopLevelSessionReportsToNobody(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("solo", StatusIdle, RunnerTypeTmux)

	info, _ := target.SessionInfo("solo")
	c.reportToParent(context.Background(), info)

	assert.Empty(t, c.Pending("solo"))
	assert.Empty(t, c.Pending(""))
}

// TestReportPrefersPlanOverTranscript: a planner subagent was asked to produce
// a plan, so that path is the answer — the transcript is just what it said.
func TestReportPrefersPlanOverTranscript(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	info := SessionInfo{
		ID: childID, Type: SessionTypePlanner, Status: StatusIdle,
		ParentSessionID:  parentID,
		PlanFilePath:     "/plans/x.md",
		ResearchFilePath: "/tmp/x.md",
	}
	text := formatSubagentReport(info, info.PlanFilePath)
	assert.Contains(t, text, "plan: /plans/x.md")
	assert.NotContains(t, text, "/tmp/x.md")
}

// TestReportToIdleParentIsWrittenImmediately checks the report takes the same
// delivery path as any other message rather than always queueing.
func TestReportToIdleParentIsWrittenImmediately(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	c, target, panes := newTestCourier(t)
	target.set(parentID, StatusIdle, RunnerTypeTmux)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)

	reportNow(c, target, childID)

	require.Len(t, panes.recorded(), 2)
	assert.Contains(t, panes.recorded()[0], "subagent child")
	assert.Empty(t, c.Pending(parentID))
}

// TestFailedReportIsRetriedOnTheNextTransition is the dedup-before-delivery
// case: shouldReport reserves the status before the write is attempted, so a
// write that fails must release the reservation or the parent never hears about
// this child again.
func TestFailedReportIsRetriedOnTheNextTransition(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)
	// An idle parent is written to directly, so a paste failure surfaces as a
	// failed report rather than a queued one.
	target.set(parentID, StatusIdle, RunnerTypeTmux)
	panes := &fakePanes{pasteErr: errors.New("tmux exploded")}
	c.panes = panes

	reportNow(c, target, childID)
	require.Empty(t, c.Pending(parentID), "the write failed, so nothing should be queued")
	require.Empty(t, panes.recorded(), "a failed paste records no write")

	// The same child goes idle again; the report must be attempted afresh.
	working := echoPanes(target)
	c.panes = working
	reportNow(c, target, childID)

	assert.Contains(t, strings.Join(working.recorded(), "\n"), "subagent "+string(childID),
		"the report was never retried after the first attempt failed")
}

// TestGeneratedReportDoesNotCountAsTheChildSpeaking keeps the courier's own
// report from registering as a self-report. If it did, a failed delivery would
// leave every remaining state marked as already told.
func TestGeneratedReportDoesNotCountAsTheChildSpeaking(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)
	target.set(parentID, StatusIdle, RunnerTypeTmux)
	c.panes = &fakePanes{pasteErr: errors.New("tmux exploded")}

	reportNow(c, target, childID)

	// A failure must leave nothing recorded for this child at all.
	c.mu.Lock()
	seen := len(c.reported[childID])
	c.mu.Unlock()
	assert.Zero(t, seen, "a failed report must leave no state claiming the parent was told")
	_ = parentID
}

// TestReportToDeadParentIsDropped covers the child outliving its parent: there
// is nowhere to report, and that must not be an error or a leaked queue.
func TestReportToDeadParentIsDropped(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	c, target, _ := newTestCourier(t)
	target.set(parentID, StatusCompleted, RunnerTypeTmux)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)

	reportNow(c, target, childID)

	assert.Empty(t, c.Pending(parentID))
}

// TestQueueThatCannotPersistIsNotReportedAsQueued pins the meaning of
// "queued": the caller is told the message survives a restart, so a queue that
// could not be written must be an error rather than a promise this process
// cannot keep.
func TestQueueThatCannotPersistIsNotReportedAsQueued(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	dir := t.TempDir()
	c, err := NewCourier(target, &fakePanes{}, dir)
	require.NoError(t, err)
	target.set("s1", StatusRunning, RunnerTypeTmux)

	// Make the queue file unwritable by putting a directory where it goes.
	require.NoError(t, os.MkdirAll(c.queuePath("s1"), 0o700))

	queued, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.Error(t, err, "an unpersistable queue must not be reported as queued")
	assert.False(t, queued)
	assert.Empty(t, c.Pending("s1"), "the failed delivery must not linger in memory either")
}

// TestDrainIdleDeliversAfterAReload covers the restart gap: Watch only reacts
// to idle transitions, and a recipient that is already idle when bramble comes
// back makes no transition to react to.
func TestDrainIdleDeliversAfterAReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("s1", StatusRunning, RunnerTypeTmux)

	first, err := NewCourier(target, echoPanes(target), dir)
	require.NoError(t, err)
	queued, err := first.Send(context.Background(), "", "s1", "held over a restart", true)
	require.NoError(t, err)
	require.True(t, queued)

	// A fresh courier over the same directory is what a restart looks like.
	reloaded, err := NewCourier(target, echoPanes(target), dir)
	require.NoError(t, err)
	require.Len(t, reloaded.Pending("s1"), 1, "the queue should have been reloaded")

	// The recipient is already idle and will never transition again.
	target.set("s1", StatusIdle, RunnerTypeTmux)
	reloaded.DrainIdle(context.Background())

	assert.Empty(t, reloaded.Pending("s1"), "a queue reloaded for an already-idle session was never delivered")
}

// TestResultArtifactsAreNotWorldReadable pins the privacy of what a subagent
// leaves in a shared temp dir: a captured pane is the child's whole transcript,
// and os.TempDir() is traversable by every local user.
func TestResultArtifactsAreNotWorldReadable(t *testing.T) {
	// Not parallel: the result dir is shared by every session under one HOME,
	// which TestMain has already pointed at a temp dir. Against a real shared
	// dir this test passed even with the fix reverted, because os.WriteFile
	// leaves an existing file's mode alone and an earlier run had created it
	// 0600 — so it must start from a directory it knows nothing has touched.
	t.Setenv("HOME", t.TempDir())
	c, target, _, childID := reportFixture(t, StatusIdle)
	target.annotate(childID, func(i *SessionInfo) { i.RunnerType = RunnerTypeTmux })
	target.captured[childID] = []string{"secret: hunter2"}

	path := c.resultPathFor(target.mustInfo(childID))
	require.NotEmpty(t, path, "the pane capture should have produced a result file")

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "a transcript must not be readable by other users")

	di, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), di.Mode().Perm(), "the result dir must not be traversable by other users")
}

// TestCompletedChildIsReportedAfterTheManagerDropsIt is the window-close path
// Gemini and Agy depend on, and the one the tmux monitor races: it emits
// StatusCompleted and immediately deletes the session, so a subscriber that
// looks the child up by ID finds nothing and reports nothing.
func TestCompletedChildIsReportedAfterTheManagerDropsIt(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	target := newFakeTarget()
	c, err := NewCourier(target, echoPanes(target), t.TempDir())
	require.NoError(t, err)
	target.set(parentID, StatusRunning, RunnerTypeTmux)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := c.Watch(ctx, mgr)
	defer unsub()

	// The child is gone from the target by the time the event is handled —
	// exactly what deleting it from m.sessions produces.
	child := SessionInfo{
		ID:              childID,
		ParentSessionID: parentID,
		Status:          StatusCompleted,
		Type:            SessionTypeCodeTalk,
		RunnerType:      RunnerTypeTmux,
	}
	mgr.emitSessionStateChange(SessionStateChangeEvent{
		Info:      child,
		SessionID: childID,
		OldStatus: StatusRunning,
		NewStatus: StatusCompleted,
	})

	require.Eventually(t, func() bool { return len(c.Pending(parentID)) == 1 },
		5*time.Second, 10*time.Millisecond,
		"a child the manager already dropped was never reported to its parent")
	assert.Contains(t, c.Pending(parentID)[0].Text, "is completed")
}

// TestWatchReportsChildCompletion is the end-to-end wiring: a real Manager
// state change produces a report with no polling.
func TestWatchReportsChildCompletion(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	target := newFakeTarget()
	c, err := NewCourier(target, echoPanes(target), t.TempDir())
	require.NoError(t, err)

	mgr := NewManagerWithConfig(ManagerConfig{RepoName: "repo"})
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := c.Watch(ctx, mgr)
	defer unsub()

	target.set(parentID, StatusRunning, RunnerTypeTmux)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	target.annotate(childID, func(i *SessionInfo) { i.ResearchFilePath = "/tmp/child.md" })

	mgr.emitSessionStateChange(SessionStateChangeEvent{
		SessionID: childID, OldStatus: StatusRunning, NewStatus: StatusIdle,
	})

	require.Eventually(t, func() bool {
		return len(c.Pending(parentID)) == 1
	}, 5*time.Second, 10*time.Millisecond, "parent was never told its subagent finished")
	assert.Contains(t, c.Pending(parentID)[0].Text, "/tmp/child.md")
}

// TestTmuxChildResultComesFromPaneCapture is the codex-in-tmux path. That mode
// never runs the TUI turn loop, so bramble holds no transcript — without the
// capture the parent would be told "your subagent finished" and handed nothing.
func TestTmuxChildResultComesFromPaneCapture(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)
	target.mu.Lock()
	target.captured[childID] = []string{"codex here", "the answer is 42"}
	target.mu.Unlock()

	reportNow(c, target, childID)

	pending := c.Pending(parentID)
	require.Len(t, pending, 1)

	path, err := ResultFilePath(childID)
	require.NoError(t, err)
	assert.Contains(t, pending[0].Text, "result: "+path)

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "codex here\nthe answer is 42\n", string(body))
	t.Cleanup(func() { os.Remove(path) })
}

// TestCaptureFailureStillReports keeps a dead pane from swallowing the report:
// the parent needs to know the child finished even without a result file.
func TestCaptureFailureStillReports(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusFailed)
	target.mu.Lock()
	target.captureErr = errors.New("window is gone")
	target.mu.Unlock()

	reportNow(c, target, childID)

	pending := c.Pending(parentID)
	require.Len(t, pending, 1)
	assert.Contains(t, pending[0].Text, "subagent child")
	assert.NotContains(t, pending[0].Text, "result:")
}

// TestTUIChildDoesNotCapturePane guards against reaching for tmux on a session
// that has no pane; its transcript is the result.
func TestTUIChildDoesNotCapturePane(t *testing.T) {
	t.Parallel()
	parentID, childID := ids(t)
	c, target, _ := newTestCourier(t)
	target.set(parentID, StatusRunning, RunnerTypeTmux)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTUI)
	target.annotate(childID, func(i *SessionInfo) { i.ResearchFilePath = "/tmp/transcript.md" })

	reportNow(c, target, childID)

	require.Len(t, c.Pending(parentID), 1)
	assert.Contains(t, c.Pending(parentID)[0].Text, "result: /tmp/transcript.md")
}

// TestFollowUpToChildRearmsReporting is what makes a conversation possible
// rather than a single exchange. The child's first idle is reported; then the
// parent replies, and the answer to *that* must be reported too, or the parent
// is left polling a child it just spoke to.
func TestFollowUpToChildRearmsReporting(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	// Round 1: the child finishes and is reported.
	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1)

	// The parent replies. The child is idle, so this is written straight away.
	_, err := c.Send(context.Background(), parentID, childID, "round two please", true)
	require.NoError(t, err)

	// Round 2: the child finishes again and must be reported again.
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)

	assert.Len(t, c.Pending(parentID), 2, "the second answer was never reported")
}

// TestUnansweredChildIsStillReportedOnlyOnce keeps the re-arming narrow: with
// no new message, repeated idle transitions stay quiet.
func TestUnansweredChildIsStillReportedOnlyOnce(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	child, _ := target.SessionInfo(childID)
	for i := 0; i < 3; i++ {
		c.reportToParent(context.Background(), child)
	}
	assert.Len(t, c.Pending(parentID), 1)
}

// TestQueuedFollowUpRearmsReportingWhenDelivered covers the same rule on the
// deferred path: the re-arm must happen when the message is actually written,
// not when it was queued.
func TestQueuedFollowUpRearmsReportingWhenDelivered(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1)

	// The child is busy again, so the parent's reply is held.
	target.setChild(childID, parentID, StatusRunning, RunnerTypeTmux)
	queued, err := c.Send(context.Background(), parentID, childID, "round two", true)
	require.NoError(t, err)
	require.True(t, queued)

	// Still one report: nothing has been delivered to the child yet.
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1, "queueing alone must not re-arm reporting")

	// Now it lands, and the following turn is reportable again.
	c.Drain(context.Background(), childID)
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)
	assert.Len(t, c.Pending(parentID), 2)
}

// TestSubmittedWriteMarksSessionRunning pins the fix for a bug that silently
// ended every two-way conversation after one round.
//
// A tmux session's status comes entirely from outside: its agent's notify hook
// reports idleness and nothing reports the opposite. So a session bramble typed
// into stayed "idle" for the whole turn, and the notify that ended that turn hit
// SetSessionIdle's StatusRunning guard and was dropped — no state change, no
// drain, no report. The parent simply never heard back a second time.
func TestSubmittedWriteMarksSessionRunning(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTmux)

	_, err := c.Send(context.Background(), "", "s1", "do the thing", true)
	require.NoError(t, err)

	assert.Equal(t, []SessionID{"s1"}, target.markedRunning)
	info, _ := target.SessionInfo("s1")
	assert.Equal(t, StatusRunning, info.Status)
}

// TestUnsubmittedWriteDoesNotMarkRunning: staging a draft in a pane starts no
// turn, so claiming one would strand the session in "running".
func TestUnsubmittedWriteDoesNotMarkRunning(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTmux)

	_, err := c.Send(context.Background(), "", "s1", "draft", false)
	require.NoError(t, err)

	assert.Empty(t, target.markedRunning)
	info, _ := target.SessionInfo("s1")
	assert.Equal(t, StatusIdle, info.Status)
}

// TestTUIWriteDoesNotMarkRunning: the TUI turn loop sets StatusRunning itself
// when it picks the follow-up off the channel.
func TestTUIWriteDoesNotMarkRunning(t *testing.T) {
	t.Parallel()
	c, target, _ := newTestCourier(t)
	target.set("s1", StatusIdle, RunnerTypeTUI)

	_, err := c.Send(context.Background(), "", "s1", "hello", true)
	require.NoError(t, err)

	assert.Empty(t, target.markedRunning)
}

// TestTwoWayConversationKeepsReporting is the whole feature in one test: a
// child reports, its parent replies, and the answer to that reply is reported
// too. Both fixes are load-bearing here — re-arming the idle report, and
// marking the session running so the turn boundary exists at all.
func TestTwoWayConversationKeepsReporting(t *testing.T) {
	t.Parallel()
	c, target, parentID, childID := reportFixture(t, StatusIdle)

	// Round 1: the child answers its opening prompt.
	reportNow(c, target, childID)
	require.Len(t, c.Pending(parentID), 1)

	// The parent replies; the child starts a turn.
	_, err := c.Send(context.Background(), parentID, childID, "round two", true)
	require.NoError(t, err)
	info, _ := target.SessionInfo(childID)
	require.Equal(t, StatusRunning, info.Status)

	// Round 2 ends, and the parent hears about it.
	target.setChild(childID, parentID, StatusIdle, RunnerTypeTmux)
	reportNow(c, target, childID)

	assert.Len(t, c.Pending(parentID), 2, "the second round was never reported")
}

// TestDroppedPasteIsRetried covers the gap between an agent announcing it is
// idle and its TUI being ready for input. tmux reports success for a paste the
// TUI drops, so without a read-back the message vanishes silently — and the
// session is then marked running for a turn that never began.
func TestDroppedPasteIsRetried(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusIdle, RunnerTypeTmux)

	panes := echoPanes(target)
	var pastes int
	panes.echo = func(text string) {
		// The first paste is swallowed, as codex does mid-finalize.
		pastes++
		if pastes > 1 {
			target.appendPane(text)
		}
	}
	c, err := NewCourier(target, panes, t.TempDir())
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "the real message", true)
	require.NoError(t, err)

	assert.Equal(t, 2, pastes, "a dropped paste should be retried once")
	assert.Contains(t, panes.recorded(), "enter(@7)", "the retry must still be submitted")
	assert.Equal(t, []SessionID{"s1"}, target.markedRunning)
}

// TestPersistentlyDroppedPasteKeepsDeliveryQueued: if the prompt never takes
// the text, the message must stay queued for the next idle rather than being
// reported as delivered — and the session must not be marked running for a
// turn that never started.
func TestPersistentlyDroppedPasteKeepsDeliveryQueued(t *testing.T) {
	t.Parallel()
	target := newFakeTarget()
	target.set("s1", StatusRunning, RunnerTypeTmux)

	panes := &fakePanes{} // echo unset: every paste is swallowed
	c, err := NewCourier(target, panes, t.TempDir())
	require.NoError(t, err)

	_, err = c.Send(context.Background(), "", "s1", "never lands", true)
	require.NoError(t, err)
	require.Len(t, c.Pending("s1"), 1)

	target.set("s1", StatusIdle, RunnerTypeTmux)
	c.Drain(context.Background(), "s1")

	assert.Len(t, c.Pending("s1"), 1, "an undelivered message must stay queued")
	assert.NotContains(t, panes.recorded(), "enter(@7)", "nothing may be submitted")
	assert.Empty(t, target.markedRunning, "no turn started, so none may be claimed")
}

// TestConcurrentSendsToOneRecipientAllPersist covers several subagents
// finishing at once and reporting to the same parent — the normal shape of a
// fan-out, and the only place the queue takes concurrent writes.
//
// In memory this was always safe. On disk it was not: each enqueue took a
// snapshot under the lock and then wrote it *outside* the lock, so a goroutine
// that snapshotted first could write last and put back a queue missing
// everything appended in between. The messages were delivered normally, so the
// loss only surfaced after a restart — the one case the on-disk queue exists
// for.
func TestConcurrentSendsToOneRecipientAllPersist(t *testing.T) {
	t.Parallel()

	const senders = 12
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("parent", StatusRunning, RunnerTypeTmux)

	c, err := NewCourier(target, echoPanes(target), dir)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := c.Send(context.Background(), "", "parent", fmt.Sprintf("report-%02d", i), true)
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()

	require.Len(t, c.Pending("parent"), senders, "in-memory queue lost a report")

	// Reload from disk: this is what a restarted bramble would see.
	reloaded, err := NewCourier(target, echoPanes(target), dir)
	require.NoError(t, err)
	assert.Lenf(t, reloaded.Pending("parent"), senders,
		"the persisted queue lost reports; a restart would drop them")
}

// TestConcurrentDrainAndSendKeepsQueueConsistent covers the other overlap: a
// parent going idle and draining while more subagents are still reporting to
// it.
func TestConcurrentDrainAndSendKeepsQueueConsistent(t *testing.T) {
	t.Parallel()

	const senders = 12
	dir := t.TempDir()
	target := newFakeTarget()
	target.set("parent", StatusIdle, RunnerTypeTmux)

	c, err := NewCourier(target, echoPanes(target), dir)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := c.Send(context.Background(), "", "parent", fmt.Sprintf("report-%02d", i), true)
			assert.NoError(t, err)
		}(i)
	}
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target.set("parent", StatusIdle, RunnerTypeTmux)
			c.Drain(context.Background(), "parent")
		}()
	}
	wg.Wait()

	// Whatever is still queued in memory must match what is on disk: a stale
	// write would leave a restart with a different queue than this process has.
	inMemory := c.Pending("parent")
	reloaded, err := NewCourier(target, echoPanes(target), dir)
	require.NoError(t, err)
	assert.Lenf(t, reloaded.Pending("parent"), len(inMemory),
		"the persisted queue disagrees with the live one")
}
