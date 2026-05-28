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
    python3 pr_ops.py state-is-new-series <pr_number> <round>
    python3 pr_ops.py state-append-round <pr_number> <n> <head_before>
    python3 pr_ops.py state-finalize-round <pr_number> <n> <head_after> <actions_json_file>
    python3 pr_ops.py state-mark-complete <pr_number> <reason>

``identify`` detects the current branch and probes for a PR. When the
branch has no PR it still returns branch/base/owner/repo so the
orchestrator can run in branch-only mode. ``sync-base`` is deliberately
NOT a subcommand here — invoke ``.claude/skills/git:sync-base/git-sync.py``
directly. All subcommands print JSON to stdout and exit non-zero on error.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from pathlib import Path
from typing import Any

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

from datetime import UTC

from _common import (  # noqa: E402 — sys.path tweak above
    SOURCE_INLINE,
    SOURCE_ISSUE,
    SOURCE_REVIEW,
    CommandError,
    atomic_write_json,
    current_branch,
    detect_base_branch,
    print_json,
    read_json,
    run,
    severity_rank,
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
        "number,title,url,baseRefName,headRefName,headRefOid",
        "--jq",
        "{pr_number: .number, title: .title, url: .url, base: .baseRefName, "
        "head: .headRefName, head_sha: .headRefOid}",
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
        "head_sha": None,
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
                "original_commit_id": c.get("original_commit_id"),
            }
        )

    for c in issues:
        user = c.get("user") or {}
        body = c.get("body", "") or ""
        # BUGBOT and similar bots post their "found N issues" summary as a
        # top-level issue comment, not a review-level one — so the same
        # filter must run here too. The actionable inline comments come
        # through the comments loop above.
        if _is_bot_review_summary(user, None, body):
            noise.append(_noise_sample(user, c["id"], body, "review-summary"))
            continue
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

    Returns the wrapped shape ``{"comments": [...], "head_sha": str|None,
    "noise_filtered": int, "noise_samples": [...]}``. Each kept inline
    comment carries ``original_commit_id`` and an ``is_stale_prior_commit``
    flag that's true when the comment was anchored to a SHA that has since
    been superseded by ``pr["head_sha"]`` — triage routes those into a
    dedicated bucket so cursor[bot] comments on amended commits don't get
    re-fixed. ``bramble_ops.triage --pr-comments`` accepts either this
    wrapped shape or the legacy bare list for backward compat.
    """
    inline = _fetch_inline_comments(pr["owner_repo"], pr["pr_number"])
    issues = _fetch_issue_comments(pr["owner_repo"], pr["pr_number"])
    reviews = _fetch_reviews(pr["owner_repo"], pr["pr_number"])
    kept, noise = classify_comments(inline, issues, reviews)
    head_sha = pr.get("head_sha")
    for c in kept:
        ocid = c.get("original_commit_id")
        c["is_stale_prior_commit"] = bool(
            head_sha and ocid and ocid != head_sha
        )
    return {
        "comments": kept,
        "head_sha": head_sha,
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
    """Read the state file and decorate it with derived signals.

    Returns the persisted state plus ``is_heartbeat_stale``: a boolean
    computed at read time (never written back) that the orchestrator's
    Step 0.5 resume check uses to distinguish abandoned runs from
    interrupted ones. Stale = ``completed: false`` AND
    ``last_heartbeat_at`` is older than ``HEARTBEAT_STALE_SECONDS`` (or
    missing entirely on a state file written before the heartbeat field
    existed).
    """
    pr, branch = _resolve_ctx(ctx)
    _, path = state_paths(pr, branch=branch)
    state = read_json(path, default={}) or {}
    if state:
        state["is_heartbeat_stale"] = _is_heartbeat_stale(state)
        state["is_first_round_of_series"] = _is_first_round_of_series(
            state, state.get("current_round") or 1
        )
    return state


# Two hours covers the common cases (compaction, network blip, user stepped
# away) without rushing to abandon a legitimate long-running round. Long bramble
# reviews can run ~10 minutes per backend, so a freshly-written heartbeat covers
# even a stalled triage round comfortably; anything past 2h is almost certainly
# a process that won't come back.
HEARTBEAT_STALE_SECONDS = 2 * 60 * 60


def _is_heartbeat_stale(state: dict[str, Any]) -> bool:
    if state.get("completed"):
        return False
    ts = state.get("last_heartbeat_at")
    if not ts:
        # No heartbeat field at all on an in-progress run = either a state
        # file written by a pre-heartbeat orchestrator (treat as stale so we
        # don't wedge forever) or a brand-new run that crashed before its
        # first round-start. Either way, fresh-start is safe.
        return True
    from datetime import datetime

    try:
        # _utc_now writes "%Y-%m-%dT%H:%M:%SZ"; parse the same shape.
        ts_dt = datetime.strptime(ts, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=UTC)
    except (TypeError, ValueError):
        return True
    age = (datetime.now(UTC) - ts_dt).total_seconds()
    return age > HEARTBEAT_STALE_SECONDS


def _is_first_round_of_series(state: dict[str, Any] | None, n: int) -> bool:
    """True when round ``n`` starts a new review series.

    A new series starts when there is no state at all, the prior loop
    set ``completed: true`` (any exit_reason — converged, capped-at-max,
    abandoned, etc.), or this is round 1 with no rounds recorded yet.
    Must be evaluated before ``state_append_round`` clears the
    ``completed`` flag.
    """
    if state is None or not state.get("rounds"):
        return True
    if state.get("completed"):
        return True
    return n == 1


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
                "low_only_streak": 0,
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
    # When the orchestrator re-invokes pr-polish on a state file that
    # already converged (or hit the round cap), the prior loop set
    # completed=true / exit_reason=<reason> / completed_at=<ts>. Adding a
    # new round number means a new loop is starting; clear those fields
    # so mid-loop reads of the state file aren't confusingly inconsistent
    # ("current_round=6 AND completed: converged at <prior timestamp>").
    # state-mark-complete will set them again at this new loop's exit.
    if state.get("completed"):
        state["completed"] = False
        state["exit_reason"] = None
        state["completed_at"] = None
    # Heartbeat pulses on every append — covers fresh starts and resumes.
    # state_load reads this back via _is_heartbeat_stale to distinguish
    # legitimate interruptions from runs the user walked away from.
    state["last_heartbeat_at"] = _utc_now()
    atomic_write_json(path, state)
    return state


FIXED_ACTIONS = {"fixed"}
# ``pre_existing`` and ``flake`` are CI-only skip reasons: pre_existing
# means the test also fails on the base branch; flake means the failure
# matched a known infrastructure marker (ETXTBSY, bazel-cache, network).
# ``ack`` is a batch-acknowledged low/nit — counts as skipped so summary
# tables reflect that the orchestrator did look at it.
SKIPPED_ACTIONS = {"false_positive", "wont_fix", "stale", "pre_existing", "flake", "ack"}


def _top_severity(actions: list[dict[str, Any]]) -> str | None:
    best = None
    best_rank = -1
    for a in actions:
        sev = a.get("severity")
        rank = severity_rank(sev)
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


def _backfill_low_only_streak(prior_rounds: list[dict[str, Any]]) -> int:
    """Reconstruct the streak ending at the most recent prior round when
    its ``low_only_streak`` field is missing (state file from before the
    field existed). Walks rounds in reverse, counting consecutive low-
    only ``top_severity`` values from the most recent backwards. A
    medium-or-higher round resets the count to 0.

    Pure derived-from-history; no I/O.
    """
    streak = 0
    for rnd in sorted(prior_rounds, key=lambda r: r.get("n") or 0, reverse=True):
        if severity_rank(rnd.get("top_severity")) <= severity_rank("low"):
            streak += 1
        else:
            break
    return streak


def _compute_low_only_streak(
    prior_rounds: list[dict[str, Any]], this_top_severity: str | None
) -> int:
    """Increment the prior round's streak when this round's top severity is
    low/nit/None (zero findings counts as low-only); reset to 0 otherwise.

    Walks one round back rather than the whole history — the recurrence is
    streak[N] = streak[N-1] + 1 when this round is low-only, else 0, so we
    only need the most recent value. Round 1 starts the counter at 1 if
    low-only.

    Backwards-compat: when the prior round was finalized by an
    older orchestrator that didn't write ``low_only_streak``, we
    reconstruct it from the persisted ``top_severity`` history. Without
    this, an in-progress audit upgraded mid-loop would lose its streak
    continuity and the new convergence shortcut would not trigger until
    two fresh low rounds accumulated post-upgrade.

    The convergence rule reads streak >= 2 to trigger early exit; B1 reads
    it to inject reviewer-pressure text. Any caller can derive its own
    threshold from the same field.
    """
    is_low_only = severity_rank(this_top_severity) <= severity_rank("low")
    if not is_low_only:
        return 0
    if not prior_rounds:
        return 1
    prev = max(prior_rounds, key=lambda r: r.get("n") or 0)
    prev_streak = prev.get("low_only_streak")
    if prev_streak is None:
        prev_streak = _backfill_low_only_streak(prior_rounds)
    return prev_streak + 1


def state_finalize_round(
    ctx: int | str,
    n: int,
    head_after: str,
    actions: list[dict[str, Any]],
    *,
    envelope_overrides: dict[str, Path] | None = None,
    auto_reply: bool = True,
) -> dict[str, Any]:
    """Finalize a round and persist its results.

    ``envelope_overrides`` maps backend name to the on-disk envelope file
    Monitor captured for that backend (canonically
    ``$STATE_DIR/r<n>/a<attempt>/<backend>-envelope.json`` — the
    attempt-scoped log dir ``round_bundle`` returns). Backends absent from
    the mapping are skipped — finalize hydrates only what was actually run.

    ``auto_reply`` posts a GitHub inline reply on every github-inline row
    whose action ∈ {fixed, stale, false_positive, wont_fix} and which
    doesn't already carry a ``reply_url``. The resulting URL (or per-row
    ``reply_error`` on failure) is written back into the persisted action
    entry. Idempotent across re-runs. Disable for tests / dry-run flows.
    """
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
    if auto_reply and pr_number is not None:
        _post_inline_replies(merged, pr_number, head_after)
    entry["comment_actions"] = merged
    entry["head_after"] = head_after
    entry.update(recompute_counts(merged))
    # ``low_only_streak`` reflects the streak *after* this round closes, so
    # it must be computed from rounds prior to this one (`r.n < n`) plus
    # the freshly recomputed ``top_severity`` for this round.
    prior_rounds = [r for r in rounds if (r.get("n") or 0) < n]
    entry["low_only_streak"] = _compute_low_only_streak(prior_rounds, entry.get("top_severity"))
    _persist_round_findings(state_dir, entry, pr_number, branch, n, envelope_overrides or {})
    atomic_write_json(path, state)
    return state


# Action verbs eligible for an auto-reply on the inline comment they
# triaged. ``ack`` is intentionally omitted: low/nit batch acks would
# generate notification spam without giving the bot useful signal.
_AUTO_REPLY_ACTIONS = ("fixed", "stale", "false_positive", "wont_fix")


def _reply_body(action: dict[str, Any], head_after: str) -> str:
    """Render the auto-reply body for one comment_actions row.

    Bodies match the contract in SKILL.md Step 3.d so consumers (bots,
    humans skimming a PR thread) can grep for the marker phrase. The
    short SHA is the round's head_after — the commit that the round's
    fixes actually landed in.
    """
    short_sha = (head_after or "")[:7]
    verb = action.get("action")
    reason = (action.get("reason") or "").strip()
    if verb == "fixed":
        return f"Fixed in {short_sha}." if short_sha else "Fixed."
    if verb == "stale":
        if short_sha:
            return (
                f"Superseded by {short_sha} — the cited code was changed/removed "
                "in a later commit. (Auto-reply from /pr-polish.)"
            )
        return (
            "Superseded — the cited code was changed/removed in a later commit. "
            "(Auto-reply from /pr-polish.)"
        )
    if verb == "false_positive":
        tail = f": {reason}" if reason else ""
        return f"Marked false positive{tail}. (Auto-reply from /pr-polish.)"
    if verb == "wont_fix":
        tail = f": {reason}" if reason else ""
        return f"Won't fix{tail}. (Auto-reply from /pr-polish.)"
    return ""


def _post_inline_replies(
    actions: list[dict[str, Any]], pr_number: int, head_after: str
) -> None:
    """Post auto-replies on github-inline rows; mutate ``actions`` in place.

    Side-effect: every row that gets a successful reply gains a
    ``reply_url`` key; rows whose POST fails gain a ``reply_error`` key
    (the next finalize attempt retries). Rows already carrying a
    ``reply_url`` are skipped — idempotent across replays. Failures
    are stderr-warned but never raised: the loss of one bot reply must
    not block finalize for the rest of the round.
    """
    eligible = [
        a
        for a in actions
        if a.get("source") == "github-inline"
        and a.get("action") in _AUTO_REPLY_ACTIONS
        and not a.get("reply_url")
        and a.get("comment_id") is not None
    ]
    if not eligible:
        return
    # Resolve owner/repo once. If gh isn't usable here, mark each row's
    # reply_error and bail rather than crashing finalize.
    try:
        _, _, owner_repo = _owner_repo()
    except Exception as e:  # noqa: BLE001 — gh failure must not brick finalize
        msg = f"_owner_repo failed: {e}"
        print(f"pr_ops: auto-reply skipped — {msg}", file=sys.stderr)
        for a in eligible:
            a["reply_error"] = msg
        return
    for a in eligible:
        body = _reply_body(a, head_after)
        if not body:
            continue
        try:
            res = reply_inline(owner_repo, pr_number, int(a["comment_id"]), body)
        except Exception as e:  # noqa: BLE001 — rate limits / deleted comments / network
            msg = str(e)
            a["reply_error"] = msg
            print(
                f"pr_ops: reply-inline comment_id={a.get('comment_id')} failed: {msg}",
                file=sys.stderr,
            )
            continue
        url = (
            res.get("html_url")
            or res.get("url")
            or (
                f"https://github.com/{owner_repo}/pull/{pr_number}#discussion_r{a['comment_id']}"
                if res
                else None
            )
        )
        if url:
            a["reply_url"] = url
            # Clear any prior reply_error left by an earlier failed
            # finalize attempt — the retry succeeded.
            a.pop("reply_error", None)


def _persist_round_findings(
    state_dir: Path,
    entry: dict[str, Any],
    pr_number: int | None,
    branch: str | None,
    n: int,
    envelope_overrides: dict[str, Path],
) -> None:
    """Copy per-backend bramble envelopes into ``<state_dir>/reviews/`` and
    hydrate ``codex_findings`` / ``cursor_findings`` / ``gemini_findings`` /
    ``lint_findings`` from them. Backends absent from ``envelope_overrides``
    are skipped — finalize only persists what the orchestrator actually ran.
    """
    # Imported lazily to avoid a top-level circular import between
    # pr_ops and bramble_ops.
    import bramble_ops  # noqa: PLC0415

    reviews_dir = state_dir / "reviews"
    # Re-finalizing a round with a different envelope set must not leave
    # stale per-backend data behind. Drop any prior `<backend>_findings`,
    # session_ids[backend], and resume_status[backend] for backends NOT
    # in the new envelope set. Without this, a partial re-finalize would
    # mix new findings against the old session_ids and the next round's
    # prior_session_id could resume the wrong session.
    # Treat a missing-on-disk override the same as an absent override:
    # _persist will skip it below, so the in-memory state must be
    # cleared too or stale findings/session_ids would survive.
    incoming = {
        b for b in bramble_ops.BACKENDS
        if (src := envelope_overrides.get(b)) is not None and src.exists()
    }
    # Always run cleanup, even when ``incoming`` is empty. Skipping the
    # zero-envelope case let prior-round per-backend state survive a
    # re-finalize that was supposed to overwrite it.
    for backend in bramble_ops.BACKENDS:
        if backend in incoming:
            continue
        # Reset findings to empty (matches state_append_round's
        # initial seed for codex/cursor); don't pop, so consumers
        # that index into the field unconditionally still work.
        entry[f"{backend}_findings"] = []
        for bucket_key in ("session_ids", "resume_status"):
            bucket = entry.get(bucket_key)
            if isinstance(bucket, dict):
                bucket.pop(backend, None)
                if not bucket:
                    entry.pop(bucket_key, None)
        # Disk parity: an archived envelope on disk would contradict
        # the trimmed in-memory state. Drop the file so post-loop
        # audits see a consistent picture.
        stale_review = state_dir / "reviews" / f"r{n}-{backend}.json"
        try:
            stale_review.unlink(missing_ok=True)
        except OSError:
            pass
    for backend in bramble_ops.BACKENDS:
        src = envelope_overrides.get(backend)
        if src is None or not src.exists():
            continue
        try:
            obj = read_json(src, default=None)
        except Exception:  # noqa: BLE001 — malformed envelope shouldn't brick finalize
            obj = None
        entry[f"{backend}_findings"] = bramble_ops.parse_envelope(obj, source=backend)
        # Clear any prior per-backend session_ids/resume_status/sufficiency
        # before re-hydrating. Without this, an envelope that exists on
        # disk but parses to non-dict / missing keys would let the previous
        # finalize's values survive — same resume-stale-session class
        # of bug as the omitted-backend cleanup above.
        for bucket_key in ("session_ids", "resume_status", "sufficiency_claims"):
            bucket = entry.get(bucket_key)
            if isinstance(bucket, dict):
                bucket.pop(backend, None)
                if not bucket:
                    entry.pop(bucket_key, None)
        if isinstance(obj, dict):
            if obj.get("session_id"):
                entry.setdefault("session_ids", {})[backend] = obj.get("session_id")
            if obj.get("resume_status"):
                entry.setdefault("resume_status", {})[backend] = obj.get("resume_status")
            # v2 schema: the reviewer may emit a per-turn sufficiency
            # claim. Persist it so the round summary and final report
            # can surface it as audit-trail context. Absence is fine —
            # parse_sufficiency returns None when the field isn't there.
            suff = bramble_ops.parse_sufficiency(obj)
            if suff is not None:
                entry.setdefault("sufficiency_claims", {})[backend] = suff
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


_REPLY_PERSIST_KEYS = ("reply_url", "reply_error")


def _merge_actions(
    existing: list[dict[str, Any]], new: list[dict[str, Any]]
) -> list[dict[str, Any]]:
    """Append new actions; dedupe on (comment_id) or (source, path, line, topic).

    On key collision the incoming row wins, but reply-persistence fields
    (``reply_url`` / ``reply_error``) carry forward from the existing row
    when the incoming one omits them. Without this, re-finalizing a round
    from a freshly recomputed action list would drop the reply_url written
    by a prior finalize pass and ``_post_inline_replies`` would repost the
    same inline comment.
    """
    by_key: dict[tuple, dict[str, Any]] = {}
    for a in existing:
        by_key[_action_key(a)] = a
    for a in new:
        key = _action_key(a)
        prior = by_key.get(key)
        if prior is not None:
            for k in _REPLY_PERSIST_KEYS:
                if k in prior and k not in a:
                    a[k] = prior[k]
        by_key[key] = a
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


def state_mark_abandoned(ctx: int | str) -> dict[str, Any]:
    """Tombstone an in-progress run whose heartbeat went stale.

    Writes ``completed: true`` with ``exit_reason: "abandoned"`` so the
    state file's audit trail records that nobody finished it. Distinct from
    ``state_mark_complete`` because the orchestrator calls this without a
    user-facing reason — it's a janitorial action triggered by the Step 0.5
    resume check when ``state_load`` reports ``is_heartbeat_stale: true``.
    The 50-state-file analysis showed 4/50 runs ended with
    ``completed: false, exit_reason: null``; this subcommand closes that gap
    so future analyses can distinguish abandoned from interrupted.
    """
    pr_number, branch = _resolve_ctx(ctx)
    _, path = state_paths(pr_number, branch=branch)
    state = read_json(path, default=None)
    if state is None:
        raise RuntimeError(f"state file not found for ctx {ctx}")
    state["completed"] = True
    state["exit_reason"] = "abandoned"
    state["completed_at"] = _utc_now()
    atomic_write_json(path, state)
    return state


def _utc_now() -> str:
    from datetime import datetime

    return datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


# ---------------------------------------------------------------------------
# Orchestration glue: preflight / round-bundle / finalize-and-report
# ---------------------------------------------------------------------------
#
# These three subcommands compress the mechanical path/state plumbing the
# orchestrator would otherwise rebuild inline each round. They are
# deliberately thin — each returns a JSON dict the agent reads with one
# ``jq -r`` call. Decision points (apply this fix? exit the loop?) stay
# with the agent.


def preflight() -> dict[str, Any]:
    """Resolve the binaries + helper paths the round loop depends on.

    Returns ``{bramble_bin, bramble_resume_supported, git_sync_path,
    git_sync_supports_no_push, skill_dir}``. The orchestrator reads this
    once at session start; missing-but-required pieces produce a
    non-empty ``errors`` list so the agent can fail loudly before the
    first round burns a Monitor budget.

    Each probe is a small subprocess; this is the only place that
    pattern lives now, instead of being copied into the SKILL.md
    template as inline bash that the agent rebuilds verbatim.
    """
    out: dict[str, Any] = {
        "bramble_bin": None,
        "bramble_resume_supported": False,
        "git_sync_path": None,
        "git_sync_supports_no_push": False,
        "skill_dir": str(Path(__file__).resolve().parent.parent),
        "errors": [],
    }
    bin_candidate = Path.cwd() / "bazel-bin/bramble/bramble_/bramble"
    if bin_candidate.is_file() and os.access(bin_candidate, os.X_OK):
        out["bramble_bin"] = str(bin_candidate)
    else:
        out["bramble_bin"] = "bramble"
    try:
        help_res = subprocess.run(
            [out["bramble_bin"], "code-review", "--help"],
            check=False,
            capture_output=True,
            text=True,
            timeout=15,
        )
        out["bramble_resume_supported"] = "--resume-session-id" in (
            (help_res.stdout or "") + (help_res.stderr or "")
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as e:
        out["errors"].append(f"bramble code-review --help failed: {e}")
    if not out["bramble_resume_supported"]:
        out["errors"].append(
            f"{out['bramble_bin']!r} does not support --resume-session-id; "
            "the round loop requires continuous-conversation review"
        )

    # git:sync-base — prefer the repo-local install (matches the code
    # under review); fall back to the user-installed copy in ~/.claude.
    sync_candidates = [
        Path.cwd() / ".claude/skills/git:sync-base/git-sync.py",
        Path.home() / ".claude/skills/git:sync-base/git-sync.py",
    ]
    for cand in sync_candidates:
        if cand.is_file():
            out["git_sync_path"] = str(cand)
            break
    if out["git_sync_path"]:
        try:
            help_res = subprocess.run(
                ["python3", out["git_sync_path"], "--help"],
                check=False,
                capture_output=True,
                text=True,
                timeout=10,
            )
            out["git_sync_supports_no_push"] = "--no-push" in (help_res.stdout or "")
        except subprocess.TimeoutExpired as e:
            out["errors"].append(f"git-sync --help timed out: {e}")
    else:
        out["errors"].append("git:sync-base not found on disk")
    return out


_ATTEMPT_DIR_RE = re.compile(r"a(\d+)$")


def _next_attempt(state_dir: Path, n: int) -> int:
    """Next free attempt index for round ``n`` under ``state_dir``.

    Returns ``max(existing attempt index) + 1`` (first attempt is ``1``),
    where an attempt dir is exactly ``a<number>`` — only those count.
    Matching on the numeric suffix rather than a bare ``a`` prefix keeps
    unrelated dirs (a manual ``archive/``) from bumping the index, and
    taking the max rather than a count means a gap (``a1`` deleted, ``a2``
    kept) still yields a *free* index instead of colliding with ``a2``.
    A resumed round thus gets a fresh attempt dir, which is what keeps the
    Monitor barrier from ever seeing a prior attempt's stale envelope.
    """
    round_dir = state_dir / f"r{n}"
    if not round_dir.is_dir():
        return 1
    indices = [
        int(m.group(1))
        for p in round_dir.iterdir()
        if p.is_dir() and (m := _ATTEMPT_DIR_RE.fullmatch(p.name))
    ]
    return max(indices, default=0) + 1


def round_bundle(ctx: int | str, n: int) -> dict[str, Any]:
    """Return everything the orchestrator needs to arm Monitors for round ``n``.

    Wraps four existing helpers into one call:
      - ``state_load`` for the state + derived booleans (heartbeat,
        is_first_round_of_series).
      - bramble_ops's ``goal_for_round`` + ``prior_session_id`` per
        backend (codex, cursor, gemini).
      - ``state_paths`` for the per-round log directory.

    The bash that arms Monitors becomes one ``round-bundle`` call + a
    ``jq -r`` of the result. Backends without prior session ids return
    empty strings (same shape ``bramble_ops.py prior-session-id``
    prints) so the orchestrator's ``${VAR:+--resume-session-id "$VAR"}``
    expansion keeps working.

    ``head_before`` defaults to ``git rev-parse HEAD`` — the orchestrator
    can override by post-processing the bundle, but the common path
    doesn't need to.

    The log dir is **attempt-scoped** (``r{n}/a{attempt}``). ``attempt``
    is the next free integer for the round (count existing ``a*`` subdirs
    + 1; first attempt is ``a1``). A resumed round therefore gets a fresh
    attempt dir with no envelopes, so the Monitor barrier can never see a
    prior attempt's stale envelope — which is why the orchestrator no
    longer deletes envelopes between attempts.
    """
    import bramble_ops  # noqa: PLC0415

    pr_number, branch = _resolve_ctx(ctx)
    state_dir, state_file = state_paths(pr_number, branch=branch)
    log_dir = state_dir / f"r{n}" / f"a{_next_attempt(state_dir, n)}"
    state = read_json(state_file, default=None)
    is_new_series = 1 if _is_first_round_of_series(state, n) else 0

    head_res = run(["git", "rev-parse", "HEAD"], check=False)
    head_before = head_res.stdout.strip() if head_res.returncode == 0 else ""

    # PR_SUMMARY: leave empty here — building it requires a base-branch
    # diff that the orchestrator already computes once at Step 1. The
    # agent threads it into goal_for_round via a separate arg. Keep this
    # helper bramble-agnostic at the PR_SUMMARY boundary.
    goal_text = ""
    if state is not None:
        try:
            goal_text = bramble_ops.goal_for_round(
                n,
                pr_summary="",
                state=state,
                head_before=head_before or None,
                is_new_series=bool(is_new_series),
            )
        except Exception as e:  # noqa: BLE001 — diagnostic, not fatal
            goal_text = f"# goal_for_round failed: {e}"

    resume_ids: dict[str, str] = {}
    for backend in bramble_ops.BACKENDS:
        try:
            sid = bramble_ops.prior_session_id(
                state,
                backend,
                n,
                is_new_series=bool(is_new_series),
            )
        except Exception:  # noqa: BLE001 — empty resume is the safe fallback
            sid = ""
        resume_ids[backend] = sid or ""

    return {
        "state_dir": str(state_dir),
        "state_file": str(state_file),
        "log_dir": str(log_dir),
        "envelope_paths": {
            backend: str(log_dir / f"{backend}-envelope.json")
            for backend in bramble_ops.BACKENDS
        },
        "head_before": head_before,
        "is_new_series": is_new_series,
        "goal_text": goal_text,
        "resume_ids": resume_ids,
    }


def finalize_and_report(
    ctx: int | str,
    n: int,
    head_after: str,
    actions: list[dict[str, Any]],
    *,
    envelope_overrides: dict[str, Path] | None = None,
) -> dict[str, Any]:
    """Finalize a round and return a one-shot orchestrator-readable report.

    Wraps ``state_finalize_round`` and then computes the audit-trail
    digest the orchestrator displays per-round: top severity, sufficiency
    consensus, low_only_streak, and a short round_summary line. The
    convergence decision stays with the agent — this only surfaces the
    signals consistently so the agent doesn't grep state JSON per field.

    Returns: ``{converged_signal: bool|null, exit_reason_hint: str|null,
    low_only_streak: int, top_severity: str|null, sufficiency_consensus:
    bool|null, sufficiency_claims: dict, next_round_n: int,
    round_summary: str}``.

    ``converged_signal`` is True when the existing rules would fire
    (``low_only_streak >= 2`` OR ``len(action_plan.must_fix) == 0 and
    top_severity ∈ {low, nit, null}``). It's a *hint*, not a gate —
    SKILL.md's convergence prose is still the authoritative reference.
    ``exit_reason_hint`` mirrors the same hint as a string the agent can
    pass to ``state-mark-complete`` if it decides to exit.
    """
    state = state_finalize_round(
        ctx, n, head_after, actions, envelope_overrides=envelope_overrides,
    )
    rounds = state.get("rounds") or []
    entry = next((r for r in rounds if r.get("n") == n), None)
    if entry is None:
        return {"error": f"round {n} missing after finalize"}

    fixed = entry.get("fixed_count") or 0
    skipped = entry.get("skipped_count") or 0
    top_sev = entry.get("top_severity")
    streak = entry.get("low_only_streak") or 0

    claims = entry.get("sufficiency_claims") or {}
    backends_complete = [b for b, c in claims.items() if c.get("is_confident_complete")]
    backends_incomplete = [b for b, c in claims.items() if not c.get("is_confident_complete")]
    # Consensus: ≥2 backends claiming complete with no backend claiming
    # incomplete. None when fewer than 2 backends emitted a claim either
    # way (silent backends count as "no signal").
    if len(claims) < 2:
        consensus: bool | None = None
    elif len(backends_complete) >= 2 and not backends_incomplete:
        consensus = True
    elif backends_incomplete:
        consensus = False
    else:
        consensus = None

    converged: bool | None
    exit_reason_hint: str | None
    low_top = top_sev in (None, "low", "nit")
    if streak >= 2 and low_top:
        converged = True
        exit_reason_hint = "converged"
    elif top_sev in (None, "low", "nit") and fixed == 0 and skipped == 0:
        converged = True
        exit_reason_hint = "all-low"
    else:
        converged = None
        exit_reason_hint = None

    suffix = ""
    if consensus is True:
        suffix = " (both backends signalled sufficiency)"
    elif consensus is False:
        suffix = " (one backend signalled more sites remain)"
    round_summary = (
        f"Round {n}: top={top_sev or 'none'}, fixed {fixed}, skipped {skipped}, "
        f"low_only_streak={streak}{suffix}"
    )

    return {
        "converged_signal": converged,
        "exit_reason_hint": exit_reason_hint,
        "low_only_streak": streak,
        "top_severity": top_sev,
        "sufficiency_consensus": consensus,
        "sufficiency_claims": claims,
        "next_round_n": n + 1,
        "round_summary": round_summary,
    }


# ---------------------------------------------------------------------------
# Posting replies and top-level comments
# ---------------------------------------------------------------------------


def remote_head(branch: str) -> dict[str, Any]:
    """Return the current ``HEAD`` SHA of ``origin/<branch>`` from the remote.

    Uses ``git ls-remote`` rather than ``git rev-parse origin/<branch>``:
    in bare-repo / worktree setups, the local ``origin/<branch>`` ref can
    lag the remote even after a successful push (memory:
    ``feedback_force_with_lease_in_worktrees``). ``ls-remote`` is the
    only reliable way to ask "what does the remote currently hold?"
    without a prior fetch.

    Returns a dict with::
        {
            "branch": "<branch>",
            "local_head": "<sha-or-empty>",     # git rev-parse HEAD
            "remote_head": "<sha-or-empty>",    # git ls-remote origin refs/heads/<branch>
            "in_sync": <bool>,                  # local == remote (both non-empty)
            "remote_present": <bool>,           # remote has the branch at all
        }
    """
    try:
        local = run(["git", "rev-parse", "HEAD"], check=True).stdout.strip()
    except (CommandError, FileNotFoundError):
        local = ""
    try:
        ls = run(
            ["git", "ls-remote", "origin", f"refs/heads/{branch}"], check=False
        )
    except FileNotFoundError:
        return {
            "branch": branch,
            "local_head": local,
            "remote_head": "",
            "in_sync": False,
            "remote_present": False,
        }
    remote = ""
    if ls.returncode == 0:
        # ls-remote output: "<sha>\trefs/heads/<branch>\n" or empty when
        # the branch doesn't exist on the remote.
        first = ls.stdout.splitlines()[:1]
        if first:
            remote = first[0].split()[0].strip()
    return {
        "branch": branch,
        "local_head": local,
        "remote_head": remote,
        "in_sync": bool(local) and bool(remote) and local == remote,
        "remote_present": bool(remote),
    }


def reply_inline(owner_repo: str, pr: int, comment_id: int, body: str) -> dict[str, Any]:
    # Pipe JSON via stdin rather than `-f body=...`: gh treats values starting
    # with `@` as file references, so a comment body starting with `@` would
    # otherwise read a local file or send the wrong payload.
    path = f"repos/{owner_repo}/pulls/{pr}/comments/{comment_id}/replies"
    payload = json.dumps({"body": body})
    res = run(
        ["gh", "api", "--method", "POST", path, "--input", "-"],
        check=True,
        input_text=payload,
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

    sp = sub.add_parser(
        "remote-head",
        help=(
            "Compare local HEAD to origin/<branch> via git ls-remote (not "
            "rev-parse origin/<branch>, which lags in worktrees). Emits "
            "{local_head, remote_head, in_sync, remote_present}."
        ),
    )
    sp.add_argument("branch")

    sp = sub.add_parser("ci-failed-tests")
    sp.add_argument("--pr", type=int)

    sp = sub.add_parser("ci-compare-base")
    sp.add_argument("--pr", type=int)

    # ``ctx`` on state subcommands is either a bare PR number or
    # ``branch:<name>`` for branch-only runs. Keeping the positional
    # untyped avoids bifurcating the CLI.
    sp = sub.add_parser("state-load")
    sp.add_argument("ctx", help="PR number or 'branch:<name>'")

    sp = sub.add_parser(
        "state-is-new-series",
        help="Print 1 if round n starts a new review series, else 0.",
    )
    sp.add_argument("ctx", help="PR number or 'branch:<name>'")
    sp.add_argument("n", type=int)

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
    sp.add_argument(
        "--envelope",
        action="append",
        default=[],
        metavar="<backend>=<path>",
        help=(
            "Per-backend envelope path used to hydrate findings, session_ids, "
            "and resume_status. Repeatable: --envelope codex=... --envelope cursor=.... "
            "Backends not passed are skipped."
        ),
    )

    sp = sub.add_parser("state-mark-complete")
    sp.add_argument("ctx", help="PR number or 'branch:<name>'")
    sp.add_argument("reason")

    sp = sub.add_parser(
        "state-mark-abandoned",
        help="Tombstone a stale-heartbeat run as abandoned (Step 0.5 resume).",
    )
    sp.add_argument("ctx", help="PR number or 'branch:<name>'")

    sub.add_parser(
        "preflight",
        help=(
            "Resolve bramble binary, git-sync path, and probe for "
            "--resume-session-id / --no-push support. Returns one JSON "
            "dict; non-empty errors[] means the round loop should fail "
            "fast before launching Monitors."
        ),
    )

    sp = sub.add_parser(
        "round-bundle",
        help=(
            "Return everything the orchestrator needs to arm Monitors "
            "for round N: log/state paths, envelope paths, head_before, "
            "is_new_series, goal_text, and per-backend resume ids. "
            "Replaces ~6 separate helper invocations."
        ),
    )
    sp.add_argument("ctx", help="PR number or 'branch:<name>'")
    sp.add_argument("n", type=int, help="round number")

    sp = sub.add_parser(
        "finalize-and-report",
        help=(
            "Finalize round N and emit a one-shot audit report "
            "(converged_signal, exit_reason_hint, sufficiency_consensus, "
            "round_summary). Same finalize semantics as "
            "state-finalize-round; the convergence decision stays with "
            "the agent — this only surfaces signals consistently."
        ),
    )
    sp.add_argument("ctx", help="PR number or 'branch:<name>'")
    sp.add_argument("n", type=int, help="round number")
    sp.add_argument("head_after", help="HEAD SHA after this round's commits")
    sp.add_argument("actions_file", help="Path to JSON file with comment_actions array")
    sp.add_argument(
        "--envelope",
        action="append",
        default=[],
        metavar="BACKEND=PATH",
        help="Same shape as state-finalize-round; repeat per backend.",
    )

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
        elif args.cmd == "remote-head":
            print_json(remote_head(args.branch))
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
        elif args.cmd == "state-is-new-series":
            pr, branch = _resolve_ctx(args.ctx)
            _, path = state_paths(pr, branch=branch)
            state = read_json(path, default=None)
            print(1 if _is_first_round_of_series(state, args.n) else 0)
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
            import bramble_ops  # noqa: PLC0415 — lazy to avoid import cost on other subcommands
            envelope_overrides: dict[str, Path] = {}
            for spec in args.envelope:
                if "=" not in spec:
                    raise ValueError(f"--envelope must be <backend>=<path>, got {spec!r}")
                backend, _, ep = spec.partition("=")
                if backend not in bramble_ops.BACKENDS:
                    # Fail fast on typos like ``--envelope curor=...`` —
                    # _persist_round_findings would otherwise silently
                    # ignore the unknown backend and finalize without
                    # hydrating findings.
                    raise ValueError(
                        f"--envelope: unknown backend {backend!r}; "
                        f"expected one of {sorted(bramble_ops.BACKENDS)}"
                    )
                envelope_overrides[backend] = Path(ep)
            if not envelope_overrides:
                # Without envelopes, finalize records comment_actions but
                # not per-backend findings, session_ids, or archived
                # envelopes. Next round's prior_session_id walks past this
                # round and may resume a stale earlier session, breaking
                # continuous-conversation review. Loud stderr warning so
                # orchestrator pilot errors don't go silent.
                print(
                    "pr_ops: state-finalize-round called without --envelope; "
                    "session_ids and per-backend findings will not be "
                    "persisted for this round.",
                    file=sys.stderr,
                )
            print_json(
                state_finalize_round(
                    args.ctx,
                    args.n,
                    args.head_after,
                    actions,
                    envelope_overrides=envelope_overrides,
                )
            )
        elif args.cmd == "state-mark-complete":
            print_json(state_mark_complete(args.ctx, args.reason))
        elif args.cmd == "state-mark-abandoned":
            print_json(state_mark_abandoned(args.ctx))
        elif args.cmd == "preflight":
            print_json(preflight())
        elif args.cmd == "round-bundle":
            print_json(round_bundle(args.ctx, args.n))
        elif args.cmd == "finalize-and-report":
            actions = json.loads(Path(args.actions_file).read_text())
            if not isinstance(actions, list):
                raise ValueError("actions file must be a JSON array")
            import bramble_ops  # noqa: PLC0415
            envelope_overrides: dict[str, Path] = {}
            for spec in args.envelope:
                if "=" not in spec:
                    raise ValueError(f"--envelope must be <backend>=<path>, got {spec!r}")
                backend, _, ep = spec.partition("=")
                if backend not in bramble_ops.BACKENDS:
                    raise ValueError(
                        f"--envelope: unknown backend {backend!r}; "
                        f"expected one of {sorted(bramble_ops.BACKENDS)}"
                    )
                envelope_overrides[backend] = Path(ep)
            print_json(
                finalize_and_report(
                    args.ctx,
                    args.n,
                    args.head_after,
                    actions,
                    envelope_overrides=envelope_overrides,
                )
            )
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
