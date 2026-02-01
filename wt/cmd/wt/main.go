// wt - Git worktree CLI for power users managing multiple concurrent branches.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/wt"
)

var (
	repoFlag string
	wtRoot   string
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "wt",
	Short: "Git worktree CLI for power users",
	Long: `wt - Git worktree CLI for power users managing multiple concurrent branches.

A CLI tool for managing Git worktrees with support for bare clones,
branch management, and shell navigation.

Environment:
  WT_ROOT     Base directory for worktrees (default: ~/worktrees)`,
}

func init() {
	wtRoot = os.Getenv("WT_ROOT")
	if wtRoot == "" {
		home, _ := os.UserHomeDir()
		wtRoot = filepath.Join(home, "worktrees")
	}

	rootCmd.PersistentFlags().StringVarP(&repoFlag, "repo", "R", "",
		"Target repository (default: auto-detect from cwd)")

	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(newCmd)
	rootCmd.AddCommand(openCmd)
	rootCmd.AddCommand(lsCmd)
	rootCmd.AddCommand(rmCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(mergeCmd)
	rootCmd.AddCommand(prCmd)
	rootCmd.AddCommand(cdCmd)
	rootCmd.AddCommand(goalCmd)
	rootCmd.AddCommand(pruneCmd)
	rootCmd.AddCommand(shellenvCmd)
}

// getManager creates a Manager, resolving repo from flag or cwd.
func getManager() (*wt.Manager, error) {
	ctx := context.Background()
	output := wt.DefaultOutput()

	if repoFlag != "" {
		barePath := filepath.Join(wtRoot, repoFlag, ".bare")
		if _, err := os.Stat(barePath); os.IsNotExist(err) {
			output.Error(fmt.Sprintf("Repository '%s' not found", repoFlag))
			repos, _ := wt.ListAllRepos(wtRoot)
			if len(repos) > 0 {
				output.Info(fmt.Sprintf("Available: %s", strings.Join(repos, ", ")))
			}
			return nil, fmt.Errorf("repository not found")
		}
		return wt.NewManager(wtRoot, repoFlag), nil
	}

	repoName, err := wt.GetCurrentRepoName(ctx, &wt.DefaultGitRunner{}, wtRoot)
	if err != nil {
		output.Error("Not in a wt-managed repository. Use --repo to specify one.")
		repos, _ := wt.ListAllRepos(wtRoot)
		if len(repos) > 0 {
			output.Info(fmt.Sprintf("Available: %s", strings.Join(repos, ", ")))
		}
		return nil, err
	}

	return wt.NewManager(wtRoot, repoName), nil
}

// initCmd: wt init <repo-url>
var initCmd = &cobra.Command{
	Use:   "init <repo-url>",
	Short: "Initialize repo with bare clone",
	Long: `Init creates a bare clone and sets up the default branch worktree.

Rough commands:
  git clone --bare <url> .bare/
  git config remote.origin.fetch "+refs/heads/*:refs/remotes/origin/*"
  git fetch origin
  git worktree add <default-branch>/ <default-branch>`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := args[0]
		repoName := wt.GetRepoNameFromURL(url)
		m := wt.NewManager(wtRoot, repoName)
		ctx := context.Background()

		mainPath, err := m.Init(ctx, url)
		if err != nil {
			return err
		}

		fmt.Printf("__WT_CD__:%s\n", mainPath)
		return nil
	},
}

