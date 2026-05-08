#!/usr/bin/env python3
"""State and identity helpers for /design-doc-polish.

This module is the design-doc analogue of /pr-polish's pr_ops.py — it
owns:

  - Identity: resolve ``<doc-path>`` to an absolute path, derive a
    collision-resistant ``doc_slug``, and return canonical state paths.
  - State I/O: state-load / state-append-round / state-finalize-round /
    state-mark-complete / state-mark-abandoned, all atomic.
  - The narrow set of ``recompute_counts`` / ``_merge_actions``
    helpers needed for finalize.

It deliberately does NOT fork the triage/envelope/session-id machinery
(those live in pr-polish/scripts/bramble_ops.py and are mode-aware as of
the same change that introduced this skill — see the plan in
/home/ubuntu/.claude/plans/create-a-skill-similar-wondrous-hollerith.md).
We import bramble_ops + _common via sys.path so a triage / consensus
bug-fix lands once, not twice.

State path: ``~/.bramble/projects/<repo>-doc-<doc-slug>/design-doc-polish-state.json``
where ``doc_slug`` = ``<basename-no-ext>-<sha256(abs_path)[:12]>``. The
SHA prefix avoids cross-directory basename collisions; the basename
keeps the directory name human-readable.

Context token: ``doc:<doc-slug>``, mirroring pr_ops's ``branch:<name>``.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

# Import _common and bramble_ops from the sibling pr-polish skill. The two
# modules are stable triage/state primitives — forking would mean every
# bug fix lands twice and consensus/spiral semantics drift over time. The
# path resolution is anchored on this file's location so worktree moves
# don't break the import.
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

# Doc slugs combine basename and a SHA prefix; the regex sanitises the
# basename half so the directory name stays filesystem-friendly.
_BASENAME_SAFE_RE = re.compile(r"[^A-Za-z0-9._-]+")


def _sanitize_basename(name: str) -> str:
    cleaned = _BASENAME_SAFE_RE.sub("-", name).strip("-").lower()
    return cleaned or "doc"


def doc_slug(abs_path: str | Path) -> str:
    """Compute the canonical slug for a doc path.

    Format: ``<sanitized-basename-no-ext>-<sha256(abs_path)[:12]>``.

    Examples:
        ``/repo/docs/design/sessionmodel-architecture.md``
        → ``sessionmodel-architecture-3a7f2c1b9d4e``

    Stable: re-running on the same absolute path returns the same slug.
    Collision-safe: two docs with the same basename in different
    directories produce different slugs because the SHA includes the
    full absolute path.
    """
    abs_str = str(Path(abs_path).resolve())
    digest = hashlib.sha256(abs_str.encode("utf-8")).hexdigest()[:12]
    base = Path(abs_str).stem  # filename without extension
    return f"{_sanitize_basename(base)}-{digest}"


def state_paths_for_doc(slug: str) -> tuple[Path, Path]:
    """Return ``(state_dir, state_file)`` for a given doc slug.

    Mirrors ``_common.state_paths`` but routes to a separate
    ``<repo>-doc-<slug>/`` directory and the
    ``design-doc-polish-state.json`` filename so the two skills don't
    collide on the same filesystem path.
    """
    repo = _common.repo_slug()
    state_dir = Path.home() / ".bramble" / "projects" / f"{repo}-doc-{slug}"
    return state_dir, state_dir / "design-doc-polish-state.json"


def identify(doc_path: str | Path, *, repo_root: Path | None = None) -> dict[str, Any]:
    """Resolve a user-supplied doc path to canonical identity info.

    Validates that the path:
      - exists,
      - is a regular file (not a directory or symlink to nothing),
      - lives under the repo worktree (otherwise commits won't land
        in the right place; we'd rather fail loud than commit on the
        wrong branch).

    Returns the full identity record:
        {
          "doc_path": "<repo-relative path>",
          "doc_path_abs": "<absolute path>",
          "doc_slug": "<basename-sha>",
          "state_dir": "<state dir>",
          "state_file": "<state file>",
          "ctx": "doc:<slug>",
        }

    Non-``.md`` extensions are warned-only on the CLI (handled by the
    caller); this function accepts any regular file so design-doc workflow
    works with ``.txt`` and extensionless drafts too.
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
            f"design doc {doc_path} is not under the current repo worktree {repo_root}; "
            "commits would land in the wrong place"
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
    """Parse ``doc:<slug>`` into the bare slug. Errors on any other shape
    so a copy-paste from a pr-polish run (PR number, ``branch:<name>``)
    fails loud instead of writing to the wrong directory.
    """
    if not isinstance(ctx, str) or not ctx.startswith("doc:"):
        raise ValueError(f"design-doc ctx must be 'doc:<slug>', got {ctx!r}")
    slug = ctx[len("doc:"):]
    if not slug:
        raise ValueError("design-doc ctx 'doc:' missing slug")
    return slug


