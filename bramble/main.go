// Package main provides the TUI application entry point.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bazelment/yoloswe/logging/klogfmt"

	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/cmd/codereview"
	"github.com/bazelment/yoloswe/bramble/cmd/delegator"
	"github.com/bazelment/yoloswe/bramble/cmd/meetingbot"
	"github.com/bazelment/yoloswe/bramble/cmd/speak"
	"github.com/bazelment/yoloswe/bramble/control"
	"github.com/bazelment/yoloswe/bramble/ipc"
	"github.com/bazelment/yoloswe/bramble/remote"
	"github.com/bazelment/yoloswe/bramble/selfexec"
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/taskrouter"
	"github.com/bazelment/yoloswe/bramble/tmuxctl"
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
	debugAddr       string
	yoloFlag        bool
	// Voice reporting flags.
	enableVoiceReports bool
	elevenLabsAPIKey   string
	ttsVoice           string
	voiceReportMode    string
	voiceSaveDir       string
)

// Restart plumbing. restart is filled in after the TUI exits asking to be
// replaced; main() acts on it once runTUI's deferred cleanup has finished.
var (
	restart struct {
		statePath string
		pending   bool
	}
	restartRequests restartRequester
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
	rootCmd.AddCommand(meetingbot.Cmd)

	rootCmd.Flags().StringVar(&repoFlag, "repo", "", "Repository name to open directly")
	rootCmd.Flags().StringVar(&editorFlag, "editor", "", "Editor command for [e]dit (default: $EDITOR or 'code')")
	rootCmd.Flags().StringVar(&sessionModeFlag, "session-mode", "auto", "Session execution mode: auto (default), tui, or tmux")
	rootCmd.Flags().BoolVar(&tmuxExitOnQuit, "tmux-exit-on-quit", false, "Kill Bramble-created tmux windows when quitting Bramble")
	rootCmd.Flags().StringVar(&protocolLogDir, "protocol-log-dir", "", "Directory for provider protocol/stderr logs (optional; also supports $BRAMBLE_PROTOCOL_LOG_DIR)")
	rootCmd.Flags().StringVar(&debugAddr, "debug-addr", "", "if set, serve pprof + expvar on this addr (e.g. localhost:6060)")
	rootCmd.Flags().BoolVar(&yoloFlag, "yolo", false, "Skip all permission prompts (dangerous!)")
	rootCmd.Flags().BoolVar(&enableVoiceReports, "enable-voice-reports", false, "Enable voice reporting on session completion (requires ELEVENLABS_API_KEY)")
	rootCmd.Flags().StringVar(&elevenLabsAPIKey, "elevenlabs-api-key", "", "ElevenLabs API key (or set ELEVENLABS_API_KEY env var)")
	rootCmd.Flags().StringVar(&ttsVoice, "tts-voice", "", "ElevenLabs voice ID for TTS synthesis")
	rootCmd.Flags().StringVar(&voiceReportMode, "voice-report-mode", "auto", "Voice report playback mode: auto, direct, file, redirect (local is deprecated alias for direct)")
	rootCmd.Flags().StringVar(&voiceSaveDir, "voice-save-dir", "", "Directory for file-mode voice reports (default: ~/.bramble/voice-reports)")
}

func main() {
	klogfmt.Init()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// The exec happens here, not in runTUI, because syscall.Exec never runs
	// deferred functions: only once Execute returns have runTUI's defers
	// unbound both sockets and persisted every live session.
	if restart.pending {
		env := os.Environ()
		if restart.statePath != "" {
			env = append(env, app.RestartStateEnvVar+"="+restart.statePath)
		}
		if err := selfexec.Exec(os.Args, env); err != nil {
			fmt.Fprintf(os.Stderr, "bramble restart failed: %v\n", err)
			os.Exit(1)
		}
	}
}

