package nebula

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/store"
)

//nolint:govet
type Config struct {
	BaseURL, Token, RunID string
	Events                store.EventStore
	Client                *http.Client
	PollInterval          time.Duration
}
type Mailbox struct{ c Config }

func New(c Config) (*Mailbox, error) {
	if c.BaseURL == "" || c.Events == nil {
		return nil, fmt.Errorf("base URL and event store are required")
	}
	if c.Client == nil {
		c.Client = &http.Client{}
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	return &Mailbox{c: c}, nil
}
func (m *Mailbox) req(ctx context.Context, method, path string, body any) (map[string]any, error) {
	var r *http.Request
	var err error
	if body != nil {
		b, _ := json.Marshal(body)
		r, err = http.NewRequestWithContext(ctx, method, strings.TrimRight(m.c.BaseURL, "/")+path, strings.NewReader(string(b)))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r, err = http.NewRequestWithContext(ctx, method, strings.TrimRight(m.c.BaseURL, "/")+path, http.NoBody)
	}
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+m.c.Token)
	resp, err := m.c.Client.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("nebula mail: status %s", resp.Status)
	}
	var out map[string]any
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}
func (m *Mailbox) SendLane(ctx context.Context, laneID, body string) error {
	_, err := m.req(ctx, http.MethodPost, "/api/mail", map[string]any{"to": "lane:" + laneID, "body": body, "runId": m.c.RunID})
	return err
}
func (m *Mailbox) AskOwner(ctx context.Context, q mailbox.Question) (string, error) {
	v, err := m.req(ctx, http.MethodPost, "/api/mail", map[string]any{"to": "owner", "ask": q.Ask, "options": q.Options, "default": q.Default, "deadline": q.Deadline, "question": true, "runId": m.c.RunID})
	if err != nil {
		return "", err
	}
	if id, ok := v["id"].(string); ok {
		return id, nil
	}
	return "", nil
}
func (m *Mailbox) Watch(ctx context.Context) (<-chan store.Event, error) {
	out := make(chan store.Event)
	go func() {
		defer close(out)
		ticker := time.NewTicker(m.c.PollInterval)
		defer ticker.Stop()
		var cursor string
		for {
			v, err := m.req(ctx, http.MethodGet, "/api/mail?cursor="+cursor, nil)
			if err == nil {
				if arr, ok := v["messages"].([]any); ok {
					for _, raw := range arr {
						b, _ := json.Marshal(raw)
						var msg mailbox.Message
						_ = json.Unmarshal(b, &msg)
						p, _ := json.Marshal(msg)
						kind := store.EventMail
						if msg.Owner {
							kind = store.EventOwnerMessage
						}
						ev, e := m.c.Events.Append(ctx, store.Event{RunID: m.c.RunID, LaneID: msg.LaneID, Kind: kind, Source: msg.From, Summary: msg.Body, Payload: p})
						if e == nil {
							select {
							case out <- ev:
							case <-ctx.Done():
								return
							}
						}
					}
					if c, ok := v["nextCursor"].(string); ok {
						cursor = c
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return out, nil
}
