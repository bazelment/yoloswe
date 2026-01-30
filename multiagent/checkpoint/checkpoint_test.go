package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/multiagent/protocol"
)

func TestManager_Lifecycle(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "test-session-lifecycle"

	mgr := NewManager(tempDir, sessionID)

	// Set mission
	mgr.SetMission("Build a hello world CLI")

	// Verify initial state
	cp := mgr.Current()
	if cp.Phase != PhaseNotStarted {
		t.Errorf("expected phase %s, got %s", PhaseNotStarted, cp.Phase)
	}
	if cp.Mission != "Build a hello world CLI" {
		t.Errorf("expected mission 'Build a hello world CLI', got %q", cp.Mission)
	}

	// Start design phase
	if err := mgr.StartDesign(); err != nil {
		t.Fatalf("StartDesign() error: %v", err)
	}
	if cp.Phase != PhaseDesigning {
		t.Errorf("expected phase %s, got %s", PhaseDesigning, cp.Phase)
	}

	// Complete design
	designResp := &protocol.DesignResponse{
		Architecture: "Simple CLI with main.go",
	}
	if err := mgr.CompleteDesign(designResp, 0.01); err != nil {
		t.Fatalf("CompleteDesign() error: %v", err)
	}
	if cp.DesignResponse == nil {
		t.Error("expected design response to be set")
	}
	if cp.TotalCost != 0.01 {
		t.Errorf("expected cost 0.01, got %f", cp.TotalCost)
	}

	// Start build phase
	if err := mgr.StartBuild(); err != nil {
		t.Fatalf("StartBuild() error: %v", err)
	}
	if cp.Phase != PhaseBuilding {
		t.Errorf("expected phase %s, got %s", PhaseBuilding, cp.Phase)
	}

	// Complete build
	buildResp := &protocol.BuildResponse{
		FilesCreated:  []string{"main.go"},
		FilesModified: []string{"go.mod"},
	}
	if err := mgr.CompleteBuild(buildResp, 0.02); err != nil {
		t.Fatalf("CompleteBuild() error: %v", err)
	}
	if len(cp.FilesCreated) != 1 || cp.FilesCreated[0] != "main.go" {
		t.Errorf("expected files created ['main.go'], got %v", cp.FilesCreated)
	}
	if cp.TotalCost != 0.03 {
		t.Errorf("expected cost 0.03, got %f", cp.TotalCost)
	}

	// Start review phase
	if err := mgr.StartReview(); err != nil {
		t.Fatalf("StartReview() error: %v", err)
	}
	if cp.Phase != PhaseReviewing {
		t.Errorf("expected phase %s, got %s", PhaseReviewing, cp.Phase)
	}

	// Complete review
	reviewResp := &protocol.ReviewResponse{
		Summary: "Looks good",
		Issues:  []protocol.Issue{}, // No issues means approved
	}
	if err := mgr.CompleteReview(reviewResp, 0.005); err != nil {
		t.Fatalf("CompleteReview() error: %v", err)
	}
	if cp.ReviewResponse.HasCriticalIssues() {
		t.Error("expected review to have no critical issues")
	}

	// Mark complete
	if err := mgr.Complete(); err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if cp.Phase != PhaseCompleted {
		t.Errorf("expected phase %s, got %s", PhaseCompleted, cp.Phase)
	}
}

func TestManager_Persistence(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "test-session-persist"

	// Create and save checkpoint
	mgr := NewManager(tempDir, sessionID)
	mgr.SetMission("Test persistence")
	if err := mgr.StartDesign(); err != nil {
		t.Fatalf("StartDesign() error: %v", err)
	}

	// Load checkpoint
	loaded, err := Load(tempDir, sessionID)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected checkpoint to be loaded")
	}
	if loaded.Mission != "Test persistence" {
		t.Errorf("expected mission 'Test persistence', got %q", loaded.Mission)
	}
	if loaded.Phase != PhaseDesigning {
		t.Errorf("expected phase %s, got %s", PhaseDesigning, loaded.Phase)
	}
}

