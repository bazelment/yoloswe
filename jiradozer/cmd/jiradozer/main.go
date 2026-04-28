// Command jiradozer drives a development workflow from an issue tracker.
// It plans, builds, validates, and ships — with human approval at each step.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	ghtracker "github.com/bazelment/yoloswe/jiradozer/tracker/github"
	"github.com/bazelment/yoloswe/jiradozer/tracker/linear"
	"github.com/bazelment/yoloswe/jiradozer/tracker/local"
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
		sourceFilters   []string
		maxConcurrent   int
		branchPrefix    string
		description     string
		descriptionFile string
		planFile        string
		dryRun          bool
		forceCleanup    bool
	)

	opts := cliapp.Options{
		ToolName:       "jiradozer",
		SensitiveFlags: []string{"--description"},
	}

	var args runArgs
	rootCmd := &cobra.Command{
		Use:   "jiradozer",
		Short: "Issue-driven development workflow",
		Long:  "Drives a plan → build → validate → ship workflow from an issue tracker with human-in-the-loop approval at each step.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			args = runArgs{
				issueID:         issueID,
				configPath:      configPath,
				workDir:         workDir,
				modelID:         modelID,
				pollInterval:    pollInterval,
				maxBudget:       maxBudget,
				runStep:         runStep,
				autoApprove:     autoApprove,
				sourceFilters:   sourceFilters,
				maxConcurrent:   maxConcurrent,
				branchPrefix:    branchPrefix,
				description:     description,
				descriptionFile: descriptionFile,
				planFile:        planFile,
				dryRun:          dryRun,
				dryRunSet:       cmd.Flags().Changed("dry-run"),
				forceCleanup:    forceCleanup,
			}
			app := cliapp.FromContext(cmd.Context())
			return run(cmd.Context(), app, args)
		},
	}
	rootCmd.SilenceUsage = true

	cliapp.RegisterStandardFlags(rootCmd, &opts)
	rootCmd.Flags().StringVar(&issueID, "issue", "", "Issue identifier for single-issue mode (e.g. ENG-123, owner/repo#42, or https://github.com/owner/repo/issues/42)")
	rootCmd.Flags().StringVar(&configPath, "config", "jiradozer.yaml", "Path to config file")
	rootCmd.Flags().StringVar(&workDir, "work-dir", "", "Working directory (overrides config)")
	rootCmd.Flags().StringVar(&modelID, "model", "", "Agent model ID (overrides config)")
	rootCmd.Flags().DurationVar(&pollInterval, "poll-interval", 0, "Comment polling interval (overrides config)")
	rootCmd.Flags().Float64Var(&maxBudget, "max-budget", 0, "Max budget in USD (overrides config)")
	rootCmd.Flags().StringVar(&runStep, "run-step", "", "Run a single step and exit (for debugging): plan, build, create_pr, validate, ship")
	rootCmd.Flags().StringVar(&autoApprove, "auto-approve", "", "Auto-approve review steps (comma-separated: plan,build,validate,ship or 'all')")
	rootCmd.Flags().StringArrayVar(&sourceFilters, "filter", nil, "Issue filter as key=value (repeatable, e.g. --filter team=ENG --filter state=Todo,Backlog)")
	rootCmd.Flags().IntVar(&maxConcurrent, "max-concurrent", 0, "Max concurrent workflows (overrides config)")
	rootCmd.Flags().StringVar(&branchPrefix, "branch-prefix", "", "Worktree branch prefix (overrides config)")
	rootCmd.Flags().StringVar(&description, "description", "", "Task description for local mode (no external tracker needed)")
	rootCmd.Flags().StringVar(&descriptionFile, "description-file", "", "Read task description from file (use - for stdin)")
	rootCmd.Flags().StringVar(&planFile, "plan-file", "", "Plan file for build step (use - for stdin)")
	rootCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Team mode only: for each newly-discovered issue, print the equivalent `bramble new-session` command instead of launching a workflow. TUI remains empty — look at stdout for the printed commands.")
	rootCmd.Flags().BoolVar(&forceCleanup, "force-cleanup", false, "Team mode only: delete worktrees even for failed or cancelled runs. By default, failed and cancelled worktrees are preserved so in-progress work (including pushed branches / open PRs) is not lost.")

	os.Exit(cliapp.Run(&opts, func(ctx context.Context, app *cliapp.App) error {
		return rootCmd.ExecuteContext(cliapp.WithApp(ctx, app))
	}))
}

