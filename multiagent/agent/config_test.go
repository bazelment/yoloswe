package agent

import (
	"testing"
	"time"
)

func TestAgentRoleString(t *testing.T) {
	tests := []struct {
		role AgentRole
		want string
	}{
		{RoleOrchestrator, "orchestrator"},
		{RolePlanner, "planner"},
		{RoleDesigner, "designer"},
		{RoleBuilder, "builder"},
		{RoleReviewer, "reviewer"},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			if got := tt.role.String(); got != tt.want {
				t.Errorf("String() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAgentRoleIsLongRunning(t *testing.T) {
	tests := []struct {
		role AgentRole
		want bool
	}{
		{RoleOrchestrator, true},
		{RolePlanner, true},
		{RoleDesigner, false},
		{RoleBuilder, false},
		{RoleReviewer, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			if got := tt.role.IsLongRunning(); got != tt.want {
				t.Errorf("IsLongRunning() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	tests := []struct {
		role AgentRole
	}{
		{RoleOrchestrator},
		{RolePlanner},
		{RoleDesigner},
		{RoleBuilder},
		{RoleReviewer},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			config := DefaultConfig(tt.role)

			if config.Role != tt.role {
				t.Errorf("Role = %v, want %v", config.Role, tt.role)
			}

			if config.Model != "sonnet" {
				t.Errorf("Model = %v, want sonnet", config.Model)
			}

			if config.WorkDir != "." {
				t.Errorf("WorkDir = %v, want .", config.WorkDir)
			}

			if config.SessionDir != ".claude-swarm/sessions" {
				t.Errorf("SessionDir = %v, want .claude-swarm/sessions", config.SessionDir)
			}

			if config.MaxTurnsPerTask != 10 {
				t.Errorf("MaxTurnsPerTask = %v, want 10", config.MaxTurnsPerTask)
			}

			if config.TurnTimeout != 5*time.Minute {
				t.Errorf("TurnTimeout = %v, want 5m", config.TurnTimeout)
			}

			if config.BudgetUSD != 0 {
				t.Errorf("BudgetUSD = %v, want 0", config.BudgetUSD)
			}
		})
	}
}

func TestDefaultSwarmConfig(t *testing.T) {
	config := DefaultSwarmConfig()

	if config.WorkDir != "." {
		t.Errorf("WorkDir = %v, want .", config.WorkDir)
	}

	if config.SessionDir != ".claude-swarm/sessions" {
		t.Errorf("SessionDir = %v, want .claude-swarm/sessions", config.SessionDir)
	}

	if config.OrchestratorModel != "sonnet" {
		t.Errorf("OrchestratorModel = %v, want sonnet", config.OrchestratorModel)
	}

	if config.PlannerModel != "sonnet" {
		t.Errorf("PlannerModel = %v, want sonnet", config.PlannerModel)
	}

	if config.DesignerModel != "sonnet" {
		t.Errorf("DesignerModel = %v, want sonnet", config.DesignerModel)
	}

	if config.BuilderModel != "sonnet" {
		t.Errorf("BuilderModel = %v, want sonnet", config.BuilderModel)
	}

	if config.ReviewerModel != "haiku" {
		t.Errorf("ReviewerModel = %v, want haiku", config.ReviewerModel)
	}

	if config.TotalBudgetUSD != 1.0 {
		t.Errorf("TotalBudgetUSD = %v, want 1.0", config.TotalBudgetUSD)
	}

	if config.MaxIterations != 50 {
		t.Errorf("MaxIterations = %v, want 50", config.MaxIterations)
	}
}
