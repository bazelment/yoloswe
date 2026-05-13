package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestParseCLIArgsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := parseCLIArgs([]string{"session.jsonl"})
	if err != nil {
		t.Fatalf("parseCLIArgs() error = %v", err)
	}
	if cfg.verbosity != "normal" || cfg.color != "auto" || cfg.verbose {
		t.Fatalf("default config = %+v", cfg)
	}
	if cfg.verboseEffective() {
		t.Fatal("default config should not be verbose")
	}
	if cfg.noColor() {
		t.Fatal("default config should not force no-color")
	}
	if strings.Join(cfg.paths, ",") != "session.jsonl" {
		t.Fatalf("paths = %v, want [session.jsonl]", cfg.paths)
	}
}

func TestParseCLIArgsOptions(t *testing.T) {
	t.Parallel()

	cfg, err := parseCLIArgs([]string{
		"-verbose",
		"-verbosity=quiet",
		"-color=never",
		"one.jsonl",
		"two.jsonl",
	})
	if err != nil {
		t.Fatalf("parseCLIArgs() error = %v", err)
	}
	if !cfg.verbose || cfg.verbosity != "quiet" || cfg.color != "never" {
		t.Fatalf("parsed config = %+v", cfg)
	}
	if !cfg.verboseEffective() {
		t.Fatal("-verbose should raise quiet verbosity to verbose output")
	}
	if !cfg.noColor() {
		t.Fatal("-color=never should force no-color")
	}
	if strings.Join(cfg.paths, ",") != "one.jsonl,two.jsonl" {
		t.Fatalf("paths = %v, want two paths", cfg.paths)
	}
}

func TestParseCLIArgsErrors(t *testing.T) {
	t.Parallel()

	_, err := parseCLIArgs(nil)
	if !errors.Is(err, errMissingPath) {
		t.Fatalf("parseCLIArgs(nil) error = %v, want errMissingPath", err)
	}

	_, err = parseCLIArgs([]string{"-unknown"})
	if err == nil {
		t.Fatal("parseCLIArgs(-unknown) error = nil, want flag error")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("parseCLIArgs(-unknown) error = %q", err.Error())
	}
}

func TestPrintUsage(t *testing.T) {
	t.Parallel()

	cfg := defaultCLIConfig()
	fs := newFlagSet(&cfg)
	var buf bytes.Buffer
	printUsage(&buf, "sessionplayer", fs)

	got := buf.String()
	for _, want := range []string{
		"Usage: sessionplayer",
		"directory containing messages.jsonl",
		"-verbosity=verbose -color=never",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("usage output missing %q:\n%s", want, got)
		}
	}
}
