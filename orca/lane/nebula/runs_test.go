package nebula_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bazelment/yoloswe/orca/lane/nebula"
)

func TestRunOrchestratorLabelRoundTrip(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(b))
		mu.Unlock()
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"id":"run 1","externalOrchestratorLabel":"claude","laneCount":2}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	drv, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := drv.RunOrchestrator(context.Background(), "run 1")
	if err != nil || got != "claude" {
		t.Fatalf("RunOrchestrator = %q, %v", got, err)
	}
	if err := drv.SetRunOrchestrator(context.Background(), "run 1", "orca"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"GET /api/code-runs/run 1 ", `PUT /api/code-runs/run 1 {"externalOrchestratorLabel":"orca"}`}
	if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestRunOrchestratorSession(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/code-runs/session-run":
			_, _ = io.WriteString(w, `{"id":"session-run","orchestrator":"swarm orchestrator","orchestratorSessionId":"sess-orch"}`)
		case "/api/code-runs/host-run":
			// A host (tmux) orchestrator joins no session.
			_, _ = io.WriteString(w, `{"id":"host-run","orchestrator":""}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	drv, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for run, want := range map[string]string{"session-run": "sess-orch", "host-run": ""} {
		if got, err := drv.RunOrchestratorSession(context.Background(), run); err != nil || got != want {
			t.Fatalf("%s: RunOrchestratorSession = %q, %v; want %q", run, got, err, want)
		}
	}
	if _, err := drv.RunOrchestratorSession(context.Background(), "missing"); err == nil {
		t.Fatal("missing run: want an error")
	}
}
