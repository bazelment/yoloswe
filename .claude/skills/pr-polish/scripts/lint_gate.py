#!/usr/bin/env python3
"""Deterministic lint gate for /pr-polish.

Runs fast static analyzers against the diff, emits a bramble-shaped envelope
that flows through the same parse → triage → action_plan pipeline as the LLM
reviewers. The kernel post-push gap analysis (see
``~/.claude/plans/review-the-recent-50-swirling-ember.md``) showed CodeQL-style
findings (empty ``except`` blocks, unused imports) account for ~40% of the
substantive bot comments that arrive after /pr-polish converges; running the
linters locally turns those into round-N findings instead of post-push noise.

Each linter is OPTIONAL: if its binary is not on ``PATH`` the dispatcher logs
a one-line note to stderr and skips it. We never fail the run on a missing
linter — that would tax users who don't have ruff or golangci-lint installed.

Output:
    ``<state_dir>/r<round>/lint-envelope.json`` — a bramble-shaped envelope
    consumed by ``bramble_ops.parse_envelope`` / ``triage`` via
    ``--stream lint=...``.

Usage:
    python3 lint_gate.py --state-dir <dir> --round <n> [--base BRANCH]

This module never invokes bramble. It's pure subprocess + JSON marshalling.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sys
from pathlib import Path
from typing import Any

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

from _common import (  # noqa: E402
    CommandError,
    atomic_write_json,
    detect_base_branch,
    run,
    topic_of,
)


# ---------------------------------------------------------------------------
# File-set discovery
# ---------------------------------------------------------------------------


def changed_files(base: str) -> list[str]:
    """Return paths of files added/modified/renamed since the merge base.

    Uses ``--diff-filter=AMR`` so deletions don't appear (linting a file that
    no longer exists is wasted work and can confuse linter binaries that
    require an on-disk target).
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
        # base might not exist locally; fall back to merge-base of HEAD with
        # whatever HEAD~ branched from. Conservative: empty list = no findings.
        return []
    return [line for line in res.stdout.splitlines() if line.strip()]


def _bucket(path: str) -> str | None:
    """Classify a path into a linter bucket. Returns None to skip."""
    p = path.lower()
    if p.endswith(".py"):
        return "py"
    if p.endswith(".go"):
        return "go"
    if p.endswith((".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs")):
        return "js"
    return None


# ---------------------------------------------------------------------------
# Per-linter dispatch — pure function from (paths, env) to a list of issues.
# ---------------------------------------------------------------------------


def _have(binary: str) -> bool:
    """``shutil.which``, hoisted so tests can monkey-patch a single seam."""
    return shutil.which(binary) is not None


def run_ruff(paths: list[str]) -> list[dict[str, Any]]:
    """Run ``ruff check --output-format=json`` and normalize.

    Severity mapping is conservative:
      * codes starting ``E9`` or ``F8`` (syntax / undefined-name) → high
      * other ``E`` (pyflakes errors) and ``F`` (logical) → medium
      * everything else (style W, naming N, etc.) → low
    """
    if not paths or not _have("ruff"):
        return []
    res = run(["ruff", "check", "--output-format=json", *paths], check=False)
    # ruff exits non-zero when issues are found; that's expected, not an error.
    if not res.stdout.strip():
        return []
    try:
        items = json.loads(res.stdout)
    except json.JSONDecodeError:
        return []
    out: list[dict[str, Any]] = []
    for it in items:
        code = (it.get("code") or "").upper()
        if code.startswith(("E9", "F8")):
            sev = "high"
        elif code.startswith(("E", "F")):
            sev = "medium"
        else:
            sev = "low"
        msg = (it.get("message") or "").strip()
        loc = it.get("location") or {}
        out.append(
            {
                "file": it.get("filename"),
                "line": loc.get("row"),
                "severity": sev,
                # Topic anchors on the code so two ruff hits at the same line
                # for different rules don't dedupe into one finding.
                "topic": topic_of(f"ruff {code} {msg}"),
                "message": f"[ruff {code}] {msg}",
            }
        )
    return out


