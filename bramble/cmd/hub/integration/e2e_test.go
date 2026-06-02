//go:build integration

// Package integration is the full-chain end-to-end test for the bramble tmux
// control plane: a real isolated tmux server + a scriptable agent stand-in
// (cat), a real hub, and a real remote agent client, driven through the hub's
// browser-facing API exactly as the web UI drives it. No real browser and no
// real LLM, so the test is deterministic and dependency-free.
//
// Chain exercised:
//
//	browser API  ->  hub  ->(WS)  remote agent client  ->  control.Dispatcher
//	     ->  tmuxctl  ->  real tmux pane  ->  capture  ->  PaneDelta  ->  back
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/control"
	"github.com/bazelment/yoloswe/bramble/hub"
	"github.com/bazelment/yoloswe/bramble/remote"
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/tmuxctl"
)

// e2eRegistry maps one synthetic session ID to a real tmux target, so the
// dispatcher's session-centric path resolves to the live pane without standing
// up full bramble Managers. It exercises the same Registry interface the real
// SessionRegistry implements.
type e2eRegistry struct {
	ctl    tmuxctl.Controller
	target string
}

func (r *e2eRegistry) GetAllSessions() []session.SessionInfo {
	return []session.SessionInfo{{
		ID: "e2e", Type: "builder", Status: "running",
		WorktreeName: "e2e-wt", RunnerType: "tmux", TmuxWindowID: r.target,
	}}
}
func (r *e2eRegistry) ResolveTmuxTarget(id session.SessionID) (string, error) {
	if id == "e2e" {
		return r.target, nil
	}
	return "", &control.RemoteError{Message: "not found"}
}
func (r *e2eRegistry) CapturePaneText(_ session.SessionID, n int) ([]string, error) {
	return r.ctl.Capture(context.Background(), r.target, n)
}
func (r *e2eRegistry) StopSession(session.SessionID) error { return nil }

func startTmux(t *testing.T) (socketPath, target string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	socketPath = filepath.Join(t.TempDir(), "tmux.sock")
	out, err := exec.Command("tmux", "-S", socketPath, "new-session", "-d",
		"-P", "-F", "#{window_id}", "cat").Output()
	require.NoError(t, err)
	target = strings.TrimSpace(string(out))
	_ = exec.Command("tmux", "-S", socketPath, "set-option", "-t", target,
		"remain-on-exit", "on").Run()
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socketPath, "kill-server").Run() })
	return socketPath, target
}

func TestEndToEndBrowserToTmux(t *testing.T) {
	t.Parallel()

	const agentToken, browserSecret = "agent-tok", "browser-sec"

	// 1. Real isolated tmux + scriptable agent stand-in (cat echoes its input).
	socketPath, target := startTmux(t)

	// 2. Real hub.
	h := hub.NewHub(agentToken, hub.NewAuthenticator(browserSecret))
	hubSrv := httptest.NewServer(h.Handler())
	t.Cleanup(hubSrv.Close)

	// 3. Real remote agent client, dispatcher backed by the real tmux controller.
	ctl := tmuxctl.NewWithSocketPath(socketPath)
	reg := &e2eRegistry{ctl: ctl, target: target}
	disp := control.NewDispatcher(reg, ctl)
	agentWS := "ws" + strings.TrimPrefix(hubSrv.URL, "http") + "/agent"
	client := remote.New(remote.Config{
		HubURL: agentWS, Token: agentToken, MachineID: "e2e-machine",
		Hostname: "e2e-host", Dispatcher: disp,
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = client.Run(ctx) }()

	// 4. Browser logs in (assert a bad secret is rejected first).
	badJar, _ := cookiejar.New(nil)
	badResp, err := (&http.Client{Jar: badJar}).PostForm(hubSrv.URL+"/login",
		url.Values{"secret": {"nope"}})
	require.NoError(t, err)
	_ = badResp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, badResp.StatusCode)

	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar}
	loginResp, err := cl.PostForm(hubSrv.URL+"/login", url.Values{"secret": {browserSecret}})
	require.NoError(t, err)
	_ = loginResp.Body.Close()
	require.NotEqual(t, http.StatusUnauthorized, loginResp.StatusCode)

	// 5. Wait until the machine is connected, then list machines + sessions.
	var machineID string
	require.Eventually(t, func() bool {
		resp, err := cl.Get(hubSrv.URL + "/api/machines")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var ms []struct {
			ID       string `json:"id"`
			Hostname string `json:"hostname"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&ms)
		if len(ms) == 1 {
			machineID = ms[0].ID
			return true
		}
		return false
	}, 3*time.Second, 25*time.Millisecond, "machine should register with the hub")
	require.Equal(t, "e2e-machine", machineID)

	// 6. Open the browser stream WS (carry the auth cookie) and subscribe.
	header := http.Header{}
	for _, ck := range jar.Cookies(mustURL(t, hubSrv.URL)) {
		header.Add("Cookie", ck.Name+"="+ck.Value)
	}
	streamWS := "ws" + strings.TrimPrefix(hubSrv.URL, "http") + "/api/stream?machine=" + machineID
	ws, _, err := websocket.DefaultDialer.Dial(streamWS, header)
	require.NoError(t, err)
	defer ws.Close()
	conn := control.NewWSConn(ws)

	deltas := make(chan control.PaneDelta, 32)
	go func() {
		for {
			m, err := conn.ReadMsg()
			if err != nil {
				return
			}
			if m.Type == control.TypePaneDelta {
				var d control.PaneDelta
				_ = m.DecodePayload(&d)
				deltas <- d
			}
		}
	}()

	sub, _ := control.NewRequest(control.TypePaneSubscribe, "b-sub",
		control.SubscribeReq{SessionID: "e2e", IntervalMS: 300})
	sub.SubID = "stream-1"
	require.NoError(t, conn.WriteMsg(sub))

	// 7. Send input through the browser API; cat echoes it into the pane.
	sentinel := "E2E-PING-7f3a"
	in, _ := control.NewRequest(control.TypeSessionSendInput, "b-in",
		control.SendInputReq{SessionID: "e2e", Text: sentinel, Submit: true})
	require.NoError(t, conn.WriteMsg(in))

	// 8. Assert the sentinel streams back as a PaneDelta — the full round trip:
	// browser API -> hub -> agent -> tmuxctl -> tmux -> capture -> delta -> browser.
	require.Eventually(t, func() bool {
		for {
			select {
			case d := <-deltas:
				for _, line := range d.Lines {
					if strings.Contains(line, sentinel) {
						return true
					}
				}
			default:
				return false
			}
		}
	}, 5*time.Second, 50*time.Millisecond, "sentinel should round-trip back as a live pane delta")
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	require.NoError(t, err)
	return u
}
