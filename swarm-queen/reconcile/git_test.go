package reconcile

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newRepo builds a real git repository. These probes exist to encode git's
// actual behaviour -- especially where it is counterintuitive -- so faking git
// would test only my assumptions about it.
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
	out, err := ExecGit{}.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProbeWorktreeCountsCommitsSinceFork(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	fork := git(t, dir, "rev-parse", "HEAD")

	st, err := ProbeWorktree(context.Background(), ExecGit{}, dir, fork)
	if err != nil {
		t.Fatal(err)
	}
	if st.CommitsSinceFork != 0 || !st.Clean() {
		t.Errorf("fresh worktree: %+v", st)
	}

	write(t, dir, "a.txt", "hello")
	git(t, dir, "add", "a.txt")
	git(t, dir, "commit", "-q", "-m", "work")

	st, err = ProbeWorktree(context.Background(), ExecGit{}, dir, fork)
	if err != nil {
		t.Fatal(err)
	}
	if st.CommitsSinceFork != 1 {
		t.Errorf("CommitsSinceFork = %d, want 1", st.CommitsSinceFork)
	}
	if !st.Clean() {
		t.Errorf("expected clean after commit: %+v", st)
	}
}

// An empty branch behind a .done file is the single most dangerous claim in the
// system: merging on it ships nothing while reporting success.
func TestEmptyBranchIsDetectable(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	fork := git(t, dir, "rev-parse", "HEAD")

	st, err := ProbeWorktree(context.Background(), ExecGit{}, dir, fork)
	if err != nil {
		t.Fatal(err)
	}
	if st.CommitsSinceFork != 0 {
		t.Fatalf("expected an empty branch, got %d commits", st.CommitsSinceFork)
	}
}

// --porcelain collapses an untracked DIRECTORY to one line, so DirtyCount is a
// presence check and must never be reported as a file count. Untracked work is
// also protected by no branch, so it must be flagged before any reap.
func TestDirtyCountIsPresenceNotFileCount(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"one", "two", "three"} {
		write(t, dir, filepath.Join("scratch", n), n)
	}

	st, err := ProbeWorktree(context.Background(), ExecGit{}, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.DirtyCount != 1 {
		t.Errorf("DirtyCount = %d; git collapses the untracked dir to one line, "+
			"so this is a presence check only", st.DirtyCount)
	}
	if !st.HasUntracked {
		t.Error("HasUntracked must be true: a worktree removal would destroy this work")
	}
	if st.Clean() {
		t.Error("worktree with untracked files must not read as clean")
	}
}

func TestProbeWorktreeMissingDirectory(t *testing.T) {
	t.Parallel()
	_, err := ProbeWorktree(context.Background(), ExecGit{}, "/nonexistent/lane", "")
	if !errors.Is(err, ErrNoWorktree) {
		t.Errorf("err = %v, want ErrNoWorktree", err)
	}
}

// An unresolvable fork point must be an error, never a silent zero -- zero would
// read as "this phase did nothing" and could fail a lane that worked.
func TestUnresolvableForkIsAnErrorNotZero(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	st, err := ProbeWorktree(context.Background(), ExecGit{}, dir, "does-not-exist")
	if err == nil {
		t.Fatalf("expected an error, got %+v", st)
	}
	if st.CommitsSinceFork != 0 {
		t.Errorf("partial state leaked a commit count: %+v", st)
	}
}

// Branches are SQUASH-merged, which rewrites history: the squashed commit is not
// an ancestor of the branch, so ancestry reports "not merged" for work that
// actually landed. Gate on content instead.
func TestBranchMergedHandlesSquashMerge(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)

	git(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "f.txt", "feature work")
	git(t, dir, "add", "f.txt")
	git(t, dir, "commit", "-q", "-m", "feature work")

	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--squash", "feature")
	git(t, dir, "commit", "-q", "-m", "squashed feature")

	// Ancestry says no...
	_, ancErr := ExecGit{}.Run(context.Background(), dir,
		"merge-base", "--is-ancestor", "feature", "main")
	if ancErr == nil {
		t.Skip("git reports ancestry for a squash merge; the premise no longer holds")
	}

	// ...but the content landed, so the branch is merged.
	merged, err := BranchMerged(context.Background(), ExecGit{}, dir, "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !merged {
		t.Error("squash-merged branch reported as unmerged; deleting on ancestry alone " +
			"would strand it, and refusing to delete leaks a worktree forever")
	}
}

func TestBranchMergedRejectsUnmergedWork(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "f.txt", "unmerged")
	git(t, dir, "add", "f.txt")
	git(t, dir, "commit", "-q", "-m", "unmerged work")
	git(t, dir, "checkout", "-q", "main")

	merged, err := BranchMerged(context.Background(), ExecGit{}, dir, "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	if merged {
		t.Error("unmerged branch reported as merged — reaping it would destroy work")
	}
}

func TestBranchMergedDetectsPlainAncestry(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "f.txt", "work")
	git(t, dir, "add", "f.txt")
	git(t, dir, "commit", "-q", "-m", "work")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge", "feature")

	merged, err := BranchMerged(context.Background(), ExecGit{}, dir, "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !merged {
		t.Error("fast-forward/no-ff merged branch reported as unmerged")
	}
}
