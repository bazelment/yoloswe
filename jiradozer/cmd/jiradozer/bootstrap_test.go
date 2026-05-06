package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/wt"
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

// TestRoundsExampleUncommentsCleanly verifies the commented `rounds:` example
// in the bootstrap output is valid YAML when the user uncomments it. We feed
// an uncommented version into LoadConfig under a rounds-capable step (build)
// and require it to parse and validate. Without this, a typo or off-by-one
// indent in roundsExampleBlock would only surface for users who actually
// try to enable rounds.
func TestRoundsExampleUncommentsCleanly(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")

	uncommented := uncommentRoundsExample(roundsExampleBlock)
	require.Contains(t, uncommented, "rounds:",
		"uncomment helper must produce a `rounds:` key (sanity check)")

	// Minimal config: every step needs a prompt + comment_template, but the
	// build step uses rounds in place of a prompt. round_comment_template
	// is required when rounds is set. Indented 4 spaces to match the style
	// the bootstrap uses, so the uncommented rounds block (also 4-space
	// indented) sits at the right depth under `build:`.
	cfg := `tracker:
    kind: linear
    api_key: $LINEAR_API_KEY
agent:
    model: sonnet
plan:
    prompt: "Plan {{.Identifier}}"
    comment_template: "## {{.Heading}}\n{{.Output}}"
create_pr:
    prompt: "PR"
    comment_template: "## {{.Heading}}\n{{.Output}}"
validate:
    prompt: "Validate"
    comment_template: "## {{.Heading}}\n{{.Output}}"
ship:
    prompt: "Ship"
    comment_template: "## {{.Heading}}\n{{.Output}}"
build:
    permission_mode: bypass
    comment_template: "## {{.Heading}}\n{{.Output}}"
    round_comment_template: "## {{.Heading}} {{.Round}}/{{.TotalRounds}}\n{{.Output}}"
` + uncommented

	dir := t.TempDir()
	path := filepath.Join(dir, "jiradozer.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o644))

	loaded, err := jiradozer.LoadConfig(path)
	require.NoError(t, err, "uncommented rounds example must parse and validate; got config:\n%s", cfg)
	require.Len(t, loaded.Build.Rounds, 2, "rounds example must produce exactly 2 rounds")
	assert.NotEmpty(t, loaded.Build.Rounds[0].Prompt, "first example round should be an agent prompt round")
	assert.NotEmpty(t, loaded.Build.Rounds[1].Command, "second example round should be a shell command round")
}

// uncommentRoundsExample turns the commented `roundsExampleBlock` into the
// YAML a user gets after stripping leading `#` markers. Relies on the
// layout invariant in roundsExampleBlock: prose lives above `#rounds:`
// and never below it. Lines before `#rounds:` are dropped; from `#rounds:`
// onward, every line begins with `    #` followed by YAML content, so we
// strip the `#` to get back active YAML.
func uncommentRoundsExample(block string) string {
	const (
		indent      = "    "
		startMarker = "    #rounds:"
	)
	var out []string
	started := false
	for _, line := range strings.Split(block, "\n") {
		if !started {
			if line == startMarker {
				started = true
				out = append(out, indent+"rounds:")
			}
			continue
		}
		if strings.HasPrefix(line, indent+"#") {
			out = append(out, indent+strings.TrimPrefix(line, indent+"#"))
			continue
		}
		// First line that doesn't begin with `    #` ends the rounds block.
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestBootstrapRefusesExistingFile verifies that --force is required when
// the output path already exists.
func TestBootstrapRefusesExistingFile(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")

	dir := t.TempDir()
	path := filepath.Join(dir, "jiradozer.yaml")
	require.NoError(t, os.WriteFile(path, []byte("# pre-existing\n"), 0o644))

	args := &bootstrapArgs{}
	configPath := "jiradozer.yaml"
	cmd := newBootstrapCommand(args, &configPath)
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
	configPath := "jiradozer.yaml"
	cmd := newBootstrapCommand(args, &configPath)
	cmd.SetArgs([]string{"--output", path, "--force"})
	require.NoError(t, cmd.Execute())

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotEqual(t, "# pre-existing\n", string(got))
	assert.Contains(t, string(got), "jiradozer bootstrap")
}

// TestBootstrapDefaultPath verifies standalone bootstrap invocation writes to
// the configured default path when no output override is supplied.
func TestBootstrapDefaultPath(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")

	dir := t.TempDir()
	t.Chdir(dir)

	args := &bootstrapArgs{}
	configPath := "jiradozer.yaml"
	cmd := newBootstrapCommand(args, &configPath)
	require.NoError(t, cmd.Execute())

	got, err := os.ReadFile(filepath.Join(dir, "jiradozer.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "jiradozer bootstrap")
}

func TestBootstrapWithRepoCreatesWorktreeAndKeepsConfigOutsideRepo(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")
	wtRoot := t.TempDir()
	t.Setenv("WT_ROOT", wtRoot)
	outputDir := t.TempDir()
	t.Chdir(outputDir)
	remoteDir := newBootstrapRemote(t)

	args := &bootstrapArgs{}
	configPath := "jiradozer.yaml"
	cmd := newBootstrapCommand(args, &configPath)
	cmd.SetArgs([]string{"--repo", remoteDir})
	require.NoError(t, cmd.Execute())

	repoName := wt.GetRepoNameFromURL(remoteDir)
	mainPath := filepath.Join(wtRoot, repoName, "main")
	configPathInRepoContainer := filepath.Join(wtRoot, repoName, "jiradozer.yaml")
	require.FileExists(t, filepath.Join(wtRoot, repoName, ".bare", "HEAD"))
	require.DirExists(t, mainPath)
	require.FileExists(t, configPathInRepoContainer)
	require.NoFileExists(t, filepath.Join(outputDir, "jiradozer.yaml"))
	require.NoFileExists(t, filepath.Join(mainPath, "jiradozer.yaml"))

	cfg, err := jiradozer.LoadConfig(configPathInRepoContainer)
	require.NoError(t, err)
	assert.Equal(t, mainPath, cfg.WorkDir)

	git := &wt.DefaultGitRunner{}
	result, err := git.Run(context.Background(), []string{"status", "--porcelain"}, mainPath)
	require.NoError(t, err)
	assert.Empty(t, result.Stdout, "bootstrap --repo should leave the cloned worktree clean")
}

func TestBootstrapWithRepoReusesExistingWorktree(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")
	wtRoot := t.TempDir()
	t.Setenv("WT_ROOT", wtRoot)
	outputDir := t.TempDir()
	t.Chdir(outputDir)
	remoteDir := newBootstrapRemote(t)

	args := &bootstrapArgs{}
	configPath := "jiradozer.yaml"
	cmd := newBootstrapCommand(args, &configPath)
	cmd.SetArgs([]string{"--repo", remoteDir})
	require.NoError(t, cmd.Execute())

	cmd = newBootstrapCommand(&bootstrapArgs{}, &configPath)
	cmd.SetArgs([]string{"--repo", remoteDir})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	cmd = newBootstrapCommand(&bootstrapArgs{}, &configPath)
	cmd.SetArgs([]string{"--repo", remoteDir, "--force"})
	require.NoError(t, cmd.Execute())
}

func TestBootstrapWithRepoExpandsOwnerRepoShorthand(t *testing.T) {
	assert.Equal(t, "https://github.com/owner/repo.git", normalizeRepoURL("owner/repo"))
	assert.Equal(t, "https://example.com/owner/repo.git", normalizeRepoURL("https://example.com/owner/repo.git"))
	assert.Equal(t, "git@github.com:owner/repo.git", normalizeRepoURL("git@github.com:owner/repo.git"))
}

func newBootstrapRemote(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	git := &wt.DefaultGitRunner{}

	remoteDir := filepath.Join(t.TempDir(), "repo.git")
	require.NoError(t, os.MkdirAll(remoteDir, 0o755))
	_, err := git.Run(ctx, []string{"init", "--bare"}, remoteDir)
	require.NoError(t, err)

	setupDir := t.TempDir()
	_, err = git.Run(ctx, []string{"clone", remoteDir, setupDir}, "")
	require.NoError(t, err)
	_, err = git.Run(ctx, []string{"config", "user.email", "test@example.com"}, setupDir)
	require.NoError(t, err)
	_, err = git.Run(ctx, []string{"config", "user.name", "Test User"}, setupDir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(setupDir, "README.md"), []byte("# Test Repo\n"), 0o644))
	_, err = git.Run(ctx, []string{"add", "."}, setupDir)
	require.NoError(t, err)
	_, err = git.Run(ctx, []string{"commit", "-m", "initial commit"}, setupDir)
	require.NoError(t, err)
	_, err = git.Run(ctx, []string{"branch", "-M", "main"}, setupDir)
	require.NoError(t, err)
	_, err = git.Run(ctx, []string{"push", "-u", "origin", "main"}, setupDir)
	require.NoError(t, err)
	_, err = git.Run(ctx, []string{"symbolic-ref", "HEAD", "refs/heads/main"}, remoteDir)
	require.NoError(t, err)
	return remoteDir
}
