// Command swarm-queen is a deterministic harness for subagent swarms.
//
// It owns the loop, the state, and the verification that an orchestrator session
// otherwise has to hold in its own context across compactions.
package main

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/cliapp"
)

var rootOpts = cliapp.Options{ToolName: "swarm-queen"}

var rootCmd = &cobra.Command{
	Use:   "swarm-queen",
	Short: "Deterministic harness for subagent swarms",
	Long: `swarm-queen owns a swarm's loop, state, and verification.

Every claim a lane makes -- a .done file, an idle notification, a review verdict
-- is verified against git, gh, tmux and bramble before it is acted on.`,
}

func init() {
	cliapp.RegisterStandardFlags(rootCmd, &rootOpts)
}

func main() {
	os.Exit(cliapp.Run(&rootOpts, func(ctx context.Context, app *cliapp.App) error {
		return rootCmd.ExecuteContext(cliapp.WithApp(ctx, app))
	}))
}
