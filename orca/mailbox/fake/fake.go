package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/store"
)

//nolint:govet
type Mailbox struct {
	Events    store.EventStore
	RunID     string
	mu        sync.Mutex
	sent      []mailbox.Message
	answers   map[string]string
	dismissed map[string]string
	seen      map[string]struct{}
	inbound   chan mailbox.Message
}

func New(events store.EventStore, runID string) *Mailbox {
	return &Mailbox{
		Events:    events,
		RunID:     runID,
		answers:   make(map[string]string),
		dismissed: make(map[string]string),
		seen:      make(map[string]struct{}),
		inbound:   make(chan mailbox.Message, 32),
	}
}
func (m *Mailbox) SendLane(_ context.Context, laneID, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, mailbox.Message{RunID: m.RunID, LaneID: laneID, Body: body})
	return nil
}
func (m *Mailbox) Answer(_ context.Context, msgID, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.answers == nil {
		m.answers = make(map[string]string)
	}
	m.answers[msgID] = body
	m.sent = append(m.sent, mailbox.Message{
		ID:        msgID,
		RunID:     m.RunID,
		Body:      body,
		Answer:    body,
		InReplyTo: msgID,
		Status:    "answered",
	})
	return nil
}
func (m *Mailbox) Dismiss(_ context.Context, msgID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dismissed == nil {
		m.dismissed = make(map[string]string)
	}
	m.dismissed[msgID] = reason
	m.sent = append(m.sent, mailbox.Message{
		ID:        msgID,
		RunID:     m.RunID,
		Body:      reason,
		InReplyTo: msgID,
		Status:    "dismissed",
	})
	return nil
}
func (m *Mailbox) DismissedFor(msgID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	reason, ok := m.dismissed[msgID]
	return reason, ok
}
func (m *Mailbox) Dismissed() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.dismissed))
	for k, v := range m.dismissed {
		out[k] = v
	}
	return out
}
func (m *Mailbox) Reply(_ context.Context, laneID, msgID, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, mailbox.Message{
		RunID:     m.RunID,
		LaneID:    laneID,
		Body:      body,
		InReplyTo: msgID,
	})
	return nil
}
func (m *Mailbox) AnswerFor(msgID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ans, ok := m.answers[msgID]
	return ans, ok
}
func (m *Mailbox) AskOwner(_ context.Context, q mailbox.Question) (string, error) {
	if q.Ask == "" {
		return "", fmt.Errorf("ask is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, mailbox.Message{RunID: m.RunID, Body: q.Ask, Owner: true, Question: true})
	return "", nil
}
func (m *Mailbox) Inject(msg mailbox.Message) { m.inbound <- msg }
func (m *Mailbox) Sent() []mailbox.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mailbox.Message(nil), m.sent...)
}
func (m *Mailbox) Watch(ctx context.Context) (<-chan store.Event, error) {
	if m.Events == nil {
		return nil, fmt.Errorf("event store is required")
	}
	m.seedSeenFromStore(ctx)
	out := make(chan store.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-m.inbound:
				msg.Normalize()
				if msg.ID != "" && m.markOrCheckSeen(msg.ID, false) {
					continue
				}
				ev, err := mailbox.AppendEvent(ctx, m.Events, m.RunID, msg)
				if err != nil {
					return
				}
				if msg.ID != "" {
					m.markOrCheckSeen(msg.ID, true)
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (m *Mailbox) seedSeenFromStore(ctx context.Context) {
	if m.Events == nil {
		return
	}
	evs, err := m.Events.List(ctx, store.EventFilter{
		RunID: m.RunID,
		Kinds: []store.EventKind{store.EventMail, store.EventOwnerMessage},
	})
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = make(map[string]struct{})
	}
	for i := range evs {
		ev := &evs[i]
		var msg mailbox.Message
		if len(ev.Payload) > 0 && json.Unmarshal(ev.Payload, &msg) == nil && msg.ID != "" {
			m.seen[msg.ID] = struct{}{}
		}
	}
}

func (m *Mailbox) markOrCheckSeen(id string, mark bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = make(map[string]struct{})
	}
	_, dup := m.seen[id]
	if mark {
		m.seen[id] = struct{}{}
	}
	return dup
}