type runArgs struct {
	description     string
	planFile        string
	configPath      string
	workDir         string
	modelID         string
	runStep         string
	autoApprove     string
	branchPrefix    string
	issueID         string
	descriptionFile string
	planContent     string
	sourceFilters   []string
	pollInterval    time.Duration
	maxBudget       float64
	maxConcurrent   int
	dryRun          bool
	dryRunSet       bool
	forceCleanup    bool
}

func run(ctx context.Context, app *cliapp.App, args runArgs) error {
	logger := app.Logger
	renderer := app.Renderer

	// Resolve --description-file into --description.
	if args.descriptionFile != "" {
		if args.description != "" {
			return fmt.Errorf("--description and --description-file are mutually exclusive")
		}
		if args.descriptionFile == "-" && args.planFile == "-" {
			return fmt.Errorf("cannot use stdin (-) for both --description-file and --plan-file")
		}
		data, err := readFileOrStdin(args.descriptionFile)
		if err != nil {
			return fmt.Errorf("read description file: %w", err)
		}
		args.description = strings.TrimSpace(string(data))
		if args.description == "" {
			return fmt.Errorf("description file is empty")
		}
	}

	// Resolve --plan-file into planContent.
	// Only meaningful for --run-step=build.
	if args.planFile != "" {
		if args.runStep != "build" {
			if args.runStep == "" {
				return fmt.Errorf("--plan-file requires --run-step=build")
			}
			return fmt.Errorf("--plan-file is only used with --run-step=build (got --run-step=%s)", args.runStep)
		}
		data, err := readFileOrStdin(args.planFile)
		if err != nil {
			return fmt.Errorf("read plan file: %w", err)
		}
		args.planContent = strings.TrimSpace(string(data))
		if args.planContent == "" {
			return fmt.Errorf("plan file is empty")
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
		cfg.Source.Filters = nil
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
	if len(args.sourceFilters) > 0 {
		if cfg.Source.Filters == nil {
			cfg.Source.Filters = make(map[string]string)
		}
		for _, kv := range args.sourceFilters {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("invalid --filter %q: expected key=value", kv)
			}
			k = strings.TrimSpace(k)
			if k == "" {
				return fmt.Errorf("invalid --filter %q: empty key", kv)
			}
			cfg.Source.Filters[k] = strings.TrimSpace(v)
		}
	}
	if args.maxConcurrent > 0 {
		cfg.Source.MaxConcurrent = args.maxConcurrent
	}
	if args.branchPrefix != "" {
		cfg.Source.BranchPrefix = args.branchPrefix
	}
	// --dry-run overrides the config in both directions: explicitly passing
	// --dry-run=true/false lets a user flip the setting without editing YAML.
	// When the flag is not set at all, the config value stands.
	if args.dryRunSet {
		cfg.Source.DryRun = args.dryRun
	}

	// Validate mutual exclusivity. CLI flags (--issue, --description) take
	// precedence over config-file values (source.filters). Only count
	// source.filters as a mode when no CLI flag is given, so users can have
	// source.filters in their config for multi-issue mode and still use
	// --issue for single-issue.
	modeCount := 0
	if args.issueID != "" {
		modeCount++
	}
	if args.description != "" {
		modeCount++
	}
	if modeCount == 0 && cfg.Source.HasSource() {
		modeCount++
	}
	if modeCount > 1 {
		return fmt.Errorf("--issue and --description/--description-file are mutually exclusive")
	}
	if modeCount == 0 {
		return fmt.Errorf("either --issue, --filter, --description/--description-file, or source.filters in config is required")
	}

	if err := validateDryRunMode(cfg, args); err != nil {
		return err
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
			case "create_pr":
				// create_pr has no review step; auto-approve is a no-op.
			default:
				return fmt.Errorf("unknown step %q in --auto-approve (valid: %s, all)", s, strings.Join(allSteps, ", "))
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
	issueTracker, err := createTracker(cfg, args.issueID)
	if err != nil {
		return err
	}

	// Local description mode.
	if args.description != "" {
		return runFromDescription(ctx, args.description, args.runStep, args.planContent, issueTracker, cfg, renderer, logger)
	}

	// Multi-issue team mode (only when no --issue flag was given).
	if cfg.Source.HasSource() && args.issueID == "" {
		return runMultiIssue(ctx, app, issueTracker, cfg, args)
	}

	// Single-issue headless mode.
	logger.Info("fetching issue", "identifier", args.issueID)
	issue, err := issueTracker.FetchIssue(ctx, args.issueID)
	if err != nil {
		return fmt.Errorf("fetch issue: %w", err)
	}
	logger.Info("found issue", "id", issue.ID, "title", issue.Title, "state", issue.State)

	if args.runStep != "" {
		return runSingleStep(ctx, args.runStep, issue, cfg, args.planContent, renderer, logger)
	}

	// Run the full workflow.
	wf := jiradozer.NewWorkflow(issueTracker, issue, cfg, logger)
	wf.SetRenderer(renderer)
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
	return a.mgr.Remove(ctx, nameOrBranch, deleteBranch, false)
}

// validateDryRunMode enforces that --dry-run is only used with team mode.
// Single-issue and local description modes already give the caller an
// obvious command to run, so dry-run is rejected in those paths.
func validateDryRunMode(cfg *jiradozer.Config, args runArgs) error {
	if !cfg.Source.DryRun {
		return nil
	}
	if args.issueID != "" || args.description != "" {
		return fmt.Errorf("--dry-run only applies to team mode (--filter or source.filters in config); --issue and --description do not support it")
	}
	return nil
}

// resolveRepoName picks the repo name used by wt.Manager (for worktree
// placement) and by dry-run printed commands (for --repo). For GitHub, the
// team filter is "owner/repo"; only the repo portion is used to avoid a
// nested worktree directory layout.
func resolveRepoName(cfg *jiradozer.Config) string {
	repoName := cfg.Source.Filters[tracker.FilterTeam]
	if repoName == "" {
		repoName = "jiradozer"
	}
	if cfg.Tracker.Kind == "github" {
		if _, repo, err := ghtracker.ParseOwnerRepo(repoName); err == nil {
			repoName = repo
		}
	}
	return repoName
}

func runMultiIssue(ctx context.Context, app *cliapp.App, issueTracker tracker.IssueTracker, cfg *jiradozer.Config, args runArgs) error {
	renderer := app.Renderer
	logger := app.Logger
	orchCtx, orchCancel := context.WithCancel(ctx)
	defer orchCancel()

	repoName := resolveRepoName(cfg)
	disc := jiradozer.NewDiscovery(issueTracker, cfg.Source.ToFilter(), cfg.PollInterval, logger)

	// Dry-run mode: print bramble new-session commands to stdout. No real
	// worktrees are needed; the wtManager won't be called.
	if cfg.Source.DryRun {
		dummyWtMgr := &wtAdapter{mgr: wt.NewManager(".", repoName)}
		orch := jiradozer.NewOrchestrator(issueTracker, cfg, dummyWtMgr, repoName, logger)
		err := orch.RunWithDiscovery(orchCtx, disc)
		orchCancel()
		orch.Shutdown()
		return err
	}

	// Non-dry-run: detect the real wt-managed repository.
	wtMgr, err := resolveWTManager()
	if err != nil {
		return fmt.Errorf("team mode requires a wt-managed repository: %w", err)
	}
	logger.Info("resolved wt-managed repository", "repo_dir", wtMgr.RepoDir())

	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve jiradozer binary path: %w", err)
	}
	absConfig, err := filepath.Abs(args.configPath)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	childArgs := buildChildArgs(app, args, absConfig)
	logDir, err := cliapp.LogDir("jiradozer")
	if err != nil {
		return fmt.Errorf("resolve log dir: %w", err)
	}

	orch := jiradozer.NewOrchestrator(issueTracker, cfg, &wtAdapter{mgr: wtMgr}, repoName, logger)
	orch.SetSubprocessMode(selfPath, childArgs, logDir)
	orch.SetForceCleanup(args.forceCleanup)

	// Print status updates to stderr.
	go func() {
		for status := range orch.StatusUpdates() {
			switch {
			case status.Step == jiradozer.StepInit:
				renderer.Status(fmt.Sprintf("[%s] started — %s", status.Issue.Identifier, status.Issue.Title))
			case status.Step == jiradozer.StepDone:
				elapsed := time.Since(status.StartedAt).Truncate(time.Second)
				renderer.Status(fmt.Sprintf("[%s] completed (%s)", status.Issue.Identifier, elapsed))
			case status.Step == jiradozer.StepCancelled:
				renderer.Status(fmt.Sprintf("[%s] cancelled", status.Issue.Identifier))
			case status.Step == jiradozer.StepFailed:
				renderer.Error(status.Error, fmt.Sprintf("[%s] failed", status.Issue.Identifier))
			}
		}
	}()

	err = orch.RunWithDiscovery(orchCtx, disc)
	orchCancel()
	orch.Shutdown()

	// Report preserved worktrees so the user knows what's left.
	if preserved := orch.PreservedWorktrees(); len(preserved) > 0 {
		fmt.Fprintf(os.Stderr, "\nPreserved %d worktree(s):\n", len(preserved))
		for _, pw := range preserved {
			reason := "cancelled"
			switch pw.Step {
			case jiradozer.StepDone:
				reason = "shipped (PR open, not yet merged)"
			case jiradozer.StepFailed:
				reason = "failed (inspect and recover; pushed branch / open PR still intact)"
			}
			fmt.Fprintf(os.Stderr, "  %s  %s  (%s)  [%s]\n", pw.Issue, pw.Branch, pw.WorktreePath, reason)
		}
		fmt.Fprintf(os.Stderr, "\nTo remove after merging: wt remove <branch>\n")
		fmt.Fprintf(os.Stderr, "To remove failed/cancelled worktrees on next run: re-run with --force-cleanup\n")
	}

	return err
}

// resolveWTManager detects the wt-managed repository from the current
// working directory, using the same logic as `wt` CLI.
func resolveWTManager() (*wt.Manager, error) {
	wtRoot := os.Getenv("WT_ROOT")
	if wtRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot determine home directory: %w", err)
		}
		wtRoot = filepath.Join(home, "worktrees")
	}
	ctx := context.Background()
	repoName, err := wt.GetCurrentRepoName(ctx, &wt.DefaultGitRunner{}, wtRoot)
	if err != nil {
		return nil, fmt.Errorf("not in a wt-managed repository (WT_ROOT=%s): %w", wtRoot, err)
	}
	return wt.NewManager(wtRoot, repoName), nil
}

