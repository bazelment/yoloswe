// Package lifecycle owns the destructive half of a swarm: snapshotting work at
// risk, and reaping resources once they are provably closed.
//
// Everything here is written on the assumption that the other party will not
// act. The skill states the principle outright in snapshot_at_risk.sh: "When a
// control depends on the other party acting, add one that does not." Briefing a
// lane to commit early is not protection, because a nudge is only read when the
// lane's turn ENDS, and the risk window is the middle of a long turn.
package lifecycle

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
)

// BackupRefPrefix is where snapshots live. Ordinary git refs, so the work is
// recoverable with ordinary git and survives a session death.
const BackupRefPrefix = "refs/backup/"

// BackupRef returns the ref name for a lane's snapshot.
func BackupRef(lane string) string { return BackupRefPrefix + lane }

// SnapshotResult reports what a snapshot did.
type SnapshotResult struct {
	Lane    string
	Ref     string
	Commit  string
	Reason  string
	Files   int
	Skipped bool
}

// SnapshotAtRisk backs up a worktree's uncommitted work without touching the
// lane's index or HEAD.
//
// It writes through a temporary index and git plumbing, so an agent mid-turn
// never finds its git state changed underneath it. Guard on "has uncommitted
// work", never on "has no commits": the original shell version skipped any lane
// with >=1 commit, which switched the backup off at exactly the moment the
// commit-early pressure started working.
func SnapshotAtRisk(ctx context.Context, g reconcile.GitRunner, lane, worktree string) (SnapshotResult, error) {
	res := SnapshotResult{Lane: lane, Ref: BackupRef(lane)}

	if fi, err := os.Stat(worktree); err != nil || !fi.IsDir() {
		res.Skipped, res.Reason = true, "worktree does not exist"
		return res, nil
	}

	porcelain, err := g.Run(ctx, worktree, "status", "--porcelain")
	if err != nil {
		return res, fmt.Errorf("status: %w", err)
	}
	if strings.TrimSpace(porcelain) == "" {
		res.Skipped, res.Reason = true, "worktree is clean"
		return res, nil
	}

	// A temporary index keeps the lane's own index untouched.
	tmpIndex, err := os.CreateTemp("", "swarm-queen-index-*")
	if err != nil {
		return res, err
	}
	idxPath := tmpIndex.Name()
	tmpIndex.Close()
	os.Remove(idxPath) // git wants to create it itself
	defer os.Remove(idxPath)

	env := reconcile.WithEnv(g, "GIT_INDEX_FILE="+idxPath)

	if _, err := env.Run(ctx, worktree, "read-tree", "HEAD"); err != nil {
		return res, fmt.Errorf("read-tree: %w", err)
	}
	if _, err := env.Run(ctx, worktree, "add", "-A"); err != nil {
		return res, fmt.Errorf("stage into temp index: %w", err)
	}
	tree, err := env.Run(ctx, worktree, "write-tree")
	if err != nil {
		return res, fmt.Errorf("write-tree: %w", err)
	}
	changed, err := env.Run(ctx, worktree, "diff", "--cached", "--name-only", "HEAD")
	if err != nil {
		return res, fmt.Errorf("diff temp index: %w", err)
	}
	res.Files = len(nonEmptyLines(changed))

	// Nothing new since the last snapshot: same tree, so re-committing would add
	// a ref with identical content.
	if prev, err := g.Run(ctx, worktree, "rev-parse", "-q", "--verify", res.Ref); err == nil && prev != "" {
		if prevTree, err := g.Run(ctx, worktree, "rev-parse", prev+"^{tree}"); err == nil && prevTree == tree {
			res.Skipped, res.Reason = true, "no change since the last snapshot"
			return res, nil
		}
	}

	msg := fmt.Sprintf(`backup(%s): swarm-queen snapshot of uncommitted work

Written via git plumbing while the session was mid-turn. The session's own index
and HEAD were NOT touched.`, lane)

	commit, err := g.Run(ctx, worktree, "commit-tree", tree, "-p", "HEAD", "-m", msg)
	if err != nil {
		return res, fmt.Errorf("commit-tree: %w", err)
	}
	if _, err := g.Run(ctx, worktree, "update-ref", res.Ref, commit); err != nil {
		return res, fmt.Errorf("update-ref %s: %w", res.Ref, err)
	}
	res.Commit = commit
	return res, nil
}

// ReleaseBackup drops a lane's snapshot ref.
//
// Only safe once the lane is committed AND clean, or merged. A leaked ref costs
// nothing operationally, which is exactly why releasing it is the cleanup step
// that gets skipped -- and why it needs a mechanical check rather than a habit.
func ReleaseBackup(ctx context.Context, g reconcile.GitRunner, repoDir, lane string) error {
	ref := BackupRef(lane)
	if _, err := g.Run(ctx, repoDir, "rev-parse", "-q", "--verify", ref); err != nil {
		return nil // already gone
	}
	if _, err := g.Run(ctx, repoDir, "update-ref", "-d", ref); err != nil {
		return fmt.Errorf("release %s: %w", ref, err)
	}
	return nil
}

// HasBackup reports whether a lane's snapshot ref exists.
func HasBackup(ctx context.Context, g reconcile.GitRunner, repoDir, lane string) bool {
	out, err := g.Run(ctx, repoDir, "rev-parse", "-q", "--verify", BackupRef(lane))
	return err == nil && strings.TrimSpace(out) != ""
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
