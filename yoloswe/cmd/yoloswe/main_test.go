package main

import (
	"os"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

func TestNewPlanCmdDefaults(t *testing.T) {
	t.Parallel()

	cmd := newPlanCmd()
	model, _ := cmd.Flags().GetString("model")
	buildModel, _ := cmd.Flags().GetString("build-model")
	simple, _ := cmd.Flags().GetBool("simple")

	if model != "opus" || buildModel != "sonnet" || simple {
		t.Fatalf("plan command defaults: model=%q build-model=%q simple=%v", model, buildModel, simple)
	}
	if cmd.Use != "plan [flags] <prompt>" {
		t.Fatalf("plan command Use = %q", cmd.Use)
	}
}

func TestNewBuildCmdDefaults(t *testing.T) {
	t.Parallel()

	cmd := newBuildCmd()
	builderModel, _ := cmd.Flags().GetString("builder-model")
	budget, _ := cmd.Flags().GetFloat64("budget")
	timeout, _ := cmd.Flags().GetInt("timeout")
	maxIterations, _ := cmd.Flags().GetInt("max-iterations")
	requireApproval, _ := cmd.Flags().GetBool("require-approval")

	if builderModel != "sonnet" || budget != 100 || timeout != 3600 || maxIterations != 100 || requireApproval {
		t.Fatalf("build command defaults: builder=%q budget=%v timeout=%d max=%d approval=%v",
			builderModel, budget, timeout, maxIterations, requireApproval)
	}
}

func TestNewCodeTalkCmdFlags(t *testing.T) {
	t.Parallel()

	cmd := newCodeTalkCmd()
	if err := cmd.ParseFlags([]string{"--backend", "codex", "--llm-api-key-env", "BASETEN_API_KEY"}); err != nil {
		t.Fatalf("ParseFlags() error = %v", err)
	}

	backend, _ := cmd.Flags().GetString("backend")
	apiKeyEnv, _ := cmd.Flags().GetString("llm-api-key-env")
	if backend != agent.ProviderCodex || apiKeyEnv != "BASETEN_API_KEY" {
		t.Fatalf("codetalk parsed flags: backend=%q apiKeyEnv=%q", backend, apiKeyEnv)
	}

	apiKeyFlag := cmd.Flags().Lookup("llm-api-key")
	if apiKeyFlag == nil {
		t.Fatal("llm-api-key flag is missing")
	}
	if apiKeyFlag.Hidden {
		return
	}
	t.Fatal("llm-api-key flag should be hidden from help output")
}

func TestResolveWorkDir(t *testing.T) {
	t.Parallel()

	if got, err := resolveWorkDir("/tmp/work"); err != nil || got != "/tmp/work" {
		t.Fatalf("resolveWorkDir(explicit) = (%q, %v), want /tmp/work nil", got, err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	got, err := resolveWorkDir("")
	if err != nil {
		t.Fatalf("resolveWorkDir(empty) error = %v", err)
	}
	if got != wd {
		t.Fatalf("resolveWorkDir(empty) = %q, want cwd %q", got, wd)
	}
}

func TestCodeTalkCommandHelpDoesNotShowRawAPIKeyFlag(t *testing.T) {
	t.Parallel()

	cmd := newCodeTalkCmd()
	help := cmd.Flags().FlagUsages()
	if strings.Contains(help, "llm-api-key ") {
		t.Fatalf("help output exposes hidden llm-api-key flag:\n%s", help)
	}
}