func TestManager_FailedState(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "test-session-fail"

	mgr := NewManager(tempDir, sessionID)
	mgr.SetMission("Test failure")

	// Start design
	if err := mgr.StartDesign(); err != nil {
		t.Fatalf("StartDesign() error: %v", err)
	}

	// Simulate failure
	testErr := errors.New("simulated failure")
	if err := mgr.Fail(testErr); err != nil {
		t.Fatalf("Fail() error: %v", err)
	}

	cp := mgr.Current()
	if cp.Phase != PhaseFailed {
		t.Errorf("expected phase %s, got %s", PhaseFailed, cp.Phase)
	}
	if cp.LastError != "simulated failure" {
		t.Errorf("expected error 'simulated failure', got %q", cp.LastError)
	}
}

func TestCheckpoint_CanResume(t *testing.T) {
	tests := []struct {
		name      string
		phase     Phase
		design    *protocol.DesignResponse
		canResume bool
	}{
		{"not started", PhaseNotStarted, nil, false},
		{"designing", PhaseDesigning, nil, true},
		{"building", PhaseBuilding, nil, true},
		{"reviewing", PhaseReviewing, nil, true},
		{"completed", PhaseCompleted, nil, false},
		{"failed no design", PhaseFailed, nil, false},
		{"failed with design", PhaseFailed, &protocol.DesignResponse{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &Checkpoint{
				Phase:          tt.phase,
				DesignResponse: tt.design,
			}
			if cp.CanResume() != tt.canResume {
				t.Errorf("CanResume() = %v, want %v", cp.CanResume(), tt.canResume)
			}
		})
	}
}

func TestCheckpoint_ResumePhase(t *testing.T) {
	tests := []struct {
		name        string
		checkpoint  Checkpoint
		resumePhase Phase
	}{
		{
			"from designing",
			Checkpoint{Phase: PhaseDesigning},
			PhaseDesigning,
		},
		{
			"from building no response",
			Checkpoint{Phase: PhaseBuilding},
			PhaseBuilding,
		},
		{
			"from building with response",
			Checkpoint{Phase: PhaseBuilding, BuildResponse: &protocol.BuildResponse{}},
			PhaseReviewing,
		},
		{
			"from reviewing no critical",
			Checkpoint{Phase: PhaseReviewing, ReviewResponse: &protocol.ReviewResponse{Summary: "OK", Issues: []protocol.Issue{}}},
			PhaseReviewing,
		},
		{
			"from reviewing with critical",
			Checkpoint{Phase: PhaseReviewing, ReviewResponse: &protocol.ReviewResponse{
				Summary: "Issues found",
				Issues:  []protocol.Issue{{Severity: "critical", File: "main.go", Message: "error"}},
			}},
			PhaseBuilding,
		},
		{
			"from failed with design only",
			Checkpoint{Phase: PhaseFailed, DesignResponse: &protocol.DesignResponse{}},
			PhaseBuilding,
		},
		{
			"from failed with build",
			Checkpoint{Phase: PhaseFailed, DesignResponse: &protocol.DesignResponse{}, BuildResponse: &protocol.BuildResponse{}},
			PhaseReviewing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.checkpoint.ResumePhase(); got != tt.resumePhase {
				t.Errorf("ResumePhase() = %v, want %v", got, tt.resumePhase)
			}
		})
	}
}

func TestExists(t *testing.T) {
	tempDir := t.TempDir()

	// Non-existent session
	if Exists(tempDir, "non-existent") {
		t.Error("Exists() should return false for non-existent session")
	}

	// Create a checkpoint
	mgr := NewManager(tempDir, "exists-test")
	if err := mgr.StartDesign(); err != nil {
		t.Fatalf("StartDesign() error: %v", err)
	}

	// Now it should exist
	if !Exists(tempDir, "exists-test") {
		t.Error("Exists() should return true after checkpoint created")
	}
}

