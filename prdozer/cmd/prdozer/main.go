// Command prdozer watches GitHub pull requests and keeps them merge-ready by
// invoking the /pr-polish skill in response to base moves, CI failures, or new
// review comments.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/cliapp"
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
		repoOverride string
	)
	opts := cliapp.Options{ToolName: "prdozer"}

	rootCmd := &cobra.Command{
		Use:   "prdozer",
		Short: "Watch PRs and keep them merge-ready via /pr-polish",
		Long:  "Polls one or more GitHub pull requests at a configured interval. When the base moves, CI fails, or new review comments arrive, invokes the /pr-polish skill to bring the PR back to merge-ready state.",
		RunE: func(_ *cobra.Command, _ []string) error {
			args := runArgs{
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
				repoOverride: repoOverride,
			}
			cliapp.Run(opts, func(ctx context.Context, app *cliapp.App) error {
				return run(ctx, app, args)
			})
			return nil // unreachable; cliapp.Run calls os.Exit
		},
	}
	rootCmd.SilenceUsage = true

	cliapp.RegisterStandardFlags(rootCmd, &opts)
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
	rootCmd.Flags().StringVar(&repoOverride, "repo", "", "Short repo name for state-file path (default: derive from cwd)")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

type runArgs struct {
	configPath   string
	workDir      string
	modelID      string
	prList       string
	repoOverride string
	pollInterval time.Duration
	maxBudget    float64
	all          bool
	once         bool
	dryRun       bool
	local        bool
	autoMerge    bool
}

func run(ctx context.Context, app *cliapp.App, args runArgs) error {
	logger := app.Logger
	renderer := app.Renderer

	cwd, _ := os.Getwd()
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
	_, err := os.Stat(args.configPath)
	switch {
	case err == nil:
		return prdozer.LoadConfig(args.configPath)
	case os.IsNotExist(err):
		// No config file is fine — we synthesize one from CLI flags.
		return prdozer.DefaultConfig(), nil
	default:
		// Permission denied, broken symlink, unreadable filesystem, etc.
		// Surfacing these stops prdozer from silently running with defaults
		// when the user intended to supply a config.
		return nil, fmt.Errorf("stat config %s: %w", args.configPath, err)
	}
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

// repoNameFromCwd derives a short repo identifier to namespace state files.
// Preference order:
//  1. `git remote get-url origin` → basename of the repo URL (strip .git).
//  2. Walk up from cwd until a `.git` entry is found; use that directory's name.
//  3. Fall back to the final path segment of cwd.
//
// Callers can override with --repo. The earlier heuristic of
// filepath.Base(filepath.Dir(cwd)) picked up the worktree parent directory
// (often "feature" or "worktrees") and produced colliding state-file paths.
func repoNameFromCwd(cwd string) string {
	if name := repoNameFromGitRemote(cwd); name != "" {
		return name
	}
	if name := repoNameFromGitDir(cwd); name != "" {
		return name
	}
	return filepath.Base(cwd)
}

func repoNameFromGitRemote(cwd string) string {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return ""
	}
	// Strip any trailing .git and take the final path segment. Handles both
	// https://github.com/owner/repo(.git) and git@github.com:owner/repo(.git).
	url = strings.TrimSuffix(url, ".git")
	for _, sep := range []string{"/", ":"} {
		if i := strings.LastIndex(url, sep); i >= 0 && i+1 < len(url) {
			url = url[i+1:]
		}
	}
	return url
}

func repoNameFromGitDir(cwd string) string {
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return filepath.Base(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
