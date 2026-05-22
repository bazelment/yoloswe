"""Library for collection mode — building a frozen, judged ground-truth set.

Collection mode (driven by ``collect.py`` + the SKILL) runs multiple rounds
of ``bramble code-review`` over a *single* harvested diff, plus an independent
judge sub-agent per round, until the judge's full-diff census of real bugs
*saturates*. The saturated census is frozen into the dataset JSON as a
``ground_truth_v3`` block, which replay mode then scores against mechanically.

Why this exists: the harvested ``comment_actions`` only label findings the
*original* review surfaced and an engineer acted on. They are incomplete and
the original triage can be wrong. Collection re-establishes truth by judging
the union of findings from several re-reviews against the actual diff, so the
resulting ``true_positives`` set is a *complete* real-bug census, not just the
subset the original loop happened to catch.

The hard parts live here so they can be unit-tested without running bramble
or spawning a judge:

  * :func:`merge_judge_round` — fold a round's verdicts into the cumulative
    TP / FP sets and the cumulative census.
  * :func:`census_converged` — the two-part saturation test.
  * :func:`freeze` — serialize the ``ground_truth_v3`` block and write it
    into the dataset JSON in place (atomic temp + rename).
  * :func:`comment_action_xref` — agreement rate of the judged verdicts vs
    the harvested ``is_real_issue`` labels.

The judge verdict JSON a collection-mode sub-agent writes has this shape::

    {
      "round": 2,
      "finding_verdicts": [
        {"file": "deploy.py", "line": 12, "severity": "high",
         "topic": "unhandled None from get_rollout",
         "verdict": "true_positive",          // | false_positive | unsure
         "reason": "...", "surfaced_by": ["codex"]}
      ],
      "census": [
        {"file": "deploy.py", "line": 12, "severity": "high",
         "description": "real bug present in the diff"}
      ]
    }

``finding_verdicts`` carries one entry per reviewer finding seen so far (the
cumulative union). ``census`` is the judge's *independent* enumeration of the
real bugs in the diff — it may name bugs no reviewer finding caught.
"""

from __future__ import annotations

import json
import re
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Optional

import harvest_lib as hl
from harvest_lib import GROUND_TRUTH_KEY

# The block key stays `ground_truth_v3` (replay mode and the index look for
# it by that name) even though the schema version is now 4 — the v3/v4
# difference is additive (contested list, verdict_history, judge-set
# severity), not a rename. The key itself lives in harvest_lib so the
# harvester (which preserves the block) and this module agree.
GROUND_TRUTH_SCHEMA_VERSION = 4
# Schema versions a frozen GT block may carry that this code still reads.
KNOWN_GT_SCHEMA_VERSIONS = frozenset({3, 4})

VERDICT_TRUE_POSITIVE = "true_positive"
VERDICT_FALSE_POSITIVE = "false_positive"
VERDICT_UNSURE = "unsure"
VALID_VERDICTS = frozenset(
    {VERDICT_TRUE_POSITIVE, VERDICT_FALSE_POSITIVE, VERDICT_UNSURE}
)

# The judge assigns the canonical severity of a finding independently — the
# reviewer's reported severity is only an input. A verdict must use one of
# these; replay scores a finding's severity against the judge's.
VALID_SEVERITIES = frozenset({"high", "medium", "low", "nit"})

# Two findings/census items are "the same defect" when their normalized path
# matches and their lines are within this many rows. Topic is a secondary
# signal — same path+line but a wildly different topic is still merged,
# because two reviewers describing one bug rarely word it identically.
_LINE_SLACK = 3


# ===========================================================================
# Data model
# ===========================================================================


