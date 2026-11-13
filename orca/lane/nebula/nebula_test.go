package nebula_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
	"github.com/bazelment/yoloswe/orca/store"
)

type autoAdvanceClock struct {
	*clock.FakeClock
}

func (c autoAdvanceClock) NewTimer(d time.Duration) clock.Timer {
	t := c.FakeClock.NewTimer(d)
	c.FakeClock.Advance(d)
	return t
}

type recordedCall struct {
	Headers http.Header
	Method  string
	Path    string
	Query   string
	Body    string
}

type fakeNebulaServer struct {
	srv              *httptest.Server
	sessions         map[string]*nebula.SessionInfo
	terminals        map[string]*nebula.TerminalInfo
	reports          map[string]map[int]map[string]any
	latestRep        map[string]map[string]any
	sseEvents        map[string][]string
	calls            []recordedCall
	capacity         nebula.CapacityStatus
	nextSessSeq      int
	nextTermSeq      int
	startTerm409s    int
	steer409s        int
	stayProvisioning bool
	mu               sync.Mutex
}

func newFakeNebulaServer(t *testing.T) *fakeNebulaServer {
	t.Helper()
	f := &fakeNebulaServer{
		sessions:  make(map[string]*nebula.SessionInfo),
		terminals: make(map[string]*nebula.TerminalInfo),
		reports:   make(map[string]map[int]map[string]any),
		latestRep: make(map[string]map[string]any),
		sseEvents: make(map[string][]string),
		capacity: nebula.CapacityStatus{
			Backends: []nebula.BackendCapacity{
				{Backend: "firecracker", Running: 4, Waiting: 0, Full: false},
			},
			MaxWaitSeconds: 1800,
		},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNebulaServer) record(r *http.Request) string {
	data, _ := io.ReadAll(r.Body)
	body := string(data)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCall{
		Method:  r.Method,
		Path:    r.URL.Path,
		Query:   r.URL.RawQuery,
		Body:    body,
		Headers: r.Header.Clone(),
	})
	return body
}

