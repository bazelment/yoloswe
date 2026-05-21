#!/usr/bin/env python3
"""Harvest past bramble code-review runs into a structured eval dataset.

Walks ``~/.bramble/projects/<repo>-<pr>/`` directories produced by the
``/pr-polish`` skill and emits one JSON record per PR (R1 + final round
only), plus a top-level ``index.json``. The per-PR record carries
ground-truth labels derived from ``comment_actions.action`` and enough
metadata to replay ``bramble code-review`` apple-to-apple later.

See ``README.md`` for the dataset schema and matching rules.
"""

from __future__ import annotations

import argparse
import datetime as _dt
import subprocess
import sys
from pathlib import Path
from typing import Optional

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import harvest_lib as hl  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parents[4]  # yoloswe worktree root
DEFAULT_BRAMBLE_OPS = (
    REPO_ROOT / ".claude" / "skills" / "pr-polish" / "scripts" / "bramble_ops.py"
)
DEFAULT_SOURCE_DIR = Path.home() / ".bramble" / "projects"
# The dataset lives OUTSIDE the repo: it is derived from real private PRs
# (file paths, commit SHAs, reviewer findings) and must never be committed.
# It sits next to ~/.bramble/projects/, the pr-polish data it is built from.
DEFAULT_OUT_DIR = Path.home() / ".bramble" / "code-review-eval" / "dataset"


def fetch_pr_summary(repo_url: Optional[str], pr_number: str) -> Optional[str]:
    """Best-effort `gh pr view` fetch. Returns None on any failure."""
    if not repo_url or not pr_number:
        return None
    # Extract org/repo from https://github.com/org/repo
    if "github.com/" not in repo_url:
        return None
    slug = repo_url.split("github.com/", 1)[1]
    try:
        res = subprocess.run(
            ["gh", "pr", "view", pr_number, "-R", slug, "--json", "title,body"],
            capture_output=True,
            text=True,
            check=False,
            timeout=15,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return None
    if res.returncode != 0:
        return None
    import json

    try:
        obj = json.loads(res.stdout)
    except json.JSONDecodeError:
        return None
    title = obj.get("title") or ""
    body = obj.get("body") or ""
    text = (title + "\n\n" + body).strip()
    return text or None


def _iso_utc_now() -> str:
    return _dt.datetime.now(_dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _log(verbose: bool, msg: str) -> None:
    if verbose:
        print(msg, file=sys.stderr)


def main(argv: Optional[list[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--source-dir", type=Path, default=DEFAULT_SOURCE_DIR)
    p.add_argument("--out-dir", type=Path, default=DEFAULT_OUT_DIR)
    p.add_argument(
        "--repos-root",
        action="append",
        default=[],
        metavar="NAME=PATH",
        help="Map repo dir name (e.g. kernel) to its local checkout path. Repeatable.",
    )
    p.add_argument(
        "--bramble-ops-path",
        type=Path,
        default=DEFAULT_BRAMBLE_OPS,
        help="Path to the pr-polish bramble_ops.py (for goal-text reconstruction).",
    )
    p.add_argument(
        "--include-incomplete",
        action="store_true",
        default=True,
        help="Include PRs where the polish loop did not converge (default true).",
    )
    p.add_argument(
        "--skip-pr-summary",
        action="store_true",
        help="Skip `gh pr view` for PR summaries (R1 goal_text will be null).",
    )
    p.add_argument(
        "--only",
        action="append",
        default=[],
        help="Filter to specific project dir names (e.g. kernel-3945). Repeatable.",
    )
    p.add_argument("--dry-run", action="store_true")
    p.add_argument("--verbose", "-v", action="store_true")
    args = p.parse_args(argv)

    try:
        repo_map = hl.RepoMap.from_flags(args.repos_root)
    except ValueError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2

    if not args.bramble_ops_path.exists():
        print(
            f"warning: bramble_ops.py not found at {args.bramble_ops_path}; "
            "R2+ goal reconstruction will fail",
            file=sys.stderr,
        )

    harvester_sha = hl.harvester_git_sha(REPO_ROOT)
    harvested_at = _iso_utc_now()

    projects = hl.discover_project_dirs(args.source_dir)
    if args.only:
        only = set(args.only)
        projects = [(d, r, n) for (d, r, n) in projects if d.name in only]

    if not projects:
        print(
            f"no PR-numbered project dirs found under {args.source_dir}",
            file=sys.stderr,
        )
        return 2

    _log(args.verbose, f"discovered {len(projects)} project dirs")

    records: list[hl.PRRecord] = []
    partial = False

    for state_dir, repo_name, pr_number in projects:
        _log(args.verbose, f"-> {repo_name}-{pr_number}")
        repo_path = repo_map.lookup(repo_name)
        repo_url = hl.get_repo_url(repo_path)
        pr_summary: Optional[str] = None
        if not args.skip_pr_summary:
            pr_summary = fetch_pr_summary(repo_url, pr_number)
            if pr_summary is None:
                _log(args.verbose, f"   pr_summary unavailable; R1 goal will be null")
                partial = True

        try:
            record = hl.build_pr_record(
                state_dir,
                repo_name,
                pr_number,
                repo_map=repo_map,
                pr_summary=pr_summary,
                harvester_sha=harvester_sha,
                harvested_at=harvested_at,
                bramble_ops_path=args.bramble_ops_path,
                include_incomplete=args.include_incomplete,
            )
        except Exception as e:
            print(f"error: failed to build record for {state_dir.name}: {e}",
                  file=sys.stderr)
            partial = True
            continue
        if record is None:
            _log(args.verbose, f"   skipped")
            continue
        records.append(record)
        if args.dry_run:
            _log(args.verbose,
                 f"   would write: {len(record.harvested_rounds)} rounds, "
                 f"{sum(len(r.review_runs) for r in record.harvested_rounds)} review_runs")
            continue
        path = hl.write_pr_record(args.out_dir, record)
        _log(args.verbose, f"   wrote {path}")

    if not args.dry_run and records:
        index = hl.build_index(
            records, generated_at=harvested_at, harvester_sha=harvester_sha
        )
        idx_path = hl.write_index(args.out_dir, index)
        _log(args.verbose, f"wrote {idx_path}")

    print(
        f"harvested {len(records)} PR(s) "
        f"({'dry-run' if args.dry_run else 'written'})",
        file=sys.stderr,
    )

    if not records:
        return 2
    return 1 if partial else 0


if __name__ == "__main__":
    raise SystemExit(main())
