package nebula_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
	"github.com/bazelment/yoloswe/orca/store"
)

func githubTestDriver(t *testing.T, status int, body string, token string) (*nebula.Driver, *httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		u, _ := url.Parse(srv.URL + r.URL.Path)
		r2.URL = u
		r2.Host = u.Host
		return http.DefaultTransport.RoundTrip(r2)
	})}
	d, err := nebula.New(nebula.Config{APIURL: "http://nebula.test", Token: "nebula-token", GitHubToken: token}, nebula.WithRepositoryFullName("acme/repo"), nebula.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	return d, srv, &paths
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReportOnlyNoWork404WithoutConfirmedAuthContinues(t *testing.T) {
	d, _, paths := githubTestDriver(t, http.StatusNotFound, `{"message":"Not Found"}`, "token")
	noWork, err := d.ReportOnlyNoWork(t.Context(), lane.Lane{BaseBranch: "main", Branch: "lane-x"})
	if err == nil || noWork {
		t.Fatalf("result=(%v, %v), want error and no completion", noWork, err)
	}
	if len(*paths) != 2 {
		t.Fatalf("requests=%v, want compare plus access confirmation", *paths)
	}
}

func TestExecSessionUsesNebulaExecSchema(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		var command string
		if err := json.Unmarshal(raw["command"], &command); err != nil {
			http.Error(w, `{"code":"VALIDATION_ERROR","message":"field \"command\" must be a string"}`, http.StatusBadRequest)
			return
		}
		var timeout int
		if err := json.Unmarshal(raw["timeoutMs"], &timeout); err != nil || timeout < 1 || timeout > 30000 {
			http.Error(w, `{"code":"VALIDATION_ERROR","message":"timeoutMs out of bounds"}`, http.StatusBadRequest)
			return
		}
		seen = command
		_ = json.NewEncoder(w).Encode(nebula.ExecResult{ExitCode: 0})
	}))
	defer srv.Close()

	arrayReq, err := http.NewRequest(http.MethodPost, srv.URL+"/api/code-sessions/s/exec", strings.NewReader(`{"command":["sh","-c","true"],"timeoutMs":30000}`))
	if err != nil {
		t.Fatal(err)
	}
	arrayReq.Header.Set("Content-Type", "application/json")
	arrayResp, err := srv.Client().Do(arrayReq)
	if err != nil {
		t.Fatal(err)
	}
	arrayResp.Body.Close()
	if arrayResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("array command status=%d, want 400", arrayResp.StatusCode)
	}

	drv, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drv.ExecSession(t.Context(), "s", []string{"sh", "-c", "printf 'x'", "--", "main", "branch with spaces"}); err != nil {
		t.Fatalf("string command rejected: %v", err)
	}
	if seen != `'sh' '-c' 'printf '\''x'\''' '--' 'main' 'branch with spaces'` {
		t.Fatalf("wire command=%q, want strict POSIX quoting", seen)
	}
}

func TestReportOnlyNoWorkTimeoutContinues(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}
	d, err := nebula.New(nebula.Config{APIURL: "http://nebula.test", Token: "token"}, nebula.WithRepositoryFullName("acme/repo"), nebula.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	noWork, err := d.ReportOnlyNoWork(t.Context(), lane.Lane{BaseBranch: "main", Branch: "lane-x"})
	if err == nil || noWork {
		t.Fatalf("result=(%v, %v), want timeout error", noWork, err)
	}
}

func TestReportOnlyNoWorkRateLimitContinues(t *testing.T) {
	d, _, _ := githubTestDriver(t, http.StatusTooManyRequests, `{"message":"rate limit"}`, "token")
	noWork, err := d.ReportOnlyNoWork(t.Context(), lane.Lane{BaseBranch: "main", Branch: "lane-x"})
	if err == nil || noWork || !strings.Contains(err.Error(), "status 429") {
		t.Fatalf("result=(%v, %v), want rate limit error", noWork, err)
	}
}

func TestReportOnlyNoWorkCompareOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"branch one commit ahead", `{"ahead_by":1}`, false},
		{"explicit no change", `{"ahead_by":0}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := githubTestDriver(t, http.StatusOK, tc.body, "token")
			got, err := d.ReportOnlyNoWork(t.Context(), lane.Lane{BaseBranch: "main", Branch: "lane-x"})
			if err != nil || got != tc.want {
				t.Fatalf("result=(%v, %v), want %v", got, err, tc.want)
			}
		})
	}
}

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

type fakeNebulaServer struct { //nolint:govet // test double state is intentionally grouped
	srv              *httptest.Server
	sessions         map[string]*nebula.SessionInfo
	terminals        map[string]*nebula.TerminalInfo
	reports          map[string]map[int]map[string]any
	latestRep        map[string]map[string]any
	sseEvents        map[string][]string
	execStdout       map[string]string
	calls            []recordedCall
	capacity         nebula.CapacityStatus
	nextSessSeq      int
	nextTermSeq      int
	startTerm409s    int
	steer409s        int
	stayProvisioning bool
	spawnBaseCommit  string
	mu               sync.Mutex
}

func newFakeNebulaServer(t *testing.T) *fakeNebulaServer {
	t.Helper()
	f := &fakeNebulaServer{
		sessions:   make(map[string]*nebula.SessionInfo),
		terminals:  make(map[string]*nebula.TerminalInfo),
		reports:    make(map[string]map[int]map[string]any),
		latestRep:  make(map[string]map[string]any),
		sseEvents:  make(map[string][]string),
		execStdout: make(map[string]string),
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
			BaseCommit:   f.spawnBaseCommit,
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

		if len(parts) == 2 && parts[1] == "terminals" && r.Method == http.MethodGet {
			f.mu.Lock()
			var terms []nebula.TerminalInfo
			for _, tm := range f.terminals {
				if tm.CodeSessionID == sessID {
					terms = append(terms, *tm)
				}
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"terminals": terms})
			return
		}

		if len(parts) == 2 && parts[1] == "exec" && r.Method == http.MethodPost {
			var req struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal([]byte(body), &req)
			cmdStr := req.Command
			f.mu.Lock()
			out := f.execStdout[sessID]
			if strings.Contains(cmdStr, "DISK:") && strings.Contains(cmdStr, "COMMITS:") {
				out = f.execStdout[sessID+":git"] + f.execStdout[sessID+":health"]
			} else if strings.Contains(cmdStr, "DISK:") && f.execStdout[sessID+":health"] != "" {
				out = f.execStdout[sessID+":health"]
			} else if strings.Contains(cmdStr, "COMMITS:") && f.execStdout[sessID+":git"] != "" {
				out = f.execStdout[sessID+":git"]
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(nebula.ExecResult{Stdout: out, ExitCode: 0})
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
			if tm := f.terminals[termID]; tm != nil {
				tm.AgentState = "working"
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

func TestPhaseBriefContract(t *testing.T) {
	d, _, _ := newTestDriver(t, newFakeNebulaServer(t))
	if got := d.PhaseSkill("review"); got != "/pr-polish-nebula" {
		t.Fatalf("review skill=%q", got)
	}
	if got := d.PhaseSkill("clean"); got != "/simplify" {
		t.Fatalf("clean skill=%q", got)
	}
	if got := d.ReportInstructions("run", "lane", "review", 1); !strings.Contains(got, "--outcome <done|no-change|needs-swe|blocked|needs-input>") || !strings.Contains(got, "Reason:") {
		t.Fatalf("handback=%q", got)
	}
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

func TestEnsureRunCreatesBeforeFirstSpawn(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/code-runs/run-order":
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-repositories":
			_, _ = io.WriteString(w, `{"repositories":[{"id":"repo","fullName":"bazelment/yoloswe"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"session":{"id":"sess-1","status":"ready"}}`)
		default:
			http.Error(w, `{"message":"unexpected request"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	drv, _, _ := newTestDriver(t, &fakeNebulaServer{srv: srv}, nebula.WithAPIURL(srv.URL))
	if err := drv.EnsureRun(context.Background(), "run-order"); err != nil {
		t.Fatalf("EnsureRun: %v", err)
	}
	if _, err := drv.SpawnSession(context.Background(), lane.SpawnRequest{RunID: "run-order", LaneID: "lane-1", BaseBranch: "main", Branch: "lane-1"}); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 2 || calls[0] != "PUT /api/code-runs/run-order" {
		t.Fatalf("calls = %v, want run creation first", calls)
	}
}

func TestEnsureRunAcceptsAlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"RUN_EXISTS","message":"already exists"}`)
	}))
	defer srv.Close()
	drv, _, _ := newTestDriver(t, &fakeNebulaServer{srv: srv}, nebula.WithAPIURL(srv.URL))
	if err := drv.EnsureRun(context.Background(), "existing"); err != nil {
		t.Fatalf("EnsureRun returned error for existing run: %v", err)
	}
}

func TestEnsureRunRejectsUnrelatedConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"SESSION_NOT_READY","message":"session is transitioning"}`)
	}))
	defer srv.Close()
	drv, _, _ := newTestDriver(t, &fakeNebulaServer{srv: srv}, nebula.WithAPIURL(srv.URL))
	if err := drv.EnsureRun(context.Background(), "conflict"); err == nil {
		t.Fatal("EnsureRun accepted unrelated conflict")
	}
}

func TestEnsureRunStopsOnOtherErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code":"FORBIDDEN","message":"denied"}`)
	}))
	defer srv.Close()
	drv, _, _ := newTestDriver(t, &fakeNebulaServer{srv: srv}, nebula.WithAPIURL(srv.URL))
	err := drv.EnsureRun(context.Background(), "denied")
	if err == nil {
		t.Fatal("EnsureRun returned nil for forbidden response")
	}
	var apiErr *nebula.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("EnsureRun error = %v, want APIError status %d", err, http.StatusForbidden)
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
	if res.Branch != "swarm-lane/run-42/nebuladriver" || res.BaseBranch != "main" {
		t.Fatalf("SpawnResult branch metadata = %+v", res)
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

func TestSpawnStoresAPIBaseCommitOnly(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = rev-parse ] && [ \"$2\" = origin/main ] || exit 1\nprintf 'resolve\\n' >> git-calls\ncat origin-main\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	base := strings.Repeat("a", 40)
	moved := strings.Repeat("b", 40)
	for _, api := range []bool{true, false} {
		t.Run(map[bool]string{true: "api base wins", false: "fallback is captured once"}[api], func(t *testing.T) {
			worktree := t.TempDir()
			ref := filepath.Join(worktree, "origin-main")
			if err := os.WriteFile(ref, []byte(base), 0o600); err != nil {
				t.Fatal(err)
			}
			srv := newFakeNebulaServer(t)
			if api {
				srv.spawnBaseCommit = moved
			}
			drv, _, _ := newTestDriver(t, srv)
			res, err := drv.SpawnSession(t.Context(), lane.SpawnRequest{RunID: "base", LaneID: "git", BaseBranch: "main", Worktree: worktree})
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if api {
				want = moved
			}
			if res.BaseCommit != want {
				t.Fatalf("BaseCommit=%q, want %q", res.BaseCommit, want)
			}
			if err := os.WriteFile(ref, []byte(moved), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := drv.StartPhaseTerminal(t.Context(), lane.StartPhaseRequest{RunID: "base", LaneID: "git", SessionID: res.SessionID, Phase: "swe", Harness: "claude"}); err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(filepath.Join(worktree, "git-calls"))
			if api {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("API base must bypass git: calls=%q err=%v", calls, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("local base resolution must be skipped: calls=%q err=%v", calls, err)
			}
		})
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
		Model:     "gpt-6.1-sol",
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
	if term.Kind != "codex" || term.Model != "gpt-6.1-sol" || term.ReasoningEffort != "high" || term.Title != "lane-a review r2" {
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

func TestHostWatchFindsReportWithoutSessionMetadata(t *testing.T) {
	srv := newFakeNebulaServer(t)
	drv, _, _ := newTestDriver(t, srv, nebula.WithPollInterval(time.Millisecond))
	res, err := drv.SpawnSession(t.Context(), lane.SpawnRequest{RunID: "host-run", LaneID: "lane", Phase: "swe", Harness: "claude", Brief: "work"})
	if err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	srv.sessions[res.SessionID].Metadata = nil
	srv.latestRep[res.TerminalID] = map[string]any{
		"terminalId": res.TerminalID,
		"outcome":    "done",
		"head":       srv.sessions[res.SessionID].BaseCommit,
		"body":       "Outcome: done\nHEAD: " + srv.sessions[res.SessionID].BaseCommit,
	}
	srv.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ch := drv.Watch(ctx, "host-run")
	for {
		select {
		case ev := <-ch:
			if ev.Kind == lane.EventPhaseReported {
				if ev.LaneID != "lane" || ev.Report == nil || ev.Report.Outcome != lane.OutcomeDone {
					t.Fatalf("unexpected report event: %+v", ev)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("host watch did not surface durable phase report")
		}
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

func TestStartPhaseTerminalReusesExistingSessionID(t *testing.T) {
	t.Parallel()
	srv := newFakeNebulaServer(t)
	drv, _, _ := newTestDriver(t, srv, nebula.WithAPIURL(srv.srv.URL))
	srv.mu.Lock()
	srv.sessions["existing-session"] = &nebula.SessionInfo{ID: "existing-session", Status: "ready"}
	srv.mu.Unlock()

	started, err := drv.StartPhaseTerminal(t.Context(), lane.StartPhaseRequest{
		RunID: "run-existing", LaneID: "lane-existing", SessionID: "existing-session",
		Phase: "swe", Round: 1, Brief: "continue", Harness: "claude",
	})
	if err != nil {
		t.Fatalf("StartPhaseTerminal: %v", err)
	}
	if started.SessionID != "existing-session" {
		t.Fatalf("SessionID = %q, want existing-session", started.SessionID)
	}
	srv.mu.Lock()
	created := srv.nextSessSeq
	srv.mu.Unlock()
	if created != 0 {
		t.Fatalf("created %d new sessions while reusing existing session", created)
	}
}

func TestWatchUniformEventKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ev   nebula.StreamEventPayload
		kind store.EventKind
		want lane.EventKind
	}{
		{"phase reported", nebula.StreamEventPayload{Report: &lane.PhaseReport{Phase: "swe", Outcome: lane.OutcomeDone}}, store.EventLaneReport, lane.EventPhaseReported},
		{"turn ended", nebula.StreamEventPayload{TerminalEvent: &nebula.TerminalEvent{Type: "turn.ended"}}, store.EventPhaseExit, lane.EventTurnEndedNoReport},
		{"waiting", nebula.StreamEventPayload{TerminalEvent: &nebula.TerminalEvent{Type: "input.waiting"}}, store.EventStall, lane.EventWaitingForInput},
		{"exited", nebula.StreamEventPayload{TerminalEvent: &nebula.TerminalEvent{Type: "terminal.exited"}}, store.EventPhaseExit, lane.EventExited},
		{"quota", nebula.StreamEventPayload{TerminalEvent: &nebula.TerminalEvent{Type: "turn.capacity"}}, store.EventStall, lane.EventQuotaFailure},
		{"phase usage limit outcome", nebula.StreamEventPayload{TerminalEvent: &nebula.TerminalEvent{Outcome: "usage_limit"}}, store.EventPhaseExit, lane.EventQuotaFailure},
		{"stall capacity outcome", nebula.StreamEventPayload{TerminalEvent: &nebula.TerminalEvent{Type: "input.waiting", Outcome: "capacity"}}, store.EventStall, lane.EventQuotaFailure},
		{"usage limit payload", nebula.StreamEventPayload{UsageLimit: &nebula.UsageLimitInfo{}}, store.EventStall, lane.EventQuotaFailure},
		{"agent usage limited", nebula.StreamEventPayload{AgentSignal: "usage_limited"}, store.EventStall, lane.EventQuotaFailure},
		{"full capacity payload", nebula.StreamEventPayload{Capacity: &nebula.CapacityStatus{Backends: []nebula.BackendCapacity{{Full: true}}}}, store.EventStall, lane.EventQuotaFailure},
		{"auth", nebula.StreamEventPayload{TerminalEvent: &nebula.TerminalEvent{Type: "terminal.failed", Outcome: "auth_failed"}}, store.EventPhaseExit, lane.EventAuthFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeNebulaServer(t)
			drv, _, st := newTestDriver(t, srv, nebula.WithAPIURL(srv.srv.URL))
			payload, err := json.Marshal(tc.ev)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Events().Append(t.Context(), store.Event{RunID: "run-kinds", LaneID: "lane", Kind: tc.kind, Payload: payload, Summary: tc.name}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			select {
			case ev := <-drv.Watch(ctx, "run-kinds"):
				if ev.Kind != tc.want {
					t.Fatalf("kind=%q, want %q", ev.Kind, tc.want)
				}
			case <-time.After(time.Second):
				t.Fatal("timeout")
			}
		})
	}
}

func TestWatchIgnoresInputWaitingWhileBackgroundTaskIsCheckingIn(t *testing.T) {
	t.Parallel()
	srv := newFakeNebulaServer(t)
	drv, _, st := newTestDriver(t, srv, nebula.WithAPIURL(srv.srv.URL))
	payload, err := json.Marshal(nebula.StreamEventPayload{TerminalEvent: &nebula.TerminalEvent{
		Type:            "input.waiting",
		AgentState:      "idle",
		Text:            "checking back in 240s (task-656)\n1 task(s)\n> ",
		BackgroundTasks: json.RawMessage(`{"tasks":[{"id":"task-656"}]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Events().Append(t.Context(), store.Event{RunID: "run-waiting", LaneID: "soak", Kind: store.EventStall, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	select {
	case ev := <-drv.Watch(ctx, "run-waiting"):
		t.Fatalf("working input.waiting became lane event %q", ev.Kind)
	case <-ctx.Done():
	}
}

func TestStartPhaseTerminalStopsPreviousAndPrehandoverTerminalsAndRefusesStaleSteer(t *testing.T) {
	srv := newFakeNebulaServer(t)
	t.Setenv(nebula.EnvSessionID, "sess-host")
	drv, _, _ := newTestDriver(t, srv, nebula.WithAPIURL(srv.srv.URL))
	ctx := t.Context()

	srv.mu.Lock()
	srv.sessions["sess-stale"] = &nebula.SessionInfo{
		ID:         "sess-stale",
		Status:     "ready",
		WorkBranch: "swarm-lane/run-1/m5-finish",
		Metadata:   map[string]string{"swarmRun": "run-1", "swarmLane": "m5-finish"},
	}
	// Pre-handover terminal 19f8df22 still running idle in the session, plus shell/operator terminals
	// and a sibling terminal tagged for a different lane via SNL_GH_LANE.
	srv.terminals["19f8df22"] = &nebula.TerminalInfo{
		ID:            "19f8df22",
		CodeSessionID: "sess-stale",
		Status:        "idle",
		AgentState:    "idle",
	}
	srv.terminals["operator-shell"] = &nebula.TerminalInfo{
		ID:            "operator-shell",
		CodeSessionID: "sess-stale",
		Kind:          "shell",
		Status:        "running",
	}
	srv.terminals["operator-bash"] = &nebula.TerminalInfo{
		ID:            "operator-bash",
		CodeSessionID: "sess-stale",
		Kind:          "bash",
		Status:        "running",
	}
	srv.terminals["operator-sh"] = &nebula.TerminalInfo{
		ID:            "operator-sh",
		CodeSessionID: "sess-stale",
		Kind:          "sh",
		Status:        "running",
	}
	srv.terminals["operator-kind"] = &nebula.TerminalInfo{
		ID:            "operator-kind",
		CodeSessionID: "sess-stale",
		Kind:          "operator",
		Status:        "running",
	}
	srv.terminals["other-lane-term"] = &nebula.TerminalInfo{
		ID:            "other-lane-term",
		CodeSessionID: "sess-stale",
		Status:        "running",
		Env:           map[string]string{"SNL_GH_LANE": "other-lane"},
	}
	srv.sessions["sess-other-lane"] = &nebula.SessionInfo{
		ID:       "sess-other-lane",
		Status:   "ready",
		Metadata: map[string]string{"swarmRun": "run-1", "swarmLane": "other-lane"},
	}
	srv.terminals["other-sess-term"] = &nebula.TerminalInfo{
		ID:            "other-sess-term",
		CodeSessionID: "sess-other-lane",
		Status:        "running",
	}
	srv.sessions["sess-host"] = &nebula.SessionInfo{
		ID:       "sess-host",
		Status:   "ready",
		Metadata: map[string]string{"swarmRun": "run-1", "swarmLane": "m5-finish"},
	}
	srv.terminals["host-sess-term"] = &nebula.TerminalInfo{
		ID:            "host-sess-term",
		CodeSessionID: "sess-host",
		Status:        "running",
	}
	srv.mu.Unlock()

	cleanRes, err := drv.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
		RunID:     "run-1",
		LaneID:    "m5-finish",
		SessionID: "sess-stale",
		Phase:     "clean",
		Round:     1,
		Harness:   "claude",
		Brief:     "Clean phase",
	})
	if err != nil {
		t.Fatalf("StartPhaseTerminal(clean): %v", err)
	}

	srv.mu.Lock()
	preStatus := srv.terminals["19f8df22"].Status
	shellStatus := srv.terminals["operator-shell"].Status
	bashStatus := srv.terminals["operator-bash"].Status
	shStatus := srv.terminals["operator-sh"].Status
	opKindStatus := srv.terminals["operator-kind"].Status
	otherLaneTermStatus := srv.terminals["other-lane-term"].Status
	srv.mu.Unlock()
	if preStatus != "stopped" {
		t.Fatalf("pre-handover terminal 19f8df22 status = %q, want stopped", preStatus)
	}
	for id, got := range map[string]string{
		"operator-shell":  shellStatus,
		"operator-bash":   bashStatus,
		"operator-sh":     shStatus,
		"operator-kind":   opKindStatus,
		"other-lane-term": otherLaneTermStatus,
	} {
		if got != "running" {
			t.Fatalf("terminal %s status = %q, want running", id, got)
		}
	}

	// Starting a phase terminal on a session whose swarmLane metadata belongs to another lane,
	// or on the host session (currSessID), must not sweep unrelated terminals in that session.
	if _, err := drv.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
		RunID:     "run-1",
		LaneID:    "lane-mismatch",
		SessionID: "sess-other-lane",
		Phase:     "swe",
		Round:     1,
		Harness:   "claude",
		Brief:     "Mismatch session",
	}); err != nil {
		t.Fatalf("StartPhaseTerminal(sess-other-lane): %v", err)
	}
	if _, err := drv.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
		RunID:     "run-1",
		LaneID:    "lane-host",
		SessionID: "sess-host",
		Phase:     "swe",
		Round:     1,
		Harness:   "claude",
		Brief:     "Host session",
	}); err != nil {
		t.Fatalf("StartPhaseTerminal(sess-host): %v", err)
	}
	srv.mu.Lock()
	otherSessTermStatus := srv.terminals["other-sess-term"].Status
	hostSessTermStatus := srv.terminals["host-sess-term"].Status
	srv.mu.Unlock()
	if otherSessTermStatus != "running" {
		t.Fatalf("other-sess-term status = %q, want running", otherSessTermStatus)
	}
	if hostSessTermStatus != "running" {
		t.Fatalf("host-sess-term status = %q, want running", hostSessTermStatus)
	}

	reviewRes, err := drv.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
		RunID:     "run-1",
		LaneID:    "m5-finish",
		SessionID: "sess-stale",
		Phase:     "review",
		Round:     1,
		Harness:   "codex",
		Brief:     "Review phase",
	})
	if err != nil {
		t.Fatalf("StartPhaseTerminal(review): %v", err)
	}

	srv.mu.Lock()
	cleanStatus := srv.terminals[cleanRes.TerminalID].Status
	reviewStatus := srv.terminals[reviewRes.TerminalID].Status
	srv.mu.Unlock()
	if cleanStatus != "stopped" {
		t.Fatalf("previous phase terminal %s status = %q, want stopped", cleanRes.TerminalID, cleanStatus)
	}
	if reviewStatus != "running" {
		t.Fatalf("new phase terminal %s status = %q, want running", reviewRes.TerminalID, reviewStatus)
	}

	if err := drv.Steer(ctx, lane.SteerRequest{
		RunID:      "run-1",
		LaneID:     "m5-finish",
		SessionID:  "sess-stale",
		TerminalID: cleanRes.TerminalID,
		Message:    "input to previous phase terminal",
	}); !errors.Is(err, lane.ErrStaleTerminal) {
		t.Fatalf("Steer to previous phase terminal = %v, want ErrStaleTerminal", err)
	}
}

func TestLaneGitStateAndSessionHealth(t *testing.T) {
	t.Parallel()
	srv := newFakeNebulaServer(t)
	drv, _, _ := newTestDriver(t, srv, nebula.WithAPIURL(srv.srv.URL))
	ctx := t.Context()

	srv.mu.Lock()
	srv.sessions["sess-git"] = &nebula.SessionInfo{ID: "sess-git", Status: "ready"}
	srv.execStdout["sess-git:git"] = "COMMITS:2\nOLDEST_COMMIT:1763180000\nDIRTY:1763179000\torca/controller/controller.go\nDIRTY:1763180500\torca/lane/new.go\n"
	srv.execStdout["sess-git:health"] = "DISK:94\nPROC:review_cli\nPROC:bazel\n"
	srv.mu.Unlock()

	ln := lane.Lane{
		ID:         "lane-git",
		RunID:      "run-1",
		SessionID:  "sess-git",
		Branch:     "origin/swarm-lane/run-1/lane-git$(touch /tmp/pwn)",
		BaseBranch: "origin/main",
	}

	gs, err := drv.LaneGitState(ctx, ln)
	if err != nil {
		t.Fatalf("LaneGitState: %v", err)
	}
	if gs.UnpushedCommits != 2 || len(gs.DirtyFiles) != 2 || gs.OldestUnpushedAt != time.Unix(1763179000, 0).UTC() {
		t.Fatalf("LaneGitState = %+v, want 2 commits, 2 dirty files, oldest 1763179000", gs)
	}

	srv.mu.Lock()
	var gitCmd string
	for _, call := range srv.calls {
		if call.Method == http.MethodPost && call.Path == "/api/code-sessions/sess-git/exec" {
			var req struct {
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(call.Body), &req) == nil && req.Command != "" {
				gitCmd = req.Command
				break
			}
		}
	}
	srv.mu.Unlock()
	for _, want := range []string{"'sh' '-c'", "'--' 'main' 'swarm-lane/run-1/lane-git$(touch /tmp/pwn)'"} {
		if !strings.Contains(gitCmd, want) {
			t.Fatalf("LaneGitState exec command = %q, missing %q", gitCmd, want)
		}
	}
	if strings.Contains(strings.SplitN(gitCmd, "'--'", 2)[0], "lane-git") || strings.Contains(strings.SplitN(gitCmd, "'--'", 2)[0], "origin/origin/") {
		t.Fatalf("LaneGitState script interpolated branch/base into shell source: %s", gitCmd)
	}

	health, err := drv.LaneSessionHealth(ctx, ln)
	if err != nil {
		t.Fatalf("LaneSessionHealth: %v", err)
	}
	if health.DiskUsedPercent != 94 || len(health.BusyProcesses) != 2 {
		t.Fatalf("LaneSessionHealth = %+v, want DiskUsedPercent=94 and 2 busy processes", health)
	}
}

func TestWatchWorkingTerminalAndQuietDeduplication(t *testing.T) {
	t.Parallel()
	srv := newFakeNebulaServer(t)
	fc := clock.NewFakeClock(time.Date(2026, 11, 15, 5, 13, 0, 0, time.UTC))
	st := store.NewMemoryStore(fc)
	drv, err := nebula.New(
		nebula.Config{
			APIURL:       srv.srv.URL,
			Token:        "test-token",
			PollInterval: 2 * time.Millisecond,
		},
		nebula.WithClock(fc),
		nebula.WithEventStore(st.Events()),
	)
	if err != nil {
		t.Fatalf("nebula.New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	srv.mu.Lock()
	srv.sessions["sess-rev"] = &nebula.SessionInfo{
		ID:       "sess-rev",
		Status:   "ready",
		Metadata: map[string]string{"swarmRun": "run-watch", "swarmLane": "lane-rev"},
	}
	srv.mu.Unlock()

	res, err := drv.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
		RunID:     "run-watch",
		LaneID:    "lane-rev",
		SessionID: "sess-rev",
		Phase:     "review",
		Round:     1,
		Harness:   "codex",
		Brief:     "Review PR",
	})
	if err != nil {
		t.Fatalf("StartPhaseTerminal: %v", err)
	}

	srv.mu.Lock()
	srv.terminals[res.TerminalID].AgentState = "working"
	srv.mu.Unlock()

	ch := drv.Watch(ctx, "run-watch")

	sessionPolls := func() int {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		n := 0
		for _, c := range srv.calls {
			if c.Method == http.MethodGet && c.Path == "/api/code-sessions" {
				n++
			}
		}
		return n
	}
	waitCompletedPollsAfter := func(base, count int) {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		// When GET /api/code-sessions has been called base+count+1 times,
		// at least `count` full Watch iterations that started after `base` have completed.
		for sessionPolls() < base+count+1 {
			select {
			case <-deadline.C:
				t.Fatalf("timed out waiting for %d completed Watch polls after %d (got %d)", count, base, sessionPolls())
			default:
				time.Sleep(time.Millisecond)
			}
		}
	}

	// Over 10 polls while the terminal's AgentState is "working", Watch must emit 0 EventNoActivity events.
	for poll := 1; poll <= 10; poll++ {
		base := sessionPolls()
		fc.Advance(time.Minute)
		waitCompletedPollsAfter(base, 1)
		select {
		case ev := <-ch:
			t.Fatalf("poll %d: unexpected Watch event while working: %+v", poll, ev)
		default:
		}
	}

	// Add a second session in the same run whose terminal is idle while the first stays working:
	// Watch must still emit EventNoActivity once for the idle terminal rather than suppressing across the run.
	srv.mu.Lock()
	srv.sessions["sess-idle"] = &nebula.SessionInfo{
		ID:       "sess-idle",
		Status:   "ready",
		Metadata: map[string]string{"swarmRun": "run-watch", "swarmLane": "lane-idle"},
	}
	srv.terminals["term-idle"] = &nebula.TerminalInfo{
		ID:            "term-idle",
		CodeSessionID: "sess-idle",
		Status:        "running",
		AgentState:    "idle",
	}
	srv.mu.Unlock()
	waitCompletedPollsAfter(sessionPolls(), 1)
	fc.Advance(time.Minute)

	select {
	case ev := <-ch:
		if ev.Kind != lane.EventNoActivity {
			t.Fatalf("got event kind %q, want %q", ev.Kind, lane.EventNoActivity)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for EventNoActivity when sibling lane terminal is idle")
	}

	for poll := 1; poll <= 5; poll++ {
		base := sessionPolls()
		fc.Advance(time.Minute)
		waitCompletedPollsAfter(base, 1)
		select {
		case ev := <-ch:
			t.Fatalf("subsequent quiet poll %d emitted duplicate event: %+v", poll, ev)
		default:
		}
	}
}

func TestSteerFlushesQueuedCodexOnceAndReportsUndelivered(t *testing.T) {
	t.Parallel()
	var keys [][]string
	inputs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/code-terminals/t-1/input":
			inputs++
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "GET /api/code-terminals/t-1":
			_, _ = io.WriteString(w, `{"id":"t-1","kind":"codex","agentState":"idle","screen":"edit last queued message"}`)
		case "POST /api/code-terminals/t-1/keys":
			var body struct {
				Keys []string `json:"keys"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			keys = append(keys, body.Keys)
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	clk := autoAdvanceClock{clock.NewFakeClock(time.Time{})}
	drv, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "synthetic", Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	err = drv.Steer(t.Context(), lane.SteerRequest{RunID: "run-1", LaneID: "question", SessionID: "s-1", TerminalID: "t-1", Message: "Restore main"})
	if inputs != 1 || fmt.Sprint(keys) != "[[Esc Enter]]" || err == nil || fmt.Sprintf("%T", err) != "*lane.UndeliveredError" {
		t.Fatalf("inputs=%d keys=%v err=(%T) %v, want one input, one Esc/Enter flush and typed undelivered result", inputs, keys, err, err)
	}
}

func TestSteerConfirmsExecutionAndSelectsWithKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, harness, initial, wantKeys string
		selectOption                     int
	}{
		{name: "already working", harness: "codex", initial: "working"},
		{name: "codex queued answer", harness: "codex", initial: "idle", wantKeys: "[Esc Enter]"},
		{name: "claude picker", harness: "claude", initial: "idle", selectOption: 2, wantKeys: "[2 Enter]"},
		{name: "multi digit picker", harness: "claude", initial: "idle", selectOption: 12, wantKeys: "[1 2 Enter]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := 0
			var keys []string
			state := tc.initial
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "POST /api/code-terminals/t-1/input":
					inputs++
					_, _ = io.WriteString(w, `{}`)
				case "POST /api/code-terminals/t-1/keys":
					var body struct {
						Keys []string `json:"keys"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					keys = append(keys, body.Keys...)
					state = "working"
					_, _ = io.WriteString(w, `{}`)
				case "GET /api/code-terminals/t-1":
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "t-1", "kind": tc.harness, "agentState": state})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			clk := autoAdvanceClock{clock.NewFakeClock(time.Time{})}
			drv, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "synthetic", Clock: clk})
			if err != nil {
				t.Fatal(err)
			}
			message := "Restore main"
			wantInputs := 1
			if tc.selectOption > 0 {
				message = ""
				wantInputs = 0
			}
			if err := drv.Steer(t.Context(), lane.SteerRequest{RunID: "run-1", LaneID: "question", SessionID: "s-1", TerminalID: "t-1", Message: message, Select: tc.selectOption}); err != nil {
				t.Fatal(err)
			}
			if inputs != wantInputs || strings.Join(keys, " ") != strings.Trim(tc.wantKeys, "[]") {
				t.Fatalf("inputs=%d keys=%v, want %d %s", inputs, keys, wantInputs, tc.wantKeys)
			}
		})
	}
}

func TestSteerConfirmsResumedEventAfterFastTurn(t *testing.T) {
	t.Parallel()
	clk := autoAdvanceClock{clock.NewFakeClock(time.Time{})}
	st := store.NewMemoryStore(clk)
	defer st.Close()
	var drv *nebula.Driver
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/code-terminals/t-1/input":
			err := drv.StreamEvents(r.Context(), nebula.StreamRequest{RunID: "run-1", LaneID: "question", TerminalID: "t-1"})
			if err != nil {
				t.Error(err)
			}
			_, _ = io.WriteString(w, `{}`)
		case "GET /api/code-terminals/t-1/events/stream":
			_, _ = fmt.Fprintf(w, "id: 1\nevent: turn.resumed\ndata: {\"seq\":1,\"type\":\"turn.resumed\",\"at\":%q}\n\n", clk.Now().Format(time.RFC3339Nano))
		case "GET /api/code-terminals/t-1":
			t.Error("confirmed turn should not require terminal metadata")
			_, _ = io.WriteString(w, `{"id":"t-1","kind":"codex","agentState":"idle"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var err error
	drv, err = nebula.New(nebula.Config{APIURL: srv.URL, Token: "synthetic", Clock: clk, Events: st.Events()})
	if err != nil {
		t.Fatal(err)
	}
	if err := drv.Steer(t.Context(), lane.SteerRequest{RunID: "run-1", LaneID: "question", SessionID: "s-1", TerminalID: "t-1", Message: "Restore main"}); err != nil {
		t.Fatal(err)
	}
}

func TestSteerConfirmationBoundsHangingMetadataRequest(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/code-terminals/t-1/input":
			_, _ = io.WriteString(w, `{}`)
		case "GET /api/code-terminals/t-1":
			close(entered)
			<-r.Context().Done()
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	clk := clock.NewFakeClock(time.Time{})
	drv, err := nebula.New(nebula.Config{APIURL: srv.URL, Token: "synthetic", Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- drv.Steer(t.Context(), lane.SteerRequest{RunID: "run-1", LaneID: "question", SessionID: "s-1", TerminalID: "t-1", Message: "Restore main"})
	}()
	<-entered
	clk.Advance(lane.AnswerDeliveryWindow)
	err = <-done
	var undelivered *lane.UndeliveredError
	if !errors.As(err, &undelivered) || !errors.Is(err, context.Canceled) {
		t.Fatalf("confirmation err=%v, want typed undelivered cancellation", err)
	}
}

func TestFindPhaseTerminalAfterDriverRestart(t *testing.T) {
	srv := newFakeNebulaServer(t)
	drv, _, _ := newTestDriver(t, srv)
	req := lane.StartPhaseRequest{RunID: "run", LaneID: "lane", Phase: "swe", Round: 1, ActionKey: "phase.retry:lane:swe:attempt-2"}
	started, err := drv.StartPhaseTerminal(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	restarted, _, _ := newTestDriver(t, srv)
	found, err := restarted.FindPhaseTerminal(t.Context(), req)
	if err != nil || found == nil || found.TerminalID != started.TerminalID || found.SessionID != started.SessionID {
		t.Fatalf("found=%+v err=%v; want %+v", found, err, started)
	}
	stale := req
	stale.SessionID = "stale-session"
	recovered, err := restarted.FindPhaseTerminal(t.Context(), stale)
	if err != nil || recovered == nil || recovered.TerminalID != started.TerminalID {
		t.Fatalf("stale session recovery=%+v err=%v", recovered, err)
	}
	for _, field := range []string{"attempt", "round", "phase", "run"} {
		t.Run(field, func(t *testing.T) {
			other := req
			switch field {
			case "attempt":
				other.ActionKey = "phase.retry:lane:swe:attempt-3"
			case "round":
				other.Round = 2
			case "phase":
				other.Phase = "review"
			case "run":
				other.RunID = "another-run"
				other.SessionID = started.SessionID
			}
			got, err := restarted.FindPhaseTerminal(t.Context(), other)
			if err != nil || got != nil {
				t.Fatalf("mismatched launch found=%+v err=%v", got, err)
			}
		})
	}
	if err := drv.Stop(t.Context(), lane.StopRequest{RunID: "run", LaneID: "lane", SessionID: started.SessionID, TerminalID: started.TerminalID}); err != nil {
		t.Fatal(err)
	}
	found, err = restarted.FindPhaseTerminal(t.Context(), req)
	if err != nil || found != nil {
		t.Fatalf("stopped launch found=%+v err=%v", found, err)
	}
}

func TestLaneProbeBackoffAndInvalidation(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			fc := clock.NewFakeClock(time.Time{})
			fc.Set(time.Time{})
			client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stdout":"COMMITS:2\nDISK:94\nPROC:bazel test //orca/...\n"}`)), Request: r}, nil
			})}
			d, err := nebula.New(nebula.Config{APIURL: "http://synthetic.test", Token: "synthetic", Clock: fc, HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			ln := lane.Lane{RunID: "r", ID: "lane", SessionID: "s", TerminalID: "t"}
			var wg sync.WaitGroup
			for range 20 {
				wg.Go(func() {
					git, gitErr := d.LaneGitState(t.Context(), ln)
					health, healthErr := d.LaneSessionHealth(t.Context(), ln)
					if status == http.StatusOK {
						if gitErr != nil || healthErr != nil || git.UnpushedCommits != 2 || health.DiskUsedPercent != 94 {
							t.Errorf("git=%+v/%v health=%+v/%v", git, gitErr, health, healthErr)
						}
					} else if gitErr == nil || healthErr == nil {
						t.Error("failed probe returned known state")
					}
				})
			}
			wg.Wait()
			if calls != 1 {
				t.Fatalf("concurrent calls=%d, want 1", calls)
			}
			stats := d.SessionCallCounts()
			if len(stats) != 1 || stats[0].Lane != "r/lane" || stats[0].Exec != 1 || stats[0].Wake != 0 {
				t.Fatalf("stats=%+v", stats)
			}
			fc.Advance(time.Minute - time.Nanosecond)
			_, _ = d.LaneGitState(t.Context(), ln)
			if calls != 1 {
				t.Fatalf("early retry: %d calls", calls)
			}
			fc.Advance(time.Nanosecond)
			_, _ = d.LaneSessionHealth(t.Context(), ln)
			if calls != 2 {
				t.Fatalf("expired cache: %d calls", calls)
			}
			ln.TerminalID = "new-phase"
			if _, err := d.LaneGitState(t.Context(), ln); err == nil {
				t.Fatal("new phase returned stale state")
			}
			if calls != 2 {
				t.Fatalf("new phase bypassed rate limit: %d calls", calls)
			}
			fc.Advance(time.Minute)
			_, _ = d.LaneGitState(t.Context(), ln)
			if calls != 3 {
				t.Fatalf("new phase not refreshed after interval: %d calls", calls)
			}
			if err := d.WakeSession(t.Context(), "s"); status == http.StatusOK && err != nil {
				t.Fatal(err)
			}
			stats = d.SessionCallCounts()
			if len(stats) != 1 || stats[0].Exec != 1 || stats[0].Wake != 1 {
				t.Fatalf("stats=%+v", stats)
			}
		})
	}
}
