package jiradozer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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

func TestOrchestrator_UpdateConfigMaxConcurrentAffectsFutureStarts(t *testing.T) {
	cfg := testOrchestratorConfig()
	cfg.Source.MaxConcurrent = 1

	script := writeTestScript(t, "trap 'exit 130' INT; sleep 60")
	orch, _ := setupSubprocessOrch(t, cfg, script)
	defer func() {
		orch.Cancel("1")
		orch.Cancel("2")
		orch.Wait()
	}()

	issue1 := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test 1"}
	issue2 := &tracker.Issue{ID: "2", Identifier: "ENG-2", Title: "Test 2"}

	require.NoError(t, orch.Start(context.Background(), issue1))
	require.ErrorIs(t, orch.Start(context.Background(), issue2), errConcurrencyLimit)

	next := *cfg
	next.Source.MaxConcurrent = 2
	orch.UpdateConfig(&next)

	require.NoError(t, orch.Start(context.Background(), issue2))
}

func TestOrchestrator_UpdateConfigBranchPrefixAffectsOnlyFutureStarts(t *testing.T) {
	cfg := testOrchestratorConfig()
	cfg.Source.BranchPrefix = "old"
	cfg.Source.MaxConcurrent = 2

	script := writeTestScript(t, "trap 'exit 130' INT; sleep 60")
	orch, wtm := setupSubprocessOrch(t, cfg, script)
	defer func() {
		orch.Cancel("1")
		orch.Cancel("2")
		orch.Wait()
	}()

	require.NoError(t, orch.Start(context.Background(), &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test 1"}))

	next := *cfg
	next.Source.BranchPrefix = "new"
	orch.UpdateConfig(&next)

	require.NoError(t, orch.Start(context.Background(), &tracker.Issue{ID: "2", Identifier: "ENG-2", Title: "Test 2"}))

	created := wtm.getCreated()
	require.Contains(t, created, "old/ENG-1")
	require.Contains(t, created, "new/ENG-2")
}

func TestOrchestrator_RestoreActiveWaitsForChildPID(t *testing.T) {
	cfg := testOrchestratorConfig()
	wtm := newMockWTManager()
	orch := NewOrchestrator(&mockDiscoveryTracker{}, cfg, wtm, "", testLogger(t))

	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())

	orch.RestoreActive([]ManagedWorkflowSnapshot{
		{
			Issue:        &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Restored"},
			PID:          cmd.Process.Pid,
			Branch:       "jiradozer/ENG-1",
			WorktreePath: "/tmp/worktrees/jiradozer/ENG-1",
			StartedAt:    time.Now(),
		},
	})

	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, StepInit, status.Step)
		require.Equal(t, "ENG-1", status.Issue.Identifier)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for restored StepInit")
	}

	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, StepDone, status.Step)
		require.Equal(t, "ENG-1", status.Issue.Identifier)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for restored StepDone")
	}

	orch.Wait()
	require.Equal(t, 0, orch.ActiveCount())
}

func TestOrchestrator_RestoreActiveSkipsNilIssue(t *testing.T) {
	cfg := testOrchestratorConfig()
	orch := NewOrchestrator(&mockDiscoveryTracker{}, cfg, newMockWTManager(), "", testLogger(t))

	restored := orch.RestoreActive([]ManagedWorkflowSnapshot{{PID: 1234}})

	require.Empty(t, restored)
	require.Equal(t, 0, orch.ActiveCount())
}

func TestOrchestrator_RestoredCancelEmitsCancelled(t *testing.T) {
	cfg := testOrchestratorConfig()
	orch := NewOrchestrator(&mockDiscoveryTracker{}, cfg, newMockWTManager(), "", testLogger(t))

	cmd := exec.Command("sh", "-c", "trap 'exit 130' INT; sleep 10")
	require.NoError(t, cmd.Start())

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Restored"}
	restored := orch.RestoreActive([]ManagedWorkflowSnapshot{
		{
			Issue:        issue,
			PID:          cmd.Process.Pid,
			Branch:       "jiradozer/ENG-1",
			WorktreePath: "/tmp/worktrees/jiradozer/ENG-1",
			StartedAt:    time.Now(),
		},
	})
	require.Equal(t, []string{issue.ID}, restored)

	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, StepInit, status.Step)
		require.Equal(t, issue.Identifier, status.Issue.Identifier)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for restored StepInit")
	}

	orch.Cancel(issue.ID)

	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, StepCancelled, status.Step)
		require.Equal(t, issue.Identifier, status.Issue.Identifier)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for restored StepCancelled")
	}

	orch.Wait()
	require.Equal(t, 0, orch.ActiveCount())
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

