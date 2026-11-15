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
	switch st {
	case "answered", "closed", "resolved", "withdrawn", "dismissed", "obsolete", "cancelled", "canceled", "superseded", "expired":
		return true
	}
	if strings.TrimSpace(m.Answer) != "" {
		return true
	}
	lowerBody := strings.ToLower(strings.TrimSpace(m.Body))
	if strings.HasPrefix(lowerBody, "withdrawn:") ||
		strings.HasPrefix(lowerBody, "[withdrawn]") ||
		strings.Contains(lowerBody, "(withdrawn") ||
		strings.Contains(lowerBody, "question withdrawn") {
		return true
	}
	return false
}

// ParseCreatedAt parses CreatedAt if present (accepting RFC3339Nano or RFC3339).
func (m Message) ParseCreatedAt() (time.Time, bool) {
	raw := strings.TrimSpace(m.CreatedAt)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
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

// Dismisser is an optional interface implemented by mailboxes that support
// dismissing or resolving a stale/obsolete question (`nebula mail dismiss`).
type Dismisser interface {
	Dismiss(ctx context.Context, msgID, reason string) error
}

// Dismiss closes a stale/obsolete lane question on box if supported.
func Dismiss(ctx context.Context, box Mailbox, msg Message, reason string) error {
	if box == nil {
		return nil
	}
	msg.Normalize()
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "stale or obsolete question"
	}
	if msg.ID != "" {
		if d, ok := box.(Dismisser); ok {
			return d.Dismiss(ctx, msg.ID, reason)
		}
		if r, ok := box.(Responder); ok && msg.IsQuestion() {
			return r.Answer(ctx, msg.ID, "dismissed: "+reason)
		}
	}
	return nil
}

// AdoptMailSummarySource is the Event.Source used for the single adopt mail summary event.
const AdoptMailSummarySource = "adopt.mail_summary"

// AdoptMailSummaryTool is the Decision.Tool used for the single adopt mail summary decision.
const AdoptMailSummaryTool = "mail.adopt_summary"

// AdoptMailSummaryPayload records the messages marked seen during adopt.
type AdoptMailSummaryPayload struct {
	Cursor     string   `json:"cursor,omitempty"`
	SeenKeys   []string `json:"seen_keys,omitempty"`
	MessageIDs []string `json:"message_ids,omitempty"`
	Count      int      `json:"count"`
}

// AdoptCutoffSetter is implemented by mailboxes that can mark mail older than
// an adopt cutoff as already seen and emit a single summary line.
type AdoptCutoffSetter interface {
	SetAdoptCutoff(cutoff time.Time, st store.Store)
	SyncAdoptCutoff(ctx context.Context) error
}

