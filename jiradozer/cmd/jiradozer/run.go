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

type runArgs struct {
	description     string
	planFile        string
	configPath      string
	workDir         string
	modelID         string
	thinkingLevel   string
	runStep         string
	autoApprove     string
	skipPhases      string
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
	postResult      bool
}

func newRunCommand(args *runArgs) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the jiradozer workflow",
		Long:  "Execute the plan → build → validate → ship workflow against one or many issues.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			args.dryRunSet = dryRunChanged(cmd)
			app := cliapp.FromContext(cmd.Context())
			return run(cmd.Context(), app, *args)
		},
	}
	cmd.SilenceUsage = true
	registerRunFlags(cmd, args)
	return cmd
}

// dryRunChanged reports whether --dry-run was set on cmd or any ancestor.
// The flag is registered on both the root command (for the legacy
// `jiradozer --dry-run --filter ...` invocation) and the `run` subcommand,
// so cobra parses it on whichever FlagSet matches the user's placement —
// `jiradozer --dry-run run` lands on root, `jiradozer run --dry-run` on run.
// Without walking the tree we silently drop the user's flag.
func dryRunChanged(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Flags().Changed("dry-run") {
			return true
		}
	}
	return false
}

// registerRunFlags binds the run-specific flags onto cmd. Shared between the
// `run` subcommand and the root command's back-compat shim, so plain
// `jiradozer --issue X` (the legacy invocation pattern) still parses.
func registerRunFlags(cmd *cobra.Command, args *runArgs) {
	cmd.Flags().StringVar(&args.issueID, "issue", "", "Issue identifier for single-issue mode (e.g. ENG-123, owner/repo#42, or https://github.com/owner/repo/issues/42)")
	cmd.Flags().StringVar(&args.workDir, "work-dir", "", "Working directory (overrides config)")
	cmd.Flags().StringVar(&args.modelID, "model", "", "Agent model ID (overrides config)")
	cmd.Flags().StringVar(&args.thinkingLevel, "thinking-level", "", "Agent reasoning effort level: low, medium, high, max, auto (overrides config; rejected by providers without an effort knob, e.g. cursor, gemini)")
	cmd.Flags().DurationVar(&args.pollInterval, "poll-interval", 0, "Comment polling interval (overrides config)")
	cmd.Flags().Float64Var(&args.maxBudget, "max-budget", 0, "Max budget in USD (overrides config)")
	cmd.Flags().StringVar(&args.runStep, "run-step", "", "Run a single step and exit (for debugging): plan, build, create_pr, validate, ship")
	cmd.Flags().BoolVar(&args.postResult, "post-result", false, "When used with --run-step, post the step output as an issue comment. Single-shot steps use comment_template; steps with rounds use round_comment_template and post one aggregate comment after all rounds complete.")
	cmd.Flags().StringVar(&args.autoApprove, "auto-approve", "", "Auto-approve review steps (comma-separated: plan,build,validate,ship or 'all')")
	cmd.Flags().StringVar(&args.skipPhases, "skip-phases", "", "Skip high-level workflow phases for this run (comma-separated: plan,build,validate,ship; create_pr is part of build)")
	cmd.Flags().StringArrayVar(&args.sourceFilters, "filter", nil, "Issue filter as key=value (repeatable, e.g. --filter team=ENG --filter state=Todo,Backlog)")
	cmd.Flags().IntVar(&args.maxConcurrent, "max-concurrent", 0, "Max concurrent workflows (overrides config)")
	cmd.Flags().StringVar(&args.branchPrefix, "branch-prefix", "", "Worktree branch prefix (overrides config)")
	cmd.Flags().StringVar(&args.description, "description", "", "Task description for local mode (no external tracker needed)")
	cmd.Flags().StringVar(&args.descriptionFile, "description-file", "", "Read task description from file (use - for stdin)")
	cmd.Flags().StringVar(&args.planFile, "plan-file", "", "Plan file for build step (use - for stdin)")
	cmd.Flags().BoolVar(&args.dryRun, "dry-run", false, "Team mode only: for each newly-discovered issue, print the equivalent `bramble new-session` command instead of launching a workflow. TUI remains empty — look at stdout for the printed commands.")
	cmd.Flags().BoolVar(&args.forceCleanup, "force-cleanup", false, "Team mode only: delete worktrees even for failed or cancelled runs. By default, failed and cancelled worktrees are preserved so in-progress work (including pushed branches / open PRs) is not lost.")
}

