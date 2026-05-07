"""Shared helpers for pr_ops and bramble_ops.

Pure stdlib. The only external I/O is the ``run`` subprocess wrapper —
everything that hits the network, git, gh, or bramble funnels through it,
so tests can patch one boundary.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path


class CommandError(RuntimeError):
    """A subprocess returned non-zero. Carries stdout/stderr for surfacing."""

    def __init__(self, cmd: list[str], returncode: int, stdout: str, stderr: str) -> None:
        self.cmd = cmd
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr
        super().__init__(
            f"command failed (exit {returncode}): {' '.join(cmd)}\nstdout: {stdout}\nstderr: {stderr}"
        )


@dataclass(frozen=True)
class RunResult:
    stdout: str
    stderr: str
    returncode: int


def run(
    cmd: list[str],
    *,
    check: bool = True,
    env: dict[str, str] | None = None,
    cwd: str | None = None,
    input_text: str | None = None,
    timeout: float | None = None,
) -> RunResult:
    """Run a subprocess and return its result.

    The single I/O boundary of this module. Tests patch this (or its
    callers) to simulate gh/git/bramble without touching the network.
    """
    merged_env = os.environ.copy()
    if env:
        merged_env.update(env)
    proc = subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        check=False,
        env=merged_env,
        cwd=cwd,
        input=input_text,
        timeout=timeout,
    )
    res = RunResult(stdout=proc.stdout, stderr=proc.stderr, returncode=proc.returncode)
    if check and proc.returncode != 0:
        raise CommandError(cmd, proc.returncode, proc.stdout, proc.stderr)
    return res


def repo_slug() -> str:
    """Return the short repo name used for state-file slugs.

    Mirrors the shell convention in the original skill:
      basename -s .git "$(git remote get-url origin)"
      fallback: basename "$(git rev-parse --show-toplevel)"
    """
    try:
        remote = run(["git", "remote", "get-url", "origin"], check=True).stdout.strip()
        if remote:
            slug = Path(remote).name
            return slug.removesuffix(".git")
    except (CommandError, FileNotFoundError):
        pass
    top = run(["git", "rev-parse", "--show-toplevel"], check=True).stdout.strip()
    return Path(top).name


def state_paths(pr_number: int | str | None, branch: str | None = None) -> tuple[Path, Path]:
    """Return (state_dir, state_file) for a PR, or for a branch when no PR exists.

    Convention:
        PR:     ~/.bramble/projects/<repo>-<pr>/pr-polish-state.json
        branch: ~/.bramble/projects/<repo>-branch-<slug>/pr-polish-state.json

    Pass ``pr_number=None`` to get the branch-scoped path; ``branch`` must be
    provided in that case. The branch name is sanitized for filesystem safety.
    """
    if pr_number is not None:
        slug = f"{repo_slug()}-{pr_number}"
    else:
        if not branch:
            raise ValueError("state_paths requires either pr_number or branch")
        slug = f"{repo_slug()}-{branch_envelope_key(branch)}"
    state_dir = Path.home() / ".bramble" / "projects" / slug
    return state_dir, state_dir / "pr-polish-state.json"


_BRANCH_SAFE_RE = re.compile(r"[^A-Za-z0-9._-]+")


def _slugify_branch(branch: str) -> str:
    """Lowercase + replace filesystem-unfriendly chars (/, spaces, etc.) with '-'."""
    return _BRANCH_SAFE_RE.sub("-", branch.strip()).strip("-").lower() or "unnamed"


def branch_envelope_key(branch: str) -> str:
    """Canonical ``branch-<slug>`` key used in state-dir slugs and bramble
    envelope filenames. Centralized so launch and finalize agree on the
    exact filename component for a given branch — otherwise a name like
    ``feature/foo`` would create a nested ``/tmp/pp-...-branch-feature/foo-...``
    path on one side and a flat slug on the other, and ``_persist_round_findings``
    would never find the envelope it was looking for.
    """
    return f"branch-{_slugify_branch(branch)}"


def current_branch() -> str | None:
    """Return the current branch name, or None if detached HEAD or no git."""
    try:
        res = run(["git", "rev-parse", "--abbrev-ref", "HEAD"], check=True)
    except (CommandError, FileNotFoundError):
        return None
    name = res.stdout.strip()
    if not name or name == "HEAD":
        return None
    return name


def detect_base_branch() -> str:
    """Auto-detect the remote default branch via origin/HEAD.

    Mirrors ``detect_base_branch`` in .claude/skills/git:sync-base/git-sync.py.
    Ported rather than shell-out so this module stays hermetic for unit tests;
    keep the two in sync if git-sync.py changes.

    Falls back to 'main' when detection fails.
    """
    res = run(["git", "symbolic-ref", "refs/remotes/origin/HEAD"], check=False)
    if res.returncode == 0:
        ref = res.stdout.strip()
        return ref.removeprefix("refs/remotes/origin/") or "main"
    # origin/HEAD is missing; try to set it. This mutates shared state, so
    # only do it when genuinely needed.
    set_res = run(["git", "remote", "set-head", "origin", "--auto"], check=False)
    if set_res.returncode == 0:
        res = run(["git", "symbolic-ref", "refs/remotes/origin/HEAD"], check=False)
        if res.returncode == 0:
            ref = res.stdout.strip()
            return ref.removeprefix("refs/remotes/origin/") or "main"
    return "main"


def changed_files(base: str) -> list[str]:
    """Repo-relative paths added/modified/renamed vs ``origin/<base>``.

    Empty list on git failure (outside a worktree, missing remote, etc.) so
    callers can degrade gracefully rather than abort.
    """
    res = run(
        [
            "git",
            "diff",
            "--name-only",
            "--diff-filter=AMR",
            f"origin/{base}...HEAD",
        ],
        check=False,
    )
    if res.returncode != 0:
        return []
    return [line for line in res.stdout.splitlines() if line.strip()]


def atomic_write_json(path: Path, obj: object) -> None:
    """Write JSON atomically: temp file in same dir, then rename.

    A crash between the temp-write and the rename leaves the old file intact.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(
        prefix=f".{path.name}.", suffix=".tmp", dir=str(path.parent)
    )
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(obj, f, indent=2, sort_keys=False)
            f.write("\n")
        os.replace(tmp, path)
    except Exception:
        # Best-effort cleanup; swallowing errors here would mask the original.
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def read_json(path: Path, default: object = None) -> object:
    """Read a JSON file, returning ``default`` if missing."""
    try:
        with path.open("r") as f:
            return json.load(f)
    except FileNotFoundError:
        return default


def print_json(obj: object) -> None:
    """Pretty-print JSON to stdout (contract with callers: always valid JSON)."""
    print(json.dumps(obj, indent=2, sort_keys=False))


_WORD_RE = re.compile(r"[A-Za-z0-9_]+")


def topic_of(message: str, *, words: int = 8) -> str:
    """First N word-like tokens of a finding message, lowercased.

    Used as the dedupe key for bramble findings and for the N+1 spiral guard.
    Deterministic — does not depend on locale or environment.
    """
    tokens = _WORD_RE.findall(message or "")
    return " ".join(t.lower() for t in tokens[:words])


# Source tags shared by pr_ops.classify_comments and bramble_ops triage.
SOURCE_INLINE = "github-inline"
SOURCE_ISSUE = "github-issue"
SOURCE_REVIEW = "github-review"

# Single severity ladder used by pr_ops.recompute_counts and bramble_ops.triage.
SEVERITY_ORDER = {"critical": 4, "high": 3, "medium": 2, "low": 1, "nit": 0}


def severity_rank(sev: str | None) -> int:
    return SEVERITY_ORDER.get(sev or "", -1)


