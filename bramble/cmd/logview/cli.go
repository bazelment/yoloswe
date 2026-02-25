package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

type cliConfig struct {
	paths          []string
	width          int
	height         int
	enableMarkdown bool
	compact        bool
	debug          bool
}

func usage(binary string) string {
	return fmt.Sprintf(
		"Usage: %s [--width N] [--height N] [--markdown=true|false] [--compact=true|false] [--debug] <log1.jsonl> [log2.jsonl ...]",
		binary,
	)
}

func parseCLIArgs(args []string) (cliConfig, error) {
	cfg := cliConfig{
		width:          120,
		height:         30,
		enableMarkdown: true,
		compact:        true,
	}

	fs := flag.NewFlagSet("logview", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.IntVar(&cfg.width, "width", cfg.width, "render width")
	fs.IntVar(&cfg.height, "height", cfg.height, "render height")
	fs.BoolVar(&cfg.enableMarkdown, "markdown", cfg.enableMarkdown, "enable markdown rendering")
	fs.BoolVar(&cfg.compact, "compact", cfg.compact, "compact replay output")
	fs.BoolVar(&cfg.debug, "debug", false, "show raw output lines for debugging")

	plain := fs.Bool("plain", false, "alias for --markdown=false")
	full := fs.Bool("full", false, "alias for --compact=false")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	if *plain {
		cfg.enableMarkdown = false
	}
	if *full {
		cfg.compact = false
	}

	if cfg.width <= 0 {
		return cfg, errors.New("--width must be > 0")
	}
	if cfg.height <= 0 {
		return cfg, errors.New("--height must be > 0")
	}

	cfg.paths = fs.Args()
	if len(cfg.paths) == 0 {
		return cfg, errors.New("missing log file path")
	}
	return cfg, nil
}