# ---------------------------------------------------------------------------
# Heartbeat / series detection (mirrors pr_ops semantics, reused via
# constants from there so the timeout stays in lock-step).
# ---------------------------------------------------------------------------

# Same 2-hour staleness window as pr-polish. Long bramble reviews can
# easily run 10+ minutes per backend; anything past 2h is almost
# certainly an abandoned process.
HEARTBEAT_STALE_SECONDS = 2 * 60 * 60


def _utc_now() -> str:
    return datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def _is_heartbeat_stale(state: dict[str, Any]) -> bool:
    if state.get("completed"):
        return False
    ts = state.get("last_heartbeat_at")
    if not ts:
        return True
    try:
        ts_dt = datetime.strptime(ts, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=UTC)
    except (TypeError, ValueError):
        return True
    age = (datetime.now(UTC) - ts_dt).total_seconds()
    return age > HEARTBEAT_STALE_SECONDS


def _is_first_round_of_series(state: dict[str, Any] | None, n: int) -> bool:
    """True when round ``n`` starts a new review series. Same rule as
    pr-polish: no state, prior loop ``completed: true``, or round 1 with
    no rounds yet.
    """
    if state is None or not state.get("rounds"):
        return True
    if state.get("completed"):
        return True
    return n == 1


# ---------------------------------------------------------------------------
# State I/O
# ---------------------------------------------------------------------------


def state_load(ctx: str) -> dict[str, Any]:
    """Read the state file and decorate with derived signals."""
    slug = _parse_ctx(ctx)
    _, path = state_paths_for_doc(slug)
    state = _common.read_json(path, default={}) or {}
    if state:
        state["is_heartbeat_stale"] = _is_heartbeat_stale(state)
        state["is_first_round_of_series"] = _is_first_round_of_series(
            state, state.get("current_round") or 1
        )
    return state


def state_is_new_series(ctx: str, n: int) -> bool:
    """Standalone CLI helper used by SKILL.md's Step 0.5 to capture the
    series-boundary decision *before* state_append_round clears the
    completed flag. Returns True/False as a literal so the orchestrator
    can shell-substitute it directly into ``IS_NEW_SERIES=…``.
    """
    slug = _parse_ctx(ctx)
    _, path = state_paths_for_doc(slug)
    state = _common.read_json(path, default=None)
    return _is_first_round_of_series(state, n)


