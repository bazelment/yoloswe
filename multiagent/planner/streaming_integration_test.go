package planner

import (
	"testing"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

func TestNewStreamingPlanner(t *testing.T) {
	tempDir := t.TempDir()

	cfg := Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "sonnet",
			WorkDir:    tempDir,
			SessionDir: tempDir,
		},
		DesignerConfig: agent.AgentConfig{
			Role:    agent.RoleDesigner,
			Model:   "haiku",
			WorkDir: tempDir,
		},
		BuilderConfig: agent.AgentConfig{
			Role:    agent.RoleBuilder,
			Model:   "haiku",
			WorkDir: tempDir,
		},
		ReviewerConfig: agent.AgentConfig{
			Role:    agent.RoleReviewer,
			Model:   "haiku",
			WorkDir: tempDir,
		},
	}

	p := New(cfg, "test-session")
	sp := NewStreamingPlanner(p)

	if sp.Planner == nil {
		t.Error("expected Planner to be set")
	}
	if sp.activeSubAgents == nil {
		t.Error("expected activeSubAgents to be initialized")
	}
	if sp.ActiveSubAgentCount() != 0 {
		t.Errorf("expected 0 active sub-agents, got %d", sp.ActiveSubAgentCount())
	}
}

func TestStreamingPlanner_CancelNonexistentSubAgent(t *testing.T) {
	tempDir := t.TempDir()

	cfg := Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "sonnet",
			WorkDir:    tempDir,
			SessionDir: tempDir,
		},
	}

	p := New(cfg, "test-session")
	sp := NewStreamingPlanner(p)

	err := sp.CancelSubAgent("nonexistent-id", "test cancel")

	if err == nil {
		t.Error("expected error for nonexistent sub-agent")
	}
}

func TestStreamingPlanner_CancelAllSubAgents_Empty(t *testing.T) {
	tempDir := t.TempDir()

	cfg := Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "sonnet",
			WorkDir:    tempDir,
			SessionDir: tempDir,
		},
	}

	p := New(cfg, "test-session")
	sp := NewStreamingPlanner(p)

	// Should not panic with no active sub-agents
	sp.CancelAllSubAgents("shutdown")
}

func TestGenerateRequestID(t *testing.T) {
	id1 := generateRequestID()
	id2 := generateRequestID()
	id3 := generateRequestID()

	if id1 == id2 || id2 == id3 || id1 == id3 {
		t.Error("expected unique request IDs")
	}

	// Verify format
	if len(id1) < 5 {
		t.Errorf("expected request ID to have reasonable length, got %q", id1)
	}
}

func TestStreamingPlanner_IterationLimitEnforcement(t *testing.T) {
	tempDir := t.TempDir()

	cfg := Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "sonnet",
			WorkDir:    tempDir,
			SessionDir: tempDir,
		},
		MaxIterations: 3, // Low limit for testing
	}

	p := New(cfg, "test-session")
	sp := NewStreamingPlanner(p)

	// Manually set iteration count to max
	sp.mu.Lock()
	sp.iterationCount = 3
	sp.mu.Unlock()

	// Next call should fail
	err := sp.checkIterations()
	if err == nil {
		t.Error("expected error when iteration limit reached")
	}
}