@dataclass
class GTEntry:
    """One judged ground-truth finding (a true positive, false positive, or
    contested defect).

    ``first_seen_round`` / ``surfaced_by`` are accumulated as later rounds
    re-surface the same defect. ``severity`` is the judge's *own* canonical
    severity verdict; ``reviewer_severity`` is what the reviewer reported,
    kept alongside so the divergence is auditable. ``verdict_history`` is the
    per-round trail ``[{round, verdict, reason}]`` — load-bearing for a
    contested entry (one judged differently across rounds). ``resolved`` is
    meaningful only for a contested entry: ``True`` once a later round's
    judge explicitly re-ruled it. ``comment_action_xref`` is the harvested
    ``is_real_issue`` label for the matching ``comment_action``, kept purely
    as an auditable cross-check — never as the verdict.
    """

    file: Optional[str]
    line: Optional[int]
    severity: Optional[str]
    topic: str
    first_seen_round: int
    surfaced_by: list[str] = field(default_factory=list)
    judge_reason: str = ""
    reviewer_severity: Optional[str] = None
    verdict_history: list[dict] = field(default_factory=list)
    resolved: bool = True
    comment_action_xref: Optional[bool] = None


@dataclass
class GroundTruthV3:
    """The frozen, judged ground-truth set for one PR's diff.

    ``true_positives`` is the *complete* real-bug census: the convergence
    test guarantees every censused bug is covered by a reviewer finding, so
    a converged dataset has no separate "missed issues" list. When a PR
    exhausts the round budget without saturating, ``census_converged`` is
    ``False`` and ``per_round_diff`` records exactly which census items were
    still uncovered.

    ``contested`` holds findings judged inconsistently across rounds (e.g.
    ``true_positive`` in one round, ``false_positive`` in another). An
    unresolved contested entry blocks convergence — the ground truth on that
    defect is genuinely unsettled.
    """

    schema_version: int = GROUND_TRUTH_SCHEMA_VERSION
    frozen_at: str = ""
    collector_git_sha: str = ""
    rounds_run: int = 0
    census_converged: bool = False
    true_positives: list[GTEntry] = field(default_factory=list)
    false_positives: list[GTEntry] = field(default_factory=list)
    contested: list[GTEntry] = field(default_factory=list)
    # One audit row per round: census size, new census items, and any
    # census items not yet covered by a TP finding.
    per_round_diff: list[dict] = field(default_factory=list)
    dataset_xref: dict = field(default_factory=dict)


# ===========================================================================
# Defect identity — dedupe key
# ===========================================================================

# bramble records finding paths relative to WORK_DIR — which, in collection
# and replay, is a scratch git worktree. Depending on how that worktree was
# created the path may come through ABSOLUTE, prefixed with the worktree
# root. The harvested dataset stores repo-relative paths. Left unstripped,
# the SAME file looks like two different files and dedup / GT-matching fails.
#
# Strip the worktree-root prefix so every path is repo-relative. The
# patterns below cover replay's `/tmp/replay-*`, the legacy hand-rolled
# `/tmp/crr-*`, and collection's session worktrees `.../<session>/{judge,
# review}-worktree/`.
_WORKTREE_PREFIX_RES = (
    re.compile(r"^/tmp/replay-[^/]+/(.+)$"),
    re.compile(r"^/tmp/crr-[^/]+/(.+)$"),
    re.compile(r"^.*/(?:judge|review)-worktree/(.+)$"),
)


def normalize_finding_path(path: Optional[str]) -> Optional[str]:
    """Strip a worktree-checkout prefix, returning a repo-relative path.

    Idempotent and safe on already-relative paths (they match no prefix and
    pass through). Runs :func:`harvest_lib.normalize_path` afterwards so the
    result is consistent with the harvested dataset's paths.
    """
    if path is None:
        return None
    s = path.strip()
    for rx in _WORKTREE_PREFIX_RES:
        m = rx.match(s)
        if m:
            s = m.group(1)
            break
    return hl.normalize_path(s)


def _norm(text: Optional[str]) -> str:
    return (text or "").strip().lower()


def same_defect(
    a_file: Optional[str],
    a_line: Optional[int],
    b_file: Optional[str],
    b_line: Optional[int],
) -> bool:
    """Whether two (file, line) locations name the same defect.

    Same normalized path is required. Lines match when both are present and
    within ``_LINE_SLACK`` rows, OR when at least one side has no line (a
    file-level finding subsumes a line-level one in the same file).

    Paths are run through :func:`normalize_finding_path` first, so an
    absolute worktree-checkout path and the equivalent repo-relative path
    compare equal.
    """
    pa = normalize_finding_path(a_file)
    pb = normalize_finding_path(b_file)
    if not pa or not pb or pa != pb:
        return False
    if a_line is None or b_line is None:
        return True
    try:
        return abs(int(a_line) - int(b_line)) <= _LINE_SLACK
    except (TypeError, ValueError):
        return False


