package planner

import (
	"errors"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

func TestNew(t *testing.T) {
	cfg := Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "sonnet",
			WorkDir:    ".",
			SessionDir: "/tmp/test-sessions",
		},
		DesignerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: "/tmp/test-sessions",
		},
		BuilderConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: "/tmp/test-sessions",
		},
		ReviewerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: "/tmp/test-sessions",
		},
	}

	p := New(cfg, "test-session")

	if p.Role() != agent.RolePlanner {
		t.Errorf("expected role %v, got %v", agent.RolePlanner, p.Role())
	}

	if p.TotalCost() != 0 {
		t.Errorf("expected initial cost 0, got %v", p.TotalCost())
	}

	if p.TurnCount() != 0 {
		t.Errorf("expected initial turn count 0, got %v", p.TurnCount())
	}
}

func TestFormatMissionMessage(t *testing.T) {
	mission := "Build a hello world CLI in Go"
	msg := formatMissionMessage(mission)

	if !containsString(msg, mission) {
		t.Errorf("message should contain mission, got:\n%s", msg)
	}

	if !containsString(msg, "Mission") {
		t.Errorf("message should contain 'Mission', got:\n%s", msg)
	}
}

func TestFormatDesignPrompt(t *testing.T) {
	tests := []struct {
		name     string
		req      *protocol.DesignRequest
		contains []string
	}{
		{
			name: "basic request",
			req: &protocol.DesignRequest{
				Task: "Create a CLI",
			},
			contains: []string{"Task:", "Create a CLI"},
		},
		{
			name: "with context",
			req: &protocol.DesignRequest{
				Task:    "Add feature",
				Context: "Existing Go project",
			},
			contains: []string{"Task:", "Context:", "Existing Go project"},
		},
		{
			name: "with constraints",
			req: &protocol.DesignRequest{
				Task:        "Build API",
				Constraints: []string{"Use REST", "JSON only"},
			},
			contains: []string{"Task:", "Constraints:", "Use REST", "JSON only"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := formatDesignPrompt(tt.req)

			for _, s := range tt.contains {
				if !containsString(prompt, s) {
					t.Errorf("prompt should contain %q, got:\n%s", s, prompt)
				}
			}
		})
	}
}

func TestFormatBuildPrompt(t *testing.T) {
	tests := []struct {
		name     string
		req      *protocol.BuildRequest
		contains []string
	}{
		{
			name: "basic request",
			req: &protocol.BuildRequest{
				Task:    "Implement feature",
				WorkDir: "/tmp/project",
			},
			contains: []string{"Task:", "Implement feature", "Working Directory:", "/tmp/project"},
		},
		{
			name: "with design",
			req: &protocol.BuildRequest{
				Task:    "Build it",
				WorkDir: ".",
				Design: &protocol.DesignResponse{
					Architecture: "Layered architecture",
				},
			},
			contains: []string{"Design:", "Layered architecture"},
		},
		{
			name: "with feedback",
			req: &protocol.BuildRequest{
				Task:    "Fix bugs",
				WorkDir: ".",
				Feedback: &protocol.ReviewResponse{
					Issues: []protocol.Issue{
						{Severity: "critical", File: "main.go", Message: "Null pointer"},
					},
				},
			},
			contains: []string{"Feedback", "critical", "Null pointer"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := formatBuildPrompt(tt.req)

			for _, s := range tt.contains {
				if !containsString(prompt, s) {
					t.Errorf("prompt should contain %q, got:\n%s", s, prompt)
				}
			}
		})
	}
}

func TestFormatReviewPrompt(t *testing.T) {
	tests := []struct {
		name     string
		req      *protocol.ReviewRequest
		contains []string
	}{
		{
			name: "basic request",
			req: &protocol.ReviewRequest{
				Task:         "Review changes",
				FilesChanged: []string{"main.go", "util.go"},
			},
			contains: []string{"Task:", "Review changes", "main.go", "util.go"},
		},
		{
			name: "with design",
			req: &protocol.ReviewRequest{
				Task:         "Code review",
				FilesChanged: []string{"api.go"},
				OriginalDesign: &protocol.DesignResponse{
					Architecture: "RESTful design",
				},
			},
			contains: []string{"Original Design:", "RESTful design"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := formatReviewPrompt(tt.req)

			for _, s := range tt.contains {
				if !containsString(prompt, s) {
					t.Errorf("prompt should contain %q, got:\n%s", s, prompt)
				}
			}
		})
	}
}

