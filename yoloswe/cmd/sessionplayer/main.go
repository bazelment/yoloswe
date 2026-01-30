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
	"flag"
	"fmt"
	"os"

	"github.com/bazelment/yoloswe/yoloswe/sessionplayer"
)

func main() {
	verbose := flag.Bool("verbose", false, "Show all output (tool results, item events)")
	noColor := flag.Bool("no-color", false, "Disable ANSI color codes")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <path>\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Play back recorded Claude or Codex session messages with colored output.")
		fmt.Fprintln(os.Stderr, "\nThe path can be:")
		fmt.Fprintln(os.Stderr, "  - A directory containing messages.jsonl (Claude format)")
		fmt.Fprintln(os.Stderr, "  - A JSONL file (Codex format)")
		fmt.Fprintln(os.Stderr, "\nFlags:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\nExamples:")
		fmt.Fprintln(os.Stderr, "  sessionplayer .planner-sessions/session-abc123-1234567890/")
		fmt.Fprintln(os.Stderr, "  sessionplayer session.jsonl")
		fmt.Fprintln(os.Stderr, "  sessionplayer -verbose -no-color session.jsonl")
	}
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Error: no session path provided")
		flag.Usage()
		os.Exit(1)
	}

	// Handle multiple paths (shell glob expansion)
	paths := flag.Args()

	var player *sessionplayer.Player
	if *noColor {
		player = sessionplayer.NewPlayerWithOptions(os.Stdout, *verbose, true)
	} else {
		player = sessionplayer.NewPlayer(os.Stdout, *verbose)
	}

	for i, path := range paths {
		// Detect format to validate the path
		format, err := sessionplayer.DetectFormat(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %s - %v, skipping\n", path, err)
			continue
		}

		if len(paths) > 1 {
			if i > 0 {
				fmt.Println() // Separator between sessions
			}
			fmt.Fprintf(os.Stdout, "=== Playing (%s): %s ===\n\n", format, path)
		}

		if err := player.Play(path); err != nil {
			fmt.Fprintf(os.Stderr, "Error playing %s: %v\n", path, err)
			os.Exit(1)
		}
	}
}