def _entry_matches(entry: GTEntry, fv: dict) -> bool:
    return same_defect(entry.file, entry.line, fv.get("file"), fv.get("line"))


def _loc_key(file: object, line: object) -> tuple[str, Optional[int]]:
    try:
        line_i: Optional[int] = int(line) if line is not None else None
    except (TypeError, ValueError):
        line_i = None
    return (hl.normalize_path(file if isinstance(file, str) else None) or "",
            line_i)


def _census_key(item: dict) -> tuple[str, Optional[int]]:
    """Canonical identity key for a census item (its own file/line)."""
    return _loc_key(item.get("file"), item.get("line"))


def _census_keys(item: dict) -> set[tuple[str, Optional[int]]]:
    """Every key a census item answers to — its own plus merged members.

    A merged item (carrying ``merged_locations``) is the same defect as any
    of its member locations, so a later round re-censusing one member must
    be recognised as already present, not added as new.
    """
    keys = {_census_key(item)}
    for loc in item.get("merged_locations") or []:
        keys.add(_loc_key(loc.get("file"), loc.get("line")))
    return keys


# ===========================================================================
# Verdict validation
# ===========================================================================


def validate_judge_verdict(obj: object) -> Optional[str]:
    """Return an error string if ``obj`` is not a well-formed verdict JSON."""
    if not isinstance(obj, dict):
        return "verdict is not a JSON object"
    fv = obj.get("finding_verdicts")
    if not isinstance(fv, list):
        return "missing 'finding_verdicts' list"
    for i, v in enumerate(fv):
        if not isinstance(v, dict):
            return f"finding_verdicts[{i}] is not an object"
        if v.get("verdict") not in VALID_VERDICTS:
            return (
                f"finding_verdicts[{i}].verdict must be one of "
                f"{sorted(VALID_VERDICTS)}"
            )
        # The judge assigns the canonical severity; a true/false-positive
        # verdict must carry one. `unsure` need not — it sets no GT.
        sev = v.get("severity")
        if v.get("verdict") != VERDICT_UNSURE and sev not in VALID_SEVERITIES:
            return (
                f"finding_verdicts[{i}].severity must be one of "
                f"{sorted(VALID_SEVERITIES)} (the judge sets it)"
            )
    census = obj.get("census")
    if census is not None and not isinstance(census, list):
        return "'census' must be a list when present"
    merges = obj.get("census_merges")
    if merges is not None:
        if not isinstance(merges, list):
            return "'census_merges' must be a list when present"
        for i, m in enumerate(merges):
            if not isinstance(m, dict):
                return f"census_merges[{i}] is not an object"
            members = m.get("members")
            if not isinstance(members, list) or len(members) < 2:
                return (
                    f"census_merges[{i}].members must be a list of >=2 "
                    "census locations"
                )
    return None


# ===========================================================================
# Round folding + convergence
# ===========================================================================


@dataclass
class CumulativeGT:
    """Mutable accumulator threaded across collection rounds.

    ``census`` holds the union of every census item the judge has named so
    far, deduped by :func:`_census_key`. ``last_round_census_keys`` is the
    census-key set as of the previous round, used by :func:`census_converged`
    to detect a no-change round. ``contested`` holds defects judged
    inconsistently across rounds — until a later round re-rules them.
    """

    true_positives: list[GTEntry] = field(default_factory=list)
    false_positives: list[GTEntry] = field(default_factory=list)
    contested: list[GTEntry] = field(default_factory=list)
    census: list[dict] = field(default_factory=list)
    per_round_diff: list[dict] = field(default_factory=list)
    last_round_census_keys: set = field(default_factory=set)
    rounds_run: int = 0