func containsString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestCheckIterations_UnderLimit(t *testing.T) {
	cfg := Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "sonnet",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		DesignerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		BuilderConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		ReviewerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		MaxIterations: 10,
	}

	p := New(cfg, "test-session")

	// Iteration count is 0, max is 10 - should pass
	if err := p.checkIterations(); err != nil {
		t.Errorf("checkIterations() should pass when under limit, got error: %v", err)
	}

	// Simulate some iterations
	p.iterationCount = 5
	if err := p.checkIterations(); err != nil {
		t.Errorf("checkIterations() should pass when at 5/10, got error: %v", err)
	}
}

func TestCheckIterations_NoLimit(t *testing.T) {
	cfg := Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "sonnet",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		DesignerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		BuilderConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		ReviewerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		MaxIterations: 0, // No limit
	}

	p := New(cfg, "test-session")

	// No limit - should always pass
	if err := p.checkIterations(); err != nil {
		t.Errorf("checkIterations() should pass when no limit, got error: %v", err)
	}

	// Even with high iteration count, should pass
	p.iterationCount = 1000
	if err := p.checkIterations(); err != nil {
		t.Errorf("checkIterations() should pass even with high count when no limit, got error: %v", err)
	}
}

func TestCheckIterations_AtLimit(t *testing.T) {
	cfg := Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "sonnet",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		DesignerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		BuilderConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		ReviewerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		MaxIterations: 5,
	}

	p := New(cfg, "test-session")

	// Set iteration count to max
	p.iterationCount = 5

	// At max (>= limit) should fail
	err := p.checkIterations()
	if err == nil {
		t.Error("checkIterations() should return error when at limit")
	}

	if !errors.Is(err, ErrMaxIterationsExceeded) {
		t.Errorf("expected ErrMaxIterationsExceeded, got: %v", err)
	}
}

func TestCheckIterations_OverLimit(t *testing.T) {
	cfg := Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "sonnet",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		DesignerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		BuilderConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		ReviewerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		MaxIterations: 3,
	}

	p := New(cfg, "test-session")

	// Set iteration count over max
	p.iterationCount = 10

	err := p.checkIterations()
	if err == nil {
		t.Error("checkIterations() should return error when over limit")
	}

	if !errors.Is(err, ErrMaxIterationsExceeded) {
		t.Errorf("expected ErrMaxIterationsExceeded, got: %v", err)
	}
}

func TestIterationCount(t *testing.T) {
	cfg := Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "sonnet",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		DesignerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		BuilderConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		ReviewerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    ".",
			SessionDir: t.TempDir(),
		},
		MaxIterations: 10,
	}

	p := New(cfg, "test-session")

	// Initial count should be 0
	if p.IterationCount() != 0 {
		t.Errorf("expected initial iteration count 0, got %d", p.IterationCount())
	}

	// Increment and check
	p.incrementIterations()
	if p.IterationCount() != 1 {
		t.Errorf("expected iteration count 1, got %d", p.IterationCount())
	}

	p.incrementIterations()
	p.incrementIterations()
	if p.IterationCount() != 3 {
		t.Errorf("expected iteration count 3, got %d", p.IterationCount())
	}
}

