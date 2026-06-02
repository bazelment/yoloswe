package hub

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/bazelment/yoloswe/bramble/control"
)

// Hub relays between browsers and connected agent machines. It holds no tmux
// logic: agents execute tmux behind the tmuxctl allowlist; the hub authenticates
// and forwards.
type Hub struct {
	reg        *registry
	auth       *Authenticator
	agentToken string // shared token agents present in their Hello
	upgrader   websocket.Upgrader
}

// NewHub constructs a hub. agentToken authenticates agents; auth handles the
// browser side (login → session cookie).
func NewHub(agentToken string, auth *Authenticator) *Hub {
	return &Hub{
		reg:        newRegistry(),
		auth:       auth,
		agentToken: agentToken,
		upgrader:   websocket.Upgrader{},
	}
}

// Handler returns the hub's HTTP routes.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	// Agent endpoint: dev machines dial in here.
	mux.HandleFunc("/agent", h.handleAgent)
	// Browser API (cookie-authenticated).
	mux.HandleFunc("/login", h.auth.handleLogin)
	mux.HandleFunc("/api/machines", h.requireAuth(h.handleMachines))
	mux.HandleFunc("/api/control", h.requireAuth(h.handleControl))
	mux.HandleFunc("/api/stream", h.requireAuth(h.handleStream))
	// Web UI.
	mux.HandleFunc("/", h.requireAuthPage(h.handleIndex))
	return mux
}

// handleAgent upgrades an agent connection, runs the Hello handshake, registers
// the machine, and pumps its read loop until it disconnects.
func (h *Hub) handleAgent(w http.ResponseWriter, r *http.Request) {
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn := control.NewWSConn(ws)

	helloMsg, err := conn.ReadMsg()
	if err != nil || helloMsg.Type != control.TypeHello {
		_ = conn.Close()
		return
	}
	var hello control.Hello
	if err := helloMsg.DecodePayload(&hello); err != nil {
		_ = conn.Close()
		return
	}

	ack := control.HelloAck{OK: true}
	switch {
	case hello.ProtocolVersion != control.ProtocolVersion:
		ack = control.HelloAck{OK: false, Error: "protocol version mismatch"}
	case !h.agentTokenOK(hello.Token):
		// Fail closed: an empty configured token rejects every agent rather than
		// admitting any unauthenticated client.
		ack = control.HelloAck{OK: false, Error: "bad token"}
	case hello.MachineID == "":
		ack = control.HelloAck{OK: false, Error: "missing machine_id"}
	}
	ackMsg, _ := control.NewRequest(control.TypeHelloAck, "", ack)
	_ = conn.WriteMsg(ackMsg)
	if !ack.OK {
		slog.Warn("hub: rejected agent", "machine", hello.MachineID, "reason", ack.Error)
		_ = conn.Close()
		return
	}

	m := newMachine(hello.MachineID, hello.Hostname, conn)
	h.reg.add(m)
	slog.Info("hub: agent connected", "machine", m.id, "hostname", m.hostname)
	m.readLoop() // blocks until disconnect
	h.reg.remove(m.id, m)
	slog.Info("hub: agent disconnected", "machine", m.id)
}

// agentTokenOK reports whether a presented agent token matches the configured
// one, using a constant-time compare. A hub with no configured token rejects
// all agents (fail closed) — agent auth is never silently optional.
func (h *Hub) agentTokenOK(presented string) bool {
	if h.agentToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(h.agentToken)) == 1
}

// handleMachines lists connected machines.
func (h *Hub) handleMachines(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.reg.list())
}

// handleControl forwards a one-shot control request to a machine and returns
// the agent's response verbatim. Body: {"machine_id": "...", "msg": {control.Msg}}.
func (h *Hub) handleControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MachineID string          `json:"machine_id"`
		Msg       json.RawMessage `json:"msg"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err))
		return
	}
	m, ok := h.reg.get(body.MachineID)
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody(errUnknownMachine))
		return
	}
	var req control.Msg
	if err := json.Unmarshal(body.Msg, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err))
		return
	}
	resp, err := m.request(&req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody(err))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errBody(err error) map[string]string { return map[string]string{"error": err.Error()} }
