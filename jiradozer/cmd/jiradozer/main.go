// Command jiradozer drives a development workflow from an issue tracker.
// It plans, builds, validates, and ships — with human approval at each step.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/jiradozer/tracker/linear"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

func main() {
	var (
		issueID      string
		configPath   string
		workDir      string
		modelID      string
		pollInterval time.Duration
		maxBudget    float64
		runStep      string
		verbose      bool
	)

	rootCmd := &cobra.Command{
		Use:   "jiradozer",
		Short: "Issue-driven development workflow",
		Long:  "Drives a plan → build → validate → ship workflow from an issue tracker with human-in-the-loop approval at each step.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), runArgs{
				issueID:      issueID,
				configPath:   configPath,
				workDir:      workDir,
				modelID:      modelID,
				pollInterval: pollInterval,
				maxBudget:    maxBudget,
				runStep:      runStep,
				verbose:      verbose,
			})
		},
	}

	rootCmd.Flags().StringVar(&issueID, "issue", "", "Issue identifier (e.g. ENG-123) [required]")
	rootCmd.Flags().StringVar(&configPath, "config", "jiradozer.yaml", "Path to config file")
	rootCmd.Flags().StringVar(&workDir, "work-dir", "", "Working directory (overrides config)")
	rootCmd.Flags().StringVar(&modelID, "model", "", "Agent model ID (overrides config)")
	rootCmd.Flags().DurationVar(&pollInterval, "poll-interval", 0, "Comment polling interval (overrides config)")
	rootCmd.Flags().Float64Var(&maxBudget, "max-budget", 0, "Max budget in USD (overrides config)")
	rootCmd.Flags().StringVar(&runStep, "run-step", "", "Run a single step and exit (for debugging): plan, build, validate, ship")
	rootCmd.Flags().BoolVar(&verbose, "verbose", false, "Verbose logging")

	_ = rootCmd.MarkFlagRequired("issue")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

type runArgs struct {
	issueID      string
	configPath   string
	workDir      string
	modelID      string
	runStep      string
	pollInterval time.Duration
	maxBudget    float64
	verbose      bool
}

func run(ctx context.Context, args runArgs) error {
	// Set up logger.
	level := slog.LevelInfo
	if args.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Load config.
	cfg, err := jiradozer.LoadConfig(args.configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Apply CLI flag overrides.
	if args.workDir != "" {
		cfg.WorkDir = jiradozer.ExpandHome(args.workDir)
	}
	if args.modelID != "" {
		cfg.Agent.Model = args.modelID
	}
	if args.pollInterval > 0 {
		cfg.PollInterval = args.pollInterval
	}
	if args.maxBudget > 0 {
		cfg.MaxBudgetUSD = args.maxBudget
	}

	// Validate agent model.
	if _, ok := agent.ModelByID(cfg.Agent.Model); !ok {
		return fmt.Errorf("unknown model %q — available models: %s", cfg.Agent.Model, availableModels())
	}
	logger.Info("using agent",
		"model", cfg.Agent.Model,
		"work_dir", cfg.WorkDir,
		"base_branch", cfg.BaseBranch,
		"poll_interval", cfg.PollInterval,
	)

	// Create tracker client.
	tracker, err := createTracker(cfg)
	if err != nil {
		return err
	}

	// Fetch the issue.
	logger.Info("fetching issue", "identifier", args.issueID)
	issue, err := tracker.FetchIssue(ctx, args.issueID)
	if err != nil {
		return fmt.Errorf("fetch issue: %w", err)
	}
	logger.Info("found issue", "id", issue.ID, "title", issue.Title, "state", issue.State)

	if args.runStep != "" {
		stepCfg, ok := cfg.StepByName(args.runStep)
		if !ok {
			return fmt.Errorf("unknown step %q (valid: plan, build, validate, ship)", args.runStep)
		}
		resolved := cfg.ResolveStep(stepCfg)
		data := jiradozer.NewPromptData(issue, cfg.BaseBranch)
		output, _, err := jiradozer.RunStepAgent(ctx, args.runStep, data, resolved, cfg.WorkDir, "", "", logger)
		if err != nil {
			return fmt.Errorf("run-step %s: %w", args.runStep, err)
		}
		fmt.Println(output)
		return nil
	}

	// Run the full workflow.
	wf := jiradozer.NewWorkflow(tracker, issue, cfg, logger)
	return wf.Run(ctx)
}

func createTracker(cfg *jiradozer.Config) (tracker.IssueTracker, error) {
	switch cfg.Tracker.Kind {
	case "linear":
		return linear.NewClient(cfg.Tracker.APIKey), nil
	default:
		return nil, fmt.Errorf("unsupported tracker kind: %q", cfg.Tracker.Kind)
	}
}

func availableModels() string {
	var names []string
	for _, m := range agent.AllModels {
		names = append(names, m.ID)
	}
	return fmt.Sprintf("[%s]", strings.Join(names, ", "))
}
