// Command yoloswe provides commands for AI-assisted software engineering.
//
// Commands:
//   - plan: Run planning mode to design implementations before execution
//   - build: Run a builder-reviewer loop for autonomous task execution
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/yoloswe"
	"github.com/bazelment/yoloswe/yoloswe/planner"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "yoloswe",
		Short: "AI-assisted software engineering tool",
		Long: `yoloswe provides AI-assisted software engineering capabilities.

Use 'plan' to design and plan implementations before execution.
Use 'build' to run a builder-reviewer loop for autonomous task execution.`,
	}

	rootCmd.AddCommand(newPlanCmd())
	rootCmd.AddCommand(newBuildCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// Plan command flags
type planFlags struct {
	model           string
	workDir         string
	recordDir       string
	systemPrompt    string
	build           string
	externalBuilder string
	buildModel      string
	verbose         bool
	simple          bool
}

func newPlanCmd() *cobra.Command {
	flags := &planFlags{}

	cmd := &cobra.Command{
		Use:   "plan [flags] <prompt>",
		Short: "Run planning mode to design implementations",
		Long: `Plan mode helps you design implementations by analyzing requirements and designing solutions.
The AI will explore the codebase, consider approaches, and produce a detailed plan.`,
		Example: `  yoloswe plan "Create a hello world Go program"
  yoloswe plan --model opus "Implement a REST API"
  echo "Add tests" | yoloswe plan
  yoloswe plan --build new --external-builder ./yoloswe "Add comprehensive tests"`,
		Args: cobra.ArbitraryArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runPlan(cmd, args, flags)
		},
	}

	cmd.Flags().StringVar(&flags.model, "model", "opus", "Model to use for planning: haiku, sonnet, opus")
	cmd.Flags().StringVar(&flags.workDir, "dir", "", "Working directory (defaults to current directory)")
	cmd.Flags().StringVar(&flags.recordDir, "record", "", "Directory for session recordings (defaults to ~/.yoloswe)")
	cmd.Flags().StringVar(&flags.systemPrompt, "system", "", "Custom system prompt")
	cmd.Flags().BoolVar(&flags.verbose, "verbose", false, "Show detailed tool results (errors are always shown)")
	cmd.Flags().BoolVar(&flags.simple, "simple", false, "Auto-answer questions with first option and export plan on completion")
	cmd.Flags().StringVar(&flags.build, "build", "", "After planning, execute: 'current' (same session) or 'new' (fresh session)")
	cmd.Flags().StringVar(&flags.externalBuilder, "external-builder", "", "Path to external builder executable (e.g., yoloswe build). Used with --build new.")
	cmd.Flags().StringVar(&flags.buildModel, "build-model", "sonnet", "Model to use for build phase (defaults to sonnet)")

	return cmd
}

