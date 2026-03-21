// Package main provides the TUI application entry point.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/cmd/delegator"
	"github.com/bazelment/yoloswe/bramble/ipc"
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/taskrouter"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
	"github.com/bazelment/yoloswe/yoloswe"
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
	wtRoot, err := resolveWTRoot()
	if err != nil {
		return err
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

	// Start IPC server so child processes can request new sessions.
	// The registry aggregates all repo managers so IPC handlers can find
	// sessions from any repo, including those opened later via Alt-R.
	registry := session.NewSessionRegistry()
	registry.Register(sessionManager)
	sharedManagerConfig.Registry = registry
	ipcServer, ipcSockPath := startIPCServer(registry, wtRoot, repoName)
	if ipcServer != nil {
		defer ipcServer.Close()
		os.Setenv(ipc.SockEnvVar, ipcSockPath)
		// Propagate the socket path to the session manager (and the shared
		// config template used when opening additional repos) so that tmux
		// windows receive BRAMBLE_SOCK without relying on os.Getenv.
		sessionManager.SetIPCSockPath(ipcSockPath)
		sharedManagerConfig.IPCSockPath = ipcSockPath
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

// resolveWTRoot returns the worktree root directory from $WT_ROOT or ~/worktrees.
func resolveWTRoot() (string, error) {
	if v := os.Getenv("WT_ROOT"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, "worktrees"), nil
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

// --- IPC server setup --------------------------------------------------------

func startIPCServer(registry *session.SessionRegistry, wtRoot, repoName string) (*ipc.Server, string) {
	// Prefer $XDG_RUNTIME_DIR (user-private, tmpfs) over /tmp to avoid
	// symlink/TOCTOU risks in world-writable directories.
	runDir := os.Getenv("XDG_RUNTIME_DIR")
	if runDir == "" {
		runDir = os.TempDir()
	}
	sockPath := filepath.Join(runDir, fmt.Sprintf("bramble-%d.sock", os.Getpid()))
	srv := ipc.NewServer(sockPath)

	srv.Handle(ipc.RequestPing, func(_ context.Context, _ *ipc.Request) (any, error) {
		return "pong", nil
	})

	srv.Handle(ipc.RequestNewSession, func(ctx context.Context, req *ipc.Request) (any, error) {
		params, ok := req.Params.(*ipc.NewSessionParams)
		if !ok {
			return nil, fmt.Errorf("invalid params")
		}

		targetRepo := params.RepoName
		if targetRepo == "" {
			targetRepo = repoName // fall back to initial repo
		}

		mgr, ok := registry.FindManagerByRepo(targetRepo)
		if !ok {
			return nil, fmt.Errorf("repo %q is not open in bramble; open it with Alt-R first", targetRepo)
		}

		return handleNewSession(ctx, mgr, wtRoot, targetRepo, params)
	})

	srv.Handle(ipc.RequestListSessions, func(_ context.Context, _ *ipc.Request) (any, error) {
		return handleListSessions(registry), nil
	})

	srv.Handle(ipc.RequestCapturePane, func(_ context.Context, req *ipc.Request) (any, error) {
		params, ok := req.Params.(*ipc.CapturePaneParams)
		if !ok {
			return nil, fmt.Errorf("invalid params")
		}
		sid := session.SessionID(params.SessionID)
		n := params.Lines
		if n <= 0 {
			n = 10
		}
		lines, err := registry.CapturePaneText(sid, n)
		if err != nil {
			return nil, err
		}
		return &ipc.CapturePaneResult{Lines: lines}, nil
	})

	srv.Handle(ipc.RequestNotify, func(_ context.Context, req *ipc.Request) (any, error) {
		params, ok := req.Params.(*ipc.NotifyParams)
		if !ok {
			return nil, fmt.Errorf("invalid params")
		}
		sid := session.SessionID(params.SessionID)
		info, _, ok := registry.GetSessionInfo(sid)
		if !ok {
			return nil, fmt.Errorf("session not found: %s", params.SessionID)
		}
		windowTarget := info.TmuxWindowID
		if windowTarget == "" {
			windowTarget = info.TmuxWindowName
		}
		if windowTarget != "" && info.TmuxWindowName != "" {
			// Skip visual notification if user is already viewing this window —
			// the alerts are designed for background sessions, not the active one.
			if !session.IsSessionWindowActive(info.TmuxWindowID, info.TmuxWindowName) {
				session.NotifyTmuxWindow(windowTarget, info.TmuxWindowName)
			}
		}
		registry.SetSessionIdle(sid)
		return "ok", nil
	})

	if err := srv.Start(); err != nil {
		log.Printf("Warning: IPC server failed to start: %v", err)
		return nil, ""
	}
	socketPath := srv.SocketPath()
	return srv, socketPath
}

func handleNewSession(ctx context.Context, mgr *session.Manager, wtRoot, repoName string, params *ipc.NewSessionParams) (*ipc.NewSessionResult, error) {
	worktreePath := params.WorktreePath

	// Create worktree if requested
	if params.CreateWorktree && params.Branch != "" {
		m := wt.NewManager(wtRoot, repoName)
		path, err := m.New(ctx, params.Branch, params.BaseBranch, params.Goal)
		if err != nil {
			return nil, fmt.Errorf("failed to create worktree: %w", err)
		}
		worktreePath = path
	}

	if worktreePath == "" {
		return nil, fmt.Errorf("either worktree_path or branch with create_worktree is required")
	}

	var sessionType session.SessionType
	switch params.SessionType {
	case "planner", "":
		sessionType = session.SessionTypePlanner
	case "builder":
		sessionType = session.SessionTypeBuilder
	case "codetalk":
		sessionType = session.SessionTypeCodeTalk
	default:
		return nil, fmt.Errorf("unknown session_type %q (expected \"planner\", \"builder\", or \"codetalk\")", params.SessionType)
	}

	id, err := mgr.StartSession(sessionType, worktreePath, params.Prompt, params.Model)
	if err != nil {
		return nil, fmt.Errorf("failed to start session: %w", err)
	}

	return &ipc.NewSessionResult{
		SessionID:    string(id),
		WorktreePath: worktreePath,
	}, nil
}

func handleListSessions(registry *session.SessionRegistry) *ipc.ListSessionsResult {
	sessions := registry.GetAllSessions()
	summaries := make([]ipc.SessionSummary, len(sessions))
	for i := range sessions {
		s := &sessions[i]
		summaries[i] = ipc.SessionSummary{
			ID:           string(s.ID),
			Type:         string(s.Type),
			Status:       string(s.Status),
			WorktreeName: s.WorktreeName,
			Prompt:       s.Prompt,
			Model:        s.Model,
		}
	}
	return &ipc.ListSessionsResult{Sessions: summaries}
}

// --- CLI subcommands (client mode) -------------------------------------------

var pingCmd = &cobra.Command{
	Use:   "ping",
	Short: "Check if the bramble TUI server is alive",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := ipc.NewClientFromEnv()
		if err != nil {
			return err
		}
		if err := client.Ping(); err != nil {
			return err
		}
		fmt.Println("pong")
		return nil
	},
}

var newSessionCmd = &cobra.Command{
	Use:   "new-session",
	Short: "Request the running bramble TUI to create a new session",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := ipc.NewClientFromEnv()
		if err != nil {
			return err
		}

		sessionType, _ := cmd.Flags().GetString("type")
		branch, _ := cmd.Flags().GetString("branch")
		baseBranch, _ := cmd.Flags().GetString("from")
		worktreePath, _ := cmd.Flags().GetString("worktree")
		prompt, _ := cmd.Flags().GetString("prompt")
		model, _ := cmd.Flags().GetString("model")
		goal, _ := cmd.Flags().GetString("goal")
		createWT, _ := cmd.Flags().GetBool("create-worktree")
		repo, _ := cmd.Flags().GetString("repo")

		// Auto-detect repo from cwd if not explicitly specified.
		if repo == "" {
			if wtRoot, err := resolveWTRoot(); err == nil {
				cwd, _ := os.Getwd()
				repo, _ = detectRepoFromPath(cwd, wtRoot)
			}
		}

		resp, err := client.Send(&ipc.Request{
			Type: ipc.RequestNewSession,
			ID:   "cli-new-session",
			Params: &ipc.NewSessionParams{
				SessionType:    sessionType,
				Branch:         branch,
				BaseBranch:     baseBranch,
				WorktreePath:   worktreePath,
				CreateWorktree: createWT,
				Prompt:         prompt,
				Model:          model,
				Goal:           goal,
				RepoName:       repo,
			},
		})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("server error: %s", resp.Error)
		}

		out, _ := json.MarshalIndent(resp.Result, "", "  ")
		fmt.Println(string(out))
		return nil
	},
}

