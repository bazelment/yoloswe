package control

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/bramble/session"
)

const (
	// defaultStreamInterval is how often a subscription samples a pane when the
	// client does not specify one.
	defaultStreamInterval = 1500 * time.Millisecond
	// minStreamInterval floors the client-requested interval so a misbehaving or
	// hostile client cannot ask the agent to hammer tmux.
	minStreamInterval = 250 * time.Millisecond
)

// streamer manages live pane subscriptions for a single connection. Each
// subscription runs a poll loop that captures the pane and pushes PaneDelta
// frames over the connection, deduplicating unchanged snapshots so an idle pane
// produces no traffic. All subscriptions are cancelled when the connection's
// serve loop exits.
type streamer struct {
	conn Conn
	disp *Dispatcher
	subs map[string]context.CancelFunc // subID -> cancel
	mu   sync.Mutex
}

func newStreamer(conn Conn, disp *Dispatcher) *streamer {
	return &streamer{conn: conn, disp: disp, subs: make(map[string]context.CancelFunc)}
}

// subscribe starts a poll loop for req under subID. A subID already in use is
// first unsubscribed so re-subscribing is idempotent.
func (s *streamer) subscribe(ctx context.Context, subID string, req SubscribeReq) error {
	target, err := s.disp.targetFor(req.SessionID, req.Target, req.SessionID != "")
	if err != nil {
		return err
	}

	interval := time.Duration(req.IntervalMS) * time.Millisecond
	if req.IntervalMS == 0 {
		interval = defaultStreamInterval
	}
	if interval < minStreamInterval {
		interval = minStreamInterval
	}

	s.mu.Lock()
	if cancel, ok := s.subs[subID]; ok {
		cancel() // replace an existing sub with the same id
	}
	subCtx, cancel := context.WithCancel(ctx)
	s.subs[subID] = cancel
	s.mu.Unlock()

	go s.poll(subCtx, subID, target, interval)
	return nil
}

// unsubscribe stops the poll loop for subID (no-op if unknown).
func (s *streamer) unsubscribe(subID string) {
	s.mu.Lock()
	if cancel, ok := s.subs[subID]; ok {
		cancel()
		delete(s.subs, subID)
	}
	s.mu.Unlock()
}

// closeAll cancels every subscription. Called when the serve loop exits.
func (s *streamer) closeAll() {
	s.mu.Lock()
	for id, cancel := range s.subs {
		cancel()
		delete(s.subs, id)
	}
	s.mu.Unlock()
}

// poll captures the pane on a ticker and pushes a PaneDelta whenever the content
// or status changes. The first sample is always sent so the client gets an
// immediate snapshot.
func (s *streamer) poll(ctx context.Context, subID, target string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var (
		lastKey uint64
		sent    bool
	)
	// emit captures once and pushes a delta on change. It returns the capture
	// error (if any) so poll can surface a persistently broken pane instead of
	// going silently dark.
	emit := func() error {
		lines, err := s.disp.ctl.Capture(ctx, target, paneStreamLines)
		if err != nil {
			return err
		}
		ps, _ := s.disp.ctl.Status(ctx, target)
		key := streamKey(lines, ps)
		if sent && key == lastKey {
			return nil // unchanged — suppress to keep an idle pane silent
		}
		lastKey, sent = key, true
		delta := PaneDelta{Lines: lines, Status: toStatusJSON(ps)}
		msg, err := NewRequest(TypePaneDelta, "", delta)
		if err != nil {
			return nil // malformed frame — drop this tick, not a pane error
		}
		msg.SubID = subID
		_ = s.conn.WriteMsg(msg)
		return nil
	}

	consecErrs := 0
	tick := func() bool {
		if err := emit(); err != nil {
			consecErrs++
			// Tolerate transient capture blips, but a sustained failure (e.g. the
			// pane was killed) is reported as a terminal error frame so the client
			// learns the stream is dead rather than just stops receiving deltas.
			if consecErrs >= maxStreamCaptureErrs {
				// Terminal error rides the delta path (SubID-correlated) so the hub
				// routes it to the subscriber rather than the request-reply map.
				if msg, mkErr := NewRequest(TypePaneError, "", PaneError{Error: err.Error()}); mkErr == nil {
					msg.SubID = subID
					_ = s.conn.WriteMsg(msg)
				}
				return false
			}
			return true
		}
		consecErrs = 0
		return true
	}

	tick() // immediate first frame
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !tick() {
				return
			}
		}
	}
}

// maxStreamCaptureErrs is how many consecutive capture failures a subscription
// tolerates before pushing a terminal error frame and ending the poll loop.
const maxStreamCaptureErrs = 3

// paneStreamLines is how many trailing lines each delta carries.
const paneStreamLines = 200

// streamKey is a cheap change-detection key over the captured content and the
// parsed status. Avoids re-pushing identical frames. Hashing rather than
// retaining the snapshot string keeps the per-tick idle cost off the heap. The
// status is hashed over every field the wire payload carries (via toStatusJSON),
// so a change to any field the client can render — including Model — produces a
// fresh key rather than being deduped away.
func streamKey(lines []string, ps *session.PaneStatus) uint64 {
	h := fnv.New64a()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	if js := toStatusJSON(ps); js != nil {
		fmt.Fprintf(h, "|%s|%s|%s|%s|%s|%s|%v|%v",
			js.Model, js.ContextPct, js.TokenCount, js.Branch,
			js.StatusLine, js.Permissions, js.IsIdle, js.IsWorking)
	}
	return h.Sum64()
}