func runTUI(cmd *cobra.Command, args []string) error {
	// Setup context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if debugAddr != "" {
		expvar.Publish("goroutines", expvar.Func(func() any { return runtime.NumGoroutine() }))
		go func() {
			if err := http.ListenAndServe(debugAddr, nil); err != nil {
				slog.Warn("debug server stopped", "addr", debugAddr, "err", err)
			}
		}()
	}

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

	// Consume any handoff left by the process image we replaced. A restart
	// restores the repo the user was actually looking at, which outranks both
	// --repo and cwd detection — they may have switched repos in-app long
	// after launch.
	restored := app.LoadRestartStateFromEnv()

	// Determine repo to use (priority: restart handoff > --repo flag > auto-detect from cwd > picker)
	repoName := repoFlag
	if restored != nil && restored.ActiveRepo != "" {
		repoName = restored.ActiveRepo
	}
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

	// Discover repos (other than the initial one) still holding a non-terminal
	// tmux session, so the TUI can auto-open them and fully re-adopt those
	// sessions. This is a read-only probe: it never marks a dead session
	// completed, because that transition is what carries a subagent's report to
	// its parent and there is no courier here to hear it. That is also why a
	// repo whose window already died is returned — its manager still owes the
	// transition.
	resumeRepos := session.ReposNeedingTmuxReconcile(store, repoName, sharedManagerConfig.SessionMode)

	// The scan above only finds repos with a session still to reconcile, so
	// without the handoff a repo the user had opened but left idle would
	// disappear.
	resumeRepos = mergeResumeRepos(resumeRepos, restored, repoName)

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
			slog.Warn("task router failed to start, falling back to heuristic routing", "err", err)
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

	// Ordering here is load-bearing, in three steps: start the control server,
	// publish both socket paths, and only then let the IPC server listen.
	//
	// The IPC server serves RequestNewSession the moment it binds, and that
	// handler runs all the way to newTmuxRunner, which snapshots both socket
	// paths out of m.config into the window's environment for that session's
	// whole lifetime. So every path the manager will ever advertise has to be
	// final before IPC accepts its first connection. Starting control first is
	// what makes the failure case honest: if control never binds we publish an
	// empty path rather than a dead one, and no session can have read the dead
	// value in the meantime.
	//
	// This same before-any-listener property is what makes the unsynchronized
	// m.config writes safe: they all happen on this goroutine before any
	// session goroutine exists to observe them.

	// Start the control server (read+write tmux control plane) on its own Unix
	// socket. Local CLI subcommands (send-input, send-key) and the remote hub
	// agent client both drive the same control.Dispatcher.
	// The courier is created before the control server because the server's
	// dispatcher takes it: a send-input asking to be queued must either work or
	// say plainly that it cannot, never fall through to interrupting the
	// recipient. OnRegister covers the manager registered just above as well as
	// any repo opened later with Alt-R.
	courier := startCourier(ctx, registry)

	// Reconcile previously-running tmux sessions against live tmux windows.
	//
	// After the courier, not before: reconciliation is where a subagent that
	// finished while bramble was down gets its terminal transition, and that
	// is the only announcement its parent's report will ever get. Run it
	// first and the event is emitted into a courier that does not exist yet.
	if err := sessionManager.ReconcileTmuxSessions(); err != nil {
		slog.Warn("tmux session reconciliation failed", "err", err)
	}

	// Sweep again now that reconciliation has re-adopted the stored sessions:
	// the sweep inside OnRegister ran before they existed, so a recipient that
	// came back idle was not reachable yet.
	if courier != nil {
		courier.DrainIdle(ctx)
	}

	controlServer := startControlServer(registry, courier)
	controlSockPath := ""
	if controlServer != nil {
		defer controlServer.Close()
		controlSockPath = controlServer.SocketPath()
		os.Setenv(control.SockEnvVar, controlSockPath)
	}

	// The IPC server has not started yet, so its path is the one it is about to
	// bind. Publish it up front; clear it below if the bind fails.
	ipcSockPath := ipcSocketPath()
	publishSockPaths(sessionManager, &sharedManagerConfig, ipcSockPath, controlSockPath)

	// Start IPC server so child processes can request new sessions.
	// The registry aggregates all repo managers so IPC handlers can find
	// sessions from any repo, including those opened later via Alt-R.
	ipcServer := startIPCServer(registry, ipcSockPath, wtRoot, repoName)
	if ipcServer != nil {
		defer ipcServer.Close()
		os.Setenv(ipc.SockEnvVar, ipcSockPath)
	} else {
		// Nothing bound, so nothing is reachable at that path. Clear it rather
		// than exporting a dead socket into every tmux window. Safe to do here:
		// no listener ever accepted a request, so no session read the old value.
		publishSockPaths(sessionManager, &sharedManagerConfig, "", controlSockPath)
	}

	// If a hub is configured, dial out to it so the user can reach this
	// machine's sessions remotely. The agent client reuses the same dispatcher.
	if stopRemote := startRemoteAgent(ctx, registry, courier); stopRemote != nil {
		defer stopRemote()
	}

	// Query terminal size synchronously so the first View() renders a
	// properly laid-out UI instead of waiting for the async WindowSizeMsg.
	termWidth, termHeight, _ := term.GetSize(int(os.Stdout.Fd()))

	// Create and run TUI
	model := app.NewModel(ctx, wtRoot, repoName, editor, sessionManager, taskRouter, worktrees, termWidth, termHeight, providerAvailability, modelRegistry, sharedManagerConfig, resumeRepos)

	// Wire up voice reporting if requested.
	if enableVoiceReports {
		if reporter := app.BuildVoiceReporter(elevenLabsAPIKey, ttsVoice, voiceReportMode, voiceSaveDir); reporter != nil {
			defer reporter.Close()
			model.SetVoiceReporter(reporter)
		}
	}

	model.ApplyRestartState(restored)

	p := tea.NewProgram(model)

	// Let out-of-band restart requests (SIGUSR2, IPC) reach the event loop, but
	// only while there is a loop to reach. The defers are the safety net for an
	// early return; the ordinary path disarms below, the instant Run returns.
	restartRequests.set(func(msg app.RestartRequestedMsg) { p.Send(msg) })
	defer restartRequests.set(nil)
	stopRestartSignal := watchRestartSignal(func() {
		_ = restartRequests.request(app.RestartRequestedMsg{})
	})
	defer stopRestartSignal()

	finalModel, err := p.Run()

	// Disarm here rather than leaving it to the defers. The program is gone the
	// moment Run returns, but the defers do not run until the cleanup below has
	// finished writing the handoff and closing every manager — and the IPC
	// server is still accepting throughout. A restart request arriving in that
	// window would reach a p.Send that silently discards it, and the caller
	// would be told it succeeded. Refusing is the honest answer, and it is what
	// restartRequester exists to be able to do.
	restartRequests.set(nil)
	stopRestartSignal()

	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	// Close secondary repo managers on exit. The initial repo's manager is
	// closed by the defer above. This ensures tmux windows from any additionally
	// opened repos are cleaned up properly.
	if m, ok := finalModel.(app.Model); ok {
		if m.RestartRequested() {
			// A restart is not a quit: the tmux windows are exactly what the
			// replacement re-adopts, so --tmux-exit-on-quit must not fire.
			m.DisableTmuxExitOnQuitAll()
			armRestart(m.RestartSnapshot())
		}
		m.CloseSecondaryManagers(repoName)
	}

	return nil
}

