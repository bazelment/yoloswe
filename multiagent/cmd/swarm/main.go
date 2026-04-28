// Command swarm runs the multi-agent software engineering swarm.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/orchestrator"
	"github.com/bazelment/yoloswe/multiagent/progress"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

// Global flags (persistent across all commands)
var (
	workDir           string
	sessionDir        string
	enableCheckpoint  bool
	orchestratorModel string
	plannerModel      string
	designerModel     string
	builderModel      string
	reviewerModel     string
	rootOpts          = cliapp.Options{ToolName: "swarm"}
)

// Command-specific flags
var (
	budget        float64
	maxIterations int
	timeout       time.Duration
)

var rootCmd = &cobra.Command{
	Use:   "swarm",
	Short: "Multi-agent software engineering swarm",
	Long: `A multi-agent system that coordinates Orchestrator, Planner, Designer,
Builder, and Reviewer agents to accomplish software engineering tasks.`,
}

func init() {
	// Global flags (available to all commands)
	rootCmd.PersistentFlags().StringVar(&workDir, "work-dir", ".", "Working directory")
	rootCmd.PersistentFlags().StringVar(&sessionDir, "session-dir", "", "Session recording directory (default: <work-dir>/.claude-swarm/sessions)")
	rootCmd.PersistentFlags().BoolVar(&enableCheckpoint, "checkpoint", true, "Enable checkpointing for error recovery")
	cliapp.RegisterStandardFlags(rootCmd, &rootOpts)

	// Model flags
	rootCmd.PersistentFlags().StringVar(&orchestratorModel, "orchestrator-model", "sonnet", "Model for Orchestrator")
	rootCmd.PersistentFlags().StringVar(&plannerModel, "planner-model", "sonnet", "Model for Planner")
	rootCmd.PersistentFlags().StringVar(&designerModel, "designer-model", "sonnet", "Model for Designer")
	rootCmd.PersistentFlags().StringVar(&builderModel, "builder-model", "sonnet", "Model for Builder")
	rootCmd.PersistentFlags().StringVar(&reviewerModel, "reviewer-model", "haiku", "Model for Reviewer")
}

func main() {
	os.Exit(cliapp.Run(&rootOpts, func(ctx context.Context, app *cliapp.App) error {
		return rootCmd.ExecuteContext(cliapp.WithApp(ctx, app))
	}))
}

// resolveSessionDir returns the session directory, defaulting to work-dir/.claude-swarm/sessions
func resolveSessionDir() string {
	if sessionDir != "" {
		return sessionDir
	}
	return filepath.Join(workDir, ".claude-swarm", "sessions")
}

// createProgressReporter creates a progress reporter from the verbosity
// already resolved by cliapp.
func createProgressReporter(app *cliapp.App) (*progress.ConsoleReporter, *progress.AgentReporter) {
	outputMode := progress.OutputNormal
	switch {
	case app.Verbosity <= render.VerbosityQuiet:
		outputMode = progress.OutputMinimal
	case app.Verbosity >= render.VerbosityVerbose:
		outputMode = progress.OutputVerbose
	}
	consoleReporter := progress.NewConsoleReporter(progress.WithMode(outputMode))
	progressReporter := progress.NewAgentReporter(consoleReporter)
	return consoleReporter, progressReporter
}

// createSwarmConfig creates the swarm configuration from flags
func createSwarmConfig(progressReporter *progress.AgentReporter) agent.SwarmConfig {
	return agent.SwarmConfig{
		WorkDir:             workDir,
		SessionDir:          resolveSessionDir(),
		OrchestratorModel:   orchestratorModel,
		PlannerModel:        plannerModel,
		DesignerModel:       designerModel,
		BuilderModel:        builderModel,
		ReviewerModel:       reviewerModel,
		TotalBudgetUSD:      budget,
		MaxIterations:       maxIterations,
		EnableCheckpointing: enableCheckpoint,
		Progress:            progressReporter,
	}
}

// setupContext layers an optional --timeout on top of the parent ctx
// supplied by cliapp.Run (which already handles signals). Subcommands pass
// cmd.Context() as the parent.
func setupContext(parent context.Context) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		fmt.Printf("Timeout set to %v\n", timeout)
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}

// startOrchestrator creates and starts the orchestrator
func startOrchestrator(ctx context.Context, config agent.SwarmConfig) (*orchestrator.Orchestrator, error) {
	// Create work directory if it doesn't exist
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	orch, err := orchestrator.New(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create orchestrator: %w", err)
	}

	fmt.Printf("Starting swarm (session: %s)...\n", orch.SessionID())
	if err := orch.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start orchestrator: %w", err)
	}

	return orch, nil
}

// stopOrchestrator cleanly stops the orchestrator and writes summary
func stopOrchestrator(orch *orchestrator.Orchestrator, consoleReporter *progress.ConsoleReporter, config agent.SwarmConfig) {
	// Close progress reporter first to print total time
	consoleReporter.Close()

	fmt.Println("Stopping swarm...")
	if err := orch.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "Error stopping orchestrator: %v\n", err)
	}
	// Always write summary on shutdown
	if err := orch.WriteSummary(); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing summary: %v\n", err)
	} else {
		fmt.Printf("Summary written to: %s/%s/summary.json\n", config.SessionDir, orch.SessionID())
	}
}

// printSummary prints the session summary
func printSummary(orch *orchestrator.Orchestrator) {
	summary := orch.GetSummary()
	fmt.Println("\n=== Session Summary ===")
	fmt.Printf("Session ID: %s\n", summary.SessionID)
	fmt.Printf("Total Cost: $%.4f\n", summary.TotalCost)
	fmt.Printf("Orchestrator Turns: %d\n", summary.OrchestratorTurns)
	fmt.Printf("Planner Turns: %d\n", summary.PlannerTurns)
}

// printMissionResult prints the result of a mission execution
func printMissionResult(result *protocol.PlannerResult) {
	fmt.Println("\n=== Mission Complete ===")
	fmt.Printf("Success: %v\n", result.Success)
	fmt.Printf("Summary: %s\n", result.Summary)

	if len(result.FilesCreated) > 0 {
		fmt.Println("\nFiles Created:")
		for _, f := range result.FilesCreated {
			fmt.Printf("  - %s\n", f)
		}
	}

	if len(result.FilesModified) > 0 {
		fmt.Println("\nFiles Modified:")
		for _, f := range result.FilesModified {
			fmt.Printf("  - %s\n", f)
		}
	}

	if len(result.RemainingConcerns) > 0 {
		fmt.Println("\nRemaining Concerns:")
		for _, c := range result.RemainingConcerns {
			fmt.Printf("  - %s\n", c)
		}
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
