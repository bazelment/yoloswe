package jiradozer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_ValidComplete(t *testing.T) {
	cfg, err := LoadConfig("testdata/valid_complete.yaml")
	require.NoError(t, err)

	assert.Equal(t, "linear", cfg.Tracker.Kind)
	assert.Equal(t, "test-api-key-123", cfg.Tracker.APIKey)
	assert.Equal(t, "sonnet", cfg.Agent.Model)
	assert.Equal(t, "main", cfg.BaseBranch)
	assert.Equal(t, 30*time.Second, cfg.PollInterval)
	assert.Equal(t, 100.0, cfg.MaxBudgetUSD)

	// States
	assert.Equal(t, "Started", cfg.States.InProgress)
	assert.Equal(t, "Review", cfg.States.InReview)
	assert.Equal(t, "Completed", cfg.States.Done)

	// Plan step overrides
	assert.Equal(t, "Plan for {{.Identifier}}: {{.Title}}", cfg.Plan.Prompt)
	assert.Equal(t, "You are a planner.", cfg.Plan.SystemPrompt)
	assert.Equal(t, "opus", cfg.Plan.Model)
	assert.Equal(t, "plan", cfg.Plan.PermissionMode)
	assert.Equal(t, 5, cfg.Plan.MaxTurns)
	assert.Equal(t, 20.0, cfg.Plan.MaxBudgetUSD)
}

func TestLoadConfig_ValidMinimal(t *testing.T) {
	cfg, err := LoadConfig("testdata/valid_minimal.yaml")
	require.NoError(t, err)

	// Should have defaults applied.
	assert.Equal(t, "linear", cfg.Tracker.Kind)
	assert.Equal(t, "test-key", cfg.Tracker.APIKey)
	assert.Equal(t, "sonnet", cfg.Agent.Model)
	assert.Equal(t, ".", cfg.WorkDir)
	assert.Equal(t, "main", cfg.BaseBranch)
	assert.Equal(t, 15*time.Second, cfg.PollInterval)
	assert.Equal(t, 50.0, cfg.MaxBudgetUSD)

	// Default states
	assert.Equal(t, "In Progress", cfg.States.InProgress)
	assert.Equal(t, "In Review", cfg.States.InReview)
	assert.Equal(t, "Done", cfg.States.Done)

	// Default step configs
	assert.Equal(t, "plan", cfg.Plan.PermissionMode)
	assert.Equal(t, 10, cfg.Plan.MaxTurns)
	assert.Equal(t, "bypass", cfg.Build.PermissionMode)
	assert.Equal(t, 30, cfg.Build.MaxTurns)
}

func TestLoadConfig_EnvVarExpansion(t *testing.T) {
	t.Setenv("JIRADOZER_TEST_API_KEY", "expanded-secret-key")
	cfg, err := LoadConfig("testdata/env_var_api_key.yaml")
	require.NoError(t, err)
	assert.Equal(t, "expanded-secret-key", cfg.Tracker.APIKey)
}

func TestLoadConfig_EnvVarNotSet(t *testing.T) {
	// Ensure the env var is not set.
	t.Setenv("JIRADOZER_TEST_API_KEY", "")
	os.Unsetenv("JIRADOZER_TEST_API_KEY")

	_, err := LoadConfig("testdata/env_var_api_key.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JIRADOZER_TEST_API_KEY")
	assert.Contains(t, err.Error(), "not set")
}

func TestLoadConfig_MissingAPIKey(t *testing.T) {
	_, err := LoadConfig("testdata/missing_api_key.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_key")
}

func TestLoadConfig_MissingTrackerKind(t *testing.T) {
	_, err := LoadConfig("testdata/missing_tracker_kind.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracker.kind")
}

func TestLoadConfig_InvalidTemplate(t *testing.T) {
	_, err := LoadConfig("testdata/invalid_template.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "template")
}

func TestLoadConfig_NonexistentFile(t *testing.T) {
	_, err := LoadConfig("testdata/does_not_exist.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read config")
}

func TestLoadConfig_InvalidWorkDir(t *testing.T) {
	// Create a temp YAML referencing a non-existent directory.
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
work_dir: /nonexistent/path/that/does/not/exist
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "work_dir")
}

