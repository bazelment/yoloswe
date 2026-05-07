#!/usr/bin/env python3
"""
Sync current git branch with remote base branch.

Handles both regular git repositories and git worktrees. Fetches latest
changes, rebases the current branch onto the remote base branch, and
(when a PR exists) force-pushes with a precise lease.

Concurrency-safe: multiple worktrees sharing the same bare repo can run
this script simultaneously. See docstring at top of main() for the design.

On conflicts, prints a structured conflict report and exits with code 2.
The calling agent is responsible for resolving conflicts and re-running
with --continue.

Exit codes:
    0 = success (rebase complete, verification printed, pushed if PR exists)
    1 = error (not a repo, dirty tree, detached HEAD, lock held, etc.)
    2 = conflict (rebase paused, conflict report printed)

Usage:
    python3 git-sync.py --verbose
    python3 git-sync.py --backup
    python3 git-sync.py --continue --verbose
    python3 git-sync.py --base develop
    python3 git-sync.py --no-push          # skip the post-rebase force-push
"""

import argparse
import fcntl
import os
import shutil
import subprocess
import sys
from datetime import datetime
from pathlib import Path

EXIT_SUCCESS = 0
EXIT_FAILURE = 1
EXIT_CONFLICT = 2

PINNED_TARGET_FILE = "sync-base.pinned-target"
LOCK_FILE = "sync-base.lock"

# Conflict type labels from git status porcelain v2 XY codes
_CONFLICT_TYPES = {
    "DD": "both_deleted",
    "AU": "added_by_us",
    "UD": "deleted_by_them",
    "UA": "added_by_them",
    "DU": "deleted_by_us",
    "AA": "both_added",
    "UU": "both_modified",
}


class GitError(Exception):
    """Git command failed."""


