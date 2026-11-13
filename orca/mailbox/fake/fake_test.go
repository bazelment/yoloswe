package fake_test

import (
	"context"
	"testing"

	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/mailbox/fake"
	"github.com/bazelment/yoloswe/orca/store"
)

func TestInboundQuestionEvent(t *testing.T) {
	s := store.NewMemoryStore(nil)
	m := fake.New(s.Events(), "r")
	ch, _ := m.Watch(context.Background())
	m.Inject(mailbox.Message{LaneID: "l", Body: "which?", Question: true})
	ev := <-ch
	if ev.Kind != store.EventMail || ev.LaneID != "l" {
		t.Fatalf("%+v", ev)
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
