// Package main provides the TUI application entry point.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/session"
)

var (
	repoFlag   string
	editorFlag string
)

var rootCmd = &cobra.Command{
	Use:   "bramble",
	Short: "TUI for managing worktrees and AI sessions",
	Long: `A terminal UI that combines worktree management (wt) with AI planning
and building sessions (yoloswe). Allows managing multiple parallel sessions
per worktree.

One TUI session operates on a single repo at a time. The repo can be:
  - Auto-detected from current directory (if inside a wt-managed repo)
  - Specified via --repo flag
  - Selected from a menu at startup (if not specified)

Environment:
  WT_ROOT     Base directory for worktrees (default: ~/worktrees)
  EDITOR      Editor command for [e]dit (default: code)`,
	RunE: runTUI,
}

func init() {
	rootCmd.Flags().StringVar(&repoFlag, "repo", "", "Repository name to open directly")
	rootCmd.Flags().StringVar(&editorFlag, "editor", "", "Editor command for [e]dit (default: $EDITOR or 'code')")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runTUI(cmd *cobra.Command, args []string) error {
	// Setup context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	// Get WT_ROOT (same as wt command)
	wtRoot := os.Getenv("WT_ROOT")
	if wtRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home directory: %w", err)
		}
		wtRoot = filepath.Join(home, "worktrees")
	}

	// Determine repo to use (priority: --repo flag > auto-detect from cwd > picker)
	repoName := repoFlag
	if repoName == "" {
		// Try to detect current repo from cwd
		if cwd, err := os.Getwd(); err == nil {
			if repo, err := detectRepoFromPath(cwd, wtRoot); err == nil {
				repoName = repo
			}
		}
	}

	// If no repo specified, show the repo picker
	if repoName == "" {
		selectedRepo, err := runRepoPicker(wtRoot)
		if err != nil {
			return err
		}
		if selectedRepo == "" {
			return nil // User quit
		}
		repoName = selectedRepo
	}

	// Verify the repo exists
	repoPath := filepath.Join(wtRoot, repoName)
	if _, err := os.Stat(filepath.Join(repoPath, ".bare")); err != nil {
		return fmt.Errorf("repository not found: %s (expected at %s)", repoName, repoPath)
	}

	// Initialize session store and manager
	store, err := session.NewStore("")
	if err != nil {
		return fmt.Errorf("failed to create session store: %w", err)
	}

	sessionManager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName: repoName,
		Store:    store,
	})
	defer sessionManager.Close()

	// Determine editor command (priority: --editor flag > $EDITOR env > "code")
	editor := editorFlag
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}

	// Create and run TUI
	model := app.NewModel(ctx, wtRoot, repoName, editor, sessionManager)
	p := tea.NewProgram(model, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	return nil
}

// runRepoPicker shows the repo selection screen and returns the selected repo.
func runRepoPicker(wtRoot string) (string, error) {
	picker := app.NewRepoPickerModel(wtRoot)
	p := tea.NewProgram(picker, tea.WithAltScreen())

	finalModel, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("repo picker error: %w", err)
	}

	// Check if a repo was selected
	if msg, ok := finalModel.(app.RepoPickerModel); ok {
		return msg.SelectedRepo(), nil
	}

	return "", nil
}

// detectRepoFromPath finds the repo name if cwd is within a wt-managed repo.
func detectRepoFromPath(cwd, wtRoot string) (string, error) {
	// Walk up to find .bare directory (indicating wt-managed repo)
	dir := cwd
	for {
		// Check if parent has .bare
		parent := filepath.Dir(dir)
		bareDir := filepath.Join(parent, ".bare")
		if fi, err := os.Stat(bareDir); err == nil && fi.IsDir() {
			// Found it - parent is the repo root
			repoName := filepath.Base(parent)
			repoWtRoot := filepath.Dir(parent)
			if repoWtRoot == wtRoot {
				return repoName, nil
			}
		}

		// Check if current dir has .bare (we're at repo root)
		bareDir = filepath.Join(dir, ".bare")
		if fi, err := os.Stat(bareDir); err == nil && fi.IsDir() {
			repoName := filepath.Base(dir)
			repoWtRoot := filepath.Dir(dir)
			if repoWtRoot == wtRoot {
				return repoName, nil
			}
		}

		if parent == dir {
			// Reached filesystem root
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("not in a wt-managed repo")
}
