package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer"
)

// TestBootstrapPromptParity catches prompt drift: a bootstrap → YAML →
// LoadConfig round-trip must yield each canonical prompt byte-for-byte.
func TestBootstrapPromptParity(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-bootstrap-api-key")

	dir := t.TempDir()
	path := filepath.Join(dir, "jiradozer.yaml")
	content, err := bootstrapYAML()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, content, 0o644))

	cfg, err := jiradozer.LoadConfig(path)
	require.NoError(t, err)

	steps := map[string]string{
		"plan":      cfg.Plan.Prompt,
		"build":     cfg.Build.Prompt,
		"validate":  cfg.Validate.Prompt,
		"create_pr": cfg.CreatePR.Prompt,
		"ship":      cfg.Ship.Prompt,
	}
	originals := map[string]string{
		"plan":      jiradozer.BootstrapPlanPrompt,
		"build":     jiradozer.BootstrapBuildPrompt,
		"validate":  jiradozer.BootstrapValidatePrompt,
		"create_pr": jiradozer.BootstrapCreatePRPrompt,
		"ship":      jiradozer.BootstrapShipPrompt,
	}
	for name, want := range originals {
		assert.Equalf(t, want, steps[name], "step %s: bootstrap → YAML → load round-trip must preserve prompt byte-for-byte", name)
	}
}

// TestBootstrapRoundTrip writes a starter config via bootstrapYAML, then
// reloads it via jiradozer.LoadConfig. The reload must succeed and the
// CommentTemplate fields must equal the canonical bootstrap constants
// byte-for-byte; otherwise the generated YAML drifted from
// BootstrapCompleteCommentTemplate / BootstrapRoundCommentTemplate and
// users would see different comment shapes than the docs describe.
func TestBootstrapRoundTrip(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-bootstrap-api-key")

	dir := t.TempDir()
	path := filepath.Join(dir, "jiradozer.yaml")

	content, err := bootstrapYAML()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, content, 0o644))

	cfg, err := jiradozer.LoadConfig(path)
	require.NoError(t, err)

	steps := map[string]jiradozer.StepConfig{
		"plan":      cfg.Plan,
		"build":     cfg.Build,
		"validate":  cfg.Validate,
		"create_pr": cfg.CreatePR,
		"ship":      cfg.Ship,
	}
	// Steps with rounds support seed RoundCommentTemplate; create_pr cannot
	// have rounds (validate() rejects it), and plan is single-shot in the
	// bootstrap shape, so neither gets a round template.
	stepsWithRoundTemplate := map[string]bool{"build": true, "validate": true, "ship": true}
	for name, step := range steps {
		assert.NotEmptyf(t, step.Prompt, "step %s: bootstrap must seed a prompt", name)
		assert.Equalf(t, jiradozer.BootstrapCompleteCommentTemplate, step.CommentTemplate,
			"step %s: bootstrap → YAML → load round-trip must preserve comment_template byte-for-byte", name)
		if stepsWithRoundTemplate[name] {
			assert.Equalf(t, jiradozer.BootstrapRoundCommentTemplate, step.RoundCommentTemplate,
				"step %s: bootstrap → YAML → load round-trip must preserve round_comment_template byte-for-byte", name)
		}
	}
}

// TestExampleYAMLMatchesBootstrap asserts jiradozer/jiradozer.example.yaml
// is byte-identical to `jiradozer bootstrap` output. The example file is
// committed documentation; without this check it can silently drift from
// the canonical prompts.go / bootstrap constants and mislead anyone who
// reads it instead of running bootstrap.
//
// Regenerate with:
//
//	bazel run //jiradozer/cmd/jiradozer -- bootstrap --output jiradozer/jiradozer.example.yaml --force
func TestExampleYAMLMatchesBootstrap(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")

	want, err := bootstrapYAML()
	require.NoError(t, err)

	// rundir = "." in BUILD.bazel keeps the test cwd at the workspace
	// root, so the data dep resolves at jiradozer/jiradozer.example.yaml.
	got, err := os.ReadFile(filepath.Join("jiradozer", "jiradozer.example.yaml"))
	require.NoError(t, err, "jiradozer.example.yaml must exist (generated via `jiradozer bootstrap`)")

	assert.Equal(t, string(want), string(got),
		"jiradozer.example.yaml drifted from bootstrap output — regenerate via `jiradozer bootstrap --output jiradozer/jiradozer.example.yaml --force`")
}

// TestBootstrapRefusesExistingFile verifies that --force is required when
// the output path already exists.
func TestBootstrapRefusesExistingFile(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")

	dir := t.TempDir()
	path := filepath.Join(dir, "jiradozer.yaml")
	require.NoError(t, os.WriteFile(path, []byte("# pre-existing\n"), 0o644))

	args := &bootstrapArgs{}
	cmd := newBootstrapCommand(args)
	cmd.SetArgs([]string{"--output", path})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	// Verify the original file was not overwritten.
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "# pre-existing\n", string(got))
}

// TestBootstrapForceOverwrites verifies that --force lets bootstrap replace
// an existing file.
func TestBootstrapForceOverwrites(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")

	dir := t.TempDir()
	path := filepath.Join(dir, "jiradozer.yaml")
	require.NoError(t, os.WriteFile(path, []byte("# pre-existing\n"), 0o644))

	args := &bootstrapArgs{}
	cmd := newBootstrapCommand(args)
	cmd.SetArgs([]string{"--output", path, "--force"})
	require.NoError(t, cmd.Execute())

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotEqual(t, "# pre-existing\n", string(got))
	assert.Contains(t, string(got), "jiradozer bootstrap")
}
