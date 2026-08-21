package control

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/tmuxctl"
)

// fakeRegistry is a hand fake of the Registry interface for dispatcher tests.
type fakeRegistry struct {
	targets    map[string]string // sessionID -> tmux target
	resolveErr error
	captureErr error
	stopErr    error
	sessions   []session.SessionInfo
	captured   []string
	stopped    []string
}

func (f *fakeRegistry) GetAllSessions() []session.SessionInfo { return f.sessions }

func (f *fakeRegistry) ResolveTmuxTarget(id session.SessionID) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	t, ok := f.targets[string(id)]
	if !ok {
		return "", fmt.Errorf("session not found: %s", id)
	}
	return t, nil
}

func (f *fakeRegistry) CapturePaneText(id session.SessionID, _ int) ([]string, error) {
	if f.captureErr != nil {
		return nil, f.captureErr
	}
	return f.captured, nil
}

func (f *fakeRegistry) StopSession(id session.SessionID) error {
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, string(id))
	return nil
}

func newDispatcher(reg *fakeRegistry) (*Dispatcher, *tmuxctl.FakeController) {
	ctl := tmuxctl.NewFake()
	return NewDispatcher(reg, ctl), ctl
}

func req(t *testing.T, typ MsgType, payload any) *Msg {
	t.Helper()
	m, err := NewRequest(typ, "rid-1", payload)
	require.NoError(t, err)
	return m
}

func TestSessionSendInputResolvesTargetAndSubmits(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true}))

	var ok OKResult
	require.NoError(t, resp.DecodeResponse(&ok))
	assert.True(t, ok.OK)

	pastes := ctl.CallsFor("Paste")
	require.Len(t, pastes, 1)
	assert.Equal(t, "@7", pastes[0].Target)
	assert.Equal(t, "hello", pastes[0].Text)
	// Submit=true → exactly one Enter.
	enters := ctl.CallsFor("SendSpecial")
	require.Len(t, enters, 1)
	assert.Equal(t, tmuxctl.KeyEnter, enters[0].Special)
}

func TestSessionSendInputNoSubmitSendsNoEnter(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "draft", Submit: false}))

	require.NoError(t, resp.DecodeResponse(nil))
	assert.Len(t, ctl.CallsFor("Paste"), 1)
	assert.Empty(t, ctl.CallsFor("SendSpecial"), "no Enter when Submit is false")
}

func TestSessionSendInputUnknownSessionErrors(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{}}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "ghost", Text: "x", Submit: true}))

	err := resp.DecodeResponse(nil)
	require.Error(t, err)
	var re *RemoteError
	assert.ErrorAs(t, err, &re)
	// Must not touch tmux when resolution fails.
	assert.Empty(t, ctl.Calls)
}

func TestSessionSendInputNonTmuxSessionErrorsCleanly(t *testing.T) {
	t.Parallel()
	// Simulate the runner-type guard rejecting a TUI session.
	reg := &fakeRegistry{resolveErr: fmt.Errorf("session \"s1\" is not a tmux session (runner type: tui)")}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendKey,
		SendKeyReq{SessionID: "s1", Key: tmuxctl.KeyCtrlC}))

	err := resp.DecodeResponse(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a tmux session")
	assert.Empty(t, ctl.Calls)
}

func TestSessionListProjectsSummaries(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{sessions: []session.SessionInfo{
		{ID: "s1", Type: "builder", Status: "running", WorktreeName: "wt", Model: "opus", RunnerType: "tmux", TmuxWindowID: "@3"},
		{ID: "s2", Type: "planner", Status: "idle", RunnerType: "tmux-tracked", TmuxWindowName: "repo/wt:0"},
	}}
	d, _ := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionList, nil))
	var res SessionListResult
	require.NoError(t, resp.DecodeResponse(&res))
	require.Len(t, res.Sessions, 2)
	assert.Equal(t, "@3", res.Sessions[0].TmuxTarget)
	// Falls back to window name when ID is empty.
	assert.Equal(t, "repo/wt:0", res.Sessions[1].TmuxTarget)
}

func TestSessionCaptureUsesRegistryGuard(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{captured: []string{"line1", "line2"}}
	d, _ := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionCapture,
		CaptureReq{SessionID: "s1", Lines: 20}))
	var res CaptureResult
	require.NoError(t, resp.DecodeResponse(&res))
	assert.Equal(t, []string{"line1", "line2"}, res.Lines)
}

func TestPaneSendInputUsesRawTarget(t *testing.T) {
	t.Parallel()
	// Raw-pane path bypasses the registry and writes straight to the target.
	reg := &fakeRegistry{}
	d, ctl := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypePaneSendInput,
		SendInputReq{Target: "%9", Text: "raw", Submit: false}))

	require.NoError(t, resp.DecodeResponse(nil))
	pastes := ctl.CallsFor("Paste")
	require.Len(t, pastes, 1)
	assert.Equal(t, "%9", pastes[0].Target)
}

func TestPaneSendInputMissingTargetErrors(t *testing.T) {
	t.Parallel()
	d, ctl := newDispatcher(&fakeRegistry{})

	resp := d.Handle(context.Background(), req(t, TypePaneSendInput,
		SendInputReq{Text: "x"}))

	require.Error(t, resp.DecodeResponse(nil))
	assert.Empty(t, ctl.Calls)
}

func TestSessionStop(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{}
	d, _ := newDispatcher(reg)

	resp := d.Handle(context.Background(), req(t, TypeSessionStop, SessionRef{SessionID: "s1"}))
	require.NoError(t, resp.DecodeResponse(nil))
	assert.Equal(t, []string{"s1"}, reg.stopped)
}

