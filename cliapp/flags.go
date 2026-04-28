package cliapp

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
)

// RegisterStandardFlags binds --verbose, --verbosity, and --color onto cmd's
// flag set, writing to opts. Callers should call this before cmd.Execute.
//
// The flags use long-only forms by design: --verbose has no -v shorthand
// because subcommands frequently want -v for their own purposes (e.g. medivac
// previously used -v for count-style verbosity).
func RegisterStandardFlags(cmd *cobra.Command, opts *Options) {
	cmd.PersistentFlags().BoolVar(&opts.Verbose, "verbose", false,
		"Verbose output (shorthand for --verbosity=verbose)")
	cmd.PersistentFlags().StringVar(&opts.Verbosity, "verbosity", "normal",
		"Output verbosity: quiet, normal, verbose, debug")
	cmd.PersistentFlags().StringVar(&opts.Color, "color", "auto",
		"Color output: auto, always, never")
}

// resolveVerbosity collapses --verbose and --verbosity into a single
// render.Verbosity. Returns an error for unrecognized verbosity strings so
// users get explicit feedback (render.ParseVerbosity silently maps unknowns
// to VerbosityNormal, which hides typos).
func resolveVerbosity(verbose bool, verbosityStr string) (render.Verbosity, error) {
	if !isKnownVerbosity(verbosityStr) {
		return render.VerbosityNormal, fmt.Errorf("unknown --verbosity %q (valid: quiet, normal, verbose, debug)", verbosityStr)
	}
	v := render.ParseVerbosity(verbosityStr)
	if verbose && v < render.VerbosityVerbose {
		v = render.VerbosityVerbose
	}
	return v, nil
}

func isKnownVerbosity(s string) bool {
	switch s {
	case "quiet", "q",
		"normal", "n", "",
		"verbose", "v",
		"debug", "d":
		return true
	}
	return false
}

// resolveColor parses the --color string. Unknown values are rejected (rather
// than silently falling back to auto) so users see typos.
func resolveColor(s string) (render.ColorMode, error) {
	switch s {
	case "auto", "":
		return render.ColorAuto, nil
	case "always":
		return render.ColorAlways, nil
	case "never":
		return render.ColorNever, nil
	}
	return render.ColorAuto, fmt.Errorf("unknown --color %q (valid: auto, always, never)", s)
}

// StandardChildArgs returns the canonical --verbose/--verbosity/--color
// flags that should be propagated when a tool re-invokes itself as a child
// subprocess. Defaults (normal/auto) are skipped so the child argv stays
// minimal.
func (a *App) StandardChildArgs() []string {
	var out []string
	switch a.Verbosity {
	case render.VerbosityQuiet:
		out = append(out, "--verbosity", "quiet")
	case render.VerbosityVerbose:
		out = append(out, "--verbose")
	case render.VerbosityDebug:
		out = append(out, "--verbosity", "debug")
	}
	switch a.Color {
	case render.ColorAlways:
		out = append(out, "--color", "always")
	case render.ColorNever:
		out = append(out, "--color", "never")
	}
	return out
}

// stderrLevelFor maps a render.Verbosity to a slog.Level used as the stderr
// fallback when the log file isn't writable.
func stderrLevelFor(v render.Verbosity) slog.Level {
	switch {
	case v >= render.VerbosityDebug:
		return slog.LevelDebug
	case v <= render.VerbosityQuiet:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// preParseStandardFlags extracts --verbose, --verbosity, and --color from
// os.Args into opts before Run sets up the logger and renderer. Cobra will
// parse these flags again when fn calls rootCmd.Execute; this early pass
// exists solely so the logging and renderer configuration reflects the user's
// actual flag values rather than the zero/default values present before cobra
// runs. Unknown flags are silently ignored so subcommand-specific flags don't
// cause errors here.
func preParseStandardFlags(opts *Options) {
	fs := pflag.NewFlagSet("cliapp-pre-parse", pflag.ContinueOnError)
	fs.ParseErrorsWhitelist.UnknownFlags = true
	fs.BoolVar(&opts.Verbose, "verbose", opts.Verbose, "")
	fs.StringVar(&opts.Verbosity, "verbosity", opts.Verbosity, "")
	fs.StringVar(&opts.Color, "color", opts.Color, "")
	// Discard errors: unknown flags are expected (subcommand flags, positional
	// args with - prefix), and --help should not exit here.
	_ = fs.Parse(os.Args[1:])
}
