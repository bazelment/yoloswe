package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePaths(t *testing.T) {
	restore := snapshotPathFlags()
	defer restore()

	root := filepath.Join(t.TempDir(), "repo")

	trackerPath = ""
	if got, want := resolveTrackerPath(root), filepath.Join(root, ".medivac", "issues.json"); got != want {
		t.Fatalf("resolveTrackerPath() = %q, want %q", got, want)
	}
	trackerPath = filepath.Join(t.TempDir(), "custom.json")
	if got := resolveTrackerPath(root); got != trackerPath {
		t.Fatalf("resolveTrackerPath() = %q, want explicit trackerPath %q", got, trackerPath)
	}

	sessionDir = ""
	if got, want := resolveSessionDir(root), filepath.Join(root, ".medivac", "sessions"); got != want {
		t.Fatalf("resolveSessionDir() = %q, want %q", got, want)
	}
	sessionDir = filepath.Join(t.TempDir(), "sessions")
	if got := resolveSessionDir(root); got != sessionDir {
		t.Fatalf("resolveSessionDir() = %q, want explicit sessionDir %q", got, sessionDir)
	}
}

func TestResolveRepoRoot(t *testing.T) {
	restore := snapshotPathFlags()
	defer restore()

	repoRoot = "/tmp/repo"
	if got, err := resolveRepoRoot(); err != nil || got != repoRoot {
		t.Fatalf("resolveRepoRoot(explicit) = (%q, %v), want %q nil", got, err, repoRoot)
	}
}

func TestResolveWTRoot(t *testing.T) {
	t.Parallel()

	wtRoot := t.TempDir()
	repo := filepath.Join(wtRoot, "project")
	nested := filepath.Join(repo, "main", "subdir")
	if err := os.MkdirAll(filepath.Join(repo, ".bare"), 0o755); err != nil {
		t.Fatalf("create .bare: %v", err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("create nested dir: %v", err)
	}

	gotRoot, gotRepo, err := resolveWTRoot(nested)
	if err != nil {
		t.Fatalf("resolveWTRoot() error = %v", err)
	}
	if gotRoot != wtRoot || gotRepo != "project" {
		t.Fatalf("resolveWTRoot() = (%q, %q), want (%q, project)", gotRoot, gotRepo, wtRoot)
	}
}

func TestResolveWTRootFallback(t *testing.T) {
	t.Parallel()

	repo := filepath.Join(t.TempDir(), "plain-repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("create repo dir: %v", err)
	}

	gotRoot, gotRepo, err := resolveWTRoot(repo)
	if err != nil {
		t.Fatalf("resolveWTRoot() error = %v", err)
	}
	if gotRoot != filepath.Dir(repo) || gotRepo != filepath.Base(repo) {
		t.Fatalf("resolveWTRoot() fallback = (%q, %q), want (%q, %q)",
			gotRoot, gotRepo, filepath.Dir(repo), filepath.Base(repo))
	}
}

func TestTruncateSummary(t *testing.T) {
	t.Parallel()

	if got := truncateSummary("short", 10); got != "short" {
		t.Fatalf("truncateSummary(short) = %q", got)
	}
	if got := truncateSummary("abcdefghijklmnopqrstuvwxyz", 10); got != "abcdefg..." {
		t.Fatalf("truncateSummary(long) = %q, want abcdefg...", got)
	}
	if got := truncateSummary("abcdef", 2); got != "..." {
		t.Fatalf("truncateSummary(max<3) = %q, want ...", got)
	}
}

func snapshotPathFlags() func() {
	oldRepoRoot := repoRoot
	oldTrackerPath := trackerPath
	oldSessionDir := sessionDir

	return func() {
		repoRoot = oldRepoRoot
		trackerPath = oldTrackerPath
		sessionDir = oldSessionDir
	}
}