func (f *fakeNebulaServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)

	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "UNAUTHORIZED", "message": "invalid token"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path

	switch {
	case r.Method == http.MethodGet && path == "/api/code-repositories":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repositories": []map[string]any{
				{"id": "repo-default", "fullName": "other/repo"},
				{"id": "repo-yoloswe", "fullName": "bazelment/yoloswe"},
			},
		})
		return

	case r.Method == http.MethodGet && path == "/api/code-sessions/capacity":
		f.mu.Lock()
		capCopy := f.capacity
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(capCopy)
		return

	case r.Method == http.MethodGet && path == "/api/code-sessions":
		laneFilter := r.URL.Query().Get("metadata.swarmLane")
		runFilter := r.URL.Query().Get("metadata.swarmRun")
		f.mu.Lock()
		var list []nebula.SessionInfo
		for _, s := range f.sessions {
			if laneFilter != "" && s.Metadata["swarmLane"] != laneFilter {
				continue
			}
			if runFilter != "" && s.Metadata["swarmRun"] != runFilter {
				continue
			}
			list = append(list, *s)
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": list})
		return

	case r.Method == http.MethodPost && path == "/api/code-sessions":
		var req struct {
			Metadata     map[string]string `json:"metadata"`
			RepositoryID string            `json:"repositoryId"`
			BaseBranch   string            `json:"baseBranch"`
			WorkBranch   string            `json:"workBranch"`
			Title        string            `json:"title"`
			SharedHome   bool              `json:"sharedHome"`
		}
		_ = json.Unmarshal([]byte(body), &req)
		f.mu.Lock()
		f.nextSessSeq++
		id := fmt.Sprintf("sess-%d", f.nextSessSeq)
		initialStatus := "ready"
		if f.stayProvisioning {
			initialStatus = "provisioning"
		}
		s := &nebula.SessionInfo{
			ID:           id,
			Status:       initialStatus,
			Title:        req.Title,
			RepositoryID: req.RepositoryID,
			BaseBranch:   req.BaseBranch,
			WorkBranch:   req.WorkBranch,
			SharedHome:   req.SharedHome,
			Metadata:     req.Metadata,
		}
		f.sessions[id] = s
		cp := *s
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(cp)
		return

	case strings.HasPrefix(path, "/api/code-sessions/"):
		rest := strings.TrimPrefix(path, "/api/code-sessions/")
		parts := strings.Split(rest, "/")
		sessID := parts[0]

		if len(parts) == 1 && r.Method == http.MethodGet {
			f.mu.Lock()
			s, ok := f.sessions[sessID]
			if !ok {
				f.mu.Unlock()
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "NOT_FOUND", "message": "session not found"})
				return
			}
			if !f.stayProvisioning && (s.Status == "provisioning" || s.Status == "resuming") {
				s.Status = "ready"
			}
			sessCopy := *s
			var terms []nebula.TerminalInfo
			for _, tm := range f.terminals {
				if tm.CodeSessionID == sessID {
					terms = append(terms, *tm)
				}
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"session":   sessCopy,
				"terminals": terms,
			})
			return
		}

		if len(parts) == 2 && parts[1] == "wake" && r.Method == http.MethodPost {
			f.mu.Lock()
			if s, ok := f.sessions[sessID]; ok {
				s.Status = "resuming"
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}

		if len(parts) == 2 && parts[1] == "stop-all" && r.Method == http.MethodPost {
			f.mu.Lock()
			for _, tm := range f.terminals {
				if tm.CodeSessionID == sessID {
					tm.Status = "stopped"
				}
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}

		if len(parts) == 2 && parts[1] == "release" && r.Method == http.MethodPost {
			f.mu.Lock()
			if s, ok := f.sessions[sessID]; ok {
				s.Status = "released"
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}

		if len(parts) == 2 && parts[1] == "terminals" && r.Method == http.MethodPost {
			f.mu.Lock()
			if f.startTerm409s > 0 {
				f.startTerm409s--
				f.mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "SESSION_NOT_READY", "message": "session is sleeping"})
				return
			}
			var req struct {
				Env             map[string]string `json:"env"`
				Kind            string            `json:"kind"`
				Model           string            `json:"model"`
				ReasoningEffort string            `json:"reasoningEffort"`
				Prompt          string            `json:"prompt"`
				Title           string            `json:"title"`
				Start           bool              `json:"start"`
			}
			_ = json.Unmarshal([]byte(body), &req)
			f.nextTermSeq++
			termID := fmt.Sprintf("term-%d", f.nextTermSeq)
			tm := &nebula.TerminalInfo{
				Env:             req.Env,
				ID:              termID,
				CodeSessionID:   sessID,
				Kind:            req.Kind,
				Title:           req.Title,
				Status:          "running",
				Model:           req.Model,
				ReasoningEffort: req.ReasoningEffort,
			}
			f.terminals[termID] = tm
			cp := *tm
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(cp)
			return
		}

	case strings.HasPrefix(path, "/api/code-terminals/"):
		rest := strings.TrimPrefix(path, "/api/code-terminals/")
		parts := strings.Split(rest, "/")
		termID := parts[0]

		if len(parts) == 1 && r.Method == http.MethodGet {
			f.mu.Lock()
			tm, ok := f.terminals[termID]
			if !ok {
				f.mu.Unlock()
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "NOT_FOUND", "message": "terminal not found"})
				return
			}
			cp := *tm
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(cp)
			return
		}

		if len(parts) == 2 && parts[1] == "phase-report" && r.Method == http.MethodGet {
			f.mu.Lock()
			var rep map[string]any
			if revStr := r.URL.Query().Get("revision"); revStr != "" {
				var rev int
				_, _ = fmt.Sscanf(revStr, "%d", &rev)
				rep = f.reports[termID][rev]
			} else {
				rep = f.latestRep[termID]
			}
			f.mu.Unlock()
			if rep == nil {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "NO_REPORT", "message": "no phase report filed"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(rep)
			return
		}

		if len(parts) == 2 && parts[1] == "input" && r.Method == http.MethodPost {
			f.mu.Lock()
			if f.steer409s > 0 {
				f.steer409s--
				f.mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "SESSION_SLEEPING", "message": "session sleeping"})
				return
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}

		if len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost {
			f.mu.Lock()
			if tm, ok := f.terminals[termID]; ok {
				tm.Status = "stopped"
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}

		if len(parts) == 3 && parts[1] == "events" && parts[2] == "stream" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			f.mu.Lock()
			frames := f.sseEvents[termID]
			f.mu.Unlock()
			for _, frame := range frames {
				_, _ = io.WriteString(w, frame)
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
	}

	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": "NOT_FOUND", "message": "unhandled route: " + path})
}

func newTestDriver(t *testing.T, srv *fakeNebulaServer, opts ...nebula.Option) (*nebula.Driver, *clock.FakeClock, *store.MemoryStore) {
	t.Helper()
	fc := clock.NewFakeClock(time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC))
	clk := autoAdvanceClock{FakeClock: fc}
	st := store.NewMemoryStore(clk)
	baseOpts := []nebula.Option{
		nebula.WithAPIURL(srv.srv.URL),
		nebula.WithToken("test-token"),
		nebula.WithRepositoryFullName("bazelment/yoloswe"),
		nebula.WithClock(clk),
		nebula.WithEventStore(st.Events()),
		nebula.WithPollInterval(100 * time.Millisecond),
		nebula.WithSettleTimeout(1 * time.Second),
	}
	baseOpts = append(baseOpts, opts...)
	drv, err := nebula.New(nebula.Config{}, baseOpts...)
	if err != nil {
		t.Fatalf("nebula.New: %v", err)
	}
	return drv, fc, st
}

