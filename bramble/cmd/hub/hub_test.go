package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
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

func newCookieJar(t *testing.T) *cookiejar.Jar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return jar
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	require.NoError(t, err)
	return u
}

// fakeRegistry implements control.Registry for an in-test agent.
type fakeRegistry struct {
	targets  map[string]string
	sessions []session.SessionInfo
}

func (f *fakeRegistry) GetAllSessions() []session.SessionInfo { return f.sessions }
func (f *fakeRegistry) ResolveTmuxTarget(id session.SessionID) (string, error) {
	if t, ok := f.targets[string(id)]; ok {
		return t, nil
	}
	return "", &control.RemoteError{Message: "not found"}
}
func (f *fakeRegistry) CapturePaneText(session.SessionID, int) ([]string, error) { return nil, nil }
func (f *fakeRegistry) StopSession(session.SessionID) error                      { return nil }

// startTestHub starts an httptest hub and connects an in-process agent to it,
// returning the hub server and the fake controller the agent drives. The agent
// uses control.Serve so the full agent dispatch path is exercised.
func startTestHub(t *testing.T, agentToken, browserSecret string) (*httptest.Server, *tmuxctl.FakeController) {
	t.Helper()
	hub := NewHub(agentToken, newAuthenticator(browserSecret))
	srv := httptest.NewServer(hub.Handler())
	t.Cleanup(srv.Close)

	reg := &fakeRegistry{
		sessions: []session.SessionInfo{
			{ID: "s1", Type: "builder", Status: "running", WorktreeName: "wt", RunnerType: "tmux", TmuxWindowID: "@3"},
		},
		targets: map[string]string{"s1": "@3"},
	}
	ctl := tmuxctl.NewFake()
	ctl.CaptureLines = []string{"agent says hi"}
	disp := control.NewDispatcher(reg, ctl)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	conn := control.NewWSConn(ws)
	hello, _ := control.NewRequest(control.TypeHello, "", control.Hello{
		ProtocolVersion: control.ProtocolVersion,
		MachineID:       "m1", Hostname: "host1", Token: agentToken,
	})
	require.NoError(t, conn.WriteMsg(hello))
	ack, err := conn.ReadMsg()
	require.NoError(t, err)
	var ha control.HelloAck
	require.NoError(t, ack.DecodePayload(&ha))
	require.True(t, ha.OK, "agent handshake should be accepted")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = control.Serve(ctx, conn, disp) }()

	// Wait until the hub has registered the machine.
	require.Eventually(t, func() bool {
		_, ok := hub.reg.get("m1")
		return ok
	}, 2*time.Second, 10*time.Millisecond)
	return srv, ctl
}

// login posts the secret and returns the authenticated cookie jar client.
func login(t *testing.T, srv *httptest.Server, secret string) *http.Client {
	t.Helper()
	jar := newCookieJar(t)
	c := &http.Client{Jar: jar}
	form := url.Values{"secret": {secret}}
	resp, err := c.PostForm(srv.URL+"/login", form)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
	return c
}

func TestUnauthenticatedAPIRejected(t *testing.T) {
	t.Parallel()
	srv, _ := startTestHub(t, "atok", "browser-secret")

	resp, err := http.Get(srv.URL + "/api/machines")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestBadSecretRejected(t *testing.T) {
	t.Parallel()
	srv, _ := startTestHub(t, "atok", "browser-secret")

	resp, err := http.PostForm(srv.URL+"/login", url.Values{"secret": {"wrong"}})
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestListMachinesAuthenticated(t *testing.T) {
	t.Parallel()
	srv, _ := startTestHub(t, "atok", "browser-secret")
	c := login(t, srv, "browser-secret")

	resp, err := c.Get(srv.URL + "/api/machines")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var machines []machineInfo
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&machines))
	require.Len(t, machines, 1)
	assert.Equal(t, "m1", machines[0].ID)
	assert.Equal(t, "host1", machines[0].Hostname)
}

func TestControlForwardSessionList(t *testing.T) {
	t.Parallel()
	srv, _ := startTestHub(t, "atok", "browser-secret")
	c := login(t, srv, "browser-secret")

	msg, _ := control.NewRequest(control.TypeSessionList, "", nil)
	body, _ := json.Marshal(map[string]any{"machine_id": "m1", "msg": msg})
	resp, err := c.Post(srv.URL+"/api/control", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out control.Msg
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	var res control.SessionListResult
	require.NoError(t, out.DecodeResponse(&res))
	require.Len(t, res.Sessions, 1)
	assert.Equal(t, "@3", res.Sessions[0].TmuxTarget)
}

// TestBrowserStreamSendInputAndDelta drives the browser WS path: subscribe to a
// session and send input, asserting the agent's fake controller received the
// paste and that a PaneDelta is pushed back to the browser.
func TestBrowserStreamSendInputAndDelta(t *testing.T) {
	t.Parallel()
	srv, ctl := startTestHub(t, "atok", "browser-secret")
	c := login(t, srv, "browser-secret")

	// Open the browser stream WS carrying the auth cookie.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/stream?machine=m1"
	header := http.Header{}
	for _, ck := range c.Jar.Cookies(mustParseURL(t, srv.URL)) {
		header.Add("Cookie", ck.Name+"="+ck.Value)
	}
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	require.NoError(t, err)
	defer ws.Close()
	conn := control.NewWSConn(ws)

	// Subscribe to s1's pane.
	sub, _ := control.NewRequest(control.TypePaneSubscribe, "b1", control.SubscribeReq{SessionID: "s1", IntervalMS: 250})
	sub.SubID = "sub-1"
	require.NoError(t, conn.WriteMsg(sub))

	// Expect a delta carrying the fake capture output.
	gotDelta := make(chan control.PaneDelta, 4)
	go func() {
		for {
			m, err := conn.ReadMsg()
			if err != nil {
				return
			}
			if m.Type == control.TypePaneDelta {
				var d control.PaneDelta
				_ = m.DecodePayload(&d)
				gotDelta <- d
			}
		}
	}()
	select {
	case d := <-gotDelta:
		assert.Equal(t, []string{"agent says hi"}, d.Lines)
	case <-time.After(2 * time.Second):
		t.Fatal("no pane delta pushed to browser")
	}

	// Send input through the browser stream and confirm it reached the agent.
	in, _ := control.NewRequest(control.TypeSessionSendInput, "b2",
		control.SendInputReq{SessionID: "s1", Text: "do it", Submit: true})
	require.NoError(t, conn.WriteMsg(in))
	require.Eventually(t, func() bool {
		return len(ctl.CallsFor("Paste")) == 1
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "@3", ctl.CallsFor("Paste")[0].Target)
	assert.Equal(t, "do it", ctl.CallsFor("Paste")[0].Text)
}
