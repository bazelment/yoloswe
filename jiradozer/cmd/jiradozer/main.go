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

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	ghtracker "github.com/bazelment/yoloswe/jiradozer/tracker/github"
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
		sourceFilters   []string
		maxConcurrent   int
		branchPrefix    string
		verbose         bool
		verbosity       string
		color           string
		description     string
		descriptionFile string
		planFile        string
		dryRun          bool
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
				sourceFilters:   sourceFilters,
				maxConcurrent:   maxConcurrent,
				branchPrefix:    branchPrefix,
				verbose:         verbose,
				verbosity:       verbosity,
				color:           color,
				description:     description,
				descriptionFile: descriptionFile,
				planFile:        planFile,
				dryRun:          dryRun,
				dryRunSet:       cmd.Flags().Changed("dry-run"),
			})
		},
	}
	rootCmd.SilenceUsage = true

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
	rootCmd.Flags().BoolVar(&verbose, "verbose", false, "Verbose output (shorthand for --verbosity=verbose)")
	rootCmd.Flags().StringVar(&verbosity, "verbosity", "normal", "Output verbosity: quiet, normal, verbose, debug")
	rootCmd.Flags().StringVar(&color, "color", "auto", "Color output: auto, always, never")
	rootCmd.Flags().StringVar(&description, "description", "", "Task description for local mode (no external tracker needed)")
	rootCmd.Flags().StringVar(&descriptionFile, "description-file", "", "Read task description from file (use - for stdin)")
	rootCmd.Flags().StringVar(&planFile, "plan-file", "", "Plan file for build step (use - for stdin)")
	rootCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Team mode only: for each newly-discovered issue, print the equivalent `bramble new-session` command instead of launching a workflow. TUI remains empty — look at stdout for the printed commands.")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
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
	color           string
	descriptionFile string
	planContent     string
	verbosity       string
	sourceFilters   []string
	pollInterval    time.Duration
	maxBudget       float64
	maxConcurrent   int
	verbose         bool
	dryRun          bool
	dryRunSet       bool
}