func run(ctx context.Context, app *cliapp.App, args runArgs) (runErr error) {
	logger := app.Logger
	renderer := app.Renderer
	if renderer != nil {
		defer renderer.Reset()
	}

	// Failure reporting: fire once if the run returns an error. The sinks
	// (tracker comment, external notifier) and target are populated below as
	// config/tracker are resolved; until then they are nil/empty and reporting
	// is a no-op. A context cancellation (Ctrl-C / shutdown) is an expected
	// stop, not a failure, so it is not reported.
	var (
		reportTracker  jiradozer.CommentPoster
		reportNotifier jiradozer.Notifier
		reportIssueID  string
		reportTarget   string
	)
	defer func() {
		if runErr == nil || ctx.Err() != nil {
			return
		}
		jiradozer.ReportFailure(ctx, logger, reportTracker, reportIssueID, reportNotifier, jiradozer.FailureReport{
			Tool:          "jiradozer",
			Target:        reportTarget,
			Step:          jiradozer.FailingStepFromError(runErr),
			Err:           runErr,
			BuildRevision: app.Build.ShortRevision(),
			LogPath:       app.LogPath,
		})
	}()

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
	if args.postResult && args.runStep == "" {
		return fmt.Errorf("--post-result requires --run-step")
	}

	cfg, err := loadRunConfig(args)
	if err != nil {
		return err
	}
	logger.Info("using agent",
		"model", cfg.Agent.Model,
		"work_dir", cfg.WorkDir,
		"base_branch", cfg.BaseBranch,
		"poll_interval", cfg.PollInterval,
	)
	if cfg.Notify.SlackWebhook != "" {
		reportNotifier = jiradozer.SlackWebhookNotifier{WebhookURL: cfg.Notify.SlackWebhook}
	}

	// Create tracker client.
	issueTracker, err := createTracker(cfg, args.issueID)
	if err != nil {
		return err
	}
	reportTracker = issueTracker

	// Local description mode.
	if args.description != "" {
		// No tracker issue to comment on in description mode; external
		// notification still fires. Target is a short description prefix.
		reportTracker = nil
		reportTarget = describeTarget(args.description)
		return runFromDescription(ctx, args.description, args.runStep, args.planContent, issueTracker, args.postResult, cfg, renderer, logger)
	}

	// Multi-issue team mode (only when no --issue flag was given).
	if cfg.Source.HasSource() && args.issueID == "" {
		// Per-issue failures are isolated and reported by the orchestrator; a
		// top-level error here is a batch-level failure.
		reportTracker = nil
		reportTarget = "batch run"
		return runMultiIssue(ctx, app, issueTracker, cfg, args)
	}

	// Single-issue headless mode.
	logger.Info("fetching issue", "identifier", args.issueID)
	issue, err := issueTracker.FetchIssue(ctx, args.issueID)
	if err != nil {
		return fmt.Errorf("fetch issue: %w", err)
	}
	logger.Info("found issue", "id", issue.ID, "title", issue.Title, "state", issue.State)
	reportIssueID = issue.ID
	reportTarget = issue.Identifier

	if args.runStep != "" {
		return runSingleStep(ctx, args.runStep, issue, cfg, args.planContent, issueTracker, args.postResult, renderer, logger)
	}

	// Run the full workflow.
	wf := jiradozer.NewWorkflow(issueTracker, issue, cfg, logger)
	wf.SetRenderer(renderer)
	return wf.Run(ctx)
}

