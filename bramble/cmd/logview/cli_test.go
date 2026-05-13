package main

import (
	"strings"
	"testing"
)

func TestParseCLIArgs(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()

		cfg, err := parseCLIArgs([]string{"session.jsonl"})
		if err != nil {
			t.Fatalf("parseCLIArgs() error = %v", err)
		}
		if cfg.width != 120 || cfg.height != 30 || !cfg.enableMarkdown || !cfg.compact || cfg.debug {
			t.Fatalf("default config = %+v", cfg)
		}
		if len(cfg.paths) != 1 || cfg.paths[0] != "session.jsonl" {
			t.Fatalf("paths = %v, want [session.jsonl]", cfg.paths)
		}
	})

	t.Run("explicit flags and aliases", func(t *testing.T) {
		t.Parallel()

		cfg, err := parseCLIArgs([]string{
			"--width", "80",
			"--height", "20",
			"--plain",
			"--full",
			"--debug",
			"one.jsonl",
			"two.jsonl",
		})
		if err != nil {
			t.Fatalf("parseCLIArgs() error = %v", err)
		}
		if cfg.width != 80 || cfg.height != 20 || cfg.enableMarkdown || cfg.compact || !cfg.debug {
			t.Fatalf("parsed config = %+v", cfg)
		}
		if strings.Join(cfg.paths, ",") != "one.jsonl,two.jsonl" {
			t.Fatalf("paths = %v", cfg.paths)
		}
	})

	t.Run("explicit booleans", func(t *testing.T) {
		t.Parallel()

		cfg, err := parseCLIArgs([]string{
			"--markdown=false",
			"--compact=false",
			"session.jsonl",
		})
		if err != nil {
			t.Fatalf("parseCLIArgs() error = %v", err)
		}
		if cfg.enableMarkdown || cfg.compact {
			t.Fatalf("parsed booleans = markdown:%v compact:%v, want false false", cfg.enableMarkdown, cfg.compact)
		}
	})
}

func TestParseCLIArgsErrors(t *testing.T) {
	t.Parallel()

	requireParseError(t, nil, "missing log file path")
	requireParseError(t, []string{"--width", "0", "session.jsonl"}, "--width must be > 0")
	requireParseError(t, []string{"--height", "-1", "session.jsonl"}, "--height must be > 0")
	requireParseError(t, []string{"--nope"}, "flag provided but not defined")
}

func TestUsage(t *testing.T) {
	t.Parallel()

	got := usage("logview")
	for _, want := range []string{"Usage: logview", "--width N", "<log1.jsonl>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("usage() = %q, want substring %q", got, want)
		}
	}
}

func TestTruncateDebug(t *testing.T) {
	t.Parallel()

	if got := truncateDebug("hello\nworld", 20); got != `hello\nworld` {
		t.Fatalf("truncateDebug newline = %q", got)
	}
	if got := truncateDebug("abcdefghijklmnopqrstuvwxyz", 10); got != "abcdefg..." {
		t.Fatalf("truncateDebug long string = %q", got)
	}
}

func requireParseError(t *testing.T, args []string, want string) {
	t.Helper()

	_, err := parseCLIArgs(args)
	if err == nil {
		t.Fatalf("parseCLIArgs(%v) error = nil, want substring %q", args, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("parseCLIArgs(%v) error = %q, want substring %q", args, err.Error(), want)
	}
}
