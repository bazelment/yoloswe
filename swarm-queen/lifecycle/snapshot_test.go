package lifecycle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
)

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "-q", "--allow-empty", "-m", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := reconcile.ExecGit{}.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The whole point: uncommitted work is recoverable, and the lane's own git state
// is untouched so an agent mid-turn sees nothing change.
func TestSnapshotPreservesWorkWithoutTouchingLaneState(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	headBefore := git(t, dir, "rev-parse", "HEAD")
	statusBefore := git(t, dir, "status", "--porcelain")

	write(t, dir, "wip.txt", "unfinished work")

	res, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "lane-a", dir)
	if err != nil {
		t.Fatalf("SnapshotAtRisk: %v", err)
	}
	if res.Skipped || res.Commit == "" {
		t.Fatalf("expected a snapshot, got %+v", res)
	}

	// The lane's HEAD and working tree are exactly as they were.
	if got := git(t, dir, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD moved: %s -> %s", headBefore, got)
	}
	if got := git(t, dir, "status", "--porcelain"); got == statusBefore {
		t.Error("expected the file to still be uncommitted in the lane's own tree")
	}

	// The content is recoverable from the ref.
	body := git(t, dir, "show", BackupRef("lane-a")+":wip.txt")
	if body != "unfinished work" {
		t.Errorf("recovered %q, want %q", body, "unfinished work")
	}
}

// Untracked files are protected by no branch at all -- a worktree removal
// destroys them outright. One real reap nearly lost a 72KB untracked file.
func TestSnapshotCapturesUntrackedFiles(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	write(t, dir, "tests/live/_frozen.py", "never committed anywhere")

	res, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "replay", dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatalf("untracked work must be snapshotted: %+v", res)
	}
	body := git(t, dir, "show", BackupRef("replay")+":tests/live/_frozen.py")
	if body != "never committed anywhere" {
		t.Errorf("untracked file not recoverable: %q", body)
	}
}

// The guard is "has uncommitted work", NOT "has no commits". The original shell
// version skipped any lane with >=1 commit, switching the backup off exactly
// when the commit-early pressure started working.
func TestSnapshotStillRunsOnALaneThatHasCommitted(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	write(t, dir, "done.txt", "committed work")
	git(t, dir, "add", "done.txt")
	git(t, dir, "commit", "-q", "-m", "real work")

	write(t, dir, "wip.txt", "and more, uncommitted")

	res, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "lane-b", dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatal("a lane with commits AND uncommitted work must still be snapshotted")
	}
	if git(t, dir, "show", BackupRef("lane-b")+":wip.txt") != "and more, uncommitted" {
		t.Error("uncommitted work was not captured")
	}
}

func TestSnapshotSkipsCleanWorktree(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	res, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "clean", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || res.Commit != "" {
		t.Errorf("clean worktree must not create a ref: %+v", res)
	}
	if HasBackup(context.Background(), reconcile.ExecGit{}, dir, "clean") {
		t.Error("no ref should exist for a clean worktree")
	}
}

// Re-snapshotting unchanged work must not churn the ref.
func TestSnapshotIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	write(t, dir, "wip.txt", "same")

	first, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "lane-c", dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "lane-c", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Skipped {
		t.Errorf("second snapshot of identical content should skip: %+v", second)
	}
	if got := git(t, dir, "rev-parse", BackupRef("lane-c")); got != first.Commit {
		t.Errorf("ref moved on an idempotent snapshot: %s -> %s", first.Commit, got)
	}
}

// A changed file must produce a new snapshot.
func TestSnapshotFollowsChanges(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	write(t, dir, "wip.txt", "v1")
	first, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "lane-d", dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "wip.txt", "v2")
	second, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "lane-d", dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.Skipped || second.Commit == first.Commit {
		t.Errorf("changed content must produce a new snapshot: %+v", second)
	}
	if got := git(t, dir, "show", BackupRef("lane-d")+":wip.txt"); got != "v2" {
		t.Errorf("ref holds %q, want v2", got)
	}
}

func TestReleaseBackupIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	write(t, dir, "wip.txt", "x")
	if _, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "lane-e", dir); err != nil {
		t.Fatal(err)
	}
	if !HasBackup(context.Background(), reconcile.ExecGit{}, dir, "lane-e") {
		t.Fatal("expected a backup ref")
	}
	for range 2 {
		if err := ReleaseBackup(context.Background(), reconcile.ExecGit{}, dir, "lane-e"); err != nil {
			t.Fatalf("ReleaseBackup: %v", err)
		}
	}
	if HasBackup(context.Background(), reconcile.ExecGit{}, dir, "lane-e") {
		t.Error("ref should be gone")
	}
}

func TestSnapshotSkipsMissingWorktree(t *testing.T) {
	t.Parallel()
	res, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "ghost", "/nonexistent")
	if err != nil {
		t.Fatalf("a missing worktree is not an error: %v", err)
	}
	if !res.Skipped {
		t.Errorf("expected skip, got %+v", res)
	}
}
