#!/usr/bin/env python3
"""State and identity helpers for /design-doc-polish.

Doc review is short (2-3 rounds typical) and edits a single file. We
keep just enough state for the user to read an audit trail after the
loop ends; everything else (heartbeats, series detection, head-
verification) belongs to /pr-polish where multi-round runs across hours
need real recovery semantics.

State path: ``~/.bramble/projects/<repo>-doc-<slug>/design-doc-polish-state.json``
where ``<slug>`` = ``<basename-no-ext>-<sha256(abs_path)[:12]>``. Hash
prefix avoids cross-directory basename collisions; basename keeps the
directory human-readable.

Imports ``_common`` and ``bramble_ops`` from /pr-polish so triage /
envelope parsing stays shared.
"""

from __future__ import annotations

import argparse
import hashlib
import re
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

_HERE = Path(__file__).resolve().parent
_PR_POLISH_SCRIPTS = _HERE.parent.parent / "pr-polish" / "scripts"
for _p in (str(_HERE), str(_PR_POLISH_SCRIPTS)):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import _common  # noqa: E402
import bramble_ops  # noqa: E402


# ---------------------------------------------------------------------------
# Identity
# ---------------------------------------------------------------------------

_BASENAME_SAFE_RE = re.compile(r"[^A-Za-z0-9._-]+")


def _sanitize_basename(name: str) -> str:
    cleaned = _BASENAME_SAFE_RE.sub("-", name).strip("-").lower()
    return cleaned or "doc"


def doc_slug(abs_path: str | Path) -> str:
    """``<basename-no-ext>-<sha256(abs_path)[:12]>``. Stable per
    absolute path; collision-safe across directories.
    """
    abs_str = str(Path(abs_path).resolve())
    digest = hashlib.sha256(abs_str.encode("utf-8")).hexdigest()[:12]
    return f"{_sanitize_basename(Path(abs_str).stem)}-{digest}"


def state_paths_for_doc(slug: str) -> tuple[Path, Path]:
    repo = _common.repo_slug()
    state_dir = Path.home() / ".bramble" / "projects" / f"{repo}-doc-{slug}"
    return state_dir, state_dir / "design-doc-polish-state.json"


def identify(doc_path: str | Path, *, repo_root: Path | None = None) -> dict[str, Any]:
    """Resolve a user-supplied doc path to canonical identity info.

    Validates that the path exists, is a regular file, and lives under
    the repo worktree (so commits land in the right place).
    """
    p = Path(doc_path).resolve()
    if not p.exists():
        raise FileNotFoundError(f"design doc not found: {doc_path}")
    if not p.is_file():
        raise ValueError(f"design doc is not a regular file: {doc_path}")
    repo_root = (repo_root or Path.cwd()).resolve()
    try:
        rel = p.relative_to(repo_root)
    except ValueError as e:
        raise ValueError(
            f"design doc {doc_path} is not under the current repo worktree {repo_root}"
        ) from e
    slug = doc_slug(p)
    state_dir, state_file = state_paths_for_doc(slug)
    return {
        "doc_path": str(rel),
        "doc_path_abs": str(p),
        "doc_slug": slug,
        "state_dir": str(state_dir),
        "state_file": str(state_file),
        "ctx": f"doc:{slug}",
    }


def _parse_ctx(ctx: str) -> str:
    if not isinstance(ctx, str) or not ctx.startswith("doc:"):
        raise ValueError(f"design-doc ctx must be 'doc:<slug>', got {ctx!r}")
    slug = ctx[len("doc:"):]
    if not slug:
        raise ValueError("design-doc ctx 'doc:' missing slug")
    return slug


# ---------------------------------------------------------------------------
# State I/O
# ---------------------------------------------------------------------------


def _utc_now() -> str:
    return datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def state_load(ctx: str) -> dict[str, Any]:
    """Read the state file. Returns ``{}`` when absent."""
    slug = _parse_ctx(ctx)
    _, path = state_paths_for_doc(slug)
    return _common.read_json(path, default={}) or {}


