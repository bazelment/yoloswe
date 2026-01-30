package orchestrator

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

func TestNew(t *testing.T) {
	config := agent.SwarmConfig{
		WorkDir:           "/tmp/test",
		SessionDir:        "/tmp/test-sessions",
		OrchestratorModel: "sonnet",
		PlannerModel:      "sonnet",
		DesignerModel:     "haiku",
		BuilderModel:      "haiku",
		ReviewerModel:     "haiku",
		TotalBudgetUSD:    1.0,
		MaxIterations:     10,
	}

	orch, err := New(config)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if orch.Role() != agent.RoleOrchestrator {
		t.Errorf("expected role %v, got %v", agent.RoleOrchestrator, orch.Role())
	}

	if orch.SessionID() == "" {
		t.Error("expected non-empty session ID")
	}

	if orch.TotalCost() != 0 {
		t.Errorf("expected initial cost 0, got %v", orch.TotalCost())
	}

	if orch.TurnCount() != 0 {
		t.Errorf("expected initial turn count 0, got %v", orch.TurnCount())
	}
}

func TestNewWithSessionID(t *testing.T) {
	config := agent.SwarmConfig{
		SessionID:         "custom-session-123",
		WorkDir:           "/tmp/test",
		SessionDir:        "/tmp/test-sessions",
		OrchestratorModel: "sonnet",
		PlannerModel:      "sonnet",
		DesignerModel:     "haiku",
		BuilderModel:      "haiku",
		ReviewerModel:     "haiku",
	}

	orch, err := New(config)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if orch.SessionID() != "custom-session-123" {
		t.Errorf("expected session ID %q, got %q", "custom-session-123", orch.SessionID())
	}
}

func TestGetSummary(t *testing.T) {
	config := agent.SwarmConfig{
		SessionID:         "test-summary-session",
		WorkDir:           "/tmp/test",
		SessionDir:        "/tmp/test-sessions",
		OrchestratorModel: "sonnet",
		PlannerModel:      "sonnet",
		DesignerModel:     "haiku",
		BuilderModel:      "haiku",
		ReviewerModel:     "haiku",
	}

	orch, err := New(config)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	summary := orch.GetSummary()

	if summary.SessionID != "test-summary-session" {
		t.Errorf("expected session ID %q, got %q", "test-summary-session", summary.SessionID)
	}

	if summary.TotalCost != 0 {
		t.Errorf("expected total cost 0, got %v", summary.TotalCost)
	}

	if summary.OrchestratorTurns != 0 {
		t.Errorf("expected orchestrator turns 0, got %v", summary.OrchestratorTurns)
	}

	if summary.PlannerTurns != 0 {
		t.Errorf("expected planner turns 0, got %v", summary.PlannerTurns)
	}

	if _, ok := summary.AgentCosts["orchestrator"]; !ok {
		t.Error("expected orchestrator cost in agent costs")
	}

	if _, ok := summary.AgentCosts["planner"]; !ok {
		t.Error("expected planner cost in agent costs")
	}
}

func TestDefaultSwarmConfig(t *testing.T) {
	config := agent.DefaultSwarmConfig()

	if config.WorkDir != "." {
		t.Errorf("expected work dir '.', got %q", config.WorkDir)
	}

	if config.SessionDir != ".claude-swarm/sessions" {
		t.Errorf("expected session dir '.claude-swarm/sessions', got %q", config.SessionDir)
	}

	if config.OrchestratorModel != "sonnet" {
		t.Errorf("expected orchestrator model 'sonnet', got %q", config.OrchestratorModel)
	}

	if config.ReviewerModel != "haiku" {
		t.Errorf("expected reviewer model 'haiku', got %q", config.ReviewerModel)
	}

	if config.TotalBudgetUSD != 1.0 {
		t.Errorf("expected budget 1.0, got %v", config.TotalBudgetUSD)
	}

	if config.MaxIterations != 50 {
		t.Errorf("expected max iterations 50, got %v", config.MaxIterations)
	}
}

func TestCheckBudget_UnderBudget(t *testing.T) {
	config := agent.SwarmConfig{
		SessionID:         "test-budget-under",
		WorkDir:           t.TempDir(),
		SessionDir:        t.TempDir(),
		OrchestratorModel: "sonnet",
		PlannerModel:      "sonnet",
		DesignerModel:     "haiku",
		BuilderModel:      "haiku",
		ReviewerModel:     "haiku",
		TotalBudgetUSD:    1.0,
	}

	orch, err := New(config)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Cost is 0, budget is 1.0 - should pass
	if err := orch.checkBudget(); err != nil {
		t.Errorf("checkBudget() should pass when under budget, got error: %v", err)
	}
}