// describeTarget makes a compact, single-line label from a free-form task
// description for use in failure reports.
func describeTarget(description string) string {
	return jiradozer.Truncate(strings.TrimSpace(strings.ReplaceAll(description, "\n", " ")), 80)
}

func loadRunConfig(args runArgs) (*jiradozer.Config, error) {
	// Load config. Prompts and comment templates live exclusively in YAML;
	// there are no built-in defaults. Both tracker-backed and local
	// description modes therefore require a config file.
	cfg, err := jiradozer.LoadConfig(args.configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if args.description != "" {
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
	}

	// Apply CLI flag overrides.
	if args.workDir != "" {
		cfg.WorkDir = jiradozer.ExpandHome(args.workDir)
	}
	if args.modelID != "" {
		cfg.Agent.Model = args.modelID
	}
	if args.thinkingLevel != "" {
		if _, err := agent.ParseEffort(args.thinkingLevel); err != nil {
			return nil, fmt.Errorf("--thinking-level: %w", err)
		}
		cfg.Agent.Effort = args.thinkingLevel
	}
	if args.pollInterval > 0 {
		cfg.PollInterval = args.pollInterval
	}
	if args.maxBudget > 0 {
		cfg.MaxBudgetUSD = args.maxBudget
	}
	if args.skipPhases != "" {
		if err := cfg.ApplySkipPhases(tracker.SplitCSV(args.skipPhases), "cli"); err != nil {
			return nil, fmt.Errorf("--skip-phases: %w", err)
		}
	}

	// Apply source overrides.
	if len(args.sourceFilters) > 0 {
		if cfg.Source.Filters == nil {
			cfg.Source.Filters = make(map[string]string)
		}
		for _, kv := range args.sourceFilters {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return nil, fmt.Errorf("invalid --filter %q: expected key=value", kv)
			}
			k = strings.TrimSpace(k)
			if k == "" {
				return nil, fmt.Errorf("invalid --filter %q: empty key", kv)
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
		return nil, fmt.Errorf("--issue and --description/--description-file are mutually exclusive")
	}
	if modeCount == 0 {
		return nil, fmt.Errorf("either --issue, --filter, --description/--description-file, or source.filters in config is required")
	}

	if err := validateDryRunMode(cfg, args); err != nil {
		return nil, err
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
				return nil, fmt.Errorf("unknown step %q in --auto-approve (valid: %s, all)", s, strings.Join(allSteps, ", "))
			}
		}
	}

	// Validate work_dir after CLI overrides.
	if err := jiradozer.ValidateWorkDir(cfg.WorkDir); err != nil {
		return nil, err
	}

	// Validate agent model.
	if _, ok := agent.ModelByID(cfg.Agent.Model); !ok {
		return nil, fmt.Errorf("unknown model %q — available models: %s", cfg.Agent.Model, availableModels())
	}
	return cfg, nil
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
	sup, err := newTeamSupervisor(app, issueTracker, cfg, args)
	if err != nil {
		return err
	}
	return sup.Run(ctx)
}

// resolveWTManager detects the wt-managed repository from the current
// working directory, using the same logic as `wt` CLI.
func resolveWTManager() (*wt.Manager, error) {
	wtRoot, err := resolveWTRoot()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	repoName, err := wt.GetCurrentRepoName(ctx, &wt.DefaultGitRunner{}, wtRoot)
	if err != nil {
		return nil, fmt.Errorf("not in a wt-managed repository (WT_ROOT=%s): %w", wtRoot, err)
	}
	return wt.NewManager(wtRoot, repoName), nil
}

func resolveWTRoot() (string, error) {
	wtRoot := os.Getenv("WT_ROOT")
	if wtRoot != "" {
		return wtRoot, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, "worktrees"), nil
}

