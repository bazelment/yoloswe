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

	for _, name := range []string{"plan", "build", "validate", "ship"} {
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
