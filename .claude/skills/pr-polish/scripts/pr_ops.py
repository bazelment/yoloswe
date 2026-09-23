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
import time
from pathlib import Path
from typing import Any

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

from datetime import UTC

from _common import (  # noqa: E402 — sys.path tweak above
    FIXED_ACTIONS,
    KNOWN_ACTIONS,
    KNOWN_SEVERITIES,
    SKIPPED_ACTIONS,
    SOURCE_INLINE,
    SOURCE_ISSUE,
    SOURCE_REVIEW,
    CommandError,
    action_reason,
    atomic_write_json,
    changed_files,
    current_branch,
    detect_base_branch,
    format_duration_ms,
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
        "number,title,url,body,baseRefName,headRefName,headRefOid",
        "--jq",
        "{pr_number: .number, title: .title, url: .url, body: .body, "
        "base: .baseRefName, head: .headRefName, head_sha: .headRefOid}",
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
        "body": None,
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
# PR summary: the goal's frozen spine, built from the PR's own description
# ---------------------------------------------------------------------------

# Bot-appended blocks, dropped whole. A bot's restatement of the diff is the
# worst possible goal text: it anchors the reviewer on a summary of the code
# instead of the author's intent. Bot mixes vary per repo, so add patterns
# from observed bodies rather than guessing.
_BODY_BLOCK_PATTERNS: tuple[tuple[re.Pattern[str], str], ...] = (
    (
        re.compile(
            r"<!--\s*CURSOR_SUMMARY\s*-->.*?(?:<!--\s*/CURSOR_SUMMARY\s*-->|\Z)",
            re.IGNORECASE | re.DOTALL,
        ),
        "cursor-summary",
    ),
    (
        re.compile(
            r"<!--\s*GREPTILE_SUMMARY\s*-->.*?(?:<!--\s*/GREPTILE_SUMMARY\s*-->|\Z)",
            re.IGNORECASE | re.DOTALL,
        ),
        "greptile-summary",
    ),
    (
        re.compile(
            r"<!--\s*This is an auto-generated comment: release notes by coderabbit\.ai\s*-->"
            r".*?(?:<!--\s*end of auto-generated comment: release notes by coderabbit\.ai\s*-->|\Z)",
            re.IGNORECASE | re.DOTALL,
        ),
        "coderabbit-release-notes",
    ),
    (
        # Fallback: same marker shape, other payloads (walkthrough, tests).
        re.compile(
            r"<!--\s*This is an auto-generated comment: [^>]*coderabbit\.ai\s*-->"
            r".*?(?:<!--\s*end of auto-generated comment: [^>]*coderabbit\.ai\s*-->|\Z)",
            re.IGNORECASE | re.DOTALL,
        ),
        "coderabbit-block",
    ),
    (
        # The linkback's payload is the `<details>` block that follows the
        # marker, so the strip ends there. Running to `\Z` instead deleted
        # every author section that happened to sit after it (a trailing
        # `## Verification` is the common one) — the marker says where bot
        # output STARTS, never that the author wrote nothing below it.
        # Marker-only (no `<details>`) falls back to the marker itself.
        re.compile(
            r"<!--\s*linear-linkback\s*-->\s*(?:<details>.*?(?:</details>|\Z))?",
            re.IGNORECASE | re.DOTALL,
        ),
        "linear-linkback",
    ),
)

# Trailing attribution / process lines that carry no review signal. Anchored
# to the start of a line so prose that merely mentions them survives.
#
# Line-start anchoring alone is not enough: a sentence may legitimately OPEN
# with these words ("Generated with care by the platform team, this migration
# backfills rows."), and stripping the whole line then deletes review context
# — the same failure that retired the `## Deployment Notes` heading rule. So
# each pattern is pinned to the trailer's machine-emitted SHAPE, not just its
# opening words, and must match to end-of-line so a longer prose sentence
# cannot satisfy it.
_BODY_LINE_PATTERNS: tuple[tuple[re.Pattern[str], str], ...] = (
    # Emoji prefix optional — the trailer is emitted both ways. Two shapes,
    # both bounded: a markdown link, or a bare TOOL NAME.
    #
    # "Bare tool name" must stay narrow. An earlier attempt allowed any
    # punctuation-free tail, which still ate author prose ("Generated with
    # care by the platform team") because a short unpunctuated clause is not
    # distinguishable from a tool name by length alone. A product name is 1-3
    # Capitalized/CamelCase words, so require exactly that — connectives like
    # "by"/"from"/"the" are lowercase and disqualify the line.
    (
        re.compile(
            r"^\s*(?:🤖\s*)?Generated with \[[^\]]+\]\([^)]*\)\s*$"
            r"|^\s*(?:🤖\s*)?Generated with (?:[A-Z][\w.+-]*)(?: [A-Z][\w.+-]*){0,2}\s*$",
            re.MULTILINE,
        ),
        "generated-with",
    ),
    # A git trailer: `Name <email>`. Prose that happens to start with the word
    # lacks the angle-bracketed address and survives.
    (
        re.compile(r"^\s*Co-Authored-By:[^<\n]*<[^>\n]+>\s*$", re.MULTILINE | re.IGNORECASE),
        "co-authored-by",
    ),
    (
        re.compile(r"^\s*<sup>\s*Reviewed by \[.*?</sup>\s*$", re.MULTILINE | re.DOTALL),
        "bot-review-footer",
    ),
)

# Deliberately empty, and should stay that way: strip bot-appended noise,
# never author-written sections. A "drop ## Deployment/## Rollout" rule was
# tried and measured — it had no true positives and deleted real review input
# (security posture changes, stacked-PR ordering, migration semantics) that
# authors happened to file under those headings. Heading names are a human
# choice and don't predict content; only machine-emitted delimiters do.
_BODY_SECTION_TITLES: tuple[tuple[re.Pattern[str], str], ...] = ()

# A body that reduces to less than this many characters isn't a usable goal
# spine — fall back to commits + diffstat rather than hand the reviewer a
# stub like "fixes the thing".
_MIN_USABLE_BODY_CHARS = 80


def _strip_section(text: str, pattern: re.Pattern[str]) -> tuple[str, bool]:
    """Drop markdown sections whose heading matches ``pattern``.

    A section ends at the next heading of the same or shallower depth (or EOF),
    so a dropped ``## Deployment`` takes its ``### Steps`` subsection with it
    but leaves the following ``## Verification`` intact.
    """
    lines = text.split("\n")
    out: list[str] = []
    dropped = False
    skip_depth: int | None = None
    for line in lines:
        heading = re.match(r"^(#{1,6})\s", line)
        if skip_depth is not None:
            # Still inside a dropped section until a heading at <= its depth.
            if heading and len(heading.group(1)) <= skip_depth:
                skip_depth = None
            else:
                continue
        m = pattern.match(line)
        if m:
            skip_depth = len(m.group(1))
            dropped = True
            continue
        out.append(line)
    return "\n".join(out), dropped


def strip_pr_body(body: str) -> tuple[str, list[str]]:
    """Return ``(cleaned_body, dropped_tags)`` for a raw PR description.

    Removes bot-appended blocks and attribution trailers; author prose is
    preserved verbatim. Tags (not just a count) so a surprising goal is
    traceable to what was stripped.
    """
    if not body:
        return "", []
    cleaned = body.replace("\r\n", "\n")
    dropped: list[str] = []
    for pattern, tag in _BODY_BLOCK_PATTERNS:
        cleaned, n = pattern.subn("", cleaned)
        if n:
            dropped.append(tag)
    for pattern, tag in _BODY_LINE_PATTERNS:
        cleaned, n = pattern.subn("", cleaned)
        if n:
            dropped.append(tag)
    for pattern, tag in _BODY_SECTION_TITLES:
        cleaned, hit = _strip_section(cleaned, pattern)
        if hit:
            dropped.append(tag)
    # A stripped block usually leaves a `---` rule and blank runs behind it.
    cleaned = re.sub(r"\n\s*---\s*\n\s*\Z", "\n", cleaned)
    cleaned = re.sub(r"\n{3,}", "\n\n", cleaned)
    return cleaned.strip(), dropped


def _commit_diffstat_summary(base: str, max_commits: int = 10) -> str:
    """Commits + diffstat for ``origin/<base>...HEAD`` — the fallback spine."""
    log = run(
        ["git", "log", "--oneline", f"origin/{base}..HEAD"], check=False
    ).stdout.strip()
    commits = "\n".join(log.split("\n")[:max_commits]) if log else ""
    stat = run(
        ["git", "diff", "--stat", f"origin/{base}...HEAD"], check=False
    ).stdout.strip()
    parts = []
    if commits:
        parts.append(f"Commits:\n{commits}")
    if stat:
        parts.append(f"Diffstat:\n{stat}")
    return "\n\n".join(parts)


def build_pr_summary(pr: dict[str, Any] | None = None) -> dict[str, Any]:
    """Build the frozen ``--pr-summary`` spine for round 1.

    A usable PR description IS the summary — the author's statement of intent
    beats a commit list for telling a reviewer what the change is FOR, and so
    what is out of scope. Falls back to commits + diffstat when there is no
    PR, no body, or nothing usable survives stripping.

    Returns ``{"pr_summary", "source", "dropped", "pr_number", "base"}``;
    ``source`` is ``pr-body``, ``pr-body+diffstat``, or ``commits-diffstat``.
    """
    pr = pr or identify_pr()
    base = pr.get("base") or "main"
    title = (pr.get("title") or "").strip()
    body_raw = pr.get("body") or ""
    cleaned, dropped = strip_pr_body(body_raw)

    if not cleaned or len(cleaned) < _MIN_USABLE_BODY_CHARS:
        # Only shell out for the fallback when it's actually needed — on the
        # common path (usable body) these two git calls would be discarded.
        fallback = _commit_diffstat_summary(base)
        text = fallback
        source = "commits-diffstat"
        if cleaned:
            # Thin body: keep it (it may be the only stated intent) but lead
            # with it and let the diffstat carry the shape.
            text = f"{cleaned}\n\n{fallback}".strip()
            source = "pr-body+diffstat"
    else:
        text = cleaned
        source = "pr-body"
    if title:
        text = f"{title}\n\n{text}".strip()
    return {
        "pr_summary": text,
        "source": source,
        "dropped": dropped,
        "pr_number": pr.get("pr_number"),
        "base": base,
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
    pr_summary: str | None = None,
    base_branch: str | None = None,
) -> dict[str, Any]:
    """Start a new round or refresh head_before on an in-progress round.

    ``ctx`` is either the integer PR number or ``branch:<name>``. When
    ``verify_head`` is true (the default), compare the declared
    ``head_before`` against ``git rev-parse HEAD``. A mismatch means the
    orchestrator computed the SHA in one message and a commit landed before
    this call — refuse rather than corrupt the round's lineage.

    ``pr_summary`` is the PR's own statement of intent. It is written on the
    first round of a series that supplies it and not overwritten by later
    rounds in that series: it is the frozen definition of what this PR is for.
    Every later round reads it back from state, because the orchestrator
    computes it in a shell variable at Step 1 that no longer exists by round 2
    — which is why the goal silently lost the PR's purpose after round 1.

    A new series re-anchors it. Freezing across series was observed keeping a
    summary that described half the PR and cited a commit destroyed by a
    squash; ``is_new_series`` is the one boundary where the prior series'
    history is already unreachable, so re-anchoring cannot re-authorize a
    ratcheting PR's growth.

    ``base_branch`` is the PR's base ref (``identify``'s ``base``, i.e. GitHub's
    baseRefName). Frozen and re-anchored the same way, and read back by
    ``round_bundle`` so the "Files in this PR" range is anchored to the PR's
    real ancestor instead of the repo default. The two must agree: PR_SUMMARY's diffstat and the goal's
    file list describing different merge bases is the same class of bug as the
    two-dot range this change replaced.

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
    # Decide the series boundary HERE, while `completed` is still readable.
    # This function is about to clear that flag (below), which destroys the
    # only evidence that a new loop started — so every later consumer in this
    # round would see a restarted series as a continuation and drag the prior
    # series' goal and bramble sessions into a review of different code. The
    # decision is persisted on the round entry so it survives the clear.
    is_new_series = _is_first_round_of_series(state, n)
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
                "is_new_series": is_new_series,
                # Epoch millis, not the second-resolution UTC string: the
                # review-vs-orchestrator split is a difference of these
                # stamps, and a 1s clock hides a degenerate round.
                "opened_at_ms": _epoch_ms(),
            }
        )
    else:
        existing["head_before"] = head_before
        existing.setdefault("opened_at_ms", _epoch_ms())
        # Sticky: the first append of this round made the call while
        # `completed` was still readable. A resumed round re-appends after
        # the flag was already cleared, so re-deriving now would always say
        # "continuation" and silently downgrade a genuine series boundary.
        existing.setdefault("is_new_series", is_new_series)
        # Preserve the max noise count across resumes — re-fetching may
        # re-count zero if bot posts have been resolved in the meantime.
        existing["noise_filtered"] = max(existing.get("noise_filtered", 0) or 0, noise_filtered)
        if samples:
            # Keep the earliest sample set; only overwrite if empty.
            existing.setdefault("noise_samples", samples)
            if not existing["noise_samples"]:
                existing["noise_samples"] = samples
    # Frozen within a series, re-anchored at a series boundary. A PR's remit
    # is fixed when the loop first sees it; re-deriving it every round would
    # let a ratcheting PR keep re-authorizing its own growth, which is the
    # drift the scope contract exists to prevent.
    #
    # But a NEW series (prior loop completed, or the branch was rewritten) is
    # a fresh look at a PR that may have changed underneath the old summary.
    # Observed: after a squash + force-push the frozen summary still described
    # half the PR and cited a commit that no longer existed, so reviewers were
    # judging scope against a stale statement of intent. `is_new_series` is
    # exactly the boundary where re-anchoring is correct and ratcheting is not
    # possible, because the prior series' action history is already unreachable.
    if pr_summary and (is_new_series or not state.get("pr_summary")):
        state["pr_summary"] = pr_summary
    # The PR's own base branch, frozen alongside pr_summary and for the same
    # reason: the file-set range must be measured against the SAME ancestor
    # the PR_SUMMARY diffstat was built from. Without it the goal falls back
    # to the repo default (origin/HEAD), so a PR stacked on a non-default
    # branch gets its "Files in this PR" line measured against main while its
    # summary describes the stacked diff — reintroducing the out-of-scope
    # files this whole change exists to remove.
    # Same boundary rule as pr_summary above: frozen within a series, but a
    # PR retargeted to a new base between series must not keep measuring its
    # file set against the old one.
    if base_branch and (is_new_series or not state.get("base_branch")):
        state["base_branch"] = base_branch
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


# Action-verb and severity vocabularies live in _common (shared with bramble_ops
# so both derive from one source of truth). Imported at the top of this module;
# _validate_actions checks entries against KNOWN_ACTIONS, and _top_severity /
# counting logic classify via the FIXED_ACTIONS / SKIPPED_ACTIONS buckets.


def _validate_actions(actions: list[Any]) -> None:
    """Validate the per-entry shape of an actions list, failing loudly with the
    offending index/field rather than silently persisting garbage.

    Only the closed-enum fields are checked (``action``, ``severity``); unknown
    keys are allowed so forward-compat v2 fields (``invariant``,
    ``spiral_refix``, …) pass through untouched. ``action``/``severity`` are
    each optional per entry, but when present must be recognized.
    """
    for i, entry in enumerate(actions):
        if not isinstance(entry, dict):
            raise ValueError(
                f"actions[{i}] must be an object, got {type(entry).__name__}"
            )
        action = entry.get("action")
        if action is not None and action not in KNOWN_ACTIONS:
            raise ValueError(
                f"actions[{i}].action={action!r} is not a known action; "
                f"expected one of {sorted(KNOWN_ACTIONS)}"
            )
        severity = entry.get("severity")
        if severity is not None and severity not in KNOWN_SEVERITIES:
            raise ValueError(
                f"actions[{i}].severity={severity!r} is not a known severity; "
                f"expected one of {sorted(KNOWN_SEVERITIES)} or null"
            )


def _load_actions(path: Path) -> list[dict[str, Any]]:
    """Read and validate the actions file passed to finalize.

    Accepts either a bare JSON array of action entries, or an object with a
    ``comment_actions`` array (the shape the SKILL.md "State tracking" section
    describes — both are now first-class so the two can't drift). Validates
    inner entries via _validate_actions.
    """
    data = json.loads(path.read_text())
    if isinstance(data, dict) and isinstance(data.get("comment_actions"), list):
        data = data["comment_actions"]
    if not isinstance(data, list):
        raise ValueError(
            'actions file must be a JSON array or {"comment_actions": [...]}'
        )
    _validate_actions(data)
    return data


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


# Deferral verbs that can leave a finding open. ``ack`` is a bare
# acknowledgement (open regardless of reason); ``wont_fix`` is a decision that
# counts as a resolution only when it carries a rationale. ``false_positive``/
# ``stale`` genuinely remove the finding from scope, so they are not deferrals.
# ``pre_existing``/``flake`` are CI-only skips, not code findings the reviewer
# would re-raise, so they don't gate convergence here.
_DEFERRAL_VERBS = {"ack", "wont_fix"}


def _action_is_open_deferral(action: dict[str, Any]) -> bool:
    """True if this action leaves its finding open: a bare ``ack`` (any reason)
    or a ``wont_fix`` with no non-empty reason. A ``wont_fix`` with a real
    rationale is a resolution. The rationale is read from ``reason`` or
    ``notes`` — the actions-file spec treats them as interchangeable.
    """
    verb = action.get("action")
    if verb not in _DEFERRAL_VERBS:
        return False
    if verb == "wont_fix":
        return not action_reason(action)
    return True  # bare ack


def _has_unresolved_high_deferral(rounds: list[dict[str, Any]]) -> bool:
    """True if any high/critical finding's *latest* action across all rounds is
    an open deferral (bare ack, or wont_fix without a cited reason). Such a
    finding is still open and suppresses the automated convergence hint.

    Resolves each finding to its terminal action before deciding: a round-1
    bare ack that a later round records as ``fixed`` (or a reasoned
    ``wont_fix``) on the same finding is resolved and must NOT block. We key on
    _action_key (comment_id, else source/path/line/topic) — the same identity
    _merge_actions dedupes on — and let the highest-round write win.
    """
    latest: dict[tuple, dict[str, Any]] = {}
    for rnd in sorted(rounds, key=lambda r: r.get("n") or 0):
        for a in rnd.get("comment_actions") or []:
            latest[_action_key(a)] = a
    for a in latest.values():
        if severity_rank(a.get("severity")) < severity_rank("high"):
            continue
        if _action_is_open_deferral(a):
            return True
    return False


def _round_counts_as_low_only(
    top_severity: str | None, *, had_live_reviewer: bool
) -> bool:
    """Did this round produce low-only evidence?

    THE single answer to that question. Both writers of ``low_only_streak``
    (finalize and the backfill reconstruction) route through here — asking it
    separately is how the same dead-round defect landed twice, once per writer.

    A round no live model reviewer returned produced no evidence at all, so it
    is not low-only: its ``top_severity`` is None only because nobody reported,
    and ``severity_rank(None)`` sits below "low", which is exactly the
    "nobody looked" == "nothing to find" reading this field's consumers were
    hardened against.
    """
    if not had_live_reviewer:
        return False
    return severity_rank(top_severity) <= severity_rank("low")


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
        live = _round_had_live_reviewer(rnd)
        if not live:
            # A dead round holds the streak: it neither counts as evidence nor
            # terminates the run of low-only rounds before it. Same rule the
            # forward path applies; rounds predating stream_status read as
            # live, so historical reconstruction is unchanged.
            continue
        if _round_counts_as_low_only(rnd.get("top_severity"), had_live_reviewer=live):
            streak += 1
        else:
            break
    return streak


def _round_had_live_reviewer(entry: dict[str, Any]) -> bool:
    """True when at least one MODEL reviewer returned a verdict this round.

    Delegates the live/non-live vocabulary to verdict.py rather than restating
    it: statuses beyond ok/partial/error/absent exist (``exited-empty``,
    ``missing``, ``unknown``) and a second copy of the rule would drift.
    A round with no ``stream_status`` at all is treated as live — "no data" is
    not "nobody looked", matching ``rounds_without_a_live_stream``.
    """
    import verdict as _verdict  # noqa: PLC0415 — lazy: avoid a top-level cycle

    statuses = entry.get("stream_status")
    if not isinstance(statuses, dict) or not statuses:
        return True
    reviewers = {
        k: v for k, v in statuses.items()
        if k not in _verdict._NON_REVIEWER_STREAMS
    }
    if not reviewers:
        return True
    return any(v in _verdict._LIVE_STREAM_STATUSES for v in reviewers.values())


def _compute_low_only_streak(
    prior_rounds: list[dict[str, Any]],
    this_top_severity: str | None,
    *,
    had_live_reviewer: bool = True,
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
    # A round no live reviewer returned in HOLDS the streak — it neither
    # advances nor resets it. Its `top_severity` is None, which ranks below
    # "low", so a dead round used to *increment*: the same "nobody looked"
    # reading as "nothing to find" this field's consumers were fixed for, one
    # call upstream at the writer. Holding beats resetting because the round
    # produced no evidence in either direction.
    #
    # This matters most downstream, where nothing suppresses it: the streak
    # feeds convergence-pressure text into the NEXT round's reviewer goal
    # ("the last N rounds returned only low-severity findings ... returning
    # zero findings is the right call"). Inflated by dead rounds, that tells a
    # healthy reviewer its dead peers found the diff clean — biasing the one
    # surviving reviewer toward silence, which is the failure this loop exists
    # to prevent.
    if not had_live_reviewer:
        prev = max(prior_rounds, key=lambda r: r.get("n") or 0, default=None)
        if prev is None:
            return 0
        held = prev.get("low_only_streak")
        return held if isinstance(held, int) else _backfill_low_only_streak(prior_rounds)

    if not _round_counts_as_low_only(this_top_severity, had_live_reviewer=had_live_reviewer):
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
    review_wall_ms: int | None = None,
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
    # Validate BEFORE any side effect. _post_inline_replies POSTs to GitHub and
    # cannot be undone; a check that runs after it would leave replies on the PR
    # with nothing persisted, and the retry would post them again. Every
    # precondition that can abort this round belongs above this line.
    _reject_orphan_envelopes(state_dir, n, envelope_overrides or {})
    if auto_reply and pr_number is not None:
        _post_inline_replies(merged, pr_number, head_after)
    entry["comment_actions"] = merged
    entry["head_after"] = head_after
    entry.update(recompute_counts(merged))
    # ``low_only_streak`` reflects the streak *after* this round closes, so
    # it must be computed from rounds prior to this one (`r.n < n`) plus
    # the freshly recomputed ``top_severity`` for this round.
    prior_rounds = [r for r in rounds if (r.get("n") or 0) < n]
    # Stream health must be known BEFORE the streak is computed, so persist
    # this round's findings/stream_status first. Ordering is load-bearing:
    # computing the streak from a not-yet-populated entry would read every
    # round as dead.
    _persist_round_findings(state_dir, entry, pr_number, branch, n, envelope_overrides or {})
    entry["low_only_streak"] = _compute_low_only_streak(
        prior_rounds,
        entry.get("top_severity"),
        had_live_reviewer=_round_had_live_reviewer(entry),
    )
    _apply_round_timing(entry, review_wall_ms)
    atomic_write_json(path, state)
    return state


def _epoch_ms() -> int:
    return int(time.time() * 1000)


def _apply_round_timing(entry: dict[str, Any], review_wall_ms: int | None) -> None:
    """Record review wall time vs orchestrator wall time for this round.

    Orchestrator time is everything in the round that was not the review
    join: triage, fixes, and — the case this field exists to make obvious —
    polling a push channel. Issue 247: ~7 minutes of review, ~3 hours of
    wall clock, invisible until someone asked. ``review_wall_ms`` is the
    join duration the launch script measured; it is not inferred from
    heartbeats.
    """
    now = _epoch_ms()
    entry["finalized_at_ms"] = now
    opened = entry.get("opened_at_ms")
    if isinstance(review_wall_ms, int) and review_wall_ms >= 0:
        entry["review_wall_ms"] = review_wall_ms
    if not isinstance(opened, int):
        return
    round_wall = max(0, now - opened)
    entry["round_wall_ms"] = round_wall
    if isinstance(review_wall_ms, int) and review_wall_ms >= 0:
        entry["orchestrator_wall_ms"] = max(0, round_wall - review_wall_ms)


# How many consecutive quiet rounds, after a root was already named, justify
# stopping before the round cap. Two matches the low-only streak: one quiet
# round can be noise, the second says the loop is no longer learning.
_EARLY_CONVERGENCE_ROUNDS = 2


def _action_root(action: dict[str, Any]) -> str | None:
    """The root-issue id a finding was tied to, if the orchestrator named one.

    ``invariant`` is the class key reviewers already emit; ``topic`` and
    ``root_issue`` are the spellings the actions file uses for the same idea.
    """
    for key in ("root_issue", "invariant", "topic"):
        val = action.get(key)
        if isinstance(val, str) and val.strip():
            return val.strip()
    return None


def _round_reports_more_work(entry: dict[str, Any]) -> bool:
    claims = entry.get("sufficiency_claims") or {}
    if not isinstance(claims, dict):
        return False
    return any(
        isinstance(c, dict) and c.get("is_confident_complete") is False
        for c in claims.values()
    )


def _quiet_round_root(entry: dict[str, Any]) -> str | None:
    """The single root this round's findings trace to, or None if the round
    is not quiet.

    Quiet means a live reviewer, no critical/high (blocking) finding, no
    regression marker, no backend saying more sites remain, and every
    action naming the same root. An empty action list is not quiet here —
    that is the existing zero-findings rule, which has its own exit reason.
    """
    if not _round_had_live_reviewer(entry):
        return None
    if _round_reports_more_work(entry):
        return None
    actions = entry.get("comment_actions") or []
    if not actions:
        return None
    root: str | None = None
    for action in actions:
        if severity_rank(action.get("severity")) >= severity_rank("high"):
            return None
        if action.get("regression") or action.get("spiral_refix"):
            return None
        this = _action_root(action)
        if this is None:
            return None
        if root is None:
            root = this
        elif this != root:
            return None
    return root


def early_convergence_justification(rounds: list[dict[str, Any]]) -> str | None:
    """One-line reason to stop before the round cap, or None.

    Fires when the last ``_EARLY_CONVERGENCE_ROUNDS`` rounds are quiet and
    their findings all trace to one root issue that an earlier round already
    identified. Continuing would re-review the same class. The string is the
    record: a run that stops early without it is indistinguishable from one
    that hit the cap and gave up.
    """
    ordered = sorted(rounds, key=lambda r: r.get("n") or 0)
    if len(ordered) < _EARLY_CONVERGENCE_ROUNDS + 1:
        return None
    tail = ordered[-_EARLY_CONVERGENCE_ROUNDS:]
    prior = ordered[:-_EARLY_CONVERGENCE_ROUNDS]
    roots = [_quiet_round_root(r) for r in tail]
    if any(r is None for r in roots):
        return None
    root = roots[0]
    if any(r != root for r in roots):
        return None
    identified_in: int | None = None
    for rnd in prior:
        for action in rnd.get("comment_actions") or []:
            if _action_root(action) == root:
                identified_in = rnd.get("n")
                break
        if identified_in is not None:
            break
    if identified_in is None:
        return None
    return (
        f"early-convergence: {_EARLY_CONVERGENCE_ROUNDS} consecutive rounds "
        "with no critical or high (blocking) findings and no regressions; "
        f"remaining findings trace to root issue {root!r} identified in round {identified_in}"
    )


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
    reason = action_reason(action)
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


def _reject_orphan_envelopes(
    state_dir: Path,
    n: int,
    envelope_overrides: dict[str, Path],
) -> None:
    """Fail loudly when this round's log dir holds an envelope finalize wasn't given.

    Measured on kernel#8682 r1: the orchestrator concluded codex produced
    nothing, wrote ``ack … no envelope``, and finalized WITHOUT
    ``--envelope codex=…``. The envelope landed 2m26s later —
    ``status: ok`` with three findings, one at 0.98 confidence that then cost
    four more rounds to rediscover. Nothing noticed, because a backend absent
    from ``envelope_overrides`` is silently skipped (that skip is correct for a
    reviewer that never launched; it is catastrophic for one that did).

    A finding written to disk and never read is the worst failure this loop
    has, so it is a hard error rather than a warning: the round is not
    finalizable until the orchestrator either passes the envelope or removes
    it. Only backends whose envelope exists AND is non-empty are flagged — a
    zero-byte file is a reviewer that died mid-write, which the
    ``stream-missing`` path handles correctly.

    **Scoped to the attempt being finalized.** ``round_bundle`` allocates a
    FRESH ``a<n>`` dir on resume and deliberately preserves prior attempts
    (see ``_next_attempt``), so a retried round legitimately has valid
    envelopes under earlier attempts that this finalize is not about.
    Scanning the whole ``r<n>`` tree would flag every one of them and make
    resume impossible — a guard against losing findings must not itself break
    the retry path that exists to recover them.

    **A zero-envelope finalize is checked against the LATEST attempt, not
    skipped.** The active attempt is normally derived from the passed envelope
    paths, but "the orchestrator concluded the reviewers produced nothing and
    finalized without them" is exactly the #8682 r1 shape this guard exists
    for — deriving scope only from what was passed would let the motivating
    case through. Upstream merely *warns* on a zero-envelope finalize, so this
    is the only thing standing in front of it.
    """
    import bramble_ops  # noqa: PLC0415

    log_root = state_dir / f"r{n}"
    if not log_root.is_dir():
        return
    passed = {
        p.resolve()
        for p in envelope_overrides.values()
        if p is not None
    }
    # The attempt dirs the orchestrator is actually finalizing. An envelope
    # elsewhere under r<n> belongs to a different attempt and is out of scope.
    active_dirs = {p.parent for p in passed}
    if not active_dirs:
        attempts = sorted(
            (p for p in log_root.iterdir()
             if p.is_dir() and _ATTEMPT_DIR_RE.fullmatch(p.name)),
            key=lambda p: int(_ATTEMPT_DIR_RE.fullmatch(p.name).group(1)),
        )
        if not attempts:
            return
        active_dirs = {attempts[-1].resolve()}
    # A backend whose RECOVERED envelope was passed is accounted for: SKILL
    # Step 3.b runs `recover-envelope`, which writes a sibling
    # `<backend>-envelope-recovered.json` and returns THAT path, so finalize
    # legitimately receives the sibling while the original stays on disk.
    # Without this the documented recovery flow trips the guard and a recovered
    # round cannot finalize at all — a check meant to stop findings being
    # dropped would instead block the mechanism that rescues them.
    # The override must EXIST and be non-empty to cover anything: a filename
    # match alone let a stale, missing, or misspelled path silence the guard for
    # that backend, which is a wider hole than the one the exemption closes —
    # the guard would then say nothing while a real envelope went unread.
    covered_backends = set()
    for b in bramble_ops.BACKENDS:
        src = envelope_overrides.get(b)
        if src is None or not src.name.startswith(f"{b}-envelope"):
            continue
        try:
            if src.exists() and src.stat().st_size > 0:
                covered_backends.add(b)
        except OSError:
            continue
    orphans: list[str] = []
    for backend in bramble_ops.BACKENDS:
        if backend in covered_backends:
            continue
        for found in sorted(log_root.glob(f"a*/{backend}-envelope.json")):
            try:
                resolved = found.resolve()
                if resolved.parent not in active_dirs:
                    continue
                if found.stat().st_size == 0 or resolved in passed:
                    continue
            except OSError:
                continue
            orphans.append(str(found))
    if orphans:
        raise ValueError(
            f"round {n}: envelope(s) on disk were not passed to finalize: "
            + ", ".join(orphans)
            + ". A reviewer that wrote an envelope produced findings — pass it "
            "with --envelope <backend>=<path>, or delete the file if it is a "
            "stale attempt. Silently dropping it loses real review work "
            "(kernel#8682 r1)."
        )


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

    # NOTE: _reject_orphan_envelopes runs in state_finalize_round BEFORE any
    # side effect (GitHub replies), not here — see the comment at that call.
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
        for bucket_key in ("session_ids", "resume_status", "stream_status"):
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
        if src is None:
            # Never launched this round: no claim either way.
            continue
        if not src.exists():
            # Launched and wrote nothing — the crashed/never-returned backend.
            # This is the "absent" half of the live-reviewer rule, and skipping
            # it silently is what let "nobody looked" keep reading as "nothing
            # to find": with no key recorded, the gate had nothing to judge.
            entry.setdefault("stream_status", {})[backend] = "absent"
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
        for bucket_key in (
            "session_ids",
            "resume_status",
            "sufficiency_claims",
            "stream_status",
        ):
            bucket = entry.get(bucket_key)
            if isinstance(bucket, dict):
                bucket.pop(backend, None)
                if not bucket:
                    entry.pop(bucket_key, None)
        if not isinstance(obj, dict):
            # The file exists but did not parse — a reviewer that died
            # mid-write. That is a failed stream, not a missing claim, and
            # leaving no key at all is the same "nobody looked" reads as
            # "nothing to find" hole the `absent` branch above closes.
            entry.setdefault("stream_status", {})[backend] = "unreadable"
        if isinstance(obj, dict):
            # Persist the envelope's own status so downstream consumers can
            # tell "reviewed and found nothing" from "never reviewed". A bare
            # `<backend>_findings: []` cannot: on #8682 it meant a DROPPED
            # envelope in r1 and a real backend error in r2/r3, and any metric
            # reading the array length conflates the two.
            #
            # Consumers: verdict.py (`no_live_reviewer` blocker and the
            # `silent_reviewer` advisory) and SKILL Step 3.g's convergence
            # rule. NOT escape_rate.py — an earlier version of this comment
            # named it, and a comment claiming a consumer that does not exist
            # is what let this field sit write-only for two rounds. Wiring
            # escape_rate to split escapes by stream status is worth doing;
            # until it is, this list stays honest.
            entry.setdefault("stream_status", {})[backend] = (
                obj.get("status") or "unknown"
            )
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


def state_mark_complete(
    ctx: int | str, reason: str, *, justification: str = ""
) -> dict[str, Any]:
    pr_number, branch = _resolve_ctx(ctx)
    _, path = state_paths(pr_number, branch=branch)
    state = read_json(path, default=None)
    if state is None:
        raise RuntimeError(f"state file not found for ctx {ctx}")
    state["completed"] = True
    state["exit_reason"] = reason
    state["completed_at"] = _utc_now()
    # The early-exit reason without this sentence is just a label. The
    # justification is what a later reader checks against the rounds.
    if justification:
        state["exit_justification"] = justification
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


# Path prefixes whose changes mean the review is exercising bramble's OWN
# review code — so a stale prebuilt bramble would review code that differs from
# what's running. Kept here (not in _common) since it's preflight-specific.
_BRAMBLE_SELF_PREFIXES: tuple[str, ...] = (
    "bramble/",
    "yoloswe/reviewer/",
    "agent-cli-wrapper/",
)


def _reviewing_bramble_itself(base: str) -> bool:
    """True when the branch diff vs origin/<base> touches bramble's own review
    code. Best-effort: a git failure yields an empty diff → False (no warning),
    matching changed_files' degrade-gracefully contract.
    """
    files = changed_files(base)
    return any(f.startswith(p) for f in files for p in _BRAMBLE_SELF_PREFIXES)


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
        # Non-fatal advisories (distinct from ``errors``, which abort the run).
        "warnings": [],
    }
    bin_candidate = Path.cwd() / "bazel-bin/bramble/bramble_/bramble"
    used_local_build = bin_candidate.is_file() and os.access(bin_candidate, os.X_OK)
    if used_local_build:
        out["bramble_bin"] = str(bin_candidate)
    else:
        out["bramble_bin"] = "bramble"
        # If this PR/branch modifies bramble's own review code but we fell back
        # to the PATH ``bramble`` (commonly a symlink into a DIFFERENT worktree's
        # build), the review would run against stale code — not what's under
        # test. Warn (don't abort): building is slow and the operator may have a
        # reason, but the missing signal is what bit a hand-driven run.
        try:
            if _reviewing_bramble_itself(detect_base_branch()):
                out["warnings"].append(
                    "PR modifies bramble review code (bramble/, yoloswe/reviewer/, "
                    "agent-cli-wrapper/) but bramble_bin resolved to PATH 'bramble' "
                    "— likely a different worktree's build, so the review may run "
                    "against stale code. Build from this branch "
                    "(`bazel build //bramble/...`) and re-run, or export "
                    "BRAMBLE_BIN=$(pwd)/bazel-bin/bramble/bramble_/bramble."
                )
        except Exception:  # noqa: BLE001 — advisory only; never block preflight
            pass
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
    is allocated by ``_next_attempt``: ``max`` of the numeric suffix of
    existing ``a<number>`` subdirs + 1 (only ``a<number>`` matches;
    first attempt is ``a1``; gap-safe). A resumed round therefore gets a
    fresh attempt dir with no envelopes, so the Monitor barrier can never
    see a prior attempt's stale envelope — which is why the orchestrator
    no longer deletes envelopes between attempts.
    """
    import bramble_ops  # noqa: PLC0415

    pr_number, branch = _resolve_ctx(ctx)
    state_dir, state_file = state_paths(pr_number, branch=branch)
    log_dir = state_dir / f"r{n}" / f"a{_next_attempt(state_dir, n)}"
    state = read_json(state_file, default=None)
    # Prefer the boundary decision state_append_round recorded on this round.
    # Re-deriving it here would read a state whose `completed` flag that same
    # append already cleared, so every restarted series would look like a
    # continuation — inheriting the prior series' goal and bramble sessions.
    # Fall back to deriving only when the round predates the recorded field
    # (older state files) or hasn't been appended yet.
    is_new_series = 1 if _is_first_round_of_series(state, n) else 0
    if state is not None:
        recorded = next(
            (
                r.get("is_new_series")
                for r in (state.get("rounds") or [])
                if r.get("n") == n and r.get("is_new_series") is not None
            ),
            None,
        )
        if recorded is not None:
            is_new_series = 1 if recorded else 0

    head_res = run(["git", "rev-parse", "HEAD"], check=False)
    head_before = head_res.stdout.strip() if head_res.returncode == 0 else ""

    # PR_SUMMARY comes from state, where round 1 froze it. It used to be
    # passed as "" here on the theory that the orchestrator threaded it in
    # separately — but the SKILL only sets GOAL=$PR_SUMMARY when ROUND=1, so
    # from round 2 on the goal carried no statement of what the PR was for.
    # A round-16 reviewer saw action history and a diff, with the PR's own
    # purpose absent entirely.
    goal_text = ""
    if state is not None:
        try:
            goal_text = bramble_ops.goal_for_round(
                n,
                pr_summary=state.get("pr_summary") or "",
                state=state,
                head_before=head_before or None,
                is_new_series=bool(is_new_series),
                base_branch=state.get("base_branch") or None,
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
    review_wall_ms: int | None = None,
) -> dict[str, Any]:
    """Finalize a round and return a one-shot orchestrator-readable report.

    Wraps ``state_finalize_round`` and then computes the audit-trail
    digest the orchestrator displays per-round: top severity, sufficiency
    consensus, low_only_streak, and a short round_summary line. The
    convergence decision stays with the agent — this only surfaces the
    signals consistently so the agent doesn't grep state JSON per field.

    Returns: ``{converged_signal: bool|null, exit_reason_hint: str|null,
    convergence_justification: str|null, low_only_streak: int,
    top_severity: str|null, sufficiency_consensus: bool|null,
    sufficiency_claims: dict, next_round_n: int, round_summary: str,
    review_wall_ms: int|null, orchestrator_wall_ms: int|null}``.

    ``converged_signal`` is True when the existing rules would fire
    (``low_only_streak >= 2`` OR ``len(action_plan.must_fix) == 0 and
    top_severity ∈ {low, nit, null}``). It's a *hint*, not a gate —
    SKILL.md's convergence prose is still the authoritative reference.
    ``exit_reason_hint`` mirrors the same hint as a string the agent can
    pass to ``state-mark-complete`` if it decides to exit.
    """
    state = state_finalize_round(
        ctx, n, head_after, actions, envelope_overrides=envelope_overrides,
        review_wall_ms=review_wall_ms,
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
    # A high/critical finding that was only acknowledged/deferred (bare ack or
    # wont_fix with no cited reason) in THIS or any prior round keeps the loop
    # open, even when the current round's severity/streak would otherwise
    # signal convergence. This mirrors SKILL.md Step 3.g's "acknowledged !=
    # resolved" guard on the automated hint (memory: the INF-1711 lesson —
    # a deferred high must not read as resolved). We scan persisted
    # comment_actions rather than the current round's `actions` arg so a high
    # ack'd several rounds ago still blocks.
    # "Nobody looked" must not read as "nothing to find" IN THE LOOP. verdict.py
    # gates on this too, but only at exit — by then the batch has already
    # pushed, so the in-loop hint has to carry the same rule or the recovery it
    # exists to trigger never happens.
    #
    # Scoped to THIS round, unlike verdict.py's series-wide blocker. The two
    # answer different questions: "is this batch's record trustworthy" is
    # rightly permanent, but "may this round converge" must clear when the
    # reviewers recover — a series-wide read here meant one dead round blocked
    # convergence for every round after it, so a recovered run could never
    # signal done and would burn its budget to the cap.
    no_live_reviewer = not _round_had_live_reviewer(entry)

    deferred_high = _has_unresolved_high_deferral(rounds)
    justification: str | None = None
    if deferred_high or no_live_reviewer:
        converged = None
        exit_reason_hint = None
    elif (early := early_convergence_justification(rounds)):
        # Before the streak/all-low rules so a run that is still finding
        # medium notes of one known root stops with the reason that matches
        # the evidence, instead of waiting for a low-only streak that medium
        # findings reset.
        converged = True
        exit_reason_hint = "early-convergence"
        justification = early
    elif streak >= 2 and low_top:
        converged = True
        exit_reason_hint = "converged"
        justification = (
            f"low_only_streak={streak}: consecutive rounds had no blocking findings"
        )
    elif top_sev in (None, "low", "nit") and fixed == 0 and skipped == 0:
        converged = True
        exit_reason_hint = "all-low"
        justification = "no findings left to fix or skip"
    else:
        converged = None
        exit_reason_hint = None
    if justification:
        entry["convergence_justification"] = justification
        pr_number, branch = _resolve_ctx(ctx)
        _, path = state_paths(pr_number, branch=branch)
        atomic_write_json(path, state)

    suffix = ""
    if consensus is True:
        suffix = " (both backends signalled sufficiency)"
    elif consensus is False:
        suffix = " (one backend signalled more sites remain)"
    review_s = format_duration_ms(entry.get("review_wall_ms"))
    orch_s = format_duration_ms(entry.get("orchestrator_wall_ms"))
    round_summary = (
        f"Round {n}: top={top_sev or 'none'}, fixed {fixed}, skipped {skipped}, "
        f"low_only_streak={streak}, review={review_s} orchestrator={orch_s}{suffix}"
    )

    return {
        "converged_signal": converged,
        "exit_reason_hint": exit_reason_hint,
        "convergence_justification": justification,
        "low_only_streak": streak,
        "top_severity": top_sev,
        "sufficiency_consensus": consensus,
        "sufficiency_claims": claims,
        "next_round_n": n + 1,
        "round_summary": round_summary,
        "review_wall_ms": entry.get("review_wall_ms"),
        "orchestrator_wall_ms": entry.get("orchestrator_wall_ms"),
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

    sp = sub.add_parser(
        "pr-summary",
        help=(
            "Build the frozen --pr-summary spine for round 1. Prefers the "
            "PR's own description (bot blocks and post-merge sections "
            "stripped); falls back to commits + diffstat when there is no "
            "PR or no usable body. Emits {pr_summary, source, dropped}."
        ),
    )
    sp.add_argument(
        "--text-only",
        action="store_true",
        help="Print just the summary text (for direct use as --goal / --pr-summary)",
    )

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
    sp.add_argument(
        "--pr-summary",
        default=None,
        help=(
            "The PR's own statement of intent. Written once per series and not "
            "overwritten by later rounds in it — the frozen definition of what "
            "this PR is for, which every later round reads back from state. A "
            "new series re-anchors it (a squashed or force-pushed branch makes "
            "the old one stale). Without it the goal loses the PR's purpose "
            "after round 1."
        ),
    )
    sp.add_argument(
        "--base-branch",
        default=None,
        help=(
            "The PR's base ref (identify's 'base'). Frozen per series like "
            "--pr-summary, and used to anchor the 'Files in this PR' range to "
            "the same merge base the PR summary was built against. Omit it and "
            "a PR stacked on a non-default branch is measured against the repo "
            "default instead."
        ),
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
    sp.add_argument(
        "--review-wall-ms",
        type=int,
        default=None,
        help=(
            "Wall time of this round's review join, in milliseconds. "
            "Orchestrator time is the rest of the round. Omit and the "
            "summary shows review=n/a."
        ),
    )

    sp = sub.add_parser("state-mark-complete")
    sp.add_argument("ctx", help="PR number or 'branch:<name>'")
    sp.add_argument("reason")
    sp.add_argument(
        "--justification",
        default="",
        help=(
            "One-line reason the loop stopped, required for early-convergence "
            "so the exit is auditable."
        ),
    )

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
    sp.add_argument(
        "--review-wall-ms",
        type=int,
        default=None,
        help="Wall time of this round's review join, in milliseconds.",
    )

    return p


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    try:
        if args.cmd == "identify":
            print_json(identify_pr())
        elif args.cmd == "pr-summary":
            result = build_pr_summary()
            if args.text_only:
                print(result["pr_summary"])
            else:
                print_json(result)
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
                    pr_summary=args.pr_summary,
                    base_branch=args.base_branch,
                    noise_samples=samples,
                )
            )
        elif args.cmd == "state-finalize-round":
            actions = _load_actions(Path(args.actions_file))
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
                    review_wall_ms=args.review_wall_ms,
                )
            )
        elif args.cmd == "state-mark-complete":
            print_json(
                state_mark_complete(
                    args.ctx, args.reason, justification=args.justification
                )
            )
        elif args.cmd == "state-mark-abandoned":
            print_json(state_mark_abandoned(args.ctx))
        elif args.cmd == "preflight":
            print_json(preflight())
        elif args.cmd == "round-bundle":
            print_json(round_bundle(args.ctx, args.n))
        elif args.cmd == "finalize-and-report":
            actions = _load_actions(Path(args.actions_file))
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
                    review_wall_ms=args.review_wall_ms,
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
