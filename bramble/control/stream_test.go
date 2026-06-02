package control

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/tmuxctl"
)

// mutableController is a tmuxctl.Controller whose Capture output can change
// between polls, to drive the streamer's change-detection. It embeds a pointer
// to a FakeController for the unused methods and overrides Capture.
type mutableController struct {
	*tmuxctl.FakeController
	status *session.PaneStatus
	lines  []string
	mu     sync.Mutex
}

func newMutableController() *mutableController {
	return &mutableController{FakeController: tmuxctl.NewFake()}
}

func (m *mutableController) setLines(l []string) {
	m.mu.Lock()
	m.lines = l
	m.mu.Unlock()
}

func (m *mutableController) setStatus(s *session.PaneStatus) {
	m.mu.Lock()
	m.status = s
	m.mu.Unlock()
}

func (m *mutableController) Capture(_ context.Context, _ string, _ int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.lines...), nil
}

func (m *mutableController) Status(_ context.Context, _ string) (*session.PaneStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status, nil
}

// Status uses the embedded FakeController's implementation (returns nil), which
// is fine: the streamer's change detection keys off content when status is nil.

func TestStreamSubscribeEmitsAndDedups(t *testing.T) {
	t.Parallel()

	ctl := newMutableController()
	ctl.setLines([]string{"first"})
	reg := &fakeRegistry{targets: map[string]string{"s1": "@1"}}
	disp := NewDispatcher(reg, ctl)

	agent, client := pipeConns()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, agent, disp) }()

	// Collect frames the server pushes to the client.
	deltas := make(chan PaneDelta, 16)
	go func() {
		for {
			msg, err := client.ReadMsg()
			if err != nil {
				return
			}
			if msg.Type == TypePaneDelta {
				var d PaneDelta
				_ = msg.DecodePayload(&d)
				deltas <- d
			}
		}
	}()

	sub, err := NewRequest(TypePaneSubscribe, "r1", SubscribeReq{SessionID: "s1", IntervalMS: 250})
	require.NoError(t, err)
	sub.SubID = "sub-1"
	require.NoError(t, client.WriteMsg(sub))

	// First frame is the immediate snapshot.
	select {
	case d := <-deltas:
		assert.Equal(t, []string{"first"}, d.Lines)
	case <-time.After(2 * time.Second):
		t.Fatal("no initial delta")
	}

	// Unchanged content must NOT produce another frame within a couple intervals.
	select {
	case d := <-deltas:
		t.Fatalf("unexpected delta for unchanged pane: %+v", d)
	case <-time.After(700 * time.Millisecond):
	}

	// Change the content; the next poll should push a new frame.
	ctl.setLines([]string{"first", "second"})
	select {
	case d := <-deltas:
		assert.Equal(t, []string{"first", "second"}, d.Lines)
	case <-time.After(2 * time.Second):
		t.Fatal("no delta after content change")
	}

	// Unsubscribe; no further frames even after content changes.
	unsub := &Msg{Type: TypePaneUnsubscribe, ID: "r2", SubID: "sub-1"}
	require.NoError(t, client.WriteMsg(unsub))
	// Drain the unsubscribe ack reader path by giving it a moment, then change.
	time.Sleep(50 * time.Millisecond)
	ctl.setLines([]string{"third"})
	select {
	case d := <-deltas:
		t.Fatalf("unexpected delta after unsubscribe: %+v", d)
	case <-time.After(700 * time.Millisecond):
	}
}

// TestStreamEmitsOnModelOnlyChange pins that a status change with identical pane
// text (only Model differs) still produces a fresh delta — the change-detection
// key must hash every rendered status field, not a hand-picked subset.
func TestStreamEmitsOnModelOnlyChange(t *testing.T) {
	t.Parallel()

	ctl := newMutableController()
	ctl.setLines([]string{"steady"})
	ctl.setStatus(&session.PaneStatus{Model: "opus"})
	reg := &fakeRegistry{targets: map[string]string{"s1": "@1"}}
	disp := NewDispatcher(reg, ctl)

	agent, client := pipeConns()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, agent, disp) }()

	deltas := make(chan PaneDelta, 16)
	go func() {
		for {
			msg, err := client.ReadMsg()
			if err != nil {
				return
			}
			if msg.Type == TypePaneDelta {
				var d PaneDelta
				_ = msg.DecodePayload(&d)
				deltas <- d
			}
		}
	}()

	sub, err := NewRequest(TypePaneSubscribe, "r1", SubscribeReq{SessionID: "s1", IntervalMS: 250})
	require.NoError(t, err)
	sub.SubID = "sub-1"
	require.NoError(t, client.WriteMsg(sub))

	select {
	case d := <-deltas:
		require.NotNil(t, d.Status)
		assert.Equal(t, "opus", d.Status.Model)
	case <-time.After(2 * time.Second):
		t.Fatal("no initial delta")
	}

	// Only the model changes — same pane text. A fresh delta must still arrive.
	ctl.setStatus(&session.PaneStatus{Model: "sonnet"})
	select {
	case d := <-deltas:
		require.NotNil(t, d.Status)
		assert.Equal(t, "sonnet", d.Status.Model, "model-only change must not be deduped")
	case <-time.After(2 * time.Second):
		t.Fatal("model-only status change was deduped away")
	}
}

// failingController always fails Capture, to drive the terminal-error path.
type failingController struct {
	*tmuxctl.FakeController
}

func (failingController) Capture(_ context.Context, _ string, _ int) ([]string, error) {
	return nil, assert.AnError
}

// TestStreamEmitsTerminalErrorFrame verifies that sustained capture failures
// end the subscription with a TypePaneError frame (SubID-correlated) rather than
// the poll loop going silently dark.
func TestStreamEmitsTerminalErrorFrame(t *testing.T) {
	t.Parallel()

	ctl := failingController{FakeController: tmuxctl.NewFake()}
	reg := &fakeRegistry{targets: map[string]string{"s1": "@1"}}
	disp := NewDispatcher(reg, ctl)

	agent, client := pipeConns()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, agent, disp) }()

	errFrames := make(chan *Msg, 4)
	go func() {
		for {
			msg, err := client.ReadMsg()
			if err != nil {
				return
			}
			if msg.Type == TypePaneError {
				errFrames <- msg
			}
		}
	}()

	sub, err := NewRequest(TypePaneSubscribe, "r1", SubscribeReq{SessionID: "s1", IntervalMS: 250})
	require.NoError(t, err)
	sub.SubID = "sub-1"
	require.NoError(t, client.WriteMsg(sub))

	select {
	case msg := <-errFrames:
		assert.Equal(t, "sub-1", msg.SubID, "terminal error must carry the sub id")
		var pe PaneError
		require.NoError(t, msg.DecodePayload(&pe))
		assert.NotEmpty(t, pe.Error)
	case <-time.After(3 * time.Second):
		t.Fatal("no terminal error frame after sustained capture failures")
	}
}

func TestStreamSubscribeRequiresSubID(t *testing.T) {
	t.Parallel()

	disp := NewDispatcher(&fakeRegistry{targets: map[string]string{"s1": "@1"}}, tmuxctl.NewFake())
	agent, client := pipeConns()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, agent, disp) }()

	sub, err := NewRequest(TypePaneSubscribe, "r1", SubscribeReq{SessionID: "s1"})
	require.NoError(t, err)
	// No SubID set.
	require.NoError(t, client.WriteMsg(sub))

	resp, err := client.ReadMsg()
	require.NoError(t, err)
	derr := resp.DecodeResponse(nil)
	require.Error(t, derr)
	assert.Contains(t, derr.Error(), "sub_id")
}
