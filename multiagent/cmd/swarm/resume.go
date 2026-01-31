package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/multiagent/checkpoint"
)

var resumeCmd = &cobra.Command{
	Use:   "resume <session-id>",
	Short: "Resume from a previous session",
	Long: `Resume a previously interrupted or failed session from its checkpoint.

The session ID can be found in the session directory or from the output
of a previous swarm run.

Example:
  swarm resume swarm-1234567890
  swarm resume swarm-1234567890 --budget 10.0`,
	Args: cobra.ExactArgs(1),
	RunE: runResumeCmd,
}

func init() {
	resumeCmd.Flags().Float64Var(&budget, "budget", 1.0, "Total budget in USD (additional to previous cost)")
	resumeCmd.Flags().IntVar(&maxIterations, "max-iterations", 50, "Maximum builder-reviewer iterations")
	resumeCmd.Flags().DurationVar(&timeout, "timeout", 0, "Maximum execution time (e.g., 5m, 1h). 0 means no timeout")

	rootCmd.AddCommand(resumeCmd)
}

func runResumeCmd(cmd *cobra.Command, args []string) error {
	sessionID := args[0]
	sessDir := resolveSessionDir()

	// Load checkpoint
	cp, err := checkpoint.Load(sessDir, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load checkpoint: %w", err)
	}
	if cp == nil {
		return fmt.Errorf("no checkpoint found for session %s", sessionID)
	}
	if !cp.CanResume() {
		return fmt.Errorf("session %s cannot be resumed (phase: %s)", sessionID, cp.Phase)
	}

	fmt.Printf("Resuming session %s from phase: %s\n", sessionID, cp.ResumePhase())

	ctx, cancel := setupContext()
	defer cancel()

	consoleReporter, progressReporter := createProgressReporter()
	config := createSwarmConfig(progressReporter)
	config.SessionID = sessionID // Use existing session ID

	orch, err := startOrchestrator(ctx, config)
	if err != nil {
		return err
	}
	defer stopOrchestrator(orch, consoleReporter, config)

	fmt.Printf("\nResuming mission: %s\n", truncate(cp.Mission, 100))
	fmt.Printf("Phase: %s -> %s\n", cp.Phase, cp.ResumePhase())
	fmt.Printf("Previous cost: $%.4f\n", cp.TotalCost)
	fmt.Println("---")

	result, err := orch.ResumeMission(ctx, cp)
	if err != nil {
		if ctx.Err() == context.Canceled {
			fmt.Println("Mission cancelled by user")
			return nil
		}
		return fmt.Errorf("mission resume failed: %w", err)
	}

	printMissionResult(result)
	printSummary(orch)

	return nil
}
