package cliapp

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

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

// stderrLevelFor maps a render.Verbosity to a slog.Level used as the stderr
// fallback when the log file isn't writable. Mirrors the level table that
// jiradozer and prdozer have copy-pasted.
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