var notifyCmd = &cobra.Command{
	Use:   "notify",
	Short: "Notify bramble that a session needs attention",
	RunE: func(cmd *cobra.Command, args []string) error {
		// When triggered by Claude's stop hook (--silent), errors are
		// non-actionable (socket gone, session cleaned up, etc.), so
		// suppress them to avoid noisy stderr in the Claude session.
		silent, _ := cmd.Flags().GetBool("silent")

		client, err := ipc.NewClientFromEnv()
		if err != nil {
			if silent {
				return nil
			}
			return err
		}
		sessionID, _ := cmd.Flags().GetString("session-id")
		resp, err := client.Send(&ipc.Request{
			Type:   ipc.RequestNotify,
			ID:     "cli-notify",
			Params: &ipc.NotifyParams{SessionID: sessionID},
		})
		if err != nil {
			if silent {
				return nil
			}
			return err
		}
		if !resp.OK {
			if silent {
				return nil
			}
			return fmt.Errorf("server error: %s", resp.Error)
		}
		return nil
	},
}

var capturePaneCmd = &cobra.Command{
	Use:   "capture-pane",
	Short: "Capture text from a tmux session's pane",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := ipc.NewClientFromEnv()
		if err != nil {
			return err
		}
		sessionID, _ := cmd.Flags().GetString("session-id")
		lines, _ := cmd.Flags().GetInt("lines")
		resp, err := client.Send(&ipc.Request{
			Type: ipc.RequestCapturePane,
			ID:   "cli-capture-pane",
			Params: &ipc.CapturePaneParams{
				SessionID: sessionID,
				Lines:     lines,
			},
		})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("server error: %s", resp.Error)
		}
		out, _ := json.MarshalIndent(resp.Result, "", "  ")
		fmt.Println(string(out))
		return nil
	},
}