def state_append_round(
    ctx: str,
    n: int,
    head_before: str,
    *,
    doc_path: str,
    doc_path_abs: str,
    rubric: list[str],
    rubric_source: str,
    verify_head: bool = True,
) -> dict[str, Any]:
    """Start a new round (or refresh ``head_before`` on an in-progress one).

    The first call (no state file) seeds the persisted record with
    ``doc_path``, ``doc_slug``, ``rubric``, ``rubric_source``. Subsequent
    rounds reuse those — the rubric is locked at round 1 (per the user's
    "keep it consistent" choice). A subsequent call with a different doc
    path raises (integrity gate; same doc-slug across two different docs
    would be a hash collision and worth surfacing loudly).

    ``verify_head`` matches pr-polish: compares ``git rev-parse HEAD``
    against ``head_before`` and refuses on mismatch. Pass
    ``verify_head=False`` only when resuming an interrupted round.
    """
    if verify_head:
        try:
            current = _common.run(["git", "rev-parse", "HEAD"], check=True).stdout.strip()
        except (_common.CommandError, FileNotFoundError) as e:
            raise RuntimeError(f"could not read git HEAD for verification: {e}") from e
        if current != head_before:
            raise RuntimeError(
                f"HEAD {current[:7]} != declared head_before {head_before[:7]}; "
                "refuse to append round (orchestrator raced a commit — "
                "rerun with the current HEAD)"
            )
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
            "current_round": n,
            "last_commit_at_round_start": head_before,
            "rounds": [],
        }
    else:
        # Integrity gate: same slug, different doc path is a hash
        # collision (or worse, the user resolved a relative path under
        # a different worktree). Refuse rather than silently overwrite.
        existing_path = state.get("doc_path_abs")
        if existing_path and existing_path != doc_path_abs:
            raise RuntimeError(
                f"state file for slug {slug!r} was created for "
                f"{existing_path!r}, not {doc_path_abs!r}; "
                "refuse to append (slug collision — pick a different doc)"
            )
        # Round 1 of a fresh series re-pins the rubric (the orchestrator
        # may have re-inferred it after a previous series completed).
        # Mid-series, the rubric is immutable.
        prior_completed = bool(state.get("completed"))
        if prior_completed and n == 1:
            state["rubric"] = list(rubric)
            state["rubric_source"] = rubric_source
    rounds = state.setdefault("rounds", [])
    existing = next((r for r in rounds if r.get("n") == n), None)
    if existing is None:
        rounds.append(
            {
                "n": n,
                "head_before": head_before,
                "head_after": None,
                "codex_findings": [],
                "cursor_findings": [],
                "fixed_count": 0,
                "skipped_count": 0,
                "top_severity": None,
                "comment_actions": [],
            }
        )
    else:
        existing["head_before"] = head_before
    state["current_round"] = n
    state["last_commit_at_round_start"] = head_before
    if state.get("completed"):
        state["completed"] = False
        state["exit_reason"] = None
        state["completed_at"] = None
    state["last_heartbeat_at"] = _utc_now()
    _common.atomic_write_json(path, state)
    return state


# Action-verb sets for recompute_counts. design-doc mode drops the
# code-mode `pre_existing` / `flake` (CI-only) and `ack` (pr-polish
# batch-acknowledge — per user choice we use `wont_fix` with an
# explicit reason instead). Fewer states, simpler audit trail.
FIXED_ACTIONS = {"fixed"}
SKIPPED_ACTIONS = {"false_positive", "wont_fix", "stale"}


def _top_severity(actions: list[dict[str, Any]]) -> str | None:
    best = None
    best_rank = -1
    for a in actions:
        sev = a.get("severity")
        rank = _common.severity_rank(sev)
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


def _action_key(action: dict[str, Any]) -> tuple:
    """Dedup key for design-doc actions. Source + section + dimension +
    topic uniquely identifies the finding the action addresses (no
    comment_id since there are no PR comments in this skill).
    """
    return (
        action.get("source"),
        action.get("section"),
        action.get("dimension"),
        action.get("topic"),
    )


def _merge_actions(
    existing: list[dict[str, Any]], new: list[dict[str, Any]]
) -> list[dict[str, Any]]:
    """Append new actions; dedupe on the action key. New wins on conflict
    so a re-finalize with updated severities/reasons overwrites the
    in-progress entry."""
    by_key: dict[tuple, dict[str, Any]] = {}
    for a in existing:
        by_key[_action_key(a)] = a
    for a in new:
        by_key[_action_key(a)] = a
    return list(by_key.values())