func TestLoadConfig_WorkDirIsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	require.NoError(t, os.WriteFile(file, []byte("hello"), 0644))

	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
work_dir: ` + file + `
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestLoadConfig_PollIntervalZeroGetsDefault(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
poll_interval: 0s
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, 15*time.Second, cfg.PollInterval)
}

func TestLoadConfig_GitHubTrackerNoAPIKey(t *testing.T) {
	cfg, err := LoadConfig("testdata/valid_github.yaml")
	require.NoError(t, err)
	assert.Equal(t, "github", cfg.Tracker.Kind)
	assert.Empty(t, cfg.Tracker.APIKey)
}

func TestResolveStep_InheritsFromTopLevel(t *testing.T) {
	cfg, err := LoadConfig("testdata/with_overrides.yaml")
	require.NoError(t, err)

	// Plan step has its own model and budget.
	plan := cfg.ResolveStep(cfg.Plan)
	assert.Equal(t, "opus", plan.Model)
	assert.Equal(t, 10.0, plan.MaxBudgetUSD)
	assert.Equal(t, 5, plan.MaxTurns)

	// Build step has empty model and zero budget — should inherit.
	build := cfg.ResolveStep(cfg.Build)
	assert.Equal(t, "sonnet", build.Model)
	assert.Equal(t, 50.0, build.MaxBudgetUSD)
}

func TestStepByName(t *testing.T) {
	cfg, err := LoadConfig("testdata/valid_complete.yaml")
	require.NoError(t, err)

	for _, name := range []string{"plan", "build", "create_pr", "validate", "ship"} {
		step, ok := cfg.StepByName(name)
		assert.True(t, ok, "step %s should exist", name)
		assert.NotZero(t, step)
	}

	_, ok := cfg.StepByName("unknown")
	assert.False(t, ok)
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		// In sandboxed environments $HOME may not be set.
		// Verify ~ is passed through when home dir unavailable.
		assert.Equal(t, "~/projects", ExpandHome("~/projects"))
		assert.Equal(t, "~", ExpandHome("~"))
	} else {
		assert.Equal(t, filepath.Join(home, "/projects"), ExpandHome("~/projects"))
		assert.Equal(t, home, ExpandHome("~"))
	}
	assert.Equal(t, "/absolute/path", ExpandHome("/absolute/path"))
	assert.Equal(t, "relative/path", ExpandHome("relative/path"))
	assert.Equal(t, "~user/not-expanded", ExpandHome("~user/not-expanded"))
}

func TestValidateWorkDir(t *testing.T) {
	// "." and "" are always valid.
	assert.NoError(t, ValidateWorkDir("."))
	assert.NoError(t, ValidateWorkDir(""))

	// Existing directory.
	dir := t.TempDir()
	assert.NoError(t, ValidateWorkDir(dir))

	// Non-existent path.
	err := ValidateWorkDir("/nonexistent/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "work_dir")

	// Path that is a file, not a directory.
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0644))
	err = ValidateWorkDir(file)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestLoadConfig_WithRounds(t *testing.T) {
	cfg, err := LoadConfig("testdata/with_rounds.yaml")
	require.NoError(t, err)

	// Validate step should have rounds, not a prompt.
	assert.Empty(t, cfg.Validate.Prompt)
	require.Len(t, cfg.Validate.Rounds, 2)
	assert.Contains(t, cfg.Validate.Rounds[0].Prompt, "simplify")
	assert.Equal(t, 15, cfg.Validate.Rounds[0].MaxTurns)
	assert.Contains(t, cfg.Validate.Rounds[1].Prompt, "Run tests")
	assert.Equal(t, "opus", cfg.Validate.Rounds[1].Model)
	assert.Equal(t, 30.0, cfg.Validate.Rounds[1].MaxBudgetUSD)

	// Other steps should have no rounds.
	assert.Empty(t, cfg.Plan.Rounds)
	assert.Empty(t, cfg.Build.Rounds)
	assert.Empty(t, cfg.Ship.Rounds)
}

