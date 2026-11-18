package adopt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/store"
)

type shadowHTTPMailbox struct{ requests *atomic.Int64 }

func (f shadowHTTPMailbox) SendLane(context.Context, string, string) error {
	f.requests.Add(1)
	return nil
}
func (f shadowHTTPMailbox) AskOwner(context.Context, mailbox.Question) (string, error) {
	f.requests.Add(1)
	return "wait", nil
}
func (f shadowHTTPMailbox) Watch(context.Context) (<-chan store.Event, error) {
	ch := make(chan store.Event)
	close(ch)
	return ch, nil
}

func TestShadowRecordingDriverNeverReachesWriteTransport(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"state.json", "OBJECTIVE.md", "EVENTS"} {
		if err := os.WriteFile(filepath.Join(runDir, p), []byte("synthetic\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	hash := func() string {
		b, _ := os.ReadFile(filepath.Join(runDir, "state.json"))
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:])
	}
	before := hash()
	var nonGET atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			nonGET.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	rec, err := NewActionRecorder(out)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	inner := lane.NewFakeDriver()
	drv := NewRecordingDriver(inner, rec)
	st := store.NewMemoryStore(clock.RealClock{})
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "shadow", Objective: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{RunID: "shadow", ID: "needs-input", Status: lane.StatusBlocked, SessionID: "s", TerminalID: "t"}); err != nil {
		t.Fatal(err)
	}
	judge := controller.JudgeFunc(func(_ context.Context, _ string, _ []store.Event) error {
		_ = drv.Steer(context.Background(), lane.SteerRequest{RunID: "shadow", LaneID: "needs-input", Message: "nudge"})
		rec.Record("review", "merge", map[string]string{"pr": "7"}, "review pass with green CI")
		return nil
	})
	ctrl := controller.New(controller.Config{RunID: "shadow", SkipLease: true, PollInterval: 1}, st, drv, clock.RealClock{}, judge, NewRecordingMailbox(shadowHTTPMailbox{requests: &nonGET}, rec))
	if err := ctrl.Step(t.Context()); err != nil && !controller.IsTransient(err) {
		t.Fatal(err)
	}
	if err := judge.Judge(t.Context(), "shadow", []store.Event{{RunID: "shadow", LaneID: "needs-input", Kind: store.EventStall, ID: "nudge-event"}}); err != nil {
		t.Fatal(err)
	}
	if err := drv.Stop(t.Context(), lane.StopRequest{RunID: "shadow", LaneID: "needs-input", Reason: "quiet"}); err != nil {
		t.Fatal(err)
	}
	_ = drv.ReleaseSession(t.Context(), "s")
	if got := nonGET.Load(); got != 0 {
		t.Fatalf("non-GET requests=%d, want 0", got)
	}
	if got := hash(); got != before {
		t.Fatalf("run directory changed: before=%s after=%s", before, got)
	}
	b, err := os.ReadFile(filepath.Join(out, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"steer", "stop", "release", "merge"} {
		if !strings.Contains(text, "\"action\":\""+want+"\"") {
			t.Errorf("actions missing %q: %s", want, text)
		}
	}
}