// TestOrchestrator_HungIsFailedNotCancelled verifies that a watchdog-killed
// (hung) subprocess is classified as StepFailed end to end — the emitted
// status AND the preserved-worktree summary — not StepCancelled, so the alert,
// the supervisor UI, and the summary all agree the run failed.
//
// Not t.Parallel(): same ETXTBSY fork/exec race as the other Cancel* tests.
func TestOrchestrator_HungIsFailedNotCancelled(t *testing.T) {
	cfg := testOrchestratorConfig()

	// Child traps SIGINT and exits non-zero, as a stuck agent killed by the
	// watchdog's mw.cancel() (SIGINT) would.
	script := writeTestScript(t, "trap 'exit 1' INT; sleep 60")
	orch, _ := setupSubprocessOrch(t, cfg, script)

	ctx, cancel := context.WithCancel(context.Background())
	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	require.NoError(t, orch.Start(ctx, issue))

	// Drain StepInit.
	select {
	case <-orch.StatusUpdates():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StepInit")
	}

	// Simulate the watchdog's decision: mark the live workflow hung, then
	// cancel (the watchdog sets mw.hung before calling mw.cancel()).
	orch.mu.RLock()
	mw := orch.active[issue.ID]
	orch.mu.RUnlock()
	require.NotNil(t, mw, "workflow should be active")
	mw.hung.Store(true)
	cancel()

	// A hung run must surface as StepFailed, not StepCancelled.
	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, StepFailed, status.Step, "hung run must be StepFailed, not StepCancelled")
		require.Error(t, status.Error)
		require.Contains(t, status.Error.Error(), "hung")
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for StepFailed")
	}

	orch.Wait()

	// Preserved (default), and recorded as StepFailed — not StepCancelled.
	preserved := orch.PreservedWorktrees()
	require.Len(t, preserved, 1)
	require.Equal(t, StepFailed, preserved[0].Step, "preserved summary must label a hung run as failed")
}

// TestOrchestrator_FailedWorktreePreservedByDefault asserts that a StepFailed
// workflow preserves its worktree and branches by default. Failing steps
// often fire mid-workflow after real work exists (pushed branch, open PR);
// silently deleting that work is destructive. Operators can rerun with
// --force-cleanup to wipe.
func TestOrchestrator_FailedWorktreePreservedByDefault(t *testing.T) {
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

	wtm.mu.Lock()
	removed := wtm.removed
	wtm.mu.Unlock()
	require.Empty(t, removed, "failed worktree must NOT be removed by default")

	preserved := orch.PreservedWorktrees()
	require.Len(t, preserved, 1)
	require.Equal(t, "ENG-1", preserved[0].Issue)
	require.Equal(t, StepFailed, preserved[0].Step)
}

func TestOrchestrator_FailedWithForceCleanup(t *testing.T) {
	cfg := testOrchestratorConfig()

	script := writeTestScript(t, "exit 1")
	orch, wtm := setupSubprocessOrch(t, cfg, script)
	orch.SetForceCleanup(true)

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	err := orch.Start(context.Background(), issue)
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		select {
		case <-orch.StatusUpdates():
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for status")
		}
	}

	orch.Wait()

	wtm.mu.Lock()
	removed := wtm.removed
	wtm.mu.Unlock()
	require.Len(t, removed, 1, "failed worktree must be removed with --force-cleanup")
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
	stateUpdates  []string // issueIDs passed to UpdateIssueState
	stateIDs      []string // stateIDs passed to UpdateIssueState
	labelIssues   []string // issueIDs passed to AddLabel
	labelNames    []string // labels passed to AddLabel
	removedLabels []string // labels passed to RemoveLabel
	claimMu       sync.Mutex
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

func (m *mockClaimTracker) RemoveLabel(_ context.Context, _ string, label string) error {
	m.claimMu.Lock()
	defer m.claimMu.Unlock()
	m.removedLabels = append(m.removedLabels, label)
	return nil
}