// armRestart records the handoff for the exec that main() performs once every
// deferred cleanup in runTUI has run. It is best-effort: a state file we cannot
// write costs the user their selection, which is not worth cancelling a restart
// over, so the restart is still armed.
func armRestart(state app.RestartState) {
	restart.pending = true
	statePath, err := app.RestartStatePath()
	if err != nil {
		slog.Warn("cannot determine restart state path; restarting without restoring view state", "err", err)
		return
	}
	if err := app.WriteRestartState(statePath, state); err != nil {
		slog.Warn("failed to write restart state; restarting without restoring view state", "err", err)
		return
	}
	restart.statePath = statePath
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

// socketDir is where this process's Unix domain sockets live. It prefers
// $XDG_RUNTIME_DIR (user-private, tmpfs) over /tmp to avoid symlink/TOCTOU
// risks in world-writable directories.
func socketDir() string {
	if runDir := os.Getenv("XDG_RUNTIME_DIR"); runDir != "" {
		return runDir
	}
	return os.TempDir()
}

// ipcSocketPath and controlSocketPath derive each server's socket path from the
// pid alone, so a path is knowable before its server exists. runTUI depends on
// that: it must publish the IPC path to the manager before the IPC server binds,
// because that server starts serving session-creating requests immediately.
//
// Each is called exactly once per process, into a local that is then both
// published and handed to the server, so the advertised path and the bound path
// cannot diverge even though socketDir() re-reads the environment.
func ipcSocketPath() string {
	return filepath.Join(socketDir(), fmt.Sprintf("bramble-%d.sock", os.Getpid()))
}

func controlSocketPath() string {
	return filepath.Join(socketDir(), fmt.Sprintf("bramble-control-%d.sock", os.Getpid()))
}

// publishSockPaths makes both socket paths visible to every session the manager
// will create: directly on the manager for the current repo, and on the shared
// config template that openRepo copies for repos opened later via Alt-R. The two
// destinations are written together because a session launched from either sees
// the same tmux environment, and updating one without the other is how a
// secondary repo's sessions silently lose a socket.
//
// Callers must invoke this before any listener that can create sessions starts
// accepting: newTmuxRunner snapshots these values, so a session started earlier
// keeps whatever was set at its creation for its entire lifetime.
func publishSockPaths(m *session.Manager, shared *session.ManagerConfig, ipcSockPath, controlSockPath string) {
	m.SetIPCSockPath(ipcSockPath)
	m.SetControlSockPath(controlSockPath)
	shared.IPCSockPath = ipcSockPath
	shared.ControlSockPath = controlSockPath
}

// startIPCServer binds the IPC server to sockPath, which the caller has already
// published to the session manager. Taking the path as a parameter rather than
// recomputing it is what guarantees the two agree.
func startIPCServer(registry *session.SessionRegistry, sockPath, wtRoot, repoName string) *ipc.Server {
	srv := ipc.NewServer(sockPath)

	srv.Handle(ipc.RequestPing, func(_ context.Context, _ *ipc.Request) (any, error) {
		return "pong", nil
	})

	srv.Handle(ipc.RequestNewSession, func(ctx context.Context, req *ipc.Request) (any, error) {
		params, ok := req.Params.(*ipc.NewSessionParams)
		if !ok {
			return nil, fmt.Errorf("invalid params")
		}

		// A parent pins the repo more precisely than the process-wide default:
		// a subagent belongs with its parent, which may live under a different
		// manager than the repo bramble was launched on.
		parent, hasParent, err := parentForSpawn(registry, params)
		if err != nil {
			return nil, err
		}

		targetRepo := repoForSpawn(params, parent, hasParent, repoName)

		mgr, ok := registry.FindManagerByRepo(targetRepo)
		if !ok {
			return nil, fmt.Errorf("repo %q is not open in bramble; open it with Alt-R first", targetRepo)
		}

		return handleNewSession(ctx, mgr, wtRoot, targetRepo, params, parent)
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

	srv.Handle(ipc.RequestRestart, func(_ context.Context, req *ipc.Request) (any, error) {
		params, ok := req.Params.(*ipc.RestartParams)
		if !ok {
			return nil, fmt.Errorf("invalid params")
		}
		if err := restartRequests.request(app.RestartRequestedMsg{Force: params.Force}); err != nil {
			return nil, err
		}
		return "ok", nil
	})

	if err := srv.Start(); err != nil {
		slog.Warn("IPC server failed to start", "err", err)
		return nil
	}
	return srv
}

// newDispatcher builds a control dispatcher, enabling queued delivery when a
// courier is available. The nil check is not decoration: courier is a concrete
// pointer and SetCourier takes an interface, so passing a nil one through would
// store a non-nil interface holding a nil pointer and panic on first use.
func newDispatcher(registry *session.SessionRegistry, courier *session.Courier) *control.Dispatcher {
	disp := control.NewDispatcher(registry, tmuxctl.New())
	if courier != nil {
		disp.SetCourier(courier)
	}
	return disp
}

// startControlServer starts the control-protocol Unix server backed by the
// session registry and a real tmux controller. Returns nil if it fails to
// start (non-fatal — the TUI still runs, only remote/CLI control is absent).
func startControlServer(registry *session.SessionRegistry, courier *session.Courier) *control.UnixServer {
	srv := control.NewUnixServer(controlSocketPath(), newDispatcher(registry, courier))
	if err := srv.Start(); err != nil {
		slog.Warn("control server failed to start", "err", err)
		return nil
	}
	return srv
}

// startCourier builds the delivery courier and points it at every session
// manager, present and future.
//
// A failure here is not fatal. Queued delivery and subagent reports stop
// working and say so; everything else — including the unqueued send-input the
// TUI and CLI already rely on — is untouched.
func startCourier(ctx context.Context, registry *session.SessionRegistry) *session.Courier {
	courier, err := session.NewCourier(
		session.NewRegistryDeliveryTarget(registry),
		tmuxctl.NewPaneWriter(tmuxctl.New()),
		"",
	)
	if err != nil {
		slog.Warn("courier failed to start; queued delivery and subagent reports are unavailable", "err", err)
		return nil
	}
	registry.OnRegister(func(mgr *session.Manager) {
		courier.Watch(ctx, mgr)
		// Queues reloaded from disk belong to sessions that may already be
		// idle, and an idle session produces no transition for Watch to react
		// to. Sweep once now that this manager's sessions are reachable.
		courier.DrainIdle(ctx)
	})
	return courier
}

// startRemoteAgent dials the cloud hub when BRAMBLE_HUB_URL is set, serving
// control requests it forwards. Returns a stop func, or nil when no hub is
// configured. Auth and machine identity come from the environment so the TUI
// flags stay uncluttered:
//
//	BRAMBLE_HUB_URL    wss://hub.example/agent
//	BRAMBLE_HUB_TOKEN  machine auth token
//	BRAMBLE_MACHINE_ID stable machine id (defaults to hostname)
func startRemoteAgent(ctx context.Context, registry *session.SessionRegistry, courier *session.Courier) func() {
	hubURL := os.Getenv("BRAMBLE_HUB_URL")
	if hubURL == "" {
		return nil
	}
	hostname, _ := os.Hostname()
	machineID := os.Getenv("BRAMBLE_MACHINE_ID")
	if machineID == "" {
		machineID = hostname
	}
	client := remote.New(remote.Config{
		HubURL:     hubURL,
		Token:      os.Getenv("BRAMBLE_HUB_TOKEN"),
		MachineID:  machineID,
		Hostname:   hostname,
		Dispatcher: newDispatcher(registry, courier),
	})
	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		if err := client.Run(runCtx); err != nil && runCtx.Err() == nil {
			slog.Warn("remote agent client stopped", "err", err)
		}
	}()
	return cancel
}

// resolveParentSession looks a parent session ID up across every open repo.
// An empty ID is not an error — it just means the caller has no parent.
func resolveParentSession(registry *session.SessionRegistry, id string) (session.SessionInfo, bool) {
	if id == "" {
		return session.SessionInfo{}, false
	}
	info, _, ok := registry.GetSessionInfo(session.SessionID(id))
	if !ok {
		return session.SessionInfo{}, false
	}
	return info, true
}

// repoForSpawn picks the repo a new session is filed under, and so which
// manager starts it.
//
// A resolved parent outranks a repo the client only inferred from its cwd: a
// subagent belongs with its parent, the parent knows its own repo exactly, and
// an agent's cwd is merely wherever its worktree happens to be. A --repo the
// caller typed outranks both — and if that then contradicts an inherited
// worktree, handleNewSession refuses rather than guessing.
func repoForSpawn(params *ipc.NewSessionParams, parent session.SessionInfo, hasParent bool, fallbackRepo string) string {
	repo := params.RepoName
	if hasParent && (repo == "" || params.RepoInferred) && parent.RepoName != "" {
		repo = parent.RepoName
	}
	if repo == "" {
		repo = fallbackRepo
	}
	return repo
}

// parentForSpawn resolves the parent a new session should be filed under, and
// clears params.ParentSessionID when there is none — so the rest of the spawn
// sees one answer rather than an ID nothing can be looked up by.
//
// An explicitly named parent that does not resolve is an error: the caller
// asked for that session and got something else. An inherited one is only a
// default, and a default that cannot be honored must not cost the caller a
// spawn that would have worked without it — the registry sees only sessions
// adopted into an open manager, so an agent whose own repo is not open in this
// bramble would otherwise lose the ability to start any session at all.
func parentForSpawn(registry *session.SessionRegistry, params *ipc.NewSessionParams) (session.SessionInfo, bool, error) {
	parent, ok := resolveParentSession(registry, params.ParentSessionID)
	if ok || params.ParentSessionID == "" {
		return parent, ok, nil
	}
	if !params.ParentInherited {
		return session.SessionInfo{}, false, fmt.Errorf("parent session %q not found", params.ParentSessionID)
	}
	slog.Warn("spawning a top-level session: inherited parent is not adopted in this bramble; pass --no-parent to silence this",
		"parent_session_id", params.ParentSessionID)
	params.ParentSessionID = ""
	return session.SessionInfo{}, false, nil
}

func handleNewSession(ctx context.Context, mgr *session.Manager, wtRoot, repoName string, params *ipc.NewSessionParams, parent session.SessionInfo) (*ipc.NewSessionResult, error) {
	worktreePath := params.WorktreePath

	// Create worktree if requested
	if params.CreateWorktree && params.Branch != "" {
		m := wt.NewManager(wtRoot, repoName)
		path, err := m.New(ctx, params.Branch, params.BaseBranch, params.Goal)
		if err != nil {
			return nil, err
		}
		worktreePath = path
	}

	// A subagent with no worktree of its own works on its parent's tree — the
	// common "helper on the same branch" case. Asking for a fresh worktree is
	// still explicit (--create-worktree -b), so this never surprises a caller
	// who wanted isolation.
	if worktreePath == "" && parent.WorktreePath != "" {
		// A child living in its parent's tree must be filed under its parent's
		// repo. repoName picked mgr, and a --repo naming a different one would
		// register the session — and persist its history — under a repo whose
		// worktree it is not in.
		//
		// Only reachable for a repo the caller actually typed: an inferred one
		// has already lost to the parent's when the manager was chosen, so a
		// mismatch here means two explicit, incompatible requests and there is no
		// safe guess between them.
		if parent.RepoName != "" && parent.RepoName != repoName {
			return nil, fmt.Errorf("cannot spawn into repo %q while inheriting parent %s's worktree in repo %q: "+
				"drop --repo, or give the subagent a tree of its own with --worktree or --create-worktree --branch",
				repoName, parent.ID, parent.RepoName)
		}
		worktreePath = parent.WorktreePath
	}

	if worktreePath == "" {
		return nil, fmt.Errorf("either worktree_path, branch with create_worktree, or parent_session_id is required")
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

	id, err := mgr.StartSessionWithOpts(sessionType, worktreePath, params.Prompt, params.Model,
		session.SpawnOpts{ParentSessionID: session.SessionID(params.ParentSessionID)})
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
			ID:              string(s.ID),
			Type:            string(s.Type),
			Status:          string(s.Status),
			WorktreeName:    s.WorktreeName,
			Prompt:          s.Prompt,
			Model:           s.Model,
			ParentSessionID: string(s.ParentSessionID),
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

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Ask the running bramble TUI to restart into the binary on disk",
	Long: `Replaces the running bramble TUI with the binary currently at its own
path, keeping the process ID so that already-running sessions keep their IPC and
control sockets.

Rebuild or reinstall bramble first; this only performs the swap.

Without --force the TUI asks for confirmation if any in-process (non-tmux)
session would be lost, so this command returning successfully means the restart
was requested, not necessarily that it happened.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		client, err := ipc.NewClientFromEnv()
		if err != nil {
			return err
		}
		resp, err := client.Send(&ipc.Request{
			Type:   ipc.RequestRestart,
			Params: &ipc.RestartParams{Force: force},
		})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("restart request failed: %s", resp.Error)
		}
		fmt.Println("restart requested")
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
		parentFlag, _ := cmd.Flags().GetString("parent")
		noParent, _ := cmd.Flags().GetBool("no-parent")
		parent, parentInherited := resolveParentSessionID(parentFlag, os.Getenv(session.SessionIDEnvVar), noParent)

		// Auto-detect repo from cwd if not explicitly specified. Flagged as
		// inferred: an agent's cwd is wherever its worktree is, which is not the
		// same claim as a --repo the caller typed.
		repoInferred := false
		if repo == "" {
			if wtRoot, err := resolveWTRoot(); err == nil {
				cwd, _ := os.Getwd()
				repo, _ = detectRepoFromPath(cwd, wtRoot)
				repoInferred = repo != ""
			}
		}

		resp, err := client.Send(&ipc.Request{
			Type: ipc.RequestNewSession,
			ID:   "cli-new-session",
			Params: &ipc.NewSessionParams{
				SessionType:     sessionType,
				Branch:          branch,
				BaseBranch:      baseBranch,
				WorktreePath:    worktreePath,
				CreateWorktree:  createWT,
				Prompt:          prompt,
				Model:           model,
				Goal:            goal,
				RepoName:        repo,
				RepoInferred:    repoInferred,
				ParentSessionID: parent,
				ParentInherited: parentInherited,
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

// resolveOwnSessionID picks the session a self-referential command (notify)
// reports on. Unlike capture-pane/send-input, whose --session-id names a *peer*,
// notify always speaks for its caller, so a tmux-launched agent can omit the
// flag and let $BRAMBLE_SESSION_ID supply its own return address. The flag wins
// when set, keeping the baked-in stop hook and out-of-band callers unchanged.
func resolveOwnSessionID(flagID, envID string) (string, error) {
	if flagID != "" {
		return flagID, nil
	}
	if envID != "" {
		return envID, nil
	}
	return "", fmt.Errorf("no session: pass --session-id or run inside a bramble session ($%s)", session.SessionIDEnvVar)
}

// resolveParentSessionID picks the parent for a newly spawned session. Unlike
// resolveOwnSessionID, having no answer is legitimate — a spawn from a plain
// terminal is simply top-level — so this returns "" rather than an error.
// --no-parent is the escape hatch for spawning a top-level session from inside
// a bramble session, which would otherwise always inherit a parent.
//
// inherited says the ID came from the environment rather than the flag. The
// server needs that distinction to decide whether an unresolvable parent is an
// error or just a default it should drop — see ipc.NewSessionParams.
func resolveParentSessionID(flagID, envID string, noParent bool) (id string, inherited bool) {
	if noParent {
		return "", false
	}
	if flagID != "" {
		return flagID, false
	}
	return envID, envID != ""
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
		flagID, _ := cmd.Flags().GetString("session-id")
		sessionID, err := resolveOwnSessionID(flagID, os.Getenv(session.SessionIDEnvVar))
		if err != nil {
			if silent {
				return nil
			}
			return err
		}
		resp, err := sendNotifyWithRetry(client, sessionID)
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

// Backoff for a notify whose socket is missing. An in-place restart unbinds the
// IPC socket and re-binds the same path in the replacement image, but only
// after the new process has re-listed worktrees, probed providers, and
// reconciled tmux — routinely more than a second. Without a retry spanning
// that, a session finishing its turn inside the window notifies nobody, and
// since the notify handler is the only caller of SetSessionIdle the session
// never shows as idle at all. The stop hook runs with --silent, so nothing
// would ever say so.
//
// The budget is a wall-clock span rather than an attempt count because what has
// to be covered is a duration: the gap above is however long the replacement
// image takes to reach its IPC bind, and a count only expresses that
// second-hand. Delays grow geometrically so a socket that comes back quickly is
// noticed quickly, and cap out so the tail stays a poll rather than one long
// sleep that overshoots the budget.
//
// The ceiling is what a stop hook pays when bramble is gone for good rather
// than restarting, since that case is indistinguishable from here — which is
// why it is seconds and not the minute a truly generous budget would want.
// notifyRetryPolicy is the shape of that backoff, separated from the constants
// so a test can drive the loop on a millisecond budget instead of waiting out
// the real one.
type notifyRetryPolicy struct {
	budget     time.Duration
	baseDelay  time.Duration
	maxDelay   time.Duration
	multiplier int
}

var defaultNotifyRetryPolicy = notifyRetryPolicy{
	budget:     5 * time.Second,
	baseDelay:  100 * time.Millisecond,
	maxDelay:   time.Second,
	multiplier: 3,
}

// sendNotifyWithRetry sends a notify request, backing off over a transport
// failure until the budget is spent.
func sendNotifyWithRetry(client *ipc.Client, sessionID string) (*ipc.Response, error) {
	return retryNotify(defaultNotifyRetryPolicy, func() (*ipc.Response, error) {
		return client.Send(&ipc.Request{
			Type:   ipc.RequestNotify,
			ID:     "cli-notify",
			Params: &ipc.NotifyParams{SessionID: sessionID},
		})
	})
}

// retryNotify calls send until it stops returning a transport error or the
// policy's budget runs out. Only transport errors are retried: a response from
// the server, even an error one, is a real answer and is returned as-is.
func retryNotify(p notifyRetryPolicy, send func() (*ipc.Response, error)) (*ipc.Response, error) {
	var lastErr error
	deadline := time.Now().Add(p.budget)
	delay := p.baseDelay
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// Sleeping past the deadline would spend more than the budget
			// allows, and waking to a dial we have already decided not to make
			// is pure latency.
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			time.Sleep(min(delay, remaining))
			delay = min(delay*time.Duration(p.multiplier), p.maxDelay)
		}
		resp, err := send()
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, lastErr
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

		// Filtering happens client-side: the server already returns every
		// session with its parent.
		result := resp.Result
		if cmd.Flags().Changed("parent") {
			parentFlag, _ := cmd.Flags().GetString("parent")
			// --parent= (empty) means "my own children", so a caller inside a
			// bramble session need not echo its own ID back.
			parent, _ := resolveParentSessionID(parentFlag, os.Getenv(session.SessionIDEnvVar), false)
			if parent == "" {
				return fmt.Errorf("--parent needs an ID, or $%s must be set", session.SessionIDEnvVar)
			}
			filtered, err := filterSessionsByParent(resp.Result, parent)
			if err != nil {
				return err
			}
			result = filtered
		}

		out, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(out))
		return nil
	},
}

// filterSessionsByParent narrows a list-sessions result to one session's
// children. The result arrives as a generic map (ipc.Response.Result is any),
// so it is re-marshaled through the typed struct rather than reached into.
func filterSessionsByParent(result any, parent string) (*ipc.ListSessionsResult, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var list ipc.ListSessionsResult
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	children := make([]ipc.SessionSummary, 0, len(list.Sessions))
	for _, s := range list.Sessions {
		if s.ParentSessionID == parent {
			children = append(children, s)
		}
	}
	return &ipc.ListSessionsResult{Sessions: children}, nil
}

// runControl performs a one-shot control request against the running bramble's
// control socket and decodes the result into v (v may be nil).
func runControl(typ control.MsgType, payload, v any) error {
	sock := os.Getenv(control.SockEnvVar)
	if sock == "" {
		return fmt.Errorf("$%s is not set — is a bramble TUI running?", control.SockEnvVar)
	}
	req, err := control.NewRequest(typ, "cli", payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := control.Request(ctx, sock, req)
	if err != nil {
		return err
	}
	return resp.DecodeResponse(v)
}

var sendInputCmd = &cobra.Command{
	Use:   "send-input",
	Short: "Send prompt text to a session (optionally queueing it until the session is idle)",
	Long: `Send prompt text to a session's tmux pane, or with --queue to any session
whatever its runner.

Without --queue the text is typed into the pane immediately. That is the right
behaviour for a deliberate interrupt, but if the recipient is mid-turn the text
lands in its *next* prompt, out of the context that made it make sense — and a
TUI-mode session has no pane to type into at all.

With --queue the message is held until the recipient goes idle and is then
delivered through whichever path its runner supports. Use --queue for anything
the recipient should read as part of its own work.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		sessionID, _ := cmd.Flags().GetString("session-id")
		target, _ := cmd.Flags().GetString("target")
		text, _ := cmd.Flags().GetString("text")
		submit, _ := cmd.Flags().GetBool("submit")
		queue, _ := cmd.Flags().GetBool("queue")
		from, _ := cmd.Flags().GetString("from")
		if from == "" {
			// A session messaging a peer is identified by its own ID, so a
			// subagent reporting to its parent replaces bramble's generated
			// report instead of arriving alongside it.
			from = os.Getenv(session.SessionIDEnvVar)
		}

		typ := control.TypeSessionSendInput
		if sessionID == "" {
			typ = control.TypePaneSendInput
		}
		var result control.SendInputResult
		if err := runControl(typ, control.SendInputReq{
			SessionID: sessionID, Target: target, From: from,
			Text: text, Submit: submit, Queue: queue,
		}, &result); err != nil {
			return err
		}
		if result.Queued {
			fmt.Printf("queued for %s; it will be delivered when that session can take it\n", sessionID)
		}
		return nil
	},
}

var sendKeyCmd = &cobra.Command{
	Use:   "send-key",
	Short: "Send a single named key (Enter, Escape, C-c, Up, ...) to a session's pane",
	RunE: func(cmd *cobra.Command, _ []string) error {
		sessionID, _ := cmd.Flags().GetString("session-id")
		target, _ := cmd.Flags().GetString("target")
		key, _ := cmd.Flags().GetString("key")

		typ := control.TypeSessionSendKey
		if sessionID == "" {
			typ = control.TypePaneSendKey
		}
		return runControl(typ, control.SendKeyReq{
			SessionID: sessionID, Target: target, Key: tmuxctl.SpecialKey(key),
		}, nil)
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

		if err := checkCodetalkModel(model); err != nil {
			return err
		}

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

		ct.PrintUsageSummary()
		if path := ct.RecordingPath(); path != "" {
			fmt.Fprintf(os.Stderr, "Session recorded to: %s\n", path)
		}
		return nil
	},
}

// checkCodetalkModel returns an error if the model ID belongs to a non-Claude
// provider. The standalone codetalk CLI is Claude-only; non-Claude models must
// use the bramble TUI, which has full provider routing built in.
func checkCodetalkModel(modelID string) error {
	provider, ok := agent.ProviderForModelID(modelID)
	if ok && provider != agent.ProviderClaude {
		return fmt.Errorf("bramble codetalk only supports Claude models; %q uses provider %q — use the bramble TUI (bramble) to start a CodeTalk session with non-Claude models", modelID, provider)
	}
	return nil
}

func init() {
	newSessionCmd.Flags().StringP("type", "t", "planner", "Session type: planner, builder, or codetalk")
	newSessionCmd.Flags().StringP("branch", "b", "", "Branch name (creates worktree if --create-worktree)")
	newSessionCmd.Flags().StringP("from", "f", "", "Base branch for new worktree")
	newSessionCmd.Flags().StringP("worktree", "w", "", "Existing worktree path")
	newSessionCmd.Flags().StringP("prompt", "p", "", "Prompt for the session")
	newSessionCmd.Flags().StringP("model", "m", "", "Model ID (e.g. opus, sonnet)")
	newSessionCmd.Flags().StringP("goal", "g", "", "Goal for new worktree")
	newSessionCmd.Flags().Bool("create-worktree", false, "Create a new worktree for the branch")
	newSessionCmd.Flags().StringP("repo", "r", "", "Target repo name (auto-detected from cwd if omitted)")
	newSessionCmd.Flags().String("parent", "",
		"Spawn as a subagent of this session; it receives a report when the new session finishes "+
			"(defaults to $"+session.SessionIDEnvVar+")")
	newSessionCmd.Flags().Bool("no-parent", false,
		"Spawn a top-level session even when running inside a bramble session")
	listSessionsCmd.Flags().String("parent", "",
		"Only list sessions spawned by this session; --parent= means $"+session.SessionIDEnvVar)

	// Not MarkFlagRequired: inside a tmux session $BRAMBLE_SESSION_ID supplies
	// the caller's own ID, and RunE errors when neither source yields one.
	notifyCmd.Flags().String("session-id", "", "Session ID to notify (defaults to $"+session.SessionIDEnvVar+")")
	notifyCmd.Flags().Bool("silent", false, "Suppress errors silently (used by stop hooks)")

	capturePaneCmd.Flags().String("session-id", "", "Session ID to capture pane from")
	capturePaneCmd.Flags().Int("lines", 10, "Number of lines to capture")
	_ = capturePaneCmd.MarkFlagRequired("session-id")

	sendInputCmd.Flags().String("session-id", "", "Target bramble session ID (session-centric)")
	sendInputCmd.Flags().String("target", "", "Raw tmux target (window/pane id) instead of a session")
	sendInputCmd.Flags().String("text", "", "Text to deliver to the pane")
	sendInputCmd.Flags().Bool("submit", false, "Press Enter after delivering the text")
	sendInputCmd.Flags().Bool("queue", false,
		"Hold the text until the session is idle instead of typing into a live turn "+
			"(requires --session-id; also reaches TUI-mode sessions)")
	sendInputCmd.Flags().String("from", "",
		"Sender's session ID (defaults to $"+session.SessionIDEnvVar+"); a subagent "+
			"messaging its parent this way replaces bramble's generated report")
	_ = sendInputCmd.MarkFlagRequired("text")

	sendKeyCmd.Flags().String("session-id", "", "Target bramble session ID (session-centric)")
	sendKeyCmd.Flags().String("target", "", "Raw tmux target (window/pane id) instead of a session")
	sendKeyCmd.Flags().String("key", "", "Named key: Enter, Escape, C-c, C-d, Tab, BSpace, Up, Down, Left, Right")
	_ = sendKeyCmd.MarkFlagRequired("key")

	codetalkCmd.Flags().StringP("model", "m", "opus", "Model to use (e.g. opus, sonnet)")
	codetalkCmd.Flags().String("dir", "", "Working directory (defaults to current directory)")
	codetalkCmd.Flags().String("record", "", "Directory for session recordings (defaults to ~/.yoloswe)")
	codetalkCmd.Flags().String("system", "", "Custom system prompt")
	codetalkCmd.Flags().BoolP("verbose", "v", false, "Show detailed tool results")

	restartCmd.Flags().Bool("force", false, "Restart without confirming, even if in-process sessions would be lost")

	rootCmd.AddCommand(pingCmd)
	rootCmd.AddCommand(restartCmd)
	rootCmd.AddCommand(newSessionCmd)
	rootCmd.AddCommand(listSessionsCmd)
	rootCmd.AddCommand(notifyCmd)
	rootCmd.AddCommand(capturePaneCmd)
	rootCmd.AddCommand(sendInputCmd)
	rootCmd.AddCommand(sendKeyCmd)
	rootCmd.AddCommand(codereview.Cmd)
	rootCmd.AddCommand(delegator.Cmd)
	rootCmd.AddCommand(codetalkCmd)
	rootCmd.AddCommand(speak.Cmd)
}

// pickRouterProvider selects the best available provider for the task router.
// Prefers codex (original default), then claude, then gemini, then agy.
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
	if availability.IsInstalled(agent.ProviderAgy) && enabled(agent.ProviderAgy) {
		return agent.NewAgyProvider()
	}
	return nil
}
