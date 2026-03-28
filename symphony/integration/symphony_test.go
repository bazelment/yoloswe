//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/bazelbuild/rules_go/go/tools/bazel"

	"github.com/bazelment/yoloswe/symphony/config"
	symphttp "github.com/bazelment/yoloswe/symphony/http"
	"github.com/bazelment/yoloswe/symphony/model"
	"github.com/bazelment/yoloswe/symphony/orchestrator"
)

// ---- Mock Tracker ----

type mockTracker struct {
	mu             sync.Mutex
	candidates     []model.Issue
	issueStates    map[string]model.Issue
	fetchCount     atomic.Int64
	stateRefreshFn func(ids []string) ([]model.Issue, error)
}

func newMockTracker() *mockTracker {
	return &mockTracker{
		issueStates: make(map[string]model.Issue),
	}
}

func (m *mockTracker) SetCandidates(issues []model.Issue) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.candidates = issues
	for _, issue := range issues {
		m.issueStates[issue.ID] = issue
	}
}

func (m *mockTracker) UpdateState(issueID, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if issue, ok := m.issueStates[issueID]; ok {
		issue.State = state
		m.issueStates[issueID] = issue
	}
}

func (m *mockTracker) FetchCandidateIssues(_ context.Context, activeStates []string, _ string) ([]model.Issue, error) {
	m.fetchCount.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()

	activeSet := make(map[string]bool)
	for _, s := range activeStates {
		activeSet[model.NormalizeState(s)] = true
	}

	var result []model.Issue
	for _, issue := range m.candidates {
		if activeSet[model.NormalizeState(issue.State)] {
			result = append(result, issue)
		}
	}
	return result, nil
}

