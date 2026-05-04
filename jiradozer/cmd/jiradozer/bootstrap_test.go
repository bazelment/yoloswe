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
// reloads it via jiradozer.LoadConfig. The reload must succeed with
// non-empty Prompt and CommentTemplate for every named step — otherwise the
// generated YAML is broken and `jiradozer bootstrap` would hand users a
// config that fails on first run.
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
	for name, step := range steps {
		assert.NotEmptyf(t, step.Prompt, "step %s: bootstrap must seed a prompt", name)
		assert.NotEmptyf(t, step.CommentTemplate, "step %s: bootstrap must seed a comment_template", name)
	}
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
