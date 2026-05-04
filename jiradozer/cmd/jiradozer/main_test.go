package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

func TestValidateDryRunMode(t *testing.T) {
	dryRunCfg := func() *jiradozer.Config {
		return &jiradozer.Config{Source: jiradozer.SourceConfig{DryRun: true}}
	}
	tests := []struct {
		cfg     *jiradozer.Config
		name    string
		wantErr string
		args    runArgs
	}{
		{
			name: "dry-run off: any args accepted",
			cfg:  &jiradozer.Config{Source: jiradozer.SourceConfig{DryRun: false}},
			args: runArgs{issueID: "ENG-1", description: "local task"},
		},
		{
			name: "dry-run + team mode: accepted",
			cfg:  dryRunCfg(),
			args: runArgs{},
		},
		{
			name:    "dry-run + single-issue: rejected",
			cfg:     dryRunCfg(),
			args:    runArgs{issueID: "ENG-1"},
			wantErr: "--dry-run only applies to team mode",
		},
		{
			name:    "dry-run + description: rejected",
			cfg:     dryRunCfg(),
			args:    runArgs{description: "local task"},
			wantErr: "--dry-run only applies to team mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDryRunMode(tt.cfg, tt.args)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestResolveRepoName(t *testing.T) {
	tests := []struct {
		name string
		cfg  *jiradozer.Config
		want string
	}{
		{
			name: "no team filter defaults to jiradozer",
			cfg: &jiradozer.Config{
				Source: jiradozer.SourceConfig{Filters: map[string]string{}},
			},
			want: "jiradozer",
		},
		{
			name: "linear team filter used verbatim",
			cfg: &jiradozer.Config{
				Tracker: jiradozer.TrackerConfig{Kind: "linear"},
				Source: jiradozer.SourceConfig{Filters: map[string]string{
					tracker.FilterTeam: "ENG",
				}},
			},
			want: "ENG",
		},
		{
			name: "github owner/repo collapsed to repo portion",
			cfg: &jiradozer.Config{
				Tracker: jiradozer.TrackerConfig{Kind: "github"},
				Source: jiradozer.SourceConfig{Filters: map[string]string{
					tracker.FilterTeam: "bazelment/yoloswe",
				}},
			},
			want: "yoloswe",
		},
		{
			name: "github malformed team falls through to raw value",
			cfg: &jiradozer.Config{
				Tracker: jiradozer.TrackerConfig{Kind: "github"},
				Source: jiradozer.SourceConfig{Filters: map[string]string{
					tracker.FilterTeam: "not-an-owner-repo",
				}},
			},
			want: "not-an-owner-repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveRepoName(tt.cfg)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoadRunConfigAppliesCLIOverrides(t *testing.T) {
	cfgPath := writeRunConfig(t, "linear", t.TempDir())
	cfg, err := loadRunConfig(runArgs{
		configPath:    cfgPath,
		sourceFilters: []string{"team=ENG"},
		modelID:       "opus",
		pollInterval:  2 * time.Second,
		maxConcurrent: 7,
		branchPrefix:  "hotfix",
		dryRunSet:     true,
		dryRun:        true,
	})
	require.NoError(t, err)

	require.Equal(t, "opus", cfg.Agent.Model)
	require.Equal(t, 2*time.Second, cfg.PollInterval)
	require.Equal(t, "ENG", cfg.Source.Filters[tracker.FilterTeam])
	require.Equal(t, 7, cfg.Source.MaxConcurrent)
	require.Equal(t, "hotfix", cfg.Source.BranchPrefix)
	require.True(t, cfg.Source.DryRun)
}

func TestValidateReloadCompatibleRejectsTrackerChanges(t *testing.T) {
	oldCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear", APIKey: "old"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "ENG"}},
	}
	newCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "github"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "ENG"}},
	}

	require.ErrorContains(t, validateReloadCompatible(oldCfg, newCfg), "tracker kind change")

	newCfg = &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear", APIKey: "new"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "ENG"}},
	}
	require.ErrorContains(t, validateReloadCompatible(oldCfg, newCfg), "tracker config changes")
}

