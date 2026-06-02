package control

import (
	"context"
	"fmt"
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

	var lastKey string
	emit := func() {
		lines, err := s.disp.ctl.Capture(ctx, target, paneStreamLines)
		if err != nil {
			return
		}
		ps, _ := s.disp.ctl.Status(ctx, target)
		key := streamKey(lines, ps)
		if key == lastKey {
			return // unchanged — suppress to keep an idle pane silent
		}
		lastKey = key
		delta := PaneDelta{Lines: lines, Status: toStatusJSON(ps)}
		msg, err := NewRequest(TypePaneDelta, "", delta)
		if err != nil {
			return
		}
		msg.SubID = subID
		_ = s.conn.WriteMsg(msg)
	}

	emit() // immediate first frame
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			emit()
		}
	}
}

// paneStreamLines is how many trailing lines each delta carries.
const paneStreamLines = 200

// streamKey is a cheap change-detection key over the captured content and the
// parsed idle/working/status. Avoids re-pushing identical frames.
func streamKey(lines []string, ps *session.PaneStatus) string {
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
	}
	if ps != nil {
		b = append(b, fmt.Sprintf("|%v|%v|%s|%s|%s", ps.IsIdle, ps.IsWorking, ps.ContextPct, ps.TokenCount, ps.StatusLine)...)
	}
	return string(b)
}
