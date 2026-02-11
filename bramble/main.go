// Package main provides the TUI application entry point.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/remote"
	pb "github.com/bazelment/yoloswe/bramble/remote/proto"
	"github.com/bazelment/yoloswe/bramble/service"
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt/taskrouter"
)

var (
	repoFlag        string
	editorFlag      string
	sessionModeFlag string
	yoloFlag        bool
	remoteFlag      string
	tokenFlag       string
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

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the Bramble gRPC server",
	Long: `Start a headless Bramble server that manages sessions, worktrees, and task
routing. TUI clients connect via --remote flag.

Multiple clients can connect simultaneously and see the same state.`,
	RunE: runServe,
}

var (
	serveAddrFlag string
	servePortFlag int
)

func init() {
	rootCmd.Flags().StringVar(&repoFlag, "repo", "", "Repository name to open directly")
	rootCmd.Flags().StringVar(&editorFlag, "editor", "", "Editor command for [e]dit (default: $EDITOR or 'code')")
	rootCmd.Flags().StringVar(&sessionModeFlag, "session-mode", "auto", "Session execution mode: auto (default), tui, or tmux")
	rootCmd.Flags().BoolVar(&yoloFlag, "yolo", false, "Skip all permission prompts (dangerous!)")
	rootCmd.Flags().StringVar(&remoteFlag, "remote", "", "Connect to a remote Bramble server (host:port)")
	rootCmd.Flags().StringVar(&tokenFlag, "token", "", "Authentication token for remote server (also: BRAMBLE_TOKEN env)")

	serveCmd.Flags().StringVar(&serveAddrFlag, "addr", "localhost", "Address to listen on (use 0.0.0.0 for all interfaces)")
	serveCmd.Flags().IntVar(&servePortFlag, "port", 9090, "Port to listen on")
	serveCmd.Flags().StringVar(&repoFlag, "repo", "", "Repository name")
	serveCmd.Flags().BoolVar(&yoloFlag, "yolo", false, "Skip all permission prompts (dangerous!)")
	serveCmd.Flags().StringVar(&sessionModeFlag, "session-mode", "auto", "Session execution mode: auto (default), tui, or tmux")
	serveCmd.Flags().StringVar(&tokenFlag, "token", "", "Authentication token (generated if not provided, also: BRAMBLE_TOKEN env)")

	rootCmd.AddCommand(serveCmd)
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

	// Remote mode: connect to a Bramble server
	if remoteFlag != "" {
		return runRemoteTUI(ctx, remoteFlag)
	}

	// Local mode
	return runLocalTUI(ctx)
}

func runRemoteTUI(ctx context.Context, addr string) error {
	// Resolve token: flag > env
	token := tokenFlag
	if token == "" {
		token = os.Getenv("BRAMBLE_TOKEN")
	}

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(remote.TokenCallCredentials(token)))
	}

	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", addr, err)
	}
	defer conn.Close()

	sessionSvc := remote.NewSessionProxy(ctx, conn)
	defer sessionSvc.Close()
	wtSvc := remote.NewWorktreeProxy(conn)
	taskRouterSvc := remote.NewTaskRouterProxy(conn)

	// Pre-load worktrees from the server
	worktrees, _ := wtSvc.List(ctx)

	// Determine editor command (priority: --editor flag > $EDITOR env > "code")
	editor := editorFlag
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}

	// Query terminal size
	termWidth, termHeight, _ := term.GetSize(int(os.Stdout.Fd()))

	// In remote mode, wtRoot and repoName are informational only (server manages them).
	model := app.NewModel(ctx, "", "remote", editor, sessionSvc, wtSvc, taskRouterSvc, worktrees, termWidth, termHeight)
	p := tea.NewProgram(model, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	return nil
}

func runLocalTUI(ctx context.Context) error {
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

	// Initialize session store and manager
	store, err := session.NewStore("")
	if err != nil {
		return fmt.Errorf("failed to create session store: %w", err)
	}

	sessionManager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName:    repoName,
		Store:       store,
		SessionMode: session.SessionMode(sessionModeFlag),
		YoloMode:    yoloFlag,
	})
	defer sessionManager.Close()

	// Determine editor command (priority: --editor flag > $EDITOR env > "code")
	editor := editorFlag
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}

	// Create local worktree service
	wtSvc := service.NewLocalWorktreeService(wtRoot, repoName)

	// Pre-load worktrees synchronously so the first render shows branch names
	// instead of flashing an empty UI while waiting for the git subprocess.
	worktrees, _ := wtSvc.List(ctx)

	// Start the AI task router (non-fatal if it fails — routeTask falls back to heuristic)
	router := taskrouter.New(taskrouter.Config{
		WorkDir: repoPath,
		NoColor: true,
	})
	router.SetOutput(io.Discard)
	var taskRouter *taskrouter.Router
	if err := router.Start(ctx); err != nil {
		log.Printf("Warning: task router failed to start: %v (falling back to heuristic routing)", err)
	} else {
		taskRouter = router
		defer router.Stop()
	}
	taskRouterSvc := service.NewLocalTaskRouterService(taskRouter)

	// Query terminal size synchronously so the first View() renders a
	// properly laid-out UI instead of waiting for the async WindowSizeMsg.
	termWidth, termHeight, _ := term.GetSize(int(os.Stdout.Fd()))

	// Create and run TUI
	model := app.NewModel(ctx, wtRoot, repoName, editor, sessionManager, wtSvc, taskRouterSvc, worktrees, termWidth, termHeight)
	p := tea.NewProgram(model, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	return nil
}

