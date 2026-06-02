// Package control is bramble's remote control plane: a versioned JSON message
// protocol and a transport-agnostic dispatcher that drives tmux session control
// over the SessionRegistry (session-centric ops) and a tmuxctl.Controller (raw
// tmux pane ops). The same protocol is carried locally (CLI) and remotely (the
// hub WebSocket); it is purpose-built for bidirectional streaming rather than
// reusing bramble's one-shot ipc envelope.
package control

import (
	"encoding/json"

	"github.com/bazelment/yoloswe/bramble/tmuxctl"
)

// ProtocolVersion is the wire protocol version. Bumped on breaking changes; the
// hub and agent exchange it in the Hello handshake and reject a mismatch.
const ProtocolVersion = 1

// MsgType is the discriminator for a Msg.
type MsgType string

const (
	// Session-centric: address a bramble agent session by SessionID. The
	// dispatcher resolves the SessionID to a tmux target via the registry guard.
	TypeSessionList      MsgType = "session.list"
	TypeSessionStatus    MsgType = "session.status"
	TypeSessionCapture   MsgType = "session.capture"
	TypeSessionSendInput MsgType = "session.send_input"
	TypeSessionSendKey   MsgType = "session.send_key"
	TypeSessionSelect    MsgType = "session.select"
	TypeSessionStop      MsgType = "session.stop"

	// Raw-pane: address a tmux target (window/pane id) directly. Broader surface
	// for power use; still constrained by the tmuxctl allowlist.
	TypeTmuxListSessions MsgType = "tmux.list_sessions"
	TypeTmuxListWindows  MsgType = "tmux.list_windows"
	TypeTmuxListPanes    MsgType = "tmux.list_panes"
	TypePaneCapture      MsgType = "pane.capture"
	TypePaneSendInput    MsgType = "pane.send_input"
	TypePaneSendKey      MsgType = "pane.send_key"
	TypePaneNewWindow    MsgType = "pane.new_window"
	TypePaneKill         MsgType = "pane.kill"

	// Streaming: subscribe to live pane output. The server pushes TypePaneDelta
	// frames (correlated by SubID) until TypePaneUnsubscribe.
	TypePaneSubscribe   MsgType = "pane.subscribe"
	TypePaneUnsubscribe MsgType = "pane.unsubscribe"
	TypePaneDelta       MsgType = "pane.delta"

	// Response is the generic reply to a request (Result or Error set).
	TypeResponse MsgType = "response"
)

// Msg is the single envelope for every request, response, and pushed frame. A
// request carries an ID the response echoes; a subscription carries a SubID the
// pushed deltas echo. Payloads are typed structs marshaled into Payload.
type Msg struct {
	Type    MsgType         `json:"type"`
	ID      string          `json:"id,omitempty"`     // request/response correlation
	SubID   string          `json:"sub_id,omitempty"` // subscription correlation
	Payload json.RawMessage `json:"payload,omitempty"`
}

// --- request payloads --------------------------------------------------------

// SessionRef addresses a bramble agent session.
type SessionRef struct {
	SessionID string `json:"session_id"`
}

// SendInputReq delivers prompt text to a session/pane, optionally submitting it
// with an Enter after the paste.
type SendInputReq struct {
	SessionID string `json:"session_id,omitempty"` // session-centric form
	Target    string `json:"target,omitempty"`     // raw-pane form
	Text      string `json:"text"`
	Submit    bool   `json:"submit"`
}

// SendKeyReq sends a single named special key.
type SendKeyReq struct {
	SessionID string             `json:"session_id,omitempty"`
	Target    string             `json:"target,omitempty"`
	Key       tmuxctl.SpecialKey `json:"key"`
}

// CaptureReq captures recent pane output.
type CaptureReq struct {
	SessionID string `json:"session_id,omitempty"`
	Target    string `json:"target,omitempty"`
	Lines     int    `json:"lines,omitempty"`
}

// TargetRef addresses a raw tmux target.
type TargetRef struct {
	Target string `json:"target"`
}

// NewWindowReq creates a tmux window.
type NewWindowReq struct {
	Name string `json:"name,omitempty"`
	CWD  string `json:"cwd,omitempty"`
	Cmd  string `json:"cmd,omitempty"`
}

// SubscribeReq starts a live pane subscription. IntervalMS bounds how often the
// server samples the pane (clamped server-side to a sane floor).
type SubscribeReq struct {
	SessionID  string `json:"session_id,omitempty"`
	Target     string `json:"target,omitempty"`
	IntervalMS int    `json:"interval_ms,omitempty"`
}

// --- response / push payloads ------------------------------------------------

// SessionListResult lists bramble agent sessions.
type SessionListResult struct {
	Sessions []SessionSummary `json:"sessions"`
}