def _new_entry(fv: dict, round_n: int, verdict: str) -> GTEntry:
    """Build a GTEntry from a finding verdict, seeding its verdict_history."""
    return GTEntry(
        file=fv.get("file"),
        line=fv.get("line"),
        severity=fv.get("severity"),
        topic=str(fv.get("topic") or fv.get("reason") or ""),
        first_seen_round=round_n,
        surfaced_by=list(fv.get("surfaced_by") or []),
        judge_reason=str(fv.get("reason") or ""),
        reviewer_severity=fv.get("reviewer_severity"),
        verdict_history=[
            {"round": round_n, "verdict": verdict,
             "reason": str(fv.get("reason") or "")}
        ],
        resolved=True,
    )


def _accumulate_into(existing: GTEntry, fv: dict, round_n: int,
                     verdict: str) -> None:
    """Fold a re-surfacing finding verdict into an existing same-verdict entry.

    Accumulates ``surfaced_by``, appends to ``verdict_history``, and keeps the
    judge's *latest* severity verdict (a later round saw more context).
    """
    existing.first_seen_round = min(existing.first_seen_round, round_n)
    for src in fv.get("surfaced_by") or []:
        if src and src not in existing.surfaced_by:
            existing.surfaced_by.append(src)
    if not existing.judge_reason and fv.get("reason"):
        existing.judge_reason = str(fv.get("reason"))
    if fv.get("severity"):
        existing.severity = fv.get("severity")
    if fv.get("reviewer_severity") and not existing.reviewer_severity:
        existing.reviewer_severity = fv.get("reviewer_severity")
    existing.verdict_history.append(
        {"round": round_n, "verdict": verdict,
         "reason": str(fv.get("reason") or "")}
    )


def _find_entry(bucket: list[GTEntry], fv: dict) -> Optional[GTEntry]:
    for e in bucket:
        if _entry_matches(e, fv):
            return e
    return None


def apply_census_merges(
    cumulative: CumulativeGT, merges: list[dict]
) -> None:
    """Record the judge's "these locations are one defect" declarations.

    Each merge is ``{"members": [{file, line}, ...]}`` — two or more
    locations the judge declared facets of a single defect. A member may be:

    - **a census item** — collapsed: the first census member is kept as the
      canonical entry, any other census members are removed; OR
    - **a finding location** — not in the census (e.g. a reviewer finding 20
      lines from the censused line). It is *not* added to the census, but it
      IS recorded on the kept entry's ``merged_locations``.

    Every member location lands in the kept entry's ``merged_locations`` so
    :func:`census_uncovered` treats the merged defect as covered when a
    true-positive finding lands near **any** of them. This is what lets a
    finding cover a census item it sits more than ``_LINE_SLACK`` rows from
    — the line-precise match alone would miss it.

    A merge with no census member at all is skipped (nothing to anchor it).
    """
    for m in merges:
        members = [
            loc for loc in (m.get("members") or []) if isinstance(loc, dict)
        ]
        member_keys = {_census_key(loc) for loc in members}
        present = [
            i
            for i, c in enumerate(cumulative.census)
            if _census_key(c) in member_keys
        ]
        if not present:
            continue  # no census entry to anchor the merge to
        # Keep the first present census member as canonical; drop the rest.
        keep, *rest = present
        drop = set(rest)
        # Record EVERY declared member location — census or not — so a
        # true-positive finding near any of them covers the merged defect.
        locations = [
            {"file": loc.get("file"), "line": loc.get("line")}
            for loc in members
        ]
        existing = cumulative.census[keep].get("merged_locations") or []
        cumulative.census[keep]["merged_locations"] = (
            existing + [loc for loc in locations if loc not in existing]
        )
        cumulative.census = [
            c for i, c in enumerate(cumulative.census) if i not in drop
        ]