func TestLoadConfig_RoundsAndPromptConflict(t *testing.T) {
	_, err := LoadConfig("testdata/rounds_and_prompt_conflict.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestLoadConfig_RoundsEmptyPrompt(t *testing.T) {
	_, err := LoadConfig("testdata/rounds_empty_prompt.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt or command is required")
}

func TestLoadConfig_CreatePRRoundsRejected(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
create_pr:
  rounds:
    - command: "echo hello"
`
	path := dir + "/cfg.yaml"
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create_pr does not support rounds")
}

func TestLoadConfig_RoundsInvalidTemplate(t *testing.T) {
	_, err := LoadConfig("testdata/rounds_invalid_template.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "template")
}

func TestResolveRound_InheritsFromStep(t *testing.T) {
	parent := StepConfig{
		Model:          "sonnet",
		SystemPrompt:   "parent system prompt",
		PermissionMode: "bypass",
		MaxTurns:       10,
		MaxBudgetUSD:   25.0,
	}

	// Round with no overrides — inherits everything.
	round := RoundConfig{Prompt: "do stuff"}
	resolved := ResolveRound(round, parent)
	assert.Equal(t, "do stuff", resolved.Prompt)
	assert.Equal(t, "parent system prompt", resolved.SystemPrompt)
	assert.Equal(t, "sonnet", resolved.Model)
	assert.Equal(t, "bypass", resolved.PermissionMode)
	assert.Equal(t, 10, resolved.MaxTurns)
	assert.Equal(t, 25.0, resolved.MaxBudgetUSD)
}

func TestResolveRound_OverridesStep(t *testing.T) {
	parent := StepConfig{
		Model:          "sonnet",
		PermissionMode: "bypass",
		MaxTurns:       10,
		MaxBudgetUSD:   25.0,
	}

	round := RoundConfig{
		Prompt:       "do stuff",
		SystemPrompt: "be careful",
		Model:        "opus",
		MaxTurns:     20,
		MaxBudgetUSD: 50.0,
	}
	resolved := ResolveRound(round, parent)
	assert.Equal(t, "do stuff", resolved.Prompt)
	assert.Equal(t, "be careful", resolved.SystemPrompt)
	assert.Equal(t, "opus", resolved.Model)
	assert.Equal(t, 20, resolved.MaxTurns)
	assert.Equal(t, 50.0, resolved.MaxBudgetUSD)
	assert.Equal(t, "bypass", resolved.PermissionMode) // always from parent
}

func TestLoadConfig_WithCommandRounds(t *testing.T) {
	cfg, err := LoadConfig("testdata/with_command_rounds.yaml")
	require.NoError(t, err)

	require.Len(t, cfg.Build.Rounds, 2)
	assert.Equal(t, "git pull origin {{.BaseBranch}}", cfg.Build.Rounds[0].Command)
	assert.Empty(t, cfg.Build.Rounds[0].Prompt)
	assert.True(t, cfg.Build.Rounds[0].IsCommand())

	assert.Empty(t, cfg.Build.Rounds[1].Command)
	assert.NotEmpty(t, cfg.Build.Rounds[1].Prompt)
	assert.False(t, cfg.Build.Rounds[1].IsCommand())
}

func TestLoadConfig_RoundCommandAndPromptConflict(t *testing.T) {
	_, err := LoadConfig("testdata/round_command_and_prompt.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestLoadConfig_RoundNoPromptNoCommand(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
build:
  rounds:
    - max_turns: 5
`
	path := dir + "/cfg.yaml"
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt or command is required")
}

func TestRoundConfig_IsCommand(t *testing.T) {
	assert.True(t, RoundConfig{Command: "echo hello"}.IsCommand())
	assert.False(t, RoundConfig{Prompt: "do stuff"}.IsCommand())
	assert.False(t, RoundConfig{}.IsCommand())
}

func TestResolveEnv(t *testing.T) {
	t.Setenv("TEST_RESOLVE_ENV_VAR", "secret-value")

	val, err := resolveEnv("$TEST_RESOLVE_ENV_VAR")
	require.NoError(t, err)
	assert.Equal(t, "secret-value", val)

	// Non-env values pass through.
	val, err = resolveEnv("literal-value")
	require.NoError(t, err)
	assert.Equal(t, "literal-value", val)

	// Missing env var.
	os.Unsetenv("UNSET_VAR_FOR_TEST")
	_, err = resolveEnv("$UNSET_VAR_FOR_TEST")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNSET_VAR_FOR_TEST")
}