func run(ctx context.Context, args runArgs) error {
	// Resolve verbosity: --verbose is shorthand for --verbosity=verbose.
	v := render.ParseVerbosity(args.verbosity)
	if args.verbose && v < render.VerbosityVerbose {
		v = render.VerbosityVerbose
	}
	colorMode := render.ParseColorMode(args.color)

	// Map verbosity to a slog level for stderr fallback paths.
	// The log file always uses LevelDebug (full detail); stderr respects --verbosity.
	stderrLevel := slog.LevelInfo
	if v >= render.VerbosityDebug {
		stderrLevel = slog.LevelDebug
	} else if v <= render.VerbosityQuiet {
		stderrLevel = slog.LevelWarn
	}

	// Set up logger: log file gets DEBUG-level detail, terminal output goes through renderer.
	var activeLogPath string
	if home, err := os.UserHomeDir(); err == nil {
		logDir := filepath.Join(home, ".jiradozer", "logs")
		logPath := filepath.Join(logDir, fmt.Sprintf("jiradozer-%s-%d.log",
			time.Now().Format("20060102-150405"), os.Getpid()))
		if err := os.MkdirAll(logDir, 0o755); err == nil {
			f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				defer f.Close()
				// Log file only — all terminal output goes through the renderer.
				slog.SetDefault(slog.New(klogfmt.New(f, klogfmt.WithLevel(slog.LevelDebug))))
				activeLogPath = logPath
			} else {
				klogfmt.Init(klogfmt.WithLevel(stderrLevel))
				slog.Warn("failed to open log file, logging to stderr only", "path", logPath, "error", err)
			}
		} else {
			klogfmt.Init(klogfmt.WithLevel(stderrLevel))
			slog.Warn("failed to create log directory, logging to stderr only", "path", logDir, "error", err)
		}
	} else {
		klogfmt.Init(klogfmt.WithLevel(stderrLevel))
		slog.Warn("could not determine home directory, logging to stderr only", "error", err)
	}
	logger := slog.Default()

	// Create the terminal renderer for headless modes (not TUI).
	renderer := render.New(os.Stderr,
		render.WithVerbosity(v),
		render.WithColorMode(colorMode),
	)
	if activeLogPath != "" {
		renderer.Status("Logging to " + activeLogPath)
	}

	// Log invocation banner with CLI args and working directory.
	// Redact values of flags that may contain secrets.
	cwd, _ := os.Getwd()
	logger.Info("jiradozer starting", "args", redactArgs(os.Args[1:]), "cwd", cwd, "pid", os.Getpid())

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

	// Multi-issue TUI mode (only when no --issue flag was given).
	// TUI owns the terminal — don't use the renderer.
	if cfg.Source.HasSource() && args.issueID == "" {
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

func runMultiIssue(ctx context.Context, issueTracker tracker.IssueTracker, cfg *jiradozer.Config, logger *slog.Logger) error {
	repoName := resolveRepoName(cfg)
	wtMgr := &wtAdapter{mgr: wt.NewManager(cfg.WorkDir, repoName)}

	// Use a cancellable context so we can stop the orchestrator when the TUI exits.
	orchCtx, orchCancel := context.WithCancel(ctx)
	defer orchCancel()

	orch := jiradozer.NewOrchestrator(issueTracker, cfg, wtMgr, repoName, logger)
	disc := jiradozer.NewDiscovery(issueTracker, cfg.Source.ToFilter(), cfg.PollInterval, logger)

	// Dry-run mode: run the orchestrator headlessly (no TUI) so that the
	// printed bramble commands are visible on stdout. The alternate screen
	// used by Bubbletea would hide and then destroy any stdout output.
	if cfg.Source.DryRun {
		err := orch.RunWithDiscovery(orchCtx, disc)
		orchCancel()
		orch.Shutdown()
		return err
	}

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
		planPath := jiradozer.PlanFilePath(cfg.WorkDir)
		if planContent != "" {
			data.Plan = planContent
			logger.Info("using plan from --plan-file")
		} else if content, err := os.ReadFile(planPath); err == nil {
			data.Plan = strings.TrimSpace(string(content))
			logger.Info("loaded persisted plan", "path", planPath)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read persisted plan %s: %w", planPath, err)
		}
		if data.Plan == "" {
			logger.Warn("NO PLAN AVAILABLE — build step is running without a plan; use --plan-file to provide one, or run the plan step first")
		}
	}

	if len(resolved.Rounds) > 0 {
		return runSingleStepRounds(ctx, stepName, data, resolved, cfg.WorkDir, renderer, logger)
	}

	output, sessionID, err := jiradozer.RunStepAgent(ctx, stepName, data, resolved, cfg.WorkDir, "", "", renderer, logger)
	if err != nil {
		return fmt.Errorf("run-step %s: %w", stepName, err)
	}
	if output == "" {
		logger.Warn("agent produced no text output — the result may be in tool actions (check session log)", "step", stepName, "session_id", sessionID)
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
			var sessionID string
			var err error
			output, sessionID, err = jiradozer.RunStepAgent(ctx, stepName, data, roundCfg, workDir, "", "", renderer, logger)
			if err != nil {
				return fmt.Errorf("run-step %s round %d/%d: %w", stepName, i+1, totalRounds, err)
			}
			sessionIDs = append(sessionIDs, sessionID)
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

// sensitiveFlags lists flag prefixes whose values should be redacted from logs.
var sensitiveFlags = []string{"--api-key", "--token", "--secret", "--password", "--description"}

// redactArgs returns a copy of args with values of sensitive flags replaced by "***".
// rendererStatus is a nil-safe wrapper around renderer.Status.
func rendererStatus(r *render.Renderer, msg string) {
	if r != nil {
		r.Status(msg)
	}
}

func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, arg := range out {
		for _, prefix := range sensitiveFlags {
			// --flag=value form
			if strings.HasPrefix(arg, prefix+"=") {
				out[i] = prefix + "=***"
				break
			}
			// --flag value form: redact the next arg
			if arg == prefix && i+1 < len(out) {
				out[i+1] = "***"
				break
			}
		}
	}
	return out
}
