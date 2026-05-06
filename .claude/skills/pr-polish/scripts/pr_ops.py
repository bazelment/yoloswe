#!/usr/bin/env python3
"""PR-side operations for the pr-polish skill.

Wraps gh + git + state-file I/O behind a stable subcommand interface.
Stdlib only. All network/gh/git calls go through ``_common.run`` so tests
can patch one boundary.

Usage:
    python3 pr_ops.py identify
    python3 pr_ops.py fetch-comments
    python3 pr_ops.py reply-inline <comment_id> <body>
    python3 pr_ops.py comment-pr <body>
    python3 pr_ops.py ci-failed-tests [--pr N]
    python3 pr_ops.py ci-compare-base [--pr N]
    python3 pr_ops.py state-load <pr_number>
    python3 pr_ops.py state-append-round <pr_number> <n> <head_before>
    python3 pr_ops.py state-finalize-round <pr_number> <n> <head_after> <actions_json_file>
    python3 pr_ops.py state-mark-complete <pr_number> <reason>

``identify`` detects the current branch and probes for a PR. When the
branch has no PR it still returns branch/base/owner/repo so the
orchestrator can run in branch-only mode. ``sync-base`` is deliberately
NOT a subcommand here — invoke ``~/.claude/skills/git:sync-base/git-sync.py``
directly. All subcommands print JSON to stdout and exit non-zero on error.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path
from typing import Any

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

from datetime import UTC

from _common import (  # noqa: E402 — sys.path tweak above
    CommandError,
    atomic_write_json,
    current_branch,
    detect_base_branch,
    print_json,
    read_json,
    repo_slug,
    run,
    state_paths,
)

# ---------------------------------------------------------------------------
# PR identity
# ---------------------------------------------------------------------------


def identify_pr(pr_number: int | None = None) -> dict[str, Any]:
    """Return PR metadata + state paths.

    Detection order:
      1. Read the current branch (``git rev-parse --abbrev-ref HEAD``).
      2. If ``pr_number`` was passed, query that PR directly. Otherwise try
         ``gh pr view`` on the current branch.
      3. When the branch has no PR, degrade gracefully: return a dict with
         ``pr_number: None``, populated ``branch``/``base``/``owner``/``repo``.
         The orchestrator can still run in branch-only mode.

    State path follows ``state_paths``:
      - PR present:   ~/.bramble/projects/<repo>-<pr>/pr-polish-state.json
      - PR absent:    ~/.bramble/projects/<repo>-branch-<slug>/pr-polish-state.json
    """
    branch = current_branch()
    owner, repo, owner_repo = _owner_repo()

    pr_view_args = ["gh", "pr", "view"]
    if pr_number is not None:
        pr_view_args.append(str(pr_number))
    pr_view_args += [
        "--json",
        "number,title,url,baseRefName,headRefName",
        "--jq",
        "{pr_number: .number, title: .title, url: .url, base: .baseRefName, head: .headRefName}",
    ]
    pr_res = run(pr_view_args, check=False)
    pr_data: dict[str, Any] | None = None
    if pr_res.returncode == 0 and pr_res.stdout.strip():
        try:
            pr_data = json.loads(pr_res.stdout)
        except json.JSONDecodeError:
            pr_data = None

    if pr_data and pr_data.get("pr_number"):
        state_dir, state_file = state_paths(pr_data["pr_number"])
        state_dir.mkdir(parents=True, exist_ok=True)
        return {
            **pr_data,
            "branch": pr_data.get("head") or branch,
            "owner": owner,
            "repo": repo,
            "owner_repo": owner_repo,
            "state_dir": str(state_dir),
            "state_file": str(state_file),
        }

    # No PR — branch-only mode.
    if not branch:
        raise RuntimeError("cannot identify context: no PR found and current HEAD is detached")
    base = detect_base_branch()
    state_dir, state_file = state_paths(None, branch=branch)
    state_dir.mkdir(parents=True, exist_ok=True)
    return {
        "pr_number": None,
        "title": None,
        "url": None,
        "base": base,
        "head": branch,
        "branch": branch,
        "owner": owner,
        "repo": repo,
        "owner_repo": owner_repo,
        "state_dir": str(state_dir),
        "state_file": str(state_file),
    }


def _owner_repo() -> tuple[str, str, str]:
    """Return ``(owner, repo, 'owner/repo')`` via ``gh repo view``."""
    res = run(
        ["gh", "repo", "view", "--json", "owner,name", "--jq", '"\\(.owner.login)/\\(.name)"'],
        check=True,
    )
    owner_repo = res.stdout.strip().strip('"')
    owner, repo = owner_repo.split("/", 1)
    return owner, repo, owner_repo


# ---------------------------------------------------------------------------
# Comment fetching + classification
# ---------------------------------------------------------------------------

# Source tags that flow downstream into state_file.comment_actions[].source.
SOURCE_INLINE = "github-inline"
SOURCE_ISSUE = "github-issue"
SOURCE_REVIEW = "github-review"


def _fetch_inline_comments(owner_repo: str, pr: int) -> list[dict[str, Any]]:
    """Raw inline review comments (may include replies; caller filters)."""
    out = run(
        [
            "gh",
            "api",
            "--paginate",
            f"repos/{owner_repo}/pulls/{pr}/comments",
        ],
        check=True,
    ).stdout.strip()
    return json.loads(out) if out else []


def _fetch_issue_comments(owner_repo: str, pr: int) -> list[dict[str, Any]]:
    """Top-level PR comments (tracked under the issues endpoint)."""
    out = run(
        [
            "gh",
            "api",
            "--paginate",
            f"repos/{owner_repo}/issues/{pr}/comments",
        ],
        check=True,
    ).stdout.strip()
    return json.loads(out) if out else []


def _fetch_reviews(owner_repo: str, pr: int) -> list[dict[str, Any]]:
    """Review-level bodies (excluding APPROVED/DISMISSED and empty bodies)."""
    out = run(
        [
            "gh",
            "api",
            "--paginate",
            f"repos/{owner_repo}/pulls/{pr}/reviews",
        ],
        check=True,
    ).stdout.strip()
    return json.loads(out) if out else []


def classify_comments(
    inline: list[dict[str, Any]],
    issues: list[dict[str, Any]],
    reviews: list[dict[str, Any]],
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """Merge + classify comments. Pure — used directly in tests.

    Returns ``(kept, noise)``. ``noise`` holds comments dropped by the bot
    process-noise filter (linear linkbacks, claude-bot progress posts) so
    the orchestrator can persist a counter + samples without re-fetching.

    An inline comment with an existing reply from anyone is considered
    "already addressed" and filtered out. Reviews in APPROVED/DISMISSED state
    or with empty bodies are dropped. Bot review-summary boilerplate and bot
    process-noise posts are dropped into the ``noise`` list, not ``kept``.
    """
    reply_counts: dict[int, int] = {}
    for c in inline:
        parent = c.get("in_reply_to_id")
        if parent:
            reply_counts[parent] = reply_counts.get(parent, 0) + 1

    kept: list[dict[str, Any]] = []
    noise: list[dict[str, Any]] = []

    def _noise_sample(
        user: dict[str, Any], comment_id: Any, body: str, pattern: str
    ) -> dict[str, Any]:
        return {
            "id": comment_id,
            "author": user.get("login"),
            "pattern": pattern,
        }

    for c in inline:
        if c.get("in_reply_to_id"):
            continue  # This is itself a reply; skip — the parent is what we triage.
        cid = c["id"]
        user = c.get("user") or {}
        body = c.get("body", "") or ""
        noise_pattern = _bot_process_noise_pattern(user, body)
        if noise_pattern is not None:
            noise.append(_noise_sample(user, cid, body, noise_pattern))
            continue
        kept.append(
            {
                "id": cid,
                "source": SOURCE_INLINE,
                "author": user.get("login"),
                "is_bot": user.get("type") == "Bot",
                "path": c.get("path"),
                "line": c.get("line"),
                "body": body,
                "reply_count": reply_counts.get(cid, 0),
                "created_at": c.get("created_at"),
            }
        )

    for c in issues:
        user = c.get("user") or {}
        body = c.get("body", "") or ""
        noise_pattern = _bot_process_noise_pattern(user, body)
        if noise_pattern is not None:
            noise.append(_noise_sample(user, c["id"], body, noise_pattern))
            continue
        kept.append(
            {
                "id": c["id"],
                "source": SOURCE_ISSUE,
                "author": user.get("login"),
                "is_bot": user.get("type") == "Bot",
                "path": None,
                "line": None,
                "body": body,
                "reply_count": 0,
                "created_at": c.get("created_at"),
            }
        )

    for r in reviews:
        if r.get("state") in {"APPROVED", "DISMISSED"}:
            continue
        body = (r.get("body") or "").strip()
        if not body:
            continue
        user = r.get("user") or {}
        if _is_bot_review_summary(user, r.get("state"), body):
            continue  # Summary like "Bugbot found 2 issues" — inline children carry the signal.
        noise_pattern = _bot_process_noise_pattern(user, body, state=r.get("state"))
        if noise_pattern is not None:
            noise.append(_noise_sample(user, r["id"], body, noise_pattern))
            continue
        kept.append(
            {
                "id": r["id"],
                "source": SOURCE_REVIEW,
                "author": user.get("login"),
                "is_bot": user.get("type") == "Bot",
                "path": None,
                "line": None,
                "body": r.get("body", ""),
                "reply_count": 0,
                "created_at": r.get("submitted_at"),
                "state": r.get("state"),
            }
        )

    return kept, noise


_BOT_REVIEW_SUMMARY_RE = re.compile(r"found\s+\d+\s+(potential\s+)?issues?", re.IGNORECASE)

# Bots post process-automation status (Linear link-backs, in-progress
# review trackers) that aren't findings. Filter at fetch time so triage
# never sees them and the final summary isn't polluted with spurious
# `false_positive` entries. Each pattern is paired with a stable tag so
# `noise_samples` is diagnosable post-hoc.
_BOT_NOISE_PATTERNS: tuple[tuple[re.Pattern[str], str], ...] = (
    (re.compile(r"<!--\s*linear-linkback\s*-->", re.IGNORECASE), "linear-linkback"),
    (re.compile(r"reviewing pr\.\.\.", re.IGNORECASE), "claude-progress"),
    (re.compile(r"\[view job run\]", re.IGNORECASE), "claude-progress"),
)


def _bot_process_noise_pattern(
    user: dict[str, Any], body: str, *, state: str | None = None
) -> str | None:
    """Return the noise tag when this bot post is process-automation noise, else None.

    Gated on ``user.type == "Bot"`` to prevent human comments that happen to
    quote these strings from being dropped. ``CHANGES_REQUESTED`` reviews are
    always preserved — a bot in that state is gating a real decision.
    """
    if user.get("type") != "Bot":
        return None
    if state and state.upper() == "CHANGES_REQUESTED":
        return None
    for pattern, tag in _BOT_NOISE_PATTERNS:
        if pattern.search(body):
            return tag
    return None


def _is_bot_review_summary(user: dict[str, Any], state: str | None, body: str) -> bool:
    """Drop bot review-level comments that are just "found N issues" summaries.

    Keeps CHANGES_REQUESTED (real gating) and long-form prose reviews. The
    inline comments attached to the same review still pass through separately,
    so we don't lose any actionable signal.
    """
    if user.get("type") != "Bot":
        return False
    if state and state.upper() == "CHANGES_REQUESTED":
        return False
    if not _BOT_REVIEW_SUMMARY_RE.search(body):
        return False
    # Strip HTML comments and links — bugbot summaries are mostly scaffolding.
    stripped = re.sub(r"<!--.*?-->", "", body, flags=re.DOTALL)
    stripped = re.sub(r"<[^>]+>", "", stripped)
    return len(stripped.strip()) < 400


# Cap on how many example noise entries we keep per round. The counter is
# the load-bearing number; samples are just for post-hoc "what did the bot
# say that we dropped" debugging.
_NOISE_SAMPLE_CAP = 5


def fetch_comments(pr: dict[str, Any]) -> dict[str, Any]:
    """Fetch and classify PR comments.

    Returns the wrapped shape ``{"comments": [...], "noise_filtered": int,
    "noise_samples": [...]}``. ``bramble_ops.triage --pr-comments`` accepts
    either this wrapped shape or the legacy bare list for backward compat.
    """
    inline = _fetch_inline_comments(pr["owner_repo"], pr["pr_number"])
    issues = _fetch_issue_comments(pr["owner_repo"], pr["pr_number"])
    reviews = _fetch_reviews(pr["owner_repo"], pr["pr_number"])
    kept, noise = classify_comments(inline, issues, reviews)
    return {
        "comments": kept,
        "noise_filtered": len(noise),
        "noise_samples": noise[:_NOISE_SAMPLE_CAP],
    }


# ---------------------------------------------------------------------------
# CI failure detail + flake classification
# ---------------------------------------------------------------------------

# Flake markers: first match wins. Tuples of (regex, flake_reason).
_FLAKE_MARKERS: tuple[tuple[re.Pattern[str], str], ...] = (
    (re.compile(r"text file busy", re.IGNORECASE), "etxtbsy"),
    (
        re.compile(r"cache upload failed|bazel-cache.*(failed|timeout)", re.IGNORECASE),
        "bazel_cache",
    ),
    (re.compile(r"ECONNRESET|i/o timeout|network is unreachable", re.IGNORECASE), "network"),
)

_GO_FAIL_RE = re.compile(r"^---\s+FAIL:\s+([A-Za-z0-9_/]+)", re.MULTILINE)
_CTX_DEADLINE_RE = re.compile(r"context deadline exceeded", re.IGNORECASE)


def _extract_job_id_from_link(link: str) -> int | None:
    """``gh pr checks --json link`` returns .../actions/runs/<run>/job/<job>."""
    m = re.search(r"/job/(\d+)", link or "")
    return int(m.group(1)) if m else None


def _fetch_job_log(owner_repo: str, job_id: int) -> str:
    res = run(["gh", "api", f"/repos/{owner_repo}/actions/jobs/{job_id}/logs"], check=False)
    return res.stdout or ""


def classify_ci_log(log: str) -> dict[str, Any]:
    """Pure: extract failed test names + flake classification from a job log.

    Returns ``{failed_tests, is_flake, flake_reason, assertion_snippet}``.
    A flake marker wins over ``--- FAIL:`` matches — ETXTBSY / bazel cache
    issues don't have a meaningful test name to blame.
    """
    for pat, reason in _FLAKE_MARKERS:
        m = pat.search(log)
        if m:
            return {
                "failed_tests": _extract_failed_tests(log),
                "is_flake": True,
                "flake_reason": reason,
                "assertion_snippet": _snippet_around(log, m.start(), m.end()),
            }
    failed = _extract_failed_tests(log)
    if not failed and _CTX_DEADLINE_RE.search(log):
        m = _CTX_DEADLINE_RE.search(log)
        return {
            "failed_tests": [],
            "is_flake": True,
            "flake_reason": "ci_deadline",
            "assertion_snippet": _snippet_around(log, m.start(), m.end()),
        }
    snippet = ""
    if failed:
        m = _GO_FAIL_RE.search(log)
        if m:
            snippet = _snippet_around(log, m.start(), m.end())
    return {
        "failed_tests": failed,
        "is_flake": False,
        "flake_reason": None,
        "assertion_snippet": snippet,
    }


def _extract_failed_tests(log: str) -> list[str]:
    # dedupe while preserving order
    seen: set[str] = set()
    out: list[str] = []
    for m in _GO_FAIL_RE.finditer(log):
        name = m.group(1)
        if name not in seen:
            seen.add(name)
            out.append(name)
    return out


def _snippet_around(log: str, start: int, end: int, *, lines_after: int = 6) -> str:
    # Take from the matched line through ``lines_after`` subsequent lines.
    line_start = log.rfind("\n", 0, start) + 1
    tail = log[line_start:]
    cut = tail.split("\n", lines_after + 1)
    return "\n".join(cut[: lines_after + 1]).strip()


def _failing_checks(pr_number: int) -> list[dict[str, Any]]:
    res = run(
        ["gh", "pr", "checks", str(pr_number), "--json", "name,state,link,workflow"],
        check=False,
    )
    if not res.stdout.strip():
        return []
    try:
        checks = json.loads(res.stdout)
    except json.JSONDecodeError:
        return []
    bad = {"failure", "failed", "error", "fail"}
    return [c for c in checks if (c.get("state") or "").lower() in bad]


def ci_failed_tests(pr_number: int) -> list[dict[str, Any]]:
    """Return one entry per failing check, with per-test detail and flake hint.

    Entry shape:
        {job_id, job_name, workflow, url, failed_tests, is_flake,
         flake_reason, assertion_snippet}

    ``failed_tests`` is the deduped list of Go ``--- FAIL: Name`` matches
    from the job's log. Flake classification fires on marker patterns
    (ETXTBSY, bazel-cache, context deadline) before treating the run as a
    real assertion failure.
    """
    pr = identify_pr(pr_number)
    owner_repo = pr["owner_repo"]
    out: list[dict[str, Any]] = []
    for c in _failing_checks(pr_number):
        job_id = _extract_job_id_from_link(c.get("link") or "")
        if job_id is None:
            continue
        log = _fetch_job_log(owner_repo, job_id)
        cls = classify_ci_log(log)
        out.append(
            {
                "job_id": job_id,
                "job_name": c.get("name"),
                "workflow": c.get("workflow"),
                "url": c.get("link"),
                **cls,
            }
        )
    return out


def _latest_base_run_id(owner_repo: str, base: str) -> int | None:
    res = run(
        [
            "gh",
            "api",
            f"/repos/{owner_repo}/actions/runs?branch={base}&event=push&status=completed&per_page=1",
        ],
        check=False,
    )
    try:
        obj = json.loads(res.stdout or "{}")
    except json.JSONDecodeError:
        return None
    runs = obj.get("workflow_runs") or []
    if not runs:
        return None
    return int(runs[0].get("id")) if runs[0].get("id") else None


def _run_failing_tests(owner_repo: str, run_id: int) -> tuple[str, set[str]]:
    """Return ``(head_sha, {test_names})`` for all failed jobs in a workflow run."""
    res = run(
        ["gh", "api", f"/repos/{owner_repo}/actions/runs/{run_id}/jobs?per_page=100"],
        check=False,
    )
    try:
        obj = json.loads(res.stdout or "{}")
    except json.JSONDecodeError:
        return ("", set())
    sha = obj.get("head_sha") or ""
    names: set[str] = set()
    for job in obj.get("jobs") or []:
        if (job.get("conclusion") or "").lower() not in {"failure", "error"}:
            continue
        log = _fetch_job_log(owner_repo, int(job["id"]))
        cls = classify_ci_log(log)
        if cls["is_flake"]:
            continue
        for t in cls["failed_tests"]:
            names.add(t)
    return (sha, names)


def ci_compare_base(pr_number: int) -> dict[str, Any]:
    """Split current PR failures into pre_existing vs pr_caused.

    A test is ``pre_existing`` if the latest completed push-run on the base
    branch failed the same test. Result is cached per-base-SHA at
    ``<state_dir>/base-ci-<sha>.json`` so repeated calls in one session
    skip the log re-pull.
    """
    pr = identify_pr(pr_number)
    owner_repo = pr["owner_repo"]
    base = pr["base"]
    state_dir, _ = state_paths(pr_number)

    run_id = _latest_base_run_id(owner_repo, base)
    base_failures: set[str] = set()
    base_sha = ""
    if run_id is not None:
        # Fetch once, resolve sha from the jobs endpoint, then check cache.
        probe = run(
            ["gh", "api", f"/repos/{owner_repo}/actions/runs/{run_id}"],
            check=False,
        )
        try:
            probe_obj = json.loads(probe.stdout or "{}")
            base_sha = probe_obj.get("head_sha") or ""
        except json.JSONDecodeError:
            base_sha = ""
        cache_path = state_dir / f"base-ci-{base_sha}.json" if base_sha else None
        cached = read_json(cache_path, default=None) if cache_path and cache_path.exists() else None
        if cached is not None:
            base_failures = set(cached.get("failed_tests", []))
        else:
            base_sha, base_failures = _run_failing_tests(owner_repo, run_id)
            if cache_path and base_sha:
                state_dir.mkdir(parents=True, exist_ok=True)
                atomic_write_json(
                    cache_path,
                    {"run_id": run_id, "head_sha": base_sha, "failed_tests": sorted(base_failures)},
                )

    current = ci_failed_tests(pr_number)
    current_tests: set[str] = set()
    for entry in current:
        if entry.get("is_flake"):
            continue
        current_tests.update(entry.get("failed_tests") or [])

    pre_existing = sorted(current_tests & base_failures)
    pr_caused = sorted(current_tests - base_failures)
    return {
        "base": base,
        "base_run_id": run_id,
        "base_head_sha": base_sha,
        "pre_existing": pre_existing,
        "pr_caused": pr_caused,
        "current_failures": current,
    }


# ---------------------------------------------------------------------------
# State file I/O
# ---------------------------------------------------------------------------


def _parse_ctx(ctx: str) -> tuple[int | None, str | None]:
    """Parse a state context token into ``(pr_number, branch)``.

    The token is either a bare integer PR number or ``branch:<name>`` for
    branches that have no PR yet. Keeping the CLI surface untyped (single
    positional) lets callers paste whatever ``identify`` emitted without
    branching on PR vs branch.
    """
    if ctx.startswith("branch:"):
        return None, ctx[len("branch:") :]
    return int(ctx), None


def state_load(ctx: int | str) -> dict[str, Any]:
    pr, branch = _resolve_ctx(ctx)
    _, path = state_paths(pr, branch=branch)
    return read_json(path, default={}) or {}


def _resolve_ctx(ctx: int | str) -> tuple[int | None, str | None]:
    if isinstance(ctx, int):
        return ctx, None
    return _parse_ctx(ctx)


def state_append_round(
    ctx: int | str,
    n: int,
    head_before: str,
    *,
    verify_head: bool = True,
    noise_filtered: int = 0,
    noise_samples: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """Start a new round or refresh head_before on an in-progress round.

    ``ctx`` is either the integer PR number or ``branch:<name>``. When
    ``verify_head`` is true (the default), compare the declared
    ``head_before`` against ``git rev-parse HEAD``. A mismatch means the
    orchestrator computed the SHA in one message and a commit landed before
    this call — refuse rather than corrupt the round's lineage.

    ``noise_filtered`` / ``noise_samples`` record bot process-noise that
    ``fetch-comments`` dropped at intake (linear linkbacks, claude-bot
    progress posts). Default zero/empty; the orchestrator passes the
    counter from ``pp-comments.json``. Re-calling append on an existing
    round merges the new counter into whatever was already there so a
    resume doesn't zero out a round's noise tally.
    """
    if verify_head:
        try:
            current = run(["git", "rev-parse", "HEAD"], check=True).stdout.strip()
        except (CommandError, FileNotFoundError) as e:
            raise RuntimeError(f"could not read git HEAD for verification: {e}") from e
        if current != head_before:
            raise RuntimeError(
                f"HEAD {current[:7]} != declared head_before {head_before[:7]}; "
                "refuse to append round (orchestrator raced a commit — rerun with the current HEAD)"
            )
    pr_number, branch = _resolve_ctx(ctx)
    _, path = state_paths(pr_number, branch=branch)
    state = read_json(path, default=None)
    if state is None:
        state = {
            "pr_number": pr_number,
            "branch": branch,
            "started_at": _utc_now(),
            "current_round": n,
            "last_commit_at_round_start": head_before,
            "rounds": [],
        }
    rounds = state.setdefault("rounds", [])
    existing = next((r for r in rounds if r.get("n") == n), None)
    samples = noise_samples or []
    if existing is None:
        rounds.append(
            {
                "n": n,
                "head_before": head_before,
                "head_after": None,
                "codex_findings": [],
                "cursor_findings": [],
                "ci_findings": [],
                "fixed_count": 0,
                "skipped_count": 0,
                "top_severity": None,
                "top_was_false_positive": False,
                "comment_actions": [],
                "noise_filtered": noise_filtered,
                "noise_samples": samples,
            }
        )
    else:
        existing["head_before"] = head_before
        # Preserve the max noise count across resumes — re-fetching may
        # re-count zero if bot posts have been resolved in the meantime.
        existing["noise_filtered"] = max(existing.get("noise_filtered", 0) or 0, noise_filtered)
        if samples:
            # Keep the earliest sample set; only overwrite if empty.
            existing.setdefault("noise_samples", samples)
            if not existing["noise_samples"]:
                existing["noise_samples"] = samples
    state["current_round"] = n
    state["last_commit_at_round_start"] = head_before
    atomic_write_json(path, state)
    return state


FIXED_ACTIONS = {"fixed"}
# ``pre_existing`` and ``flake`` are CI-only skip reasons: pre_existing
# means the test also fails on the base branch; flake means the failure
# matched a known infrastructure marker (ETXTBSY, bazel-cache, network).
# ``ack`` is a batch-acknowledged low/nit — counts as skipped so summary
# tables reflect that the orchestrator did look at it.
SKIPPED_ACTIONS = {"false_positive", "wont_fix", "stale", "pre_existing", "flake", "ack"}
SEVERITY_ORDER = {"critical": 4, "high": 3, "medium": 2, "low": 1, "nit": 0}


def _top_severity(actions: list[dict[str, Any]]) -> str | None:
    best = None
    best_rank = -1
    for a in actions:
        sev = a.get("severity")
        rank = SEVERITY_ORDER.get(sev or "", -1)
        if rank > best_rank:
            best_rank = rank
            best = sev
    return best


def recompute_counts(actions: list[dict[str, Any]]) -> dict[str, Any]:
    fixed = sum(1 for a in actions if a.get("action") in FIXED_ACTIONS)
    skipped = sum(1 for a in actions if a.get("action") in SKIPPED_ACTIONS)
    return {
        "fixed_count": fixed,
        "skipped_count": skipped,
        "top_severity": _top_severity(actions),
    }


def state_finalize_round(
    ctx: int | str, n: int, head_after: str, actions: list[dict[str, Any]]
) -> dict[str, Any]:
    pr_number, branch = _resolve_ctx(ctx)
    state_dir, path = state_paths(pr_number, branch=branch)
    state = read_json(path, default=None)
    if state is None:
        raise RuntimeError(f"state file not found for ctx {ctx}; call state-append-round first")
    rounds = state.get("rounds") or []
    entry = next((r for r in rounds if r.get("n") == n), None)
    if entry is None:
        raise RuntimeError(f"round {n} not found in state")
    existing = entry.get("comment_actions") or []
    merged = _merge_actions(existing, actions)
    entry["comment_actions"] = merged
    entry["head_after"] = head_after
    entry.update(recompute_counts(merged))
    _persist_round_findings(state_dir, entry, pr_number, branch, n)
    atomic_write_json(path, state)
    return state


def _persist_round_findings(
    state_dir: Path, entry: dict[str, Any], pr_number: int | None, branch: str | None, n: int
) -> None:
    """Copy per-backend bramble envelopes into ``<state_dir>/reviews/`` and
    hydrate ``codex_findings`` / ``cursor_findings`` from them.

    Best-effort: missing envelopes (e.g. a backend that never ran) leave the
    existing array in place. Keeps the raw review text durable after the
    ``/tmp`` envelope is gone. CI findings only populate when a PR number is
    known — branch-only runs skip the ``gh pr checks`` pull.
    """
    # Imported lazily to avoid a top-level circular import between
    # pr_ops and bramble_ops.
    import bramble_ops  # noqa: PLC0415

    envelope_key = pr_number if pr_number is not None else f"branch-{branch}"
    reviews_dir = state_dir / "reviews"
    for backend in bramble_ops.BACKENDS:
        src = bramble_ops.envelope_path(repo_slug(), envelope_key, backend, n)
        if not src.exists():
            continue
        try:
            obj = read_json(src, default=None)
        except Exception:  # noqa: BLE001 — malformed envelope shouldn't brick finalize
            obj = None
        entry[f"{backend}_findings"] = bramble_ops.parse_envelope(obj, source=backend)
        reviews_dir.mkdir(parents=True, exist_ok=True)
        dest = reviews_dir / f"r{n}-{backend}.json"
        try:
            dest.write_text(src.read_text())
        except OSError:
            pass

    # CI findings: whatever ``gh pr checks`` shows as failed right now. Only
    # meaningful with a PR; branch mode leaves the list empty. Best effort —
    # ``gh`` errors leave the existing ``ci_findings`` array alone.
    if pr_number is not None:
        try:
            entry["ci_findings"] = ci_failed_tests(pr_number)
        except Exception:  # noqa: BLE001 — gh failure must not brick finalize
            entry.setdefault("ci_findings", [])
    else:
        entry.setdefault("ci_findings", [])


def _merge_actions(
    existing: list[dict[str, Any]], new: list[dict[str, Any]]
) -> list[dict[str, Any]]:
    """Append new actions; dedupe on (comment_id) or (source, path, line, topic)."""
    by_key: dict[tuple, dict[str, Any]] = {}
    for a in existing:
        by_key[_action_key(a)] = a
    for a in new:
        by_key[_action_key(a)] = a  # new wins on conflict
    return list(by_key.values())


def _action_key(action: dict[str, Any]) -> tuple:
    cid = action.get("comment_id")
    if cid is not None:
        return ("id", cid)
    return (
        "kpl",
        action.get("source"),
        action.get("path"),
        action.get("line"),
        action.get("topic"),
    )


def state_mark_complete(ctx: int | str, reason: str) -> dict[str, Any]:
    pr_number, branch = _resolve_ctx(ctx)
    _, path = state_paths(pr_number, branch=branch)
    state = read_json(path, default=None)
    if state is None:
        raise RuntimeError(f"state file not found for ctx {ctx}")
    state["completed"] = True
    state["exit_reason"] = reason
    state["completed_at"] = _utc_now()
    atomic_write_json(path, state)
    return state


def _utc_now() -> str:
    from datetime import datetime

    return datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


# ---------------------------------------------------------------------------
# Posting replies and top-level comments
# ---------------------------------------------------------------------------


def reply_inline(owner_repo: str, pr: int, comment_id: int, body: str) -> dict[str, Any]:
    path = f"repos/{owner_repo}/pulls/{pr}/comments/{comment_id}/replies"
    res = run(
        ["gh", "api", "--method", "POST", path, "-f", f"body={body}"],
        check=True,
    )
    return json.loads(res.stdout) if res.stdout.strip() else {}


def comment_pr(pr: int, body: str) -> str:
    """Post a top-level PR comment. Returns the URL printed by gh."""
    res = run(
        ["gh", "pr", "comment", str(pr), "--body", body],
        check=True,
    )
    return res.stdout.strip()


# ---------------------------------------------------------------------------
# CLI dispatch
# ---------------------------------------------------------------------------


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="pr_ops", description="pr-polish PR-side operations")
    sub = p.add_subparsers(dest="cmd", required=True)

    sub.add_parser("identify")

    sub.add_parser("fetch-comments")

    sp = sub.add_parser("reply-inline")
    sp.add_argument("comment_id", type=int)
    sp.add_argument("body")

    sp = sub.add_parser("comment-pr")
    sp.add_argument("body")

    sp = sub.add_parser("ci-failed-tests")
    sp.add_argument("--pr", type=int)

    sp = sub.add_parser("ci-compare-base")
    sp.add_argument("--pr", type=int)

    # ``ctx`` on state subcommands is either a bare PR number or
    # ``branch:<name>`` for branch-only runs. Keeping the positional
    # untyped avoids bifurcating the CLI.
    sp = sub.add_parser("state-load")
    sp.add_argument("ctx", help="PR number or 'branch:<name>'")

    sp = sub.add_parser("state-append-round")
    sp.add_argument("ctx", help="PR number or 'branch:<name>'")
    sp.add_argument("n", type=int)
    sp.add_argument("head_before")
    sp.add_argument(
        "--no-verify-head",
        dest="verify_head",
        action="store_false",
        default=True,
        help="Skip the git rev-parse HEAD == head_before check (resume flows only)",
    )
    sp.add_argument(
        "--noise-filtered",
        type=int,
        default=0,
        help="Count of bot process-noise comments dropped at fetch (round 1 only)",
    )
    sp.add_argument(
        "--noise-samples",
        default=None,
        help="Path to JSON file with noise_samples array (capped; debug only)",
    )

    sp = sub.add_parser("state-finalize-round")
    sp.add_argument("ctx", help="PR number or 'branch:<name>'")
    sp.add_argument("n", type=int)
    sp.add_argument("head_after")
    sp.add_argument("actions_file", help="Path to JSON file with comment_actions array")

    sp = sub.add_parser("state-mark-complete")
    sp.add_argument("ctx", help="PR number or 'branch:<name>'")
    sp.add_argument("reason")

    return p


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    try:
        if args.cmd == "identify":
            print_json(identify_pr())
        elif args.cmd == "fetch-comments":
            pr = identify_pr()
            if pr.get("pr_number") is None:
                # Branch-only mode has no PR comments to fetch.
                print_json([])
            else:
                print_json(fetch_comments(pr))
        elif args.cmd == "reply-inline":
            pr = identify_pr()
            if pr.get("pr_number") is None:
                raise RuntimeError("reply-inline requires a PR; current branch has none")
            print_json(reply_inline(pr["owner_repo"], pr["pr_number"], args.comment_id, args.body))
        elif args.cmd == "comment-pr":
            pr = identify_pr()
            if pr.get("pr_number") is None:
                raise RuntimeError("comment-pr requires a PR; current branch has none")
            url = comment_pr(pr["pr_number"], args.body)
            print_json({"url": url})
        elif args.cmd == "ci-failed-tests":
            pr_number = args.pr if args.pr is not None else identify_pr().get("pr_number")
            if pr_number is None:
                print_json([])
            else:
                print_json(ci_failed_tests(pr_number))
        elif args.cmd == "ci-compare-base":
            pr_number = args.pr if args.pr is not None else identify_pr().get("pr_number")
            if pr_number is None:
                print_json({"pre_existing": [], "pr_caused": [], "current_failures": []})
            else:
                print_json(ci_compare_base(pr_number))
        elif args.cmd == "state-load":
            print_json(state_load(args.ctx))
        elif args.cmd == "state-append-round":
            samples = None
            if args.noise_samples:
                samples = json.loads(Path(args.noise_samples).read_text())
                if not isinstance(samples, list):
                    raise ValueError("--noise-samples must point to a JSON array")
            print_json(
                state_append_round(
                    args.ctx,
                    args.n,
                    args.head_before,
                    verify_head=args.verify_head,
                    noise_filtered=args.noise_filtered,
                    noise_samples=samples,
                )
            )
        elif args.cmd == "state-finalize-round":
            actions = json.loads(Path(args.actions_file).read_text())
            if not isinstance(actions, list):
                raise ValueError("actions file must be a JSON array")
            print_json(state_finalize_round(args.ctx, args.n, args.head_after, actions))
        elif args.cmd == "state-mark-complete":
            print_json(state_mark_complete(args.ctx, args.reason))
        else:  # pragma: no cover — argparse enforces.
            raise ValueError(f"unknown cmd: {args.cmd}")
    except CommandError as e:
        print(str(e), file=sys.stderr)
        return e.returncode or 1
    except Exception as e:  # noqa: BLE001 — surface any error as non-zero
        print(f"error: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