func TestRawListWindows(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{}
	d, ctl := newDispatcher(reg)
	ctl.Windows = []tmuxctl.TmuxWindow{{ID: "@1", Name: "w"}}

	resp := d.Handle(context.Background(), req(t, TypeTmuxListWindows, TargetRef{Target: ""}))
	var res []tmuxctl.TmuxWindow
	require.NoError(t, resp.DecodeResponse(&res))
	require.Len(t, res, 1)
	assert.Equal(t, "@1", res[0].ID)
}

func TestUnsupportedTypeErrors(t *testing.T) {
	t.Parallel()
	d, _ := newDispatcher(&fakeRegistry{})
	resp := d.Handle(context.Background(), &Msg{Type: MsgType("bogus"), ID: "x"})
	err := resp.DecodeResponse(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported request type")
}

// compile-time: the real registry satisfies the narrow Registry interface.
var _ Registry = (*session.SessionRegistry)(nil)

// fakeCourier records queued sends so the dispatcher's queue branch can be
// tested without a real delivery directory or live sessions.
type fakeCourier struct { //nolint:govet // fieldalignment: readability over packing
	sendErr   error
	sends     []fakeSend
	callCount int
	queued    bool
}

type fakeSend struct {
	from, to session.SessionID
	text     string
	submit   bool
}

func (c *fakeCourier) Send(_ context.Context, from, to session.SessionID, text string, submit bool) (bool, error) {
	c.callCount++
	if c.sendErr != nil {
		return false, c.sendErr
	}
	c.sends = append(c.sends, fakeSend{from: from, to: to, text: text, submit: submit})
	return c.queued, nil
}

// TestSendInputQueueGoesThroughCourier pins that --queue takes the delivery
// path that can wait, rather than pasting into a possibly-running turn.
func TestSendInputQueueGoesThroughCourier(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, ctl := newDispatcher(reg)
	courier := &fakeCourier{queued: true}
	d.SetCourier(courier)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", From: "s0", Text: "hello", Submit: true, Queue: true}))

	var result SendInputResult
	require.NoError(t, resp.DecodeResponse(&result))
	assert.True(t, result.OK)
	assert.True(t, result.Queued)

	require.Len(t, courier.sends, 1)
	assert.Equal(t, session.SessionID("s0"), courier.sends[0].from)
	assert.Equal(t, session.SessionID("s1"), courier.sends[0].to)
	assert.Equal(t, "hello", courier.sends[0].text)
	assert.True(t, courier.sends[0].submit)

	assert.Empty(t, ctl.CallsFor("Paste"), "a queued message must not be typed into the pane")
	assert.Empty(t, ctl.CallsFor("SendSpecial"))
}

// TestSendInputQueueReportsImmediateWrite covers the courier deciding the
// recipient was already idle: the caller is told it was not queued.
func TestSendInputQueueReportsImmediateWrite(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, _ := newDispatcher(reg)
	d.SetCourier(&fakeCourier{queued: false})

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true, Queue: true}))

	var result SendInputResult
	require.NoError(t, resp.DecodeResponse(&result))
	assert.True(t, result.OK)
	assert.False(t, result.Queued)
}

// TestSendInputWithoutQueueBypassesCourier is the compatibility guard: the
// default path must stay exactly the direct paste it has always been.
func TestSendInputWithoutQueueBypassesCourier(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, ctl := newDispatcher(reg)
	courier := &fakeCourier{}
	d.SetCourier(courier)

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true}))

	var result OKResult
	require.NoError(t, resp.DecodeResponse(&result))
	assert.True(t, result.OK)
	assert.Zero(t, courier.callCount, "the unqueued path must not involve the courier")
	pastes := ctl.CallsFor("Paste")
	require.Len(t, pastes, 1)
	assert.Equal(t, "@7", pastes[0].Target)
	assert.Equal(t, "hello", pastes[0].Text)
}

// TestSendInputQueueRequiresSessionID: a raw pane target has no status, so
// there is nothing to wait for. Better to refuse than to paste anyway.
func TestSendInputQueueRequiresSessionID(t *testing.T) {
	t.Parallel()
	d, ctl := newDispatcher(&fakeRegistry{})
	d.SetCourier(&fakeCourier{})

	resp := d.Handle(context.Background(), req(t, TypePaneSendInput,
		SendInputReq{Target: "%9", Text: "hello", Queue: true}))

	err := resp.DecodeResponse(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session_id")
	assert.Empty(t, ctl.CallsFor("Paste"))
}

// TestSendInputQueueWithoutCourierErrors keeps a bramble whose courier failed
// to start from silently downgrading to an interrupting write.
func TestSendInputQueueWithoutCourierErrors(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, ctl := newDispatcher(reg) // no SetCourier

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "hello", Queue: true}))

	err := resp.DecodeResponse(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not available")
	assert.Empty(t, ctl.CallsFor("Paste"), "refusing must not fall back to interrupting the recipient")
}

// TestSendInputQueueSurfacesCourierError makes a rejected recipient (already
// completed, say) reach the caller instead of vanishing.
func TestSendInputQueueSurfacesCourierError(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{targets: map[string]string{"s1": "@7"}}
	d, _ := newDispatcher(reg)
	d.SetCourier(&fakeCourier{sendErr: fmt.Errorf("session s1 is completed and cannot receive messages")})

	resp := d.Handle(context.Background(), req(t, TypeSessionSendInput,
		SendInputReq{SessionID: "s1", Text: "hello", Queue: true}))

	err := resp.DecodeResponse(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot receive messages")
}
