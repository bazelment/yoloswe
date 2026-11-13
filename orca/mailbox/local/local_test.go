package local_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/mailbox/local"
	"github.com/bazelment/yoloswe/orca/store"
)

func TestLocalInboundAndAsk(t *testing.T) {
	d := t.TempDir()
	s := store.NewMemoryStore(nil)
	m, err := local.New(local.Config{RunID: "r", Events: s.Events(), Dir: d, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := m.Watch(context.Background())
	os.WriteFile(filepath.Join(d, "in-1"), []byte(`{"lane_id":"l","body":"help"}`), 0600)
	select {
	case ev := <-ch:
		if ev.Summary != "help" {
			t.Fatal(ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
	if _, err := m.AskOwner(context.Background(), mailbox.Question{Ask: "pick", Default: "a"}); err != nil {
		t.Fatal(err)
	}
}