def state_finalize_round(
    ctx: str,
    n: int,
    actions: list[dict[str, Any]],
    *,
    doc_path: str,
    doc_path_abs: str,
    rubric: list[str],
    rubric_source: str,
    envelope_overrides: dict[str, Path] | None = None,
) -> dict[str, Any]:
    """Write a round's results. Seeds the state file on round 1.

    Doc review never amends a round mid-flight, so this single-shot
    write replaces both the pr-polish ``state-append-round`` +
    ``state-finalize-round`` pair. The state file is the audit trail,
    not a recovery mechanism.
    """
    slug = _parse_ctx(ctx)
    state_dir, path = state_paths_for_doc(slug)
    state = _common.read_json(path, default=None)
    if state is None:
        state = {
            "doc_path": doc_path,
            "doc_path_abs": doc_path_abs,
            "doc_slug": slug,
            "rubric": list(rubric),
            "rubric_source": rubric_source,
            "started_at": _utc_now(),
            "completed": False,
            "exit_reason": None,
            "rounds": [],
        }
    elif state.get("doc_path_abs") and state["doc_path_abs"] != doc_path_abs:
        raise RuntimeError(
            f"state file for slug {slug!r} was created for "
            f"{state['doc_path_abs']!r}, not {doc_path_abs!r} (slug collision)"
        )

    rounds = state.setdefault("rounds", [])
    existing = next((r for r in rounds if r.get("n") == n), None)
    counts = recompute_counts(actions)
    if existing is None:
        rounds.append({
            "n": n,
            "comment_actions": list(actions),
            **counts,
            "codex_findings": [],
            "cursor_findings": [],
        })
    else:
        existing["comment_actions"] = list(actions)
        existing.update(counts)

    state["current_round"] = n
    if state.get("completed"):
        state["completed"] = False
        state["exit_reason"] = None

    _persist_round_findings(state_dir, rounds[-1] if existing is None else existing,
                            n, envelope_overrides or {})
    _common.atomic_write_json(path, state)
    return state


FIXED_ACTIONS = {"fixed"}
SKIPPED_ACTIONS = {"false_positive", "wont_fix", "stale"}


def recompute_counts(actions: list[dict[str, Any]]) -> dict[str, Any]:
    fixed = sum(1 for a in actions if a.get("action") in FIXED_ACTIONS)
    skipped = sum(1 for a in actions if a.get("action") in SKIPPED_ACTIONS)
    best_sev = None
    best_rank = -1
    for a in actions:
        sev = a.get("severity")
        rank = _common.severity_rank(sev)
        if rank > best_rank:
            best_rank = rank
            best_sev = sev
    return {"fixed_count": fixed, "skipped_count": skipped, "top_severity": best_sev}


DESIGN_DOC_BACKENDS = ("codex", "cursor", "gemini")


def _persist_round_findings(
    state_dir: Path,
    entry: dict[str, Any],
    n: int,
    envelope_overrides: dict[str, Path],
) -> None:
    """Hydrate ``<backend>_findings`` from envelopes and copy each
    envelope into ``<state_dir>/reviews/`` for audit. Backends absent
    from the override map are reset to empty so a partial re-finalize
    doesn't leave stale findings around.
    """
    reviews_dir = state_dir / "reviews"
    incoming = {
        b for b in DESIGN_DOC_BACKENDS
        if (src := envelope_overrides.get(b)) is not None and src.exists()
    }
    for backend in DESIGN_DOC_BACKENDS:
        if backend not in incoming:
            entry[f"{backend}_findings"] = []
            stale = reviews_dir / f"r{n}-{backend}.json"
            try:
                stale.unlink(missing_ok=True)
            except OSError:
                pass
            continue
        src = envelope_overrides[backend]
        try:
            obj = _common.read_json(src, default=None)
        except Exception:  # noqa: BLE001
            obj = None
        entry[f"{backend}_findings"] = bramble_ops.parse_envelope(obj, source=backend)
        reviews_dir.mkdir(parents=True, exist_ok=True)
        try:
            (reviews_dir / f"r{n}-{backend}.json").write_text(src.read_text())
        except OSError:
            pass


def state_mark_complete(ctx: str, reason: str) -> dict[str, Any]:
    slug = _parse_ctx(ctx)
    _, path = state_paths_for_doc(slug)
    state = _common.read_json(path, default=None)
    if state is None:
        raise RuntimeError(f"state file not found for ctx {ctx}")
    state["completed"] = True
    state["exit_reason"] = reason
    state["completed_at"] = _utc_now()
    _common.atomic_write_json(path, state)
    return state


# ---------------------------------------------------------------------------
# Rubric reader
# ---------------------------------------------------------------------------

# Mirrors yoloswe/reviewer/reviewer.go ``rubricCap`` and bramble's
# loadRubricFile (codereview.go). LOAD-BEARING: when the Go side
# changes, update both. Counted in UTF-8 bytes to match Go's len().
_RUBRIC_LINE_MAX_BYTES = 500
_RUBRIC_MAX_ENTRIES = 20
_MARKDOWN_CONTROL_PREFIXES = ("#", "-", "*", "+", ">", "=")


