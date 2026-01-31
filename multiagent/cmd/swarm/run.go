package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run <mission>",
	Short: "Execute a mission",
	Long: `Execute a software engineering mission using the multi-agent swarm.

The mission is a natural language description of what you want to accomplish.
The swarm will coordinate Designer, Builder, and Reviewer agents to complete the task.

Example:
  swarm run "Add a login page with email and password fields"
  swarm run "Fix the bug in the authentication middleware" --budget 5.0`,
	Args: cobra.ExactArgs(1),
	RunE: runMissionCmd,
}

func init() {
	runCmd.Flags().Float64Var(&budget, "budget", 1.0, "Total budget in USD")
	runCmd.Flags().IntVar(&maxIterations, "max-iterations", 50, "Maximum builder-reviewer iterations")
	runCmd.Flags().DurationVar(&timeout, "timeout", 0, "Maximum execution time (e.g., 5m, 1h). 0 means no timeout")

	rootCmd.AddCommand(runCmd)
}

func runMissionCmd(cmd *cobra.Command, args []string) error {
	mission := args[0]

	ctx, cancel := setupContext()
	defer cancel()

	consoleReporter, progressReporter := createProgressReporter()
	config := createSwarmConfig(progressReporter)

	orch, err := startOrchestrator(ctx, config)
	if err != nil {
		return err
	}
	defer stopOrchestrator(orch, consoleReporter, config)

	fmt.Printf("\nMission: %s\n", truncate(mission, 100))
	fmt.Println("---")

	result, err := orch.ExecuteMission(ctx, mission)
	if err != nil {
		if ctx.Err() == context.Canceled {
			fmt.Println("Mission cancelled by user")
			return nil
		}
		return fmt.Errorf("mission failed: %w", err)
	}

	printMissionResult(result)
	printSummary(orch)

	return nil
}
