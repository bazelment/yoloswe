package main

import (
	"errors"
	"io"
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

// TestRenderStatusColumnDegradesOnError covers the CLI render path where the
// original SIGSEGV lived: `wt ls` / `wt status` previously swallowed the error
// from GetStatus and dereferenced a nil *WorktreeStatus. renderStatusColumn
// must never panic on a nil status or a non-nil error and must surface the
// failure as "unknown" rather than rendering a broken worktree as "clean".
func TestRenderStatusColumnDegradesOnError(t *testing.T) {
	t.Parallel()

	output := wt.NewOutput(io.Discard, false) // colorized=false → bare strings

	tests := []struct {
		name   string
		status *wt.WorktreeStatus
		err    error
		want   string
	}{
		{name: "nil status with error", status: nil, err: errors.New("git boom"), want: "unknown"},
		{name: "nil status without error", status: nil, err: nil, want: "unknown"},
		{name: "non-nil status with error wins", status: &wt.WorktreeStatus{IsDirty: true}, err: errors.New("git boom"), want: "unknown"},
		{name: "clean", status: &wt.WorktreeStatus{}, err: nil, want: "clean"},
		{name: "dirty", status: &wt.WorktreeStatus{IsDirty: true}, err: nil, want: "dirty"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := renderStatusColumn(output, tt.status, tt.err) // must not panic
			if got != tt.want {
				t.Fatalf("renderStatusColumn() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRenderSyncColumn covers the `wt status` sync cell for a present status.
// The failed-GetStatus path is handled before renderSyncColumn is reached (the
// row is rendered entirely as "unknown" / "-"), so this exercises only the
// non-nil cases.
func TestRenderSyncColumn(t *testing.T) {
	t.Parallel()

	output := wt.NewOutput(io.Discard, false)

	tests := []struct {
		status *wt.WorktreeStatus
		name   string
		want   string
		w      wt.Worktree
	}{
		{name: "detached", status: &wt.WorktreeStatus{}, w: wt.Worktree{IsDetached: true}, want: "detached"},
		{name: "up to date", status: &wt.WorktreeStatus{}, w: wt.Worktree{}, want: "up to date"},
		{name: "ahead", status: &wt.WorktreeStatus{Ahead: 2}, w: wt.Worktree{}, want: "↑2"},
		{name: "behind", status: &wt.WorktreeStatus{Behind: 3}, w: wt.Worktree{}, want: "↓3"},
		{name: "ahead and behind", status: &wt.WorktreeStatus{Ahead: 1, Behind: 4}, w: wt.Worktree{}, want: "↑1 ↓4"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := renderSyncColumn(output, tt.status, tt.w); got != tt.want {
				t.Fatalf("renderSyncColumn() = %q, want %q", got, tt.want)
			}
		})
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
