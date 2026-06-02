package hub

import (
	"log/slog"
	"net/http"
	"sync"

	"github.com/bazelment/yoloswe/bramble/control"
)

// handleStream upgrades a browser WebSocket and bridges it to a single machine
// for the duration of the connection. The browser sends control.Msg frames
// (requests and subscribes); the hub forwards them to the machine and pipes the
// machine's responses and PaneDelta frames back to the browser. The target
// machine is selected via the "machine" query parameter.
//
// Subscriptions opened over this socket are tracked and torn down when the
// browser disconnects, so a closed tab does not leak a poll loop on the agent.
func (h *Hub) handleStream(w http.ResponseWriter, r *http.Request) {
	machineID := r.URL.Query().Get("machine")
	m, ok := h.reg.get(machineID)
	if !ok {
		http.Error(w, "unknown machine", http.StatusNotFound)
		return
	}
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn := control.NewWSConn(ws)
	defer conn.Close()

	b := &browserBridge{conn: conn, machine: m, prefix: m.nextID() + ":", subs: make(map[string]struct{})}
	defer b.closeSubs()

	for {
		msg, err := conn.ReadMsg()
		if err != nil {
			return
		}
		b.handle(msg)
	}
}

// browserBridge forwards one browser connection's requests to a machine and
// routes replies/deltas back. WriteMsg on the browser conn is serialized by the
// control.wsConn adapter, so concurrent delta sinks and request replies are safe.
//
// The machine's deltaSub map is shared across all browser connections, so the
// browser's tab-local sub_id is prefixed with a per-connection token before it
// reaches the machine. Two tabs that both pick "s1" thus get distinct
// machine-side keys and cannot clobber or unsubscribe each other's stream; the
// prefix is stripped off delta/error frames so the browser still sees its own id.
type browserBridge struct {
	conn    control.Conn
	machine *machine
	subs    map[string]struct{}
	prefix  string
	mu      sync.Mutex
}

func (b *browserBridge) handle(msg *control.Msg) {
	switch msg.Type {
	case control.TypePaneSubscribe:
		// Snapshot the browser's correlation ID: machine.subscribe forwards msg and
		// rewrites msg.ID to the agent-side request id, so the ack must reply with
		// the saved client id or a client awaiting it never matches.
		clientID := msg.ID
		clientSub := msg.SubID
		if clientSub == "" {
			b.reply(control.NewErr(clientID, "subscribe requires sub_id"))
			return
		}
		hubSub := b.prefix + clientSub
		b.mu.Lock()
		b.subs[hubSub] = struct{}{}
		b.mu.Unlock()
		// Route the agent's pane frames back, restoring the browser's own sub_id.
		err := b.machine.subscribe(hubSub, msg, func(frame *control.Msg) {
			frame.SubID = clientSub
			_ = b.conn.WriteMsg(frame)
		})
		if err != nil {
			b.mu.Lock()
			delete(b.subs, hubSub)
			b.mu.Unlock()
			b.reply(control.NewErr(clientID, err.Error()))
			return
		}
		b.reply(control.NewOK(clientID))
	case control.TypePaneUnsubscribe:
		hubSub := b.prefix + msg.SubID
		b.mu.Lock()
		delete(b.subs, hubSub)
		b.mu.Unlock()
		b.machine.unsubscribe(hubSub)
		b.reply(control.NewOK(msg.ID))
	default:
		// One-shot request/response: forward and pipe the reply back, preserving
		// the browser's correlation ID.
		clientID := msg.ID
		resp, err := b.machine.request(msg)
		if err != nil {
			b.reply(control.NewErr(clientID, err.Error()))
			return
		}
		resp.ID = clientID
		b.reply(resp)
	}
}

func (b *browserBridge) reply(msg *control.Msg) {
	if err := b.conn.WriteMsg(msg); err != nil {
		slog.Debug("hub: browser write failed", "err", err)
	}
}

func (b *browserBridge) closeSubs() {
	b.mu.Lock()
	ids := make([]string, 0, len(b.subs))
	for id := range b.subs {
		ids = append(ids, id)
	}
	b.subs = map[string]struct{}{}
	b.mu.Unlock()
	for _, id := range ids {
		b.machine.unsubscribe(id)
	}
}
