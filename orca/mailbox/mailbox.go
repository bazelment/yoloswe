// Package mailbox provides the transport boundary for lane and owner messages.
package mailbox

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/store"
)

const answeredDirectivePrefix = "lane_mail_answered:"

// Question is a structured question for the owner.
//
//nolint:govet
type Question struct {
	Ask      string
	Options  []string
	Default  string
	Deadline time.Time
}

// Message is an inbound mailbox message. Replies retain InReplyTo.
//
//nolint:govet
type Message struct {
	ID        string `json:"id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	LaneID    string `json:"lane_id,omitempty"`
	From      string `json:"from,omitempty"`
	Body      string `json:"body"`
	Details   string `json:"details,omitempty"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Class     string `json:"class,omitempty"`
	Status    string `json:"status,omitempty"`
	Answer    string `json:"answer,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Owner     bool   `json:"owner,omitempty"`
	Question  bool   `json:"question,omitempty"`
	Blocking  bool   `json:"blocking,omitempty"`
}

// Normalize reconciles Nebula camelCase / kind fields with canonical Message fields.
func (m *Message) Normalize() {
	if m == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(m.Kind), "question") {
		m.Question = true
	} else if m.Question && strings.TrimSpace(m.Kind) == "" {
		m.Kind = "question"
	}
	if m.Body == "" && m.Details != "" {
		m.Body = m.Details
	}
	if m.Question && strings.TrimSpace(m.Status) == "" {
		m.Status = "open"
	}
	if strings.TrimSpace(m.Answer) != "" && strings.EqualFold(strings.TrimSpace(m.Status), "open") {
		m.Status = "answered"
	}
	if strings.TrimSpace(m.From) == "" && strings.TrimSpace(m.LaneID) != "" {
		m.From = "lane:" + strings.TrimSpace(m.LaneID)
	}
}

// IsQuestion reports whether the message is a question from a lane or owner.
func (m Message) IsQuestion() bool {
	return m.Question || strings.EqualFold(strings.TrimSpace(m.Kind), "question")
}

// IsClosedOrAnswered reports whether a question message is already answered or closed.
func (m Message) IsClosedOrAnswered() bool {
	st := strings.ToLower(strings.TrimSpace(m.Status))
	return st == "answered" || st == "closed" || st == "resolved" || strings.TrimSpace(m.Answer) != ""
}

// UnmarshalJSON accepts both canonical snake_case fields and Nebula /api/mail camelCase fields.
func (m *Message) UnmarshalJSON(data []byte) error {
	type alias Message
	var raw struct {
		RunIDCamel     string `json:"runId"`
		Lane           string `json:"lane"`
		LaneIDCamel    string `json:"laneId"`
		InReplyToCamel string `json:"inReplyTo"`
		FromKind       string `json:"fromKind"`
		FromSessionID  string `json:"fromSessionId"`
		Ask            string `json:"ask"`
		CreatedAtCamel string `json:"createdAt"`
		alias
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = Message(raw.alias)
	m.RunID = cmp.Or(m.RunID, raw.RunIDCamel)
	m.LaneID = cmp.Or(m.LaneID, raw.Lane, raw.LaneIDCamel)
	m.InReplyTo = cmp.Or(m.InReplyTo, raw.InReplyToCamel)
	m.CreatedAt = cmp.Or(m.CreatedAt, raw.CreatedAtCamel)
	m.Body = cmp.Or(m.Body, raw.Ask)
	if strings.EqualFold(strings.TrimSpace(raw.FromKind), "owner") {
		m.Owner = true
	}
	if strings.TrimSpace(m.From) == "" && strings.TrimSpace(raw.FromSessionID) != "" {
		m.From = strings.TrimSpace(raw.FromSessionID)
	}
	m.Normalize()
	return nil
}

// Mailbox sends lane/owner messages and watches inbound messages. Watch
// appends every message to Events before delivering it on the returned channel.
type Mailbox interface {
	SendLane(context.Context, string, string) error
	AskOwner(context.Context, Question) (string, error)
	Watch(context.Context) (<-chan store.Event, error)
}

// Responder is an optional interface implemented by mailboxes that support
// answering a specific question message ID (`nebula mail answer`) or replying
// to a specific note message ID (`nebula mail send --in-reply-to`).
type Responder interface {
	Answer(ctx context.Context, msgID, body string) error
	Reply(ctx context.Context, laneID, msgID, body string) error
}

// Respond answers a lane question on its message ID, replies to a lane note with
// InReplyTo, or falls back to SendLane when the message has no ID.
func Respond(ctx context.Context, box Mailbox, msg Message, body string) error {
	if box == nil {
		return nil
	}
	msg.Normalize()
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	if msg.ID != "" {
		if r, ok := box.(Responder); ok {
			if msg.IsQuestion() {
				return r.Answer(ctx, msg.ID, body)
			}
			return r.Reply(ctx, msg.LaneID, msg.ID, body)
		}
	}
	if msg.LaneID != "" {
		return box.SendLane(ctx, msg.LaneID, body)
	}
	return nil
}

// AppendEvent persists an inbound message using the store event kind for its audience.
func AppendEvent(ctx context.Context, events store.EventStore, runID string, msg Message) (store.Event, error) {
	msg.Normalize()
	if msg.RunID == "" {
		msg.RunID = runID
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return store.Event{}, err
	}
	kind := store.EventMail
	if msg.Owner {
		kind = store.EventOwnerMessage
	}
	summary := msg.Body
	if msg.Details != "" && msg.Details != msg.Body {
		summary = msg.Body + " — " + msg.Details
	}
	return events.Append(ctx, store.Event{
		RunID: runID, LaneID: msg.LaneID, Kind: kind, Source: msg.From,
		Summary: summary, Payload: payload,
		Urgent: msg.Blocking || msg.IsQuestion(),
	})
}

// MarkAnswered records that a lane question or note has been answered/closed so it
// leaves the open questions list.
func MarkAnswered(ctx context.Context, st store.Store, runID, laneID, msgID, eventID, answer string) error {
	if st == nil {
		return nil
	}
	key := strings.TrimSpace(cmp.Or(msgID, eventID))
	if key == "" {
		return nil
	}
	ref := answeredDirectivePrefix + key
	if msgID != "" && eventID != "" && msgID != eventID {
		ref = answeredDirectivePrefix + msgID + "|" + eventID
	}
	dir := store.Directive{
		ID:     answeredDirectivePrefix + key,
		RunID:  runID,
		Kind:   store.DirectiveNote,
		Source: store.DirectiveSourceDispatcher,
		Scope:  laneID,
		Ref:    ref,
		Text:   strings.TrimSpace(answer),
		Status: store.DirectiveHandled,
	}
	if err := st.Directives().Add(ctx, dir); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	// Also mark any active owner_ask directive referencing this mail as handled.
	dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err == nil {
		for i := range dirs {
			d := &dirs[i]
			if !strings.HasPrefix(d.ID, "owner_ask:") && !strings.HasPrefix(d.Ref, "owner_ask:") {
				continue
			}
			var ask struct {
				SourceEventID   string `json:"source_event_id"`
				SourceMessageID string `json:"source_message_id"`
			}
			if json.Unmarshal([]byte(d.Text), &ask) == nil &&
				((msgID != "" && (ask.SourceMessageID == msgID || ask.SourceEventID == msgID)) ||
					(eventID != "" && (ask.SourceEventID == eventID || ask.SourceMessageID == eventID))) {
				_ = st.Directives().UpdateStatus(ctx, runID, d.ID, store.DirectiveHandled)
			}
		}
	}
	return nil
}

// OpenQuestion represents an unresolved question from a lane in a run.
//
//nolint:govet
type OpenQuestion struct {
	ID             string
	EventID        string
	RunID          string
	LaneID         string
	Question       string
	Details        string
	Class          string
	Blocking       bool
	CreatedAt      time.Time
	Age            string
	EscalatedAskID string
}

// OpenLaneQuestions returns all unresolved lane questions in runID ordered by arrival.
func OpenLaneQuestions(ctx context.Context, st store.Store, runID string, now time.Time) ([]OpenQuestion, error) {
	if st == nil || strings.TrimSpace(runID) == "" {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	evs, err := st.Events().List(ctx, store.EventFilter{
		RunID: runID,
		Kinds: []store.EventKind{store.EventMail},
	})
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return nil, nil
	}

	answered := make(map[string]bool)
	escalatedAskID := make(map[string]string)
	if dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: runID}); err == nil {
		for i := range dirs {
			d := &dirs[i]
			if strings.HasPrefix(d.ID, answeredDirectivePrefix) || strings.HasPrefix(d.Ref, answeredDirectivePrefix) {
				answered[strings.TrimPrefix(d.ID, answeredDirectivePrefix)] = true
				ref := strings.TrimPrefix(d.Ref, answeredDirectivePrefix)
				for _, part := range strings.Split(ref, "|") {
					if part = strings.TrimSpace(part); part != "" {
						answered[part] = true
					}
				}
				continue
			}
			if strings.HasPrefix(d.ID, "owner_ask:") || strings.HasPrefix(d.Ref, "owner_ask:") {
				var ask struct {
					ID              string `json:"id"`
					LaneID          string `json:"lane_id"`
					Question        string `json:"question"`
					SourceEventID   string `json:"source_event_id"`
					SourceMessageID string `json:"source_message_id"`
				}
				if json.Unmarshal([]byte(d.Text), &ask) == nil {
					askID := cmp.Or(strings.TrimSpace(ask.ID), strings.TrimPrefix(d.Ref, "owner_ask:"), strings.TrimPrefix(d.ID, "owner_ask:"))
					if d.Status == store.DirectiveHandled {
						if ask.ID != "" {
							answered[ask.ID] = true
						}
						if ask.SourceEventID != "" {
							answered[ask.SourceEventID] = true
						}
						if ask.SourceMessageID != "" {
							answered[ask.SourceMessageID] = true
						}
					} else if d.Status == store.DirectiveActive && (d.ExpiresAt == nil || now.Before(*d.ExpiresAt)) {
						if ask.SourceMessageID != "" {
							escalatedAskID[ask.SourceMessageID] = askID
						}
						if ask.SourceEventID != "" {
							escalatedAskID[ask.SourceEventID] = askID
						}
						if ask.LaneID != "" && ask.Question != "" {
							escalatedAskID[ask.LaneID+"|"+strings.TrimSpace(ask.Question)] = askID
						}
					}
				}
			}
		}
	}
	if decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID}); err == nil {
		for i := range decs {
			dec := &decs[i]
			if dec.Tool == "worker.question.answer" || dec.Tool == "mail.answer" {
				var args struct {
					MessageID string `json:"message_id"`
					ID        string `json:"id"`
				}
				if len(dec.Args) > 0 && json.Unmarshal(dec.Args, &args) == nil {
					if args.MessageID != "" {
						answered[args.MessageID] = true
					}
					if args.ID != "" {
						answered[args.ID] = true
					}
				}
			}
		}
	}

	type decodedMail struct {
		ev  *store.Event
		msg Message
	}
	decoded := make([]decodedMail, 0, len(evs))
	for i := range evs {
		ev := &evs[i]
		var msg Message
		if len(ev.Payload) == 0 || json.Unmarshal(ev.Payload, &msg) != nil {
			continue
		}
		if msg.InReplyTo != "" {
			answered[msg.InReplyTo] = true
		}
		if msg.ID != "" && msg.IsClosedOrAnswered() {
			answered[msg.ID] = true
		}
		decoded = append(decoded, decodedMail{ev: ev, msg: msg})
	}

	seen := make(map[string]bool)
	var out []OpenQuestion
	for i := range decoded {
		ev := decoded[i].ev
		msg := decoded[i].msg
		laneID := strings.TrimSpace(cmp.Or(msg.LaneID, ev.LaneID))
		if laneID == "" || !msg.IsQuestion() || msg.Owner {
			continue
		}
		qID := strings.TrimSpace(cmp.Or(msg.ID, ev.ID, fmt.Sprintf("seq-%d", ev.Seq)))
		qText := strings.TrimSpace(cmp.Or(msg.Body, ev.Summary))
		if msg.IsClosedOrAnswered() || answered[qID] || (msg.ID != "" && answered[msg.ID]) || (ev.ID != "" && answered[ev.ID]) {
			continue
		}
		if seen[qID] {
			continue
		}
		seen[qID] = true

		escID := cmp.Or(
			escalatedAskID[qID],
			escalatedAskID[msg.ID],
			escalatedAskID[ev.ID],
			escalatedAskID[laneID+"|"+qText],
		)
		createdAt := ev.CreatedAt
		if msg.CreatedAt != "" {
			if t, parseErr := time.Parse(time.RFC3339, msg.CreatedAt); parseErr == nil {
				createdAt = t
			}
		}
		out = append(out, OpenQuestion{
			ID:             qID,
			EventID:        ev.ID,
			RunID:          runID,
			LaneID:         laneID,
			Question:       qText,
			Details:        strings.TrimSpace(msg.Details),
			Class:          msg.Class,
			Blocking:       msg.Blocking,
			CreatedAt:      createdAt,
			Age:            FormatAge(now, createdAt),
			EscalatedAskID: escID,
		})
	}
	return out, nil
}

// OpenUnescalatedQuestions returns unresolved lane questions that have not been
// escalated to an active owner_ask directive. Use this at display sites that
// also render active owner_ask directives so escalated lane questions are not
// double-counted or listed twice.
func OpenUnescalatedQuestions(ctx context.Context, st store.Store, runID string, now time.Time) ([]OpenQuestion, error) {
	qs, err := OpenLaneQuestions(ctx, st, runID, now)
	if err != nil || len(qs) == 0 {
		return qs, err
	}
	out := make([]OpenQuestion, 0, len(qs))
	for i := range qs {
		if qs[i].EscalatedAskID == "" {
			out = append(out, qs[i])
		}
	}
	return out, nil
}

// FormatAge formats the elapsed time between createdAt and now as a compact duration string.
func FormatAge(now, createdAt time.Time) string {
	if createdAt.IsZero() || now.IsZero() || now.Before(createdAt) {
		return "0s"
	}
	d := now.Sub(createdAt).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
