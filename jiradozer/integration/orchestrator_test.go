//go:build integration

package integration

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// mockOrchestratorTracker is a minimal tracker for orchestrator tests.
// Workflows will fail quickly (no workflow states to resolve) which is
// fine — we're testing orchestrator mechanics, not the workflow itself.
type mockOrchestratorTracker struct {
	mu             sync.Mutex
	issues         []*tracker.Issue
	workflowStates []tracker.WorkflowState
}

func (m *mockOrchestratorTracker) FetchIssue(_ context.Context, identifier string) (*tracker.Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, issue := range m.issues {
		if issue.Identifier == identifier {
			return issue, nil
		}
	}
	return nil, nil
}

func (m *mockOrchestratorTracker) ListIssues(_ context.Context, _ tracker.IssueFilter) ([]*tracker.Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issues, nil
}

func (m *mockOrchestratorTracker) FetchComments(_ context.Context, _ string, _ time.Time) ([]tracker.Comment, error) {
	return nil, nil
}

func (m *mockOrchestratorTracker) FetchWorkflowStates(_ context.Context, _ string) ([]tracker.WorkflowState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workflowStates, nil
}

func (m *mockOrchestratorTracker) PostComment(_ context.Context, _ string, _ string) (tracker.Comment, error) {
	return tracker.Comment{}, nil
}

func (m *mockOrchestratorTracker) UpdateIssueState(_ context.Context, _ string, _ string) error {
	return nil
}
func (m *mockOrchestratorTracker) AddLabel(_ context.Context, _ string, _ string) error {
	return nil
}
func (m *mockOrchestratorTracker) RemoveLabel(_ context.Context, _ string, _ string) error {
	return nil
}

// mockWTManager tracks worktree operations without real git.
type mockWTManager struct {
	mu      sync.Mutex
	created map[string]string
	removed []string
}

func newMockWTManager(t *testing.T) *mockWTManager {
	return &mockWTManager{created: make(map[string]string)}
}

func (m *mockWTManager) NewWorktree(_ context.Context, branch, _, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Create a real temp directory so the workflow can reference it.
	dir, err := os.MkdirTemp("", "jiradozer-wt-*")
	if err != nil {
		return "", err
	}
	m.created[branch] = dir
	return dir, nil
}

func (m *mockWTManager) RemoveWorktree(_ context.Context, nameOrBranch string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, nameOrBranch)
	// Clean up the temp dir if it exists.
	if dir, ok := m.created[nameOrBranch]; ok {
		os.RemoveAll(dir)
	}
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

func (m *mockWTManager) getRemoved() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]string, len(m.removed))
	copy(cp, m.removed)
	return cp
}

func testOrchestratorConfig() *jiradozer.Config {
	cfg := jiradozer.DefaultConfig()
	cfg.Source.Filters = map[string]string{tracker.FilterTeam: "ENG"}
	cfg.Source.MaxConcurrent = 3
	cfg.Source.BranchPrefix = "jiradozer"
	cfg.Tracker.APIKey = "test-key"
	cfg.Plan.AutoApprove = true
	cfg.Build.AutoApprove = true
	cfg.Validate.AutoApprove = true
	cfg.Ship.AutoApprove = true
	return cfg
}

// writeIntegrationTestScript creates a shell script in a temp dir. The script
// exits immediately with the given exit code, simulating a subprocess run.
func writeIntegrationTestScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "test.sh")
	err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	require.NoError(t, err)
	return p
}

// newOrchWithScript creates an orchestrator configured to spawn a test shell
// script instead of the real jiradozer binary. Tests use this to verify
// orchestrator mechanics without a real agent.
func newOrchWithScript(t *testing.T, cfg *jiradozer.Config, wtm *mockWTManager, script string) *jiradozer.Orchestrator {
	t.Helper()
	logDir := t.TempDir()
	orch := jiradozer.NewOrchestrator(&mockOrchestratorTracker{}, cfg, wtm, "", testOrchestratorLogger(t))
	orch.SetSubprocessMode(script, nil, logDir)
	return orch
}

func testIssue(id, identifier, title string) *tracker.Issue {
	return &tracker.Issue{
		ID:         id,
		Identifier: identifier,
		Title:      title,
		TeamID:     "team-1",
	}
}

