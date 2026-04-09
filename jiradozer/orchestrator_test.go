package jiradozer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// mockWTManager implements WorktreeManager for testing.
type mockWTManager struct {
	newErr  error
	created map[string]string // branch -> worktreePath
	removed []string          // branches removed
	mu      sync.Mutex
}

func newMockWTManager() *mockWTManager {
	return &mockWTManager{created: make(map[string]string)}
}

func (m *mockWTManager) NewWorktree(_ context.Context, branch, _, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.newErr != nil {
		return "", m.newErr
	}
	path := "/tmp/worktrees/" + branch
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
	cfg.Source.Filters = map[string]string{"team": "ENG"}
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

func TestOrchestrator_BranchPrefix(t *testing.T) {
	wtm := newMockWTManager()
	cfg := testOrchestratorConfig()
	cfg.Source.BranchPrefix = "auto"
	mt := &mockDiscoveryTracker{}

	orch := NewOrchestrator(mt, cfg, wtm, testLogger(t))

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	// Start will fail because the mock tracker doesn't implement workflow states,
	// but we can check the worktree was created with the right branch.
	_ = orch.Start(context.Background(), issue)

	created := wtm.getCreated()
	require.Contains(t, created, "auto/ENG-1")
}

func TestOrchestrator_ConcurrencyLimit(t *testing.T) {
	wtm := newMockWTManager()
	cfg := testOrchestratorConfig()
	cfg.Source.MaxConcurrent = 2
	mt := &mockDiscoveryTracker{}

	orch := NewOrchestrator(mt, cfg, wtm, testLogger(t))

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
	wtm := newMockWTManager()
	cfg := testOrchestratorConfig()
	mt := &mockDiscoveryTracker{}

	orch := NewOrchestrator(mt, cfg, wtm, testLogger(t))

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	err1 := orch.Start(context.Background(), issue)
	require.NoError(t, err1)

	err2 := orch.Start(context.Background(), issue)
	require.Error(t, err2)
	require.Contains(t, err2.Error(), "already has an active workflow")
}

func TestOrchestrator_StatusUpdates(t *testing.T) {
	wtm := newMockWTManager()
	cfg := testOrchestratorConfig()
	mt := &mockDiscoveryTracker{}

	orch := NewOrchestrator(mt, cfg, wtm, testLogger(t))

	issue := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test"}
	err := orch.Start(context.Background(), issue)
	require.NoError(t, err)

	// Should receive at least the initial status update.
	select {
	case status := <-orch.StatusUpdates():
		require.Equal(t, "ENG-1", status.Issue.Identifier)
		require.Equal(t, StepInit, status.Step)
		require.False(t, status.IsDone())
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for status update")
	}
}

func TestOrchestrator_Snapshot(t *testing.T) {
	wtm := newMockWTManager()
	cfg := testOrchestratorConfig()
	mt := &mockDiscoveryTracker{}

	orch := NewOrchestrator(mt, cfg, wtm, testLogger(t))

	issue1 := &tracker.Issue{ID: "1", Identifier: "ENG-1", Title: "Test 1"}
	issue2 := &tracker.Issue{ID: "2", Identifier: "ENG-2", Title: "Test 2"}

	_ = orch.Start(context.Background(), issue1)
	_ = orch.Start(context.Background(), issue2)

	snap := orch.Snapshot()
	require.Len(t, snap, 2)

	ids := map[string]bool{}
	for _, s := range snap {
		ids[s.Issue.Identifier] = true
	}
	require.True(t, ids["ENG-1"])
	require.True(t, ids["ENG-2"])
}