// buildChildArgs constructs CLI flags to propagate from the parent
// team-mode process to each child single-issue subprocess.
func buildChildArgs(app *cliapp.App, args runArgs, absConfigPath string) []string {
	// Group persistent (root-level) flags before the subcommand token, then
	// run-subcommand flags after. --config and --verbose/--verbosity/--color
	// are registered as PersistentFlags on root, so cobra accepts them either
	// side, but keeping them on root's side matches where they were declared
	// and keeps `jiradozer --help`-style introspection consistent. Children
	// invoke the `run` subcommand explicitly so their behavior is independent
	// of any future change to the back-compat root delegation.
	out := []string{"--config", absConfigPath}
	out = append(out, app.StandardChildArgs()...)
	out = append(out, "run")
	if args.modelID != "" {
		out = append(out, "--model", args.modelID)
	}
	if args.thinkingLevel != "" {
		out = append(out, "--thinking-level", args.thinkingLevel)
	}
	if args.maxBudget > 0 {
		out = append(out, "--max-budget", fmt.Sprintf("%.2f", args.maxBudget))
	}
	if args.autoApprove != "" {
		out = append(out, "--auto-approve", args.autoApprove)
	}
	if args.skipPhases != "" {
		out = append(out, "--skip-phases", args.skipPhases)
	}
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

func runFromDescription(ctx context.Context, description, runStep, planContent string, issueTracker tracker.IssueTracker, postResult bool, cfg *jiradozer.Config, renderer *render.Renderer, logger *slog.Logger) error {
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
		return runSingleStep(ctx, runStep, issue, cfg, planContent, issueTracker, postResult, renderer, logger)
	}

	wf := jiradozer.NewWorkflow(issueTracker, issue, cfg, logger)
	wf.SetRenderer(renderer)
	return wf.Run(ctx)
}

// runStepAgentDetailed is overridable so unit tests can stub the agent call.
var runStepAgentDetailed = jiradozer.RunStepAgent

type singleStepRun struct {
	ctx         context.Context
	poster      jiradozer.CommentPoster
	issue       *tracker.Issue
	cfg         *jiradozer.Config
	renderer    *render.Renderer
	logger      *slog.Logger
	runAgent    func(context.Context, string, jiradozer.PromptData, jiradozer.StepConfig, string, string, string, *render.Renderer, *slog.Logger) (jiradozer.StepAgentResult, error)
	stepName    string
	planContent string
	postResult  bool
}

func runSingleStep(ctx context.Context, stepName string, issue *tracker.Issue, cfg *jiradozer.Config, planContent string, poster jiradozer.CommentPoster, postResult bool, renderer *render.Renderer, logger *slog.Logger) error {
	return (&singleStepRun{
		ctx:         ctx,
		stepName:    stepName,
		issue:       issue,
		cfg:         cfg,
		planContent: planContent,
		poster:      poster,
		postResult:  postResult,
		renderer:    renderer,
		logger:      logger,
		runAgent:    runStepAgentDetailed,
	}).run()
}

func (r *singleStepRun) run() error {
	stepName := r.stepName
	stepCfg, ok := r.cfg.StepByName(stepName)
	if !ok {
		return fmt.Errorf("unknown step %q (valid: %s)", stepName, strings.Join(allSteps, ", "))
	}
	resolved := r.cfg.ResolveStep(stepCfg)
	data := jiradozer.NewPromptData(r.issue, r.cfg.BaseBranch)

	// Inject plan into prompt data for the build step.
	// Priority: --plan-file flag > persisted plan.md > no plan.
	if stepName == "build" {
		if r.planContent != "" {
			data.Plan = r.planContent
			r.logger.Info("using plan from --plan-file")
		} else {
			content, err := jiradozer.LoadPersistedPlan(r.cfg.WorkDir)
			if err != nil {
				return err
			}
			if content != "" {
				data.Plan = content
				r.logger.Info("loaded persisted plan", "path", jiradozer.PlanFilePath(r.cfg.WorkDir))
			}
		}
		if data.Plan == "" {
			r.logger.Warn("NO PLAN AVAILABLE — build step is running without a plan; use --plan-file to provide one, or run the plan step first")
		}
	}

	if len(resolved.Rounds) > 0 {
		return r.runRounds(data, stepCfg, resolved)
	}

	res, err := r.runAgent(r.ctx, stepName, data, resolved, r.cfg.WorkDir, "", "", r.renderer, r.logger)
	if err != nil {
		return fmt.Errorf("run-step %s: %w", stepName, err)
	}
	output := res.Output
	if output == "" {
		r.logger.Warn("agent produced no text output — the result may be in tool actions (check session log)", "step", stepName, "session_id", res.SessionID)
	} else {
		fmt.Printf("=== %s output ===\n%s\n", stepName, output)
	}
	return r.finish(stepCfg, output, 0)
}

