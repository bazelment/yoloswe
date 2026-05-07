---
name: git:sync-base
description: Ensure the current branch is cleanly rebased onto the remote base branch so that `origin/{base}..HEAD` only shows this branch's commits.
argument-hint: ""
disable-model-invocation: false
---

# Sync Branch to Base

```bash
python3 .claude/skills/git:sync-base/git-sync.py --verbose
```

Override base branch with `--base <branch>` if needed. Auto-detects via `origin/HEAD`. Pass `--no-push` to skip the post-rebase force-push. Pass `--backup` to create a timestamped backup branch before rebasing.

The script:
1. Takes a per-worktree lock (`$GIT_DIR/sync-base.lock`) so two runs in the same worktree can't race. Does NOT block other worktrees sharing the bare repo.
2. Requires a clean tree. Commit or stash manually first. Auto-stash is deliberately disabled because a shared stash ref across worktrees is a concurrency hazard.
3. Fetches only `origin/<base>` and `origin/<current-branch>` (narrow refspec, not `git fetch origin`).
4. Pins `origin/<base>` to a SHA once and rebases onto that SHA. Other worktrees advancing `origin/<base>` mid-run cannot affect us. Includes a cherry-pick fallback for branches containing duplicate copies of base commits.
5. On success, if a PR exists for the branch, force-pushes with a precise lease (`--force-with-lease=refs/heads/<branch>:<fetched-remote-sha>` plus `--force-if-includes`) using the SHA fetched in step 3.
6. Prints the commit log above the pinned base SHA for verification.

## If exit code 2 (conflict)

The rebase is paused and the output includes a conflict report listing each file and its type (`deleted_by_us`, `deleted_by_them`, `both_modified`, `both_added`, `both_deleted`).

Resolve each conflicted file based on its type:
- **deleted_by_us/them**: decide whether to keep or delete (`git rm <file>` or `git checkout --theirs <file> && git add <file>`)
- **both_modified**: open the file, resolve `<<<<<<<` markers, `git add <file>`
- **both_added**: pick the right version, `git add <file>`

Then continue:
```bash
python3 .claude/skills/git:sync-base/git-sync.py --verbose --continue
```

Repeat until the script exits 0. `--continue` uses the same pinned target SHA saved at the start of the original run, and will also force-push on success (unless `--no-push` is passed).

## If exit code 1 (error)

A non-conflict failure. Common causes and fixes:

- **Not in a git repo / detached HEAD** -> read the error and address the root cause.
- **Working tree has uncommitted changes** -> commit or stash manually, then re-run.
- **Another git-sync is already running in this worktree (lock held)** -> wait for the other run, or if it is stale remove `$(git rev-parse --git-dir)/sync-base.lock`.
- **Push failed after successful rebase** -> the precise lease was rejected because the remote branch moved. Inspect `git log HEAD..origin/<branch>` before overriding. The rebase itself succeeded; only the push step failed, so you can re-run (the rebase will be a no-op and the push will retry with a fresh lease).
