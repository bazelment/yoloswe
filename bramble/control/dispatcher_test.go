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