// buildChildArgs constructs CLI flags to propagate from the parent
// team-mode process to each child single-issue subprocess.
func buildChildArgs(app *cliapp.App, args runArgs, absConfigPath string) []string {
	out := []string{"--config", absConfigPath}
	if args.modelID != "" {
		out = append(out, "--model", args.modelID)
	}
	if args.maxBudget > 0 {
		out = append(out, "--max-budget", fmt.Sprintf("%.2f", args.maxBudget))
	}
	if args.autoApprove != "" {
		out = append(out, "--auto-approve", args.autoApprove)
	}
	out = append(out, app.StandardChildArgs()...)
	return out
}

func createTracker(cfg *jiradozer.Config, issueID string) (tracker.IssueTracker, error) {
	switch cfg.Tracker.Kind {
	case "linear":
		return linear.NewClient(cfg.Tracker.APIKey), nil
	case "github":
		// When --issue is provided, always derive owner/repo from the identifier
		// to ensure the client is bound to the same repo as the issue. This
		// prevents mutations (comment, close) targeting a different repo than
		// the one the issue was fetched from.
		var owner, repo string
		if issueID != "" {
			var err error
			owner, repo, _, err = ghtracker.ParseIdentifier(issueID)
			if err != nil {
				return nil, fmt.Errorf("github tracker: %w", err)
			}
		} else if teamKey := cfg.Source.Filters[tracker.FilterTeam]; teamKey != "" {
			var err error
			owner, repo, err = ghtracker.ParseOwnerRepo(teamKey)
			if err != nil {
				return nil, fmt.Errorf("github tracker requires filter team as 'owner/repo': %w", err)
			}
		} else {
			return nil, fmt.Errorf("github tracker requires --filter team=owner/repo or --issue owner/repo#N")
		}
		return ghtracker.NewClient(&wt.DefaultGHRunner{}, owner, repo), nil
	case "local":
		dir := filepath.Join(cfg.WorkDir, ".jiradozer", "issues")
		return local.NewTracker(dir)
	default:
		return nil, fmt.Errorf("unsupported tracker kind: %q", cfg.Tracker.Kind)
	}
}

