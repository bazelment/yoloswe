package main

import (
	"fmt"
	"sync"

	"github.com/bazelment/yoloswe/bramble/control"
)

// machine is a connected agent: it owns the agent's control connection, runs a
// read loop that demultiplexes responses (by request ID) and pushed PaneDelta
// frames (by subscription ID), and exposes a request/response + delta-fanout
// API to the hub's browser-facing handlers.
type machine struct {
	conn     control.Conn
	pending  map[string]chan *control.Msg  // request ID -> reply channel
	deltaSub map[string]func(*control.Msg) // subscription ID -> delta sink
	id       string
	hostname string
	seq      int
	mu       sync.Mutex
	closed   bool
}

func newMachine(id, hostname string, conn control.Conn) *machine {
	return &machine{
		id:       id,
		hostname: hostname,
		conn:     conn,
		pending:  make(map[string]chan *control.Msg),
		deltaSub: make(map[string]func(*control.Msg)),
	}
}

// readLoop demultiplexes inbound frames until the connection closes. Responses
// are delivered to the waiting request; PaneDelta frames are fanned out to the
// registered sink for their SubID.
func (m *machine) readLoop() {
	for {
		msg, err := m.conn.ReadMsg()
		if err != nil {
			m.shutdown()
			return
		}
		switch msg.Type {
		case control.TypePaneDelta:
			m.mu.Lock()
			sink := m.deltaSub[msg.SubID]
			m.mu.Unlock()
			if sink != nil {
				sink(msg)
			}
		default: // TypeResponse and any other correlated reply
			m.mu.Lock()
			ch := m.pending[msg.ID]
			delete(m.pending, msg.ID)
			m.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		}
	}
}

// nextID allocates a unique request ID for this machine connection.
func (m *machine) nextID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	return fmt.Sprintf("h%d", m.seq)
}

// request forwards a control request to the agent and waits for the response,
// assigning a fresh correlation ID. The provided req's payload/type are used.
func (m *machine) request(req *control.Msg) (*control.Msg, error) {
	id := m.nextID()
	req.ID = id
	ch := make(chan *control.Msg, 1)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine %s disconnected", m.id)
	}
	m.pending[id] = ch
	m.mu.Unlock()

	if err := m.conn.WriteMsg(req); err != nil {
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
		return nil, err
	}
	return <-ch, nil
}

// subscribe forwards a subscribe request and routes future PaneDelta frames for
// subID to sink until unsubscribe is called.
func (m *machine) subscribe(subID string, req *control.Msg, sink func(*control.Msg)) error {
	req.SubID = subID
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("machine %s disconnected", m.id)
	}
	m.deltaSub[subID] = sink
	m.mu.Unlock()

	if _, err := m.request(req); err != nil {
		m.mu.Lock()
		delete(m.deltaSub, subID)
		m.mu.Unlock()
		return err
	}
	return nil
}

// unsubscribe forwards an unsubscribe request and stops routing deltas for subID.
func (m *machine) unsubscribe(subID string) {
	m.mu.Lock()
	delete(m.deltaSub, subID)
	m.mu.Unlock()
	unsub := &control.Msg{Type: control.TypePaneUnsubscribe, SubID: subID}
	_, _ = m.request(unsub)
}

// shutdown marks the machine closed and fails all in-flight requests so callers
// don't block forever when the agent disconnects.
func (m *machine) shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	for id, ch := range m.pending {
		close(ch)
		delete(m.pending, id)
	}
	m.deltaSub = map[string]func(*control.Msg){}
	_ = m.conn.Close()
}

// registry tracks connected machines by ID.
type registry struct {
	machines map[string]*machine
	mu       sync.RWMutex
}

func newRegistry() *registry {
	return &registry{machines: make(map[string]*machine)}
}

func (r *registry) add(m *machine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Replace any stale connection for the same machine id.
	if old, ok := r.machines[m.id]; ok {
		old.shutdown()
	}
	r.machines[m.id] = m
}

func (r *registry) remove(id string, m *machine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Only remove if it's still the same connection (avoid removing a fresh
	// reconnect that replaced this one).
	if cur, ok := r.machines[id]; ok && cur == m {
		delete(r.machines, id)
	}
}

func (r *registry) get(id string) (*machine, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.machines[id]
	return m, ok
}

// list returns the connected machines as id/hostname pairs.
func (r *registry) list() []machineInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]machineInfo, 0, len(r.machines))
	for _, m := range r.machines {
		out = append(out, machineInfo{ID: m.id, Hostname: m.hostname})
	}
	return out
}

type machineInfo struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
}