def _route_finding_verdict(
    cumulative: CumulativeGT, fv: dict, round_n: int
) -> None:
    """Route one finding verdict, detecting and resolving verdict conflicts.

    The quality-critical path. A defect's verdict can disagree across rounds;
    the dataset must never silently keep one. Cases, in order:

    1. **Resolution** — the defect is in ``contested``: this round's verdict
       is binding. Append to history, move it into TP or FP, mark resolved.
    2. **Flip** — the defect stands in TP and this round says FP (or vice
       versa): move it to ``contested`` (``resolved=False``) with both
       verdicts in history. It blocks convergence until a round re-rules it.
    3. **Agreement** — the defect stands in the bucket this verdict targets:
       accumulate (surfaced_by, history, latest severity).
    4. **New** — unseen defect: create the entry in the right bucket.

    ``unsure`` carries no ground truth — it does not create, flip, or
    resolve anything; it is dropped.
    """
    kind = fv.get("verdict")
    if kind == VERDICT_UNSURE:
        return
    if kind == VERDICT_TRUE_POSITIVE:
        same_bucket, other_bucket = (
            cumulative.true_positives, cumulative.false_positives
        )
    else:  # VERDICT_FALSE_POSITIVE
        same_bucket, other_bucket = (
            cumulative.false_positives, cumulative.true_positives
        )

    # 1. Resolution — this verdict re-rules a contested defect.
    contested = _find_entry(cumulative.contested, fv)
    if contested is not None:
        cumulative.contested.remove(contested)
        _accumulate_into(contested, fv, round_n, kind)
        contested.resolved = True
        existing = _find_entry(same_bucket, fv)
        if existing is not None:
            # The resolving verdict's bucket already holds a sibling entry —
            # fold the contested entry's history in and keep the one entry.
            existing.verdict_history.extend(contested.verdict_history)
            for src in contested.surfaced_by:
                if src not in existing.surfaced_by:
                    existing.surfaced_by.append(src)
        else:
            same_bucket.append(contested)
        return

    # 2. Flip — the defect stands in the OTHER bucket.
    flipped = _find_entry(other_bucket, fv)
    if flipped is not None:
        other_bucket.remove(flipped)
        _accumulate_into(flipped, fv, round_n, kind)
        flipped.resolved = False
        cumulative.contested.append(flipped)
        return

    # 3. Agreement — the defect already stands in the target bucket.
    existing = _find_entry(same_bucket, fv)
    if existing is not None:
        _accumulate_into(existing, fv, round_n, kind)
        return

    # 4. New defect.
    same_bucket.append(_new_entry(fv, round_n, kind))


def merge_judge_round(
    cumulative: CumulativeGT,
    round_n: int,
    verdict: dict,
) -> CumulativeGT:
    """Fold one round's judge verdict into the cumulative GT accumulator.

    ``verdict`` is one round's parsed (and validated) judge verdict JSON.
    Mutates and returns ``cumulative``. Each finding verdict is routed by
    :func:`_route_finding_verdict` (TP / FP / contested / resolution;
    ``unsure`` dropped), each census item is unioned into
    ``cumulative.census``, and any ``census_merges`` the judge declared are
    applied.
    """
    for fv in verdict.get("finding_verdicts") or []:
        _route_finding_verdict(cumulative, fv, round_n)

    prev_keys = set(cumulative.last_round_census_keys)
    new_items: list[dict] = []
    for item in verdict.get("census") or []:
        if not isinstance(item, dict):
            continue
        key = _census_key(item)
        # Already present — directly, or as a member of a merged item.
        present = any(
            key in _census_keys(c) for c in cumulative.census
        )
        if present:
            continue
        cumulative.census.append(item)
        new_items.append(item)

    # Apply the judge's census merges: the judge is the authority on defect
    # identity. When it declares several census locations to be one defect
    # (e.g. an early round over-split a file-scoped test-coverage gap into
    # two line numbers), collapse them so the mechanical line-precise
    # coverage check doesn't keep one facet permanently "uncovered".
    apply_census_merges(cumulative, verdict.get("census_merges") or [])

    # The convergence "unchanged" test compares the set of ALL keys (each
    # item's own plus its merged members), so a merge round changes the set
    # exactly once and a later round re-citing a merged member does not.
    this_keys: set = set()
    for c in cumulative.census:
        this_keys |= _census_keys(c)
    uncovered = census_uncovered(cumulative)
    unresolved = [e for e in cumulative.contested if not e.resolved]
    cumulative.per_round_diff.append(
        {
            "round": round_n,
            "census_size": len(cumulative.census),
            "new_census_items": new_items,
            "census_unchanged_vs_prev": this_keys == prev_keys,
            "uncovered_census_items": uncovered,
            "contested_count": len(cumulative.contested),
            "unresolved_contested_count": len(unresolved),
        }
    )
    cumulative.last_round_census_keys = this_keys
    cumulative.rounds_run = max(cumulative.rounds_run, round_n)
    return cumulative


