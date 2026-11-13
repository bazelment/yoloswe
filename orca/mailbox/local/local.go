package local

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/store"
)

//nolint:govet
type Config struct {
	RunID        string
	Events       store.EventStore
	Dir          string
	SendInput    func(context.Context, string, string) error
	PollInterval time.Duration
}

//nolint:govet
type Mailbox struct {
	cfg     Config
	inbound chan mailbox.Message
}

func New(cfg Config) (*Mailbox, error) {
	if cfg.Events == nil {
		return nil, fmt.Errorf("event store is required")
	}
	if cfg.Dir == "" {
		return nil, fmt.Errorf("dir is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	return &Mailbox{cfg: cfg, inbound: make(chan mailbox.Message, 32)}, nil
}
func (m *Mailbox) SendLane(ctx context.Context, laneID, body string) error {
	if m.cfg.SendInput == nil {
		return fmt.Errorf("send input is not configured")
	}
	return m.cfg.SendInput(ctx, laneID, body)
}
func (m *Mailbox) AskOwner(ctx context.Context, q mailbox.Question) (string, error) {
	if q.Ask == "" {
		return "", fmt.Errorf("ask is required")
	}
	b, _ := json.Marshal(q)
	if err := os.MkdirAll(m.cfg.Dir, 0755); err != nil {
		return "", err
	}
	p := filepath.Join(m.cfg.Dir, "needs-input-"+fmt.Sprint(time.Now().UnixNano())+".json")
	if err := os.WriteFile(p, b, 0600); err != nil {
		return "", err
	}
	return p, nil
}
func (m *Mailbox) Watch(ctx context.Context) (<-chan store.Event, error) {
	out := make(chan store.Event)
	go func() {
		defer close(out)
		ticker := time.NewTicker(m.cfg.PollInterval)
		defer ticker.Stop()
		for {
			m.read(ctx, out)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return out, nil
}
func (m *Mailbox) read(ctx context.Context, out chan<- store.Event) {
	entries, _ := os.ReadDir(m.cfg.Dir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "in-") {
			continue
		}
		p := filepath.Join(m.cfg.Dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var msg mailbox.Message
		if json.Unmarshal(b, &msg) != nil {
			msg.Body = string(b)
		}
		ev, err := appendEvent(ctx, m.cfg.Events, m.cfg.RunID, msg)
		if err == nil {
			_ = os.Remove(p)
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}
}
func appendEvent(ctx context.Context, es store.EventStore, runID string, msg mailbox.Message) (store.Event, error) {
	p, _ := json.Marshal(msg)
	kind := store.EventMail
	if msg.Owner {
		kind = store.EventOwnerMessage
	}
	return es.Append(ctx, store.Event{RunID: runID, LaneID: msg.LaneID, Kind: kind, Source: msg.From, Summary: msg.Body, Payload: p})
}
