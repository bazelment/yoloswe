package hub

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/control"
)

// blockingConn is a control.Conn whose WriteMsg succeeds but whose ReadMsg
// blocks forever (no reply ever arrives). It lets a test drive machine.request
// to the point where it waits on the pending channel, so shutdown's channel
// close can be observed.
type blockingConn struct {
	released chan struct{}
}

func newBlockingConn() *blockingConn { return &blockingConn{released: make(chan struct{})} }

func (c *blockingConn) WriteMsg(*control.Msg) error { return nil }
func (c *blockingConn) ReadMsg() (*control.Msg, error) {
	<-c.released
	return nil, assert.AnError
}
func (c *blockingConn) Close() error {
	select {
	case <-c.released:
	default:
		close(c.released)
	}
	return nil
}

// TestMachineRequestDisconnectReturnsError verifies that when the agent
// disconnects while a request is in flight, request returns a non-nil error and
// a nil message — rather than (nil, nil), which a caller would dereference.
func TestMachineRequestDisconnectReturnsError(t *testing.T) {
	t.Parallel()

	m := newMachine("agent-1", "host", newBlockingConn())

	type result struct {
		msg *control.Msg
		err error
	}
	done := make(chan result, 1)
	go func() {
		msg, err := m.request(&control.Msg{Type: control.TypeSessionList})
		done <- result{msg, err}
	}()

	// Once the request has registered its pending channel, simulate a disconnect.
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.pending) == 1
	}, 2*time.Second, time.Millisecond, "request should register a pending channel")

	m.shutdown()

	res := <-done
	assert.Nil(t, res.msg, "no message should be returned on disconnect")
	require.Error(t, res.err, "disconnect must surface as an error, not (nil, nil)")
}

// TestMachineRequestTimesOut verifies a hung agent (reply never arrives) causes
// request to return a timeout error rather than blocking forever.
func TestMachineRequestTimesOut(t *testing.T) {
	t.Parallel()

	orig := requestTimeout
	requestTimeout = 50 * time.Millisecond
	t.Cleanup(func() { requestTimeout = orig })

	conn := newBlockingConn()
	t.Cleanup(func() { _ = conn.Close() })
	m := newMachine("agent-1", "host", conn)

	msg, err := m.request(&control.Msg{Type: control.TypeSessionList})
	assert.Nil(t, msg, "no message on timeout")
	require.Error(t, err, "hung agent must surface a timeout error")

	// The timed-out request must not leak its pending entry.
	m.mu.Lock()
	n := len(m.pending)
	m.mu.Unlock()
	assert.Equal(t, 0, n, "timed-out request should clear its pending channel")
}