// newCmd: wt new <branch> [--from X] [--goal X]
var newCmd = &cobra.Command{
	Use:   "new <branch>",
	Short: "Create new branch worktree",
	Long: `New creates a worktree with a new branch from a base branch.

Rough commands:
  git fetch origin
  git worktree add -b <branch> <path> origin/<base>
  git config branch.<branch>.description "parent:<base>"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := getManager()
		if err != nil {
			return err
		}

		branch := args[0]
		baseBranch, _ := cmd.Flags().GetString("from")
		goal, _ := cmd.Flags().GetString("goal")
		ctx := context.Background()

		path, err := m.New(ctx, branch, baseBranch, goal)
		if err != nil {
			return err
		}

		fmt.Printf("__WT_CD__:%s\n", path)
		return nil
	},
}

func init() {
	newCmd.Flags().StringP("from", "f", "", "Base branch")
	newCmd.Flags().StringP("goal", "g", "", "High-level goal for this worktree")
}

// openCmd: wt open <branch> [--goal X]
var openCmd = &cobra.Command{
	Use:   "open <branch>",
	Short: "Open existing remote branch",
	Long: `Open creates a worktree for an existing remote branch.

Rough commands:
  git fetch origin
  git worktree add <path> <branch>   # auto-tracks origin/<branch>
  git config branch.<branch>.description "parent:<default-branch>"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := getManager()
		if err != nil {
			return err
		}

		branch := args[0]
		goal, _ := cmd.Flags().GetString("goal")
		ctx := context.Background()

		path, err := m.Open(ctx, branch, goal)
		if err != nil {
			return err
		}

		fmt.Printf("__WT_CD__:%s\n", path)
		return nil
	},
}

func init() {
	openCmd.Flags().StringP("goal", "g", "", "High-level goal for this worktree")
}

// lsCmd: wt ls [--json] [-a]
var lsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List all worktrees",
	Long: `Ls lists all worktrees in the current repository.

Rough commands:
  git worktree list --porcelain`,
	RunE: func(cmd *cobra.Command, args []string) error {
		allRepos, _ := cmd.Flags().GetBool("all")
		jsonOutput, _ := cmd.Flags().GetBool("json")

		// List all repos mode
		if allRepos {
			repos, err := wt.ListAllRepos(wtRoot)
			if err != nil {
				return err
			}

			if jsonOutput {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(repos)
			}

			if len(repos) == 0 {
				wt.DefaultOutput().Info("No repositories found")
				return nil
			}

			output := wt.DefaultOutput()
			fmt.Printf("\n%-30s %s\n", "Repository", "Path")
			fmt.Println(strings.Repeat("-", 70))
			for _, repo := range repos {
				repoStr := output.Colorize(wt.ColorCyan, repo)
				path := filepath.Join(wtRoot, repo)
				fmt.Printf("%-39s %s\n", repoStr, path)
			}
			fmt.Println()
			return nil
		}

		// List worktrees for current repo
		m, err := getManager()
		if err != nil {
			return err
		}

		ctx := context.Background()
		worktrees, err := m.List(ctx)
		if err != nil {
			return err
		}

		if jsonOutput {
			data := make([]map[string]any, len(worktrees))
			for i, w := range worktrees {
				goal, _ := m.GetGoal(ctx, w.Branch, w.Path)
				data[i] = map[string]any{
					"branch":   w.Branch,
					"path":     w.Path,
					"commit":   w.Commit,
					"detached": w.IsDetached,
					"goal":     goal,
				}
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(data)
		}

		if len(worktrees) == 0 {
			wt.DefaultOutput().Info("No worktrees found")
			return nil
		}

		output := wt.DefaultOutput()

		// Check if any worktree has a goal
		hasGoals := false
		goals := make(map[string]string)
		for _, w := range worktrees {
			goal, _ := m.GetGoal(ctx, w.Branch, w.Path)
			if goal != "" {
				hasGoals = true
				goals[w.Branch] = goal
			}
		}

		if hasGoals {
			fmt.Printf("\n%-25s %-40s %-8s %s\n", "Branch", "Path", "Status", "Goal")
			fmt.Println(strings.Repeat("-", 105))
		} else {
			fmt.Printf("\n%-25s %-50s %-10s\n", "Branch", "Path", "Status")
			fmt.Println(strings.Repeat("-", 85))
		}

		for _, w := range worktrees {
			status, _ := m.GetStatus(ctx, w)
			statusStr := output.Colorize(wt.ColorGreen, "clean")
			if status.IsDirty {
				statusStr = output.Colorize(wt.ColorYellow, "dirty")
			}
			branchStr := output.Colorize(wt.ColorCyan, truncate(w.Branch, 24))
			if hasGoals {
				goal := goals[w.Branch]
				fmt.Printf("%-34s %-40s %-8s %s\n", branchStr, truncate(w.Path, 39), statusStr, truncate(goal, 30))
			} else {
				fmt.Printf("%-34s %-50s %s\n", branchStr, w.Path, statusStr)
			}
		}
		fmt.Println()

		return nil
	},
}