func TestShadowRecorderTruncatesUnicodeArgumentsAsJSON(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	rec, err := NewActionRecorder(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Record("lane", "steer", strings.Repeat("é", 1400), "unicode"); err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var action Action
	if err := json.Unmarshal(raw, &action); err != nil {
		t.Fatalf("recorded action is invalid JSON: %v", err)
	}
	var args map[string]any
	if err := json.Unmarshal(action.Arguments, &args); err != nil {
		t.Fatalf("recorded arguments are invalid JSON: %v", err)
	}
	if len(action.Arguments) > maxShadowArgumentBytes {
		t.Fatalf("recorded arguments length=%d, want <=%d", len(action.Arguments), maxShadowArgumentBytes)
	}
	if args["truncated"] != true {
		t.Fatalf("arguments = %s, want truncated marker", action.Arguments)
	}
}

func TestRecordingMailboxAskOwnerReturnsUniqueSyntheticIDs(t *testing.T) {
	rec, err := NewActionRecorder(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	m := NewRecordingMailbox(nil, rec)
	q := mailbox.Question{LaneID: "lane", Ask: "continue?", Default: "continue with task notes"}
	first, err := m.AskOwner(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.AskOwner(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first == q.Default || second == q.Default || first == "" || second == "" {
		t.Fatalf("AskOwner IDs = %q, %q; want distinct non-default synthetic IDs", first, second)
	}
}

func TestShadowControllerDoesNotAcquireLeaseOrCheckpoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"config":{"phases":[{"name":"swe"}]},"tasks":[{"id":"lane-1","title":"work","phase":"swe","status":"planned","brief":"work"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "OBJECTIVE.md"), []byte("Goal: synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := filepath.Join(dir, "state.json")
	run, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := NewActionRecorder(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	st := store.NewMemoryStore(clock.RealClock{})
	defer st.Close()
	d := NewControllers(t.Context(), st, lane.NewFakeDriver(), clock.RealClock{}, nil)
	d.Shadow, d.Recorder = true, rec
	if err := d.Start(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Runs().Get(t.Context(), run.ID); err != nil || got.Lease != nil {
		t.Fatalf("shadow run lease=(%+v,%v), want nil", got, err)
	}
	if err := d.Stop(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(before); err != nil {
		t.Fatal(err)
	}
}

// These ticks exercise the real Nebula reads, including a session with no report
// and a recorder launch persisted just as the controller persists a launch.
func TestShadowAdoptExecReads(t *testing.T) {
	out := t.TempDir()
	rec, err := NewActionRecorder(out)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	var nonGET, fakeIDs, fakeExec atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			nonGET.Add(1)
		}
		if strings.Contains(r.URL.Path, "shadow-session-") || strings.Contains(r.URL.Path, "shadow-terminal-") {
			fakeIDs.Add(1)
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec") {
				fakeExec.Add(1)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/report") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	clk := clock.NewFakeClock(time.Time{})
	inner, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "synthetic", Clock: clk, HTTPClient: ShadowHTTPClient(nil, rec)})
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemoryStore(clk)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "exec-shadow"}); err != nil {
		t.Fatal(err)
	}
	drv := NewRecordingDriver(&adoptedRunDriver{LaneDriver: inner, store: st, run: Run{Dir: t.TempDir()}, clock: clk}, rec)
	launched, err := drv.SpawnSession(t.Context(), lane.SpawnRequest{RunID: "exec-shadow", LaneID: "launched", Phase: "swe"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range []lane.Lane{
		{RunID: "exec-shadow", ID: "adopted", SessionID: "real-session", TerminalID: "real-terminal", Phase: "swe", Status: lane.StatusRunning},
		{RunID: "exec-shadow", ID: "launched", SessionID: launched.SessionID, TerminalID: launched.TerminalID, Phase: "swe", Status: lane.StatusRunning},
	} {
		if err := st.Lanes().Upsert(t.Context(), ln); err != nil {
			t.Fatal(err)
		}
	}
	ctrl := controller.New(controller.Config{RunID: "exec-shadow", SkipLease: true}, st, drv, clk, nil)
	for tick := range 6 {
		if err := ctrl.Step(t.Context()); err != nil && !controller.IsTransient(err) {
			t.Fatal(err)
		}
		lanes, err := st.Lanes().List(t.Context(), store.LaneFilter{RunID: "exec-shadow"})
		if err != nil {
			t.Fatal(err)
		}
		for _, ln := range lanes {
			_, _ = drv.ReadReport(t.Context(), ln.RunID, ln.ID, ln.TerminalID)
			_, _ = drv.LaneGitState(t.Context(), ln)
			_, _ = drv.LaneSessionHealth(t.Context(), ln)
			_, _ = drv.TerminalAgentState(t.Context(), ln.TerminalID)
		}
		clk.Advance(time.Duration(tick+1) * time.Second)
	}
	t.Logf("6 ticks: non-GET=%d, requests with invented IDs=%d (fake exec=%d)", nonGET.Load(), fakeIDs.Load(), fakeExec.Load())
	if nonGET.Load() != 0 || fakeIDs.Load() != 0 {
		t.Fatalf("shadow escaped: non-GET=%d invented-ID requests=%d; want both 0", nonGET.Load(), fakeIDs.Load())
	}
	actions, err := readShadowActions(filepath.Join(out, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, a := range actions {
		if a.Kind == "read_degraded" {
			var args map[string]string
			if err := json.Unmarshal(a.Arguments, &args); err != nil {
				t.Fatal(err)
			}
			counts[a.Lane+"/"+args["kind"]]++
		}
	}
	for _, kind := range []string{"git_state", "session_health", "report_fallback"} {
		if counts["adopted/"+kind] != 1 {
			t.Fatalf("degraded read %s count=%d, want 1", kind, counts["adopted/"+kind])
		}
	}
	if got, err := drv.TerminalAgentState(t.Context(), launched.TerminalID); err != nil || got != "shadow-launched" {
		t.Fatalf("invented terminal=%q, %v", got, err)
	}
	if err := WriteShadowReports(t.Context(), out, t.TempDir(), "exec-shadow", st, clk.Now(), clk.Now()); err != nil {
		t.Fatal(err)
	}
	summary, err := os.ReadFile(filepath.Join(out, "summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"git_state:", "session_health:", "report_fallback:", "Refused HTTP writes", "None (0)."} {
		if !strings.Contains(string(summary), want) {
			t.Fatalf("summary missing %q", want)
		}
	}

}

func TestAdopt60LaneExecRate(t *testing.T) {
	var execs, wakes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exec") {
			execs.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/wake") {
			wakes.Add(1)
		}
		_, _ = w.Write([]byte(`{"stdout":"COMMITS:0\nDISK:30\n"}`))
	}))
	defer srv.Close()
	clk := clock.NewFakeClock(time.Time{})
	drv, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "synthetic", Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	// Four minutes, one tick every ten seconds, same ordering as controller probes.
	for range 24 {
		for i := range 60 {
			ln := lane.Lane{RunID: "rate", ID: fmt.Sprintf("lane-%d", i), SessionID: fmt.Sprintf("session-%d", i)}
			if _, err := drv.LaneGitState(t.Context(), ln); err != nil {
				t.Fatal(err)
			}
			if _, err := drv.LaneSessionHealth(t.Context(), ln); err != nil {
				t.Fatal(err)
			}
		}
		clk.Advance(10 * time.Second)
	}
	t.Logf("60 lanes / 4 minutes: exec=%d wake=%d total=%.2f/s per-lane=%.2f/min", execs.Load(), wakes.Load(), float64(execs.Load()+wakes.Load())/240, float64(execs.Load()+wakes.Load())/240)
	if got := execs.Load() + wakes.Load(); got > 240 {
		t.Fatalf("exec+wake=%d, want <=240 (1/lane/min)", got)
	}
}

func TestShadowTransportRefusesEveryNonGET(t *testing.T) {
	out := t.TempDir()
	rec, err := NewActionRecorder(out)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	var gets, writes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
		} else {
			writes.Add(1)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	client := ShadowHTTPClient(srv.Client(), rec)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, "FUTURE"} {
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+"/api/code-sessions/real/exec", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if method == http.MethodGet {
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		} else if err == nil {
			resp.Body.Close()
			t.Fatalf("%s was allowed", method)
		}
	}
	inner, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "synthetic", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if err := inner.WakeSession(t.Context(), "sleeping"); err == nil {
		t.Fatal("wake allowed")
	}
	if _, err := inner.ExecSession(t.Context(), "another", []string{"true"}); err == nil {
		t.Fatal("exec allowed")
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, srv.URL+"/api/code-sessions/real/terminals/terminal/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("nested write allowed")
	}
	if gets.Load() != 1 || writes.Load() != 0 {
		t.Fatalf("GET=%d non-GET=%d", gets.Load(), writes.Load())
	}
	st := store.NewMemoryStore(clock.RealClock{})
	defer st.Close()
	if err := WriteShadowReports(t.Context(), out, t.TempDir(), "r", st, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	summary, err := os.ReadFile(filepath.Join(out, "summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"POST /api/code-sessions/{id}/exec: 2", "POST /api/code-sessions/{id}/wake: 1", "PUT /api/code-sessions/{id}/exec: 1", "DELETE /api/code-sessions/{id}/terminals/{id}/events/stream: 1"} {
		if !strings.Contains(string(summary), want) {
			t.Fatalf("summary missing %q: %s", want, summary)
		}
	}
	// A broken recorder must still fail closed.
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if err := inner.WakeSession(t.Context(), "closed-recorder"); err == nil {
		t.Fatal("wake allowed with closed recorder")
	}
	if writes.Load() != 0 {
		t.Fatal("closed recorder allowed a write")
	}
}

func readShadowActions(path string) ([]Action, error) {
	var actions []Action
	err := walkShadowActions(path, func(a Action) error { actions = append(actions, a); return nil })
	return actions, err
}

func TestShadowSummaryRejectsMalformedAction(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "actions.jsonl"), []byte("malformed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	st := store.NewMemoryStore(clock.RealClock{})
	defer st.Close()
	if err := WriteShadowReports(t.Context(), out, t.TempDir(), "r", st, time.Now(), time.Now()); err == nil {
		t.Fatal("summary silently dropped an action")
	}
}
