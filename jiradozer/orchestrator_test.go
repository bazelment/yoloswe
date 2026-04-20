package jiradozer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// mockWTManager implements WorktreeManager for testing.
type mockWTManager struct {
	newErr  error
	baseDir string            // temp dir for creating real directories
	created map[string]string // branch -> worktreePath
	removed []string          // branches removed
	mu      sync.Mutex
}

func newMockWTManager() *mockWTManager {
	return &mockWTManager{created: make(map[string]string)}
}

func newMockWTManagerWithDir(t *testing.T) *mockWTManager {
	t.Helper()
	return &mockWTManager{
		baseDir: t.TempDir(),
		created: make(map[string]string),
	}
}

func (m *mockWTManager) NewWorktree(_ context.Context, branch, _, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.newErr != nil {
		return "", m.newErr
	}
	var path string
	if m.baseDir != "" {
		path = filepath.Join(m.baseDir, branch)
		os.MkdirAll(path, 0o755)
	} else {
		path = "/tmp/worktrees/" + branch
	}
	m.created[branch] = path
	return path, nil
}

func (m *mockWTManager) RemoveWorktree(_ context.Context, nameOrBranch string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, nameOrBranch)
	return nil
}

func (m *mockWTManager) getCreated() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]string, len(m.created))
	for k, v := range m.created {
		cp[k] = v
	}
	return cp
}

func testOrchestratorConfig() *Config {
	cfg := defaultConfig()
	cfg.Source.Filters = map[string]string{tracker.FilterTeam: "ENG"}
	cfg.Source.MaxConcurrent = 3
	cfg.Source.BranchPrefix = "jiradozer"
	cfg.Tracker.APIKey = "test-key"
	// Auto-approve all steps to avoid blocking on comment polling.
	cfg.Plan.AutoApprove = true
	cfg.Build.AutoApprove = true
	cfg.Validate.AutoApprove = true
	cfg.Ship.AutoApprove = true
	return &cfg
}

// setupSubprocessOrch creates an orchestrator with subprocess mode configured
// using the given binary (e.g. a test script).
func setupSubprocessOrch(t *testing.T, cfg *Config, binary string) (*Orchestrator, *mockWTManager) {
	t.Helper()
	wtm := newMockWTManagerWithDir(t)
	logDir := t.TempDir()
	orch := NewOrchestrator(&mockDiscoveryTracker{}, cfg, wtm, "", testLogger(t))
	orch.SetSubprocessMode(binary, nil, logDir)
	return orch, wtm
}

func TestOrchestrator_BranchPrefix(t *testing.T) {
	cfg := testOrchestratorConfig()
	cfg.Source.BranchPrefix = "auto"

	script := writeTestScript(t, "exit 0")
	orch, wtm := setupSubprocessOrch(t, cfg, script)

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	err := orch.Start(context.Background(), issue)
	require.NoError(t, err)

	created := wtm.getCreated()
	require.Contains(t, created, "auto/ENG-1")
}

func TestOrchestrator_SpecialCharsInIdentifier(t *testing.T) {
	cfg := testOrchestratorConfig()
	cfg.Source.BranchPrefix = "auto"

	script := writeTestScript(t, "exit 0")
	orch, _ := setupSubprocessOrch(t, cfg, script)

	// GitHub identifiers like "acme/app#42" contain "/" and "#" which are
	// problematic in file paths. Both must be sanitized.
	issue := &tracker.Issue{ID: "1", Identifier: "acme/app#42", Title: "Test"}
	err := orch.Start(context.Background(), issue)
	require.NoError(t, err)

	orch.Wait()

	// Verify log file was created with sanitized name.
	logDir := orch.logDir
	entries, err := os.ReadDir(logDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].Name(), "acme_app_42-")
}

func TestSanitizeForFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"ENG-123", "ENG-123"},
		{"acme/app#42", "acme_app_42"},
		{`a\b:c*d?e"f<g>h|i j`, "a_b_c_d_e_f_g_h_i_j"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeForFilename(tt.input)
			require.Equal(t, tt.want, got)
		})
	}
}

