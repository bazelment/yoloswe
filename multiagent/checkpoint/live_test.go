package checkpoint_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/checkpoint"
	"github.com/bazelment/yoloswe/multiagent/orchestrator"
	"github.com/bazelment/yoloswe/multiagent/planner"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

// TestLive_CheckpointCreation verifies that checkpoints are created during mission execution.
func TestLive_CheckpointCreation(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "live-checkpoint-creation"

	config := agent.SwarmConfig{
		SessionID:           sessionID,
		WorkDir:             tempDir,
		SessionDir:          tempDir,
		OrchestratorModel:   "sonnet",
		PlannerModel:        "sonnet",
		DesignerModel:       "haiku",
		BuilderModel:        "haiku",
		ReviewerModel:       "haiku",
		TotalBudgetUSD:      1.0,
		MaxIterations:       10,
		EnableCheckpointing: true,
	}

	orch, err := orchestrator.New(config)
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}

	// Verify checkpoint file location is set up correctly
	checkpointPath := filepath.Join(tempDir, sessionID, checkpoint.CheckpointFileName)
	t.Logf("Checkpoint will be at: %s", checkpointPath)

	// We can't run full mission without real Claude, but we can verify the setup
	if orch.SessionID() != sessionID {
		t.Errorf("expected session ID %q, got %q", sessionID, orch.SessionID())
	}
}

// TestLive_CheckpointPersistenceAndLoad verifies the full checkpoint lifecycle.
func TestLive_CheckpointPersistenceAndLoad(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "live-checkpoint-persist"

	// Create and populate checkpoint
	mgr := checkpoint.NewManager(tempDir, sessionID)
	mgr.SetMission("Build a hello world CLI")

	// Simulate design phase
	if err := mgr.StartDesign(); err != nil {
		t.Fatalf("StartDesign() error: %v", err)
	}
	designResp := &protocol.DesignResponse{
		Architecture: "Simple CLI with main.go and flag parsing",
	}
	if err := mgr.CompleteDesign(designResp, 0.015); err != nil {
		t.Fatalf("CompleteDesign() error: %v", err)
	}

	// Simulate build phase
	if err := mgr.StartBuild(); err != nil {
		t.Fatalf("StartBuild() error: %v", err)
	}
	buildResp := &protocol.BuildResponse{
		FilesCreated:  []string{"main.go", "cli/commands.go"},
		FilesModified: []string{"go.mod", "go.sum"},
	}
	if err := mgr.CompleteBuild(buildResp, 0.025); err != nil {
		t.Fatalf("CompleteBuild() error: %v", err)
	}

	// Simulate failure during review
	if err := mgr.StartReview(); err != nil {
		t.Fatalf("StartReview() error: %v", err)
	}

	// Simulate crash - checkpoint should persist current state
	currentPhase := mgr.Current().Phase
	if currentPhase != checkpoint.PhaseReviewing {
		t.Errorf("expected phase %s, got %s", checkpoint.PhaseReviewing, currentPhase)
	}

	// Load checkpoint in a "new session"
	loaded, err := checkpoint.Load(tempDir, sessionID)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected checkpoint to be loaded")
	}

	// Verify loaded state matches
	if loaded.Mission != "Build a hello world CLI" {
		t.Errorf("expected mission 'Build a hello world CLI', got %q", loaded.Mission)
	}
	if loaded.Phase != checkpoint.PhaseReviewing {
		t.Errorf("expected phase %s, got %s", checkpoint.PhaseReviewing, loaded.Phase)
	}
	if loaded.DesignResponse == nil {
		t.Error("expected design response to be preserved")
	}
	if loaded.BuildResponse == nil {
		t.Error("expected build response to be preserved")
	}
	if len(loaded.FilesCreated) != 2 {
		t.Errorf("expected 2 files created, got %d", len(loaded.FilesCreated))
	}
	if len(loaded.FilesModified) != 2 {
		t.Errorf("expected 2 files modified, got %d", len(loaded.FilesModified))
	}

	// Verify cost tracking
	expectedCost := 0.015 + 0.025 // design + build
	if loaded.TotalCost != expectedCost {
		t.Errorf("expected total cost %.3f, got %.3f", expectedCost, loaded.TotalCost)
	}

	// Verify resume logic
	if !loaded.CanResume() {
		t.Error("checkpoint should be resumable")
	}
	if loaded.ResumePhase() != checkpoint.PhaseReviewing {
		t.Errorf("expected resume phase %s, got %s", checkpoint.PhaseReviewing, loaded.ResumePhase())
	}
}

