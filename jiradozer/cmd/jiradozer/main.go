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

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/jiradozer/tracker/linear"
	"github.com/bazelment/yoloswe/jiradozer/tui"
	"github.com/bazelment/yoloswe/logging/klogfmt"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

func main() {
	var (
		issueID       string
		configPath    string
		workDir       string
		modelID       string
		pollInterval  time.Duration
		maxBudget     float64
		runStep       string
		autoApprove   string
		team          string
		sourceStates  []string
		sourceLabels  []string
		maxConcurrent int
		branchPrefix  string
		verbose       bool
	)

	rootCmd := &cobra.Command{
		Use:   "jiradozer",
		Short: "Issue-driven development workflow",
		Long:  "Drives a plan → build → validate → ship workflow from an issue tracker with human-in-the-loop approval at each step.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), runArgs{
				issueID:       issueID,
				configPath:    configPath,
				workDir:       workDir,
				modelID:       modelID,
				pollInterval:  pollInterval,
				maxBudget:     maxBudget,
				runStep:       runStep,
				autoApprove:   autoApprove,
				team:          team,
				sourceStates:  sourceStates,
				sourceLabels:  sourceLabels,
				maxConcurrent: maxConcurrent,
				branchPrefix:  branchPrefix,
				verbose:       verbose,
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
	rootCmd.Flags().StringVar(&autoApprove, "auto-approve", "", "Auto-approve review steps (comma-separated: plan,build,validate,ship or 'all')")
	rootCmd.Flags().StringVar(&team, "team", "", "Team key for multi-issue mode (e.g. ENG)")
	rootCmd.Flags().StringSliceVar(&sourceStates, "source-states", nil, "Issue states to track (default: Todo)")
	rootCmd.Flags().StringSliceVar(&sourceLabels, "source-labels", nil, "Issue label filter")
	rootCmd.Flags().IntVar(&maxConcurrent, "max-concurrent", 0, "Max concurrent workflows (overrides config)")
	rootCmd.Flags().StringVar(&branchPrefix, "branch-prefix", "", "Worktree branch prefix (overrides config)")
	rootCmd.Flags().BoolVar(&verbose, "verbose", false, "Verbose logging")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

type runArgs struct {
	issueID       string
	configPath    string
	workDir       string
	modelID       string
	runStep       string
	autoApprove   string
	team          string
	branchPrefix  string
	sourceStates  []string
	sourceLabels  []string
	pollInterval  time.Duration
	maxBudget     float64
	maxConcurrent int
	verbose       bool
}

func run(ctx context.Context, args runArgs) error {
	// Set up logger.
	level := slog.LevelInfo
	if args.verbose {
		level = slog.LevelDebug
	}
	klogfmt.Init(klogfmt.WithLevel(level))
	logger := slog.Default()

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

	// Apply source overrides.
	if args.team != "" {
		cfg.Source.Team = args.team
	}
	if len(args.sourceStates) > 0 {
		cfg.Source.States = args.sourceStates
	}
	if len(args.sourceLabels) > 0 {
		cfg.Source.Labels = args.sourceLabels
	}
	if args.maxConcurrent > 0 {
		cfg.Source.MaxConcurrent = args.maxConcurrent
	}
	if args.branchPrefix != "" {
		cfg.Source.BranchPrefix = args.branchPrefix
	}

	// Validate mutual exclusivity.
	if args.issueID != "" && cfg.Source.Team != "" {
		return fmt.Errorf("--issue and --team (or source.team in config) are mutually exclusive")
	}
	if args.issueID == "" && cfg.Source.Team == "" {
		return fmt.Errorf("either --issue or --team (or source.team in config) is required")
	}

	// Apply auto-approve overrides.
	if args.autoApprove != "" {
		for _, s := range parseAutoApprove(args.autoApprove) {
			switch s {
			case "plan":
				cfg.Plan.AutoApprove = true
			case "build":
				cfg.Build.AutoApprove = true
			case "validate":
				cfg.Validate.AutoApprove = true
			case "ship":
				cfg.Ship.AutoApprove = true
			default:
				return fmt.Errorf("unknown step %q in --auto-approve (valid: plan, build, validate, ship, all)", s)
			}
		}
	}

	// Validate work_dir after CLI overrides.
	if err := jiradozer.ValidateWorkDir(cfg.WorkDir); err != nil {
		return err
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
	issueTracker, err := createTracker(cfg)
	if err != nil {
		return err
	}

	// Multi-issue TUI mode.
	if cfg.Source.Team != "" {
		return runMultiIssue(ctx, issueTracker, cfg, logger)
	}

	// Single-issue headless mode.
	logger.Info("fetching issue", "identifier", args.issueID)
	issue, err := issueTracker.FetchIssue(ctx, args.issueID)
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
	wf := jiradozer.NewWorkflow(issueTracker, issue, cfg, logger)
	return wf.Run(ctx)
}

// wtAdapter adapts wt.Manager to the jiradozer.WorktreeManager interface.
type wtAdapter struct {
	mgr *wt.Manager
}

func (a *wtAdapter) NewWorktree(ctx context.Context, branch, baseBranch, goal string) (string, error) {
	return a.mgr.New(ctx, branch, baseBranch, goal)
}

func (a *wtAdapter) RemoveWorktree(ctx context.Context, nameOrBranch string, deleteBranch bool) error {
	return a.mgr.Remove(ctx, nameOrBranch, deleteBranch)
}

func runMultiIssue(ctx context.Context, issueTracker tracker.IssueTracker, cfg *jiradozer.Config, logger *slog.Logger) error {
	// Determine repo root for worktree manager.
	// Use WorkDir as the root for wt.Manager — it should point to
	// the bare repo's parent directory.
	repoName := cfg.Source.Team // Use team key as repo name convention.
	wtMgr := &wtAdapter{mgr: wt.NewManager(cfg.WorkDir, repoName)}

	orch := jiradozer.NewOrchestrator(issueTracker, cfg, wtMgr, logger)
	disc := jiradozer.NewDiscovery(issueTracker, cfg.Source.ToFilter(), cfg.PollInterval, logger)

	go func() {
		if err := orch.RunWithDiscovery(ctx, disc); err != nil && ctx.Err() == nil {
			logger.Error("orchestrator error", "error", err)
		}
	}()

	p := tea.NewProgram(tui.NewModel(orch))
	_, err := p.Run()
	return err
}

func createTracker(cfg *jiradozer.Config) (tracker.IssueTracker, error) {
	switch cfg.Tracker.Kind {
	case "linear":
		return linear.NewClient(cfg.Tracker.APIKey), nil
	default:
		return nil, fmt.Errorf("unsupported tracker kind: %q", cfg.Tracker.Kind)
	}
}

var allSteps = []string{"plan", "build", "validate", "ship"}

func parseAutoApprove(value string) []string {
	if strings.TrimSpace(value) == "all" {
		return allSteps
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			result = append(result, s)
		}
	}
	return result
}

func availableModels() string {
	var names []string
	for _, m := range agent.AllModels {
		names = append(names, m.ID)
	}
	return fmt.Sprintf("[%s]", strings.Join(names, ", "))
}
