#!/usr/bin/env python3
"""Compute a machine-checkable verdict for one /pr-polish run.

Today the "ready/not-ready verdict" is free-form prose the orchestrating
agent writes about its own work, and it is never persisted. That makes it
unusable by any downstream consumer and impossible to audit: a run that
exhausted its round budget with work still outstanding
(``exit_reason: capped-at-max``) reaches the same summary as one that
genuinely converged.

This module computes a verdict from the state file and git, persists it,
and exits non-zero when the run is not ready — so a non-LLM caller can gate
on it.

Two lists, deliberately:

* **blockers** force ``not_ready``. Each is a fact that can be checked.
* **advisories** never block. They carry signals worth surfacing that are
  too soft to gate on (e.g. a reviewer's own "am I confident" claim, which
  is self-reported by the same class of model whose blind spots we are
  trying to measure).

``inconclusive`` is a third state, distinct from ``not_ready``: it means
the evidence needed to decide was unavailable, not that the run was bad.
Conflating the two would train users to ignore the verdict on any repo
where a check cannot run.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Optional

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

from _common import (  # noqa: E402
    action_reason,
    atomic_write_json,
    format_duration_ms,
    severity_rank,
)

# Producer-side backend roster; ``reviewer_stream_health`` derives its
# state keys from it so a new backend cannot go unreported.
import bramble_ops  # noqa: E402

VERDICT_SCHEMA_VERSION = 1

# Exit reasons that can never be "ready": the loop stopped for a reason
# other than finishing its work.
_ABNORMAL_EXITS = {
    "capped-at-max",
    "spiral-escalated",
    "pr-mismatch-abort",
    "sync-conflict",
    "dirty-tree-preflight",
    "user-paused",
    "abandoned",
    # Every reviewer failed to return a verdict, so the local bar was never
    # actually held. The work may be fine — but nothing here checked it, and
    # under the batch protocol that means the external reviewers are the first
    # real review this diff gets.
    "reviewers-unavailable",
}

# A high/critical wont_fix currently clears its block on ANY non-empty
# reason string — a one-character escape hatch. Require enough substance
# that a reason is at least an argument. Deliberately a shape heuristic,
# not an LLM grader: grading rationales with a model would reinsert an
# unverified model at exactly the point being hardened.
_MIN_REASON_CHARS = 40
_MIN_REASON_WORDS = 6
_CONTENTLESS_REASONS = {
    "n/a",
    "na",
    "not applicable",
    "by design",
    "wontfix",
    "won't fix",
    "out of scope",
    "no",
    "skip",
    "later",
}


def reason_is_substantive(reason: Optional[str]) -> bool:
    """True if a deferral reason is an argument rather than a placeholder."""
    text = (reason or "").strip()
    if len(text) < _MIN_REASON_CHARS:
        return False
    if len(text.split()) < _MIN_REASON_WORDS:
        return False
    return text.lower().rstrip(".") not in _CONTENTLESS_REASONS


def _is_file_path(path: Any) -> bool:
    """True if this looks like a repo path rather than an opaque id.

    ``ci``-sourced action rows carry a workflow run id (e.g. "90720812688")
    in ``path``. It is not a file, so a diff can never "touch" it.
    """
    text = str(path or "").strip()
    if not text or text.isdigit():
        return False
    return "/" in text or "." in text


def _action_key(action: dict[str, Any]) -> tuple:
    """Identity of a finding across rounds — mirrors pr_ops._action_key."""
    cid = action.get("comment_id")
    if cid:
        return ("id", cid)
    return (
        "loc",
        action.get("source"),
        action.get("path"),
        action.get("line"),
        action.get("topic"),
    )


def latest_actions(state: dict) -> dict[tuple, dict]:
    """Each finding's terminal action — the highest-round write wins.

    A round-1 bare ``ack`` that a later round records as ``fixed`` is
    resolved; only the last word counts.
    """
    latest: dict[tuple, dict] = {}
    for rnd in sorted(state.get("rounds") or [], key=lambda r: r.get("n") or 0):
        for a in rnd.get("comment_actions") or []:
            latest[_action_key(a)] = a
    return latest


def open_high_deferrals(state: dict) -> list[dict]:
    """High/critical findings left deferred without a real rationale."""
    out = []
    for a in latest_actions(state).values():
        if severity_rank(a.get("severity")) < severity_rank("high"):
            continue
        verb = a.get("action")
        if verb == "ack":
            out.append(a)
        elif verb == "wont_fix" and not reason_is_substantive(action_reason(a)):
            out.append(a)
    return out


def _claim_matches(claim: str, touched: set[str]) -> bool:
    """Does ``claim`` name one of the repo-relative paths in ``touched``?

    Exact match first. Failing that, treat the shorter of the two as a
    path *suffix* of the longer, on whole path components only. Two
    reasons the comparison cannot simply be equality:

    * reviewers do not agree on how much of the path they emit — some
      cite ``verdict.py``, some ``.claude/skills/pr-polish/scripts/verdict.py``
    * an action recorded from a PR comment carries whatever prefix the
      external bot chose.

    Suffix matching keeps those honest claims verifiable while refusing
    the collision that basename matching allowed: ``other/util.go`` no
    longer satisfies a claim on ``pkg/a/util.go``, because neither is a
    component-aligned suffix of the other. A bare ``util.go`` claim still
    matches any ``util.go`` — that ambiguity is in the input, not the
    comparison, and this is no weaker than the previous behaviour.
    """
    if claim in touched:
        return True
    claim_parts = Path(claim).parts
    if not claim_parts:
        return False
    for t in touched:
        t_parts = Path(t).parts
        if len(claim_parts) <= len(t_parts):
            if t_parts[-len(claim_parts):] == claim_parts:
                return True
        elif claim_parts[-len(t_parts):] == t_parts:
            return True
    return False


def refuted_class_claims(state: dict) -> list[dict]:
    """Class-level fix claims that a later round contradicted.

    A ``fixed`` action carrying an ``invariant`` claims more than "this
    line is fixed" — it claims the *rule* now holds. The cheapest
    falsifier costs nothing to collect and cannot be gamed by writing
    prose: if a later round fixes the same file again, the claimed rule
    did not cover its class. The refuting evidence is produced by a
    subsequent round's own behaviour, not by the claiming round's
    self-report, which is exactly what ``sites_found`` could never be.

    Same design as :func:`verify_fix_claims`: a *necessary* condition,
    not a sufficient one. A later fix in the same file may be an
    unrelated defect, so this is an advisory — it says where to look,
    never that the run failed. Measured across the 641 runs on disk,
    173 of 726 invariant-bearing claims (23.8%) are refuted this way.

    Claims are keyed ``(path, invariant)``, not by path alone: one file
    can carry two distinct invariants, and collapsing them would hide a
    refutation of the second behind a claim about the first.

    Deliberately keyed on ``invariant`` rather than ``sites_found``:
    the former is populated on ~1000 rows, the latter on ~20.
    """
    claims: dict[tuple[str, str], dict] = {}
    out: list[dict] = []
    for rnd in sorted(state.get("rounds") or [], key=lambda r: r.get("n") or 0):
        n = rnd.get("n")
        fixed_here = {
            a["path"]
            for a in rnd.get("comment_actions") or []
            if a.get("action") == "fixed" and a.get("path")
        }
        # Refute against claims standing *before* this round, so a class
        # fixed across several sites in one round never refutes itself.
        for key, claim in list(claims.items()):
            if claim["path"] in fixed_here:
                out.append({**claim, "refuted_by_round": n})
                del claims[key]
        for a in rnd.get("comment_actions") or []:
            if a.get("action") != "fixed" or not a.get("path") or not a.get("invariant"):
                continue
            claims.setdefault(
                (a["path"], a["invariant"]),
                {"round": n, "path": a["path"], "invariant": a["invariant"]},
            )
    return out


def verify_fix_claims(state: dict, repo_root: Optional[Path]) -> dict:
    """Check each ``fixed`` claim against what the round actually changed.

    The cheapest credible check: a round that claims to have fixed
    something at ``path`` must have touched ``path``. It is a *necessary*
    condition, not a sufficient one — touching a file is not fixing a bug —
    and that is the right trade. Re-reading the cited line is ambiguous
    (a real fix usually moves it), and demanding a regression test per
    finding would block on findings that are untestable by construction.

    The sharpest case this catches: a round whose ``head_before ==
    head_after`` (it committed nothing) still claiming ``fixed``.

    Paths are checked against the union of touched files from the claiming
    round onward, so a fix that actually landed a round later is not a
    false positive.
    """
    rounds = sorted(state.get("rounds") or [], key=lambda r: r.get("n") or 0)
    touched_from: dict[int, set[str]] = {}
    # Rounds whose commits are absent from the checkout: their claims are
    # unknowable, not false.
    unresolvable: set[int] = set()
    if repo_root is not None:
        import subprocess

        per_round: dict[int, set[str]] = {}
        for rnd in rounds:
            n = rnd.get("n") or 0
            before, after = rnd.get("head_before"), rnd.get("head_after")
            files: set[str] = set()
            if before and after and before != after:
                res = subprocess.run(
                    ["git", "-C", str(repo_root), "diff", "--name-only",
                     f"{before}..{after}"],
                    capture_output=True, text=True, check=False,
                )
                if res.returncode == 0:
                    # Keep full repo-relative paths. Collapsing to
                    # ``Path(...).name`` here made the whole check
                    # basename-only, which is load-bearing in a monorepo:
                    # a claim on ``yoloswe/reviewer/BUILD.bazel`` verified
                    # against *any* touched ``BUILD.bazel``, and the same
                    # for SKILL.md, __init__.py, README.md and every
                    # repeated ``tests/test_*.py``. See ``_claim_matches``
                    # for how bare-basename claims are still honoured.
                    files = {
                        ln.strip()
                        for ln in res.stdout.splitlines()
                        if ln.strip()
                    }
                else:
                    # The commits are not in this checkout — normal for an
                    # older PR whose branch was deleted after merge. We
                    # cannot tell whether the claim is true, and reporting
                    # "unverified" would be indistinguishable from a real
                    # fabrication. Mark the round unknown and skip it.
                    unresolvable.add(n)
            per_round[n] = files
        # Suffix union: everything touched at round n or later.
        acc: set[str] = set()
        for n in sorted(per_round, reverse=True):
            acc = acc | per_round[n]
            touched_from[n] = set(acc)

    total = verified = unverified = unknown = 0
    unverified_rows: list[dict] = []
    for rnd in rounds:
        n = rnd.get("n") or 0
        for a in rnd.get("comment_actions") or []:
            if a.get("action") != "fixed":
                continue
            path = a.get("path")
            if not path or not _is_file_path(path):
                # Nothing checkable: a PR-level/doc finding has no path, and
                # a `ci` row's "path" is a workflow run id, not a file. Both
                # are excluded rather than counted as unverified — flagging
                # them would be a guaranteed false positive on every run
                # that had a CI failure.
                continue
            if repo_root is None or n in unresolvable:
                unknown += 1
                continue
            total += 1
            if _claim_matches(str(path), touched_from.get(n, set())):
                verified += 1
            else:
                unverified += 1
                unverified_rows.append(
                    {
                        "round": n,
                        "path": path,
                        "line": a.get("line"),
                        "severity": a.get("severity"),
                        "no_commit": rnd.get("head_before") == rnd.get("head_after"),
                    }
                )
    return {
        # `total` counts only claims we could actually check, so the
        # verified/unverified split is meaningful. `unknown` is claims whose
        # round commits are missing from the checkout.
        "total": total,
        "verified": verified,
        "unverified": unverified,
        "unknown": unknown,
        "checked": repo_root is not None,
        "unverified_rows": unverified_rows,
    }


def compute_verdict(state: dict, *, repo_root: Optional[Path] = None) -> dict:
    """The verdict for one run. Pure over state + git; no network."""
    blockers: list[dict] = []
    advisories: list[dict] = []

    exit_reason = state.get("exit_reason")
    if not state.get("completed"):
        advisories.append(
            {"code": "run_incomplete", "detail": "run has no completed_at"}
        )
    if exit_reason in _ABNORMAL_EXITS:
        blockers.append(
            {
                "code": "abnormal_exit",
                "detail": f"exit_reason={exit_reason} — the loop stopped "
                "without finishing its work",
                "severity": "high",
            }
        )

    for a in open_high_deferrals(state):
        blockers.append(
            {
                "code": "open_high_deferral",
                "detail": f"{a.get('path')}:{a.get('line')} "
                f"[{a.get('severity')}] {a.get('action')} with no "
                "substantive reason",
                "severity": "high",
            }
        )

    # "Nobody looked" is not "nothing to find". A round whose every stream is
    # error/absent reviewed nothing, so its empty finding list cannot support a
    # ready verdict — and under the batch protocol the batch has already pushed
    # on it. This is the automated half of the stream_status contract: without
    # it the field is written and never read, and "reviewed clean" stays
    # indistinguishable from "never reviewed" in the verdict.
    dead_rounds = rounds_without_a_live_stream(state)
    if dead_rounds:
        blockers.append(
            {
                "code": "no_live_reviewer",
                "detail": "round(s) "
                + ", ".join(str(n) for n in dead_rounds)
                + " had no reviewer return a verdict (all streams error/absent)",
                "severity": "high",
            }
        )

    fix_claims = verify_fix_claims(state, repo_root)
    for row in fix_claims["unverified_rows"]:
        if severity_rank(row.get("severity")) >= severity_rank("high"):
            blockers.append(
                {
                    "code": "unverified_fix_claim",
                    "detail": f"round {row['round']} claims fixed at "
                    f"{row['path']} but did not touch it"
                    + (" (round committed nothing)" if row["no_commit"] else ""),
                    "severity": "high",
                }
            )
        else:
            advisories.append(
                {
                    "code": "unverified_fix_claim",
                    "detail": f"round {row['round']}: {row['path']}",
                }
            )
    if not fix_claims["checked"] and fix_claims["total"]:
        advisories.append(
            {
                "code": "fix_claims_unchecked",
                "detail": "no repo root supplied; fixed-claims not verified",
            }
        )

    # A class claim a later round contradicted. Advisory by construction:
    # making it a blocker would push the loop to stop labelling invariants,
    # which would cost the signal that makes this check possible at all.
    for row in refuted_class_claims(state):
        advisories.append(
            {
                "code": "class_claim_refuted",
                "detail": f"round {row['round']} claimed invariant "
                f"'{row['invariant']}' at {row['path']}; round "
                f"{row['refuted_by_round']} fixed that file again",
            }
        )

    # Reviewer self-assessment: surfaced, never gating.
    for rnd in state.get("rounds") or []:
        for backend, claim in (rnd.get("sufficiency_claims") or {}).items():
            if isinstance(claim, dict) and claim.get("is_confident_complete") is False:
                advisories.append(
                    {
                        "code": "sufficiency_dissent",
                        "detail": f"round {rnd.get('n')}: {backend} reported "
                        "is_confident_complete=false",
                    }
                )

    streams = reviewer_stream_health(state)
    stream_statuses = reviewer_stream_statuses(state)
    for backend, count in streams.items():
        if count != 0:
            continue
        # Say WHY it was silent when the envelope statuses are on record.
        # "codex found nothing" and "codex never returned" warrant opposite
        # responses, and the bare count cannot tell them apart.
        seen = stream_statuses.get(backend) or {}
        live = sum(n for s, n in seen.items() if s in _LIVE_STREAM_STATUSES)
        if seen and live == 0:
            detail = (
                f"{backend} never returned a verdict "
                f"({', '.join(f'{s}x{n}' for s, n in sorted(seen.items()))})"
            )
        else:
            detail = (
                f"{backend} produced zero findings in every "
                "round — consensus was structurally impossible"
            )
        advisories.append({"code": "silent_reviewer", "detail": detail})

    if blockers:
        verdict = "not_ready"
    elif not state.get("completed"):
        verdict = "inconclusive"
    else:
        verdict = "ready"

    return {
        "schema_version": VERDICT_SCHEMA_VERSION,
        "verdict": verdict,
        "pr": state.get("pr_number"),
        "exit_reason": exit_reason,
        "completed_at": state.get("completed_at"),
        "blockers": blockers,
        "advisories": advisories,
        "evidence": {
            "rounds_run": len(state.get("rounds") or []),
            "fix_claims": {
                k: v for k, v in fix_claims.items() if k != "unverified_rows"
            },
            "reviewer_streams": streams,
            "reviewer_stream_statuses": stream_statuses,
            "open_high_deferrals": len(open_high_deferrals(state)),
            # Per-round review vs orchestrator wall time. A round whose
            # orchestrator time dwarfs its review time is the degenerate
            # poll (issue 247); the numbers are how it shows up without
            # someone asking.
            "round_timing": round_timing(state),
        },
    }


def round_timing(state: dict) -> list[dict]:
    """One row per round: review wall time and orchestrator wall time."""
    rows = []
    for rnd in sorted(state.get("rounds") or [], key=lambda r: r.get("n") or 0):
        review_ms = rnd.get("review_wall_ms")
        orch_ms = rnd.get("orchestrator_wall_ms")
        rows.append(
            {
                "n": rnd.get("n"),
                "review_wall_ms": review_ms,
                "orchestrator_wall_ms": orch_ms,
                "summary": (
                    f"review={format_duration_ms(review_ms)} "
                    f"orchestrator={format_duration_ms(orch_ms)}"
                ),
            }
        )
    return rows


def reviewer_stream_health(state: dict) -> dict[str, int]:
    """Findings per backend across all rounds.

    A backend at zero for the whole run is not necessarily broken — the PR
    may be clean — but it means ``>=2 sources`` consensus was unreachable,
    which is invisible in the state file today and looks identical to two
    reviewers agreeing.

    The roster is derived from ``bramble_ops.BACKENDS``, not restated. This
    function's whole job is to distinguish "backend ran and found nothing"
    from "backend never ran" — a hand-written list reintroduces exactly
    that ambiguity for any backend missing from it, since an absent key and
    an unlisted backend both yield no entry in the output.
    """
    keys = {b: f"{b}_findings" for b in bramble_ops.BACKENDS}
    out: dict[str, int] = {}
    for backend, key in keys.items():
        seen = False
        count = 0
        for rnd in state.get("rounds") or []:
            if key in rnd:
                seen = True
                count += len(rnd.get(key) or [])
        if seen:
            out[backend] = count
    return out


def reviewer_stream_statuses(state: dict) -> dict[str, dict[str, int]]:
    """Per-backend tally of persisted envelope statuses across all rounds.

    The count-based view above cannot separate "ran and found nothing" from
    "never ran" — a zero is produced by both. ``stream_status`` records the
    envelope's own status, so report it alongside: ``{"codex": {"ok": 3,
    "error": 2}}``. Backends with no recorded status in any round are omitted,
    matching ``reviewer_stream_health``'s absent-key convention.
    """
    out: dict[str, dict[str, int]] = {}
    for rnd in state.get("rounds") or []:
        statuses = rnd.get("stream_status")
        if not isinstance(statuses, dict):
            continue
        for backend, status in statuses.items():
            out.setdefault(backend, {})
            key = str(status)
            out[backend][key] = out[backend].get(key, 0) + 1
    return out


#: Envelope statuses that mean a reviewer actually returned a verdict.
#: Derived from bramble_ops.STATUSES_WITH_BODY, not restated: that constant
#: exists so a future body-carrying status is added once and every consumer
#: follows. A second literal with the same membership is the drift it was
#: introduced to prevent — this module was the first place to reintroduce it.
_LIVE_STREAM_STATUSES = frozenset(bramble_ops.STATUSES_WITH_BODY)

#: Streams in ``BACKENDS`` that are NOT model reviewers and therefore cannot
#: stand in for one. ``lint`` is a static diff pass: ``lint_gate.py`` writes
#: ``"status": "ok"`` unconditionally and SKILL Step 3.f always passes its
#: envelope, so counting it as coverage made the no-live-reviewer gate
#: unreachable — it fired on a set that always contained a live member.
#: Derived by exclusion from ``bramble_ops.BACKENDS`` so a NEW model backend is
#: counted automatically; only a new non-reviewer stream needs adding here.
_NON_REVIEWER_STREAMS = frozenset({"lint"})


def rounds_without_a_live_stream(state: dict) -> list[int]:
    """Rounds where no reviewer returned a verdict, newest-relevant first.

    A round whose streams are all ``error``/``absent`` reviewed nothing. Its
    empty finding list is produced equally by a clean diff and by a backend
    that never returned, and under the batch protocol the batch pushes on it —
    handing the first real review to the external reviewers.

    Only rounds that recorded ``stream_status`` at all are judged. Rounds
    written before the field existed are skipped rather than assumed broken:
    treating "no data" as "nobody looked" is the same absent-vs-empty
    conflation this function exists to end.
    """
    out: list[int] = []
    for rnd in state.get("rounds") or []:
        statuses = rnd.get("stream_status")
        if not isinstance(statuses, dict) or not statuses:
            continue
        # Only model reviewers count. lint is always present and always "ok",
        # so including it made this predicate unsatisfiable.
        reviewers = {
            k: v for k, v in statuses.items() if k not in _NON_REVIEWER_STREAMS
        }
        if not reviewers:
            continue
        if any(v in _LIVE_STREAM_STATUSES for v in reviewers.values()):
            continue
        n = rnd.get("n")
        if isinstance(n, int):
            out.append(n)
    return out


def main(argv: Optional[list[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("state_dir", type=Path)
    p.add_argument(
        "--repo-root",
        type=Path,
        help="Checkout used to verify fixed-claims. Omitted = unchecked.",
    )
    p.add_argument(
        "--write",
        action="store_true",
        help="Persist to <state_dir>/verdict.json and state['verdict'].",
    )
    args = p.parse_args(argv)

    path = args.state_dir / "pr-polish-state.json"
    try:
        state = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as e:
        print(f"error: cannot read {path}: {e}", file=sys.stderr)
        return 2

    result = compute_verdict(state, repo_root=args.repo_root)
    if args.write:
        atomic_write_json(args.state_dir / "verdict.json", result)
        # Consumers — the harvester, escape_rate.py, anything auditing a past
        # run — read the state file, not the sidecar. A verdict written only
        # to verdict.json is undiscoverable exactly where it gets looked for.
        state["verdict"] = result
        atomic_write_json(path, state)
    print(json.dumps(result, indent=2))
    # Non-zero on anything but a clean bill: this is what makes the verdict
    # usable from a script rather than only readable by a human.
    return 0 if result["verdict"] == "ready" else 1


if __name__ == "__main__":
    raise SystemExit(main())
