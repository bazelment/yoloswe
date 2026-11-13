// Package mailbox provides the transport boundary for lane and owner messages.
package mailbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bazelment/yoloswe/orca/store"
)

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
type Message struct {
	ID        string `json:"id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	LaneID    string `json:"lane_id,omitempty"`
	From      string `json:"from,omitempty"`
	Body      string `json:"body"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	Owner     bool   `json:"owner,omitempty"`
	Question  bool   `json:"question,omitempty"`
}

// Mailbox sends lane/owner messages and watches inbound messages. Watch
// appends every message to Events before delivering it on the returned channel.
type Mailbox interface {
	SendLane(context.Context, string, string) error
	AskOwner(context.Context, Question) (string, error)
	Watch(context.Context) (<-chan store.Event, error)
}

// AppendEvent persists an inbound message using the store event kind for its audience.
func AppendEvent(ctx context.Context, events store.EventStore, runID string, msg Message) (store.Event, error) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return store.Event{}, err
	}
	kind := store.EventMail
	if msg.Owner {
		kind = store.EventOwnerMessage
	}
	return events.Append(ctx, store.Event{
		RunID: runID, LaneID: msg.LaneID, Kind: kind, Source: msg.From,
		Summary: msg.Body, Payload: payload,
	})
}