def census_uncovered(cumulative: CumulativeGT) -> list[dict]:
    """Census items not covered by any true-positive finding.

    A census item is *covered* when some ``true_positives`` entry names the
    same defect (:func:`same_defect`). For a merged item (carrying
    ``merged_locations``), a finding near **any** member location counts —
    the judge declared those locations one defect. At convergence this list
    is empty.
    """
    out: list[dict] = []
    for item in cumulative.census:
        locations = item.get("merged_locations") or [
            {"file": item.get("file"), "line": item.get("line")}
        ]
        covered = any(
            same_defect(tp.file, tp.line, loc.get("file"), loc.get("line"))
            for tp in cumulative.true_positives
            for loc in locations
        )
        if not covered:
            out.append(item)
    return out


def unresolved_contested(cumulative: CumulativeGT) -> list[GTEntry]:
    """Contested defects no round has re-ruled yet — they block convergence."""
    return [e for e in cumulative.contested if not e.resolved]


def census_converged(cumulative: CumulativeGT) -> bool:
    """The three-part saturation test.

    Converges when **all** hold after the most recent round:

      1. the cumulative census set was unchanged versus the prior round
         (no new real bug surfaced), and
      2. every census item is covered by a true-positive finding (the
         reviewers, in aggregate, caught every real bug the judge censused),
         and
      3. no contested finding is still unresolved — the judges have not left
         a defect they disagreed on un-re-ruled.

    Needs at least two rounds — a single round has nothing to be "unchanged"
    against. The empty-census case still requires two rounds so a reviewer
    that simply found nothing twice is what produces the clean signal.
    """
    if cumulative.rounds_run < 2:
        return False
    last = cumulative.per_round_diff[-1] if cumulative.per_round_diff else {}
    if not last.get("census_unchanged_vs_prev"):
        return False
    if unresolved_contested(cumulative):
        return False
    return not census_uncovered(cumulative)


# ===========================================================================
# Cross-check vs harvested comment_actions
# ===========================================================================


def comment_action_xref(
    cumulative: CumulativeGT,
    harvested_rounds: list[dict],
) -> dict:
    """Agreement rate of the judged verdicts vs harvested ``is_real_issue``.

    For each TP/FP entry, find the harvested finding at the same defect
    location and compare its ``ground_truth.is_real_issue`` to the judge's
    verdict (TP ⇒ real, FP ⇒ not real). Entries where the harvest had no
    opinion (``is_real_issue`` null, or no matching finding) are skipped.

    A low agreement rate means the harvested dataset is unreliable — worth
    re-triaging the original PR or fixing the harvester. It is reported, not
    acted on: the judge's verdict stands.
    """
    harvested: list[tuple[Optional[str], Optional[int], Optional[bool]]] = []
    for hr in harvested_rounds or []:
        for rr in hr.get("review_runs") or []:
            for f in rr.get("findings") or []:
                gt = f.get("ground_truth") or {}
                harvested.append(
                    (f.get("file"), f.get("line"), gt.get("is_real_issue"))
                )

    def _harvest_label(
        file: Optional[str], line: Optional[int]
    ) -> Optional[bool]:
        for h_file, h_line, is_real in harvested:
            if is_real is None:
                continue
            if same_defect(file, line, h_file, h_line):
                return bool(is_real)
        return None

    comparisons = 0
    agreements = 0
    disagreements: list[dict] = []
    # Contested entries get their xref label stamped too (for audit), but
    # are excluded from the agreement rate — their verdict is not settled.
    for entry in cumulative.contested:
        entry.comment_action_xref = _harvest_label(entry.file, entry.line)
    for entry, judged_real in (
        [(e, True) for e in cumulative.true_positives]
        + [(e, False) for e in cumulative.false_positives]
    ):
        label = _harvest_label(entry.file, entry.line)
        entry.comment_action_xref = label
        if label is None:
            continue
        comparisons += 1
        if label == judged_real:
            agreements += 1
        else:
            disagreements.append(
                {
                    "file": entry.file,
                    "line": entry.line,
                    "topic": entry.topic,
                    "judge_verdict": (
                        VERDICT_TRUE_POSITIVE
                        if judged_real
                        else VERDICT_FALSE_POSITIVE
                    ),
                    "harvest_is_real_issue": label,
                }
            )

    return {
        "comparisons": comparisons,
        "agreements": agreements,
        "comment_action_agreement_rate": (
            agreements / comparisons if comparisons else None
        ),
        "disagreements": disagreements,
    }