func init() {
	lsCmd.Flags().BoolP("json", "j", false, "JSON output")
	lsCmd.Flags().BoolP("all", "a", false, "List all repositories")
}

// rmCmd: wt rm <branch> [-D]
var rmCmd = &cobra.Command{
	Use:   "rm <branch>",
	Short: "Remove worktree",
	Long: `Rm removes a worktree. With -D, also deletes local and remote branches.

Rough commands:
  git worktree remove <path>
  git branch -D <branch>         # with -D flag
  git push origin --delete <branch>  # with -D flag`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := getManager()
		if err != nil {
			return err
		}

		branch := args[0]
		deleteBranch, _ := cmd.Flags().GetBool("delete-branch")
		ctx := context.Background()

		return m.Remove(ctx, branch, deleteBranch)
	},
}

func init() {
	rmCmd.Flags().BoolP("delete-branch", "D", false, "Delete branch too")
}

// statusCmd: wt status [-a]
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show status dashboard",
	Long: `Status shows a dashboard of all worktrees with sync and PR status.

Rough commands:
  git worktree list --porcelain
  git status --porcelain              # per worktree
  git rev-list --left-right --count   # ahead/behind
  gh pr view --json ...               # PR info`,
	RunE: func(cmd *cobra.Command, args []string) error {
		allRepos, _ := cmd.Flags().GetBool("all")
		ctx := context.Background()
		output := wt.DefaultOutput()

		// Get list of repos to process
		var repos []string
		if allRepos {
			var err error
			repos, err = wt.ListAllRepos(wtRoot)
			if err != nil {
				return err
			}
			if len(repos) == 0 {
				output.Info("No repositories found")
				return nil
			}
		} else {
			m, err := getManager()
			if err != nil {
				return err
			}
			repoName, _ := filepath.Rel(wtRoot, m.RepoDir())
			repos = []string{repoName}
		}

		first := true
		for _, repoName := range repos {
			m := wt.NewManager(wtRoot, repoName)
			worktrees, err := m.List(ctx)
			if err != nil {
				continue
			}
			if len(worktrees) == 0 {
				continue
			}

			if allRepos {
				if !first {
					fmt.Println()
				}
				fmt.Printf("%s\n", output.Colorize(wt.ColorBold, repoName))
			}
			first = false

			fmt.Printf("\n%s %s %s %s %s\n",
				wt.Pad("Branch", 41), wt.Pad("Sync", 12), wt.Pad("Status", 8), wt.Pad("Last Commit", 12), "PR")
			fmt.Println(strings.Repeat("-", 91))

			for _, w := range worktrees {
				status, _ := m.GetStatus(ctx, w)

				// Sync status
				var syncStr string
				if w.IsDetached {
					syncStr = output.Colorize(wt.ColorDim, "detached")
				} else if status.Ahead == 0 && status.Behind == 0 {
					syncStr = output.Colorize(wt.ColorGreen, "up to date")
				} else {
					var parts []string
					if status.Ahead > 0 {
						parts = append(parts, output.Colorize(wt.ColorGreen, fmt.Sprintf("↑%d", status.Ahead)))
					}
					if status.Behind > 0 {
						parts = append(parts, output.Colorize(wt.ColorRed, fmt.Sprintf("↓%d", status.Behind)))
					}
					syncStr = strings.Join(parts, " ")
				}

				// Status
				statusStr := output.Colorize(wt.ColorGreen, "clean")
				if status.IsDirty {
					statusStr = output.Colorize(wt.ColorYellow, "dirty")
				}

				// Last commit time
				var timeStr string
				if !status.LastCommitTime.IsZero() {
					delta := time.Since(status.LastCommitTime)
					if delta.Hours() >= 24 {
						timeStr = fmt.Sprintf("%dd ago", int(delta.Hours()/24))
					} else if delta.Hours() >= 1 {
						timeStr = fmt.Sprintf("%dh ago", int(delta.Hours()))
					} else {
						timeStr = fmt.Sprintf("%dm ago", int(delta.Minutes()))
					}
				} else {
					timeStr = "-"
				}

				// PR with status
				prStr := "-"
				if status.PRNumber > 0 {
					prNum := fmt.Sprintf("#%d", status.PRNumber)
					switch {
					case status.PRState == "MERGED":
						prStr = output.Colorize(wt.ColorGreen, prNum+" merged")
					case status.PRState == "CLOSED":
						prStr = output.Colorize(wt.ColorDim, prNum+" closed")
					case status.PRIsDraft:
						prStr = output.Colorize(wt.ColorDim, prNum+" draft")
					case status.PRReviewStatus == "APPROVED":
						prStr = output.Colorize(wt.ColorGreen, prNum+" approved")
					case status.PRReviewStatus == "CHANGES_REQUESTED":
						prStr = output.Colorize(wt.ColorRed, prNum+" changes")
					default:
						prStr = prNum
					}
				}

				branchStr := output.Colorize(wt.ColorCyan, truncate(w.Branch, 40))
				fmt.Printf("%s %s %s %s %s\n",
					wt.Pad(branchStr, 41), wt.Pad(syncStr, 12), wt.Pad(statusStr, 8), wt.Pad(timeStr, 12), prStr)
			}
		}
		fmt.Println()

		return nil
	},
}

