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
	// Pin to the prompt parse failure: every other step is well-formed so
	// the load reaches plan's "{{.Identifier} missing closing brace" rather
	// than tripping on a missing comment_template elsewhere.
	assert.Contains(t, err.Error(), "plan.prompt template")
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
` + minimalSteps()
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, 15*time.Second, cfg.PollInterval)
}

func TestConfig_SkipPhases_ValidatesNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
skip_phases: [plan, not_a_phase]
` + minimalSteps()
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skip_phases")
	assert.Contains(t, err.Error(), "not_a_phase")
	assert.Contains(t, err.Error(), "plan, build, validate, ship")
}

// minimalSteps returns a YAML block populating the prompt and
// comment_template fields jiradozer requires for every named step. Useful
// for inline-YAML test cases that exercise something *other than* the
// step-validation rules but still must satisfy the bootstrap-or-die check.
func minimalSteps() string {
	return `
plan:
  prompt: "Plan {{.Identifier}}"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
build:
  prompt: "Build {{.Identifier}}"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
create_pr:
  prompt: "Open PR"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
validate:
  prompt: "Run tests"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
ship:
  prompt: "Ship it"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
`
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

	// Plan step has its own model, effort, and budget.
	plan := cfg.ResolveStep(cfg.Plan)
	assert.Equal(t, "opus", plan.Model)
	assert.Equal(t, "high", plan.Effort)
	assert.Equal(t, 10.0, plan.MaxBudgetUSD)
	assert.Equal(t, 5, plan.MaxTurns)

	// Build step has empty fields — should inherit from agent defaults.
	build := cfg.ResolveStep(cfg.Build)
	assert.Equal(t, "sonnet", build.Model)
	assert.Equal(t, "medium", build.Effort)
	assert.Equal(t, 50.0, build.MaxBudgetUSD)
}

func TestLoadConfig_InvalidEffort(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "agent_effort",
			yaml: "tracker:\n  kind: linear\n  api_key: k\nagent:\n  model: sonnet\n  effort: turbo\n",
		},
		{
			name: "step_effort",
			yaml: "tracker:\n  kind: linear\n  api_key: k\nagent:\n  model: sonnet\nplan:\n  effort: hihg\n",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0644))
			_, err := LoadConfig(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "effort")
		})
	}
}

func TestLoadConfig_ValidEffort(t *testing.T) {
	for _, level := range []string{"low", "medium", "high", "max", "auto"} {
		level := level
		t.Run(level, func(t *testing.T) {
			t.Parallel()
			yaml := "tracker:\n  kind: linear\n  api_key: k\nagent:\n  model: sonnet\n  effort: " + level + "\n" + minimalSteps()
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
			cfg, err := LoadConfig(path)
			require.NoError(t, err)
			assert.Equal(t, level, cfg.Agent.Effort)
		})
	}
}