var listSessionsCmd = &cobra.Command{
	Use:   "list-sessions",
	Short: "List active sessions from the running bramble TUI",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := ipc.NewClientFromEnv()
		if err != nil {
			return err
		}

		resp, err := client.Send(&ipc.Request{
			Type: ipc.RequestListSessions,
			ID:   "cli-list-sessions",
		})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("server error: %s", resp.Error)
		}

		out, _ := json.MarshalIndent(resp.Result, "", "  ")
		fmt.Println(string(out))
		return nil
	},
}

var codetalkCmd = &cobra.Command{
	Use:   "codetalk [flags] <prompt>",
	Short: "Start a code understanding session",
	Long: `CodeTalk deeply explores a codebase area and provides structured analysis.
After the initial exploration, it accepts follow-up questions interactively.`,
	Example: `  bramble codetalk "the happy path of search handling"
  bramble codetalk --model opus "how does auth middleware work"
  bramble codetalk --dir /path/to/repo "explain the session lifecycle"`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		prompt := strings.Join(args, " ")
		if prompt == "" {
			return fmt.Errorf("prompt is required")
		}

		model, _ := cmd.Flags().GetString("model")
		workDir, _ := cmd.Flags().GetString("dir")
		recordDir, _ := cmd.Flags().GetString("record")
		systemPrompt, _ := cmd.Flags().GetString("system")
		verbose, _ := cmd.Flags().GetBool("verbose")

		if workDir == "" {
			var err error
			workDir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get working directory: %w", err)
			}
		}

		ct := yoloswe.NewCodeTalkSession(yoloswe.CodeTalkConfig{
			Model:        model,
			WorkDir:      workDir,
			RecordingDir: recordDir,
			SystemPrompt: systemPrompt,
			Verbose:      verbose,
		}, os.Stdout)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			fmt.Fprintln(os.Stderr, "\nInterrupted, shutting down...")
			cancel()
		}()

		if err := ct.Start(ctx); err != nil {
			return fmt.Errorf("failed to start session: %w", err)
		}
		defer ct.Stop()

		// Initial exploration
		if _, err := ct.RunTurn(ctx, prompt); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		// Interactive follow-up loop
		scanner := bufio.NewScanner(os.Stdin)
		for {
			fmt.Fprint(os.Stderr, "\n> ")
			if !scanner.Scan() {
				break
			}
			input := strings.TrimSpace(scanner.Text())
			if input == "" {
				continue
			}
			if _, err := ct.RunTurn(ctx, input); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			}
		}

		if path := ct.RecordingPath(); path != "" {
			fmt.Fprintf(os.Stderr, "\nSession recorded to: %s\n", path)
		}
		return nil
	},
}

