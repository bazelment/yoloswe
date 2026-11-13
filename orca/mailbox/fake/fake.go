package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/store"
)

//nolint:govet
type Mailbox struct {
	Events  store.EventStore
	RunID   string
	mu      sync.Mutex
	sent    []mailbox.Message
	inbound chan mailbox.Message
}

func New(events store.EventStore, runID string) *Mailbox {
	return &Mailbox{Events: events, RunID: runID, inbound: make(chan mailbox.Message, 32)}
}
func (m *Mailbox) SendLane(_ context.Context, laneID, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, mailbox.Message{RunID: m.RunID, LaneID: laneID, Body: body})
	return nil
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
	out := make(chan store.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-m.inbound:
				ev, err := event(ctx, m.Events, m.RunID, msg)
				if err != nil {
					return
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
func event(ctx context.Context, es store.EventStore, runID string, msg mailbox.Message) (store.Event, error) {
	return mailbox.AppendEvent(ctx, es, runID, msg)
}