func runFromDescription(ctx context.Context, description, runStep, planContent string, issueTracker tracker.IssueTracker, cfg *jiradozer.Config, renderer *render.Renderer, logger *slog.Logger) error {
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

	if runStep != "" {
		return runSingleStep(ctx, runStep, issue, cfg, planContent, renderer, logger)
	}

	wf := jiradozer.NewWorkflow(issueTracker, issue, cfg, logger)
	wf.SetRenderer(renderer)
	return wf.Run(ctx)
}

// runStepAgentDetailed is overridable so unit tests can stub the agent call.
var runStepAgentDetailed = jiradozer.RunStepAgent

func runSingleStep(ctx context.Context, stepName string, issue *tracker.Issue, cfg *jiradozer.Config, planContent string, renderer *render.Renderer, logger *slog.Logger) error {
	stepCfg, ok := cfg.StepByName(stepName)
	if !ok {
		return fmt.Errorf("unknown step %q (valid: %s)", stepName, strings.Join(allSteps, ", "))
	}
	resolved := cfg.ResolveStep(stepCfg)
	data := jiradozer.NewPromptData(issue, cfg.BaseBranch)

	// Inject plan into prompt data for the build step.
	// Priority: --plan-file flag > persisted plan.md > no plan.
	if stepName == "build" {
		if planContent != "" {
			data.Plan = planContent
			logger.Info("using plan from --plan-file")
		} else {
			content, err := jiradozer.LoadPersistedPlan(cfg.WorkDir)
			if err != nil {
				return err
			}
			if content != "" {
				data.Plan = content
				logger.Info("loaded persisted plan", "path", jiradozer.PlanFilePath(cfg.WorkDir))
			}
		}
		if data.Plan == "" {
			logger.Warn("NO PLAN AVAILABLE — build step is running without a plan; use --plan-file to provide one, or run the plan step first")
		}
	}

	if len(resolved.Rounds) > 0 {
		return runSingleStepRounds(ctx, stepName, data, resolved, cfg.WorkDir, renderer, logger)
	}

	res, err := runStepAgentDetailed(ctx, stepName, data, resolved, cfg.WorkDir, "", "", renderer, logger)
	if err != nil {
		return fmt.Errorf("run-step %s: %w", stepName, err)
	}
	output := res.Output
	if output == "" {
		logger.Warn("agent produced no text output — the result may be in tool actions (check session log)", "step", stepName, "session_id", res.SessionID)
	} else {
		fmt.Printf("=== %s output ===\n%s\n", stepName, output)
	}
	if stepName == "plan" {
		jiradozer.PersistPlan(cfg.WorkDir, output, logger)
	}
	return nil
}