# ===========================================================================
# Freeze
# ===========================================================================


def build_ground_truth(
    cumulative: CumulativeGT,
    *,
    collector_git_sha: str,
    harvested_rounds: list[dict],
    frozen_at: Optional[str] = None,
) -> GroundTruthV3:
    """Assemble the final :class:`GroundTruthV3` block from the accumulator."""
    xref = comment_action_xref(cumulative, harvested_rounds)
    return GroundTruthV3(
        frozen_at=frozen_at or hl.iso_utc_now(),
        collector_git_sha=collector_git_sha,
        rounds_run=cumulative.rounds_run,
        census_converged=census_converged(cumulative),
        true_positives=list(cumulative.true_positives),
        false_positives=list(cumulative.false_positives),
        contested=list(cumulative.contested),
        per_round_diff=list(cumulative.per_round_diff),
        dataset_xref=xref,
    )


def ground_truth_to_dict(gt: GroundTruthV3) -> dict:
    return asdict(gt)


def freeze(dataset_path: Path, gt: GroundTruthV3) -> Path:
    """Write the ``ground_truth_v3`` block into the dataset JSON in place.

    The harvested ``harvested_rounds`` / ``pr`` fields are left untouched —
    collection mode only *adds* the block.
    """
    dataset = json.loads(dataset_path.read_text())
    dataset[GROUND_TRUTH_KEY] = ground_truth_to_dict(gt)
    return hl.atomic_write_json(dataset_path, dataset)


def load_ground_truth(dataset: dict) -> Optional[dict]:
    """Return the frozen ``ground_truth_v3`` block, or None if not collected."""
    block = dataset.get(GROUND_TRUTH_KEY)
    return block if isinstance(block, dict) else None


# ===========================================================================
# Dataset validation — structural checks + quality gates
# ===========================================================================

# A frozen GT whose `dataset_xref` agreement rate is below this is suspect:
# the judge disagreed with the harvested triage often enough that either the
# harvester's matcher or the original PR triage is unreliable.
_MIN_AGREEMENT_RATE = 0.6


