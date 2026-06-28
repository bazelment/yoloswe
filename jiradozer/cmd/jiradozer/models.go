package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

func newModelsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "List supported models and backends",
		Long: "Print the curated models grouped by backend and the provider prefixes that are " +
			"accepted for model IDs that are not curated (e.g. composer-2.5).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			writeModels(cmd.OutOrStdout())
			return nil
		},
	}
	cmd.SilenceUsage = true
	return cmd
}

func writeModels(w io.Writer) {
	fmt.Fprintln(w, "Supported models by backend:")
	for _, provider := range agent.AllProviders {
		var ids []string
		for _, m := range agent.AllModels {
			if m.Provider == provider {
				ids = append(ids, m.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s: %s\n", provider, strings.Join(ids, ", "))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, supportedModelsHelp())
}

// supportedModelsHelp returns a one-line summary of the curated models and the
// accepted provider prefixes. Shared by the `models` command and the
// "unknown model" errors so both stay in sync.
func supportedModelsHelp() string {
	return fmt.Sprintf(
		"available models: [%s]; any ID matching a known backend prefix (%s) is also accepted (e.g. composer-2.5). Run `jiradozer models` for details.",
		strings.Join(agent.AllModelIDs(), ", "),
		agent.KnownModelPrefixes(),
	)
}