func runSingleStepRounds(ctx context.Context, stepName string, data jiradozer.PromptData, resolved jiradozer.StepConfig, workDir string, renderer *render.Renderer, logger *slog.Logger) error {
	totalRounds := len(resolved.Rounds)
	logger.Info("step: "+stepName, "rounds", totalRounds)
	rendererStatus(renderer, fmt.Sprintf("Step: %s (%d rounds)", stepName, totalRounds))

	var allOutputs []string
	var sessionIDs []string
	for i, round := range resolved.Rounds {
		if ctx.Err() != nil {
			return fmt.Errorf("run-step %s: %w", stepName, ctx.Err())
		}
		logger.Info("round start", "step", stepName, "round", i+1, "total", totalRounds)
		rendererStatus(renderer, fmt.Sprintf("Round %d/%d", i+1, totalRounds))

		var output string
		if round.IsCommand() {
			var err error
			output, err = jiradozer.RunCommand(ctx, stepName, data, round.Command, workDir, logger)
			if err != nil {
				return fmt.Errorf("run-step %s round %d/%d: %w", stepName, i+1, totalRounds, err)
			}
		} else {
			roundCfg := jiradozer.ResolveRound(round, resolved)
			res, err := runStepAgentDetailed(ctx, stepName, data, roundCfg, workDir, "", "", renderer, logger)
			if res.SessionID != "" {
				sessionIDs = append(sessionIDs, res.SessionID)
			}
			if err != nil {
				return fmt.Errorf("run-step %s round %d/%d: %w", stepName, i+1, totalRounds, err)
			}
			output = res.Output
		}
		allOutputs = append(allOutputs, output)
	}

	combined := jiradozer.JoinRoundOutputs(allOutputs)
	if combined != "" {
		fmt.Printf("=== %s output (%d rounds) ===\n%s\n", stepName, totalRounds, combined)
	} else {
		logger.Warn("agent produced no text output across all rounds", "step", stepName, "session_ids", sessionIDs)
	}
	if stepName == "plan" {
		jiradozer.PersistPlan(workDir, combined, logger)
	}
	return nil
}

// readFileOrStdin reads from the given path, or from stdin if path is "-".
func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

var allSteps = []string{"plan", "build", "create_pr", "validate", "ship"}

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

// rendererStatus is a nil-safe wrapper around renderer.Status.
func rendererStatus(r *render.Renderer, msg string) {
	if r != nil {
		r.Status(msg)
	}
}