def state_finalize_round(
    ctx: str,
    n: int,
    head_after: str,
    actions: list[dict[str, Any]],
    *,
    envelope_overrides: dict[str, Path] | None = None,
) -> dict[str, Any]:
    """Finalize a round and persist its results.

    ``envelope_overrides`` maps backend name (``codex`` / ``cursor`` /
    ``gemini``) to the on-disk envelope file Monitor captured for that
    backend. Backends absent from the mapping have their per-backend
    state cleared (so a partial re-finalize doesn't leave stale
    session_ids around — same correctness rule as pr-polish).
    """
    slug = _parse_ctx(ctx)
    state_dir, path = state_paths_for_doc(slug)
    state = _common.read_json(path, default=None)
    if state is None:
        raise RuntimeError(
            f"state file not found for ctx {ctx}; call state-append-round first"
        )
    rounds = state.get("rounds") or []
    entry = next((r for r in rounds if r.get("n") == n), None)
    if entry is None:
        raise RuntimeError(f"round {n} not found in state")
    existing = entry.get("comment_actions") or []
    merged = _merge_actions(existing, actions)
    entry["comment_actions"] = merged
    entry["head_after"] = head_after
    entry.update(recompute_counts(merged))
    _persist_round_findings(state_dir, entry, n, envelope_overrides or {})
    state["last_heartbeat_at"] = _utc_now()
    _common.atomic_write_json(path, state)
    return state


# Backends are the same list as pr-polish, minus ``lint`` (lint gate
# doesn't apply to docs). We keep ``gemini`` so ``--gemini`` works.
DESIGN_DOC_BACKENDS = ("codex", "cursor", "gemini")


def _persist_round_findings(
    state_dir: Path,
    entry: dict[str, Any],
    n: int,
    envelope_overrides: dict[str, Path],
) -> None:
    """Hydrate ``codex_findings`` / ``cursor_findings`` / ``gemini_findings``
    from envelopes; copy each envelope into ``<state_dir>/reviews/`` for
    post-loop audit. Mirrors pr_ops._persist_round_findings minus the CI
    finding hydration (no CI in design-doc mode).
    """
    reviews_dir = state_dir / "reviews"
    incoming = {
        b for b in DESIGN_DOC_BACKENDS
        if (src := envelope_overrides.get(b)) is not None and src.exists()
    }
    # Cleanup pass: drop per-backend state for backends not in the
    # incoming set, so a partial re-finalize doesn't keep stale
    # session_ids that would cause the next round's resume to land
    # on the wrong session.
    for backend in DESIGN_DOC_BACKENDS:
        if backend in incoming:
            continue
        entry[f"{backend}_findings"] = []
        for bucket_key in ("session_ids", "resume_status"):
            bucket = entry.get(bucket_key)
            if isinstance(bucket, dict):
                bucket.pop(backend, None)
                if not bucket:
                    entry.pop(bucket_key, None)
        stale_review = state_dir / "reviews" / f"r{n}-{backend}.json"
        try:
            stale_review.unlink(missing_ok=True)
        except OSError:
            pass
    # Hydration pass: read each envelope, parse its findings, copy the
    # raw envelope to reviews_dir for archival.
    for backend in DESIGN_DOC_BACKENDS:
        src = envelope_overrides.get(backend)
        if src is None or not src.exists():
            continue
        try:
            obj = _common.read_json(src, default=None)
        except Exception:  # noqa: BLE001 — malformed envelope must not brick finalize
            obj = None
        entry[f"{backend}_findings"] = bramble_ops.parse_envelope(obj, source=backend)
        for bucket_key in ("session_ids", "resume_status"):
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
        reviews_dir.mkdir(parents=True, exist_ok=True)
        dest = reviews_dir / f"r{n}-{backend}.json"
        try:
            dest.write_text(src.read_text())
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


