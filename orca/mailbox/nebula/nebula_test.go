package nebula_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/mailbox/nebula"
	"github.com/bazelment/yoloswe/orca/store"
)

func TestHTTPMailbox(t *testing.T) {
	s := store.NewMemoryStore(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"id": "q1"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{"lane_id": "l", "body": "answer"}}})
	}))
	defer srv.Close()
	m, _ := nebula.New(nebula.Config{BaseURL: srv.URL, Token: "x", RunID: "r", Events: s.Events(), PollInterval: time.Millisecond})
	if _, err := m.AskOwner(context.Background(), mailbox.Question{Ask: "pick", Options: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ch, _ := m.Watch(ctx)
	select {
	case ev := <-ch:
		if ev.Kind != store.EventMail {
			t.Fatal(ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestHostMailboxScopesToRun(t *testing.T) {
	s := store.NewMemoryStore(nil)
	var gotBox, gotRun string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotBox, gotRun = r.URL.Query().Get("box"), r.URL.Query().Get("run")
		w.Header().Set("Content-Type", "application/json")
		// The server would return the account inbox for an unscoped request.
		// A host request is scoped by the run mailbox before it reaches us.
		json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{"id": "own", "run_id": "run", "body": "own"}}})
	}))
	defer srv.Close()
	m, err := nebula.New(nebula.Config{BaseURL: srv.URL, Token: "x", RunID: "run", Events: s.Events(), Host: true, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ch, err := m.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.Summary != "own" {
			t.Fatalf("event=%+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
	if gotBox != "host" || gotRun != "run" {
		t.Fatalf("mail scope box=%q run=%q, want host/run", gotBox, gotRun)
	}
}

func TestInvalidURLReturnsError(t *testing.T) {
	m, err := nebula.New(nebula.Config{BaseURL: ":invalid", Events: store.NewMemoryStore(nil).Events()})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SendLane(context.Background(), "lane", "hello"); err == nil {
		t.Fatal("expected an invalid URL error")
	}
}