def validate_dataset(
    dataset: dict, *, round_budget: int = 10
) -> tuple[list[str], list[str]]:
    """Validate one per-PR dataset record. Returns ``(errors, warnings)``.

    **errors** are structural — a malformed record a consumer cannot trust.
    **warnings** are quality gates — a well-formed but *weak* dataset
    (unconverged, contested, low harvest agreement, budget-forced, empty).

    A record with no ``ground_truth_v3`` block is structurally valid but
    warns "ground truth not collected" — the harvest alone is not a usable
    benchmark.
    """
    errors: list[str] = []
    warnings: list[str] = []

    sv = dataset.get("schema_version")
    if sv is None:
        errors.append("missing top-level schema_version")
    elif sv not in (2,):  # the harvested per-PR schema
        warnings.append(f"unexpected dataset schema_version={sv}")

    gt = load_ground_truth(dataset)
    if gt is None:
        warnings.append(
            "ground truth not collected (no ground_truth_v3 block) — "
            "run /code-review-replay collect"
        )
        return errors, warnings

    gt_sv = gt.get("schema_version")
    if gt_sv not in KNOWN_GT_SCHEMA_VERSIONS:
        errors.append(
            f"ground_truth_v3.schema_version={gt_sv} is not known "
            f"{sorted(KNOWN_GT_SCHEMA_VERSIONS)}"
        )
    elif gt_sv != GROUND_TRUTH_SCHEMA_VERSION:
        warnings.append(
            f"ground_truth_v3 is schema {gt_sv}, current is "
            f"{GROUND_TRUTH_SCHEMA_VERSION} — re-collect for the "
            "contested-verdict / judge-severity guarantees"
        )

    for bucket in ("true_positives", "false_positives", "contested"):
        rows = gt.get(bucket)
        if rows is None and bucket == "contested" and gt_sv == 3:
            continue  # schema 3 had no contested list — already warned
        if not isinstance(rows, list):
            errors.append(f"ground_truth_v3.{bucket} must be a list")
            continue
        for i, e in enumerate(rows):
            if not isinstance(e, dict):
                errors.append(f"{bucket}[{i}] is not an object")
                continue
            if e.get("file") is None:
                errors.append(f"{bucket}[{i}] missing 'file'")
            if "line" not in e:
                errors.append(f"{bucket}[{i}] missing 'line'")
            if e.get("severity") not in VALID_SEVERITIES:
                errors.append(
                    f"{bucket}[{i}] severity {e.get('severity')!r} not in "
                    f"{sorted(VALID_SEVERITIES)}"
                )
    if not isinstance(gt.get("per_round_diff"), list):
        errors.append("ground_truth_v3.per_round_diff must be a list")

    # ---- quality gates ----
    if not gt.get("census_converged"):
        warnings.append("census did not converge — recall denominator may "
                        "be incomplete")
    unresolved = [
        e for e in (gt.get("contested") or [])
        if isinstance(e, dict) and not e.get("resolved", True)
    ]
    if unresolved:
        warnings.append(
            f"{len(unresolved)} contested finding(s) unresolved — judges "
            "disagreed and no round re-ruled them"
        )
    rate = (gt.get("dataset_xref") or {}).get("comment_action_agreement_rate")
    if rate is not None and rate < _MIN_AGREEMENT_RATE:
        warnings.append(
            f"low harvest agreement ({rate:.2f} < {_MIN_AGREEMENT_RATE}) — "
            "the harvested triage or matcher is suspect"
        )
    if gt.get("rounds_run") == round_budget:
        warnings.append(
            f"rounds_run hit the budget ({round_budget}) — convergence may "
            "have been forced"
        )
    if not (gt.get("true_positives") or []):
        warnings.append("true_positives is empty — no real bugs in the GT")

    return errors, warnings


# ===========================================================================
# Index refresh
# ===========================================================================


def index_entry_fields(per_pr: dict) -> dict:
    """The collection-quality fields an ``index.json`` entry carries.

    Computed from a per-PR dataset dict: whether ground truth was collected
    and, if so, its convergence flag. Shared by :func:`refresh_index_entry`
    and the harvester's ``build_index`` so the two never drift.
    """
    gt = load_ground_truth(per_pr)
    return {
        "ground_truth_collected": gt is not None,
        "census_converged": (
            bool(gt.get("census_converged")) if gt is not None else None
        ),
    }


def refresh_index_entry(out_dir: Path, repo_pr: str) -> Optional[Path]:
    """Patch one PR's entry in ``index.json`` after its GT block changed.

    ``harvest.py`` writes ``index.json`` once, before collection runs — so a
    just-frozen GT block is invisible in the index until this re-reads the
    per-PR file and patches that one entry's collection-quality fields. Other
    entries are untouched. Atomic temp+rename, mirroring :func:`freeze`.

    Returns the index path on success, or ``None`` when there is no
    ``index.json`` or no per-PR file to read.
    """
    index_path = out_dir / "index.json"
    per_pr_path = out_dir / f"{repo_pr}.json"
    if not index_path.is_file() or not per_pr_path.is_file():
        return None
    index = json.loads(index_path.read_text())
    per_pr = json.loads(per_pr_path.read_text())
    fields = index_entry_fields(per_pr)
    patched = False
    for entry in index.get("prs") or []:
        if entry.get("file") == f"{repo_pr}.json":
            entry.update(fields)
            patched = True
            break
    if not patched:
        return None
    return hl.atomic_write_json(index_path, index)
