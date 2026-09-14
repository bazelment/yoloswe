package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// GitRunner executes git commands. Injectable so probes are testable without a
// real repository.
type GitRunner interface {
	Run(ctx context.Context, dir string, args ...string) (string, error)
}

// EnvGitRunner executes a git command with additional environment variables.
// Snapshotting requires this capability so it never falls back to the lane's
// real index when using a temporary one.
type EnvGitRunner interface {
	GitRunner
	RunWithEnv(ctx context.Context, dir string, env []string, args ...string) (string, error)
}

// ExecGit runs the real git binary.
type ExecGit struct{}

// Run executes git in dir and returns trimmed stdout.
func (ExecGit) Run(ctx context.Context, dir string, args ...string) (string, error) {
	return ExecGit{}.RunWithEnv(ctx, dir, nil, args...)
}

// RunWithEnv executes git with extra environment variables.
func (ExecGit) RunWithEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// WorktreeState is what git actually reports about a lane's worktree.
//
// Every field here is measured. None of it is a lane's self-report.
type WorktreeState struct {
	Path             string
	Head             string
	CommitsSinceFork int
	DirtyCount       int
	Exists           bool
	HasUntracked     bool
	// Measured reports that every probe git command actually ran. False means
	// the fields below Path are UNKNOWN, not zero.
	//
	// This bit exists because the zero value of this struct is indistinguishable
	// from a real measurement of a clean worktree, and Exists is set BEFORE the
	// first git command runs -- so a failed `status` or `rev-list` returns
	// Exists=true, DirtyCount=0, which reads as "present and clean" and is
	// exactly the state that permits a destructive reap. Four callers shared one
	// copy-pasted idiom that discarded the error; only one acted on it. A field
	// every caller must pass through is harder to drop than an error every
	// caller must remember to check.
	Measured bool
}

// Clean reports whether the worktree has no uncommitted changes.
//
// An unmeasured worktree is never clean: "we could not look" must not answer
// the question "is there anything here to lose".
func (w WorktreeState) Clean() bool { return w.Measured && w.Exists && w.DirtyCount == 0 }

// Unknown reports that the probe could not complete, so nothing below Path can
// be trusted. Destructive paths must refuse on this.
func (w WorktreeState) Unknown() bool { return !w.Measured }

// Measure marks a hand-built WorktreeState as measured.
//
// For tests and for callers constructing a state from evidence they gathered
// themselves. Named rather than a bare field so that the unmeasured zero value
// stays the default and reaching for this is a deliberate claim.
func (w WorktreeState) Measure() WorktreeState {
	w.Measured = true
	return w
}

// ErrNoWorktree means the recorded path is not a directory.
var ErrNoWorktree = errors.New("worktree does not exist")

// ProbeWorktree measures a lane's worktree against its fork point.
//
// forkRef may be a SHA or a ref. When empty, commit counting is skipped rather
// than silently compared against a moving target: counting against a branch that
// other lanes also advance produces a number that means nothing.
func ProbeWorktree(ctx context.Context, g GitRunner, path, forkRef string) (WorktreeState, error) {
	st := WorktreeState{Path: path}
	if path == "" {
		// No path recorded is a complete answer: there is no worktree.
		st.Measured = true
		return st, ErrNoWorktree
	}
	fi, err := os.Stat(path)
	switch {
	case err == nil && !fi.IsDir():
		st.Measured = true
		return st, ErrNoWorktree
	case errors.Is(err, os.ErrNotExist):
		// Genuinely absent, which is measured evidence rather than an unknown:
		// there is nothing to destroy and nothing left to look at.
		st.Measured = true
		return st, ErrNoWorktree
	case err != nil:
		// Anything else (a permission error, an unreadable mount) means we could
		// not look. That is NOT evidence that the worktree is gone.
		return st, fmt.Errorf("stat worktree %s: %w", path, err)
	}
	st.Exists = true

	head, err := g.Run(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return st, fmt.Errorf("resolve HEAD: %w", err)
	}
	st.Head = head

	porcelain, err := g.Run(ctx, path, "status", "--porcelain")
	if err != nil {
		return st, fmt.Errorf("status: %w", err)
	}
	for _, line := range strings.Split(porcelain, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		st.DirtyCount++
		if strings.HasPrefix(line, "??") {
			st.HasUntracked = true
		}
	}

	if forkRef != "" {
		count, err := g.Run(ctx, path, "rev-list", "--count", forkRef+"..HEAD")
		if err != nil {
			// An unresolvable fork point is not "zero commits" -- that would
			// read as "phase did nothing" and could fail a lane that worked.
			return st, fmt.Errorf("count commits since %s: %w", forkRef, err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(count))
		if err != nil {
			return st, fmt.Errorf("parse commit count %q: %w", count, err)
		}
		st.CommitsSinceFork = n
	}
	st.Measured = true
	return st, nil
}

// BranchMerged reports whether branch's content is present on target.
//
// Ancestry alone is insufficient: branches are squash-merged, which rewrites
// history, so `git branch -d` refuses and ancestry reports "not merged" for work
// that actually landed. The authoritative question is whether the branch still
// carries content the target lacks.
//
// Use a TWO-dot diff. Three-dot diffs against the merge-base, which predates the
// squash commit, so it still reports the branch's changes and a squash-merged
// branch reads as unmerged -- the exact bug this function exists to avoid. Two
// dots compares the tips, which is the question actually being asked.
func BranchMerged(ctx context.Context, g GitRunner, repoDir, branch, target string) (bool, error) {
	if branch == "" || target == "" {
		return false, fmt.Errorf("branch and target are both required")
	}
	// Ancestry is the cheap, unambiguous case.
	if _, err := g.Run(ctx, repoDir, "merge-base", "--is-ancestor", branch, target); err == nil {
		return true, nil
	}
	// Otherwise ask whether the branch still carries unique content.
	diff, err := g.Run(ctx, repoDir, "diff", "--stat", target+".."+branch)
	if err != nil {
		return false, fmt.Errorf("diff %s..%s: %w", target, branch, err)
	}
	return strings.TrimSpace(diff) == "", nil
}

// envGit runs git with extra environment variables.
type envGit struct {
	inner GitRunner
	env   []string
}

// WithEnv returns a GitRunner that adds env vars to every invocation. Used for
// GIT_INDEX_FILE, so a snapshot can stage into a throwaway index without
// touching the lane's own.
func WithEnv(g GitRunner, env ...string) GitRunner { return envGit{inner: g, env: env} }

func (e envGit) Run(ctx context.Context, dir string, args ...string) (string, error) {
	runner, ok := e.inner.(EnvGitRunner)
	if !ok {
		return "", fmt.Errorf("git runner does not support environment variables")
	}
	return runner.RunWithEnv(ctx, dir, e.env, args...)
}
