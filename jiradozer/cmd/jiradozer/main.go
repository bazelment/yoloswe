// Command jiradozer drives a development workflow from an issue tracker.
// It plans, builds, validates, and ships — with human approval at each step.
package main

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/cliapp"
)

func main() {
	opts := cliapp.Options{
		ToolName:       "jiradozer",
		SensitiveFlags: []string{"--description"},
	}

	rootCmd := newRootCommand(&opts)

	os.Exit(cliapp.Run(&opts, func(ctx context.Context, app *cliapp.App) error {
		return rootCmd.ExecuteContext(cliapp.WithApp(ctx, app))
	}))
}

// newRootCommand builds the cobra tree: a root with three subcommands
// (run, bootstrap, validate-config). The root's RunE delegates to run's
// handler so plain `jiradozer` (with no subcommand) keeps working for
// existing invocations and shell scripts; for that to be useful, the same
// run-only flags (--issue, --filter, --description, etc.) are also bound
// directly on root.
func newRootCommand(opts *cliapp.Options) *cobra.Command {
	var rargs runArgs
	var bargs bootstrapArgs

	rootCmd := &cobra.Command{
		Use:   "jiradozer",
		Short: "Issue-driven development workflow",
		Long:  "Drives a plan → build → validate → ship workflow from an issue tracker with human-in-the-loop approval at each step.",
	}
	rootCmd.SilenceUsage = true

	cliapp.RegisterStandardFlags(rootCmd, opts)
	// --config is a persistent flag so `run`, `validate-config`, and the
	// back-compat root invocation all read the same default.
	rootCmd.PersistentFlags().StringVar(&rargs.configPath, "config", "jiradozer.yaml", "Path to config file")

	runCmd := newRunCommand(&rargs)
	bootstrapCmd := newBootstrapCommand(&bargs)
	validateConfigCmd := newValidateConfigCommand(&rargs.configPath)

	rootCmd.AddCommand(runCmd, bootstrapCmd, validateConfigCmd)

	// Back-compat: bare `jiradozer --issue X --description Y` (no
	// subcommand) behaves like `jiradozer run --issue X --description Y`.
	// The same flags must be bound on the root command for cobra to parse
	// them, and they share the same runArgs pointer so values flow through.
	registerRunFlags(rootCmd, &rargs)
	rootCmd.RunE = func(cmd *cobra.Command, _ []string) error {
		rargs.dryRunSet = dryRunChanged(cmd)
		app := cliapp.FromContext(cmd.Context())
		return run(cmd.Context(), app, rargs)
	}

	return rootCmd
}