func init() {
	statusCmd.Flags().BoolP("all", "a", false, "Show status for all repositories")
}

// syncCmd: wt sync [-a]
var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Fetch and rebase all worktrees",
	Long: `Sync fetches the latest changes and rebases all worktrees.

For cascading branches (created with --from), sync automatically detects
when a parent branch has been merged and rebases onto the default branch.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		allRepos, _ := cmd.Flags().GetBool("all")
		ctx := context.Background()
		output := wt.DefaultOutput()

		// Get list of repos to process
		var repos []string
		if allRepos {
			var err error
			repos, err = wt.ListAllRepos(wtRoot)
			if err != nil {
				return err
			}
			if len(repos) == 0 {
				output.Info("No repositories found")
				return nil
			}
		} else {
			m, err := getManager()
			if err != nil {
				return err
			}
			repoName, _ := filepath.Rel(wtRoot, m.RepoDir())
			repos = []string{repoName}
		}

		for i, repoName := range repos {
			if allRepos {
				if i > 0 {
					fmt.Println()
				}
				fmt.Printf("%s\n", output.Colorize(wt.ColorBold, repoName))
			}

			m := wt.NewManager(wtRoot, repoName)
			if err := m.Sync(ctx); err != nil {
				output.Error(fmt.Sprintf("Failed to sync %s: %v", repoName, err))
			}
		}

		return nil
	},
}

func init() {
	syncCmd.Flags().BoolP("all", "a", false, "Sync all repositories")
}

// mergeCmd: wt merge [--keep] [--squash|--rebase|--merge]
var mergeCmd = &cobra.Command{
	Use:   "merge",
	Short: "Merge current branch's PR and cleanup",
	Long: `Merge the PR for the current branch, remove the worktree, and handle cascading branches.

By default, this command will:
1. Merge the PR using the repository's default merge method
2. Remove the worktree and delete local/remote branches
3. Find any branches that were based on this one
4. Rebase those branches onto the default branch
5. Update their PR base branches

Use --keep to skip worktree/branch cleanup.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := getManager()
		if err != nil {
			return err
		}

		keep, _ := cmd.Flags().GetBool("keep")
		squash, _ := cmd.Flags().GetBool("squash")
		rebaseFlag, _ := cmd.Flags().GetBool("rebase")
		mergeCommit, _ := cmd.Flags().GetBool("merge")

		// Determine merge method
		var mergeMethod string
		if squash {
			mergeMethod = "squash"
		} else if rebaseFlag {
			mergeMethod = "rebase"
		} else if mergeCommit {
			mergeMethod = "merge"
		}

		ctx := context.Background()
		opts := wt.MergeOptions{
			Keep:        keep,
			MergeMethod: mergeMethod,
		}

		return m.MergePR(ctx, opts)
	},
}

func init() {
	mergeCmd.Flags().BoolP("keep", "k", false, "Keep worktree and branches after merge")
	mergeCmd.Flags().Bool("squash", false, "Squash merge the PR")
	mergeCmd.Flags().Bool("rebase", false, "Rebase merge the PR")
	mergeCmd.Flags().Bool("merge", false, "Create a merge commit")
}

