// Package main provides the TUI application entry point.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/taskrouter"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

var (
	repoFlag        string
	editorFlag      string
	sessionModeFlag string
	tmuxExitOnQuit  bool
	protocolLogDir  string
	yoloFlag        bool
)

var rootCmd = &cobra.Command{
	Use:   "bramble",
	Short: "TUI for managing worktrees and AI sessions",
	Long: `A terminal UI that combines worktree management (wt) with AI planning
and building sessions (yoloswe). Allows managing multiple parallel sessions
per worktree.

The initial repo is chosen at startup via:
  - Auto-detected from current directory (if inside a wt-managed repo)
  - Specified via --repo flag
  - Selected from a menu at startup (if not specified)

Additional repos can be opened mid-session with Alt-R. All sessions across
all opened repos are visible in the Shift-S overlay.

Environment:
  WT_ROOT     Base directory for worktrees (default: ~/worktrees)
  EDITOR      Editor command for [e]dit (default: code)
  BRAMBLE_PROTOCOL_LOG_DIR  Directory for Codex/Gemini protocol logs`,
	RunE: runTUI,
}

func init() {
	rootCmd.Flags().StringVar(&repoFlag, "repo", "", "Repository name to open directly")
	rootCmd.Flags().StringVar(&editorFlag, "editor", "", "Editor command for [e]dit (default: $EDITOR or 'code')")
	rootCmd.Flags().StringVar(&sessionModeFlag, "session-mode", "auto", "Session execution mode: auto (default), tui, or tmux")
	rootCmd.Flags().BoolVar(&tmuxExitOnQuit, "tmux-exit-on-quit", false, "Kill Bramble-created tmux windows when quitting Bramble")
	rootCmd.Flags().StringVar(&protocolLogDir, "protocol-log-dir", "", "Directory for provider protocol/stderr logs (optional; also supports $BRAMBLE_PROTOCOL_LOG_DIR)")
	rootCmd.Flags().BoolVar(&yoloFlag, "yolo", false, "Skip all permission prompts (dangerous!)")
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
		selectedRepo, err := runRepoPicker(ctx, wtRoot)
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

	// Initialize session store
	store, err := session.NewStore("")
	if err != nil {
		return fmt.Errorf("failed to create session store: %w", err)
	}

	// Determine editor command (priority: --editor flag > $EDITOR env > "code")
	editor := editorFlag
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}

	// Pre-load worktrees synchronously so the first render shows branch names
	// instead of flashing an empty UI while waiting for the git subprocess.
	manager := wt.NewManager(wtRoot, repoName)
	worktrees, _ := manager.List(ctx)

	// Probe which provider CLIs are installed
	providerAvailability := agent.NewProviderAvailability()

	// Load settings and build filtered model registry
	settings := app.LoadSettings()
	modelRegistry := agent.NewModelRegistry(providerAvailability, settings.GetEnabledProviders())

	// Build a shared manager config template (minus RepoName) so the TUI
	// can create new managers when opening additional repos mid-session.
	sharedManagerConfig := session.ManagerConfig{
		Store:          store,
		SessionMode:    session.SessionMode(sessionModeFlag),
		TmuxExitOnQuit: tmuxExitOnQuit,
		YoloMode:       yoloFlag,
		ModelRegistry:  modelRegistry,
		ProtocolLogDir: func() string {
			if protocolLogDir != "" {
				return protocolLogDir
			}
			return os.Getenv("BRAMBLE_PROTOCOL_LOG_DIR")
		}(),
	}

	// Initialize session manager for the initial repo.
	initialConfig := sharedManagerConfig
	initialConfig.RepoName = repoName
	sessionManager := session.NewManagerWithConfig(initialConfig)
	defer sessionManager.Close()

	// Reconcile previously-running tmux sessions against live tmux windows.
	if err := sessionManager.ReconcileTmuxSessions(); err != nil {
		log.Printf("Warning: tmux session reconciliation failed: %v", err)
	}

	// Discover repos (other than the initial one) that have live tmux sessions.
	// Dead sessions from those repos are cleaned up; live ones will be auto-opened
	// by the TUI so their sessions are fully re-adopted.
	resumeRepos := session.ReposWithLiveTmuxSessions(store, repoName)

	// Start the AI task router using the best available provider.
	// Priority: codex (original default) → claude → gemini.
	var taskRouter *taskrouter.Router
	routerProvider := pickRouterProvider(providerAvailability, settings.GetEnabledProviders())
	if routerProvider != nil {
		router := taskrouter.New(taskrouter.Config{
			Provider: routerProvider,
			WorkDir:  repoPath,
		})
		router.SetOutput(io.Discard)
		if err := router.Start(ctx); err != nil {
			log.Printf("Warning: task router failed to start: %v (falling back to heuristic routing)", err)
		} else {
			taskRouter = router
			defer router.Stop()
		}
	}

	// Query terminal size synchronously so the first View() renders a
	// properly laid-out UI instead of waiting for the async WindowSizeMsg.
	termWidth, termHeight, _ := term.GetSize(int(os.Stdout.Fd()))

	// Create and run TUI
	model := app.NewModel(ctx, wtRoot, repoName, editor, sessionManager, taskRouter, worktrees, termWidth, termHeight, providerAvailability, modelRegistry, sharedManagerConfig, resumeRepos)
	p := tea.NewProgram(model)

	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	// Close secondary repo managers on exit. The initial repo's manager is
	// closed by the defer above. This ensures tmux windows from any additionally
	// opened repos are cleaned up properly.
	if m, ok := finalModel.(app.Model); ok {
		m.CloseSecondaryManagers(repoName)
	}

	return nil
}

// runRepoPicker shows the repo selection screen and returns the selected repo.
func runRepoPicker(ctx context.Context, wtRoot string) (string, error) {
	settings := app.LoadSettings()
	palette := app.Dark
	if p, ok := app.ThemeByName(settings.ThemeName); ok {
		palette = p
	}
	picker := app.NewRepoPickerModel(ctx, wtRoot, app.NewStyles(palette))
	p := tea.NewProgram(picker)

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

// pickRouterProvider selects the best available provider for the task router.
// Prefers codex (original default), then claude, then gemini.
// Returns nil if no suitable provider is installed and enabled.
func pickRouterProvider(availability *agent.ProviderAvailability, enabledProviders []string) agent.Provider {
	enabled := func(name string) bool {
		if enabledProviders == nil {
			return true // nil means all enabled
		}
		for _, p := range enabledProviders {
			if p == name {
				return true
			}
		}
		return false
	}

	// Try codex first (best for routing tasks, original default)
	if availability.IsInstalled(agent.ProviderCodex) && enabled(agent.ProviderCodex) {
		return agent.NewCodexProvider()
	}
	// Fall back to claude
	if availability.IsInstalled(agent.ProviderClaude) && enabled(agent.ProviderClaude) {
		return agent.NewClaudeProvider()
	}
	// Fall back to gemini
	if availability.IsInstalled(agent.ProviderGemini) && enabled(agent.ProviderGemini) {
		return agent.NewGeminiProvider()
	}
	return nil
}
