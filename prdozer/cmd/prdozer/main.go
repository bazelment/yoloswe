// Command prdozer watches GitHub pull requests and keeps them merge-ready by
// invoking the /pr-polish skill in response to base moves, CI failures, or new
// review comments.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/logging/klogfmt"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/prdozer"
	"github.com/bazelment/yoloswe/wt"
)

func main() {
	var (
		configPath   string
		workDir      string
		modelID      string
		pollInterval time.Duration
		maxBudget    float64
		prList       string
		all          bool
		once         bool
		dryRun       bool
		local        bool
		autoMerge    bool
		verbose      bool
		verbosity    string
		color        string
		repoOverride string
	)
	rootCmd := &cobra.Command{
		Use:   "prdozer",
		Short: "Watch PRs and keep them merge-ready via /pr-polish",
		Long:  "Polls one or more GitHub pull requests at a configured interval. When the base moves, CI fails, or new review comments arrive, invokes the /pr-polish skill to bring the PR back to merge-ready state.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), runArgs{
				configPath:   configPath,
				workDir:      workDir,
				modelID:      modelID,
				pollInterval: pollInterval,
				maxBudget:    maxBudget,
				prList:       prList,
				all:          all,
				once:         once,
				dryRun:       dryRun,
				local:        local,
				autoMerge:    autoMerge,
				verbose:      verbose,
				verbosity:    verbosity,
				color:        color,
				repoOverride: repoOverride,
			})
		},
	}
	rootCmd.SilenceUsage = true

	rootCmd.Flags().StringVar(&configPath, "config", "prdozer.yaml", "Path to config file (optional)")
	rootCmd.Flags().StringVar(&workDir, "work-dir", "", "Working directory (overrides config)")
	rootCmd.Flags().StringVar(&modelID, "model", "", "Agent model ID (overrides config)")
	rootCmd.Flags().DurationVar(&pollInterval, "poll-interval", 0, "Polling interval (overrides config)")
	rootCmd.Flags().Float64Var(&maxBudget, "max-budget", 0, "Max budget USD per polish session (overrides config)")
	rootCmd.Flags().StringVar(&prList, "pr", "", "Watch specific PR number(s); comma-separated")
	rootCmd.Flags().BoolVar(&all, "all", false, "Watch all open PRs matching source.filter")
	rootCmd.Flags().BoolVar(&once, "once", false, "Run a single tick per PR then exit")
	rootCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Detect changes and log decisions without invoking the agent")
	rootCmd.Flags().BoolVar(&local, "local", false, "Pass --local to /pr-polish (no CI/bot wait)")
	rootCmd.Flags().BoolVar(&autoMerge, "auto-merge", false, "Merge PRs that become mergeable")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output (shorthand for --verbosity=verbose)")
	rootCmd.Flags().StringVar(&verbosity, "verbosity", "normal", "Output verbosity: quiet, normal, verbose, debug")
	rootCmd.Flags().StringVar(&color, "color", "auto", "Color output: auto, always, never")
	rootCmd.Flags().StringVar(&repoOverride, "repo", "", "Short repo name for state-file path (default: derive from cwd)")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

type runArgs struct {
	configPath   string
	workDir      string
	modelID      string
	prList       string
	verbosity    string
	color        string
	repoOverride string
	pollInterval time.Duration
	maxBudget    float64
	all          bool
	once         bool
	dryRun       bool
	local        bool
	autoMerge    bool
	verbose      bool
}

