package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/control"
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/tmuxctl"
)

// fakeRegistry is a minimal control.Registry for wiring the client's dispatcher.
type fakeRegistry struct {
	targets map[string]string
}

func (f *fakeRegistry) GetAllSessions() []session.SessionInfo { return nil }
func (f *fakeRegistry) ResolveTmuxTarget(id session.SessionID) (string, error) {
	if t, ok := f.targets[string(id)]; ok {
		return t, nil
	}
	return "", assertNotFound(id)
}
func (f *fakeRegistry) CapturePaneText(session.SessionID, int) ([]string, error) { return nil, nil }
func (f *fakeRegistry) StopSession(session.SessionID) error                      { return nil }

func assertNotFound(id session.SessionID) error {
	return &control.RemoteError{Message: "not found: " + string(id)}
}

// fakeHub is an httptest WebSocket server standing in for the cloud hub. It
// accepts an agent connection, runs the Hello handshake per its policy, and
// optionally forwards a single request to the agent and captures the response.
type fakeHub struct {
	server      *httptest.Server
	gotHello    chan control.Hello
	forward     *control.Msg // request to forward after handshake (optional)
	forwardResp chan *control.Msg
	wsURL       string
	wantToken   string
}

func newFakeHub(t *testing.T, wantToken string) *fakeHub {
	t.Helper()
	h := &fakeHub{
		wantToken:   wantToken,
		gotHello:    make(chan control.Hello, 1),
		forwardResp: make(chan *control.Msg, 1),
	}
	up := websocket.Upgrader{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		conn := control.NewWSConn(ws)

		helloMsg, err := conn.ReadMsg()
		if err != nil || helloMsg.Type != control.TypeHello {
			return
		}
		var hello control.Hello
		_ = helloMsg.DecodePayload(&hello)
		h.gotHello <- hello

		ack := control.HelloAck{OK: hello.Token == h.wantToken}
		if !ack.OK {
			ack.Error = "bad token"
		}
		ackMsg, _ := control.NewRequest(control.TypeHelloAck, "", ack)
		_ = conn.WriteMsg(ackMsg)
		if !ack.OK {
			return
		}

		if h.forward != nil {
			if err := conn.WriteMsg(h.forward); err != nil {
				return
			}
			resp, err := conn.ReadMsg()
			if err != nil {
				return
			}
			h.forwardResp <- resp
		}
		// Keep the connection open briefly so the agent's Serve loop stays up.
		_, _, _ = ws.ReadMessage()
	}))
	h.wsURL = "ws" + strings.TrimPrefix(h.server.URL, "http") + "/agent"
	t.Cleanup(h.server.Close)
	return h
}

func newClient(hub *fakeHub, token string, disp *control.Dispatcher) *Client {
	return New(Config{
		HubURL:     hub.wsURL,
		Token:      token,
		MachineID:  "machine-1",
		Hostname:   "testhost",
		Dispatcher: disp,
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 20 * time.Millisecond,
	})
}

func TestHandshakeSuccessAndForwardedRequest(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistry{targets: map[string]string{"s1": "@8"}}
	ctl := tmuxctl.NewFake()
	disp := control.NewDispatcher(reg, ctl)

	hub := newFakeHub(t, "secret")
	// The hub forwards a send-input request after the handshake.
	fwd, err := control.NewRequest(control.TypeSessionSendInput, "fwd-1",
		control.SendInputReq{SessionID: "s1", Text: "hi", Submit: true})
	require.NoError(t, err)
	hub.forward = fwd

	client := newClient(hub, "secret", disp)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	// Handshake happened with the right identity.
	select {
	case hello := <-hub.gotHello:
		assert.Equal(t, "machine-1", hello.MachineID)
		assert.Equal(t, "secret", hello.Token)
		assert.Equal(t, control.ProtocolVersion, hello.ProtocolVersion)
	case <-time.After(2 * time.Second):
		t.Fatal("no hello received")
	}

	// Forwarded request reached the agent's dispatcher and round-tripped back.
	select {
	case resp := <-hub.forwardResp:
		require.Equal(t, "fwd-1", resp.ID)
		require.NoError(t, resp.DecodeResponse(nil))
	case <-time.After(2 * time.Second):
		t.Fatal("no forwarded response")
	}

	// The dispatcher actually drove the (fake) controller.
	require.Eventually(t, func() bool {
		return len(ctl.CallsFor("Paste")) == 1 && len(ctl.CallsFor("SendSpecial")) == 1
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "@8", ctl.CallsFor("Paste")[0].Target)
}

func TestHandshakeRejectedOnBadToken(t *testing.T) {
	t.Parallel()

	disp := control.NewDispatcher(&fakeRegistry{}, tmuxctl.NewFake())
	hub := newFakeHub(t, "right-token")
	client := newClient(hub, "wrong-token", disp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	// Hub sees the hello but rejects it; the client must not proceed to serve.
	select {
	case hello := <-hub.gotHello:
		assert.Equal(t, "wrong-token", hello.Token)
	case <-time.After(2 * time.Second):
		t.Fatal("no hello received")
	}
	// No forward configured; a rejected client simply backs off and retries,
	// which the test tears down via cancel. Reaching here without hang is the
	// assertion (reject path doesn't deadlock the client).
}

// connectAndServe returns a connection-level error on a bad token; assert that
// directly for a deterministic check of the reject path.
func TestConnectAndServeReturnsErrorOnReject(t *testing.T) {
	t.Parallel()

	disp := control.NewDispatcher(&fakeRegistry{}, tmuxctl.NewFake())
	hub := newFakeHub(t, "right")
	client := newClient(hub, "wrong", disp)

	err := client.connectAndServe(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rejected")
}