func (m *mockClaimTracker) getRemovedLabels() []string {
	m.claimMu.Lock()
	defer m.claimMu.Unlock()
	return append([]string(nil), m.removedLabels...)
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

// TestOrchestrator_StartClaimsInProgress verifies the two-phase claim:
// addLockLabel runs before cmd.Start (so the label signals intent
// across processes immediately), and transitionToInProgress runs only
// after cmd.Start succeeds (so failures between worktree creation and
// cmd.Start leave the issue rediscoverable). Together they form a
// distributed lock against concurrent jiradozer processes.
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

// TestOrchestrator_StartReleasesLockLabelOnLogOpenFailure verifies that
// when OpenFile fails after addLockLabel has attached the lock label,
// Start() releases the label and never transitions the issue state, so
// discovery's state-filtered queries can rediscover the issue.
//
// Forces OpenFile failure by setting logDir to a regular file path: the
// per-issue log path then has a file (not directory) as its parent.
func TestOrchestrator_StartReleasesLockLabelOnLogOpenFailure(t *testing.T) {
	cfg := testOrchestratorConfig()

	// logDir is a regular file, not a directory — OpenFile will fail
	// because the log path's parent is not a directory.
	tmpFile := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(tmpFile, nil, 0o600))

	wtm := newMockWTManagerWithDir(t)

	issue := &tracker.Issue{
		ID:         "issue-1",
		Identifier: "ENG-1",
		Title:      "Test",
		TeamID:     "team-eng",
	}

	mt := &mockClaimTracker{}
	orch := NewOrchestrator(mt, cfg, wtm, "", testLogger(t))
	orch.SetSubprocessMode("/bin/true", nil, tmpFile)

	err := orch.Start(context.Background(), issue)
	require.Error(t, err, "OpenFile must fail when logDir is a regular file")
	require.Contains(t, err.Error(), "open log file")

	// Lock label was added by addLockLabel and must be released.
	removed := mt.getRemovedLabels()
	require.Len(t, removed, 1, "expected lock label to be released after OpenFile failure")
	require.Equal(t, LockLabel, removed[0])

	// State transition is deferred until after cmd.Start succeeds, so
	// it must not have happened — discovery's state filter still sees
	// the issue and can retry.
	stateUpdates, _ := mt.getStateUpdates()
	require.Empty(t, stateUpdates,
		"state must not transition to In Progress on log-open failure")

	// Slot must also be unreserved so the next Start() can reuse it.
	require.Equal(t, 0, orch.ActiveCount())
}

// TestOrchestrator_StartReleasesLockLabelOnCmdStartFailure verifies the
// same lock-label release on the cmd.Start() failure path. Uses a
// non-existent binary so exec.Cmd.Start fails fast.
func TestOrchestrator_StartReleasesLockLabelOnCmdStartFailure(t *testing.T) {
	cfg := testOrchestratorConfig()

	logDir := t.TempDir()
	wtm := newMockWTManagerWithDir(t)

	issue := &tracker.Issue{
		ID:         "issue-1",
		Identifier: "ENG-1",
		Title:      "Test",
		TeamID:     "team-eng",
	}

	mt := &mockClaimTracker{}
	orch := NewOrchestrator(mt, cfg, wtm, "", testLogger(t))
	// Path that exists in t.TempDir but is not executable — cmd.Start
	// returns "permission denied" / "exec format error".
	nonExec := filepath.Join(t.TempDir(), "definitely-not-a-binary")
	require.NoError(t, os.WriteFile(nonExec, []byte("not a binary"), 0o600))
	orch.SetSubprocessMode(nonExec, nil, logDir)

	err := orch.Start(context.Background(), issue)
	require.Error(t, err, "cmd.Start must fail for a non-executable binary")
	require.Contains(t, err.Error(), "start subprocess")

	removed := mt.getRemovedLabels()
	require.Len(t, removed, 1, "expected lock label to be released after cmd.Start failure")
	require.Equal(t, LockLabel, removed[0])

	stateUpdates, _ := mt.getStateUpdates()
	require.Empty(t, stateUpdates,
		"state must not transition to In Progress on cmd.Start failure")

	require.Equal(t, 0, orch.ActiveCount())
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