func TestNewFromEnvCredentials(t *testing.T) {
	t.Setenv(nebula.EnvAPIURL, "")
	t.Setenv(nebula.EnvToken, "")
	if _, err := nebula.NewFromEnv(); !errors.Is(err, nebula.ErrMissingCredentials) {
		t.Fatalf("expected ErrMissingCredentials, got %v", err)
	}

	t.Setenv(nebula.EnvAPIURL, "https://nebula.example.com/")
	t.Setenv(nebula.EnvToken, "secret-token")
	drv, err := nebula.NewFromEnv()
	if err != nil {
		t.Fatalf("unexpected error with env credentials: %v", err)
	}
	if drv == nil {
		t.Fatal("expected non-nil Driver")
	}
}

func TestReadReport(t *testing.T) {
	srv := newFakeNebulaServer(t)
	drv, _, _ := newTestDriver(t, srv)
	ctx := context.Background()

	_, err := drv.ReadReport(ctx, "run-1", "core", "term-missing")
	if !errors.Is(err, nebula.ErrNoReport) || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNoReport/ErrNotFound, got %v", err)
	}

	reportedAt := time.Date(2026, 11, 12, 3, 25, 27, 0, time.UTC)
	srv.mu.Lock()
	srv.latestRep["term-1"] = map[string]any{
		"terminalId":  "term-1",
		"terminalUrl": "https://nebula.example.com/sessions/s1?terminalId=term-1",
		"revision":    2,
		"seq":         5,
		"outcome":     "done",
		"head":        "4b72ee6462c67a6112fae216adb3fec6403e3c86",
		"pr":          362,
		"reportedAt":  reportedAt.Format(time.RFC3339),
		"body": strings.Join([]string{
			"Outcome: done",
			"HEAD: 4b72ee6462c67a6112fae216adb3fec6403e3c86",
			"Riskiest step: sqlite lease transaction",
			"Observed: bazel test //orca/... -> 5 passed",
			"",
			"Implemented orca/ foundation and verified tests.",
		}, "\n"),
	}
	srv.reports["term-1"] = map[int]map[string]any{
		1: {
			"terminalId": "term-1",
			"revision":   1,
			"seq":        3,
			"outcome":    "needs-swe",
			"head":       "1111111111111111111111111111111111111111",
			"Breaks":     "Lint",
			"body":       "Breaks: Lint\nFix lint failure.",
		},
	}
	srv.mu.Unlock()

	rep, err := drv.ReadReport(ctx, "run-1", "core", "term-1")
	if err != nil {
		t.Fatalf("ReadReport: %v", err)
	}
	if rep.Outcome != lane.OutcomeDone {
		t.Fatalf("Outcome = %q, want %q", rep.Outcome, lane.OutcomeDone)
	}
	if rep.HeadSHA != "4b72ee6462c67a6112fae216adb3fec6403e3c86" {
		t.Fatalf("HeadSHA = %q", rep.HeadSHA)
	}
	if rep.PR != 362 {
		t.Fatalf("PR = %d, want 362", rep.PR)
	}
	if rep.LaneID != "core" || rep.TerminalID != "term-1" {
		t.Fatalf("LaneID=%q TerminalID=%q", rep.LaneID, rep.TerminalID)
	}
	if rep.RiskiestStep != "sqlite lease transaction" {
		t.Fatalf("RiskiestStep = %q", rep.RiskiestStep)
	}
	if rep.Observed != "bazel test //orca/... -> 5 passed" {
		t.Fatalf("Observed = %q", rep.Observed)
	}
	if !rep.ReportedAt.Equal(reportedAt) {
		t.Fatalf("ReportedAt = %v, want %v", rep.ReportedAt, reportedAt)
	}

	rev1, err := drv.ReadReportRevision(ctx, "run-1", "core", "term-1", 1)
	if err != nil {
		t.Fatalf("ReadReportRevision(1): %v", err)
	}
	if rev1.Outcome != lane.OutcomeNeedsSWE {
		t.Fatalf("rev1.Outcome = %q, want %q", rev1.Outcome, lane.OutcomeNeedsSWE)
	}
	if rev1.Breaks != "Lint" {
		t.Fatalf("rev1.Breaks = %q, want Lint", rev1.Breaks)
	}
}