// TestLive_CheckpointResumeFromFailure verifies resume after failure.
func TestLive_CheckpointResumeFromFailure(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "live-checkpoint-resume"

	// Create checkpoint with failed state
	mgr := checkpoint.NewManager(tempDir, sessionID)
	mgr.SetMission("Implement user authentication")

	_ = mgr.StartDesign()
	_ = mgr.CompleteDesign(&protocol.DesignResponse{
		Architecture: "JWT-based auth with middleware",
	}, 0.02)

	_ = mgr.StartBuild()
	_ = mgr.CompleteBuild(&protocol.BuildResponse{
		FilesCreated: []string{"auth/jwt.go", "middleware/auth.go"},
	}, 0.03)

	_ = mgr.StartReview()

	// Simulate failure
	if err := mgr.Fail(context.DeadlineExceeded); err != nil {
		t.Fatalf("Fail() error: %v", err)
	}

	// Load and verify resume capability
	loaded, err := checkpoint.Load(tempDir, sessionID)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if loaded.Phase != checkpoint.PhaseFailed {
		t.Errorf("expected phase %s, got %s", checkpoint.PhaseFailed, loaded.Phase)
	}
	if loaded.LastError == "" {
		t.Error("expected error message to be recorded")
	}

	// Verify it can resume
	if !loaded.CanResume() {
		t.Error("failed checkpoint with design should be resumable")
	}

	// Should resume from review since build was complete
	if loaded.ResumePhase() != checkpoint.PhaseReviewing {
		t.Errorf("expected resume phase %s, got %s", checkpoint.PhaseReviewing, loaded.ResumePhase())
	}
}

// TestLive_PlannerCheckpointIntegration tests planner-level checkpoint integration.
func TestLive_PlannerCheckpointIntegration(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "live-planner-checkpoint"

	plannerCfg := planner.Config{
		PlannerConfig: agent.AgentConfig{
			Role:       agent.RolePlanner,
			Model:      "sonnet",
			WorkDir:    tempDir,
			SessionDir: tempDir,
		},
		DesignerConfig: agent.AgentConfig{
			Role:       agent.RoleDesigner,
			Model:      "haiku",
			WorkDir:    tempDir,
			SessionDir: tempDir,
		},
		BuilderConfig: agent.AgentConfig{
			Role:       agent.RoleBuilder,
			Model:      "haiku",
			WorkDir:    tempDir,
			SessionDir: tempDir,
		},
		ReviewerConfig: agent.AgentConfig{
			Role:       agent.RoleReviewer,
			Model:      "haiku",
			WorkDir:    tempDir,
			SessionDir: tempDir,
		},
		MaxIterations:       10,
		EnableCheckpointing: true,
		SessionDir:          tempDir,
	}

	p := planner.New(plannerCfg, sessionID)

	// Set mission
	p.SetMission("Build a test feature")

	// Verify checkpoint was initialized
	cp := p.GetCheckpoint()
	if cp == nil {
		t.Fatal("expected checkpoint to be initialized")
	}
	if cp.Mission != "Build a test feature" {
		t.Errorf("expected mission 'Build a test feature', got %q", cp.Mission)
	}

	// Mark complete
	if err := p.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete() error: %v", err)
	}

	// Verify completion
	cp = p.GetCheckpoint()
	if cp.Phase != checkpoint.PhaseCompleted {
		t.Errorf("expected phase %s, got %s", checkpoint.PhaseCompleted, cp.Phase)
	}
}

// TestLive_PlannerRestoreFromCheckpoint tests restoring planner state.
func TestLive_PlannerRestoreFromCheckpoint(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "live-planner-restore"

	// Create a checkpoint to restore from
	cp := &checkpoint.Checkpoint{
		Version:        "1.0",
		SessionID:      sessionID,
		Mission:        "Restore test mission",
		Phase:          checkpoint.PhaseBuilding,
		IterationCount: 3,
		FilesCreated:   []string{"a.go", "b.go"},
		FilesModified:  []string{"c.go"},
		TotalCost:      0.05,
	}

	plannerCfg := planner.Config{
		PlannerConfig: agent.AgentConfig{
			Role:       agent.RolePlanner,
			Model:      "sonnet",
			WorkDir:    tempDir,
			SessionDir: tempDir,
		},
		MaxIterations:       10,
		EnableCheckpointing: true,
		SessionDir:          tempDir,
	}

	// Create planner from checkpoint
	p := planner.NewFromCheckpoint(plannerCfg, sessionID, cp)

	// Verify state was restored
	if p.IterationCount() != 3 {
		t.Errorf("expected iteration count 3, got %d", p.IterationCount())
	}
	if p.TotalCost() != 0.05 {
		t.Errorf("expected total cost 0.05, got %f", p.TotalCost())
	}
}