func (m *mockTracker) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]model.Issue, error) {
	if m.stateRefreshFn != nil {
		return m.stateRefreshFn(ids)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []model.Issue
	for _, id := range ids {
		if issue, ok := m.issueStates[id]; ok {
			result = append(result, issue)
		}
	}
	return result, nil
}

func (m *mockTracker) FetchIssuesByStates(_ context.Context, _ []string) ([]model.Issue, error) {
	return nil, nil
}

// ---- Fake Codex Binary ----

// fakeCodexPath returns the path to the pre-built fake codex binary.
// When running under Bazel, uses runfiles. Falls back to go build for direct execution.
func fakeCodexPath(t *testing.T) string {
	t.Helper()
	bin, ok := bazel.FindBinary("symphony/integration/testdata", "fake_codex")
	if ok {
		return bin
	}
	t.Fatal("fake_codex binary not found in runfiles; run via bazel test")
	return ""
}

// ---- Test Helpers ----

func makeTestConfig(codexBin, workspaceRoot string) func() *config.ServiceConfig {
	return func() *config.ServiceConfig {
		return config.NewServiceConfig(&model.WorkflowDefinition{
			Config: map[string]any{
				"tracker": map[string]any{
					"kind":           "linear",
					"api_key":        "test-api-key",
					"project_slug":   "TEST",
					"active_states":  []any{"Todo", "In Progress"},
					"terminal_states": []any{"Done", "Cancelled"},
				},
				"polling": map[string]any{
					"interval_ms": 500, // Fast polling for tests
				},
				"agent": map[string]any{
					"max_concurrent_agents": 2,
					"max_turns":             3,
					"max_retry_backoff_ms":  5000,
				},
				"codex": map[string]any{
					"command":          codexBin,
					"turn_timeout_ms":  10000,
					"read_timeout_ms":  5000,
					"stall_timeout_ms": 60000,
				},
				"workspace": map[string]any{
					"root": workspaceRoot,
				},
			},
		})
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// ---- Tests ----

// TestDispatchAndComplete verifies the full lifecycle:
// poll -> dispatch -> agent runs -> worker completes -> tokens aggregated
func TestDispatchAndComplete(t *testing.T) {
	codexBin := fakeCodexPath(t)
	workspaceRoot := t.TempDir()

	tracker := newMockTracker()
	p1 := 1
	tracker.SetCandidates([]model.Issue{
		{
			ID:         "issue-1",
			Identifier: "TEST-1",
			Title:      "First test issue",
			State:      "Todo",
			Priority:   &p1,
		},
	})

	cfgFn := makeTestConfig(codexBin, workspaceRoot)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orch := orchestrator.New(cfgFn, tracker, orchestrator.RealClock{}, logger)

	// Start HTTP server for snapshot access.
	httpSrv := symphttp.NewServer(orch, 0, logger)
	require.NoError(t, httpSrv.Start())
	defer httpSrv.Shutdown(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orchDone := make(chan error, 1)
	go func() {
		orchDone <- orch.Run(ctx)
	}()

	// Wait for the tracker to be polled at least once.
	waitForCondition(t, 5*time.Second, "tracker polled", func() bool {
		return tracker.fetchCount.Load() > 0
	})

	// Wait for the worker to complete (issue should leave running state).
	// The fake codex completes after 1 turn, so the worker should exit quickly.
	waitForCondition(t, 15*time.Second, "worker completed", func() bool {
		snap, err := orch.RequestSnapshot(ctx)
		if err != nil {
			return false
		}
		// Running should be empty once the worker finishes.
		return len(snap.Running) == 0 && snap.Totals.TotalTokens > 0
	})

	// Take a final snapshot and verify token accounting.
	snap, err := orch.RequestSnapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, len(snap.Running), "no workers should be running")
	require.Greater(t, snap.Totals.TotalTokens, int64(0), "should have accumulated tokens")
	require.Greater(t, snap.Totals.InputTokens, int64(0))
	require.Greater(t, snap.Totals.OutputTokens, int64(0))
	require.Greater(t, snap.Totals.SecondsRunning, float64(0))

	// Verify HTTP API returns valid JSON.
	resp, err := http.Get("http://" + httpSrv.Addr() + "/api/v1/state")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	var stateResp map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&stateResp))
	require.Contains(t, stateResp, "generated_at")
	require.Contains(t, stateResp, "codex_totals")

	cancel()
	select {
	case err := <-orchDone:
		// context.Canceled is expected.
		if err != nil && err != context.Canceled {
			t.Fatalf("orchestrator error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("orchestrator did not shut down in time")
	}
}

// TestConcurrencyLimit verifies that max_concurrent_agents is respected.
func TestConcurrencyLimit(t *testing.T) {
	codexBin := fakeCodexPath(t)
	workspaceRoot := t.TempDir()

	tracker := newMockTracker()
	p1 := 1
	// Set 5 candidates but max_concurrent is 2.
	var issues []model.Issue
	for i := 1; i <= 5; i++ {
		issues = append(issues, model.Issue{
			ID:         fmt.Sprintf("issue-%d", i),
			Identifier: fmt.Sprintf("TEST-%d", i),
			Title:      fmt.Sprintf("Issue %d", i),
			State:      "Todo",
			Priority:   &p1,
		})
	}
	tracker.SetCandidates(issues)

	cfgFn := makeTestConfig(codexBin, workspaceRoot)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orch := orchestrator.New(cfgFn, tracker, orchestrator.RealClock{}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orchDone := make(chan error, 1)
	go func() {
		orchDone <- orch.Run(ctx)
	}()

	// The fake codex with SLOW_MODE takes ~2s per turn. Check that at peak we have <= 2 running.
	// We need the fake codex to be slow for this test.
	// Set env var that the fake codex reads.
	t.Setenv("FAKE_CODEX_SLOW", "true")

	maxSeen := int64(0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40; i++ {
			snap, err := orch.RequestSnapshot(ctx)
			if err != nil {
				continue
			}
			running := int64(len(snap.Running))
			for {
				old := atomic.LoadInt64(&maxSeen)
				if running <= old || atomic.CompareAndSwapInt64(&maxSeen, old, running) {
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	<-done

	require.LessOrEqual(t, maxSeen, int64(2), "should never exceed max_concurrent_agents=2")

	cancel()
	<-orchDone
}

// TestReconcileTerminal verifies that when an issue moves to terminal state,
// the orchestrator cleans up the workspace.
func TestReconcileTerminal(t *testing.T) {
	codexBin := fakeCodexPath(t)
	workspaceRoot := t.TempDir()

	tracker := newMockTracker()
	p1 := 1
	tracker.SetCandidates([]model.Issue{
		{
			ID:         "issue-term",
			Identifier: "TEST-TERM",
			Title:      "Terminal test",
			State:      "In Progress",
			Priority:   &p1,
		},
	})

	// Make the fake codex slow so we have time to change state.
	t.Setenv("FAKE_CODEX_SLOW", "true")

	cfgFn := makeTestConfig(codexBin, workspaceRoot)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orch := orchestrator.New(cfgFn, tracker, orchestrator.RealClock{}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orchDone := make(chan error, 1)
	go func() {
		orchDone <- orch.Run(ctx)
	}()

	// Wait for the issue to be dispatched and running.
	waitForCondition(t, 10*time.Second, "issue running", func() bool {
		snap, err := orch.RequestSnapshot(ctx)
		if err != nil {
			return false
		}
		return len(snap.Running) == 1
	})

	// Move issue to terminal state in the tracker.
	tracker.UpdateState("issue-term", "Done")

	// Wait for reconciliation to clean up (orchestrator should detect terminal state).
	waitForCondition(t, 15*time.Second, "issue cleaned up after terminal", func() bool {
		snap, err := orch.RequestSnapshot(ctx)
		if err != nil {
			return false
		}
		return len(snap.Running) == 0
	})

	cancel()
	<-orchDone
}

// TestHTTPRefresh verifies POST /api/v1/refresh triggers an immediate poll.
func TestHTTPRefresh(t *testing.T) {
	codexBin := fakeCodexPath(t)
	workspaceRoot := t.TempDir()

	tracker := newMockTracker()
	// Start with no candidates.
	tracker.SetCandidates(nil)

	cfgFn := func() *config.ServiceConfig {
		cfg := makeTestConfig(codexBin, workspaceRoot)()
		cfg.PollIntervalMs = 60000 // Very slow polling.
		return cfg
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orch := orchestrator.New(cfgFn, tracker, orchestrator.RealClock{}, logger)

	httpSrv := symphttp.NewServer(orch, 0, logger)
	require.NoError(t, httpSrv.Start())
	defer httpSrv.Shutdown(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orchDone := make(chan error, 1)
	go func() {
		orchDone <- orch.Run(ctx)
	}()

	// Wait for initial tick.
	waitForCondition(t, 5*time.Second, "initial tick", func() bool {
		return tracker.fetchCount.Load() > 0
	})

	beforeCount := tracker.fetchCount.Load()

	// Trigger refresh via HTTP.
	resp, err := http.Post("http://"+httpSrv.Addr()+"/api/v1/refresh", "", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)

	// Wait for the refresh to trigger a new fetch.
	waitForCondition(t, 5*time.Second, "refresh triggered new fetch", func() bool {
		return tracker.fetchCount.Load() > beforeCount
	})

	cancel()
	<-orchDone
}

// TestIssueEndpoint verifies GET /api/v1/{identifier} returns issue details.
func TestIssueEndpoint(t *testing.T) {
	codexBin := fakeCodexPath(t)
	workspaceRoot := t.TempDir()

	tracker := newMockTracker()
	p1 := 1
	tracker.SetCandidates([]model.Issue{
		{
			ID:         "issue-api",
			Identifier: "TEST-API",
			Title:      "API test issue",
			State:      "Todo",
			Priority:   &p1,
		},
	})

	// Slow codex so the issue stays running while we query.
	t.Setenv("FAKE_CODEX_SLOW", "true")

	cfgFn := makeTestConfig(codexBin, workspaceRoot)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orch := orchestrator.New(cfgFn, tracker, orchestrator.RealClock{}, logger)

	httpSrv := symphttp.NewServer(orch, 0, logger)
	require.NoError(t, httpSrv.Start())
	defer httpSrv.Shutdown(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orchDone := make(chan error, 1)
	go func() {
		orchDone <- orch.Run(ctx)
	}()

	// Wait for issue to be running.
	waitForCondition(t, 10*time.Second, "issue running", func() bool {
		snap, err := orch.RequestSnapshot(ctx)
		if err != nil {
			return false
		}
		return len(snap.Running) == 1
	})

	// Query by identifier.
	resp, err := http.Get("http://" + httpSrv.Addr() + "/api/v1/TEST-API")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	var issueResp map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&issueResp))
	require.Equal(t, "running", issueResp["status"])
	require.Equal(t, "TEST-API", issueResp["issue_identifier"])

	// Query non-existent.
	resp404, err := http.Get("http://" + httpSrv.Addr() + "/api/v1/NONEXISTENT")
	require.NoError(t, err)
	defer resp404.Body.Close()
	require.Equal(t, 404, resp404.StatusCode)

	cancel()
	<-orchDone
}

// readJSONLine reads one JSON line from a bufio.Scanner.
func readJSONLine(scanner *bufio.Scanner) (map[string]any, error) {
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("EOF")
	}
	var msg map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w: %s", err, scanner.Text())
	}
	return msg, nil
}