// writeTestScript creates a shell script in a temp dir that ignores all
// arguments and runs the given body. Returns the script path.
func writeTestScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "test.sh")
	err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	require.NoError(t, err)
	return p
}

func TestOrchestrator_ConcurrencyLimit(t *testing.T) {
	cfg := testOrchestratorConfig()
	cfg.Source.MaxConcurrent = 2

	script := writeTestScript(t, "sleep 60")
	orch, _ := setupSubprocessOrch(t, cfg, script)

	issue1 := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test 1"}
	issue2 := &tracker.Issue{ID: "2", Identifier: "ENG-2", Title: "Test 2"}
	issue3 := &tracker.Issue{ID: "3", Identifier: "ENG-3", Title: "Test 3"}

	err1 := orch.Start(context.Background(), issue1)
	require.NoError(t, err1)
	err2 := orch.Start(context.Background(), issue2)
	require.NoError(t, err2)

	// Third should hit the limit.
	err3 := orch.Start(context.Background(), issue3)
	require.Error(t, err3)
	require.Contains(t, err3.Error(), "concurrency limit")
}

func TestOrchestrator_DuplicateIssue(t *testing.T) {
	cfg := testOrchestratorConfig()

	script := writeTestScript(t, "sleep 60")
	orch, _ := setupSubprocessOrch(t, cfg, script)

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	err1 := orch.Start(context.Background(), issue)
	require.NoError(t, err1)

	err2 := orch.Start(context.Background(), issue)
	require.Error(t, err2)
	require.Contains(t, err2.Error(), "already has an active workflow")
}

func TestOrchestrator_StatusUpdates(t *testing.T) {
	cfg := testOrchestratorConfig()

	script := writeTestScript(t, "exit 0")
	orch, _ := setupSubprocessOrch(t, cfg, script)

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	err := orch.Start(context.Background(), issue)
	require.NoError(t, err)

	// Should receive the initial status update.
	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, "ENG-1", status.Issue.Identifier)
		require.Equal(t, StepInit, status.Step)
		require.False(t, status.IsDone())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StepInit status")
	}

	// Should receive StepDone when the subprocess exits.
	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, "ENG-1", status.Issue.Identifier)
		require.Equal(t, StepDone, status.Step)
		require.True(t, status.IsDone())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StepDone status")
	}
}

func TestOrchestrator_SubprocessFailed(t *testing.T) {
	cfg := testOrchestratorConfig()

	script := writeTestScript(t, "exit 1")
	orch, _ := setupSubprocessOrch(t, cfg, script)

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	err := orch.Start(context.Background(), issue)
	require.NoError(t, err)

	// Drain StepInit.
	select {
	case <-orch.StatusUpdates():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StepInit")
	}

	// Should receive StepFailed.
	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, StepFailed, status.Step)
		require.Error(t, status.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StepFailed")
	}
}

