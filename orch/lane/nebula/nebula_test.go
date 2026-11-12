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

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/lane"
	"github.com/bazelment/yoloswe/orch/lane/nebula"
	"github.com/bazelment/yoloswe/orch/store"
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
				Kind            string `json:"kind"`
				Model           string `json:"model"`
				ReasoningEffort string `json:"reasoningEffort"`
				Prompt          string `json:"prompt"`
				Title           string `json:"title"`
				Start           bool   `json:"start"`
			}
			_ = json.Unmarshal([]byte(body), &req)
			f.nextTermSeq++
			termID := fmt.Sprintf("term-%d", f.nextTermSeq)
			tm := &nebula.TerminalInfo{
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

	// Missing report returns ErrNoReport / store.ErrNotFound.
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
			"Observed: bazel test //orch/... -> 5 passed",
			"",
			"Implemented orch/ foundation and verified tests.",
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
	if rep.Observed != "bazel test //orch/... -> 5 passed" {
		t.Fatalf("Observed = %q", rep.Observed)
	}
	if !rep.ReportedAt.Equal(reportedAt) {
		t.Fatalf("ReportedAt = %v, want %v", rep.ReportedAt, reportedAt)
	}

	// Read specific revision where Outcome is only in top-level JSON field.
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

	// 1. Spawn a brand-new session + initial phase terminal.
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

	// 2. Put sess-1 to sleep and verify SpawnSession reuses and wakes it instead of creating sess-2.
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

	// 3. Verify settle timeout when a new session stays in provisioning past SettleTimeout.
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

	// Pre-populate a ready session.
	srv.mu.Lock()
	srv.sessions["sess-10"] = &nebula.SessionInfo{
		ID:         "sess-10",
		Status:     "ready",
		WorkBranch: "swarm-lane/run-1/lane-a",
		Metadata:   map[string]string{"swarmRun": "run-1", "swarmLane": "lane-a"},
	}
	// Inject one 409 on start terminal to verify wake-and-retry.
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

	// Steer with 409 wake-and-retry.
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

	// Stop single terminal.
	if err := drv.Stop(ctx, lane.StopRequest{
		RunID:      "run-1",
		LaneID:     "lane-a",
		TerminalID: "term-1",
	}); err != nil {
		t.Fatalf("Stop terminal: %v", err)
	}

	// Stop all terminals in session.
	if err := drv.Stop(ctx, lane.StopRequest{
		RunID:     "run-1",
		LaneID:    "lane-a",
		SessionID: "sess-10",
	}); err != nil {
		t.Fatalf("Stop session: %v", err)
	}

	// Release session with force=true.
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

	// Also poll session usage and cluster capacity into Store Events.
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
	// Expect 5 SSE events (seq 1, 3, 4, 5, 6; seq 2 skipped because pendingSubagents=2) + 1 usage event + 1 capacity event = 7 events.
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