func TestSpawnReuseWakeAndSettleTimeout(t *testing.T) {
	srv := newFakeNebulaServer(t)
	drv, _, _ := newTestDriver(t, srv)
	ctx := context.Background()

	res, err := drv.SpawnSession(ctx, lane.SpawnRequest{
		RunID:      "run-42",
		LaneID:     "nebuladriver",
		Phase:      "swe",
		Round:      1,
		Harness:    "claude",
		Model:      "claude-opus-4-6",
		Effort:     "high",
		Brief:      "Implement Nebula driver",
		Branch:     "swarm-lane/run-42/nebuladriver",
		BaseBranch: "main",
		Env:        map[string]string{"FOO": "bar"},
	})
	if err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}
	if res.SessionID != "sess-1" || res.TerminalID != "term-1" {
		t.Fatalf("SpawnResult = %+v, want sess-1 / term-1", res)
	}

	srv.mu.Lock()
	srv.sessions["sess-1"].Status = "sleeping"
	srv.mu.Unlock()

	reused, err := drv.SpawnSession(ctx, lane.SpawnRequest{
		RunID:  "run-42",
		LaneID: "nebuladriver",
		Branch: "swarm-lane/run-42/nebuladriver",
	})
	if err != nil {
		t.Fatalf("SpawnSession reuse: %v", err)
	}
	if reused.SessionID != "sess-1" {
		t.Fatalf("expected reused SessionID sess-1, got %q", reused.SessionID)
	}
	if reused.TerminalID != "" {
		t.Fatalf("expected empty TerminalID when Phase/Harness omitted, got %q", reused.TerminalID)
	}

	srv.mu.Lock()
	srv.stayProvisioning = true
	srv.mu.Unlock()

	_, err = drv.SpawnSession(ctx, lane.SpawnRequest{
		RunID:  "run-42",
		LaneID: "otherlane",
		Branch: "swarm-lane/run-42/otherlane",
	})
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("expected settle timeout error, got %v", err)
	}
}

func TestStartPhaseTerminalSteerStopAndRelease(t *testing.T) {
	srv := newFakeNebulaServer(t)
	drv, _, _ := newTestDriver(t, srv)
	ctx := context.Background()

	srv.mu.Lock()
	srv.sessions["sess-10"] = &nebula.SessionInfo{
		ID:         "sess-10",
		Status:     "ready",
		WorkBranch: "swarm-lane/run-1/lane-a",
		Metadata:   map[string]string{"swarmRun": "run-1", "swarmLane": "lane-a"},
	}
	srv.startTerm409s = 1
	srv.mu.Unlock()

	started, err := drv.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
		RunID:     "run-1",
		LaneID:    "lane-a",
		SessionID: "sess-10",
		Phase:     "review",
		Round:     2,
		Harness:   "codex",
		Model:     "gpt-5.4",
		Effort:    "high",
		Brief:     "Review PR #362",
	})
	if err != nil {
		t.Fatalf("StartPhaseTerminal: %v", err)
	}
	if started.SessionID != "sess-10" || started.TerminalID != "term-1" {
		t.Fatalf("StartPhaseResult = %+v", started)
	}

	term, err := drv.GetTerminal(ctx, "term-1")
	if err != nil {
		t.Fatalf("GetTerminal: %v", err)
	}
	if term.Kind != "codex" || term.Model != "gpt-5.4" || term.ReasoningEffort != "high" || term.Title != "lane-a review r2" {
		t.Fatalf("unexpected terminal fields: %+v", term)
	}

	srv.mu.Lock()
	srv.steer409s = 1
	srv.mu.Unlock()

	if err := drv.Steer(ctx, lane.SteerRequest{
		RunID:      "run-1",
		LaneID:     "lane-a",
		TerminalID: "term-1",
		Message:    "Check edge case in cursor advancement.",
	}); err != nil {
		t.Fatalf("Steer: %v", err)
	}

	if err := drv.Stop(ctx, lane.StopRequest{
		RunID:      "run-1",
		LaneID:     "lane-a",
		TerminalID: "term-1",
	}); err != nil {
		t.Fatalf("Stop terminal: %v", err)
	}

	if err := drv.Stop(ctx, lane.StopRequest{
		RunID:     "run-1",
		LaneID:    "lane-a",
		SessionID: "sess-10",
	}); err != nil {
		t.Fatalf("Stop session: %v", err)
	}

	if err := drv.ReleaseSession(ctx, "sess-10", true); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	srv.mu.Lock()
	status := srv.sessions["sess-10"].Status
	srv.mu.Unlock()
	if status != "released" {
		t.Fatalf("session status = %q, want released", status)
	}
}

