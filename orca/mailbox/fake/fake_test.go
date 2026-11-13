package fake_test

import (
	"context"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/mailbox/fake"
	"github.com/bazelment/yoloswe/orca/store"
)

func TestInboundQuestionEvent(t *testing.T) {
	s := store.NewMemoryStore(nil)
	m := fake.New(s.Events(), "r")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ch, _ := m.Watch(ctx)
	m.Inject(mailbox.Message{LaneID: "l", Body: "which?", Question: true})
	select {
	case ev := <-ch:
		if ev.Kind != store.EventMail || ev.LaneID != "l" {
			t.Fatalf("%+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for inbound question")
	}
}
func TestSendAndAsk(t *testing.T) {
	m := fake.New(store.NewMemoryStore(nil).Events(), "r")
	_ = m.SendLane(context.Background(), "l", "hi")
	_, _ = m.AskOwner(context.Background(), mailbox.Question{Ask: "choose", Options: []string{"a", "b"}})
	if len(m.Sent()) != 2 {
		t.Fatal(m.Sent())
	}
}