// prCmd: wt pr [--title X] [--body X] [--base X] [--draft] [--no-push]
var prCmd = &cobra.Command{
	Use:   "pr",
	Short: "Push and create a GitHub PR",
	Long: `Push the current branch to origin and create a GitHub Pull Request.

Base branch is auto-detected:
  1. Explicit --base flag
  2. Parent branch (for cascading branches created with --from)
  3. Repository default branch

Examples:
  wt pr                           # Auto-detect base
  wt pr --draft                   # Create draft PR
  wt pr --base develop            # Target develop
  wt pr -t "Add feature X"        # With title`,
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := getManager()
		if err != nil {
			return err
		}

		title, _ := cmd.Flags().GetString("title")
		body, _ := cmd.Flags().GetString("body")
		base, _ := cmd.Flags().GetString("base")
		draft, _ := cmd.Flags().GetBool("draft")
		noPush, _ := cmd.Flags().GetBool("no-push")

		ctx := context.Background()
		result, err := m.CreatePR(ctx, wt.PROptions{
			Title:  title,
			Body:   body,
			Base:   base,
			Draft:  draft,
			NoPush: noPush,
		})
		if err != nil {
			return err
		}

		// Display result
		output := wt.DefaultOutput()
		if result.Existed {
			output.Info(fmt.Sprintf("PR already exists: #%d", result.Number))
		} else {
			output.Success(fmt.Sprintf("Created PR #%d", result.Number))
		}
		fmt.Printf("  %s -> %s\n", result.Branch, result.Base)
		fmt.Printf("  %s\n", result.URL)

		return nil
	},
}

func init() {
	prCmd.Flags().StringP("title", "t", "", "PR title")
	prCmd.Flags().StringP("body", "b", "", "PR body")
	prCmd.Flags().String("base", "", "Base branch (override auto-detection)")
	prCmd.Flags().BoolP("draft", "d", false, "Create as draft PR")
	prCmd.Flags().Bool("no-push", false, "Skip push if already pushed")
}

// cdCmd: wt cd [branch]
var cdCmd = &cobra.Command{
	Use:   "cd [branch]",
	Short: "Navigate to worktree",
	Long: `Cd navigates to a worktree directory.

Requires shell integration (eval "$(wt shellenv)") to change the
shell's working directory. Without it, just prints the path.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := getManager()
		if err != nil {
			return err
		}

		var branch string
		if len(args) > 0 {
			branch = args[0]
		}

		path, err := m.GetWorktreePath(branch)
		if err != nil {
			wt.DefaultOutput().Error(fmt.Sprintf("Worktree %s not found", branch))
			return err
		}

		fmt.Printf("__WT_CD__:%s\n", path)
		return nil
	},
}

// goalCmd: wt goal [goal-text]
var goalCmd = &cobra.Command{
	Use:   "goal [goal-text]",
	Short: "View or set worktree goal",
	Long: `View or set the high-level goal for the current worktree.

Without arguments, shows the current goal.
With an argument, sets the goal.

Examples:
  wt goal                           # Show current goal
  wt goal "Implement OAuth login"   # Set goal`,
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := getManager()
		if err != nil {
			return err
		}

		ctx := context.Background()
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}

		// Get current branch using git directly
		git := &wt.DefaultGitRunner{}
		result, err := git.Run(ctx, []string{"branch", "--show-current"}, cwd)
		if err != nil {
			return fmt.Errorf("not in a git worktree: %w", err)
		}
		branch := strings.TrimSpace(result.Stdout)
		if branch == "" {
			return fmt.Errorf("not on a branch (detached HEAD?)")
		}

		output := wt.DefaultOutput()

		if len(args) == 0 {
			// Show current goal
			goal, _ := m.GetGoal(ctx, branch, cwd)
			if goal == "" {
				output.Info("No goal set for this worktree")
			} else {
				fmt.Println(goal)
			}
			return nil
		}

		// Set goal
		goal := strings.Join(args, " ")
		if err := m.SetGoal(ctx, branch, goal, cwd); err != nil {
			return fmt.Errorf("failed to set goal: %w", err)
		}
		output.Success(fmt.Sprintf("Goal set for %s", branch))
		return nil
	},
}