func init() {
	newSessionCmd.Flags().StringP("type", "t", "planner", "Session type: planner or builder")
	newSessionCmd.Flags().StringP("branch", "b", "", "Branch name (creates worktree if --create-worktree)")
	newSessionCmd.Flags().StringP("from", "f", "", "Base branch for new worktree")
	newSessionCmd.Flags().StringP("worktree", "w", "", "Existing worktree path")
	newSessionCmd.Flags().StringP("prompt", "p", "", "Prompt for the session")
	newSessionCmd.Flags().StringP("model", "m", "", "Model ID (e.g. opus, sonnet)")
	newSessionCmd.Flags().StringP("goal", "g", "", "Goal for new worktree")
	newSessionCmd.Flags().Bool("create-worktree", false, "Create a new worktree for the branch")
	newSessionCmd.Flags().StringP("repo", "r", "", "Target repo name (auto-detected from cwd if omitted)")

	notifyCmd.Flags().String("session-id", "", "Session ID to notify")
	notifyCmd.Flags().Bool("silent", false, "Suppress errors silently (used by stop hooks)")
	_ = notifyCmd.MarkFlagRequired("session-id")

	capturePaneCmd.Flags().String("session-id", "", "Session ID to capture pane from")
	capturePaneCmd.Flags().Int("lines", 10, "Number of lines to capture")
	_ = capturePaneCmd.MarkFlagRequired("session-id")

	codetalkCmd.Flags().StringP("model", "m", "opus", "Model to use (e.g. opus, sonnet)")
	codetalkCmd.Flags().String("dir", "", "Working directory (defaults to current directory)")
	codetalkCmd.Flags().String("record", "", "Directory for session recordings (defaults to ~/.yoloswe)")
	codetalkCmd.Flags().String("system", "", "Custom system prompt")
	codetalkCmd.Flags().BoolP("verbose", "v", false, "Show detailed tool results")

	rootCmd.AddCommand(pingCmd)
	rootCmd.AddCommand(newSessionCmd)
	rootCmd.AddCommand(listSessionsCmd)
	rootCmd.AddCommand(notifyCmd)
	rootCmd.AddCommand(capturePaneCmd)
	rootCmd.AddCommand(delegator.Cmd)
	rootCmd.AddCommand(codetalkCmd)
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