func TestStateMachine(t *testing.T) {
	sm := NewStateMachine()

	// Initial state should be Idle
	if sm.State() != StateIdle {
		t.Errorf("expected initial state Idle, got %v", sm.State())
	}

	// Test valid transition: Idle -> Planning
	err := sm.Transition(StatePlanning, "test")
	if err != nil {
		t.Errorf("expected valid transition Idle->Planning, got error: %v", err)
	}
	if sm.State() != StatePlanning {
		t.Errorf("expected state Planning after transition, got %v", sm.State())
	}

	// Test invalid transition: Planning -> Idle (not allowed)
	err = sm.Transition(StateIdle, "test")
	if err == nil {
		t.Error("expected error for invalid transition Planning->Idle, got nil")
	}

	// Test valid transition: Planning -> Designing
	err = sm.Transition(StateDesigning, "test")
	if err != nil {
		t.Errorf("expected valid transition Planning->Designing, got error: %v", err)
	}

	// Test valid transition: Designing -> Building
	err = sm.Transition(StateBuilding, "test")
	if err != nil {
		t.Errorf("expected valid transition Designing->Building, got error: %v", err)
	}

	// Test valid transition: Building -> Reviewing
	err = sm.Transition(StateReviewing, "test")
	if err != nil {
		t.Errorf("expected valid transition Building->Reviewing, got error: %v", err)
	}

	// Test valid transition: Reviewing -> Completed
	err = sm.Transition(StateCompleted, "test")
	if err != nil {
		t.Errorf("expected valid transition Reviewing->Completed, got error: %v", err)
	}

	// Test terminal state methods
	if !sm.State().IsTerminal() {
		t.Error("expected Completed to be terminal state")
	}

	// Test history tracking
	history := sm.History()
	if len(history) != 5 {
		t.Errorf("expected 5 transitions in history, got %d", len(history))
	}

	// Test reset
	sm.Reset()
	if sm.State() != StateIdle {
		t.Errorf("expected state Idle after reset, got %v", sm.State())
	}
}

func TestStateMachineFailureTransitions(t *testing.T) {
	sm := NewStateMachine()

	// Test failure transition from Planning
	sm.Transition(StatePlanning, "start")
	err := sm.Transition(StateFailed, "error")
	if err != nil {
		t.Errorf("expected valid transition Planning->Failed, got error: %v", err)
	}
	if sm.State() != StateFailed {
		t.Errorf("expected state Failed, got %v", sm.State())
	}

	// Test that Failed is terminal
	if !sm.State().IsTerminal() {
		t.Error("expected Failed to be terminal state")
	}

	// Test reset from Failed
	sm.Reset()
	if sm.State() != StateIdle {
		t.Errorf("expected state Idle after reset, got %v", sm.State())
	}
}

func TestSessionStats(t *testing.T) {
	var stats SessionStats

	// Initial state
	if stats.TotalTokens() != 0 {
		t.Errorf("expected 0 total tokens initially, got %d", stats.TotalTokens())
	}
	if stats.CostUSD != 0 {
		t.Errorf("expected 0 cost initially, got %f", stats.CostUSD)
	}

	// Add usage
	stats.Add(claude.TurnUsage{
		InputTokens:     100,
		OutputTokens:    50,
		CacheReadTokens: 20,
		CostUSD:         0.05,
	})

	if stats.InputTokens != 100 {
		t.Errorf("expected 100 input tokens, got %d", stats.InputTokens)
	}
	if stats.OutputTokens != 50 {
		t.Errorf("expected 50 output tokens, got %d", stats.OutputTokens)
	}
	if stats.TotalTokens() != 150 {
		t.Errorf("expected 150 total tokens, got %d", stats.TotalTokens())
	}
	if stats.CostUSD != 0.05 {
		t.Errorf("expected 0.05 cost, got %f", stats.CostUSD)
	}
	if stats.TurnCount != 1 {
		t.Errorf("expected 1 turn, got %d", stats.TurnCount)
	}

	// Add more usage
	stats.Add(claude.TurnUsage{
		InputTokens:  50,
		OutputTokens: 25,
		CostUSD:      0.03,
	})

	if stats.TotalTokens() != 225 {
		t.Errorf("expected 225 total tokens, got %d", stats.TotalTokens())
	}
	if stats.CostUSD != 0.08 {
		t.Errorf("expected 0.08 cost, got %f", stats.CostUSD)
	}
	if stats.TurnCount != 2 {
		t.Errorf("expected 2 turns, got %d", stats.TurnCount)
	}

	// Test reset
	stats.Reset()
	if stats.TotalTokens() != 0 {
		t.Errorf("expected 0 tokens after reset, got %d", stats.TotalTokens())
	}
}