// pruneCmd: wt prune [--dry-run]
var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Clean stale metadata",
	Long: `Prune removes stale worktree metadata for directories that no longer exist.

Rough commands:
  git worktree prune`,
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := getManager()
		if err != nil {
			return err
		}

		dryRun, _ := cmd.Flags().GetBool("dry-run")
		ctx := context.Background()

		pruned, err := m.Prune(ctx, dryRun)
		if err != nil {
			return err
		}

		for _, line := range pruned {
			fmt.Println(line)
		}

		return nil
	},
}

func init() {
	pruneCmd.Flags().BoolP("dry-run", "n", false, "Show what would be removed")
}

// shellenvCmd: wt shellenv
var shellenvCmd = &cobra.Command{
	Use:   "shellenv",
	Short: "Print shell integration",
	Long: `Shellenv prints shell functions for directory navigation.

Add to ~/.bashrc or ~/.zshrc:
  eval "$(wt shellenv)"

This wraps the wt command to handle directory changes from
init, new, open, and cd commands.`,
	Run: func(cmd *cobra.Command, args []string) {
		shell := os.Getenv("SHELL")
		if strings.Contains(shell, "zsh") {
			fmt.Print(zshScript)
		} else {
			fmt.Print(bashScript)
		}
	},
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

const bashScript = `# wt shell integration for bash
# Add to ~/.bashrc: eval "$(wt shellenv)"

wt() {
    case "$1" in
        cd|init|new|open|merge)
            local output exit_code
            output=$(command wt "$@")
            exit_code=$?
            echo "$output"
            if [[ "$output" =~ __WT_CD__:([^[:space:]]+) ]]; then
                cd "${BASH_REMATCH[1]}" || return 1
            fi
            return $exit_code
            ;;
        *)
            command wt "$@"
            ;;
    esac
}

_wt_completions() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    local prev="${COMP_WORDS[COMP_CWORD-1]}"
    case "$prev" in
        --repo|-R) COMPREPLY=($(compgen -W "$(ls ~/worktrees 2>/dev/null)" -- "$cur")) ;;
        wt) COMPREPLY=($(compgen -W "--repo -R init new open ls rm status sync merge pr cd prune shellenv" -- "$cur")) ;;
        rm|cd|open) COMPREPLY=($(compgen -W "$(command wt ls --json 2>/dev/null | grep -o '"branch": "[^"]*"' | cut -d'"' -f4)" -- "$cur")) ;;
    esac
}
complete -F _wt_completions wt
`

const zshScript = `# wt shell integration for zsh
# Add to ~/.zshrc: eval "$(wt shellenv)"

wt() {
    case "$1" in
        cd|init|new|open|merge)
            local output exit_code
            output=$(command wt "$@")
            exit_code=$?
            echo "$output"
            if [[ "$output" =~ '__WT_CD__:([^[:space:]]+)' ]]; then
                cd "${match[1]}" || return 1
            fi
            return $exit_code
            ;;
        *)
            command wt "$@"
            ;;
    esac
}

_wt_completions() {
    local -a commands=(
        'init:Initialize repo with bare clone'
        'new:Create new branch worktree'
        'open:Open existing remote branch'
        'ls:List all worktrees'
        'rm:Remove worktree'
        'status:Show status dashboard'
        'sync:Sync all worktrees'
        'merge:Merge PR and cleanup'
        'pr:Push and create GitHub PR'
        'cd:Navigate to worktree'
        'prune:Clean stale metadata'
        'shellenv:Print shell integration'
    )
    local -a repos=($(ls ~/worktrees 2>/dev/null))
    _arguments \
        '--repo[Target repository]:repository:($repos)' \
        '-R[Target repository]:repository:($repos)' \
        '1: :->cmd' \
        '*: :->args'
    case "$state" in
        cmd) _describe 'command' commands ;;
        args)
            case "${words[2]}" in
                rm|cd|open) _values 'worktree' $(command wt ls --json 2>/dev/null | grep -o '"branch": "[^"]*"' | cut -d'"' -f4) ;;
            esac ;;
    esac
}
compdef _wt_completions wt
`
