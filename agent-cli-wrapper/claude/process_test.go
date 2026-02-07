package claude

import (
	"strings"
	"testing"
)

func TestBuildCLIArgs_AllowedTools(t *testing.T) {
	config := defaultConfig()
	config.AllowedTools = []string{"Read", "Write", "Bash"}

	pm := newProcessManager(config)
	args, err := pm.BuildCLIArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that each tool appears with --allowed-tools flag
	argsStr := strings.Join(args, " ")
	if !strings.Contains(argsStr, "--allowed-tools Read") {
		t.Error("expected --allowed-tools Read")
	}
	if !strings.Contains(argsStr, "--allowed-tools Write") {
		t.Error("expected --allowed-tools Write")
	}
	if !strings.Contains(argsStr, "--allowed-tools Bash") {
		t.Error("expected --allowed-tools Bash")
	}
}

func TestBuildCLIArgs_DisallowedTools(t *testing.T) {
	config := defaultConfig()
	config.DisallowedTools = []string{"WebSearch", "WebFetch"}

	pm := newProcessManager(config)
	args, err := pm.BuildCLIArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that each tool appears with --disallowed-tools flag
	argsStr := strings.Join(args, " ")
	if !strings.Contains(argsStr, "--disallowed-tools WebSearch") {
		t.Error("expected --disallowed-tools WebSearch")
	}
	if !strings.Contains(argsStr, "--disallowed-tools WebFetch") {
		t.Error("expected --disallowed-tools WebFetch")
	}
}

func TestBuildCLIArgs_Betas(t *testing.T) {
	config := defaultConfig()
	config.Betas = []string{"feature1", "feature2"}

	pm := newProcessManager(config)
	args, err := pm.BuildCLIArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that each beta appears with --beta flag
	argsStr := strings.Join(args, " ")
	if !strings.Contains(argsStr, "--beta feature1") {
		t.Error("expected --beta feature1")
	}
	if !strings.Contains(argsStr, "--beta feature2") {
		t.Error("expected --beta feature2")
	}
}

func TestBuildCLIArgs_Agents(t *testing.T) {
	config := defaultConfig()
	config.Agents = []AgentDefinition{
		{
			Name:        "researcher",
			Description: "Research agent",
			Model:       "opus",
		},
		{
			Name:            "executor",
			AllowedTools:    []string{"Bash"},
			DisallowedTools: []string{"WebFetch"},
		},
	}

	pm := newProcessManager(config)
	args, err := pm.BuildCLIArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that --agents flag is present
	argsStr := strings.Join(args, " ")
	if !strings.Contains(argsStr, "--agents") {
		t.Error("expected --agents flag")
	}

	// Find the --agents flag and check JSON
	agentsIdx := -1
	for i, arg := range args {
		if arg == "--agents" && i+1 < len(args) {
			agentsIdx = i + 1
			break
		}
	}
	if agentsIdx == -1 {
		t.Fatal("--agents flag not found")
	}

	agentsJSON := args[agentsIdx]
	if !strings.Contains(agentsJSON, "researcher") {
		t.Error("expected agents JSON to contain 'researcher'")
	}
	if !strings.Contains(agentsJSON, "executor") {
		t.Error("expected agents JSON to contain 'executor'")
	}
}

func TestBuildCLIArgs_ExtraArgs(t *testing.T) {
	config := defaultConfig()
	config.ExtraArgs = []string{"--custom-flag", "value"}

	pm := newProcessManager(config)
	args, err := pm.BuildCLIArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that extra args are included
	found := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--custom-flag" && args[i+1] == "value" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected --custom-flag value in args")
	}
}

func TestBuildCLIArgs_MultipleOptions(t *testing.T) {
	config := defaultConfig()
	config.Model = "opus"
	config.AllowedTools = []string{"Read"}
	config.DisallowedTools = []string{"Write"}
	config.Betas = []string{"test-feature"}
	config.ExtraArgs = []string{"--debug"}

	pm := newProcessManager(config)
	args, err := pm.BuildCLIArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	argsStr := strings.Join(args, " ")

	// Check all options are present
	if !strings.Contains(argsStr, "--model opus") {
		t.Error("expected --model opus")
	}
	if !strings.Contains(argsStr, "--allowed-tools Read") {
		t.Error("expected --allowed-tools Read")
	}
	if !strings.Contains(argsStr, "--disallowed-tools Write") {
		t.Error("expected --disallowed-tools Write")
	}
	if !strings.Contains(argsStr, "--beta test-feature") {
		t.Error("expected --beta test-feature")
	}
	if !strings.Contains(argsStr, "--debug") {
		t.Error("expected --debug")
	}
}