func TestOrchestrator_DryRun(t *testing.T) {
	wtm := newMockWTManager()
	cfg := testOrchestratorConfig()
	cfg.Source.DryRun = true
	cfg.BaseBranch = "main"
	cfg.Agent.Model = "sonnet"
	mt := &mockDiscoveryTracker{}

	orch := NewOrchestrator(mt, cfg, wtm, "yoloswe", testLogger(t))
	var buf bytes.Buffer
	orch.SetDryRunOutput(&buf)

	url := "https://linear.app/ENG-1"
	issue := &tracker.Issue{
		ID:         "1",
		Identifier: "ENG-1",
		Title:      "Fix widget rendering",
		URL:        &url,
	}

	err := orch.Start(context.Background(), issue)
	require.NoError(t, err)

	// Worktree manager must not have been touched.
	require.Empty(t, wtm.getCreated())
	// No status updates should have been emitted.
	select {
	case s := <-orch.StatusUpdates():
		t.Fatalf("unexpected status update in dry-run mode: %+v", s)
	case <-time.After(50 * time.Millisecond):
	}
	// Nothing was added to the active map.
	require.Equal(t, 0, orch.ActiveCount())

	out := buf.String()
	require.Contains(t, out, "bramble new-session")
	require.Contains(t, out, "--create-worktree")
	require.Contains(t, out, `--branch 'jiradozer/ENG-1'`)
	require.Contains(t, out, `--from 'main'`)
	require.Contains(t, out, `--model 'sonnet'`)
	require.Contains(t, out, `--repo 'yoloswe'`)
	require.Contains(t, out, `--goal 'Fix widget rendering'`)
	require.Contains(t, out, "Work on ENG-1: Fix widget rendering")
	require.Contains(t, out, url)

	// A second Start with the same issue should also print — dry-run does
	// not track duplicates, but discovery's seen set keeps real repeats
	// from reaching this code path in practice.
	buf.Reset()
	err = orch.Start(context.Background(), issue)
	require.NoError(t, err)
	require.Contains(t, buf.String(), "bramble new-session")
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty string", "", "''"},
		{"simple word", "hello", "'hello'"},
		{"with spaces", "hello world", "'hello world'"},
		{"shell metachars are inert inside quotes", "$(whoami) && rm -rf / `id` !echo", "'$(whoami) && rm -rf / `id` !echo'"},
		{"newline preserved literally", "line1\nline2", "'line1\nline2'"},
		{"single quote escaped via close-escape-reopen", "it's", `'it'\''s'`},
		{"multiple single quotes", "a'b'c", `'a'\''b'\''c'`},
		{"only single quote", "'", `''\'''`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, shellQuote(tt.in))
		})
	}
}

func TestOrchestrator_CancelPreservesWorktree(t *testing.T) {
	cfg := testOrchestratorConfig()

	// Script that waits for SIGINT (simulates a long-running subprocess).
	script := writeTestScript(t, "trap 'exit 130' INT; sleep 60")
	orch, wtm := setupSubprocessOrch(t, cfg, script)

	ctx, cancel := context.WithCancel(context.Background())
	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	err := orch.Start(ctx, issue)
	require.NoError(t, err)

	// Drain StepInit.
	select {
	case <-orch.StatusUpdates():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StepInit")
	}

	// Cancel the context (simulates Ctrl+C).
	cancel()

	// Should receive StepCancelled.
	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, StepCancelled, status.Step)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for StepCancelled")
	}

	orch.Wait()

	// Worktree should NOT have been removed.
	wtm.mu.Lock()
	removed := wtm.removed
	wtm.mu.Unlock()
	require.Empty(t, removed, "cancelled worktree should not be removed")

	// Preserved worktrees should be recorded.
	preserved := orch.PreservedWorktrees()
	require.Len(t, preserved, 1)
	require.Equal(t, "ENG-1", preserved[0].Issue)
	require.Contains(t, preserved[0].Branch, "ENG-1")
}

func TestOrchestrator_CancelWithForceCleanup(t *testing.T) {
	cfg := testOrchestratorConfig()

	script := writeTestScript(t, "trap 'exit 130' INT; sleep 60")
	orch, wtm := setupSubprocessOrch(t, cfg, script)
	orch.SetForceCleanup(true)

	ctx, cancel := context.WithCancel(context.Background())
	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	err := orch.Start(ctx, issue)
	require.NoError(t, err)

	// Drain StepInit.
	select {
	case <-orch.StatusUpdates():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StepInit")
	}

	cancel()

	// Should receive StepCancelled.
	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, StepCancelled, status.Step)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for StepCancelled")
	}

	orch.Wait()

	// With forceCleanup, worktree SHOULD be removed.
	wtm.mu.Lock()
	removed := wtm.removed
	wtm.mu.Unlock()
	require.Len(t, removed, 1, "cancelled worktree should be removed with --force-cleanup")

	// No preserved worktrees.
	require.Empty(t, orch.PreservedWorktrees())
}

