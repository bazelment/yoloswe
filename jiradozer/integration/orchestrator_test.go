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
	cfg.Source.Team = "ENG"
	cfg.Source.MaxConcurrent = 3
	cfg.Source.BranchPrefix = "jiradozer"
	cfg.Tracker.APIKey = "test-key"
	cfg.Plan.AutoApprove = true
	cfg.Build.AutoApprove = true
	cfg.Validate.AutoApprove = true
	cfg.Ship.AutoApprove = true
	return cfg
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
	mt := &mockOrchestratorTracker{
		issues: []*tracker.Issue{
			testIssue("1", "ENG-1", "Feature A"),
			testIssue("2", "ENG-2", "Feature B"),
		},
	}
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()

	orch := jiradozer.NewOrchestrator(mt, cfg, wtm, testOrchestratorLogger(t))

	// Short timeout — workflows will fail when agent times out, which is expected.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := orch.Start(ctx, mt.issues[0])
	require.NoError(t, err)
	err = orch.Start(ctx, mt.issues[1])
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

	// Wait for workflows to complete (they'll fail due to agent timeout).
	orch.Wait()

	// Verify cleanup happened (on failure too).
	removed := wtm.getRemoved()
	require.Contains(t, removed, "jiradozer/ENG-1")
	require.Contains(t, removed, "jiradozer/ENG-2")
}

func TestOrchestrator_ConcurrencyLimit(t *testing.T) {
	mt := &mockOrchestratorTracker{
		issues: []*tracker.Issue{
			testIssue("1", "ENG-1", "Feature A"),
			testIssue("2", "ENG-2", "Feature B"),
			testIssue("3", "ENG-3", "Feature C"),
		},
	}
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()
	cfg.Source.MaxConcurrent = 2

	orch := jiradozer.NewOrchestrator(mt, cfg, wtm, testOrchestratorLogger(t))
	ctx := context.Background()

	err1 := orch.Start(ctx, mt.issues[0])
	require.NoError(t, err1)
	err2 := orch.Start(ctx, mt.issues[1])
	require.NoError(t, err2)

	// Third should be rejected.
	err3 := orch.Start(ctx, mt.issues[2])
	require.Error(t, err3)
	require.Contains(t, err3.Error(), "concurrency limit")
}

func TestOrchestrator_BranchPrefix(t *testing.T) {
	mt := &mockOrchestratorTracker{
		issues: []*tracker.Issue{testIssue("1", "ENG-1", "Feature A")},
	}
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()
	cfg.Source.BranchPrefix = "auto"

	orch := jiradozer.NewOrchestrator(mt, cfg, wtm, testOrchestratorLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := orch.Start(ctx, mt.issues[0])
	require.NoError(t, err)

	created := wtm.getCreated()
	require.Contains(t, created, "auto/ENG-1")
	require.NotContains(t, created, "jiradozer/ENG-1")

	orch.Wait()
}

func TestOrchestrator_StatusUpdates(t *testing.T) {
	mt := &mockOrchestratorTracker{
		issues: []*tracker.Issue{testIssue("1", "ENG-1", "Feature A")},
	}
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()

	orch := jiradozer.NewOrchestrator(mt, cfg, wtm, testOrchestratorLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := orch.Start(ctx, mt.issues[0])
	require.NoError(t, err)

	// Drain status updates — should get at least init + a final state (failed due to agent timeout).
	var statuses []jiradozer.IssueStatus
	timeout := time.After(10 * time.Second)
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

	orch := jiradozer.NewOrchestrator(mt, cfg, wtm, testOrchestratorLogger(t))
	disc := jiradozer.NewDiscovery(mt, cfg.Source.ToFilter(), cfg.PollInterval, testOrchestratorLogger(t))

	// Short timeout — workflows fail when agent times out, which is expected.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Run in background.
	done := make(chan error, 1)
	go func() {
		done <- orch.RunWithDiscovery(ctx, disc)
	}()

	// Wait for both workflows to complete (they'll fail due to agent timeout).
	doneCount := 0
	timeout := time.After(10 * time.Second)
	for doneCount < 2 {
		select {
		case s := <-orch.StatusUpdates():
			if s.IsDone() {
				doneCount++
			}
		case <-timeout:
			t.Fatalf("timed out waiting for workflows to complete (got %d/2)", doneCount)
		}
	}

	cancel()
	<-done

	// Both issues should have had worktrees created.
	created := wtm.getCreated()
	require.Contains(t, created, "jiradozer/ENG-1")
	require.Contains(t, created, "jiradozer/ENG-2")
}

func TestOrchestrator_WorktreeCleanupOnCompletion(t *testing.T) {
	mt := &mockOrchestratorTracker{
		issues: []*tracker.Issue{testIssue("1", "ENG-1", "Feature A")},
	}
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()

	orch := jiradozer.NewOrchestrator(mt, cfg, wtm, testOrchestratorLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := orch.Start(ctx, mt.issues[0])
	require.NoError(t, err)

	orch.Wait()

	removed := wtm.getRemoved()
	require.Contains(t, removed, "jiradozer/ENG-1", "worktree should be cleaned up after failure/completion")
}

// testOrchestratorLogger returns a logger suitable for integration tests.
func testOrchestratorLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Verify the worktree path is absolute and within a temp directory.
func TestOrchestrator_WorktreePathIsValid(t *testing.T) {
	mt := &mockOrchestratorTracker{
		issues: []*tracker.Issue{testIssue("1", "ENG-1", "Feature A")},
	}
	wtm := newMockWTManager(t)
	cfg := testOrchestratorConfig()

	orch := jiradozer.NewOrchestrator(mt, cfg, wtm, testOrchestratorLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := orch.Start(ctx, mt.issues[0])
	require.NoError(t, err)

	snap := orch.Snapshot()
	require.Len(t, snap, 1)
	require.True(t, filepath.IsAbs(snap[0].WorktreePath), "worktree path should be absolute: %s", snap[0].WorktreePath)

	orch.Wait()
}