func TestDelete(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "delete-test"

	// Create a checkpoint
	mgr := NewManager(tempDir, sessionID)
	if err := mgr.StartDesign(); err != nil {
		t.Fatalf("StartDesign() error: %v", err)
	}

	// Verify it exists
	if !Exists(tempDir, sessionID) {
		t.Fatal("checkpoint should exist before delete")
	}

	// Delete
	if err := Delete(tempDir, sessionID); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}

	// Verify it's gone
	if Exists(tempDir, sessionID) {
		t.Error("checkpoint should not exist after delete")
	}

	// Delete again should not error
	if err := Delete(tempDir, sessionID); err != nil {
		t.Errorf("Delete() on non-existent should not error: %v", err)
	}
}

func TestLoad_NonExistent(t *testing.T) {
	tempDir := t.TempDir()

	cp, err := Load(tempDir, "non-existent")
	if err != nil {
		t.Errorf("Load() should not error for non-existent: %v", err)
	}
	if cp != nil {
		t.Error("Load() should return nil for non-existent")
	}
}

func TestManager_StartIteration(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "test-iteration"

	mgr := NewManager(tempDir, sessionID)
	mgr.SetMission("Test iteration")

	// Go through design -> build -> review with issues
	_ = mgr.StartDesign()
	_ = mgr.CompleteDesign(&protocol.DesignResponse{}, 0.01)
	_ = mgr.StartBuild()
	_ = mgr.CompleteBuild(&protocol.BuildResponse{FilesCreated: []string{"a.go"}}, 0.02)
	_ = mgr.StartReview()
	_ = mgr.CompleteReview(&protocol.ReviewResponse{Summary: "Issues", Issues: []protocol.Issue{{Severity: "critical", File: "a.go", Message: "error"}}}, 0.005)

	cp := mgr.Current()
	if cp.IterationCount != 0 {
		t.Errorf("expected iteration count 0, got %d", cp.IterationCount)
	}

	// Start new iteration
	if err := mgr.StartIteration(); err != nil {
		t.Fatalf("StartIteration() error: %v", err)
	}

	if cp.IterationCount != 1 {
		t.Errorf("expected iteration count 1, got %d", cp.IterationCount)
	}
	if cp.Phase != PhaseBuilding {
		t.Errorf("expected phase %s, got %s", PhaseBuilding, cp.Phase)
	}
	// Build and review responses should be cleared
	if cp.BuildResponse != nil {
		t.Error("expected build response to be cleared")
	}
	if cp.ReviewResponse != nil {
		t.Error("expected review response to be cleared")
	}
	// But files should still be tracked
	if len(cp.FilesCreated) != 1 {
		t.Errorf("expected 1 file created, got %d", len(cp.FilesCreated))
	}
}

func TestManager_LastUpdated(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "test-updated"

	mgr := NewManager(tempDir, sessionID)
	initialTime := mgr.Current().LastUpdated

	time.Sleep(10 * time.Millisecond)

	_ = mgr.StartDesign()
	afterDesign := mgr.Current().LastUpdated

	if !afterDesign.After(initialTime) {
		t.Error("LastUpdated should be updated after StartDesign")
	}
}

func TestCheckpointFile_Location(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "test-location"

	mgr := NewManager(tempDir, sessionID)
	_ = mgr.StartDesign()

	expectedPath := filepath.Join(tempDir, sessionID, CheckpointFileName)
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Errorf("checkpoint file not created at expected location: %s", expectedPath)
	}
}

func TestCheckpoint_Version(t *testing.T) {
	tempDir := t.TempDir()
	sessionID := "test-version"

	mgr := NewManager(tempDir, sessionID)
	_ = mgr.StartDesign()

	loaded, err := Load(tempDir, sessionID)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded.Version != "1.0" {
		t.Errorf("expected version '1.0', got %q", loaded.Version)
	}
}
