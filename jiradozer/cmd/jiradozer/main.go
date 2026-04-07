// Command jiradozer drives a development workflow from an issue tracker.
// It plans, builds, validates, and ships — with human approval at each step.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/jiradozer/tracker/linear"
	"github.com/bazelment/yoloswe/jiradozer/tracker/local"
	"github.com/bazelment/yoloswe/jiradozer/tui"
	"github.com/bazelment/yoloswe/logging/klogfmt"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

func main() {
	var (
		issueID         string
		configPath      string
		workDir         string
		modelID         string
		pollInterval    time.Duration
		maxBudget       float64
		runStep         string
		autoApprove     string
		team            string
		sourceStates    []string
		sourceLabels    []string
		maxConcurrent   int
		branchPrefix    string
		verbose         bool
		description     string
		descriptionFile string
	)

	rootCmd := &cobra.Command{
		Use:   "jiradozer",
		Short: "Issue-driven development workflow",
		Long:  "Drives a plan → build → validate → ship workflow from an issue tracker with human-in-the-loop approval at each step.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), runArgs{
				issueID:         issueID,
				configPath:      configPath,
				workDir:         workDir,
				modelID:         modelID,
				pollInterval:    pollInterval,
				maxBudget:       maxBudget,
				runStep:         runStep,
				autoApprove:     autoApprove,
				team:            team,
				sourceStates:    sourceStates,
				sourceLabels:    sourceLabels,
				maxConcurrent:   maxConcurrent,
				branchPrefix:    branchPrefix,
				verbose:         verbose,
				description:     description,
				descriptionFile: descriptionFile,
			})
		},
	}
	rootCmd.SilenceUsage = true

	rootCmd.Flags().StringVar(&issueID, "issue", "", "Issue identifier for single-issue mode (e.g. ENG-123)")
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
	rootCmd.Flags().StringVar(&description, "description", "", "Task description for local mode (no external tracker needed)")
	rootCmd.Flags().StringVar(&descriptionFile, "description-file", "", "Read task description from file (use - for stdin)")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

type runArgs struct {
	issueID         string
	configPath      string
	workDir         string
	modelID         string
	runStep         string
	autoApprove     string
	team            string
	branchPrefix    string
	description     string
	descriptionFile string
	sourceStates    []string
	sourceLabels    []string
	pollInterval    time.Duration
	maxBudget       float64
	maxConcurrent   int
	verbose         bool
}

func run(ctx context.Context, args runArgs) error {
	// Set up logger.
	level := slog.LevelInfo
	if args.verbose {
		level = slog.LevelDebug
	}
	klogfmt.Init(klogfmt.WithLevel(level))
	logger := slog.Default()

	// Resolve --description-file into --description.
	if args.descriptionFile != "" {
		if args.description != "" {
			return fmt.Errorf("--description and --description-file are mutually exclusive")
		}
		var (
			data []byte
			err  error
		)
		if args.descriptionFile == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(args.descriptionFile)
		}
		if err != nil {
			return fmt.Errorf("read description file: %w", err)
		}
		args.description = strings.TrimSpace(string(data))
		if args.description == "" {
			return fmt.Errorf("description file is empty")
		}
	}

	// Load config — use defaults when running in local description mode
	// (config file is optional).
	var cfg *jiradozer.Config
	if args.description != "" {
		// Try loading config file for overrides but don't fail if missing.
		loaded, loadErr := jiradozer.LoadConfig(args.configPath)
		if loadErr == nil {
			cfg = loaded
		} else {
			cfg = jiradozer.DefaultConfig()
		}
		// Force local tracker, clear team, and reset state names to match the
		// local tracker's fixed states ("In Progress", "In Review", "Done").
		cfg.Tracker.Kind = "local"
		cfg.Source.Team = ""
		defaults := jiradozer.DefaultConfig()
		cfg.States = defaults.States
		// Default all steps to auto-approve in local mode unless overridden.
		if args.autoApprove == "" {
			cfg.Plan.AutoApprove = true
			cfg.Build.AutoApprove = true
			cfg.Validate.AutoApprove = true
			cfg.Ship.AutoApprove = true
		}
	} else {
		var err error
		cfg, err = jiradozer.LoadConfig(args.configPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
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
	modeCount := 0
	if args.issueID != "" {
		modeCount++
	}
	if cfg.Source.Team != "" {
		modeCount++
	}
	if args.description != "" {
		modeCount++
	}
	if modeCount > 1 {
		return fmt.Errorf("--issue, --team, and --description/--description-file are mutually exclusive")
	}
	if modeCount == 0 {
		return fmt.Errorf("either --issue, --team, or --description/--description-file is required")
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

	// Local description mode.
	if args.description != "" {
		return runFromDescription(ctx, args.description, issueTracker, cfg, logger)
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
		output, sessionID, err := jiradozer.RunStepAgent(ctx, args.runStep, data, resolved, cfg.WorkDir, "", "", logger)
		if err != nil {
			return fmt.Errorf("run-step %s: %w", args.runStep, err)
		}
		if output == "" {
			logger.Warn("agent produced no text output — the result may be in tool actions (check session log)", "step", args.runStep, "session_id", sessionID)
		} else {
			fmt.Printf("=== %s output ===\n%s\n", args.runStep, output)
		}
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

	// Use a cancellable context so we can stop the orchestrator when the TUI exits.
	orchCtx, orchCancel := context.WithCancel(ctx)
	defer orchCancel()

	orch := jiradozer.NewOrchestrator(issueTracker, cfg, wtMgr, logger)
	disc := jiradozer.NewDiscovery(issueTracker, cfg.Source.ToFilter(), cfg.PollInterval, logger)

	go func() {
		if err := orch.RunWithDiscovery(orchCtx, disc); err != nil && orchCtx.Err() == nil {
			logger.Error("orchestrator error", "error", err)
		}
	}()

	p := tea.NewProgram(tui.NewModel(orch))
	_, err := p.Run()

	// Signal shutdown (unblocks any pending terminal status sends),
	// cancel the orchestrator context, and wait for all workflows to
	// drain so worktrees are cleaned up before the process exits.
	orchCancel()
	orch.Shutdown()

	return err
}

func createTracker(cfg *jiradozer.Config) (tracker.IssueTracker, error) {
	switch cfg.Tracker.Kind {
	case "linear":
		return linear.NewClient(cfg.Tracker.APIKey), nil
	case "local":
		dir := filepath.Join(cfg.WorkDir, ".jiradozer", "issues")
		return local.NewTracker(dir)
	default:
		return nil, fmt.Errorf("unsupported tracker kind: %q", cfg.Tracker.Kind)
	}
}

func runFromDescription(ctx context.Context, description string, issueTracker tracker.IssueTracker, cfg *jiradozer.Config, logger *slog.Logger) error {
	lt, ok := issueTracker.(*local.Tracker)
	if !ok {
		return fmt.Errorf("--description requires local tracker (got %T)", issueTracker)
	}

	title := jiradozer.GenerateTitle(description)
	logger.Info("title generated", "title", title)

	issue, err := lt.CreateIssue(title, description)
	if err != nil {
		return fmt.Errorf("create local issue: %w", err)
	}
	logger.Info("created local issue", "identifier", issue.Identifier, "title", issue.Title)

	wf := jiradozer.NewWorkflow(issueTracker, issue, cfg, logger)
	return wf.Run(ctx)
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
