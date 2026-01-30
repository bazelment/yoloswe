// Command yoloswe runs a builder-reviewer loop for software engineering tasks.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/yoloswe"
)

var (
	builderModel    string
	reviewerModel   string
	dir             string
	budget          float64
	timeout         int
	maxIterations   int
	record          string
	verbose         bool
	systemPrompt    string
	requireApproval bool
	resumeSession   string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "yoloswe [flags] <prompt>",
		Short: "Run a builder-reviewer loop for software engineering tasks",
		Long: `yoloswe runs a builder-reviewer loop for software engineering tasks.
The builder (Claude) implements the task, and the reviewer (Codex) reviews.
The loop continues until the reviewer accepts or limits are reached.`,
		Example: `  yoloswe "Add unit tests for the user service"
  yoloswe --budget 10 --timeout 1800 "Refactor the database layer"
  yoloswe --builder-model opus "Fix the authentication bug"
  yoloswe "Implement feature X" --timeout 7200  # flags work anywhere`,
		Args: cobra.MinimumNArgs(1),
		Run:  run,
	}

	// Define flags
	rootCmd.Flags().StringVar(&builderModel, "builder-model", "sonnet", "Builder model: haiku, sonnet, opus")
	rootCmd.Flags().StringVar(&reviewerModel, "reviewer-model", "gpt-5.2-codex", "Reviewer model: gpt-5.2-codex, o4-mini")
	rootCmd.Flags().StringVar(&dir, "dir", "", "Working directory (default: current)")
	rootCmd.Flags().Float64Var(&budget, "budget", 100.0, "Max USD for builder session")
	rootCmd.Flags().IntVar(&timeout, "timeout", 3600, "Max seconds")
	rootCmd.Flags().IntVar(&maxIterations, "max-iterations", 100, "Max builder-reviewer iterations")
	rootCmd.Flags().StringVar(&record, "record", "", "Session recordings directory (default: ~/.yoloswe)")
	rootCmd.Flags().BoolVar(&verbose, "verbose", false, "Show detailed output")
	rootCmd.Flags().StringVar(&systemPrompt, "system", "", "Custom system prompt for builder")
	rootCmd.Flags().BoolVar(&requireApproval, "require-approval", false, "Require user approval for tool executions (default: auto-approve)")
	rootCmd.Flags().StringVar(&resumeSession, "resume", "", "Resume from a previous session ID")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) {
	prompt := strings.Join(args, " ")

	// Get working directory
	workDir := dir
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting working directory: %v\n", err)
			os.Exit(1)
		}
	}

	// Set default recording directory if not specified
	recordingDir := record
	if recordingDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting home directory: %v\n", err)
			os.Exit(1)
		}
		recordingDir = filepath.Join(homeDir, ".yoloswe")
	}

	// Create config - use prompt as the goal for reviewer context
	config := yoloswe.Config{
		BuilderModel:    builderModel,
		BuilderWorkDir:  workDir,
		RecordingDir:    recordingDir,
		SystemPrompt:    systemPrompt,
		RequireApproval: requireApproval,
		ResumeSessionID: resumeSession,
		ReviewerModel:   reviewerModel,
		Goal:            prompt, // Use prompt as goal
		MaxBudgetUSD:    budget,
		MaxTimeSeconds:  timeout,
		MaxIterations:   maxIterations,
		Verbose:         verbose,
	}

	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Fprintln(os.Stderr, "\nInterrupted, shutting down...")
		cancel()
	}()

	// Print configuration
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("YOLOSWE - Builder-Reviewer Loop")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("Builder model:  %s\n", config.BuilderModel)
	fmt.Printf("Reviewer model: %s\n", config.ReviewerModel)
	fmt.Printf("Working dir:    %s\n", config.BuilderWorkDir)
	fmt.Printf("Budget:         $%.2f\n", config.MaxBudgetUSD)
	fmt.Printf("Timeout:        %ds\n", config.MaxTimeSeconds)
	fmt.Printf("Max iterations: %d\n", config.MaxIterations)
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("Prompt: %s\n", prompt)
	fmt.Println(strings.Repeat("=", 60))

	// Create and run SWE wrapper
	swe := yoloswe.New(config)

	if err := swe.Run(ctx, prompt); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		swe.PrintSummary()
		os.Exit(1)
	}

	swe.PrintSummary()

	// Exit with appropriate code based on result
	stats := swe.Stats()
	if stats.ExitReason == yoloswe.ExitReasonAccepted {
		os.Exit(0)
	}
	os.Exit(1)
}