// SessionSummary is a brief session snapshot for the control UI.
type SessionSummary struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	WorktreeName string `json:"worktree_name"`
	Model        string `json:"model"`
	RunnerType   string `json:"runner_type"`
	TmuxTarget   string `json:"tmux_target"`
}

// CaptureResult holds captured pane lines.
type CaptureResult struct {
	Lines []string `json:"lines"`
}

// NewWindowResult returns the created window's stable ID.
type NewWindowResult struct {
	WindowID string `json:"window_id"`
}

// PaneDelta is one pushed live-output frame for a subscription. Lines is the
// current content snapshot (ANSI-stripped); Status is the parsed agent status
// when available.
type PaneDelta struct {
	Status *PaneStatusJSON `json:"status,omitempty"`
	Lines  []string        `json:"lines"`
}

// PaneStatusJSON is the JSON-friendly projection of session.PaneStatus.
type PaneStatusJSON struct {
	Model       string `json:"model,omitempty"`
	ContextPct  string `json:"context_pct,omitempty"`
	TokenCount  string `json:"token_count,omitempty"`
	Branch      string `json:"branch,omitempty"`
	StatusLine  string `json:"status_line,omitempty"`
	Permissions string `json:"permissions,omitempty"`
	IsIdle      bool   `json:"is_idle"`
	IsWorking   bool   `json:"is_working"`
}

// OKResult is a minimal success payload for ops with no data to return.
type OKResult struct {
	OK bool `json:"ok"`
}

// Response is the payload of a TypeResponse Msg. Exactly one of Result/Error is
// meaningful: Error non-empty means the request failed; otherwise Result holds
// the marshaled typed result.
type Response struct {
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Hello is the first frame an agent sends to the hub on connect. The hub
// authenticates Token, records the machine, and rejects a ProtocolVersion
// mismatch. It is sent as a TypeHello Msg payload, not over the Msg envelope's
// request/response fields, because it precedes normal dispatch.
type Hello struct {
	MachineID       string `json:"machine_id"`
	Hostname        string `json:"hostname"`
	Token           string `json:"token"`
	ProtocolVersion int    `json:"protocol_version"`
}

// HelloAck is the hub's reply to a Hello. OK=false with Error set means the hub
// rejected the connection (bad token, version mismatch).
type HelloAck struct {
	Error string `json:"error,omitempty"`
	OK    bool   `json:"ok"`
}

const (
	// TypeHello and TypeHelloAck carry the connection handshake.
	TypeHello    MsgType = "hello"
	TypeHelloAck MsgType = "hello_ack"
)

// --- helpers -----------------------------------------------------------------

// NewRequest builds a request Msg of the given type with a JSON-marshaled
// payload and correlation ID.
func NewRequest(t MsgType, id string, payload any) (*Msg, error) {
	m := &Msg{Type: t, ID: id}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		m.Payload = raw
	}
	return m, nil
}

// okResponse builds a successful TypeResponse Msg wrapping the marshaled result.
func okResponse(id string, result any) *Msg {
	resp := Response{}
	if result != nil {
		if raw, err := json.Marshal(result); err == nil {
			resp.Result = raw
		} else {
			return errResponse(id, err)
		}
	}
	payload, _ := json.Marshal(resp)
	return &Msg{Type: TypeResponse, ID: id, Payload: payload}
}

// errResponse builds a failed TypeResponse Msg carrying the error string.
func errResponse(id string, err error) *Msg {
	payload, _ := json.Marshal(Response{Error: err.Error()})
	return &Msg{Type: TypeResponse, ID: id, Payload: payload}
}

// NewErr builds a failed TypeResponse Msg with the given error message. Used by
// relays (the hub) that synthesize responses without a Go error value.
func NewErr(id, message string) *Msg {
	payload, _ := json.Marshal(Response{Error: message})
	return &Msg{Type: TypeResponse, ID: id, Payload: payload}
}

// NewOK builds a successful TypeResponse Msg with an OKResult payload.
func NewOK(id string) *Msg {
	return okResponse(id, OKResult{OK: true})
}

// decode unmarshals a Msg payload into v.
func (m *Msg) decode(v any) error {
	if len(m.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(m.Payload, v)
}

// DecodePayload unmarshals a Msg's raw payload into v. Used for non-Response
// frames (e.g. the Hello/HelloAck handshake) where there is no Result wrapper.
func (m *Msg) DecodePayload(v any) error { return m.decode(v) }

// DecodeResponse extracts the Response from a TypeResponse Msg, returning the
// error if Error is set, otherwise unmarshaling Result into v (v may be nil).
func (m *Msg) DecodeResponse(v any) error {
	var resp Response
	if err := m.decode(&resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return &RemoteError{Message: resp.Error}
	}
	if v != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, v)
	}
	return nil
}

// RemoteError is returned by DecodeResponse when the peer reported a failure.
type RemoteError struct{ Message string }

func (e *RemoteError) Error() string { return e.Message }
