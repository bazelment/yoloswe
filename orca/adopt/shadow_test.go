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
	"github.com/bazelment/yoloswe/orca/model"
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
	var forwardedCommand atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			nonGET.Add(1)
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec") {
			var body struct {
				Command string `json:"command"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				forwardedCommand.Store(body.Command)
			}
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
		_, _ = w.Write([]byte(`{"stdout":"COMMITS:2\nDISK:94\nPROC:bazel test //...\n"}`))
	}))
	defer srv.Close()
	clk := clock.NewFakeClock(time.Time{})
	inner, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "synthetic", Clock: clk, HTTPClient: ShadowHTTPClient(nil, rec, clk)})
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemoryStore(clk)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "exec-shadow"}); err != nil {
		t.Fatal(err)
	}
	drv := NewRecordingDriver(inner, rec)
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
			git, gitErr := drv.LaneGitState(t.Context(), ln)
			health, healthErr := drv.LaneSessionHealth(t.Context(), ln)
			if ln.ID == "adopted" && (gitErr != nil || healthErr != nil || git.UnpushedCommits != 2 || health.DiskUsedPercent != 94 || !health.HasBusyProcess()) {
				t.Fatalf("probe git=%+v (%v), health=%+v (%v)", git, gitErr, health, healthErr)
			}
			_, _ = drv.TerminalAgentState(t.Context(), ln.TerminalID)
		}
		clk.Advance(time.Duration(tick+1) * time.Second)
	}
	t.Logf("6 ticks: non-GET=%d, requests with invented IDs=%d (fake exec=%d)", nonGET.Load(), fakeIDs.Load(), fakeExec.Load())
	if nonGET.Load() != 1 || fakeIDs.Load() != 0 {
		t.Fatalf("shadow escaped: non-GET=%d invented-ID requests=%d; want 1 read-only exec and 0 invented-ID requests", nonGET.Load(), fakeIDs.Load())
	}
	command, ok := forwardedCommand.Load().(string)
	if !ok {
		t.Fatal("shadow did not forward a lane probe")
	}
	argv, err := parseShellWords(command)
	if err != nil {
		t.Fatalf("forwarded command is not POSIX shell words: %v", err)
	}
	if len(argv) != 6 || argv[0] != "sh" || argv[1] != "-c" || argv[3] != "--" || argv[4] != "main" || argv[5] != "" {
		t.Fatalf("unquoted lane probe argv=%q, want sh -c script -- main empty-branch", argv)
	}
	actions, err := readShadowActions(filepath.Join(out, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	var recordedCommand string
	for _, a := range actions {
		if a.Kind == "exec_read" {
			var args struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(a.Arguments, &args); err != nil {
				t.Fatal(err)
			}
			recordedCommand = args.Command
		}
		if a.Kind == "read_degraded" {
			var args map[string]string
			if err := json.Unmarshal(a.Arguments, &args); err != nil {
				t.Fatal(err)
			}
			counts[a.Lane+"/"+args["kind"]]++
		}
	}
	if recordedCommand != command {
		t.Fatalf("recorded command=%q, forwarded command=%q", recordedCommand, command)
	}
	for kind, want := range map[string]int{"git_state": 0, "session_health": 0, "report_fallback": 1} {
		if counts["adopted/"+kind] != want {
			t.Fatalf("degraded read %s count=%d, want %d", kind, counts["adopted/"+kind], want)
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
	for _, want := range []string{"report_fallback:", "Refused HTTP writes", "None (0)."} {
		if !strings.Contains(string(summary), want) {
			t.Fatalf("summary missing %q", want)
		}
	}

}

func TestShadow60LaneExecRate(t *testing.T) {
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
	rec, err := NewActionRecorder(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	drv, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "synthetic", Clock: clk, HTTPClient: ShadowHTTPClient(srv.Client(), rec, clk)})
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
	if execs.Load() != 240 || wakes.Load() != 0 {
		t.Fatalf("exec=%d wake=%d, want 240 reads and 0 wakes (1/lane/min)", execs.Load(), wakes.Load())
	}
}

func TestShadowTransportRefusesUnallowlistedNonGET(t *testing.T) {
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

func TestShadowReadOnlyExecAllowlist(t *testing.T) {
	rec, err := NewActionRecorder(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	var forwarded atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		_, _ = w.Write([]byte(`{"stdout":" M file.go\n"}`))
	}))
	defer srv.Close()
	inner, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "synthetic", HTTPClient: ShadowHTTPClient(srv.Client(), rec)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inner.ExecSession(t.Context(), "read", []string{"git", "status", "--porcelain"}); err != nil {
		t.Errorf("allowlisted git status refused: %v", err)
	}
	for i, command := range [][]string{{"rm", "-rf", "/"}, {"git", "push"}, {"sh", "-c", "git status; rm -rf /"}, {"git", "log", "--output=/tmp/write"}, {"pgrep", "--pidfile=/tmp/file"}, {"cat", "/etc/passwd"}} {
		if _, err := inner.ExecSession(t.Context(), fmt.Sprintf("write-%d", i), command); err == nil {
			t.Errorf("unsafe exec forwarded: %q", command)
		}
	}
	if err := inner.WakeSession(t.Context(), "sleeping"); err == nil {
		t.Error("wake forwarded")
	}
	t.Logf("forwarded read-only execs=%d; unsafe execs and wake refused", forwarded.Load())
	if forwarded.Load() != 1 {
		t.Fatalf("forwarded=%d, want 1 read-only exec", forwarded.Load())
	}
}

func TestShadowHostDiffClassification(t *testing.T) {
	out, run := t.TempDir(), t.TempDir()
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	actions := []Action{
		{At: at, Lane: "a", Kind: "start_phase", Arguments: json.RawMessage(`{"Phase":"review"}`)},
		{At: at, Lane: "b", Kind: "stop", Arguments: json.RawMessage(`{"TerminalID":"term-b"}`)},
		{At: at, Lane: "c", Kind: "steer", Arguments: json.RawMessage(`{"TerminalID":"term-c"}`)},
		{At: at, Lane: "d", Kind: "start_phase", Arguments: json.RawMessage(`{"Phase":"review"}`)},
		{At: at, Lane: "e", Kind: "stop", Arguments: json.RawMessage(`{"TerminalID":"term-e"}`)},
	}
	f, err := os.Create(filepath.Join(out, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if err := json.NewEncoder(f).Encode(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rows := "2026-11-17T11:30:00Z\ta\tstarted\treview terminal=host-a\n" +
		"2026-11-17T12:30:00Z\tb\tstop\tterminal=term-b\n" +
		"2026-11-17T12:00:00Z\tc\tsteer\tterminal=term-c\n" +
		"2026-11-17T12:00:00Z\td\tstarted\tswe terminal=host-d\n" +
		"2026-11-17T12:00:00Z\te\tstop\tterminal=other\n" +
		"2026-11-17T12:00:00Z\tf\tdecision\tgraded soak\n" +
		"2026-11-17T12:00:00Z\tg\tfund\tbudget approved\n"
	if err := os.WriteFile(filepath.Join(run, "EVENTS"), []byte(rows), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeHostDiff(out, run, filepath.Join(out, "actions.jsonl"), at, at); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "host-diff.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	t.Logf("mechanical classifications: %v", result.Counts)
	for kind, want := range map[string]int{"agree": 1, "differs": 2, "host-did-not": 2, "orca-missed": 2} {
		if result.Counts[kind] != want {
			t.Errorf("%s=%d, want %d; %s", kind, result.Counts[kind], want, raw)
		}
	}
}

func TestHostDiffExcludesTransportObservations(t *testing.T) {
	out, run := t.TempDir(), t.TempDir()
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	actionsPath := filepath.Join(out, "actions.jsonl")
	f, err := os.Create(actionsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []Action{
		{At: at, Lane: "lane", Kind: "start_phase", Arguments: json.RawMessage(`{"phase":"swe"}`)},
		{At: at, Lane: "", Kind: "exec_read", Arguments: json.RawMessage(`{"command":"probe"}`)},
		{At: at, Lane: "lane", Kind: "read_degraded", Arguments: json.RawMessage(`{"kind":"session_health"}`)},
	} {
		if err := json.NewEncoder(f).Encode(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeHostDiff(out, run, actionsPath, at, at); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "host-diff.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result HostDiff
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Actions) != 1 || result.Actions[0].Action.Kind != "start_phase" {
		t.Fatalf("actions=%+v, want only decision action", result.Actions)
	}
}

func TestShadowTransportRateLimitAndSleepingRead(t *testing.T) {
	clk := clock.NewFakeClock(time.Time{})
	rec, err := NewActionRecorder(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	var execs, wakes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/wake") {
			wakes.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/exec") {
			execs.Add(1)
		}
		if strings.Contains(r.URL.Path, "sleeping") {
			w.WriteHeader(http.StatusConflict)
			return
		}
		_, _ = w.Write([]byte(`{"stdout":"COMMITS:1\nDISK:95\n"}`))
	}))
	defer srv.Close()
	inner, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "synthetic", Clock: clk, HTTPClient: ShadowHTTPClient(srv.Client(), rec, clk)})
	if err != nil {
		t.Fatal(err)
	}
	command := []string{"git", "status", "--porcelain"}
	if _, err := inner.ExecSession(t.Context(), "real", command); err != nil {
		t.Fatal(err)
	}
	if _, err := inner.ExecSession(t.Context(), "real", command); err == nil {
		t.Fatal("second exec inside minute allowed")
	}
	clk.Advance(time.Minute)
	if _, err := inner.ExecSession(t.Context(), "real", command); err != nil {
		t.Fatal(err)
	}
	drv := NewRecordingDriver(inner, rec)
	ln := lane.Lane{RunID: "r", ID: "sleeping", SessionID: "sleeping"}
	for range 3 {
		if _, err := drv.LaneGitState(t.Context(), ln); err == nil {
			t.Fatal("sleeping git read succeeded")
		}
		if _, err := drv.LaneSessionHealth(t.Context(), ln); err == nil {
			t.Fatal("sleeping health read succeeded")
		}
	}
	if execs.Load() != 3 || wakes.Load() != 0 {
		t.Fatalf("exec=%d wake=%d, want 3 and 0", execs.Load(), wakes.Load())
	}
	actions, err := readShadowActions(rec.path)
	if err != nil {
		t.Fatal(err)
	}
	degraded := 0
	for _, a := range actions {
		if a.Kind == "read_degraded" {
			degraded++
		}
	}
	if degraded != 2 {
		t.Fatalf("degraded=%d, want git and health once each", degraded)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := inner.ExecSession(t.Context(), "closed-recorder", command); err == nil {
		t.Fatal("read allowed without audit recorder")
	}
	if execs.Load() != 3 {
		t.Fatal("closed recorder forwarded an exec")
	}
}

func TestShadowReadOnlyCommandArguments(t *testing.T) {
	for _, command := range [][]string{
		{"git", "status", "--porcelain"}, {"git", "rev-parse", "HEAD"}, {"git", "log", "--oneline", "-n", "5"}, {"git", "rev-list", "--count", "origin/main..HEAD"}, {"git", "diff", "--stat", "origin/main"},
		{"df"}, {"df", "-P", "."}, {"pgrep", "-af", "bazel|review"}, {"cat", "/workspace/project/.git/phase-report.terminal-id.md"},
		{"cat", "/workspace/project/.git/worktrees/lane/phase-report.terminal-id.md"},
	} {
		if _, ok := shadowReadOnlyCommand(command); !ok {
			t.Errorf("read-only command refused: %q", command)
		}
	}
	for _, command := range [][]string{
		{"git", "-c", "alias.status=!rm -rf /", "status"}, {"git", "status", "--porcelain; rm -rf /"}, {"git", "diff", "--stat", "--ext-diff"}, {"git", "log", "--output=/tmp/log"}, {"git", "rev-list", "--count", "--output=/tmp/log"},
		{"df", "--output=/tmp/file"}, {"pgrep", "-F", "/tmp/file"}, {"cat", "../.git/phase-report.terminal.md"}, {"cat", "/workspace/.git/config"}, {"bash", "-c", "git status"},
	} {
		if _, ok := shadowReadOnlyCommand(command); ok {
			t.Errorf("unsafe command allowed: %q", command)
		}
	}
}

func TestHostDiffDoesNotReuseRowsOrGuessMissingTargets(t *testing.T) {
	out, run := t.TempDir(), t.TempDir()
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	actions := []Action{
		{At: at, Lane: "a", Kind: "stop", Arguments: json.RawMessage(`{"terminal_id":"other"}`)},
		{At: at, Lane: "a", Kind: "stop", Arguments: json.RawMessage(`{"terminal_id":"same"}`)},
		{At: at, Lane: "b", Kind: "steer", Arguments: json.RawMessage(`{"terminal_id":"missing"}`)},
		{At: at, Lane: "c", Kind: "start_phase", Arguments: json.RawMessage(`{"phase":"swe"}`)},
		{At: at, Lane: "d", Kind: "fund", Arguments: json.RawMessage(`{}`)},
	}
	f, err := os.Create(filepath.Join(out, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if err := json.NewEncoder(f).Encode(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rows := "2026-11-17T12:00:00Z\ta\tstop\tterminal=same\n" +
		"2026-11-17T12:00:00Z\tb\tsteer\tno target recorded\n" +
		"2026-11-17T12:30:00.000000001Z\tc\tstarted\tswe\n" +
		"2026-11-17T12:00:00Z\tother-lane\tstarted\tswe\n" +
		"2026-11-17T12:00:00Z\td\tfund\tno target recorded\n"
	if err := os.WriteFile(filepath.Join(run, "EVENTS"), []byte(rows), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeHostDiff(out, run, filepath.Join(out, "actions.jsonl"), at, at); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "host-diff.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result HostDiff
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"host-did-not", "agree", "differs", "host-did-not", "differs"} {
		if result.Actions[i].Classification != want {
			t.Errorf("action %d=%s, want %s", i, result.Actions[i].Classification, want)
		}
	}
	if result.Counts["orca-missed"] != 1 {
		t.Fatalf("counts=%v, want one host decision for other-lane", result.Counts)
	}
}

func TestSoleOperatorHostDiffCountsPostRunRows(t *testing.T) {
	out, run := t.TempDir(), t.TempDir()
	start := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	actions := []Action{
		{At: start.Add(time.Minute), Lane: "a", Kind: "start_phase", Arguments: json.RawMessage(`{"phase":"review"}`)},
		{At: start.Add(2 * time.Minute), Lane: "b", Kind: "stop", Arguments: json.RawMessage(`{"terminal_id":"term-b"}`)},
	}
	f, err := os.Create(filepath.Join(out, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if err := json.NewEncoder(f).Encode(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rows := strings.Join([]string{
		start.Add(time.Minute).Format(time.RFC3339Nano) + "\ta\tstarted\treview",
		start.Add(2*time.Minute).Format(time.RFC3339Nano) + "\tb\tstop\tterminal=term-b",
		start.Add(5*time.Minute).Format(time.RFC3339Nano) + "\tc\tstarted\tswe",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(run, "EVENTS"), []byte(rows), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeHostDiffWithWindow(out, run, filepath.Join(out, "actions.jsonl"), start, start, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "host-diff.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got HostDiff
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Matched != 2 || got.MatchedPercent < 66.6 || got.MatchedPercent > 66.7 || got.Counts["orca-missed"] != 1 {
		t.Fatalf("sole operator score = %+v", got)
	}
}

func TestRecordingDriverDeduplicatesIdenticalShadowIntents(t *testing.T) {
	out := t.TempDir()
	rec, err := NewActionRecorder(out)
	if err != nil {
		t.Fatal(err)
	}
	drv := NewRecordingDriver(nil, rec)
	req := lane.StopRequest{RunID: "run", LaneID: "lane", TerminalID: "term"}
	if err := drv.Stop(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := drv.Stop(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	actions, err := readShadowActions(filepath.Join(out, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("identical intents recorded %d times, want 1", len(actions))
	}
}

func TestRunFileSignatureWatchesLedgerAndReports(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "EVENTS"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first := runFileSignature(dir)
	if err := os.WriteFile(filepath.Join(dir, "reports", "lane.review.md"), []byte("report"), 0600); err == nil {
		t.Fatal("writing a report in a missing directory unexpectedly succeeded")
	}
	if err := os.Mkdir(filepath.Join(dir, "reports"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reports", "lane.review.md"), []byte("report"), 0600); err != nil {
		t.Fatal(err)
	}
	second := runFileSignature(dir)
	if first == second {
		t.Fatal("ledger/report change did not change watcher signature")
	}
}

func TestShadowSummaryIncludesCLITurnUsageWithoutActions(t *testing.T) {
	out := t.TempDir()
	rec, err := NewActionRecorder(out)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	clk := clock.NewFakeClock(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "usage"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Threads().StartThread(t.Context(), store.Thread{ID: "dispatcher", RunID: "usage", Role: store.ThreadRoleDispatcher}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Threads().StartTurn(t.Context(), store.Turn{ID: "turn", ThreadID: "dispatcher", RunID: "usage"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Threads().CompleteTurn(t.Context(), "dispatcher", "turn", store.TurnStatusCompleted, model.Usage{InputTokens: 100, OutputTokens: 40, CostUSD: 9}, ""); err != nil {
		t.Fatal(err)
	}
	if err := WriteShadowReports(t.Context(), out, t.TempDir(), "usage", st, clk.Now(), clk.Now()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Estimated tokens: 100 input, 40 output", "Estimated cost: $9.0000"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("summary missing %q: %s", want, raw)
		}
	}
}

func TestSessionWorkspaceDirectWritesAreIsolated(t *testing.T) {
	t.Parallel()
	live, copyDir := t.TempDir(), filepath.Join(t.TempDir(), "workspace")
	write(t, filepath.Join(live, "EVENTS"), "host event\n")
	write(t, filepath.Join(live, "env.sh"), "synthetic excluded environment\n")
	if err := SyncWorkspace(live, copyDir); err != nil {
		t.Fatal(err)
	}
	// A session edit through a link must not reach the live run on refresh.
	if err := os.Remove(filepath.Join(copyDir, "EVENTS")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(live, "EVENTS"), filepath.Join(copyDir, "EVENTS")); err != nil {
		t.Fatal(err)
	}
	if err := SyncWorkspace(live, copyDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(copyDir, "env.sh")); !os.IsNotExist(err) {
		t.Fatalf("env copied: %v", err)
	}
	info, err := os.Lstat(filepath.Join(copyDir, "EVENTS"))
	if err != nil || !info.Mode().IsRegular() || readText(filepath.Join(live, "EVENTS")) != "host event\n" {
		t.Fatalf("refresh followed a session link: %v %v", info, err)
	}
}