func TestCheckBudget_NoBudgetLimit(t *testing.T) {
	config := agent.SwarmConfig{
		SessionID:         "test-budget-unlimited",
		WorkDir:           t.TempDir(),
		SessionDir:        t.TempDir(),
		OrchestratorModel: "sonnet",
		PlannerModel:      "sonnet",
		DesignerModel:     "haiku",
		BuilderModel:      "haiku",
		ReviewerModel:     "haiku",
		TotalBudgetUSD:    0, // No budget limit
	}

	orch, err := New(config)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Budget is 0 (unlimited) - should always pass
	if err := orch.checkBudget(); err != nil {
		t.Errorf("checkBudget() should pass when budget is 0 (unlimited), got error: %v", err)
	}

	// Even with artificial cost added, should pass
	orch.totalCost = 100.0
	if err := orch.checkBudget(); err != nil {
		t.Errorf("checkBudget() should pass even with high cost when budget is unlimited, got error: %v", err)
	}
}

func TestCheckBudget_OverBudget(t *testing.T) {
	config := agent.SwarmConfig{
		SessionID:         "test-budget-over",
		WorkDir:           t.TempDir(),
		SessionDir:        t.TempDir(),
		OrchestratorModel: "sonnet",
		PlannerModel:      "sonnet",
		DesignerModel:     "haiku",
		BuilderModel:      "haiku",
		ReviewerModel:     "haiku",
		TotalBudgetUSD:    0.10, // $0.10 budget
	}

	orch, err := New(config)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Simulate cost exceeding budget
	orch.totalCost = 0.15 // $0.15 > $0.10 budget

	err = orch.checkBudget()
	if err == nil {
		t.Error("checkBudget() should return error when over budget")
	}

	if !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("expected ErrBudgetExceeded, got: %v", err)
	}
}

func TestCheckBudget_AtBudget(t *testing.T) {
	config := agent.SwarmConfig{
		SessionID:         "test-budget-at",
		WorkDir:           t.TempDir(),
		SessionDir:        t.TempDir(),
		OrchestratorModel: "sonnet",
		PlannerModel:      "sonnet",
		DesignerModel:     "haiku",
		BuilderModel:      "haiku",
		ReviewerModel:     "haiku",
		TotalBudgetUSD:    1.0,
	}

	orch, err := New(config)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Set cost exactly at budget
	orch.totalCost = 1.0

	// At budget (not over) should pass
	if err := orch.checkBudget(); err != nil {
		t.Errorf("checkBudget() should pass when exactly at budget, got error: %v", err)
	}

	// Just over budget should fail
	orch.totalCost = 1.01
	if err := orch.checkBudget(); err == nil {
		t.Error("checkBudget() should return error when just over budget")
	}
}

func TestWriteSummary(t *testing.T) {
	tempDir := t.TempDir()

	config := agent.SwarmConfig{
		SessionID:         "test-write-summary",
		WorkDir:           tempDir,
		SessionDir:        tempDir,
		OrchestratorModel: "sonnet",
		PlannerModel:      "sonnet",
		DesignerModel:     "haiku",
		BuilderModel:      "haiku",
		ReviewerModel:     "haiku",
		TotalBudgetUSD:    1.0,
	}

	orch, err := New(config)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Simulate some cost
	orch.totalCost = 0.05

	// Write summary
	if err := orch.WriteSummary(); err != nil {
		t.Fatalf("WriteSummary() error: %v", err)
	}

	// Check file exists
	summaryPath := filepath.Join(tempDir, "test-write-summary", "summary.json")
	if _, err := os.Stat(summaryPath); os.IsNotExist(err) {
		t.Fatalf("summary.json was not created at %s", summaryPath)
	}

	// Read and verify content
	data, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("failed to read summary.json: %v", err)
	}

	var summary Summary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("failed to unmarshal summary.json: %v", err)
	}

	if summary.SessionID != "test-write-summary" {
		t.Errorf("expected session ID 'test-write-summary', got %q", summary.SessionID)
	}

	if summary.TotalCost != 0.05 {
		t.Errorf("expected total cost 0.05, got %v", summary.TotalCost)
	}
}