func TestStreamEventsAndUsageCapacityIntoStore(t *testing.T) {
	srv := newFakeNebulaServer(t)
	drv, _, st := newTestDriver(t, srv)
	ctx := context.Background()

	srv.mu.Lock()
	srv.latestRep["term-99"] = map[string]any{
		"terminalId": "term-99",
		"revision":   1,
		"seq":        1,
		"outcome":    "done",
		"head":       "8942b50a2c4576b7ae4201dcad1754db65d6643b",
		"pr":         365,
		"body":       "Outcome: done\nHEAD: 8942b50a2c4576b7ae4201dcad1754db65d6643b\n\nAll tests green.",
	}
	srv.sseEvents["term-99"] = []string{
		": keepalive comment\n\n",
		"id: 1\nevent: phase.reported\ndata: {\"terminalId\":\"term-99\",\"seq\":1,\"type\":\"phase.reported\",\"outcome\":\"done\",\"at\":\"2026-11-12T03:10:00Z\"}\n\n",
		// Intermediate turn.ended with pendingSubagents > 0 should be skipped by default.
		"id: 2\nevent: turn.ended\ndata: {\"terminalId\":\"term-99\",\"seq\":2,\"type\":\"turn.ended\",\"outcome\":\"completed\",\"pendingSubagents\":2,\"at\":\"2026-11-12T03:11:00Z\"}\n\n",
		"id: 3\nevent: turn.ended\ndata: {\"terminalId\":\"term-99\",\"seq\":3,\"type\":\"turn.ended\",\"outcome\":\"completed\",\"pendingSubagents\":0,\"at\":\"2026-11-12T03:12:00Z\"}\n\n",
		"id: 4\nevent: input.waiting\ndata: {\"terminalId\":\"term-99\",\"seq\":4,\"type\":\"input.waiting\",\"eotSource\":\"hook\",\"text\":\"Which DB schema?\",\"at\":\"2026-11-12T03:13:00Z\"}\n\n",
		"id: 5\nevent: turn.capacity\ndata: {\"terminalId\":\"term-99\",\"seq\":5,\"type\":\"turn.capacity\",\"text\":\"firecracker pool full\",\"at\":\"2026-11-12T03:14:00Z\"}\n\n",
		"id: 6\nevent: terminal.exited\ndata: {\"terminalId\":\"term-99\",\"seq\":6,\"type\":\"terminal.exited\",\"outcome\":\"exited\",\"at\":\"2026-11-12T03:15:00Z\"}\n\n",
	}
	srv.sessions["sess-99"] = &nebula.SessionInfo{
		ID:     "sess-99",
		Status: "ready",
	}
	srv.terminals["term-99"] = &nebula.TerminalInfo{
		ID:            "term-99",
		CodeSessionID: "sess-99",
		Status:        "running",
		AgentSignal:   "usage_limited",
		Usage: &nebula.TerminalUsage{
			InputTokens:      12000,
			OutputTokens:     3400,
			CacheReadTokens:  80000,
			EstimatedCostUSD: 0.42,
		},
		UsageLimit: &nebula.UsageLimitInfo{
			ResetsAt: "2026-11-12T05:00:00Z",
		},
	}
	srv.capacity = nebula.CapacityStatus{
		Backends: []nebula.BackendCapacity{
			{Backend: "firecracker", Running: 80, Waiting: 3, Full: true, PoolAtMax: true},
		},
		MaxWaitSeconds: 1800,
	}
	srv.mu.Unlock()

	err := drv.StreamEvents(ctx, nebula.StreamRequest{
		RunID:      "run-stream",
		LaneID:     "nebuladriver",
		SessionID:  "sess-99",
		TerminalID: "term-99",
		Consumer:   "nebula:term-99",
	})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}

	usageEvs, err := drv.PollSessionUsage(ctx, st.Events(), "run-stream", "nebuladriver", "sess-99", "term-99")
	if err != nil {
		t.Fatalf("PollSessionUsage: %v", err)
	}
	if len(usageEvs) != 1 || !usageEvs[0].Urgent {
		t.Fatalf("unexpected usage events: %+v", usageEvs)
	}

	capEv, err := drv.PollCapacity(ctx, st.Events(), "run-stream", "nebuladriver")
	if err != nil {
		t.Fatalf("PollCapacity: %v", err)
	}
	if !capEv.Urgent || capEv.Kind != store.EventStall {
		t.Fatalf("unexpected capacity event: %+v", capEv)
	}

	evs, err := st.Events().List(ctx, store.EventFilter{RunID: "run-stream"})
	if err != nil {
		t.Fatalf("Events().List: %v", err)
	}
	// 5 SSE events (seq 2 skipped) + 1 usage + 1 capacity = 7.
	if len(evs) != 7 {
		t.Fatalf("got %d store events, want 7: %+v", len(evs), evs)
	}

	if evs[0].Kind != store.EventLaneReport || evs[0].Class != store.EventClassAuto {
		t.Fatalf("event[0] = %+v, want lane_report / auto", evs[0])
	}
	var payload0 nebula.StreamEventPayload
	if err := json.Unmarshal(evs[0].Payload, &payload0); err != nil {
		t.Fatalf("unmarshal payload0: %v", err)
	}
	if payload0.Report == nil || payload0.Report.PR != 365 || payload0.Report.Outcome != lane.OutcomeDone {
		t.Fatalf("payload0.Report = %+v", payload0.Report)
	}

	if evs[1].Kind != store.EventPhaseExit || evs[1].Class != store.EventClassAuto {
		t.Fatalf("event[1] (turn.ended) = %+v", evs[1])
	}
	if evs[2].Kind != store.EventStall || !evs[2].Urgent {
		t.Fatalf("event[2] (input.waiting) = %+v", evs[2])
	}
	if evs[3].Kind != store.EventStall || !evs[3].Urgent {
		t.Fatalf("event[3] (turn.capacity) = %+v", evs[3])
	}
	if evs[4].Kind != store.EventPhaseExit {
		t.Fatalf("event[4] (terminal.exited) = %+v", evs[4])
	}

	cur, err := st.Events().GetCursor(ctx, "run-stream", "nebula:term-99")
	if err != nil {
		t.Fatalf("GetCursor: %v", err)
	}
	if cur.Seq != 6 {
		t.Fatalf("cursor seq = %d, want 6", cur.Seq)
	}
}