func TestOrchestrator_WorktreeCreation(t *testing.T) {
	issues := []*tracker.Issue{
		testIssue("1", "ENG-1", "Feature A"),
		testIssue("2", "ENG-2", "Feature B"),
	}
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()
	script := writeIntegrationTestScript(t, "exit 0")
	orch := newOrchWithScript(t, cfg, wtm, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := orch.Start(ctx, issues[0])
	require.NoError(t, err)
	err = orch.Start(ctx, issues[1])
	require.NoError(t, err)

	created := wtm.getCreated()
	require.Contains(t, created, "jiradozer/ENG-1")
	require.Contains(t, created, "jiradozer/ENG-2")

	// Verify worktree directories were created.
	for _, dir := range created {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.True(t, info.IsDir())
	}

	// Wait for subprocesses to complete.
	orch.Wait()

	// On successful completion the worktree is intentionally left intact so
	// the PR (which may not be merged yet) retains its base branch.
	removed := wtm.getRemoved()
	require.NotContains(t, removed, "jiradozer/ENG-1")
	require.NotContains(t, removed, "jiradozer/ENG-2")
}

func TestOrchestrator_WorktreeRemovedOnFailure(t *testing.T) {
	t.Parallel()
	issues := []*tracker.Issue{
		testIssue("1", "ENG-1", "Feature A"),
	}
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()
	script := writeIntegrationTestScript(t, "exit 1")
	orch := newOrchWithScript(t, cfg, wtm, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := orch.Start(ctx, issues[0])
	require.NoError(t, err)

	// Wait for subprocess to complete.
	orch.Wait()

	// On failure the worktree must be cleaned up.
	removed := wtm.getRemoved()
	require.Contains(t, removed, "jiradozer/ENG-1")
}

func TestOrchestrator_ConcurrencyLimit(t *testing.T) {
	issues := []*tracker.Issue{
		testIssue("1", "ENG-1", "Feature A"),
		testIssue("2", "ENG-2", "Feature B"),
		testIssue("3", "ENG-3", "Feature C"),
	}
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()
	cfg.Source.MaxConcurrent = 2
	script := writeIntegrationTestScript(t, "sleep 60")
	orch := newOrchWithScript(t, cfg, wtm, script)

	ctx := context.Background()

	err1 := orch.Start(ctx, issues[0])
	require.NoError(t, err1)
	err2 := orch.Start(ctx, issues[1])
	require.NoError(t, err2)

	// Third should be rejected.
	err3 := orch.Start(ctx, issues[2])
	require.Error(t, err3)
	require.Contains(t, err3.Error(), "concurrency limit")
}

func TestOrchestrator_BranchPrefix(t *testing.T) {
	issue := testIssue("1", "ENG-1", "Feature A")
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()
	cfg.Source.BranchPrefix = "auto"
	script := writeIntegrationTestScript(t, "exit 0")
	orch := newOrchWithScript(t, cfg, wtm, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := orch.Start(ctx, issue)
	require.NoError(t, err)

	created := wtm.getCreated()
	require.Contains(t, created, "auto/ENG-1")
	require.NotContains(t, created, "jiradozer/ENG-1")

	orch.Wait()
}

func TestOrchestrator_StatusUpdates(t *testing.T) {
	issue := testIssue("1", "ENG-1", "Feature A")
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()
	// Script exits cleanly so we get StepDone.
	script := writeIntegrationTestScript(t, "exit 0")
	orch := newOrchWithScript(t, cfg, wtm, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := orch.Start(ctx, issue)
	require.NoError(t, err)

	// Drain status updates — init + done.
	var statuses []jiradozer.IssueStatus
	timeout := time.After(5 * time.Second)
	for {
		select {
		case s := <-orch.StatusUpdates():
			statuses = append(statuses, s)
			if s.IsDone() {
				goto done
			}
		case <-timeout:
			t.Fatal("timed out waiting for status updates")
		}
	}
done:
	require.NotEmpty(t, statuses)
	require.Equal(t, "ENG-1", statuses[0].Issue.Identifier)
	require.Equal(t, jiradozer.StepInit, statuses[0].Step)
	require.True(t, statuses[len(statuses)-1].IsDone())
}

func TestOrchestrator_DiscoveryIntegration(t *testing.T) {
	issueA := testIssue("1", "ENG-1", "Feature A")
	issueB := testIssue("2", "ENG-2", "Feature B")

	mt := &mockOrchestratorTracker{
		issues: []*tracker.Issue{issueA, issueB},
	}
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()
	cfg.Source.MaxConcurrent = 5
	cfg.PollInterval = 50 * time.Millisecond
	script := writeIntegrationTestScript(t, "exit 0")
	orch := newOrchWithScript(t, cfg, wtm, script)
	disc := jiradozer.NewDiscovery(mt, cfg.Source.ToFilter(), cfg.PollInterval, testOrchestratorLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run in background.
	done := make(chan error, 1)
	go func() {
		done <- orch.RunWithDiscovery(ctx, disc)
	}()

	// Wait for both subprocesses to complete.
	doneCount := 0
	timeout := time.After(10 * time.Second)
	for doneCount < 2 {
		select {
		case s := <-orch.StatusUpdates():
			if s.IsDone() {
				doneCount++
			}
		case <-timeout:
			t.Fatalf("timed out waiting for subprocesses to complete (got %d/2)", doneCount)
		}
	}

	cancel()
	<-done

	// Both issues should have had worktrees created.
	created := wtm.getCreated()
	require.Contains(t, created, "jiradozer/ENG-1")
	require.Contains(t, created, "jiradozer/ENG-2")
}

func TestOrchestrator_WorktreePreservedOnCompletion(t *testing.T) {
	// On successful completion the worktree is intentionally left intact so
	// the PR (which may not be merged yet) retains its base branch.
	issue := testIssue("1", "ENG-1", "Feature A")
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()
	script := writeIntegrationTestScript(t, "exit 0")
	orch := newOrchWithScript(t, cfg, wtm, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := orch.Start(ctx, issue)
	require.NoError(t, err)

	orch.Wait()

	removed := wtm.getRemoved()
	require.NotContains(t, removed, "jiradozer/ENG-1", "worktree must not be removed after successful completion")

	preserved := orch.PreservedWorktrees()
	require.Len(t, preserved, 1)
	require.Equal(t, "ENG-1", preserved[0].Issue)
}

// testOrchestratorLogger returns a logger suitable for integration tests.
func testOrchestratorLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Verify the worktree path is absolute and within a temp directory.
func TestOrchestrator_WorktreePathIsValid(t *testing.T) {
	issue := testIssue("1", "ENG-1", "Feature A")
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()
	script := writeIntegrationTestScript(t, "sleep 60")
	orch := newOrchWithScript(t, cfg, wtm, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := orch.Start(ctx, issue)
	require.NoError(t, err)

	snap := orch.Snapshot()
	require.Len(t, snap, 1)
	require.True(t, filepath.IsAbs(snap[0].WorktreePath), "worktree path should be absolute: %s", snap[0].WorktreePath)

	orch.Wait()
}
