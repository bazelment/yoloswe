#!/usr/bin/env python3
"""Compare r1 (narrow) vs r2 (scope-widened) bramble code-review envelopes.

For each kernel PR, loads pp-comments.json (substantive bot findings),
then checks whether the r2 envelope (cursor or codex) surfaced the same
finding. Prints a per-PR table and a per-finding verdict.

Usage:
    python3 compare-r1-r2.py [--bramble-projects DIR] [--r2-dir DIR]
                              [--prs PR1,PR2,...] [--json]

Defaults:
    --bramble-projects ~/.bramble/projects
    --r2-dir           /tmp/issue-178-verify
    --prs              2978,2799,2998,2755
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from pathlib import Path
from typing import Any


# ---------------------------------------------------------------------------
# Bot-comment noise filter
# ---------------------------------------------------------------------------

NOISE_AUTHORS: set[str] = {
    "github-actions[bot]",
    "github-code-quality[bot]",
}

# CodeQL / github-code-quality patterns that are lint-gate noise
NOISE_BODY_PATTERNS: list[re.Pattern] = [
    re.compile(r"Unused (import|global variable|local variable)", re.IGNORECASE),
    re.compile(r"Statement has no effect", re.IGNORECASE),
    re.compile(r"OpenTofu Plan", re.IGNORECASE),
    re.compile(r"Pulumi report", re.IGNORECASE),
    re.compile(r"Claude finished @", re.IGNORECASE),
]

NOISE_SOURCES: set[str] = {"github-issue", "github-review"}


def is_substantive(comment: dict) -> bool:
    """Return True if the comment is a substantive bot bug-finding."""
    author = comment.get("author", "")
    if author in NOISE_AUTHORS:
        return False
    source = comment.get("source", "")
    if source in NOISE_SOURCES:
        return False
    body = comment.get("body", "")
    for pat in NOISE_BODY_PATTERNS:
        if pat.search(body):
            return False
    return comment.get("is_bot", False)


# ---------------------------------------------------------------------------
# Envelope loading
# ---------------------------------------------------------------------------

def load_envelope(path: Path) -> dict | None:
    """Load a bramble envelope JSON.

    Envelopes are usually a whole-file JSON dict (the ``--envelope-file``
    output). Some captures are NDJSON: zero or more progress lines followed
    by exactly one terminal envelope identified by the ``schema_version``
    top-level key, and require ``status`` too — matches bramble_ops'
    extract_terminal_envelope contract so an envelope accepted here
    isn't silently rejected downstream.
    """
    if not path.exists():
        return None
    try:
        text = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        return None

    def _is_envelope(o: object) -> bool:
        return isinstance(o, dict) and "schema_version" in o and "status" in o

    try:
        obj = json.loads(text)
        if _is_envelope(obj):
            return obj
    except json.JSONDecodeError:
        pass
    for line in reversed(text.splitlines()):
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            continue
        if _is_envelope(obj):
            return obj
    return None


def envelope_issues(env: dict | None) -> list[dict]:
    if env is None:
        return []
    # Envelope may nest issues under 'review.issues' or at top level.
    review = env.get("review", {})
    if review and "issues" in review:
        return review["issues"]
    return env.get("issues", [])


# ---------------------------------------------------------------------------
# Matching logic
# ---------------------------------------------------------------------------

def _file_tail(path: str | None) -> str:
    """Return the filename portion only."""
    if not path:
        return ""
    return os.path.basename(path)


def _normalize_body(body: str) -> str:
    """Strip Markdown formatting and lower-case for fuzzy matching."""
    body = re.sub(r"<[^>]+>", " ", body)          # strip HTML
    body = re.sub(r"[`*#\[\]()_~]", " ", body)    # strip MD markup
    body = body.lower()
    body = re.sub(r"\s+", " ", body).strip()
    return body


def _kw_overlap(a: str, b: str, threshold: int = 3) -> bool:
    """True if a and b share >= threshold significant tokens (4+ chars)."""
    ta = set(w for w in a.split() if len(w) >= 4)
    tb = set(w for w in b.split() if len(w) >= 4)
    return len(ta & tb) >= threshold


def comment_caught_in_envelope(comment: dict, issues: list[dict]) -> tuple[bool, str]:
    """Check whether any issue in the envelope matches the bot comment.

    Match strategy (any of):
    1. Same file basename + line within ±10
    2. Same file basename + significant keyword overlap in body/message
    3. No file on comment + significant keyword overlap (cross-file findings)
    """
    c_path = comment.get("path") or ""
    c_line = comment.get("line")
    c_body = _normalize_body(comment.get("body", ""))
    c_file = _file_tail(c_path)

    for issue in issues:
        i_path = issue.get("file", "") or ""
        i_line = issue.get("line")
        i_file = _file_tail(i_path)
        i_msg = _normalize_body((issue.get("message") or "") + " " + (issue.get("suggestion") or ""))

        # File match
        file_match = c_file and i_file and c_file == i_file

        # Line proximity
        line_match = False
        if c_line and i_line:
            try:
                line_match = abs(int(c_line) - int(i_line)) <= 10
            except (TypeError, ValueError):
                pass

        kw_match = _kw_overlap(c_body, i_msg)

        # ``confidence`` may be missing, JSON null, a string, or any other
        # non-numeric — coerce defensively so a single malformed field can't
        # abort the whole batch with TypeError.
        raw_conf = issue.get("confidence")
        try:
            conf = float(raw_conf) if raw_conf is not None else 1.0
        except (TypeError, ValueError):
            conf = 1.0
        conf_str = f", conf={conf:.2f}" if conf < 1.0 else ""
        if file_match and (line_match or kw_match):
            return True, f"{i_file}:{i_line} ({issue.get('severity')}{conf_str})"
        if not c_file and kw_match:
            return True, f"{i_file}:{i_line} ({issue.get('severity')}{conf_str})"

    return False, ""


# ---------------------------------------------------------------------------
# Per-PR comparison
# ---------------------------------------------------------------------------

def compare_pr(
    pr: str,
    bramble_dir: Path,
    r2_dir: Path,
    verbose: bool = False,
) -> dict:
    pr_dir = bramble_dir / f"kernel-{pr}"
    r2_state = r2_dir / f"kernel-{pr}"

    # Load bot comments. Tolerate corrupt / partially-written JSON: when
    # comparing across many PRs we'd rather skip one with empty comments
    # than abort the entire batch.
    comments_path = pr_dir / "pp-comments.json"
    all_comments: list[dict] = []
    if comments_path.exists():
        try:
            data = json.loads(comments_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError, UnicodeDecodeError) as exc:
            print(f"warning: skipping unreadable {comments_path}: {exc}", file=sys.stderr)
            data = None
        if isinstance(data, dict):
            all_comments = data.get("comments", []) or []
        elif isinstance(data, list):
            all_comments = data

    substantive = [c for c in all_comments if is_substantive(c)]

    # Load r1 and r2 envelopes
    r1_cursor_env = load_envelope(pr_dir / "r1" / "cursor-envelope.json")
    r1_codex_env  = load_envelope(pr_dir / "r1" / "codex-envelope.json")
    r2_cursor_env = load_envelope(r2_state / "cursor-r2-envelope.json")
    r2_codex_env  = load_envelope(r2_state / "codex-r2-envelope.json")

    r1_issues = envelope_issues(r1_cursor_env) + envelope_issues(r1_codex_env)
    r2_issues = envelope_issues(r2_cursor_env) + envelope_issues(r2_codex_env)

    findings: list[dict] = []
    caught_count = 0

    for c in substantive:
        caught_r1, match_r1 = comment_caught_in_envelope(c, r1_issues)
        caught_r2, match_r2 = comment_caught_in_envelope(c, r2_issues)
        caught = caught_r2
        if caught:
            caught_count += 1

        body_snippet = c.get("body", "")[:120].replace("\n", " ")
        findings.append({
            "file": c.get("path", ""),
            "line": c.get("line"),
            "author": c.get("author", ""),
            "snippet": body_snippet,
            "caught_r1": caught_r1,
            "match_r1": match_r1,
            "caught_r2": caught,
            "match_r2": match_r2,
        })

    # Regression check for 2755: any r1 finding missing in r2?
    # Run the loop unconditionally — when r2 is empty, every r1 finding is
    # by definition r1-only. Guarding on `if r2_issues` would silently
    # report zero regressions in the worst case (r2 envelope missing).
    r1_only = 0
    for issue in r1_issues:
        caught, _ = comment_caught_in_envelope(
            {"path": issue.get("file"), "line": issue.get("line"),
             "body": issue.get("message", ""), "is_bot": True},
            r2_issues
        )
        if not caught:
            r1_only += 1

    return {
        "pr": pr,
        "total_substantive": len(substantive),
        "caught": caught_count,
        "findings": findings,
        "r1_issue_count": len(r1_issues),
        "r2_issue_count": len(r2_issues),
        "r1_only_count": r1_only,
        "r2_cursor_loaded": r2_cursor_env is not None,
        "r2_codex_loaded": r2_codex_env is not None,
    }


# ---------------------------------------------------------------------------
# Output formatting
# ---------------------------------------------------------------------------

def format_results(results: list[dict], verbose: bool = False) -> tuple[str, bool]:
    lines: list[str] = []
    lines.append("## Phase 3 Verification: r1 (narrow) vs r2 (scope-widened)")
    lines.append("")

    all_pass = True
    verdicts: list[str] = []

    for r in results:
        pr = r["pr"]
        total = r["total_substantive"]
        caught = r["caught"]
        r1_count = r["r1_issue_count"]
        r2_count = r["r2_issue_count"]
        r1_only = r["r1_only_count"]

        if pr == "2755":
            regressions = r1_only
            status = "✓" if regressions == 0 else "✗"
            line = (f"**kernel-{pr}**: regressions={regressions}  "
                    f"findings r1={r1_count} r2={r2_count} "
                    f"({'+'  if r2_count >= r1_count else ''}{r2_count - r1_count})")
            verdicts.append(f"{status} {line}")
            if regressions > 0:
                all_pass = False
        else:
            # Acceptance bar: every substantive comment must be caught.
            # Partial recall (caught < total) and total==0 with caught==0
            # both used to read as a pass, masking real regressions.
            if total == 0:
                status = "·"  # nothing to verify; neither pass nor fail.
                line = f"**kernel-{pr}**: caught={caught}/{total} (skipped — no substantive comments)"
            else:
                status = "✓" if caught == total else "✗"
                line = f"**kernel-{pr}**: caught={caught}/{total}"
                if caught != total:
                    all_pass = False
            for f in r["findings"]:
                check = "✓" if f["caught_r2"] else "✗"
                fpath = os.path.basename(f["file"]) if f["file"] else "(issue-level)"
                snippet = f["snippet"][:80].replace("|", "\\|")
                line += f"  {check} {fpath}: {snippet}"
            verdicts.append(f"{status} {line}")

    for v in verdicts:
        lines.append(v)
        lines.append("")

    return "\n".join(lines), all_pass


# ---------------------------------------------------------------------------
# Narrative generation
# ---------------------------------------------------------------------------

def narrative(results: list[dict]) -> str:
    """Generate a per-PR narrative."""
    paras: list[str] = []

    for r in results:
        pr = r["pr"]
        total = r["total_substantive"]
        caught = r["caught"]
        r2_c = r["r2_cursor_loaded"]
        r2_x = r["r2_codex_loaded"]

        backend_note = ""
        if not r2_c:
            backend_note = " (cursor r2 envelope missing)"
        if not r2_x:
            backend_note += " (codex r2 envelope missing)"

        paras.append(f"### kernel-{pr}{backend_note}")

        for f in r["findings"]:
            check = "✓" if f["caught_r2"] else "✗"
            fpath = os.path.basename(f["file"]) if f["file"] else "(no file)"
            if f["caught_r2"]:
                matched = f["match_r2"]
                was_r1 = " (also in r1)" if f["caught_r1"] else " (new in r2 — wider scope helped)"
                paras.append(f"- {check} **{fpath}**: caught{was_r1}. Matched envelope finding {matched}.")
            else:
                was_r1 = " (was in r1)" if f["caught_r1"] else " (missed by both r1 and r2)"
                paras.append(f"- {check} **{fpath}**: NOT caught{was_r1}. The file was in scope but the specific pattern was not flagged.")

        if r["pr"] == "2755":
            paras.append(f"- Regression check: r1={r['r1_issue_count']} findings, r2={r['r2_issue_count']} findings, {r['r1_only_count']} r1 findings not carried over to r2.")

        paras.append("")

    return "\n".join(paras)


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--bramble-projects",
                   default=str(Path.home() / ".bramble" / "projects"),
                   help="Root of bramble project state directories")
    p.add_argument("--r2-dir",
                   default="/tmp/issue-178-verify",
                   help="Root of r2 envelope directories")
    p.add_argument("--prs", default="2978,2799,2998,2755",
                   help="Comma-separated PR numbers")
    p.add_argument("--json", action="store_true",
                   help="Emit JSON instead of Markdown")
    p.add_argument("--verbose", action="store_true")
    args = p.parse_args(argv)

    bramble_dir = Path(args.bramble_projects)
    r2_dir = Path(args.r2_dir)
    prs = [pr.strip() for pr in args.prs.split(",")]

    results = []
    for pr in prs:
        result = compare_pr(pr, bramble_dir, r2_dir, verbose=args.verbose)
        results.append(result)

    # Always compute all_pass so --json shares the same exit-code contract
    # as Markdown mode. CI gating on Phase-3 outcomes only worked in the
    # Markdown path before — --json silently returned 0 even on regression.
    _, all_pass = format_results(results)

    if args.json:
        payload = {"results": results, "all_pass": all_pass}
        print(json.dumps(payload, indent=2))
        return 0 if all_pass else 1

    table, _ = format_results(results)
    print(table)
    print()
    print(narrative(results))
    print()
    if all_pass:
        print("**Overall verdict: ✓ Phase 3 PASSED** — all acceptance bars met.")
    else:
        print("**Overall verdict: ✗ Phase 3 INCOMPLETE** — some acceptance bars not met.")

    return 0 if all_pass else 1


if __name__ == "__main__":
    raise SystemExit(main())
