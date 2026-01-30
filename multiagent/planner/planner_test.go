package planner

import (
	"errors"
	"testing"

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
