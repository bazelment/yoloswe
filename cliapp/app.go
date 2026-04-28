// Package cliapp owns the standard CLI lifecycle for tools in this monorepo:
// flag registration, log-file setup, slog/render.Renderer wiring, signal
// handling with double-signal force-exit, an invocation banner with arg
// redaction, and exit-code mapping.
//
// Typical usage:
//
//	func main() {
//	    var opts cliapp.Options
//	    opts.ToolName = "mycli"
//	    var args runArgs
//	    rootCmd := &cobra.Command{
//	        Use: "mycli",
//	        RunE: func(cmd *cobra.Command, _ []string) error {
//	            return cliapp.Run(opts, func(ctx context.Context, app *cliapp.App) error {
//	                return run(ctx, app, args)
//	            })
//	        },
//	    }
//	    cliapp.RegisterStandardFlags(rootCmd, &opts)
//	    // ... domain flags ...
//	    _ = rootCmd.Execute()
//	}
package cliapp

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/logging/klogfmt"
)

// Options configures a CLI lifecycle. Fields populated by RegisterStandardFlags
// are Verbose, Verbosity, and Color; ToolName and SensitiveFlags are set by
// the caller before calling Run.
type Options struct { //nolint:govet // fieldalignment: readability over packing
	// ToolName is used for the log directory ($HOME/.<ToolName>/logs by
	// default) and the log filename prefix.
	ToolName string
	// Verbosity is "quiet", "normal", "verbose", or "debug".
	Verbosity string
	// Color is "auto", "always", or "never".
	Color string
	// LogDirOverride lets the caller pin the log directory to a specific
	// path (e.g. medivac uses <repo-root>/.medivac/logs). When empty,
	// the default $HOME/.<ToolName>/logs is used.
	LogDirOverride string
	// LogFileLevel controls the minimum slog level captured in the log
	// file. Defaults to slog.LevelDebug. Tools that emit custom levels
	// below Debug (e.g. medivac's LevelTrace/LevelDump) can set this to
	// a more verbose level so the file captures them.
	LogFileLevel slog.Leveler
	// SensitiveFlags are flag prefixes whose values get redacted in the
	// invocation banner. They are appended to DefaultSensitiveFlags.
	SensitiveFlags []string
	// Verbose is the boolean shorthand for --verbosity=verbose.
	Verbose bool
}

// App is the runtime handle passed to the user's run function. Logger and
// Renderer are always non-nil. LogPath and LogFile are empty/nil if the log
// file couldn't be opened (in which case slog has been initialized to write
// to stderr at the resolved verbosity level).
//
// LogFile is exposed for callers that need to pass the raw file handle to
// subprocesses or other writers (e.g. medivac's engine.Config.LogFile).
type App struct {
	Logger   *slog.Logger
	Renderer *render.Renderer
	LogFile  *os.File
	LogPath  string
}

// RunFunc is the user's entry point. The returned error determines the exit
// code: nil → 0, ctx.Err() → 130 (interrupted), anything else → 1.
type RunFunc func(ctx context.Context, app *App) error

// appKey is the context key for retrieving the App from a context. Used by
// multi-subcommand CLIs that wrap cobra.ExecuteContext inside cliapp.Run.
type appKey struct{}

// WithApp returns a copy of ctx that carries the given App. This is how
// multi-subcommand CLIs propagate the App to subcommand RunE callbacks via
// cobra's ExecuteContext.
func WithApp(ctx context.Context, app *App) context.Context {
	return context.WithValue(ctx, appKey{}, app)
}

// FromContext returns the App associated with ctx, or nil if none is set.
func FromContext(ctx context.Context) *App {
	app, _ := ctx.Value(appKey{}).(*App)
	return app
}

// Run is the lifecycle entry point. It never returns — it calls os.Exit at
// the end. Behavior:
//
//  1. Resolves verbosity and color from opts. Returns exit code 2 with a
//     stderr message on bad flag values.
//  2. Opens the log file (DEBUG level) and sets slog.Default. On failure,
//     falls back to stderr-only logging at the verbosity-mapped level and
//     emits a warning.
//  3. Builds the render.Renderer on stderr.
//  4. Logs an invocation banner with redacted os.Args[1:].
//  5. Sets up a context with double-signal force-exit (Ctrl-C twice → exit 130).
//  6. Invokes fn. On nil → exit 0. On ctx-cancellation → exit 130. Otherwise
//     logs the error via slog and exits 1.
func Run(opts Options, fn RunFunc) {
	if opts.ToolName == "" {
		fmt.Fprintln(os.Stderr, "cliapp: ToolName is required")
		os.Exit(2)
	}

	v, err := resolveVerbosity(opts.Verbose, opts.Verbosity)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	colorMode, err := resolveColor(opts.Color)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	logFile, cleanup, logPath := setupLogging(opts, v)
	defer cleanup()

	renderer := render.New(os.Stderr,
		render.WithVerbosity(v),
		render.WithColorMode(colorMode),
	)
	if logPath != "" {
		renderer.Status("Logging to " + logPath)
	}

	logger := slog.Default()

	cwd, _ := os.Getwd()
	sensitive := append(append([]string(nil), DefaultSensitiveFlags...), opts.SensitiveFlags...)
	logger.Info(opts.ToolName+" starting",
		"args", RedactArgs(os.Args[1:], sensitive),
		"cwd", cwd,
		"pid", os.Getpid(),
	)

	ctx, cancel := notifyContext(context.Background(), func() {
		fmt.Fprintln(os.Stderr, "Received second signal, forcing exit")
		os.Exit(130)
	})
	defer cancel()

	app := &App{
		Logger:   logger,
		Renderer: renderer,
		LogFile:  logFile,
		LogPath:  logPath,
	}

	runErr := fn(ctx, app)

	switch {
	case runErr == nil:
		os.Exit(0)
	case ctx.Err() != nil:
		// Interrupted via signal. The user's fn returned because of
		// cancellation; treat as 130 regardless of what err it surfaced.
		os.Exit(130)
	default:
		logger.Error(opts.ToolName+" failed", "error", runErr)
		os.Exit(1)
	}
}

// setupLogging opens the log file and sets slog.Default to write to it at
// the configured file level. On failure, falls back to klogfmt.Init at the
// verbosity-mapped stderr level and emits a slog.Warn describing the
// fallback. Returns the open file handle (or nil), a cleanup func, and the
// active log path (empty when fallback engaged).
func setupLogging(opts Options, v render.Verbosity) (*os.File, func(), string) {
	stderrLevel := stderrLevelFor(v)

	logDir, candidatePath, err := resolveLogPath(opts.ToolName, opts.LogDirOverride)
	if err != nil {
		klogfmt.Init(klogfmt.WithLevel(stderrLevel))
		slog.Warn("could not determine log path, logging to stderr only", "error", err)
		return nil, func() {}, ""
	}

	f, err := openLogFile(logDir, candidatePath)
	if err != nil {
		klogfmt.Init(klogfmt.WithLevel(stderrLevel))
		slog.Warn("failed to open log file, logging to stderr only",
			"path", candidatePath, "error", err)
		return nil, func() {}, ""
	}

	fileLevel := slog.Leveler(slog.LevelDebug)
	if opts.LogFileLevel != nil {
		fileLevel = opts.LogFileLevel
	}
	slog.SetDefault(slog.New(klogfmt.New(f, klogfmt.WithLevel(fileLevel))))
	return f, func() { _ = f.Close() }, candidatePath
}