func TestValidateReloadCompatibleRejectsSourceModeChanges(t *testing.T) {
	oldCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear", APIKey: "key"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "ENG"}},
	}
	newCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear", APIKey: "key"},
	}

	require.ErrorContains(t, validateReloadCompatible(oldCfg, newCfg), "source mode change")
}

// TestDryRunFlagPlacement verifies --dry-run is honored regardless of where
// the user puts it relative to the run subcommand. Because the flag is
// registered on both root and `run` (via registerRunFlags) but cobra only
// records `Changed=true` on whichever FlagSet actually parsed it, a naive
// `cmd.Flags().Changed("dry-run")` in run's RunE silently drops the flag
// when the user wrote `jiradozer --dry-run run …`. Both invocation paths
// run through dryRunChanged: the run-subcommand RunE consults runCmd, and
// the back-compat root RunE (`jiradozer --dry-run` with no subcommand)
// consults rootCmd directly.
func TestDryRunFlagPlacement(t *testing.T) {
	tests := []struct {
		name     string
		checkCmd string // "run" or "root" — which command's RunE actually fires
		argv     []string
		want     bool
	}{
		{name: "no dry-run", argv: []string{"run"}, checkCmd: "run", want: false},
		{name: "dry-run on run subcommand", argv: []string{"run", "--dry-run"}, checkCmd: "run", want: true},
		{name: "dry-run before run subcommand", argv: []string{"--dry-run", "run"}, checkCmd: "run", want: true},
		// Back-compat path: bare `jiradozer --dry-run --filter team=ENG`
		// (no `run`) lands on root's RunE, so dryRunChanged is invoked
		// against rootCmd. registerRunFlags is bound on root for this case.
		{name: "dry-run on root, no subcommand", argv: []string{"--dry-run"}, checkCmd: "root", want: true},
		{name: "no dry-run on root, no subcommand", argv: []string{}, checkCmd: "root", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rargs runArgs
			rootCmd := &cobra.Command{
				Use:  "jiradozer",
				RunE: func(cmd *cobra.Command, _ []string) error { return nil },
			}
			runCmd := &cobra.Command{
				Use:  "run",
				RunE: func(cmd *cobra.Command, _ []string) error { return nil },
			}
			registerRunFlags(rootCmd, &rargs)
			registerRunFlags(runCmd, &rargs)
			rootCmd.AddCommand(runCmd)
			rootCmd.SetArgs(tt.argv)
			require.NoError(t, rootCmd.Execute())

			target := runCmd
			if tt.checkCmd == "root" {
				target = rootCmd
			}
			got := dryRunChanged(target)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Redaction is tested in cliapp/redact_test.go; jiradozer just composes its
// sensitive flag list into cliapp.Options.SensitiveFlags.

func TestBuildChildArgs(t *testing.T) {
	tests := []struct {
		wantContain []string
		wantOmit    []string
		name        string
		args        runArgs
	}{
		{
			name:        "thinking-level set is propagated",
			args:        runArgs{thinkingLevel: "high"},
			wantContain: []string{"--thinking-level", "high"},
		},
		{
			name:     "thinking-level empty is omitted",
			args:     runArgs{},
			wantOmit: []string{"--thinking-level"},
		},
		{
			name:        "model + thinking-level both propagated",
			args:        runArgs{modelID: "opus", thinkingLevel: "max"},
			wantContain: []string{"--model", "opus", "--thinking-level", "max"},
		},
	}

	app := &cliapp.App{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildChildArgs(app, tt.args, "/tmp/jiradozer.yaml")
			joined := ""
			for _, a := range got {
				joined += a + " "
			}
			for _, want := range tt.wantContain {
				assert.Contains(t, joined, want)
			}
			for _, want := range tt.wantOmit {
				assert.NotContains(t, joined, want)
			}
		})
	}
}

func writeRunConfig(t *testing.T, trackerKind, workDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jiradozer.yaml")
	t.Setenv("LINEAR_API_KEY", "test-key")
	content, err := bootstrapYAML()
	require.NoError(t, err)
	content = bytes.Replace(content, []byte("kind: linear"), []byte("kind: "+trackerKind), 1)
	content = bytes.Replace(content, []byte("work_dir: ."), []byte("work_dir: "+workDir), 1)
	if trackerKind == "github" || trackerKind == "local" {
		content = bytes.Replace(content, []byte("api_key: $LINEAR_API_KEY"), []byte("api_key: \"\""), 1)
	}
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

// TestBuildChildArgsOrdering pins the argv layout: persistent (root-level)
// flags — --config and the standard --verbose/--verbosity/--color set —
// must appear before the `run` subcommand token, and run-only flags
// (--model, --thinking-level, --max-budget, --auto-approve) must appear
// after it. Cobra's PersistentFlags inheritance means either side parses
// today, but mixing breaks the rule that a flag is declared adjacent to
// the command that owns it; this test fails fast if a future edit puts a
// run-only flag before `run` or vice versa.
func TestBuildChildArgsOrdering(t *testing.T) {
	app := &cliapp.App{Verbosity: render.VerbosityVerbose, Color: render.ColorAlways}
	args := runArgs{
		modelID:       "opus",
		thinkingLevel: "max",
		maxBudget:     12.5,
		autoApprove:   "all",
	}
	got := buildChildArgs(app, args, "/tmp/jiradozer.yaml")

	indexOf := func(needle string) int {
		for i, a := range got {
			if a == needle {
				return i
			}
		}
		return -1
	}
	runIdx := indexOf("run")
	require.NotEqual(t, -1, runIdx, "argv must contain `run` subcommand token; got %v", got)

	persistentBeforeRun := []string{"--config", "--verbose", "--color"}
	for _, flag := range persistentBeforeRun {
		idx := indexOf(flag)
		if idx == -1 {
			continue
		}
		assert.Lessf(t, idx, runIdx,
			"persistent flag %s must appear before `run` (got argv: %v)", flag, got)
	}

	runOnlyAfterRun := []string{"--model", "--thinking-level", "--max-budget", "--auto-approve"}
	for _, flag := range runOnlyAfterRun {
		idx := indexOf(flag)
		require.NotEqualf(t, -1, idx, "argv must contain run-only flag %s; got %v", flag, got)
		assert.Greaterf(t, idx, runIdx,
			"run-only flag %s must appear after `run` (got argv: %v)", flag, got)
	}
}

func TestBootstrapUsesConfigPathFromRoot(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")

	tests := []struct {
		name     string
		wantFile string
		args     []string
	}{
		{
			name:     "config before bootstrap",
			wantFile: "custom.yaml",
			args:     []string{"--config", "custom.yaml", "bootstrap"},
		},
		{
			name:     "config after bootstrap",
			wantFile: "custom.yaml",
			args:     []string{"bootstrap", "--config", "custom.yaml"},
		},
		{
			name:     "output overrides config",
			wantFile: "output.yaml",
			args:     []string{"bootstrap", "--config", "config.yaml", "--output", "output.yaml"},
		},
		{
			name:     "output still works without config",
			wantFile: "output.yaml",
			args:     []string{"bootstrap", "--output", "output.yaml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			cmd := newRootCommand(&cliapp.Options{ToolName: "jiradozer"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs(tt.args)
			require.NoError(t, cmd.Execute())

			got, err := os.ReadFile(tt.wantFile)
			require.NoError(t, err)
			assert.Contains(t, string(got), "jiradozer bootstrap")

			if tt.wantFile == "output.yaml" {
				_, err := os.Stat("config.yaml")
				assert.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestValidateConfigUsesConfigPathFromRoot(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "config before validate-config",
			args: []string{"--config", "custom.yaml", "validate-config"},
		},
		{
			name: "config after validate-config",
			args: []string{"validate-config", "--config", "custom.yaml"},
		},
	}

	content, err := bootstrapYAML()
	require.NoError(t, err)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			require.NoError(t, os.WriteFile("custom.yaml", content, 0o644))

			cmd := newRootCommand(&cliapp.Options{ToolName: "jiradozer"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs(tt.args)
			require.NoError(t, cmd.Execute())
			assert.Contains(t, out.String(), "ok: custom.yaml")
		})
	}
}
