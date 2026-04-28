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
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/yoloswe"
	"github.com/bazelment/yoloswe/yoloswe/planner"
)

var rootOpts = cliapp.Options{ToolName: "yoloswe"}

func main() {
	rootCmd := &cobra.Command{
		Use:   "yoloswe",
		Short: "AI-assisted software engineering tool",
		Long: `yoloswe provides AI-assisted software engineering capabilities.

Use 'plan' to design and plan implementations before execution.
Use 'build' to run a builder-reviewer loop for autonomous task execution.`,
	}

	cliapp.RegisterStandardFlags(rootCmd, &rootOpts)
	rootCmd.AddCommand(newPlanCmd())
	rootCmd.AddCommand(newBuildCmd())

	os.Exit(cliapp.Run(&rootOpts, func(ctx context.Context, app *cliapp.App) error {
		return rootCmd.ExecuteContext(cliapp.WithApp(ctx, app))
	}))
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlan(cmd, args, flags)
		},
	}

	cmd.Flags().StringVar(&flags.model, "model", "opus", "Model to use for planning: haiku, sonnet, opus")
	cmd.Flags().StringVar(&flags.workDir, "dir", "", "Working directory (defaults to current directory)")
	cmd.Flags().StringVar(&flags.recordDir, "record", "", "Directory for session recordings (defaults to ~/.yoloswe)")
	cmd.Flags().StringVar(&flags.systemPrompt, "system", "", "Custom system prompt")
	cmd.Flags().BoolVar(&flags.simple, "simple", false, "Auto-answer questions with first option and export plan on completion")
	cmd.Flags().StringVar(&flags.build, "build", "", "After planning, execute: 'current' (same session) or 'new' (fresh session)")
	cmd.Flags().StringVar(&flags.externalBuilder, "external-builder", "", "Path to external builder executable (e.g., yoloswe build). Used with --build new.")
	cmd.Flags().StringVar(&flags.buildModel, "build-model", "sonnet", "Model to use for build phase (defaults to sonnet)")

	return cmd
}

func runPlan(cmd *cobra.Command, args []string, flags *planFlags) error {
	app := cliapp.FromContext(cmd.Context())

	prompt := strings.Join(args, " ")
	if prompt == "" {
		prompt = readFromStdin()
	}
	if prompt == "" {
		_ = cmd.Usage()
		return fmt.Errorf("no prompt provided")
	}

	workDir := flags.workDir
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
	}

	buildMode := planner.BuildMode(flags.build)
	if !buildMode.IsValid() {
		return fmt.Errorf("invalid build mode %q (valid: 'current', 'new', or empty)", flags.build)
	}

	config := planner.Config{
		Model:               flags.model,
		WorkDir:             workDir,
		RecordingDir:        flags.recordDir,
		SystemPrompt:        flags.systemPrompt,
		Verbose:             app.Verbosity >= render.VerbosityVerbose,
		Simple:              flags.simple,
		Prompt:              prompt,
		BuildMode:           buildMode,
		ExternalBuilderPath: flags.externalBuilder,
		BuildModel:          flags.buildModel,
	}

	p := planner.NewPlannerWrapper(config)

	ctx := cmd.Context()
	if err := p.Start(ctx); err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	defer p.Stop()

	if err := p.Run(ctx, prompt); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	p.PrintUsageSummary()
	if path := p.RecordingPath(); path != "" {
		fmt.Fprintf(os.Stderr, "\nSession recorded to: %s\n", path)
	}
	return nil
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
	requireApproval bool
	reviewFirst     bool
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(cmd, args, flags)
		},
	}

	cmd.Flags().StringVar(&flags.builderModel, "builder-model", "sonnet", "Builder model: haiku, sonnet, opus")
	cmd.Flags().StringVar(&flags.reviewerModel, "reviewer-model", "", "Reviewer model (default: gpt-5.4-mini)")
	cmd.Flags().StringVar(&flags.dir, "dir", "", "Working directory (default: current)")
	cmd.Flags().Float64Var(&flags.budget, "budget", 100.0, "Max USD for builder session")
	cmd.Flags().IntVar(&flags.timeout, "timeout", 3600, "Max seconds")
	cmd.Flags().IntVar(&flags.maxIterations, "max-iterations", 100, "Max builder-reviewer iterations")
	cmd.Flags().StringVar(&flags.record, "record", "", "Session recordings directory (default: ~/.yoloswe)")
	cmd.Flags().StringVar(&flags.systemPrompt, "system", "", "Custom system prompt for builder")
	cmd.Flags().BoolVar(&flags.requireApproval, "require-approval", false, "Require user approval for tool executions (default: auto-approve)")
	cmd.Flags().StringVar(&flags.resumeSession, "resume", "", "Resume from a previous session ID")
	cmd.Flags().BoolVar(&flags.reviewFirst, "review-first", false, "Skip first builder turn and start with review")

	return cmd
}

func runBuild(cmd *cobra.Command, args []string, flags *buildFlags) error {
	app := cliapp.FromContext(cmd.Context())
	prompt := strings.Join(args, " ")

	workDir := flags.dir
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
	}

	recordingDir := flags.record
	if recordingDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("get home directory: %w", err)
		}
		recordingDir = filepath.Join(homeDir, ".yoloswe")
	}

	config := yoloswe.Config{
		BuilderModel:    flags.builderModel,
		BuilderWorkDir:  workDir,
		RecordingDir:    recordingDir,
		SystemPrompt:    flags.systemPrompt,
		RequireApproval: flags.requireApproval,
		ResumeSessionID: flags.resumeSession,
		ReviewFirst:     flags.reviewFirst,
		ReviewerModel:   flags.reviewerModel,
		Goal:            prompt,
		MaxBudgetUSD:    flags.budget,
		MaxTimeSeconds:  flags.timeout,
		MaxIterations:   flags.maxIterations,
		Verbose:         app.Verbosity >= render.VerbosityVerbose,
	}

	app.Logger.Info("yoloswe build config",
		"builder_model", config.BuilderModel,
		"reviewer_model", config.ReviewerModel,
		"work_dir", config.BuilderWorkDir,
		"budget_usd", config.MaxBudgetUSD,
		"timeout_seconds", config.MaxTimeSeconds,
		"max_iterations", config.MaxIterations,
		"prompt", prompt,
	)

	swe := yoloswe.New(config)
	runErr := swe.Run(cmd.Context(), prompt)
	swe.PrintSummary()
	if runErr != nil {
		return runErr
	}
	if swe.Stats().ExitReason != yoloswe.ExitReasonAccepted {
		return fmt.Errorf("build did not complete successfully (reason: %v)", swe.Stats().ExitReason)
	}
	return nil
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
