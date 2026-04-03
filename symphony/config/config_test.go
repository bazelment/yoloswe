package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bazelment/yoloswe/symphony/model"
)

func TestNewServiceConfig_Defaults(t *testing.T) {
	wf := &model.WorkflowDefinition{
		Config: map[string]any{},
	}
	cfg := NewServiceConfig(wf)

	if cfg.PollIntervalMs != 30000 {
		t.Errorf("PollIntervalMs = %d, want 30000", cfg.PollIntervalMs)
	}
	if cfg.MaxConcurrentAgents != 10 {
		t.Errorf("MaxConcurrentAgents = %d, want 10", cfg.MaxConcurrentAgents)
	}
	if cfg.MaxTurns != 20 {
		t.Errorf("MaxTurns = %d, want 20", cfg.MaxTurns)
	}
	if cfg.MaxRetryBackoffMs != 300000 {
		t.Errorf("MaxRetryBackoffMs = %d, want 300000", cfg.MaxRetryBackoffMs)
	}
	if cfg.HookTimeoutMs != 60000 {
		t.Errorf("HookTimeoutMs = %d, want 60000", cfg.HookTimeoutMs)
	}
	if cfg.AgentCommand != "codex app-server" {
		t.Errorf("AgentCommand = %q, want codex app-server", cfg.AgentCommand)
	}
	if cfg.AgentTurnTimeoutMs != 3600000 {
		t.Errorf("AgentTurnTimeoutMs = %d, want 3600000", cfg.AgentTurnTimeoutMs)
	}
	if cfg.AgentReadTimeoutMs != 5000 {
		t.Errorf("AgentReadTimeoutMs = %d, want 5000", cfg.AgentReadTimeoutMs)
	}
	if cfg.AgentStallTimeoutMs != 300000 {
		t.Errorf("AgentStallTimeoutMs = %d, want 300000", cfg.AgentStallTimeoutMs)
	}
	if cfg.WorkspaceRoot != filepath.Join(os.TempDir(), "symphony_workspaces") {
		t.Errorf("WorkspaceRoot = %q", cfg.WorkspaceRoot)
	}
	if len(cfg.ActiveStates) != 2 || cfg.ActiveStates[0] != "Todo" {
		t.Errorf("ActiveStates = %v", cfg.ActiveStates)
	}
	if len(cfg.TerminalStates) != 5 {
		t.Errorf("TerminalStates = %v", cfg.TerminalStates)
	}
}

func TestNewServiceConfig_EnvResolution(t *testing.T) {
	t.Setenv("TEST_LINEAR_KEY", "secret-key-123")

	wf := &model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":    "linear",
				"api_key": "$TEST_LINEAR_KEY",
			},
		},
	}
	cfg := NewServiceConfig(wf)

	if cfg.TrackerAPIKey != "secret-key-123" {
		t.Errorf("TrackerAPIKey = %q, want secret-key-123", cfg.TrackerAPIKey)
	}
}

func TestNewServiceConfig_EnvResolution_Empty(t *testing.T) {
	t.Setenv("EMPTY_VAR", "")

	wf := &model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"api_key": "$EMPTY_VAR",
			},
		},
	}
	cfg := NewServiceConfig(wf)

	if cfg.TrackerAPIKey != "" {
		t.Errorf("TrackerAPIKey = %q, want empty", cfg.TrackerAPIKey)
	}
}

func TestNewServiceConfig_LinearEndpointDefault(t *testing.T) {
	wf := &model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind": "linear",
			},
		},
	}
	cfg := NewServiceConfig(wf)

	if cfg.TrackerEndpoint != "https://api.linear.app/graphql" {
		t.Errorf("TrackerEndpoint = %q", cfg.TrackerEndpoint)
	}
}

func TestNewServiceConfig_StringIntegers(t *testing.T) {
	wf := &model.WorkflowDefinition{
		Config: map[string]any{
			"polling": map[string]any{
				"interval_ms": "15000",
			},
			"agent": map[string]any{
				"max_concurrent_agents": "5",
			},
		},
	}
	cfg := NewServiceConfig(wf)

	if cfg.PollIntervalMs != 15000 {
		t.Errorf("PollIntervalMs = %d, want 15000", cfg.PollIntervalMs)
	}
	if cfg.MaxConcurrentAgents != 5 {
		t.Errorf("MaxConcurrentAgents = %d, want 5", cfg.MaxConcurrentAgents)
	}
}

func TestNewServiceConfig_HookTimeoutNonPositiveFallback(t *testing.T) {
	wf := &model.WorkflowDefinition{
		Config: map[string]any{
			"hooks": map[string]any{
				"timeout_ms": 0,
			},
		},
	}
	cfg := NewServiceConfig(wf)

	if cfg.HookTimeoutMs != 60000 {
		t.Errorf("HookTimeoutMs = %d, want 60000 (fallback)", cfg.HookTimeoutMs)
	}

	wf2 := &model.WorkflowDefinition{
		Config: map[string]any{
			"hooks": map[string]any{
				"timeout_ms": -1,
			},
		},
	}
	cfg2 := NewServiceConfig(wf2)

	if cfg2.HookTimeoutMs != 60000 {
		t.Errorf("HookTimeoutMs = %d, want 60000 (fallback for negative)", cfg2.HookTimeoutMs)
	}
}

