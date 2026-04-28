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
//	    rootCmd := &cobra.Command{Use: "mycli", ...}
//	    cliapp.RegisterStandardFlags(rootCmd, &opts)
//	    // ... domain flags ...
//	    os.Exit(cliapp.Run(&opts, func(ctx context.Context, app *cliapp.App) error {
//	        return rootCmd.ExecuteContext(cliapp.WithApp(ctx, app))
//	    }))
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

// Options configures a CLI lifecycle. Verbose, Verbosity, and Color are
// populated by RegisterStandardFlags; ToolName, LogFileLevel, and
// SensitiveFlags are set by the caller before calling Run.
type Options struct { //nolint:govet // fieldalignment: readability over packing
	// ToolName is used for the log directory ($HOME/.<ToolName>/logs) and
	// the log filename prefix.
	ToolName string
	// Verbosity is "quiet", "normal", "verbose", or "debug".
	Verbosity string
	// Color is "auto", "always", or "never".
	Color string
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

// App is the runtime handle passed to the user's run function. All fields
// are non-zero except LogPath (empty if the log file couldn't be opened, in
// which case slog has been initialized to write to stderr).
type App struct {
	Logger    *slog.Logger
	Renderer  *render.Renderer
	LogPath   string
	Verbosity render.Verbosity
	Color     render.ColorMode
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

// Run is the lifecycle entry point. It returns the process exit code; the
// caller is expected to do `os.Exit(cliapp.Run(...))`. Returning instead of
// calling os.Exit directly lets defers run, so log-file flushes are not
// truncated.
//
// opts must be a pointer to the same Options struct that was passed to
// RegisterStandardFlags, so that Run reads the values cobra will write during
// flag parsing. Run pre-parses --verbose, --verbosity, and --color from
// os.Args before fn is called, so the logger and renderer are set up with the
// user's actual flag values rather than the defaults.
//
// Behavior:
//
//  1. Pre-parses standard flags from os.Args so opts reflects user intent.
//  2. Resolves verbosity and color from opts. Returns 2 with a stderr
//     message on bad flag values.
//  3. Opens the log file (DEBUG level by default) and sets slog.Default. On
//     failure, falls back to stderr-only logging at the verbosity-mapped
//     level and emits a warning.
//  4. Builds the render.Renderer on stderr.
//  5. Logs an invocation banner with redacted os.Args[1:].
//  6. Sets up a context with double-signal force-exit (Ctrl-C twice → 130).
//  7. Invokes fn. nil → 0; ctx-cancellation → 130; otherwise logs the error
//     via slog and returns 1.
func Run(opts *Options, fn RunFunc) int {
	if opts == nil {
		fmt.Fprintln(os.Stderr, "cliapp: Options must not be nil")
		return 2
	}
	if opts.ToolName == "" {
		fmt.Fprintln(os.Stderr, "cliapp: ToolName is required")
		return 2
	}

	// Pre-parse the standard flags from os.Args so the logger and renderer
	// are configured with the user's actual values. Cobra will parse them
	// again during fn; this early pass just ensures our setup sees them.
	preParseStandardFlags(opts)

	v, err := resolveVerbosity(opts.Verbose, opts.Verbosity)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	colorMode, err := resolveColor(opts.Color)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	logger, logPath, closeLog := setupLogging(opts, v)
	defer closeLog()

	renderer := render.New(os.Stderr,
		render.WithVerbosity(v),
		render.WithColorMode(colorMode),
	)
	if logPath != "" {
		renderer.Status("Logging to " + logPath)
	}

	cwd, _ := os.Getwd()
	sensitive := append(append([]string(nil), DefaultSensitiveFlags...), opts.SensitiveFlags...)
	logger.Info(opts.ToolName+" starting",
		"args", RedactArgs(os.Args[1:], sensitive),
		"cwd", cwd,
		"pid", os.Getpid(),
	)

	ctx, cancel := notifyContext(context.Background(), func() {
		fmt.Fprintln(os.Stderr, "Received second signal, forcing exit")
		// Best-effort flush before the hard exit.
		closeLog()
		os.Exit(130)
	})
	defer cancel()

	app := &App{
		Logger:    logger,
		Renderer:  renderer,
		LogPath:   logPath,
		Verbosity: v,
		Color:     colorMode,
	}

	runErr := fn(ctx, app)

	switch {
	case runErr == nil:
		return 0
	case ctx.Err() != nil:
		return 130
	default:
		logger.Error(opts.ToolName+" failed", "error", runErr)
		return 1
	}
}

// setupLogging opens the log file and returns a logger that writes to it at
// the configured file level. On failure, falls back to klogfmt.Init at the
// verbosity-mapped stderr level and emits a slog.Warn describing the
// fallback. The returned closer is always safe to call multiple times.
func setupLogging(opts *Options, v render.Verbosity) (logger *slog.Logger, logPath string, closer func()) {
	noop := func() {}
	stderrLevel := stderrLevelFor(v)

	logDir, candidatePath, err := resolveLogPath(opts.ToolName)
	if err != nil {
		klogfmt.Init(klogfmt.WithLevel(stderrLevel))
		slog.Warn("could not determine log path, logging to stderr only", "error", err)
		return slog.Default(), "", noop
	}

	f, err := openLogFile(logDir, candidatePath)
	if err != nil {
		klogfmt.Init(klogfmt.WithLevel(stderrLevel))
		slog.Warn("failed to open log file, logging to stderr only",
			"path", candidatePath, "error", err)
		return slog.Default(), "", noop
	}

	fileLevel := slog.Leveler(slog.LevelDebug)
	if opts.LogFileLevel != nil {
		fileLevel = opts.LogFileLevel
	}
	l := slog.New(klogfmt.New(f, klogfmt.WithLevel(fileLevel)))
	slog.SetDefault(l)

	var closeOnce bool
	return l, candidatePath, func() {
		if closeOnce {
			return
		}
		closeOnce = true
		_ = f.Sync()
		_ = f.Close()
	}
}