// FormatAdoptMailSummary renders a concise one-line summary for host mails
// marked seen on adopt.
func FormatAdoptMailSummary(msgs []Message) string {
	if len(msgs) == 0 {
		return ""
	}
	var previews []string
	for i := range msgs {
		if len(previews) >= 4 {
			break
		}
		m := msgs[i]
		m.Normalize()
		who := strings.TrimSpace(cmp.Or(m.LaneID, m.From, "host"))
		body := strings.Join(strings.Fields(strings.TrimSpace(m.Body)), " ")
		if len(body) > 48 {
			body = body[:48] + "..."
		}
		if body != "" {
			previews = append(previews, who+": "+body)
		} else {
			previews = append(previews, who)
		}
	}
	suffix := ""
	if len(previews) > 0 {
		suffix = " (" + strings.Join(previews, "; ")
		if len(msgs) > len(previews) {
			suffix += fmt.Sprintf("; +%d more", len(msgs)-len(previews))
		}
		suffix += ")"
	}
	return fmt.Sprintf("adopted mail summary: %d prior host message(s) marked seen%s", len(msgs), suffix)
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

type decodedMail struct {
	msg Message
	ev  store.Event
}

// IsAnswered reports whether the lane mail identified by msgID or eventID has
// already been answered or closed in st.
func IsAnswered(ctx context.Context, st store.Store, runID, msgID, eventID string) bool {
	if st == nil || strings.TrimSpace(runID) == "" {
		return false
	}
	msgID = strings.TrimSpace(msgID)
	eventID = strings.TrimSpace(eventID)
	if msgID == "" && eventID == "" {
		return false
	}
	answered, _, _, err := collectAnsweredMailState(ctx, st, runID, time.Time{})
	if err != nil {
		return false
	}
	return (msgID != "" && answered[msgID]) || (eventID != "" && answered[eventID])
}

// IsQuestionObsoleteOrClosed reports whether an owner ask or lane question is
// already answered, withdrawn, dismissed, or belongs to a completed/merged lane.
func IsQuestionObsoleteOrClosed(ctx context.Context, st store.Store, runID, laneID, msgID, eventID, questionText string) bool {
	if st == nil || strings.TrimSpace(runID) == "" {
		return false
	}
	lowerQ := strings.ToLower(strings.TrimSpace(questionText))
	if strings.HasPrefix(lowerQ, "withdrawn:") ||
		strings.HasPrefix(lowerQ, "[withdrawn]") ||
		strings.Contains(lowerQ, "(withdrawn") ||
		strings.Contains(lowerQ, "question withdrawn") {
		return true
	}
	if laneID = strings.TrimSpace(laneID); laneID != "" {
		if ln, err := st.Lanes().Get(ctx, runID, laneID); err == nil && ln != nil {
			if ln.Status.Terminal() || strings.TrimSpace(ln.MergeSHA) != "" {
				return true
			}
		}
	}
	msgID = strings.TrimSpace(msgID)
	eventID = strings.TrimSpace(eventID)
	if msgID == "" && eventID == "" {
		return false
	}
	return IsAnswered(ctx, st, runID, msgID, eventID)
}

func collectAnsweredMailState(ctx context.Context, st store.Store, runID string, now time.Time) (map[string]bool, map[string]string, []decodedMail, error) {
	answered := make(map[string]bool)
	escalatedAskID := make(map[string]string)
	if st == nil {
		return answered, escalatedAskID, nil, nil
	}
	obsoleteLane := make(map[string]bool)
	if lanes, lErr := st.Lanes().List(ctx, store.LaneFilter{RunID: runID}); lErr == nil {
		for i := range lanes {
			if lanes[i].Status.Terminal() || strings.TrimSpace(lanes[i].MergeSHA) != "" {
				obsoleteLane[strings.TrimSpace(lanes[i].ID)] = true
			}
		}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: runID}); err == nil {
		heldAskIDs := make(map[string]bool)
		for i := range dirs {
			d := &dirs[i]
			if ref := strings.TrimSpace(d.Ref); ref != "" && strings.HasPrefix(strings.TrimSpace(d.Text), "hold:") {
				heldAskIDs[ref] = true
			}
		}
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
					laneID := strings.TrimSpace(cmp.Or(ask.LaneID, d.Scope))
					if obsoleteLane[laneID] {
						if ask.ID != "" {
							answered[ask.ID] = true
						}
						if ask.SourceEventID != "" {
							answered[ask.SourceEventID] = true
						}
						if ask.SourceMessageID != "" {
							answered[ask.SourceMessageID] = true
						}
						continue
					}
					if d.Status == store.DirectiveHandled && !heldAskIDs[askID] {
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
			res := strings.TrimSpace(dec.Result)
			if !dec.Vetted || res == "" || strings.HasPrefix(res, "error:") || strings.HasPrefix(res, "rejected:") {
				continue
			}
			switch dec.Tool {
			case "worker.question.answer", "mail.answer", "mail.dismiss", "mail.resolve", "worker.question.dismiss", "question.dismiss":
				var args struct {
					MessageID string `json:"message_id"`
					ID        string `json:"id"`
					EventID   string `json:"event_id"`
				}
				if len(dec.Args) > 0 && json.Unmarshal(dec.Args, &args) == nil {
					if args.MessageID != "" {
						answered[args.MessageID] = true
					}
					if args.ID != "" {
						answered[args.ID] = true
					}
					if args.EventID != "" {
						answered[args.EventID] = true
					}
				}
			}
		}
	}
	evs, err := st.Events().List(ctx, store.EventFilter{
		RunID: runID,
		Kinds: []store.EventKind{store.EventMail},
	})
	if err != nil || len(evs) == 0 {
		return answered, escalatedAskID, nil, err
	}
	decoded := make([]decodedMail, 0, len(evs))
	for i := range evs {
		ev := evs[i]
		if ev.Source == AdoptMailSummarySource {
			var sum AdoptMailSummaryPayload
			if len(ev.Payload) > 0 && json.Unmarshal(ev.Payload, &sum) == nil {
				for _, id := range sum.MessageIDs {
					if id != "" {
						answered[id] = true
					}
				}
			}
			continue
		}
		var msg Message
		if len(ev.Payload) == 0 || json.Unmarshal(ev.Payload, &msg) != nil {
			continue
		}
		laneID := strings.TrimSpace(cmp.Or(msg.LaneID, ev.LaneID))
		if msg.InReplyTo != "" {
			answered[msg.InReplyTo] = true
		}
		if msg.IsClosedOrAnswered() || obsoleteLane[laneID] {
			if msg.ID != "" {
				answered[msg.ID] = true
			}
			if ev.ID != "" {
				answered[ev.ID] = true
			}
		}
		decoded = append(decoded, decodedMail{ev: ev, msg: msg})
	}
	return answered, escalatedAskID, decoded, nil
}

// OpenLaneQuestions returns all unresolved lane questions in runID ordered by arrival.
func OpenLaneQuestions(ctx context.Context, st store.Store, runID string, now time.Time) ([]OpenQuestion, error) {
	if st == nil || strings.TrimSpace(runID) == "" {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	answered, escalatedAskID, decoded, err := collectAnsweredMailState(ctx, st, runID, now)
	if err != nil || len(decoded) == 0 {
		return nil, err
	}

	seen := make(map[string]bool)
	var out []OpenQuestion
	for i := range decoded {
		ev := &decoded[i].ev
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