def _sanitize_prompt_hint(s: str) -> bool:
    """Python port of yoloswe/reviewer.SanitizePromptHint. Keep in
    lock-step with the Go side; the matching test in test_doc_ops.py
    is the safety net.
    """
    if not s or "\r" in s or "\n" in s or s != s.strip():
        return False
    if s[0] in _MARKDOWN_CONTROL_PREFIXES:
        return False
    if s[0].isdigit():
        i = 0
        while i < len(s) and s[i].isdigit():
            i += 1
        if i < len(s) and s[i] in ".)":
            return False
    return True


def read_rubric_file(path: str) -> list[str]:
    """Read a rubric file with the same validation rules as bramble's
    loadRubricFile. Skips ``#`` comments and blank lines; rejects
    overlong lines (UTF-8 bytes), leading markdown control chars,
    and >20 entries. Failing fast here matches the rule the Go side
    enforces every round.
    """
    out = []
    for i, line in enumerate(Path(path).read_text().split("\n"), start=1):
        trimmed = line.strip()
        if not trimmed or trimmed.startswith("#"):
            continue
        if len(trimmed.encode("utf-8")) > _RUBRIC_LINE_MAX_BYTES:
            raise ValueError(f"rubric line {i} exceeds {_RUBRIC_LINE_MAX_BYTES} bytes (UTF-8)")
        if not _sanitize_prompt_hint(trimmed):
            raise ValueError(f"rubric line {i} failed sanitization: {trimmed!r}")
        out.append(trimmed)
    if not out:
        raise ValueError(f"rubric file {path!r} has no non-blank, non-comment lines")
    if len(out) > _RUBRIC_MAX_ENTRIES:
        raise ValueError(f"rubric file {path!r} has {len(out)} entries; cap is {_RUBRIC_MAX_ENTRIES}")
    return out


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="doc_ops")
    sub = p.add_subparsers(dest="cmd", required=True)

    sp = sub.add_parser("identify", help="Resolve doc path → identity record (JSON).")
    sp.add_argument("doc_path")

    sp = sub.add_parser("state-load", help="Read state file (returns {} when absent).")
    sp.add_argument("ctx")

    sp = sub.add_parser(
        "state-finalize-round",
        help="Write a round's findings + actions, seeding state on round 1.",
    )
    sp.add_argument("ctx")
    sp.add_argument("round_", type=int)
    sp.add_argument("actions_file")
    sp.add_argument("--doc-path", required=True)
    sp.add_argument("--doc-path-abs", required=True)
    sp.add_argument("--rubric-file", required=True)
    sp.add_argument("--rubric-source", required=True)
    sp.add_argument("--envelope", action="append", default=[],
                    metavar="BACKEND=PATH",
                    help="Per-backend envelope path; may be repeated.")

    sp = sub.add_parser("state-mark-complete")
    sp.add_argument("ctx")
    sp.add_argument("reason")

    sp = sub.add_parser("read-rubric-file",
                        help="Validate a rubric file and emit JSON list of questions.")
    sp.add_argument("path")

    return p


def _parse_envelope_args(pairs: list[str]) -> dict[str, Path]:
    out: dict[str, Path] = {}
    for entry in pairs:
        if "=" not in entry:
            raise ValueError(f"--envelope must be BACKEND=PATH, got {entry!r}")
        backend, path = entry.split("=", 1)
        if backend not in DESIGN_DOC_BACKENDS:
            raise ValueError(f"unknown backend in --envelope: {backend!r}")
        out[backend] = Path(path)
    return out


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    try:
        if args.cmd == "identify":
            _common.print_json(identify(args.doc_path))
        elif args.cmd == "state-load":
            _common.print_json(state_load(args.ctx))
        elif args.cmd == "state-finalize-round":
            actions = _common.read_json(Path(args.actions_file), default=[])
            if not isinstance(actions, list):
                raise ValueError("actions_file must be a JSON array")
            rubric = read_rubric_file(args.rubric_file)
            state = state_finalize_round(
                args.ctx, args.round_, actions,
                doc_path=args.doc_path,
                doc_path_abs=args.doc_path_abs,
                rubric=rubric,
                rubric_source=args.rubric_source,
                envelope_overrides=_parse_envelope_args(args.envelope),
            )
            _common.print_json(state)
        elif args.cmd == "state-mark-complete":
            _common.print_json(state_mark_complete(args.ctx, args.reason))
        elif args.cmd == "read-rubric-file":
            _common.print_json(read_rubric_file(args.path))
        else:  # pragma: no cover
            raise ValueError(f"unknown cmd: {args.cmd}")
    except Exception as e:  # noqa: BLE001
        print(f"doc_ops: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