func TestStreamEventsCursorScopingResumeStopAndErrorHandling(t *testing.T) {
	srv := newFakeNebulaServer(t)
	drv, _, st := newTestDriver(t, srv)
	ctx := context.Background()

	// Seed controller cursor on the same run to ensure StreamEvents never overwrites it.
	if err := st.Events().SetCursor(ctx, "run-cursor", "controller", 42); err != nil {
		t.Fatalf("SetCursor(controller): %v", err)
	}

	srv.mu.Lock()
	srv.sseEvents["term-a"] = []string{
		"event: ping\ndata: non-json-heartbeat\n\n",
		"id: 1\nevent: turn.ended\ndata: {\"terminalId\":\"term-a\",\"seq\":1,\"type\":\"turn.ended\",\"outcome\":\"completed\"}\n\n",
		// Duplicate/out-of-order seq 1 must be ignored.
		"id: 1\nevent: turn.ended\ndata: {\"terminalId\":\"term-a\",\"seq\":1,\"type\":\"turn.ended\",\"outcome\":\"completed\"}\n\n",
		"id: 2\nevent: usage.updated\ndata: {\"terminalId\":\"term-a\",\"seq\":2,\"type\":\"usage.updated\",\"text\":\"tokens\"}\n\n",
		"id: 3\nevent: phase.reported\ndata: {\"terminalId\":\"term-a\",\"seq\":3,\"type\":\"phase.reported\",\"outcome\":\"done\"}\n\n",
		"id: 4\nevent: terminal.exited\ndata: {\"terminalId\":\"term-a\",\"seq\":4,\"type\":\"terminal.exited\",\"outcome\":\"exited\"}\n\n",
	}
	srv.sseEvents["term-b"] = []string{
		"id: 1\nevent: turn.ended\ndata: {\"terminalId\":\"term-b\",\"seq\":1,\"type\":\"turn.ended\",\"outcome\":\"completed\"}\n\n",
		"id: 2\nevent: terminal.exited\ndata: {\"terminalId\":\"term-b\",\"seq\":2,\"type\":\"terminal.exited\",\"outcome\":\"exited\"}\n\n",
	}
	srv.mu.Unlock()

	// StopOnReport stops at seq=3 (where ReadReport fails because no report is filed).
	if err := drv.StreamEvents(ctx, nebula.StreamRequest{
		RunID:        "run-cursor",
		LaneID:       "lane-1",
		SessionID:    "sess-1",
		TerminalID:   "term-a",
		Consumer:     "controller",
		StopOnReport: true,
	}); err != nil {
		t.Fatalf("StreamEvents(term-a StopOnReport): %v", err)
	}

	ctrlCur, err := st.Events().GetCursor(ctx, "run-cursor", "controller")
	if err != nil {
		t.Fatalf("GetCursor(controller): %v", err)
	}
	if ctrlCur.Seq != 42 {
		t.Fatalf("controller cursor overwritten: got %d, want 42", ctrlCur.Seq)
	}

	termACur, err := st.Events().GetCursor(ctx, "run-cursor", nebula.StreamCursorConsumer("controller", "term-a"))
	if err != nil {
		t.Fatalf("GetCursor(term-a): %v", err)
	}
	if termACur.Seq != 3 {
		t.Fatalf("term-a cursor = %d, want 3", termACur.Seq)
	}

	// Resume term-a with StopOnExit: should send Last-Event-ID=3 and process only seq=4.
	if err := drv.StreamEvents(ctx, nebula.StreamRequest{
		RunID:      "run-cursor",
		LaneID:     "lane-1",
		SessionID:  "sess-1",
		TerminalID: "term-a",
		Consumer:   "controller",
		StopOnExit: true,
	}); err != nil {
		t.Fatalf("StreamEvents(term-a resume StopOnExit): %v", err)
	}

	srv.mu.Lock()
	var termALastEventIDs []string
	for _, c := range srv.calls {
		if c.Path == "/api/code-terminals/term-a/events/stream" {
			termALastEventIDs = append(termALastEventIDs, c.Headers.Get("Last-Event-ID"))
		}
	}
	srv.mu.Unlock()
	if len(termALastEventIDs) != 2 || termALastEventIDs[0] != "" || termALastEventIDs[1] != "3" {
		t.Fatalf("term-a stream Last-Event-ID headers = %v, want [\"\", \"3\"]", termALastEventIDs)
	}

	// Second terminal with the SAME Consumer name must start at seq 1 and not skip events.
	if err := drv.StreamEvents(ctx, nebula.StreamRequest{
		RunID:      "run-cursor",
		LaneID:     "lane-1",
		SessionID:  "sess-1",
		TerminalID: "term-b",
		Consumer:   "controller",
		StopOnExit: true,
	}); err != nil {
		t.Fatalf("StreamEvents(term-b StopOnExit): %v", err)
	}

	evs, err := st.Events().List(ctx, store.EventFilter{RunID: "run-cursor"})
	if err != nil {
		t.Fatalf("Events().List: %v", err)
	}
	// term-a: seq 1 (turn.ended), seq 2 (usage.updated), seq 3 (phase.reported), seq 4 (terminal.exited)
	// term-b: seq 1 (turn.ended), seq 2 (terminal.exited)
	if len(evs) != 6 {
		t.Fatalf("got %d events, want 6: %+v", len(evs), evs)
	}

	// seq 2 (usage.updated) is non-urgent telemetry and must be EventClassAuto.
	if evs[1].Class != store.EventClassAuto || evs[1].Urgent {
		t.Fatalf("usage.updated event = %+v, want Auto / non-urgent", evs[1])
	}

	// seq 3 (phase.reported with missing report) must surface ReportError and Judgment/urgent.
	var repPayload nebula.StreamEventPayload
	if err := json.Unmarshal(evs[2].Payload, &repPayload); err != nil {
		t.Fatalf("unmarshal phase.reported payload: %v", err)
	}
	if repPayload.ReportError == "" || evs[2].Class != store.EventClassJudgment || !evs[2].Urgent {
		t.Fatalf("phase.reported missing report: event=%+v payload=%+v", evs[2], repPayload)
	}
	if strings.Contains(evs[2].Summary, "reported: reported") {
		t.Fatalf("phase.reported summary synthesized invalid outcome: %q", evs[2].Summary)
	}

	// EOF before StopOnReport is satisfied must return ErrStreamEnded.
	srv.mu.Lock()
	srv.sseEvents["term-eof"] = []string{
		"id: 1\nevent: turn.ended\ndata: {\"terminalId\":\"term-eof\",\"seq\":1,\"type\":\"turn.ended\",\"outcome\":\"completed\"}\n\n",
	}
	srv.sseEvents["term-bad-json"] = []string{
		"id: 1\nevent: turn.ended\ndata: {not-valid-json\n\n",
	}
	srv.mu.Unlock()

	if err := drv.StreamEvents(ctx, nebula.StreamRequest{
		RunID:        "run-cursor",
		LaneID:       "lane-1",
		TerminalID:   "term-eof",
		StopOnReport: true,
	}); !errors.Is(err, nebula.ErrStreamEnded) {
		t.Fatalf("expected ErrStreamEnded on premature EOF, got %v", err)
	}

	if err := drv.StreamEvents(ctx, nebula.StreamRequest{
		RunID:      "run-cursor",
		LaneID:     "lane-1",
		TerminalID: "term-bad-json",
	}); err == nil || !strings.Contains(err.Error(), "nebula sse decode event") {
		t.Fatalf("expected SSE decode error on malformed JSON, got %v", err)
	}
}

