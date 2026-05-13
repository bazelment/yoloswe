package main

import (
	"path/filepath"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/wt"
)

func TestToWTColorMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   render.ColorMode
		want wt.ColorMode
	}{
		{name: "auto", in: render.ColorAuto, want: wt.ColorAuto},
		{name: "always", in: render.ColorAlways, want: wt.ColorAlways},
		{name: "never", in: render.ColorNever, want: wt.ColorNever},
		{name: "unknown defaults to auto", in: render.ColorMode(99), want: wt.ColorAuto},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := toWTColorMode(tt.in); got != tt.want {
				t.Fatalf("toWTColorMode(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	if got := truncate("branch-name", 20); got != "branch-name" {
		t.Fatalf("truncate() changed short string to %q", got)
	}
	if got := truncate("branch-name", 6); got != "branch" {
		t.Fatalf("truncate() = %q, want branch", got)
	}
}

func TestShortenHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := filepath.Join(home, "repo", "branch")
	if got := shortenHome(path); got != "~/repo/branch" {
		t.Fatalf("shortenHome(%q) = %q, want ~/repo/branch", path, got)
	}

	outside := filepath.Join(t.TempDir(), "repo")
	if got := shortenHome(outside); got != outside {
		t.Fatalf("shortenHome(%q) = %q, want original path", outside, got)
	}
}