func TestStepByName(t *testing.T) {
	cfg, err := LoadConfig("testdata/valid_complete.yaml")
	require.NoError(t, err)

	for _, name := range StepNames {
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

plan:
  prompt: "Plan"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
build:
  prompt: "Build"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"

# create_pr cannot use rounds; validation must reject this before getting
# tangled in any prompt/comment_template checks.
create_pr:
  rounds:
    - command: "echo hello"

validate:
  prompt: "Validate"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
ship:
  prompt: "Ship"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
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

// TestLoadConfig_RoundsOmitsCommentTemplate documents the relaxed
// validation rule: a step with rounds set may omit comment_template
// because runStepRounds only renders round_comment_template. Without
// this test the relaxation could silently regress (re-tightened
// validation would still pass every other test).
func TestLoadConfig_RoundsOmitsCommentTemplate(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
plan:
  prompt: "Plan {{.Identifier}}"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
build:
  prompt: "Build {{.Identifier}}"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
create_pr:
  prompt: "PR"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
validate:
  round_comment_template: "## {{.Heading}} Round {{.Round}}/{{.TotalRounds}}\n\n{{.Output}}"
  rounds:
    - prompt: "Run tests"
ship:
  prompt: "Ship"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.Validate.CommentTemplate)
	assert.NotEmpty(t, cfg.Validate.RoundCommentTemplate)
}

// TestLoadConfig_RoundCommentTemplateTypoOnSingleShotStep locks in
// validation of round_comment_template on steps with no rounds. Bootstrap
// seeds round_comment_template on every rounds-capable step, so a typo
// there should fail at LoadConfig time even when rounds aren't enabled
// — otherwise the only test coverage was the rounds-set path.
func TestLoadConfig_RoundCommentTemplateTypoOnSingleShotStep(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
plan:
  prompt: "Plan {{.Identifier}}"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
  round_comment_template: "## {{.Headng}} Round {{.Round}}/{{.TotalRounds}}\n\n{{.Output}}"
build:
  prompt: "Build"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
create_pr:
  prompt: "PR"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
validate:
  prompt: "V"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
ship:
  prompt: "Ship"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plan.round_comment_template")
	assert.Contains(t, err.Error(), "Headng")
}

// TestLoadConfig_ConditionalBranchTypoCaughtAtLoad locks in the second
// (filled-data) Execute pass: a typo guarded by `{{- if .X}}` branches
// is invisible to the zero-value pass because the branch is false.
// Without the filled-data pass, validation misses these.
func TestLoadConfig_ConditionalBranchTypoCaughtAtLoad(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
plan:
  prompt: |-
    Issue: {{.Identifier}}
    {{- if .Description}}
    Description: {{.Decsription}}
    {{- end}}
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
build:
  prompt: "Build"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
create_pr:
  prompt: "PR"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
validate:
  prompt: "V"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
ship:
  prompt: "Ship"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plan.prompt")
	assert.Contains(t, err.Error(), "Decsription")
}

// TestLoadConfig_TemplateFieldTypoCaughtAtLoad ensures the eager
// Execute() in validatePromptTemplate / validateCommentTemplate catches
// references to non-existent struct fields. Without execution-time
// validation, {{.Headng}} (typo of Heading) only fails when a comment
// is posted at runtime.
func TestLoadConfig_TemplateFieldTypoCaughtAtLoad(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
plan:
  prompt: "Plan {{.Identifier}}"
  comment_template: "## {{.Headng}} Complete\n\n{{.Output}}"
build:
  prompt: "Build"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
create_pr:
  prompt: "PR"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
validate:
  prompt: "V"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
ship:
  prompt: "Ship"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plan.comment_template")
	assert.Contains(t, err.Error(), "Headng")
}

func TestResolveRound_InheritsFromStep(t *testing.T) {
	parent := StepConfig{
		Model:                 "sonnet",
		Effort:                "high",
		SystemPrompt:          "parent system prompt",
		PermissionMode:        "bypass",
		MaxTurns:              10,
		MaxBudgetUSD:          25.0,
		TransientRetries:      4,
		MaxToolErrorRetries:   3,
		StreamTurnGracePeriod: 12 * time.Minute,
	}

	// Round with no overrides — inherits everything.
	round := RoundConfig{Prompt: "do stuff"}
	resolved := ResolveRound(round, parent)
	assert.Equal(t, "do stuff", resolved.Prompt)
	assert.Equal(t, "parent system prompt", resolved.SystemPrompt)
	assert.Equal(t, "sonnet", resolved.Model)
	assert.Equal(t, "high", resolved.Effort)
	assert.Equal(t, "bypass", resolved.PermissionMode)
	assert.Equal(t, 10, resolved.MaxTurns)
	assert.Equal(t, 25.0, resolved.MaxBudgetUSD)
	assert.Equal(t, 4, resolved.TransientRetries)
	assert.Equal(t, 3, resolved.MaxToolErrorRetries)
	// StreamTurnGracePeriod has no per-round override field, so a round always
	// inherits the parent's value verbatim (the wiring cursor flagged as
	// untested in PR #259).
	assert.Equal(t, 12*time.Minute, resolved.StreamTurnGracePeriod)
}

// loadConfigFromYAML writes a YAML body to a temp file and loads it. Used by
// the LLM-endpoint negative tests below; keeping the bytes inline avoids a
// proliferation of one-off testdata fixtures for each malformed shape.
func loadConfigFromYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0644))
	return LoadConfig(path)
}