func TestNewServiceConfig_PerStateConcurrency(t *testing.T) {
	wf := &model.WorkflowDefinition{
		Config: map[string]any{
			"agent": map[string]any{
				"max_concurrent_agents_by_state": map[string]any{
					"Todo":        2,
					"In Progress": 5,
					"Invalid":     -1,
					"Zero":        0,
				},
			},
		},
	}
	cfg := NewServiceConfig(wf)

	if cfg.MaxConcurrentByState == nil {
		t.Fatal("MaxConcurrentByState should not be nil")
	}
	if cfg.MaxConcurrentByState["todo"] != 2 {
		t.Errorf("todo = %d, want 2", cfg.MaxConcurrentByState["todo"])
	}
	if cfg.MaxConcurrentByState["in progress"] != 5 {
		t.Errorf("in progress = %d, want 5", cfg.MaxConcurrentByState["in progress"])
	}
	if _, ok := cfg.MaxConcurrentByState["invalid"]; ok {
		t.Error("invalid entry should be ignored")
	}
	if _, ok := cfg.MaxConcurrentByState["zero"]; ok {
		t.Error("zero entry should be ignored")
	}
}

func TestNewServiceConfig_TildeExpansion(t *testing.T) {
	wf := &model.WorkflowDefinition{
		Config: map[string]any{
			"workspace": map[string]any{
				"root": "~/symphony_workspaces",
			},
		},
	}
	cfg := NewServiceConfig(wf)

	home, err := os.UserHomeDir()
	if err != nil {
		// In sandbox environments, home dir may not be available.
		// In that case, tilde should be preserved as-is.
		if cfg.WorkspaceRoot != "~/symphony_workspaces" {
			t.Errorf("WorkspaceRoot = %q, want ~/symphony_workspaces (home unavailable)", cfg.WorkspaceRoot)
		}
		return
	}
	expected := filepath.Join(home, "symphony_workspaces")
	if cfg.WorkspaceRoot != expected {
		t.Errorf("WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, expected)
	}
}

func TestNewServiceConfig_ServerPort(t *testing.T) {
	wf := &model.WorkflowDefinition{
		Config: map[string]any{
			"server": map[string]any{
				"port": 8080,
			},
		},
	}
	cfg := NewServiceConfig(wf)

	if cfg.ServerPort == nil {
		t.Fatal("ServerPort should not be nil")
	}
	if *cfg.ServerPort != 8080 {
		t.Errorf("ServerPort = %d, want 8080", *cfg.ServerPort)
	}
}

func TestNewServiceConfig_AgentSessionEmptyOverridesCodexFallback(t *testing.T) {
	wf := &model.WorkflowDefinition{
		Config: map[string]any{
			"codex": map[string]any{
				"approval_policy": "auto-edit",
			},
			"agent_session": map[string]any{
				"approval_policy": "",
			},
		},
	}
	cfg := NewServiceConfig(wf)

	if cfg.AgentApprovalPolicy != "" {
		t.Errorf("AgentApprovalPolicy = %q, want empty (explicit override)", cfg.AgentApprovalPolicy)
	}
}

func TestNewServiceConfig_AgentSessionIntOverridesCodexFallback(t *testing.T) {
	wf := &model.WorkflowDefinition{
		Config: map[string]any{
			"codex": map[string]any{
				"turn_timeout_ms": 7200000,
			},
			"agent_session": map[string]any{
				"turn_timeout_ms": 1800000,
			},
		},
	}
	cfg := NewServiceConfig(wf)

	if cfg.AgentTurnTimeoutMs != 1800000 {
		t.Errorf("AgentTurnTimeoutMs = %d, want 1800000 (agent_session should override codex)", cfg.AgentTurnTimeoutMs)
	}
}

func TestNewServiceConfig_IntFallbackToCodex(t *testing.T) {
	wf := &model.WorkflowDefinition{
		Config: map[string]any{
			"codex": map[string]any{
				"turn_timeout_ms": 7200000,
			},
		},
	}
	cfg := NewServiceConfig(wf)

	if cfg.AgentTurnTimeoutMs != 7200000 {
		t.Errorf("AgentTurnTimeoutMs = %d, want 7200000 (should fall back to codex)", cfg.AgentTurnTimeoutMs)
	}
}

func TestNewServiceConfig_NoServerPort(t *testing.T) {
	wf := &model.WorkflowDefinition{
		Config: map[string]any{},
	}
	cfg := NewServiceConfig(wf)

	if cfg.ServerPort != nil {
		t.Errorf("ServerPort should be nil, got %v", cfg.ServerPort)
	}
}