func TestOrchestrator_CancelWithCleanExit(t *testing.T) {
	// Verify that a subprocess that traps SIGINT and exits 0 is still
	// classified as StepCancelled, not StepDone.
	//
	// Intentionally NOT t.Parallel(): fork/exec of the freshly written
	// test.sh races ETXTBSY against other tests' cmd.Start() calls under
	// load (observed on GitHub Actions). The other Cancel* tests in this
	// file are also serial for the same reason.
	cfg := testOrchestratorConfig()

	// Script traps SIGINT and exits cleanly (exit 0).
	script := writeTestScript(t, "trap 'exit 0' INT; sleep 60")
	orch, wtm := setupSubprocessOrch(t, cfg, script)

	ctx, cancel := context.WithCancel(context.Background())
	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	err := orch.Start(ctx, issue)
	require.NoError(t, err)

	// Drain StepInit.
	select {
	case <-orch.StatusUpdates():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StepInit")
	}

	// Cancel the context (simulates Ctrl+C).
	cancel()

	// Should receive StepCancelled, not StepDone, even though exit code is 0.
	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, StepCancelled, status.Step, "clean exit after SIGINT must be StepCancelled")
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for StepCancelled")
	}

	orch.Wait()

	// Worktree should NOT have been removed (default: preserve on cancel).
	wtm.mu.Lock()
	removed := wtm.removed
	wtm.mu.Unlock()
	require.Empty(t, removed, "cancelled worktree should not be removed")
}

func TestOrchestrator_FailedStillCleansUp(t *testing.T) {
	cfg := testOrchestratorConfig()

	script := writeTestScript(t, "exit 1")
	orch, wtm := setupSubprocessOrch(t, cfg, script)

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	err := orch.Start(context.Background(), issue)
	require.NoError(t, err)

	// Drain StepInit + StepFailed.
	for i := 0; i < 2; i++ {
		select {
		case <-orch.StatusUpdates():
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for status")
		}
	}

	orch.Wait()

	// Failed worktrees should still be cleaned up.
	wtm.mu.Lock()
	removed := wtm.removed
	wtm.mu.Unlock()
	require.Len(t, removed, 1, "failed worktree should be removed")
	require.Empty(t, orch.PreservedWorktrees())
}

// countingWTManager wraps mockWTManager and counts NewWorktree calls.
type countingWTManager struct {
	*mockWTManager
	newCalls int
	mu2      sync.Mutex
}

func (c *countingWTManager) NewWorktree(ctx context.Context, branch, base, goal string) (string, error) {
	c.mu2.Lock()
	c.newCalls++
	c.mu2.Unlock()
	return c.mockWTManager.NewWorktree(ctx, branch, base, goal)
}

func (c *countingWTManager) callCount() int {
	c.mu2.Lock()
	defer c.mu2.Unlock()
	return c.newCalls
}