const validBaseSteps = `
plan:
  prompt: "Plan"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
build:
  prompt: "Build"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
create_pr:
  prompt: "Open PR"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
validate:
  prompt: "Test"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
ship:
  prompt: "Ship"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
`

func TestLoadConfig_RejectsMalformedAgentLLMEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "missing base url",
			body: `
tracker: {kind: linear, api_key: x}
agent:
  model: sonnet
  llm_endpoint:
    api_key_env: BASETEN_API_KEY
` + validBaseSteps,
			wantErr: "agent.llm_endpoint",
		},
		{
			name: "bad scheme",
			body: `
tracker: {kind: linear, api_key: x}
agent:
  model: sonnet
  llm_endpoint:
    base_url: ftp://example.com
    api_key_env: BASETEN_API_KEY
` + validBaseSteps,
			wantErr: "must be http(s)",
		},
		{
			name: "hostless url",
			body: `
tracker: {kind: linear, api_key: x}
agent:
  model: sonnet
  llm_endpoint:
    base_url: "https:///v1"
    api_key_env: BASETEN_API_KEY
` + validBaseSteps,
			wantErr: "missing a host",
		},
		{
			name: "bad provider name",
			body: `
tracker: {kind: linear, api_key: x}
agent:
  model: sonnet
  llm_endpoint:
    base_url: https://example.com
    api_key_env: BASETEN_API_KEY
    provider_name: "has space"
` + validBaseSteps,
			wantErr: "invalid ProviderName",
		},
		{
			name: "bad header name",
			body: `
tracker: {kind: linear, api_key: x}
agent:
  model: sonnet
  llm_endpoint:
    base_url: https://example.com
    api_key_env: BASETEN_API_KEY
    headers:
      "X Custom Header": value
` + validBaseSteps,
			wantErr: "invalid header name",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadConfigFromYAML(t, tc.body)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestLoadConfig_RejectsMalformedStepLLMEndpoint(t *testing.T) {
	t.Parallel()
	body := `
tracker: {kind: linear, api_key: x}
agent:
  model: sonnet
plan:
  prompt: "Plan"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
  llm_endpoint:
    base_url: ftp://example.com
    api_key_env: X
build:
  prompt: "Build"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
create_pr:
  prompt: "PR"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
validate:
  prompt: "T"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
ship:
  prompt: "S"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
`
	_, err := loadConfigFromYAML(t, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plan.llm_endpoint")
	assert.Contains(t, err.Error(), "must be http(s)")
}

func TestResolveRound_InheritsLLMEndpoint(t *testing.T) {
	parent := StepConfig{
		Model: "sonnet",
		LLMEndpoint: &LLMEndpointConfig{
			BaseURL:   "https://inference.baseten.co/v1",
			APIKeyEnv: "BASETEN_API_KEY",
		},
	}
	round := RoundConfig{Prompt: "do stuff"}
	resolved := ResolveRound(round, parent)
	require.NotNil(t, resolved.LLMEndpoint, "expected LLMEndpoint to be inherited from parent step")
	assert.Equal(t, "https://inference.baseten.co/v1", resolved.LLMEndpoint.BaseURL)
	assert.Equal(t, "BASETEN_API_KEY", resolved.LLMEndpoint.APIKeyEnv)
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

plan:
  prompt: "Plan"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"

build:
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
  round_comment_template: "## {{.Heading}} Round {{.Round}}/{{.TotalRounds}}\n\n{{.Output}}"
  rounds:
    - max_turns: 5

create_pr:
  prompt: "PR"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
validate:
  prompt: "Validate"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
ship:
  prompt: "Ship"
  comment_template: "## {{.Heading}}\n\n{{.Output}}"
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

// TestValidateStepRequiresPrompt verifies the bootstrap-or-die contract for
// step configs without prompts (and without rounds). The error must point at
// `jiradozer bootstrap`.
func TestValidateStepRequiresPrompt(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
plan:
  permission_mode: plan
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plan")
	assert.Contains(t, err.Error(), "prompt is required")
	assert.Contains(t, err.Error(), "jiradozer bootstrap")
}

// TestValidateStepRequiresCommentTemplate verifies that comment_template is
// required for every step.
func TestValidateStepRequiresCommentTemplate(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
plan:
  permission_mode: plan
  prompt: "Plan {{.Identifier}}"
build:
  permission_mode: bypass
  prompt: "Build {{.Identifier}}"
create_pr:
  permission_mode: bypass
  prompt: "PR"
validate:
  permission_mode: bypass
  prompt: "Validate"
ship:
  permission_mode: bypass
  prompt: "Ship"
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "comment_template is required")
	assert.Contains(t, err.Error(), "jiradozer bootstrap")
}

// TestValidateStepRejectsNegativeIdleTimeout verifies that a typo'd
// negative idle_timeout fails config load loudly instead of silently
// disabling the watchdog at runtime (where 0 and <0 are otherwise
// indistinguishable to runWatchdog).
func TestValidateStepRejectsNegativeIdleTimeout(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
plan:
  permission_mode: plan
  prompt: "Plan {{.Identifier}}"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
  idle_timeout: -5m
build:
  permission_mode: bypass
  prompt: "Build {{.Identifier}}"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
create_pr:
  permission_mode: bypass
  prompt: "PR"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
validate:
  permission_mode: bypass
  prompt: "Validate"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
ship:
  permission_mode: bypass
  prompt: "Ship"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	_, err := LoadConfig(path)
	require.Error(t, err, "negative idle_timeout must be rejected")
	assert.Contains(t, err.Error(), "idle_timeout")
	assert.Contains(t, err.Error(), "must not be negative")
}

// TestValidateStepRejectsNegativeStreamTurnGracePeriod mirrors the
// idle_timeout guard: a typo'd negative stream_turn_grace_period must fail
// config load loudly rather than be silently coerced to "use the provider
// default" at the option boundary (buildExecuteOpts only honors >0).
func TestValidateStepRejectsNegativeStreamTurnGracePeriod(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
plan:
  permission_mode: plan
  prompt: "Plan {{.Identifier}}"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
  stream_turn_grace_period: -5m
build:
  permission_mode: bypass
  prompt: "Build {{.Identifier}}"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
create_pr:
  permission_mode: bypass
  prompt: "PR"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
validate:
  permission_mode: bypass
  prompt: "Validate"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
ship:
  permission_mode: bypass
  prompt: "Ship"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	_, err := LoadConfig(path)
	require.Error(t, err, "negative stream_turn_grace_period must be rejected")
	assert.Contains(t, err.Error(), "stream_turn_grace_period")
	assert.Contains(t, err.Error(), "must not be negative")
}

// TestValidateStepRequiresRoundCommentTemplateWhenRounds verifies that a
// step with rounds set must also supply round_comment_template; otherwise
// loading fails.
func TestValidateStepRequiresRoundCommentTemplateWhenRounds(t *testing.T) {
	dir := t.TempDir()
	yaml := `
tracker:
  kind: linear
  api_key: test-key
agent:
  model: sonnet
plan:
  permission_mode: plan
  prompt: "Plan {{.Identifier}}"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
build:
  permission_mode: bypass
  prompt: "Build {{.Identifier}}"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
create_pr:
  permission_mode: bypass
  prompt: "PR"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
validate:
  permission_mode: bypass
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
  rounds:
    - prompt: "Run tests"
ship:
  permission_mode: bypass
  prompt: "Ship"
  comment_template: "## {{.Heading}} Complete\n\n{{.Output}}"
`
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate")
	assert.Contains(t, err.Error(), "round_comment_template is required")
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