def run_git(*args: str, check: bool = True, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    """Run a git command and return the result."""
    run_env = None
    if env:
        run_env = {**os.environ, **env}
    result = subprocess.run(
        ["git", *args],
        capture_output=True,
        text=True,
        env=run_env,
    )
    if check and result.returncode != 0:
        raise GitError(result.stderr.strip() or result.stdout.strip())
    return result


def get_git_output(*args: str) -> str:
    """Run a git command and return stdout."""
    return run_git(*args).stdout.strip()


def get_git_dir() -> Path:
    """Per-worktree .git dir (not the common dir)."""
    return Path(get_git_output("rev-parse", "--git-dir")).resolve()


def is_worktree() -> bool:
    """Check if current directory is a git worktree (not the main repo)."""
    git_dir = get_git_output("rev-parse", "--git-dir")
    git_common = get_git_output("rev-parse", "--git-common-dir")
    return git_dir != git_common


def get_worktree_label() -> str:
    """A short label identifying this worktree, for namespacing refs/logs.

    Uses the top-level directory basename since that's what the user sees.
    """
    top = get_git_output("rev-parse", "--show-toplevel")
    return Path(top).name or "root"


def get_current_branch() -> str:
    """Get the name of the current branch.

    During a rebase, HEAD is detached, so `rev-parse --abbrev-ref` returns
    "HEAD". In that case, read the branch name from the rebase state files.
    """
    name = get_git_output("rev-parse", "--abbrev-ref", "HEAD")
    if name != "HEAD":
        return name

    # Detached: check for rebase state
    for subpath in ("rebase-merge/head-name", "rebase-apply/head-name"):
        p = Path(get_git_output("rev-parse", "--git-path", subpath))
        if p.exists():
            head_name = p.read_text().strip()
            return head_name.removeprefix("refs/heads/")
    return "HEAD"


def has_uncommitted_changes() -> bool:
    """Check for staged or unstaged changes."""
    status = get_git_output("status", "--porcelain")
    return bool(status)


def is_rebase_in_progress() -> bool:
    """Check if a rebase is currently in progress."""
    rebase_merge = get_git_output("rev-parse", "--git-path", "rebase-merge")
    rebase_apply = get_git_output("rev-parse", "--git-path", "rebase-apply")
    return os.path.exists(rebase_merge) or os.path.exists(rebase_apply)


def acquire_worktree_lock(git_dir: Path, verbose: bool):
    """Take a non-blocking exclusive flock on $GIT_DIR/sync-base.lock.

    Scoped to this worktree (per-worktree $GIT_DIR), so other worktrees
    are unaffected. Prevents two concurrent sync-base runs in the SAME
    worktree from stomping each other.

    Returns the open file handle. Caller must keep it alive for the lock
    to persist; it is released on process exit.
    """
    lock_path = git_dir / LOCK_FILE
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    fh = open(lock_path, "w")
    try:
        fcntl.flock(fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError as e:
        fh.close()
        raise GitError(
            f"Another git-sync is already running in this worktree "
            f"(lock held on {lock_path}). Wait for it to finish or remove the lock file."
        ) from e
    if verbose:
        print(f"Acquired worktree lock: {lock_path}")
    fh.write(f"pid={os.getpid()} started={datetime.now().isoformat()}\n")
    fh.flush()
    return fh


def create_backup_branch(branch: str, worktree_label: str, verbose: bool) -> str:
    """Create a timestamped backup branch, namespaced by worktree."""
    timestamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    backup_name = f"{branch}-backup-{worktree_label}-{timestamp}"
    if verbose:
        print(f"Creating backup branch: {backup_name}")
    run_git("branch", backup_name)
    return backup_name


def detect_base_branch(verbose: bool) -> str:
    """Auto-detect the remote default branch via origin/HEAD.

    Falls back to 'main' if origin/HEAD is not set. Only calls
    `remote set-head --auto` (which mutates shared state) when
    origin/HEAD is genuinely missing.
    """
    result = run_git("symbolic-ref", "refs/remotes/origin/HEAD", check=False)
    if result.returncode == 0:
        ref = result.stdout.strip()
        branch = ref.removeprefix("refs/remotes/origin/")
        if verbose:
            print(f"Auto-detected base branch: {branch}")
        return branch

    if verbose:
        print("origin/HEAD not set, attempting auto-detection...")
    set_result = run_git("remote", "set-head", "origin", "--auto", check=False)
    if set_result.returncode == 0:
        result = run_git("symbolic-ref", "refs/remotes/origin/HEAD", check=False)
        if result.returncode == 0:
            ref = result.stdout.strip()
            branch = ref.removeprefix("refs/remotes/origin/")
            if verbose:
                print(f"Auto-detected base branch: {branch}")
            return branch

    if verbose:
        print("Could not detect base branch, falling back to 'main'")
    return "main"


def fetch_refs(base: str, current: str, verbose: bool) -> tuple[str, str]:
    """Fetch only the refs we need: origin/<base> and origin/<current>.

    Narrowing the fetch avoids pulling every branch (which can race with
    other worktrees' fetches and is wasteful in bare-repo setups).

    Returns (base_sha, remote_current_sha_or_empty). remote_current_sha is
    empty string if the current branch has no remote counterpart yet.
    """
    base_old = run_git("rev-parse", f"origin/{base}", check=False).stdout.strip()

    # Always fetch base. Also fetch current branch's remote if it exists.
    # Using explicit refspecs writes to the standard remote-tracking refs.
    refspecs = [f"+refs/heads/{base}:refs/remotes/origin/{base}"]
    # Probe whether the remote has a branch matching `current`.
    ls = run_git("ls-remote", "--heads", "origin", current, check=False)
    remote_has_current = bool(ls.stdout.strip())
    if remote_has_current:
        refspecs.append(f"+refs/heads/{current}:refs/remotes/origin/{current}")

    if verbose:
        print(f"Fetching from origin: {' '.join(refspecs)}")
    run_git("fetch", "origin", *refspecs)

    base_new = get_git_output("rev-parse", f"origin/{base}")
    if verbose:
        if base_old and base_old != base_new:
            print(f"origin/{base} updated: {base_old[:7]} -> {base_new[:7]}")
        elif base_old == base_new:
            print(f"origin/{base} unchanged at {base_new[:7]}")
        else:
            print(f"origin/{base} now at {base_new[:7]}")

    remote_current_sha = ""
    if remote_has_current:
        remote_current_sha = get_git_output("rev-parse", f"origin/{current}")
        if verbose:
            print(f"origin/{current} at {remote_current_sha[:7]}")

    return base_new, remote_current_sha


def is_ancestor(commit: str, of: str) -> bool:
    """Check if commit is an ancestor of another commit."""
    result = run_git("merge-base", "--is-ancestor", commit, of, check=False)
    return result.returncode == 0


def get_conflict_info() -> list[dict[str, str]]:
    """Parse unmerged entries from git status --porcelain=v2."""
    output = get_git_output("status", "--porcelain=v2")
    conflicts = []
    for line in output.splitlines():
        if not line.startswith("u "):
            continue
        parts = line.split("\t")
        path = parts[1] if len(parts) > 1 else parts[0].split(" ")[-1]
        fields = parts[0].split(" ")
        xy = fields[1] if len(fields) > 1 else "UU"
        conflict_type = _CONFLICT_TYPES.get(xy, f"unknown({xy})")
        conflicts.append({"path": path, "conflict_type": conflict_type, "xy": xy})
    return conflicts


def print_conflict_report(base: str, conflicts: list[dict[str, str]]) -> None:
    """Print structured conflict info for the calling agent."""
    print("\n--- CONFLICT REPORT ---")
    print(f"base_branch: origin/{base}")
    print(f"rebase_paused: {is_rebase_in_progress()}")
    print(f"conflict_count: {len(conflicts)}")
    for c in conflicts:
        print(f"  - {c['path']}: {c['conflict_type']}")
    print("---")
    print("Resolve conflicted files, `git add <file>`, then re-run with --continue")


def print_verification(base_sha: str, base_label: str) -> None:
    """Print post-rebase verification: commits on branch above the pinned base SHA."""
    log_output = get_git_output("log", "--oneline", f"{base_sha}..HEAD")
    print(f"\nCommits on branch above {base_label} ({base_sha[:7]}):")
    if log_output:
        print(log_output)
    else:
        print(f"  (none - branch is at {base_label})")


def save_pinned_target(git_dir: Path, target_sha: str, base: str) -> None:
    """Persist the pinned target SHA so --continue uses the same SHA."""
    (git_dir / PINNED_TARGET_FILE).write_text(f"{target_sha}\n{base}\n")


def load_pinned_target(git_dir: Path) -> tuple[str, str] | None:
    """Load pinned target SHA and base label. Returns None if absent."""
    path = git_dir / PINNED_TARGET_FILE
    if not path.exists():
        return None
    lines = path.read_text().splitlines()
    if len(lines) < 2:
        return None
    return lines[0].strip(), lines[1].strip()


def clear_pinned_target(git_dir: Path) -> None:
    path = git_dir / PINNED_TARGET_FILE
    if path.exists():
        path.unlink()


def rebase_onto_sha(target_sha: str, base_label: str, verbose: bool) -> bool:
    """Rebase current branch onto a pinned SHA (not a moving ref).

    Uses --onto with the current merge-base as the fork point to handle branches
    that contain cherry-picked copies of base commits.
    """
    fork_point = get_git_output("merge-base", target_sha, "HEAD")
    if verbose:
        print(f"Rebasing onto {base_label} @ {target_sha[:7]} (fork-point: {fork_point[:7]})...")
    result = run_git("rebase", "--onto", target_sha, fork_point, check=False)
    if result.returncode != 0:
        if verbose:
            print(f"Rebase paused with conflicts: {result.stderr.strip()}")
        return False

    new_merge_base = get_git_output("merge-base", target_sha, "HEAD")
    if new_merge_base != target_sha:
        if verbose:
            print(f"Warning: merge-base ({new_merge_base[:7]}) != target ({target_sha[:7]})")
            print("Branch contains duplicate commits. Using cherry-pick strategy...")
        return _rebase_via_cherry_pick(target_sha, base_label, verbose)
    return True


def _rebase_via_cherry_pick(target_sha: str, base_label: str, verbose: bool) -> bool:
    """Fallback: identify unique commits via --cherry-pick and replay them."""
    original_head = get_git_output("rev-parse", "HEAD")

    unique_output = get_git_output(
        "log", "--cherry-pick", "--right-only", "--reverse",
        "--format=%H", f"{target_sha}...HEAD"
    )
    if not unique_output:
        if verbose:
            print(f"No unique commits found - branch is fully merged into {base_label}")
        run_git("reset", "--hard", target_sha)
        return True

    unique_shas = unique_output.splitlines()
    if verbose:
        print(f"Cherry-picking {len(unique_shas)} unique commit(s) onto {base_label}...")

    run_git("reset", "--hard", target_sha)
    for sha in unique_shas:
        result = run_git("cherry-pick", sha, check=False)
        if result.returncode != 0:
            if verbose:
                print(f"Cherry-pick {sha[:7]} failed: {result.stderr.strip()}")
                print("Restoring original branch state...")
            run_git("cherry-pick", "--abort", check=False)
            run_git("reset", "--hard", original_head)
            return False
    return True


def continue_rebase(verbose: bool) -> bool:
    """Run git rebase --continue."""
    if verbose:
        print("Continuing rebase...")
    result = run_git("rebase", "--continue", check=False, env={"GIT_EDITOR": "true"})
    if result.returncode != 0:
        if verbose:
            stderr = result.stderr.strip()
            if stderr:
                print(f"Rebase continue: {stderr}")
        return False
    return True


def pr_exists_for_branch(branch: str, verbose: bool) -> bool:
    """Check if a PR exists for the given branch via `gh`. False if gh missing.

    Passes the branch name explicitly because `gh pr view` with no argument
    resolves via `@{push}` upstream tracking, which isn't set on freshly-created
    worktree branches — leading to false negatives (gh returns the default
    branch's PR status, or none).
    """
    if shutil.which("gh") is None:
        if verbose:
            print("gh CLI not available - skipping push step")
        return False
    result = subprocess.run(
        ["gh", "pr", "view", branch, "--json", "url,headRefName"],
        capture_output=True, text=True,
    )
    return result.returncode == 0 and bool(result.stdout.strip())


def push_with_precise_lease(current: str, remote_current_sha: str, verbose: bool) -> bool:
    """Force-push using --force-with-lease pinned to the fetched remote SHA.

    Precise lease (lease=<ref>:<sha>) is safer than bare --force-with-lease:
    it verifies that the remote is exactly what we just fetched, rather than
    trusting whatever our local remote-tracking ref happens to say.
    """
    if remote_current_sha:
        lease = f"--force-with-lease=refs/heads/{current}:{remote_current_sha}"
    else:
        # First push of this branch to remote; lease is vacuously true.
        lease = "--force-with-lease"
    if verbose:
        print(f"Pushing: git push {lease} --force-if-includes origin HEAD:{current}")
    result = subprocess.run(
        ["git", "push", lease, "--force-if-includes", "origin", f"HEAD:refs/heads/{current}"],
        capture_output=True, text=True,
    )
    sys.stdout.write(result.stdout)
    sys.stderr.write(result.stderr)
    return result.returncode == 0


def maybe_push(current: str, remote_current_sha: str, verbose: bool, skip: bool) -> int:
    """If a PR exists and push isn't skipped, force-push with precise lease."""
    if skip:
        if verbose:
            print("--no-push set, skipping push")
        return EXIT_SUCCESS
    if not pr_exists_for_branch(current, verbose):
        if verbose:
            print("No PR for this branch - skipping push")
        return EXIT_SUCCESS
    if push_with_precise_lease(current, remote_current_sha, verbose):
        return EXIT_SUCCESS
    print(
        "Push failed. If lease was rejected, someone pushed to the branch. "
        "Inspect `git log HEAD..origin/<branch>` before overriding.",
        file=sys.stderr,
    )
    return EXIT_FAILURE


def main() -> int:
    """Concurrency-safe sync of current branch onto origin/<base>.

    Design:
    - Per-worktree flock on $GIT_DIR/sync-base.lock (doesn't block other worktrees).
    - Clean tree required; no auto-stash (the biggest concurrency hazard).
    - Fetch only origin/<base> and origin/<current>, not every remote branch.
    - Pin origin/<base> to a SHA once and rebase onto that SHA, so fetches
      in other worktrees (which can advance origin/<base> mid-run) don't
      affect us.
    - Backup branches are namespaced per-worktree.
    - On success, if a PR exists, push with --force-with-lease=<ref>:<sha>
      using the SHA we just fetched (precise lease).
    """
    parser = argparse.ArgumentParser(
        description="Sync current git branch with remote base branch.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument("--verbose", "-v", action="store_true", help="Enable verbose output")
    parser.add_argument("--base", "-b", default=None, help="Base branch to rebase onto (auto-detected from origin/HEAD if not specified)")
    parser.add_argument("--backup", action="store_true", help="Create a backup branch before rebasing")
    parser.add_argument("--continue", dest="continue_rebase", action="store_true", help="Continue a paused rebase after resolving conflicts")
    parser.add_argument("--no-push", action="store_true", help="Skip the post-rebase force-push even if a PR exists")

    args = parser.parse_args()
    verbose: bool = args.verbose
    backup: bool = args.backup
    skip_push: bool = args.no_push

    try:
        run_git("rev-parse", "--git-dir")
    except GitError:
        print("Error: Not in a git repository", file=sys.stderr)
        return EXIT_FAILURE

    try:
        git_dir = get_git_dir()
        lock_fh = acquire_worktree_lock(git_dir, verbose)
    except GitError as e:
        print(f"Error: {e}", file=sys.stderr)
        return EXIT_FAILURE

    # Keep lock_fh referenced for the duration of main() by storing on a local.
    _ = lock_fh

    try:
        current_branch = get_current_branch()
        rebase_in_progress = is_rebase_in_progress()
        # Detached HEAD is fine mid-rebase (get_current_branch reads the rebase
        # state to recover the branch name); only fail when truly detached.
        if current_branch == "HEAD" and not rebase_in_progress:
            print("Error: Detached HEAD state", file=sys.stderr)
            return EXIT_FAILURE

        # --continue path: resume a paused rebase using the pinned target.
        if args.continue_rebase:
            if not rebase_in_progress:
                print("Error: No rebase in progress to continue", file=sys.stderr)
                return EXIT_FAILURE
            pinned = load_pinned_target(git_dir)
            if pinned is None:
                pinned_base = args.base if args.base else detect_base_branch(verbose)
                pinned_sha = get_git_output("rev-parse", f"origin/{pinned_base}")
            else:
                pinned_sha, pinned_base = pinned

            if not continue_rebase(verbose):
                conflicts = get_conflict_info()
                if conflicts:
                    print_conflict_report(pinned_base, conflicts)
                    return EXIT_CONFLICT
                print("Rebase continue failed unexpectedly", file=sys.stderr)
                return EXIT_FAILURE

            while is_rebase_in_progress():
                conflicts = get_conflict_info()
                if conflicts:
                    print_conflict_report(pinned_base, conflicts)
                    return EXIT_CONFLICT
                if not continue_rebase(verbose):
                    conflicts = get_conflict_info()
                    if conflicts:
                        print_conflict_report(pinned_base, conflicts)
                        return EXIT_CONFLICT
                    print("Rebase continue failed unexpectedly", file=sys.stderr)
                    return EXIT_FAILURE

            print(f"Rebase completed onto origin/{pinned_base}")
            clear_pinned_target(git_dir)
            print_verification(pinned_sha, f"origin/{pinned_base}")
            # After a successful --continue, also do the default push.
            # Re-read remote current SHA via a narrow ls-remote (no full fetch needed).
            ls = run_git("ls-remote", "--heads", "origin", current_branch, check=False)
            remote_current_sha = ls.stdout.split()[0] if ls.stdout.strip() else ""
            return maybe_push(current_branch, remote_current_sha, verbose, skip_push)

        # Not a --continue: fresh sync. Guard against pre-existing rebase state.
        if is_rebase_in_progress():
            print(
                "Error: A rebase is already in progress. Resolve it or run with --continue.",
                file=sys.stderr,
            )
            return EXIT_FAILURE

        if has_uncommitted_changes():
            print(
                "Error: Working tree has uncommitted changes. Commit or stash them manually "
                "before running sync-base (auto-stash disabled for worktree safety).",
                file=sys.stderr,
            )
            return EXIT_FAILURE

        base: str = args.base if args.base else detect_base_branch(verbose)
        worktree_status = "worktree" if is_worktree() else "regular repo"
        worktree_label = get_worktree_label()
        if verbose:
            print(f"Current branch: {current_branch} ({worktree_status}, label={worktree_label})")

        if backup:
            backup_branch = create_backup_branch(current_branch, worktree_label, verbose)
            print(f"Backup created: {backup_branch}")

        base_sha, remote_current_sha = fetch_refs(base, current_branch, verbose)
        save_pinned_target(git_dir, base_sha, base)

        if is_ancestor(base_sha, "HEAD"):
            print(f"Already up to date with origin/{base} ({base_sha[:7]})")
            clear_pinned_target(git_dir)
            print_verification(base_sha, f"origin/{base}")
            return maybe_push(current_branch, remote_current_sha, verbose, skip_push)

        if rebase_onto_sha(base_sha, f"origin/{base}", verbose):
            print(f"Successfully rebased {current_branch} onto origin/{base}")
            clear_pinned_target(git_dir)
            print_verification(base_sha, f"origin/{base}")
            return maybe_push(current_branch, remote_current_sha, verbose, skip_push)

        conflicts = get_conflict_info()
        if conflicts:
            print_conflict_report(base, conflicts)
        else:
            print("Rebase failed but no conflicts detected", file=sys.stderr)
        return EXIT_CONFLICT

    except GitError as e:
        print(f"Git error: {e}", file=sys.stderr)
        return EXIT_FAILURE


if __name__ == "__main__":
    sys.exit(main())
