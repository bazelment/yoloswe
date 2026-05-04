package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/jiradozer"
)

func newValidateConfigCommand(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate-config",
		Short: "Validate a jiradozer.yaml file",
		Long:  "Load and validate the config file (default: jiradozer.yaml). Exits 0 if valid, non-zero with the error otherwise.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := *configPath
			if _, err := jiradozer.LoadConfig(path); err != nil {
				return fmt.Errorf("config %s: %w", path, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %s\n", path)
			return nil
		},
	}
	cmd.SilenceUsage = true
	return cmd
}