// TestRunWithDiscovery_PermanentErrorStopsRetrying verifies that RunWithDiscovery
// stops clearing the seen set after maxStartFailures permanent errors for the
// same issue, preventing an infinite retry storm. Uses a non-worktree error to
// exercise the generic retry path (worktree-exists errors are handled separately).
func TestRunWithDiscovery_PermanentErrorStopsRetrying(t *testing.T) {
	t.Parallel()

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	// mockDiscoveryTracker returns the same issue on every ListIssues call.
	mt := &mockDiscoveryTracker{
		results: [][]*tracker.Issue{
			{issue}, {issue}, {issue}, {issue}, {issue},
			{issue}, {issue}, {issue}, {issue}, {issue},
		},
	}

	cfg := testOrchestratorConfig()
	cfg.Source.MaxConcurrent = 3

	inner := newMockWTManager()
	inner.newErr = fmt.Errorf("disk quota exceeded") // generic permanent error
	wtm := &countingWTManager{mockWTManager: inner}

	logDir := t.TempDir()
	orch := NewOrchestrator(mt, cfg, wtm, "", testLogger(t))
	orch.SetSubprocessMode("/bin/false", nil, logDir)

	d := NewDiscovery(mt, tracker.IssueFilter{}, 5*time.Millisecond, testLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_ = orch.RunWithDiscovery(ctx, d)

	// After maxStartFailures attempts the retry storm must stop.
	// Allow one extra call in case of a race at the boundary.
	calls := wtm.callCount()
	require.LessOrEqual(t, calls, maxStartFailures+1,
		"NewWorktree called %d times; expected at most %d (maxStartFailures=%d)",
		calls, maxStartFailures+1, maxStartFailures)
}

// TestRunWithDiscovery_WorktreeExistsNoClearSeen verifies that a "worktree
// already exists" error does NOT trigger ClearSeen. The issue is already being
// handled by another process; calling ClearSeen would cause a retry storm.
func TestRunWithDiscovery_WorktreeExistsNoClearSeen(t *testing.T) {
	t.Parallel()

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	mt := &mockDiscoveryTracker{
		results: [][]*tracker.Issue{
			{issue}, {issue}, {issue}, {issue}, {issue},
		},
	}

	cfg := testOrchestratorConfig()
	cfg.Source.MaxConcurrent = 3

	inner := newMockWTManager()
	inner.newErr = fmt.Errorf("create worktree for ENG-1: worktree already exists")
	wtm := &countingWTManager{mockWTManager: inner}

	logDir := t.TempDir()
	orch := NewOrchestrator(mt, cfg, wtm, "", testLogger(t))
	orch.SetSubprocessMode("/bin/false", nil, logDir)

	d := NewDiscovery(mt, tracker.IssueFilter{}, 5*time.Millisecond, testLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_ = orch.RunWithDiscovery(ctx, d)

	// Must be tried exactly once: no ClearSeen means the issue stays in the seen
	// set and is never rediscovered, so NewWorktree is only called once.
	calls := wtm.callCount()
	require.Equal(t, 1, calls,
		"NewWorktree called %d times; expected exactly 1 (worktree-exists must not trigger ClearSeen)", calls)
}

// mockClaimTracker wraps mockDiscoveryTracker and records UpdateIssueState and AddLabel calls.
//
//nolint:govet // fieldalignment: embedded struct ordering is intentional for readability
type mockClaimTracker struct {
	mockDiscoveryTracker
	stateUpdates []string // issueIDs passed to UpdateIssueState
	stateIDs     []string // stateIDs passed to UpdateIssueState
	labelIssues  []string // issueIDs passed to AddLabel
	labelNames   []string // labels passed to AddLabel
	claimMu      sync.Mutex
}

func (m *mockClaimTracker) FetchWorkflowStates(_ context.Context, _ string) ([]tracker.WorkflowState, error) {
	return []tracker.WorkflowState{
		{ID: "state-inprogress", Name: "In Progress"},
		{ID: "state-done", Name: "Done"},
	}, nil
}

func (m *mockClaimTracker) UpdateIssueState(_ context.Context, issueID string, stateID string) error {
	m.claimMu.Lock()
	defer m.claimMu.Unlock()
	m.stateUpdates = append(m.stateUpdates, issueID)
	m.stateIDs = append(m.stateIDs, stateID)
	return nil
}

func (m *mockClaimTracker) AddLabel(_ context.Context, issueID string, label string) error {
	m.claimMu.Lock()
	defer m.claimMu.Unlock()
	m.labelIssues = append(m.labelIssues, issueID)
	m.labelNames = append(m.labelNames, label)
	return nil
}

func (m *mockClaimTracker) getStateUpdates() ([]string, []string) {
	m.claimMu.Lock()
	defer m.claimMu.Unlock()
	return append([]string(nil), m.stateUpdates...), append([]string(nil), m.stateIDs...)
}

func (m *mockClaimTracker) getLabelCalls() ([]string, []string) {
	m.claimMu.Lock()
	defer m.claimMu.Unlock()
	return append([]string(nil), m.labelIssues...), append([]string(nil), m.labelNames...)
}

// TestOrchestrator_StartClaimsInProgress verifies that Start() transitions the
// issue to "In Progress" and adds the LockLabel before launching the subprocess,
// providing a distributed lock so other jiradozer processes won't rediscover
// the same issue.
func TestOrchestrator_StartClaimsInProgress(t *testing.T) {
	cfg := testOrchestratorConfig()
	cfg.States.InProgress = "In Progress"

	script := writeTestScript(t, "exit 0")
	logDir := t.TempDir()
	wtm := newMockWTManagerWithDir(t)

	issue := &tracker.Issue{
		ID:         "issue-1",
		Identifier: "ENG-1",
		Title:      "Test issue",
		TeamID:     "team-eng",
	}

	mt := &mockClaimTracker{}
	orch := NewOrchestrator(mt, cfg, wtm, "", testLogger(t))
	orch.SetSubprocessMode(script, nil, logDir)

	err := orch.Start(context.Background(), issue)
	require.NoError(t, err)

	orch.Wait()

	// Verify state transition.
	issueIDs, stateIDs := mt.getStateUpdates()
	require.Len(t, issueIDs, 1, "expected exactly one UpdateIssueState call")
	require.Equal(t, "issue-1", issueIDs[0])
	require.Equal(t, "state-inprogress", stateIDs[0])

	// Verify lock label was added.
	labelIssues, labelNames := mt.getLabelCalls()
	require.Len(t, labelIssues, 1, "expected exactly one AddLabel call")
	require.Equal(t, "issue-1", labelIssues[0])
	require.Equal(t, LockLabel, labelNames[0])
}

// TestRunWithDiscovery_ConcurrencyLimitKeepsPending verifies that a
// concurrency-limit error keeps the issue in the pending queue rather than
// triggering a ClearSeen (which would cause an infinite retry storm when slots
// free up).
func TestRunWithDiscovery_ConcurrencyLimitKeepsPending(t *testing.T) {
	// Intentionally NOT t.Parallel(): fork/exec of a freshly written
	// shell script can race ETXTBSY against other tests' cmd.Start()
	// calls under load.
	// Use a script that exits quickly so slots free up.
	fastScript := writeTestScript(t, "exit 0")

	issue1 := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test 1"}
	issue2 := &tracker.Issue{ID: "2", Identifier: "ENG-2", Title: "Test 2"}
	issue3 := &tracker.Issue{ID: "3", Identifier: "ENG-3", Title: "Test 3"}

	cfg := testOrchestratorConfig()
	cfg.Source.MaxConcurrent = 2 // Only 2 slots.

	wtm := newMockWTManagerWithDir(t)
	logDir := t.TempDir()

	mt := &mockDiscoveryTracker{
		results: [][]*tracker.Issue{
			{issue1, issue2, issue3},
		},
	}

	orch := NewOrchestrator(mt, cfg, wtm, "", testLogger(t))
	orch.SetSubprocessMode(fastScript, nil, logDir)

	d := NewDiscovery(mt, tracker.IssueFilter{}, time.Hour, testLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = orch.RunWithDiscovery(ctx, d)

	// All three issues must have been started exactly once (third issue was
	// pending until a slot freed, then started — not duplicated via ClearSeen).
	created := wtm.getCreated()
	require.Len(t, created, 3, "expected all 3 issues to be started exactly once")
}

func TestOrchestrator_Snapshot(t *testing.T) {
	cfg := testOrchestratorConfig()

	script := writeTestScript(t, "sleep 60")
	orch, _ := setupSubprocessOrch(t, cfg, script)

	issue1 := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test 1"}
	issue2 := &tracker.Issue{ID: "2", Identifier: "ENG-2", Title: "Test 2"}

	require.NoError(t, orch.Start(context.Background(), issue1))
	require.NoError(t, orch.Start(context.Background(), issue2))

	snap := orch.Snapshot()
	require.Len(t, snap, 2)

	ids := map[string]bool{}
	for _, s := range snap {
		ids[s.Issue.Identifier] = true
		require.Equal(t, StepInit, s.Step)
	}
	require.True(t, ids["ENG-1"])
	require.True(t, ids["ENG-2"])
}
