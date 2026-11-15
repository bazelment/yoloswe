package nebula

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
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
	Host                  bool
	SessionID             string
}

//nolint:govet // logical field grouping
type Mailbox struct {
	c           Config
	mu          sync.Mutex
	cursor      string
	seen        map[string]struct{}
	adoptCutoff time.Time
	adoptSynced bool
	store       store.Store
}

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
	return &Mailbox{c: c, seen: make(map[string]struct{})}, nil
}

// SetAdoptCutoff configures the adopt timestamp so mail older than cutoff is
// marked as already seen and summarized once rather than replayed per message.
func (m *Mailbox) SetAdoptCutoff(cutoff time.Time, st store.Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !cutoff.IsZero() {
		m.adoptCutoff = cutoff.UTC()
	}
	if st != nil {
		m.store = st
	}
}

// SyncAdoptCutoff polls the mailbox once and marks any mail older than the
// adopt cutoff as already seen, recording a single summary event and decision.
func (m *Mailbox) SyncAdoptCutoff(ctx context.Context) error {
	m.seedFromStore(ctx)
	_, err := m.pollOnce(ctx, true)
	return err
}

func (m *Mailbox) req(ctx context.Context, method, path string, body any) (map[string]any, error) {
	var r *http.Request
	var err error
	if body != nil {
		b, _ := json.Marshal(body)
		r, err = http.NewRequestWithContext(ctx, method, strings.TrimRight(m.c.BaseURL, "/")+path, bytes.NewReader(b))
		if err == nil {
			r.Header.Set("Content-Type", "application/json")
		}
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
	_, err := m.req(ctx, http.MethodPost, "/api/mail", map[string]any{"to": "lane:" + laneID, "lane": laneID, "laneId": laneID, "body": body, "runId": m.c.RunID})
	return err
}
func (m *Mailbox) Answer(ctx context.Context, msgID, body string) error {
	path := "/api/mail/" + url.PathEscape(msgID) + "/answer"
	_, err := m.req(ctx, http.MethodPost, path, map[string]any{"answer": body, "body": body, "runId": m.c.RunID})
	return err
}
func (m *Mailbox) Dismiss(ctx context.Context, msgID, reason string) error {
	path := "/api/mail/" + url.PathEscape(msgID) + "/dismiss"
	_, err := m.req(ctx, http.MethodPost, path, map[string]any{"reason": reason, "body": reason, "status": "dismissed", "runId": m.c.RunID})
	return err
}
func (m *Mailbox) Reply(ctx context.Context, laneID, msgID, body string) error {
	payload := map[string]any{"inReplyTo": msgID, "body": body, "runId": m.c.RunID}
	if laneID != "" {
		payload["to"] = "lane:" + laneID
		payload["lane"] = laneID
		payload["laneId"] = laneID
	}
	_, err := m.req(ctx, http.MethodPost, "/api/mail", payload)
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

func (m *Mailbox) isOlderThanAdopt(msg mailbox.Message, initialAdoptPoll bool) bool {
	m.mu.Lock()
	cutoff := m.adoptCutoff
	m.mu.Unlock()
	if cutoff.IsZero() {
		return false
	}
	if ts, ok := msg.ParseCreatedAt(); ok {
		return !ts.After(cutoff)
	}
	return initialAdoptPoll
}

func (m *Mailbox) pollOnce(ctx context.Context, forceAdoptCheck bool) ([]store.Event, error) {
	q := url.Values{}
	host := m.c.Host || (strings.TrimSpace(m.c.SessionID) == "" && strings.TrimSpace(os.Getenv("NEBULA_SESSION_ID")) == "")
	if host {
		q.Set("box", "host")
		q.Set("run", m.c.RunID)
	}
	cur := m.getCursor()
	if cur != "" {
		q.Set("cursor", cur)
	}
	v, err := m.req(ctx, http.MethodGet, "/api/mail?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	initialAdopt := forceAdoptCheck || (!m.adoptCutoff.IsZero() && !m.adoptSynced)
	m.adoptSynced = true
	st := m.store
	m.mu.Unlock()

	nextCursor, _ := v["nextCursor"].(string)
	arr, ok := v["messages"].([]any)
	if !ok {
		return nil, nil
	}
	pageOK := true
	msgs := make([]mailbox.Message, 0, len(arr))
	keys := make([]string, 0, len(arr))
	unseen := make([]bool, 0, len(arr))
	lastUnseen := -1
	for i, raw := range arr {
		b, marshalErr := json.Marshal(raw)
		if marshalErr != nil {
			pageOK = false
			break
		}
		var msg mailbox.Message
		if unmarshalErr := json.Unmarshal(b, &msg); unmarshalErr != nil {
			pageOK = false
			break
		}
		key := m.messageSeenKey(msg)
		isUnseen := !m.checkOrMarkSeen(key, false)
		if isUnseen {
			lastUnseen = i
		}
		msgs = append(msgs, msg)
		keys = append(keys, key)
		unseen = append(unseen, isUnseen)
	}
	if !pageOK {
		return nil, nil
	}

	var (
		oldMsgs  []mailbox.Message
		oldKeys  []string
		oldIDs   []string
		appended []store.Event
	)
	for i := range msgs {
		msg := msgs[i]
		key := keys[i]
		if !unseen[i] {
			continue
		}
		if m.isOlderThanAdopt(msg, initialAdopt) {
			m.checkOrMarkSeen(key, true)
			oldMsgs = append(oldMsgs, msg)
			oldKeys = append(oldKeys, key)
			if msg.ID != "" {
				oldIDs = append(oldIDs, msg.ID)
			}
			continue
		}
		if i == lastUnseen && nextCursor != "" && msg.Cursor == "" {
			msg.Cursor = nextCursor
		}
		ev, appendErr := mailbox.AppendEvent(ctx, m.c.Events, m.c.RunID, msg)
		if appendErr != nil {
			pageOK = false
			break
		}
		m.checkOrMarkSeen(key, true)
		appended = append(appended, ev)
	}
	if len(oldMsgs) > 0 {
		summary := mailbox.FormatAdoptMailSummary(oldMsgs)
		payload, _ := json.Marshal(mailbox.AdoptMailSummaryPayload{
			Cursor:     nextCursor,
			SeenKeys:   oldKeys,
			MessageIDs: oldIDs,
			Count:      len(oldMsgs),
		})
		summaryEv, sErr := m.c.Events.Append(ctx, store.Event{
			RunID:   m.c.RunID,
			Kind:    store.EventMail,
			Class:   store.EventClassAuto,
			Source:  mailbox.AdoptMailSummarySource,
			Summary: summary,
			Payload: payload,
		})
		if sErr == nil {
			appended = append([]store.Event{summaryEv}, appended...)
		}
		if st != nil {
			_, _ = st.Decisions().Append(ctx, store.Decision{
				RunID:  m.c.RunID,
				Actor:  "controller",
				Tool:   mailbox.AdoptMailSummaryTool,
				Reason: "mark host mail older than adopt as seen",
				Result: summary,
				Vetted: true,
			})
		}
	}
	if pageOK && nextCursor != "" {
		m.setCursor(nextCursor)
	}
	return appended, nil
}

func (m *Mailbox) Watch(ctx context.Context) (<-chan store.Event, error) {
	m.seedFromStore(ctx)
	out := make(chan store.Event)
	go func() {
		defer close(out)
		ticker := time.NewTicker(m.c.PollInterval)
		defer ticker.Stop()
		for {
			appended, err := m.pollOnce(ctx, false)
			if err == nil {
				for i := range appended {
					select {
					case out <- appended[i]:
					case <-ctx.Done():
						return
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

func (m *Mailbox) seedFromStore(ctx context.Context) {
	if m.c.Events == nil {
		return
	}
	evs, err := m.c.Events.List(ctx, store.EventFilter{
		RunID: m.c.RunID,
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
		if ev.Source == mailbox.AdoptMailSummarySource && len(ev.Payload) > 0 {
			var sum mailbox.AdoptMailSummaryPayload
			if json.Unmarshal(ev.Payload, &sum) == nil {
				for _, k := range sum.SeenKeys {
					if k != "" {
						m.seen[k] = struct{}{}
					}
				}
				for _, id := range sum.MessageIDs {
					if id != "" {
						m.seen[id] = struct{}{}
					}
				}
				if sum.Cursor != "" {
					m.cursor = sum.Cursor
				}
				m.adoptSynced = true
			}
			continue
		}
		var msg mailbox.Message
		if len(ev.Payload) > 0 && json.Unmarshal(ev.Payload, &msg) == nil {
			if key := m.messageSeenKey(msg); key != "" {
				m.seen[key] = struct{}{}
			}
			if msg.Cursor != "" {
				m.cursor = msg.Cursor
			}
		}
	}
}

func (m *Mailbox) messageSeenKey(msg mailbox.Message) string {
	msg.Normalize()
	if msg.ID != "" {
		return msg.ID
	}
	canon := msg
	if canon.RunID == "" {
		canon.RunID = m.c.RunID
	}
	canon.Cursor = ""
	canon.Status = ""
	canon.Answer = ""
	b, err := json.Marshal(canon)
	if err != nil {
		return fmt.Sprintf("body:%x", sha256.Sum256([]byte(canon.Body)))
	}
	return fmt.Sprintf("body:%x", sha256.Sum256(b))
}

func (m *Mailbox) getCursor() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursor
}

func (m *Mailbox) setCursor(c string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cursor = c
}

func (m *Mailbox) checkOrMarkSeen(key string, mark bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = make(map[string]struct{})
	}
	_, dup := m.seen[key]
	if mark {
		m.seen[key] = struct{}{}
	}
	return dup
}
