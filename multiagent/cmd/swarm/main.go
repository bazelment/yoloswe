// Command swarm runs the multi-agent software engineering swarm.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/checkpoint"
	"github.com/bazelment/yoloswe/multiagent/orchestrator"
	"github.com/bazelment/yoloswe/multiagent/progress"
)

func main() {
	// Parse flags
	mission := flag.String("mission", "", "Mission to execute")
	workDir := flag.String("work-dir", ".", "Working directory")
	sessionDir := flag.String("session-dir", "", "Session recording directory (default: <work-dir>/.claude-swarm/sessions)")
	budget := flag.Float64("budget", 1.0, "Total budget in USD")
	maxIterations := flag.Int("max-iterations", 50, "Maximum iterations")

	// Model flags
	orchestratorModel := flag.String("orchestrator-model", "sonnet", "Model for Orchestrator")
	plannerModel := flag.String("planner-model", "sonnet", "Model for Planner")
	designerModel := flag.String("designer-model", "sonnet", "Model for Designer")
	builderModel := flag.String("builder-model", "sonnet", "Model for Builder")
	reviewerModel := flag.String("reviewer-model", "haiku", "Model for Reviewer")

	// Interactive mode
	interactive := flag.Bool("interactive", false, "Run in interactive mode")

	// Resume mode
	resumeSession := flag.String("resume", "", "Resume from a previous session ID")
	enableCheckpoint := flag.Bool("checkpoint", true, "Enable checkpointing for error recovery")

	// Progress output
	verbose := flag.Bool("verbose", false, "Enable verbose output (shows tool activity)")
	quiet := flag.Bool("quiet", false, "Enable quiet mode (minimal output)")

	// Timeout
	timeout := flag.Duration("timeout", 0, "Maximum time for mission execution (e.g., 5m, 1h). 0 means no timeout")

	flag.Parse()

	// Validate
	if *mission == "" && !*interactive && *resumeSession == "" {
		fmt.Fprintln(os.Stderr, "Error: --mission, --interactive, or --resume required")
		flag.Usage()
		os.Exit(1)
	}

	// Create work directory if it doesn't exist
	if err := os.MkdirAll(*workDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create work directory: %v\n", err)
		os.Exit(1)
	}

	// Default session-dir to be under work directory
	if *sessionDir == "" {
		*sessionDir = filepath.Join(*workDir, ".claude-swarm", "sessions")
	}

	// Check for resumable checkpoint if resuming
	var existingCheckpoint *checkpoint.Checkpoint
	if *resumeSession != "" {
		cp, err := checkpoint.Load(*sessionDir, *resumeSession)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load checkpoint: %v\n", err)
			os.Exit(1)
		}
		if cp == nil {
			fmt.Fprintf(os.Stderr, "No checkpoint found for session %s\n", *resumeSession)
			os.Exit(1)
		}
		if !cp.CanResume() {
			fmt.Fprintf(os.Stderr, "Session %s cannot be resumed (phase: %s)\n", *resumeSession, cp.Phase)
			os.Exit(1)
		}
		existingCheckpoint = cp
		fmt.Printf("Resuming session %s from phase: %s\n", *resumeSession, cp.ResumePhase())
	}

	// Create progress reporter
	outputMode := progress.OutputNormal
	if *verbose {
		outputMode = progress.OutputVerbose
	} else if *quiet {
		outputMode = progress.OutputMinimal
	}
	consoleReporter := progress.NewConsoleReporter(progress.WithMode(outputMode))
	progressReporter := progress.NewAgentReporter(consoleReporter)

	// Create swarm config
	config := agent.SwarmConfig{
		WorkDir:             *workDir,
		SessionDir:          *sessionDir,
		OrchestratorModel:   *orchestratorModel,
		PlannerModel:        *plannerModel,
		DesignerModel:       *designerModel,
		BuilderModel:        *builderModel,
		ReviewerModel:       *reviewerModel,
		TotalBudgetUSD:      *budget,
		MaxIterations:       *maxIterations,
		EnableCheckpointing: *enableCheckpoint,
		Progress:            progressReporter,
	}

	// Use existing session ID when resuming
	if *resumeSession != "" {
		config.SessionID = *resumeSession
	}

	// Create context with cancellation and optional timeout
	var ctx context.Context
	var cancel context.CancelFunc
	if *timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), *timeout)
		fmt.Printf("Timeout set to %v\n", *timeout)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Printf("\nReceived signal %v, initiating graceful shutdown...\n", sig)
		cancel()
		// Second signal forces exit
		sig = <-sigCh
		fmt.Printf("\nReceived second signal %v, forcing exit\n", sig)
		os.Exit(1)
	}()

	// Create orchestrator
	orch, err := orchestrator.New(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create orchestrator: %v\n", err)
		os.Exit(1)
	}

	// Start orchestrator
	fmt.Printf("Starting swarm (session: %s)...\n", orch.SessionID())
	if err := orch.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start orchestrator: %v\n", err)
		os.Exit(1)
	}
	defer func() {
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
	}()

	if *interactive {
		runInteractive(ctx, orch)
	} else if existingCheckpoint != nil {
		// Resume from checkpoint
		runResume(ctx, orch, existingCheckpoint)
	} else {
		runMission(ctx, orch, *mission)
	}

	// Print summary
	summary := orch.GetSummary()
	fmt.Println("\n=== Session Summary ===")
	fmt.Printf("Session ID: %s\n", summary.SessionID)
	fmt.Printf("Total Cost: $%.4f\n", summary.TotalCost)
	fmt.Printf("Orchestrator Turns: %d\n", summary.OrchestratorTurns)
	fmt.Printf("Planner Turns: %d\n", summary.PlannerTurns)

	// Summary is saved in the defer above
}

func runMission(ctx context.Context, orch *orchestrator.Orchestrator, mission string) {
	fmt.Printf("\nMission: %s\n", truncate(mission, 100))
	fmt.Println("---")

	result, err := orch.ExecuteMission(ctx, mission)
	if err != nil {
		if ctx.Err() == context.Canceled {
			fmt.Println("Mission cancelled by user")
			return
		}
		fmt.Fprintf(os.Stderr, "Mission failed: %v\n", err)
		return
	}

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

func runInteractive(ctx context.Context, orch *orchestrator.Orchestrator) {
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
			return
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
}

func runResume(ctx context.Context, orch *orchestrator.Orchestrator, cp *checkpoint.Checkpoint) {
	fmt.Printf("\nResuming mission: %s\n", truncate(cp.Mission, 100))
	fmt.Printf("Phase: %s -> %s\n", cp.Phase, cp.ResumePhase())
	fmt.Printf("Previous cost: $%.4f\n", cp.TotalCost)
	fmt.Println("---")

	result, err := orch.ResumeMission(ctx, cp)
	if err != nil {
		if ctx.Err() == context.Canceled {
			fmt.Println("Mission cancelled by user")
			return
		}
		fmt.Fprintf(os.Stderr, "Mission resume failed: %v\n", err)
		return
	}

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