type failOnceEventStore struct {
	store.EventStore
	failed bool
}

func (f *failOnceEventStore) Append(ctx context.Context, ev store.Event) (store.Event, error) {
	if !f.failed {
		f.failed = true
		return store.Event{}, errors.New("transient append error")
	}
	return f.EventStore.Append(ctx, ev)
}

func TestSessionMatchingEnvForwardingSteerAndPollDedupe(t *testing.T) {
	srv := newFakeNebulaServer(t)
	drv, _, st := newTestDriver(t, srv, nebula.WithAPIURL(srv.srv.URL))
	ctx := context.Background()

	// Seed an unrelated ready session with empty metadata/workBranch and a host session.
	t.Setenv(nebula.EnvSessionID, "sess-host")
	drvWithHost, _, _ := newTestDriver(t, srv)

	srv.mu.Lock()
	srv.sessions["sess-host"] = &nebula.SessionInfo{
		ID:         "sess-host",
		Status:     "ready",
		WorkBranch: "swarm-lane/run-x/lane-x",
		Metadata:   map[string]string{"swarmRun": "run-x", "swarmLane": "lane-x"},
	}
	srv.sessions["sess-unrelated"] = &nebula.SessionInfo{
		ID:     "sess-unrelated",
		Status: "ready",
	}
	srv.sessions["sess-wrong-base"] = &nebula.SessionInfo{
		ID:         "sess-wrong-base",
		Status:     "ready",
		BaseBranch: "develop",
		WorkBranch: "swarm-lane/run-x/lane-x",
		Metadata:   map[string]string{"swarmRun": "run-x", "swarmLane": "lane-x"},
	}
	srv.sessions["sess-wrong-run"] = &nebula.SessionInfo{
		ID:         "sess-wrong-run",
		Status:     "ready",
		BaseBranch: "main",
		WorkBranch: "swarm-lane/run-x/lane-x",
		Metadata:   map[string]string{"swarmRun": "other-run", "swarmLane": "lane-x"},
	}
	srv.mu.Unlock()

	// EnsureSession must not reuse sess-host or sess-unrelated; and concurrent calls
	// for the same lane must serialize and reuse a single newly created session.
	var wg sync.WaitGroup
	results := make([]*nebula.SessionInfo, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = drvWithHost.EnsureSession(ctx, lane.SpawnRequest{
				RunID:  "run-x",
				LaneID: "lane-x",
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureSession[%d]: %v", i, err)
		}
	}
	if results[0].ID == "sess-host" || results[0].ID == "sess-unrelated" || results[0].ID == "sess-wrong-base" || results[0].ID == "sess-wrong-run" {
		t.Fatalf("EnsureSession reused wrong session %q", results[0].ID)
	}
	if results[0].ID != results[1].ID {
		t.Fatalf("concurrent EnsureSession created distinct sessions: %q vs %q", results[0].ID, results[1].ID)
	}

	// StartPhaseTerminal with an existing SessionID must forward Env.
	started, err := drv.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
		RunID:     "run-x",
		LaneID:    "lane-x",
		SessionID: results[0].ID,
		Phase:     "swe",
		Round:     1,
		Harness:   "claude",
		Model:     "sonnet",
		Env:       map[string]string{"LANE_VAR": "val-1"},
	})
	if err != nil {
		t.Fatalf("StartPhaseTerminal: %v", err)
	}
	term, err := drv.GetTerminal(ctx, started.TerminalID)
	if err != nil {
		t.Fatalf("GetTerminal: %v", err)
	}
	if term.Env["LANE_VAR"] != "val-1" {
		t.Fatalf("terminal Env = %+v, want LANE_VAR=val-1", term.Env)
	}

	// Steer with only SessionID must locate the running terminal on that session.
	if err := drv.Steer(ctx, lane.SteerRequest{
		RunID:     "run-x",
		LaneID:    "lane-x",
		SessionID: results[0].ID,
		Message:   "steer via session id",
	}); err != nil {
		t.Fatalf("Steer(SessionID only): %v", err)
	}

	// PollSessionUsage and PollCapacity must deduplicate unchanged snapshots.
	srv.mu.Lock()
	srv.terminals[started.TerminalID].Usage = &nebula.TerminalUsage{
		InputTokens:      500,
		OutputTokens:     100,
		EstimatedCostUSD: 0.01,
	}
	srv.mu.Unlock()

	u1, err := drv.PollSessionUsage(ctx, st.Events(), "run-x", "lane-x", results[0].ID, started.TerminalID)
	if err != nil || len(u1) != 1 {
		t.Fatalf("first PollSessionUsage: len=%d err=%v", len(u1), err)
	}
	u2, err := drv.PollSessionUsage(ctx, st.Events(), "run-x", "lane-x", results[0].ID, started.TerminalID)
	if err != nil || len(u2) != 0 {
		t.Fatalf("second PollSessionUsage (unchanged) should dedupe, got len=%d err=%v", len(u2), err)
	}

	c1, err := drv.PollCapacity(ctx, st.Events(), "run-x", "lane-x")
	if err != nil || c1.ID == "" {
		t.Fatalf("first PollCapacity: ev=%+v err=%v", c1, err)
	}
	c2, err := drv.PollCapacity(ctx, st.Events(), "run-x", "lane-x")
	if err != nil || c2.ID != "" {
		t.Fatalf("second PollCapacity (unchanged) should dedupe, got ev=%+v err=%v", c2, err)
	}

	// Failed Append must not poison the dedupe map for subsequent retries.
	flakyUsageEvents := &failOnceEventStore{EventStore: st.Events()}
	if _, err := drv.PollSessionUsage(ctx, flakyUsageEvents, "run-retry", "lane-x", results[0].ID, started.TerminalID); err == nil {
		t.Fatal("expected PollSessionUsage error on first flaky Append")
	}
	uRetry, err := drv.PollSessionUsage(ctx, flakyUsageEvents, "run-retry", "lane-x", results[0].ID, started.TerminalID)
	if err != nil || len(uRetry) != 1 {
		t.Fatalf("retry PollSessionUsage after failed Append: len=%d err=%v", len(uRetry), err)
	}

	flakyCapEvents := &failOnceEventStore{EventStore: st.Events()}
	if _, err := drv.PollCapacity(ctx, flakyCapEvents, "run-retry", "lane-x"); err == nil {
		t.Fatal("expected PollCapacity error on first flaky Append")
	}
	cRetry, err := drv.PollCapacity(ctx, flakyCapEvents, "run-retry", "lane-x")
	if err != nil || cRetry.ID == "" {
		t.Fatalf("retry PollCapacity after failed Append: ev=%+v err=%v", cRetry, err)
	}
}