func runServe(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Shutting down...")
		cancel()
	}()

	// Get WT_ROOT
	wtRoot := os.Getenv("WT_ROOT")
	if wtRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home directory: %w", err)
		}
		wtRoot = filepath.Join(home, "worktrees")
	}

	// Resolve repo name
	repoName := repoFlag
	if repoName == "" {
		if cwd, err := os.Getwd(); err == nil {
			if repo, err := detectRepoFromPath(cwd, wtRoot); err == nil {
				repoName = repo
			}
		}
	}
	if repoName == "" {
		return fmt.Errorf("--repo flag is required for serve mode (or run from within a wt-managed repo)")
	}

	repoPath := filepath.Join(wtRoot, repoName)
	if _, err := os.Stat(filepath.Join(repoPath, ".bare")); err != nil {
		return fmt.Errorf("repository not found: %s (expected at %s)", repoName, repoPath)
	}

	// Create session manager
	store, err := session.NewStore("")
	if err != nil {
		return fmt.Errorf("failed to create session store: %w", err)
	}

	sessionManager := session.NewManagerWithConfig(session.ManagerConfig{
		RepoName:    repoName,
		Store:       store,
		SessionMode: session.SessionMode(sessionModeFlag),
		YoloMode:    yoloFlag,
	})
	defer sessionManager.Close()

	// Create worktree service
	wtSvc := service.NewLocalWorktreeService(wtRoot, repoName)

	// Create task router
	router := taskrouter.New(taskrouter.Config{
		WorkDir: repoPath,
		NoColor: true,
	})
	router.SetOutput(io.Discard)
	var taskRouter *taskrouter.Router
	if err := router.Start(ctx); err != nil {
		log.Printf("Warning: task router failed to start: %v", err)
	} else {
		taskRouter = router
		defer router.Stop()
	}
	taskRouterSvc := service.NewLocalTaskRouterService(taskRouter)

	// Create event broadcaster
	broadcaster := remote.NewEventBroadcaster()
	go broadcaster.Run(ctx, sessionManager.Events())

	// Resolve auth token: flag > env > generate
	token := tokenFlag
	if token == "" {
		token = os.Getenv("BRAMBLE_TOKEN")
	}
	if token == "" {
		generated, err := remote.GenerateToken()
		if err != nil {
			return fmt.Errorf("failed to generate auth token: %w", err)
		}
		token = generated
	}

	// Create gRPC server with auth interceptors
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(remote.TokenAuthInterceptor(token)),
		grpc.StreamInterceptor(remote.TokenStreamInterceptor(token)),
	)
	pb.RegisterBrambleSessionServiceServer(srv, remote.NewSessionServer(sessionManager, broadcaster))
	pb.RegisterBrambleWorktreeServiceServer(srv, remote.NewWorktreeServer(wtSvc))
	pb.RegisterBrambleTaskRouterServiceServer(srv, remote.NewTaskRouterServer(taskRouterSvc))

	listenAddr := fmt.Sprintf("%s:%d", serveAddrFlag, servePortFlag)
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", listenAddr, err)
	}

	log.Printf("Bramble server listening on %s (repo: %s)", listenAddr, repoName)
	if serveAddrFlag != "localhost" && serveAddrFlag != "127.0.0.1" {
		log.Printf("WARNING: listening on %s without TLS — auth tokens are sent in plaintext", serveAddrFlag)
	}
	fmt.Printf("Auth token: %s\n", token)

	// Graceful shutdown
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()

	if err := srv.Serve(lis); err != nil {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

// runRepoPicker shows the repo selection screen and returns the selected repo.
func runRepoPicker(ctx context.Context, wtRoot string) (string, error) {
	picker := app.NewRepoPickerModel(ctx, wtRoot)
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