func run(ctx context.Context, args runArgs) error {
	v := render.ParseVerbosity(args.verbosity)
	if args.verbose && v < render.VerbosityVerbose {
		v = render.VerbosityVerbose
	}
	colorMode := render.ParseColorMode(args.color)

	stderrLevel := slog.LevelInfo
	if v >= render.VerbosityDebug {
		stderrLevel = slog.LevelDebug
	} else if v <= render.VerbosityQuiet {
		stderrLevel = slog.LevelWarn
	}

	var activeLogPath string
	if home, err := os.UserHomeDir(); err == nil {
		logDir := filepath.Join(home, ".prdozer", "logs")
		logPath := filepath.Join(logDir, fmt.Sprintf("prdozer-%s-%d.log",
			time.Now().Format("20060102-150405"), os.Getpid()))
		if err := os.MkdirAll(logDir, 0o755); err == nil {
			f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				defer f.Close()
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

	renderer := render.New(os.Stderr,
		render.WithVerbosity(v),
		render.WithColorMode(colorMode),
	)
	if activeLogPath != "" {
		renderer.Status("Logging to " + activeLogPath)
	}

	cwd, _ := os.Getwd()
	logger.Info("prdozer starting", "args", os.Args[1:], "cwd", cwd, "pid", os.Getpid())

	cfg, err := loadConfig(args)
	if err != nil {
		return err
	}
	if err := applyOverrides(cfg, args); err != nil {
		return err
	}
	if _, ok := agent.ModelByID(cfg.Agent.Model); !ok {
		return fmt.Errorf("unknown model %q", cfg.Agent.Model)
	}
	logger.Info("config",
		"model", cfg.Agent.Model,
		"poll_interval", cfg.PollInterval,
		"mode", cfg.Source.Mode,
		"max_concurrent", cfg.Source.MaxConcurrent,
		"local", cfg.Polish.Local,
		"auto_merge", cfg.Polish.AutoMerge,
	)

	gh := &wt.DefaultGHRunner{}
	if err := wt.CheckGitHubAuth(ctx, gh); err != nil {
		return err
	}
	self, err := currentGitHubLogin(ctx, gh)
	if err != nil {
		logger.Warn("could not determine GitHub login; self-comment filtering disabled", "error", err)
	}
	repo := args.repoOverride
	if repo == "" {
		repo = repoNameFromCwd(cwd)
	}

	var polish prdozer.PolishRunner
	if !args.dryRun {
		polish = prdozer.NewAgentPolisher(renderer, logger)
	}

	orch := prdozer.NewOrchestrator(cfg, gh, polish, cfg.WorkDir, repo, logger,
		prdozer.WithOrchRenderer(renderer),
		prdozer.WithOrchSelfLogin(self),
		prdozer.WithOrchDryRun(args.dryRun),
	)

	if args.once {
		_, err := orch.RunOnce(ctx)
		return err
	}
	return orch.Run(ctx)
}

func loadConfig(args runArgs) (*prdozer.Config, error) {
	if _, err := os.Stat(args.configPath); err == nil {
		return prdozer.LoadConfig(args.configPath)
	}
	// No config file is fine — we synthesize one from CLI flags.
	return prdozer.DefaultConfig(), nil
}

func applyOverrides(cfg *prdozer.Config, args runArgs) error {
	if args.workDir != "" {
		cfg.WorkDir = prdozer.ExpandHome(args.workDir)
	}
	if args.modelID != "" {
		cfg.Agent.Model = args.modelID
	}
	if args.pollInterval > 0 {
		cfg.PollInterval = args.pollInterval
	}
	if args.maxBudget > 0 {
		cfg.MaxBudgetUSD = args.maxBudget
		cfg.Polish.MaxBudgetUSD = args.maxBudget
	}
	if args.local {
		cfg.Polish.Local = true
	}
	if args.autoMerge {
		cfg.Polish.AutoMerge = true
	}
	// PR selection: --pr beats --all beats config.
	switch {
	case args.prList != "":
		nums, err := parsePRList(args.prList)
		if err != nil {
			return err
		}
		cfg.Source.Mode = prdozer.SourceModeList
		cfg.Source.PRs = nums
	case args.all:
		cfg.Source.Mode = prdozer.SourceModeAll
	}
	if cfg.Source.Mode == prdozer.SourceModeList && len(cfg.Source.PRs) == 0 {
		return fmt.Errorf("--pr requires at least one PR number")
	}
	return prdozer.ValidateWorkDir(cfg.WorkDir)
}

func parsePRList(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid PR number %q: %w", p, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func currentGitHubLogin(ctx context.Context, gh wt.GHRunner) (string, error) {
	res, err := gh.Run(ctx, []string{"api", "user", "--jq", ".login"}, "")
	if err != nil {
		if res != nil && res.Stderr != "" {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(res.Stderr))
		}
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// repoNameFromCwd derives a short repo identifier from the cwd's parent dir;
// callers can override with --repo. This matches the convention used by other
// tools in the monorepo (worktree layout: <repos-root>/<repo>/<branch>).
func repoNameFromCwd(cwd string) string {
	// cwd looks like .../worktrees/<repo>/<branch>; pick the directory two levels up.
	parent := filepath.Base(filepath.Dir(cwd))
	if parent == "" || parent == "." || parent == "/" {
		return filepath.Base(cwd)
	}
	return parent
}
