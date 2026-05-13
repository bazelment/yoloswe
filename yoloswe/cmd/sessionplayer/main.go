// sessionplayer plays back recorded Claude and Codex session messages with colored output.
//
// Usage:
//
//	sessionplayer [flags] <path>
//
// The path can be:
//   - A directory containing messages.jsonl (Claude format)
//   - A JSONL file (Codex format)
//
// Examples:
//
//	sessionplayer .planner-sessions/session-abc123-1234567890/
//	sessionplayer session.jsonl
//	sessionplayer -verbose -no-color session.jsonl
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/logging/klogfmt"
	"github.com/bazelment/yoloswe/yoloswe/sessionplayer"
)

type cliConfig struct {
	verbosity string
	color     string
	paths     []string
	verbose   bool
}

func main() {
	klogfmt.Init()
	cfg := defaultCLIConfig()
	fs := newFlagSet(&cfg)
	fs.Usage = func() {
		printUsage(os.Stderr, os.Args[0], fs)
	}
	if err := parseWithFlagSet(fs, &cfg, os.Args[1:]); err != nil {
		if errors.Is(err, errMissingPath) {
			fmt.Fprintln(os.Stderr, "Error: no session path provided")
			fs.Usage()
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fs.Usage()
		os.Exit(2)
	}

	var player *sessionplayer.Player
	if cfg.noColor() {
		player = sessionplayer.NewPlayerWithOptions(os.Stdout, cfg.verboseEffective(), true)
	} else {
		player = sessionplayer.NewPlayer(os.Stdout, cfg.verboseEffective())
	}

	for i, path := range cfg.paths {
		// Detect format to validate the path
		format, err := sessionplayer.DetectFormat(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %s - %v, skipping\n", path, err)
			continue
		}

		if len(cfg.paths) > 1 {
			if i > 0 {
				fmt.Println() // Separator between sessions
			}
			fmt.Fprintf(os.Stdout, "=== Playing (%s): %s ===\n\n", format, path)
		}

		if err := player.PlayFormat(path, format); err != nil {
			fmt.Fprintf(os.Stderr, "Error playing %s: %v\n", path, err)
			os.Exit(1)
		}
	}
}

var errMissingPath = errors.New("no session path provided")

func defaultCLIConfig() cliConfig {
	return cliConfig{
		verbosity: "normal",
		color:     "auto",
	}
}

func newFlagSet(cfg *cliConfig) *flag.FlagSet {
	fs := flag.NewFlagSet("sessionplayer", flag.ContinueOnError)
	fs.BoolVar(&cfg.verbose, "verbose", false, "Verbose output (shorthand for --verbosity=verbose)")
	fs.StringVar(&cfg.verbosity, "verbosity", cfg.verbosity, "Output verbosity: quiet, normal, verbose, debug")
	fs.StringVar(&cfg.color, "color", cfg.color, "Color output: auto, always, never")
	return fs
}

func parseCLIArgs(args []string) (cliConfig, error) {
	cfg := defaultCLIConfig()
	fs := newFlagSet(&cfg)
	fs.SetOutput(io.Discard)
	err := parseWithFlagSet(fs, &cfg, args)
	return cfg, err
}

func parseWithFlagSet(fs *flag.FlagSet, cfg *cliConfig, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.paths = fs.Args()
	if len(cfg.paths) == 0 {
		return errMissingPath
	}
	return nil
}

func (cfg cliConfig) verboseEffective() bool {
	v := render.ParseVerbosity(cfg.verbosity)
	if cfg.verbose && v < render.VerbosityVerbose {
		v = render.VerbosityVerbose
	}
	return v >= render.VerbosityVerbose
}

func (cfg cliConfig) noColor() bool {
	return render.ParseColorMode(cfg.color) == render.ColorNever
}

func printUsage(w io.Writer, program string, fs *flag.FlagSet) {
	fmt.Fprintf(w, "Usage: %s [flags] <path>\n\n", program)
	fmt.Fprintln(w, "Play back recorded Claude or Codex session messages with colored output.")
	fmt.Fprintln(w, "\nThe path can be:")
	fmt.Fprintln(w, "  - A directory containing messages.jsonl (Claude format)")
	fmt.Fprintln(w, "  - A JSONL file (Codex format)")
	fmt.Fprintln(w, "\nFlags:")
	fs.SetOutput(w)
	fs.PrintDefaults()
	fmt.Fprintln(w, "\nExamples:")
	fmt.Fprintln(w, "  sessionplayer .planner-sessions/session-abc123-1234567890/")
	fmt.Fprintln(w, "  sessionplayer session.jsonl")
	fmt.Fprintln(w, "  sessionplayer -verbosity=verbose -color=never session.jsonl")
}
