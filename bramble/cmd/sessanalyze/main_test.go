package main

import (
	"strings"
	"testing"
)

func TestParseFlagsDefaults(t *testing.T) {
	t.Parallel()

	cfg := parseFlags([]string{"session.jsonl"})

	if cfg.summaryWordLimit != 100 || cfg.statsMaxRows != 25 || cfg.concurrency != 10 {
		t.Fatalf("default numeric config = %+v", cfg)
	}
	if cfg.modelStr != "haiku" {
		t.Fatalf("default model = %q, want haiku", cfg.modelStr)
	}
	if cfg.jsonOutput || cfg.verbose || cfg.listProjects || cfg.allProjects || cfg.summarize || cfg.stats || cfg.topLevelOnly {
		t.Fatalf("default boolean config = %+v", cfg)
	}
	if strings.Join(cfg.paths, ",") != "session.jsonl" {
		t.Fatalf("paths = %v, want [session.jsonl]", cfg.paths)
	}
}

func TestParseFlagsOptions(t *testing.T) {
	t.Parallel()

	cfg := parseFlags([]string{
		"--summary-limit", "50",
		"--json",
		"-v",
		"--list",
		"--since", "2d",
		"--until", "2026-04-23T12:00:00Z",
		"--all",
		"--summarize",
		"--model", "gemini",
		"--pricing-file", "pricing.json",
		"-n", "3",
		"--max-rows", "7",
		"--min-turns", "2",
		"-j", "4",
		"--stats",
		"--top-level-only",
		"one.jsonl",
		"two.jsonl",
	})

	if cfg.summaryWordLimit != 50 || cfg.limit != 3 || cfg.statsMaxRows != 7 || cfg.minTurns != 2 || cfg.concurrency != 4 {
		t.Fatalf("numeric config = %+v", cfg)
	}
	if cfg.sinceStr != "2d" || cfg.untilStr != "2026-04-23T12:00:00Z" || cfg.modelStr != "gemini" || cfg.pricingFile != "pricing.json" {
		t.Fatalf("string config = %+v", cfg)
	}
	if !cfg.jsonOutput || !cfg.verbose || !cfg.listProjects || !cfg.allProjects || !cfg.summarize || !cfg.stats || !cfg.topLevelOnly {
		t.Fatalf("boolean config = %+v", cfg)
	}
	if strings.Join(cfg.paths, ",") != "one.jsonl,two.jsonl" {
		t.Fatalf("paths = %v, want two paths", cfg.paths)
	}
}

func TestParseFlagsDoesNotRetainPreviousState(t *testing.T) {
	t.Parallel()

	first := parseFlags([]string{"--json", "--since", "24h", "first.jsonl"})
	second := parseFlags([]string{"second.jsonl"})

	if !first.jsonOutput || first.sinceStr != "24h" {
		t.Fatalf("first parse = %+v", first)
	}
	if second.jsonOutput || second.sinceStr != "" {
		t.Fatalf("second parse retained previous state: %+v", second)
	}
	if strings.Join(second.paths, ",") != "second.jsonl" {
		t.Fatalf("second paths = %v, want [second.jsonl]", second.paths)
	}
}