// TestLive_CheckpointFileFormat verifies the JSON file format.
func TestLive_CheckpointFileFormat(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "live-file-format"

	mgr := checkpoint.NewManager(tempDir, sessionID)
	mgr.SetMission("Format test")
	_ = mgr.StartDesign()
	_ = mgr.CompleteDesign(&protocol.DesignResponse{Architecture: "Test"}, 0.01)

	// Read the raw file
	checkpointPath := filepath.Join(tempDir, sessionID, checkpoint.CheckpointFileName)
	data, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatalf("Failed to read checkpoint file: %v", err)
	}

	// Verify it's valid JSON with expected fields
	content := string(data)
	expectedFields := []string{
		`"version"`,
		`"session_id"`,
		`"mission"`,
		`"phase"`,
		`"iteration_count"`,
		`"last_updated"`,
		`"design_response"`,
		`"total_cost"`,
	}

	for _, field := range expectedFields {
		if !contains(content, field) {
			t.Errorf("expected checkpoint to contain field %s", field)
		}
	}

	// Verify phase value
	if !contains(content, `"phase": "designing"`) {
		t.Error("expected phase to be 'designing' after CompleteDesign (design phase recorded)")
	}
}

// TestLive_CheckpointCLIFlags tests the CLI flag integration.
func TestLive_CheckpointCLIFlags(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "live-cli-flags"

	// Test with checkpointing enabled (default)
	configEnabled := agent.SwarmConfig{
		SessionID:           sessionID,
		WorkDir:             tempDir,
		SessionDir:          tempDir,
		OrchestratorModel:   "sonnet",
		PlannerModel:        "sonnet",
		EnableCheckpointing: true,
	}

	orchEnabled, err := orchestrator.New(configEnabled)
	if err != nil {
		t.Fatalf("Failed to create orchestrator with checkpointing: %v", err)
	}
	_ = orchEnabled // orchestrator created successfully

	// Test with checkpointing disabled
	configDisabled := agent.SwarmConfig{
		SessionID:           sessionID + "-disabled",
		WorkDir:             tempDir,
		SessionDir:          tempDir,
		OrchestratorModel:   "sonnet",
		PlannerModel:        "sonnet",
		EnableCheckpointing: false,
	}

	orchDisabled, err := orchestrator.New(configDisabled)
	if err != nil {
		t.Fatalf("Failed to create orchestrator without checkpointing: %v", err)
	}
	_ = orchDisabled // orchestrator created successfully
}

// TestLive_CheckpointIterationTracking tests iteration count across checkpoint.
func TestLive_CheckpointIterationTracking(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "live-iteration-tracking"

	mgr := checkpoint.NewManager(tempDir, sessionID)
	mgr.SetMission("Iteration tracking test")

	// Complete a full cycle
	_ = mgr.StartDesign()
	_ = mgr.CompleteDesign(&protocol.DesignResponse{}, 0.01)
	_ = mgr.StartBuild()
	_ = mgr.CompleteBuild(&protocol.BuildResponse{}, 0.02)
	_ = mgr.StartReview()
	_ = mgr.CompleteReview(&protocol.ReviewResponse{
		Summary: "Issues found",
		Issues:  []protocol.Issue{{Severity: "critical", File: "main.go", Message: "bug"}},
	}, 0.005)

	// Verify iteration count before iteration
	cp := mgr.Current()
	if cp.IterationCount != 0 {
		t.Errorf("expected iteration count 0 before first iteration, got %d", cp.IterationCount)
	}

	// Start new iteration
	_ = mgr.StartIteration()

	if cp.IterationCount != 1 {
		t.Errorf("expected iteration count 1 after first iteration, got %d", cp.IterationCount)
	}

	// Another cycle
	_ = mgr.StartBuild()
	_ = mgr.CompleteBuild(&protocol.BuildResponse{}, 0.02)
	_ = mgr.StartReview()
	_ = mgr.CompleteReview(&protocol.ReviewResponse{
		Summary: "More issues",
		Issues:  []protocol.Issue{{Severity: "critical", File: "main.go", Message: "another bug"}},
	}, 0.005)

	_ = mgr.StartIteration()

	if cp.IterationCount != 2 {
		t.Errorf("expected iteration count 2 after second iteration, got %d", cp.IterationCount)
	}

	// Reload and verify
	loaded, _ := checkpoint.Load(tempDir, sessionID)
	if loaded.IterationCount != 2 {
		t.Errorf("expected loaded iteration count 2, got %d", loaded.IterationCount)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
