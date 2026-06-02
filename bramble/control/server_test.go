package control

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/tmuxctl"
)

// pipeConns returns two control.Conns connected via an in-memory net.Pipe.
func pipeConns() (Conn, Conn) {
	a, b := net.Pipe()
	return NewJSONConn(a), NewJSONConn(b)
}

// TestUnixServerRoundTrip drives a real control.UnixServer over a Unix socket
// (under t.TempDir, no static path) and asserts a send-input request reaches the
// dispatcher and the fake controller. Exercises the local transport end-to-end.
func TestUnixServerRoundTrip(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistry{targets: map[string]string{"s1": "@4"}}
	ctl := tmuxctl.NewFake()
	disp := NewDispatcher(reg, ctl)

	sock := filepath.Join(t.TempDir(), "control.sock")
	srv := NewUnixServer(sock, disp)
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Close() })

	req, err := NewRequest(TypeSessionSendInput, "r1",
		SendInputReq{SessionID: "s1", Text: "hello", Submit: true})
	require.NoError(t, err)

	resp, err := Request(context.Background(), sock, req)
	require.NoError(t, err)
	require.Equal(t, "r1", resp.ID, "response echoes the request ID")

	var ok OKResult
	require.NoError(t, resp.DecodeResponse(&ok))
	assert.True(t, ok.OK)

	pastes := ctl.CallsFor("Paste")
	require.Len(t, pastes, 1)
	assert.Equal(t, "@4", pastes[0].Target)
	assert.Equal(t, "hello", pastes[0].Text)
}

// TestUnixServerErrorRoundTrip verifies a dispatcher error is carried back as a
// RemoteError over the transport.
func TestUnixServerErrorRoundTrip(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistry{targets: map[string]string{}} // s1 not present
	srv := NewUnixServer(filepath.Join(t.TempDir(), "c.sock"), NewDispatcher(reg, tmuxctl.NewFake()))
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Close() })

	req, err := NewRequest(TypeSessionSendKey, "r2",
		SendKeyReq{SessionID: "s1", Key: tmuxctl.KeyCtrlC})
	require.NoError(t, err)

	resp, err := Request(context.Background(), srv.SocketPath(), req)
	require.NoError(t, err)

	derr := resp.DecodeResponse(nil)
	require.Error(t, derr)
	var re *RemoteError
	assert.ErrorAs(t, derr, &re)
}

// TestJSONConnFraming round-trips multiple messages over an in-memory pipe to
// confirm newline framing handles back-to-back messages.
func TestJSONConnFraming(t *testing.T) {
	t.Parallel()

	c1, c2 := pipeConns()
	defer c1.Close()
	defer c2.Close()

	go func() {
		_ = c1.WriteMsg(&Msg{Type: TypeSessionList, ID: "a"})
		_ = c1.WriteMsg(&Msg{Type: TypePaneKill, ID: "b"})
	}()

	m1, err := c2.ReadMsg()
	require.NoError(t, err)
	assert.Equal(t, "a", m1.ID)
	m2, err := c2.ReadMsg()
	require.NoError(t, err)
	assert.Equal(t, "b", m2.ID)
}
