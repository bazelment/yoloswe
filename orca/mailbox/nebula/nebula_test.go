package nebula_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
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

func TestAnswerReplyAndRestartCursor(t *testing.T) {
	s := store.NewMemoryStore(nil)
	var (
		mu             sync.Mutex
		answeredID     string
		answeredBody   string
		replyInReplyTo string
		replyLane      string
		replyBody      string
		seenCursors    []string
	)
	cursorCh := make(chan string, 16)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/mail/04547e4e/answer":
			var body struct {
				Answer string `json:"answer"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			answeredID = "04547e4e"
			answeredBody = body.Answer
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "04547e4e"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/mail":
			var body struct {
				LaneID    string `json:"laneId"`
				Body      string `json:"body"`
				InReplyTo string `json:"inReplyTo"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			replyLane = body.LaneID
			replyBody = body.Body
			replyInReplyTo = body.InReplyTo
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "r1"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/mail":
			cur := r.URL.Query().Get("cursor")
			mu.Lock()
			seenCursors = append(seenCursors, cur)
			mu.Unlock()
			select {
			case cursorCh <- cur:
			default:
			}
			if cur == "c-next" {
				_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}, "nextCursor": "c-next"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"messages": []any{
					map[string]any{
						"id":       "04547e4e",
						"runId":    "20261114-trial-m1",
						"lane":     "m2-gaps3",
						"kind":     "question",
						"class":    "needs-host",
						"blocking": true,
						"status":   "open",
						"body":     "Host must provide the exact Next interventions 6 and 7",
					},
				},
				"nextCursor": "c-next",
			})
		}
	}))
	defer srv.Close()

	m1, err := nebula.New(nebula.Config{BaseURL: srv.URL, Token: "x", RunID: "20261114-trial-m1", Events: s.Events(), Host: true, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second)
	ch1, err := m1.Watch(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	var firstEv store.Event
	select {
	case firstEv = <-ch1:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for first mail event")
	}
	cancel1()
	for range ch1 {
	}

	var msg mailbox.Message
	if err := json.Unmarshal(firstEv.Payload, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.LaneID != "m2-gaps3" || !msg.IsQuestion() || !msg.Blocking || msg.Cursor != "c-next" {
		t.Fatalf("unexpected decoded message: %+v", msg)
	}
	if err := mailbox.Respond(context.Background(), m1, msg, "Interventions 6 and 7"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotAnsweredID, gotAnsweredBody := answeredID, answeredBody
	mu.Unlock()
	if gotAnsweredID != "04547e4e" || gotAnsweredBody != "Interventions 6 and 7" {
		t.Fatalf("expected Answer POST, got id=%q body=%q", gotAnsweredID, gotAnsweredBody)
	}
	noteMsg := mailbox.Message{ID: "note-1", RunID: "20261114-trial-m1", LaneID: "m2-gaps3", Kind: "note", Body: "note"}
	if err := mailbox.Respond(context.Background(), m1, noteMsg, "Ack note"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotReplyInReplyTo, gotReplyLane, gotReplyBody := replyInReplyTo, replyLane, replyBody
	mu.Unlock()
	if gotReplyInReplyTo != "note-1" || gotReplyLane != "m2-gaps3" || gotReplyBody != "Ack note" {
		t.Fatalf("expected Reply POST with inReplyTo, got inReplyTo=%q lane=%q body=%q", gotReplyInReplyTo, gotReplyLane, gotReplyBody)
	}

	// Drain any buffered cursors from m1 before starting m2.
	for len(cursorCh) > 0 {
		<-cursorCh
	}

	// Simulate restart with a fresh Mailbox backed by the same EventStore.
	m2, err := nebula.New(nebula.Config{BaseURL: srv.URL, Token: "x", RunID: "20261114-trial-m1", Events: s.Events(), Host: true, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	ch2, err := m2.Watch(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case cur := <-cursorCh:
		if cur != "c-next" {
			t.Fatalf("expected restarted mailbox to poll with cursor c-next, got %q", cur)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for restarted mailbox poll")
	}
	cancel2()
	for range ch2 {
	}

	evs, _ := s.Events().List(context.Background(), store.EventFilter{RunID: "20261114-trial-m1", Kinds: []store.EventKind{store.EventMail}})
	if len(evs) != 1 {
		t.Fatalf("expected 1 mail event after restart, got %d", len(evs))
	}
	mu.Lock()
	lastCursor := ""
	if len(seenCursors) > 0 {
		lastCursor = seenCursors[len(seenCursors)-1]
	}
	mu.Unlock()
	if lastCursor != "c-next" {
		t.Fatalf("expected restarted mailbox to poll with cursor c-next, got %q", lastCursor)
	}
}

type failOnSecondAppendEventStore struct {
	store.EventStore
	mu    sync.Mutex
	calls int
}

func (f *failOnSecondAppendEventStore) Append(ctx context.Context, ev store.Event) (store.Event, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	if call == 2 {
		return store.Event{}, errors.New("simulated transient append failure")
	}
	return f.EventStore.Append(ctx, ev)
}

func TestPartialPageAppendFailureDoesNotAdvancePersistedCursor(t *testing.T) {
	base := store.NewMemoryStore(nil)
	evStore := &failOnSecondAppendEventStore{EventStore: base.Events()}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "cur-page-1" {
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}, "nextCursor": "cur-page-1"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []any{
				map[string]any{"id": "msg-1", "runId": "r-partial", "lane": "l1", "kind": "note", "body": "first"},
				map[string]any{"id": "msg-2", "runId": "r-partial", "lane": "l1", "kind": "note", "body": "second"},
			},
			"nextCursor": "cur-page-1",
		})
	}))
	defer srv.Close()

	m1, err := nebula.New(nebula.Config{BaseURL: srv.URL, Token: "x", RunID: "r-partial", Events: evStore, Host: true, PollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second)
	ch1, err := m1.Watch(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch1:
		var msg mailbox.Message
		if err := json.Unmarshal(ev.Payload, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.ID != "msg-1" || msg.Cursor != "" {
			t.Fatalf("first message before page completion must not carry nextCursor: %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for msg-1")
	}
	cancel1()
	for range ch1 {
	}

	// Simulate restart: m2 must not seed cur-page-1 from msg-1, so it fetches the page again and ingests msg-2.
	m2, err := nebula.New(nebula.Config{BaseURL: srv.URL, Token: "x", RunID: "r-partial", Events: base.Events(), Host: true, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	ch2, err := m2.Watch(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev2 := <-ch2:
		var msg2 mailbox.Message
		if err := json.Unmarshal(ev2.Payload, &msg2); err != nil {
			t.Fatal(err)
		}
		if msg2.ID != "msg-2" || msg2.Cursor != "cur-page-1" {
			t.Fatalf("expected msg-2 with cursor cur-page-1 after restart, got %+v", msg2)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for msg-2 after restart")
	}
}

func TestIDLessMessageDeduplicatedAcrossRestart(t *testing.T) {
	s := store.NewMemoryStore(nil)
	pollCount := make(chan int, 16)
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		calls++
		c := calls
		mu.Unlock()
		select {
		case pollCount <- c:
		default:
		}
		status := "open"
		answer := ""
		if c >= 2 {
			status = "answered"
			answer = "resolved"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []any{
				map[string]any{"runId": "r-idless", "lane": "l1", "kind": "question", "status": status, "answer": answer, "body": "idless body"},
			},
			"nextCursor": "cur-idless",
		})
	}))
	defer srv.Close()

	m1, err := nebula.New(nebula.Config{BaseURL: srv.URL, Token: "x", RunID: "r-idless", Events: s.Events(), Host: true, PollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second)
	ch1, err := m1.Watch(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch1:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for idless message")
	}
	cancel1()
	for range ch1 {
	}

	m2, err := nebula.New(nebula.Config{BaseURL: srv.URL, Token: "x", RunID: "r-idless", Events: s.Events(), Host: true, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	ch2, err := m2.Watch(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev, ok := <-ch2:
			if ok {
				cancel2()
				t.Fatalf("unexpected duplicate event for ID-less message on restart: %+v", ev)
			}
		case c := <-pollCount:
			if c >= 3 {
				cancel2()
				for range ch2 {
				}
				evs, _ := s.Events().List(context.Background(), store.EventFilter{RunID: "r-idless", Kinds: []store.EventKind{store.EventMail}})
				if len(evs) != 1 {
					t.Fatalf("expected 1 mail event after restart for ID-less message, got %d", len(evs))
				}
				return
			}
		case <-time.After(time.Second):
			cancel2()
			t.Fatal("timeout waiting for restarted poll")
		}
	}
}
