package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var interactiveCmd = &cobra.Command{
	Use:   "interactive",
	Short: "Run in interactive mode",
	Long: `Start an interactive session where you can send multiple requests to the swarm.

In interactive mode, you can type requests one at a time and the swarm will
process them sequentially. Use Ctrl+D to exit.

Example:
  swarm interactive
  swarm interactive --verbose`,
	RunE: runInteractiveCmd,
}

func init() {
	interactiveCmd.Flags().Float64Var(&budget, "budget", 1.0, "Total budget in USD")
	interactiveCmd.Flags().IntVar(&maxIterations, "max-iterations", 50, "Maximum builder-reviewer iterations per request")
	interactiveCmd.Flags().DurationVar(&timeout, "timeout", 0, "Maximum execution time (e.g., 5m, 1h). 0 means no timeout")

	rootCmd.AddCommand(interactiveCmd)
}

func runInteractiveCmd(cmd *cobra.Command, args []string) error {
	ctx, cancel := setupContext()
	defer cancel()

	consoleReporter, progressReporter := createProgressReporter()
	config := createSwarmConfig(progressReporter)

	orch, err := startOrchestrator(ctx, config)
	if err != nil {
		return err
	}
	defer stopOrchestrator(orch, consoleReporter, config)

	fmt.Println("\nInteractive mode. Type your requests (Ctrl+D to exit):")
	fmt.Println("---")

	// Simple interactive loop
	// In a full implementation, this would use a proper readline library
	var input string
	for {
		fmt.Print("> ")
		_, err := fmt.Scanln(&input)
		if err != nil {
			break // EOF or error
		}

		if input == "" {
			continue
		}

		select {
		case <-ctx.Done():
			return nil
		default:
		}

		result, err := orch.SendMessage(ctx, input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			continue
		}

		if result.Success {
			fmt.Println("Done.")
		} else {
			fmt.Printf("Task completed with issues: %v\n", result.Error)
		}
		fmt.Printf("Cost: $%.4f\n\n", result.Usage.CostUSD)
	}

	printSummary(orch)

	return nil
}