func TestPhaseStats(t *testing.T) {
	var ps PhaseStats

	// Add stats to different phases
	ps.AddForPhase(StatePlanning, claude.TurnUsage{
		InputTokens:  100,
		OutputTokens: 50,
		CostUSD:      0.05,
	})

	ps.AddForPhase(StateBuilding, claude.TurnUsage{
		InputTokens:  200,
		OutputTokens: 100,
		CostUSD:      0.10,
	})

	ps.AddForPhase(StateReviewing, claude.TurnUsage{
		InputTokens:  150,
		OutputTokens: 75,
		CostUSD:      0.08,
	})

	// Test total calculation
	total := ps.Total()
	if total.InputTokens != 450 {
		t.Errorf("expected 450 total input tokens, got %d", total.InputTokens)
	}
	if total.OutputTokens != 225 {
		t.Errorf("expected 225 total output tokens, got %d", total.OutputTokens)
	}

	// Test cost aggregation (use approximate comparison for floats)
	totalCost := ps.TotalCostUSD()
	expectedCost := 0.23
	if totalCost < expectedCost-0.0001 || totalCost > expectedCost+0.0001 {
		t.Errorf("expected approximately 0.23 total cost, got %f", totalCost)
	}

	// Test turn count
	totalTurns := ps.TotalTurns()
	if totalTurns != 3 {
		t.Errorf("expected 3 total turns, got %d", totalTurns)
	}
}

func TestIterationConfig(t *testing.T) {
	cfg := DefaultIterationConfig()

	if cfg.MaxIterations != 10 {
		t.Errorf("expected default max iterations 10, got %d", cfg.MaxIterations)
	}
	if cfg.MaxBudgetUSD != 5.0 {
		t.Errorf("expected default max budget 5.0, got %f", cfg.MaxBudgetUSD)
	}
	if cfg.AutoApprove != true {
		t.Error("expected default auto approve true")
	}
}

func TestExitReason(t *testing.T) {
	tests := []struct {
		reason    ExitReason
		isSuccess bool
	}{
		{ExitReasonAccepted, true},
		{ExitReasonBudgetExceeded, false},
		{ExitReasonTimeExceeded, false},
		{ExitReasonMaxIterations, false},
		{ExitReasonError, false},
		{ExitReasonInterrupt, false},
	}

	for _, tt := range tests {
		if tt.reason.IsSuccess() != tt.isSuccess {
			t.Errorf("expected %v.IsSuccess() = %v, got %v", tt.reason, tt.isSuccess, tt.reason.IsSuccess())
		}
	}
}

func TestFormatFeedbackForBuilder(t *testing.T) {
	// Test nil review
	feedback := FormatFeedbackForBuilder(nil)
	if feedback != "" {
		t.Errorf("expected empty feedback for nil review, got: %s", feedback)
	}

	// Test review with no issues
	review := &protocol.ReviewResponse{
		Summary: "Looks good",
		Issues:  []protocol.Issue{},
	}
	feedback = FormatFeedbackForBuilder(review)
	if feedback != "Looks good" {
		t.Errorf("expected summary only, got: %s", feedback)
	}

	// Test review with issues
	review = &protocol.ReviewResponse{
		Summary: "Needs fixes",
		Issues: []protocol.Issue{
			{
				Severity:   "high",
				Message:    "Missing error handling",
				File:       "main.go",
				Line:       42,
				Suggestion: "Add error check",
			},
			{
				Severity: "low",
				Message:  "Consider refactoring",
			},
		},
	}
	feedback = FormatFeedbackForBuilder(review)
	if feedback == "" {
		t.Error("expected non-empty feedback")
	}
	// Should contain issue count, severity, message
	if !contains(feedback, "Missing error handling") {
		t.Error("feedback should contain issue message")
	}
	if !contains(feedback, "main.go") {
		t.Error("feedback should contain file name")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && indexAny(s, substr) >= 0)
}

func indexAny(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