def run_golangci(paths: list[str]) -> list[dict[str, Any]]:
    """Run ``golangci-lint run --out-format=json`` and normalize.

    golangci-lint targets packages, not files; we pass the directories of the
    changed Go files so the linter can resolve imports correctly.
    """
    if not paths or not _have("golangci-lint"):
        return []
    pkgs = sorted({str(Path(p).parent) or "." for p in paths})
    res = run(["golangci-lint", "run", "--out-format=json", *pkgs], check=False)
    if not res.stdout.strip():
        return []
    try:
        report = json.loads(res.stdout)
    except json.JSONDecodeError:
        return []
    issues = report.get("Issues") or []
    out: list[dict[str, Any]] = []
    # Conservative severity — linters that typically catch bugs map to medium,
    # style/format linters to low. Anything we don't recognize defaults to low
    # so noise doesn't cascade into must_fix.
    high_linters = {"errcheck", "govet", "staticcheck", "gosec"}
    low_linters = {"lll", "gofmt", "goimports", "whitespace", "wsl"}
    for iss in issues:
        linter = (iss.get("FromLinter") or "").lower()
        if linter in high_linters:
            sev = "medium"
        elif linter in low_linters:
            sev = "low"
        else:
            sev = "low"
        text = (iss.get("Text") or "").strip()
        pos = iss.get("Pos") or {}
        out.append(
            {
                "file": pos.get("Filename"),
                "line": pos.get("Line"),
                "severity": sev,
                "topic": topic_of(f"{linter} {text}"),
                "message": f"[{linter}] {text}",
            }
        )
    return out


def run_eslint(paths: list[str]) -> list[dict[str, Any]]:
    """Run ``eslint --format=json`` and normalize. Severity 2→medium, 1→low."""
    if not paths or not _have("eslint"):
        return []
    res = run(["eslint", "--format=json", *paths], check=False)
    if not res.stdout.strip():
        return []
    try:
        report = json.loads(res.stdout)
    except json.JSONDecodeError:
        return []
    out: list[dict[str, Any]] = []
    for fr in report:
        for m in fr.get("messages") or []:
            sev_int = m.get("severity") or 0
            sev = "medium" if sev_int >= 2 else "low"
            rule = m.get("ruleId") or "eslint"
            text = (m.get("message") or "").strip()
            out.append(
                {
                    "file": fr.get("filePath"),
                    "line": m.get("line"),
                    "severity": sev,
                    "topic": topic_of(f"{rule} {text}"),
                    "message": f"[{rule}] {text}",
                }
            )
    return out


# ---------------------------------------------------------------------------
# Envelope assembly
# ---------------------------------------------------------------------------


def build_envelope(issues: list[dict[str, Any]]) -> dict[str, Any]:
    """Wrap normalized issues in the bramble envelope shape ``parse_envelope``
    expects (status=ok, schema_version=1, review.issues=[...]).
    """
    return {
        "schema_version": 1,
        "status": "ok",
        "backend": "lint",
        "review": {
            "verdict": "advisory",
            "issues": issues,
        },
    }


def envelope_path_for(state_dir: Path, round_: int) -> Path:
    """``<state_dir>/r<n>/lint-envelope.json`` — same layout as the LLM-backend
    envelopes so the orchestrator's ``--stream lint=…`` wiring is symmetrical.
    """
    return state_dir / f"r{round_}" / "lint-envelope.json"


def collect_findings(paths: list[str]) -> list[dict[str, Any]]:
    """Dispatch each path to its linter and concatenate findings. Each linter
    is independent: a missing binary or a parser error in one doesn't suppress
    the others.
    """
    by_bucket: dict[str, list[str]] = {"py": [], "go": [], "js": []}
    for p in paths:
        b = _bucket(p)
        if b is not None:
            by_bucket[b].append(p)
    out: list[dict[str, Any]] = []
    out.extend(run_ruff(by_bucket["py"]))
    out.extend(run_golangci(by_bucket["go"]))
    out.extend(run_eslint(by_bucket["js"]))
    return out


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="lint_gate")
    p.add_argument("--state-dir", required=True, help="<state_dir> from pr_ops identify")
    p.add_argument("--round", dest="round_", type=int, required=True)
    p.add_argument("--base", default=None, help="base branch (default: auto-detect)")
    args = p.parse_args(argv)

    base = args.base or detect_base_branch()
    state_dir = Path(args.state_dir)
    out_path = envelope_path_for(state_dir, args.round_)

    try:
        files = changed_files(base)
    except CommandError as e:
        print(f"lint_gate: changed_files failed: {e}", file=sys.stderr)
        files = []

    issues = collect_findings(files) if files else []
    atomic_write_json(out_path, build_envelope(issues))
    # Print the path so the orchestrator can use it as --stream lint=<path>.
    print(out_path)
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