func runPlan(cmd *cobra.Command, args []string, flags *planFlags) {
	// Get prompt from args or stdin
	prompt := strings.Join(args, " ")
	if prompt == "" {
		prompt = readFromStdin()
	}
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "Error: no prompt provided")
		cmd.Usage()
		os.Exit(1)
	}

	// Set default working directory
	workDir := flags.workDir
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting working directory: %v\n", err)
			os.Exit(1)
		}
	}

	// Validate build mode
	buildMode := planner.BuildMode(flags.build)
	if !buildMode.IsValid() {
		fmt.Fprintf(os.Stderr, "Error: invalid build mode %q. Valid values: 'current', 'new', or empty\n", flags.build)
		os.Exit(1)
	}

	// Create config
	config := planner.Config{
		Model:               flags.model,
		WorkDir:             workDir,
		RecordingDir:        flags.recordDir,
		SystemPrompt:        flags.systemPrompt,
		Verbose:             flags.verbose,
		Simple:              flags.simple,
		Prompt:              prompt,
		BuildMode:           buildMode,
		ExternalBuilderPath: flags.externalBuilder,
		BuildModel:          flags.buildModel,
	}

	// Create planner wrapper
	p := planner.NewPlannerWrapper(config)

	// Setup context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Fprintln(os.Stderr, "\nInterrupted, shutting down...")
		cancel()
	}()

	// Start the session
	if err := p.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting session: %v\n", err)
		os.Exit(1)
	}
	defer p.Stop()

	// Run the planner
	if err := p.Run(ctx, prompt); err != nil {
		if ctx.Err() != nil {
			// Context cancelled, exit gracefully
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Print usage summary
	p.PrintUsageSummary()

	// Print recording path
	if path := p.RecordingPath(); path != "" {
		fmt.Fprintf(os.Stderr, "\nSession recorded to: %s\n", path)
	}
}

// Build command flags
type buildFlags struct {
	builderModel    string
	reviewerModel   string
	dir             string
	record          string
	systemPrompt    string
	resumeSession   string
	budget          float64
	timeout         int
	maxIterations   int
	verbose         bool
	requireApproval bool
}

func newBuildCmd() *cobra.Command {
	flags := &buildFlags{}

	cmd := &cobra.Command{
		Use:   "build [flags] <prompt>",
		Short: "Run a builder-reviewer loop for software engineering tasks",
		Long: `Build runs a builder-reviewer loop for software engineering tasks.
The builder (Claude) implements the task, and the reviewer (Codex) reviews.
The loop continues until the reviewer accepts or limits are reached.`,
		Example: `  yoloswe build "Add unit tests for the user service"
  yoloswe build --budget 10 --timeout 1800 "Refactor the database layer"
  yoloswe build --builder-model opus "Fix the authentication bug"
  yoloswe build "Implement feature X" --timeout 7200`,
		Args: cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			runBuild(cmd, args, flags)
		},
	}

	cmd.Flags().StringVar(&flags.builderModel, "builder-model", "sonnet", "Builder model: haiku, sonnet, opus")
	cmd.Flags().StringVar(&flags.reviewerModel, "reviewer-model", "gpt-5.2-codex", "Reviewer model: gpt-5.2-codex, o4-mini")
	cmd.Flags().StringVar(&flags.dir, "dir", "", "Working directory (default: current)")
	cmd.Flags().Float64Var(&flags.budget, "budget", 100.0, "Max USD for builder session")
	cmd.Flags().IntVar(&flags.timeout, "timeout", 3600, "Max seconds")
	cmd.Flags().IntVar(&flags.maxIterations, "max-iterations", 100, "Max builder-reviewer iterations")
	cmd.Flags().StringVar(&flags.record, "record", "", "Session recordings directory (default: ~/.yoloswe)")
	cmd.Flags().BoolVar(&flags.verbose, "verbose", false, "Show detailed output")
	cmd.Flags().StringVar(&flags.systemPrompt, "system", "", "Custom system prompt for builder")
	cmd.Flags().BoolVar(&flags.requireApproval, "require-approval", false, "Require user approval for tool executions (default: auto-approve)")
	cmd.Flags().StringVar(&flags.resumeSession, "resume", "", "Resume from a previous session ID")

	return cmd
}

func runBuild(cmd *cobra.Command, args []string, flags *buildFlags) {
	prompt := strings.Join(args, " ")

	// Get working directory
	workDir := flags.dir
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting working directory: %v\n", err)
			os.Exit(1)
		}
	}

	// Set default recording directory if not specified
	recordingDir := flags.record
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
		BuilderModel:    flags.builderModel,
		BuilderWorkDir:  workDir,
		RecordingDir:    recordingDir,
		SystemPrompt:    flags.systemPrompt,
		RequireApproval: flags.requireApproval,
		ResumeSessionID: flags.resumeSession,
		ReviewerModel:   flags.reviewerModel,
		Goal:            prompt, // Use prompt as goal
		MaxBudgetUSD:    flags.budget,
		MaxTimeSeconds:  flags.timeout,
		MaxIterations:   flags.maxIterations,
		Verbose:         flags.verbose,
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

// readFromStdin reads input from stdin if available.
func readFromStdin() string {
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		// stdin is a terminal, not piped input
		return ""
	}

	var lines []string
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return strings.Join(lines, "\n")
}