func (r *singleStepRun) runRounds(data jiradozer.PromptData, stepCfg, resolved jiradozer.StepConfig) error {
	stepName := r.stepName
	totalRounds := len(resolved.Rounds)
	r.logger.Info("step: "+stepName, "rounds", totalRounds)
	rendererStatus(r.renderer, fmt.Sprintf("Step: %s (%d rounds)", stepName, totalRounds))

	var allOutputs []string
	var sessionIDs []string
	for i, round := range resolved.Rounds {
		if r.ctx.Err() != nil {
			return fmt.Errorf("run-step %s: %w", stepName, r.ctx.Err())
		}
		r.logger.Info("round start", "step", stepName, "round", i+1, "total", totalRounds)
		rendererStatus(r.renderer, fmt.Sprintf("Round %d/%d", i+1, totalRounds))

		var output string
		if round.IsCommand() {
			var err error
			output, err = jiradozer.RunCommand(r.ctx, stepName, data, round.Command, r.cfg.WorkDir, r.logger)
			if err != nil {
				return fmt.Errorf("run-step %s round %d/%d: %w", stepName, i+1, totalRounds, err)
			}
		} else {
			roundCfg := jiradozer.ResolveRound(round, resolved)
			res, err := r.runAgent(r.ctx, stepName, data, roundCfg, r.cfg.WorkDir, "", "", r.renderer, r.logger)
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
		r.logger.Warn("agent produced no text output across all rounds", "step", stepName, "session_ids", sessionIDs)
	}
	return r.finish(stepCfg, combined, totalRounds)
}

func (r *singleStepRun) finish(stepCfg jiradozer.StepConfig, output string, totalRounds int) error {
	if r.stepName == "plan" {
		jiradozer.PersistPlan(r.cfg.WorkDir, output, r.logger)
	}
	if r.postResult && r.poster == nil {
		return fmt.Errorf("--post-result requires a comment-capable tracker")
	}
	if r.postResult {
		if err := postStepResultComment(r.ctx, r.poster, r.issue, r.stepName, stepCfg, output, totalRounds); err != nil {
			return fmt.Errorf("post step result comment: %w", err)
		}
	}
	return nil
}

func postStepResultComment(ctx context.Context, t jiradozer.CommentPoster, issue *tracker.Issue, stepName string, stepCfg jiradozer.StepConfig, output string, totalRounds int) error {
	if totalRounds > 0 {
		if stepCfg.RoundCommentTemplate == "" {
			return fmt.Errorf("step %q has no round_comment_template configured", stepName)
		}
		_, err := jiradozer.PostRoundComment(ctx, t, issue.ID, stepName, stepCfg, output, totalRounds, totalRounds)
		return err
	}
	if stepCfg.CommentTemplate == "" {
		return fmt.Errorf("step %q has no comment_template configured", stepName)
	}
	_, err := jiradozer.PostStepComment(ctx, t, issue.ID, stepName, stepCfg, output)
	return err
}

// readFileOrStdin reads from the given path, or from stdin if path is "-".
func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// allSteps is the canonical step-name list, sourced from the jiradozer package
// so it can't drift from StepByName. Callers only read it (never mutate).
var allSteps = jiradozer.StepNames

func parseAutoApprove(value string) []string {
	if strings.TrimSpace(value) == "all" {
		return allSteps
	}
	return tracker.SplitCSV(value)
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