def state_mark_abandoned(ctx: str) -> dict[str, Any]:
    slug = _parse_ctx(ctx)
    _, path = state_paths_for_doc(slug)
    state = _common.read_json(path, default=None)
    if state is None:
        raise RuntimeError(f"state file not found for ctx {ctx}")
    state["completed"] = True
    state["exit_reason"] = "abandoned"
    state["completed_at"] = _utc_now()
    _common.atomic_write_json(path, state)
    return state


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="doc_ops")
    sub = p.add_subparsers(dest="cmd", required=True)

    sp = sub.add_parser("identify", help="Resolve doc path → identity record (JSON).")
    sp.add_argument("doc_path")

    sp = sub.add_parser("state-load", help="Read state file (decorated with derived signals).")
    sp.add_argument("ctx")

    sp = sub.add_parser(
        "state-is-new-series",
        help="Print 1 if round n starts a new series, else 0.",
    )
    sp.add_argument("ctx")
    sp.add_argument("round_", type=int)

    sp = sub.add_parser("state-append-round")
    sp.add_argument("ctx")
    sp.add_argument("round_", type=int)
    sp.add_argument("head_before")
    sp.add_argument("--doc-path", required=True, help="Repo-relative doc path.")
    sp.add_argument("--doc-path-abs", required=True, help="Absolute doc path.")
    sp.add_argument(
        "--rubric-file",
        required=True,
        help="Text file holding the rubric (one question per line).",
    )
    sp.add_argument(
        "--rubric-source",
        required=True,
        help="Where the rubric came from: 'inferred', 'user-edited', or '--rubric-file <path>'.",
    )
    sp.add_argument(
        "--no-verify-head",
        action="store_true",
        help="Skip git HEAD verification (only when resuming an interrupted round).",
    )

    sp = sub.add_parser("state-finalize-round")
    sp.add_argument("ctx")
    sp.add_argument("round_", type=int)
    sp.add_argument("head_after")
    sp.add_argument("actions_file", help="JSON array of comment_actions for this round.")
    sp.add_argument(
        "--envelope",
        action="append",
        default=[],
        metavar="BACKEND=PATH",
        help="Per-backend envelope file path; may be repeated.",
    )

    sp = sub.add_parser("state-mark-complete")
    sp.add_argument("ctx")
    sp.add_argument("reason")

    sp = sub.add_parser("state-mark-abandoned")
    sp.add_argument("ctx")

    return p


def _parse_envelope_args(pairs: list[str]) -> dict[str, Path]:
    out: dict[str, Path] = {}
    for entry in pairs:
        if "=" not in entry:
            raise ValueError(f"--envelope must be BACKEND=PATH, got {entry!r}")
        backend, path = entry.split("=", 1)
        if backend not in DESIGN_DOC_BACKENDS:
            raise ValueError(
                f"unknown backend in --envelope: {backend!r} "
                f"(want one of {DESIGN_DOC_BACKENDS})"
            )
        out[backend] = Path(path)
    return out


def _read_rubric_file(path: str) -> list[str]:
    """Read a rubric file the same way bramble's loadRubricFile does:
    one question per non-blank, non-comment line. Sanitisation is
    enforced by the bramble side at runtime; here we just collect.
    """
    raw = Path(path).read_text()
    out = []
    for line in raw.split("\n"):
        trimmed = line.strip()
        if not trimmed or trimmed.startswith("#"):
            continue
        out.append(trimmed)
    if not out:
        raise ValueError(f"rubric file {path!r} has no non-blank, non-comment lines")
    return out


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    try:
        if args.cmd == "identify":
            _common.print_json(identify(args.doc_path))
        elif args.cmd == "state-load":
            _common.print_json(state_load(args.ctx))
        elif args.cmd == "state-is-new-series":
            print("1" if state_is_new_series(args.ctx, args.round_) else "0")
        elif args.cmd == "state-append-round":
            rubric = _read_rubric_file(args.rubric_file)
            state = state_append_round(
                args.ctx,
                args.round_,
                args.head_before,
                doc_path=args.doc_path,
                doc_path_abs=args.doc_path_abs,
                rubric=rubric,
                rubric_source=args.rubric_source,
                verify_head=not args.no_verify_head,
            )
            _common.print_json(state)
        elif args.cmd == "state-finalize-round":
            actions_blob = _common.read_json(Path(args.actions_file), default=[])
            if not isinstance(actions_blob, list):
                raise ValueError("actions_file must be a JSON array")
            envelopes = _parse_envelope_args(args.envelope)
            state = state_finalize_round(
                args.ctx,
                args.round_,
                args.head_after,
                actions_blob,
                envelope_overrides=envelopes,
            )
            _common.print_json(state)
        elif args.cmd == "state-mark-complete":
            _common.print_json(state_mark_complete(args.ctx, args.reason))
        elif args.cmd == "state-mark-abandoned":
            _common.print_json(state_mark_abandoned(args.ctx))
        else:  # pragma: no cover
            raise ValueError(f"unknown cmd: {args.cmd}")
    except Exception as e:  # noqa: BLE001
        print(f"doc_ops: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
