#!/usr/bin/env python3
"""Collection mode — build a frozen, judged ground-truth dataset.

The *expensive* half of the ``/code-review-replay`` skill. The SKILL drives
the per-PR round loop and spawns the judge sub-agents; this script is the
stateless mechanical glue, with one sub-command per step:

  scan            list collectable PRs (also the no-subcommand default).
  setup <pr>      harvest if needed, discover the repo, pin a worktree.
  build-prompt    write a round's judge prompt-input JSON.
  fold            merge a round's judge verdict; report convergence.
  freeze          write the ground_truth_v3 block; tear down the worktree.
  validate [pr]   structural + quality check of a frozen dataset.

Repo checkouts are auto-discovered (no ``--repos-root``). Collection state
lives in a *session directory* the SKILL threads via ``--session``::

    <session>/session-meta.json    target + source repo (set by `setup`)
    <session>/worktree/            ONE detached worktree pinned at head_before
    <session>/cumulative.json      serialized CumulativeGT (across rounds)
    <session>/r<N>/judge-prompt.json    a round's prompt-input
    <session>/r<N>/judge-verdict.json   the judge sub-agent's verdict
"""

from __future__ import annotations

import argparse
import json
import random
import subprocess
import sys
from dataclasses import asdict
from pathlib import Path
from typing import Optional

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import collect_lib as cl  # noqa: E402
import harvest_lib as hl  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parents[4]
EVAL_ROOT = Path.home() / ".bramble" / "code-review-eval"
DEFAULT_DATASET_DIR = EVAL_ROOT / "dataset"
DEFAULT_SESSION_ROOT = EVAL_ROOT / "collect"
# pr-polish run history — the set of PRs collection can harvest from.
DEFAULT_SOURCE_DIR = Path.home() / ".bramble" / "projects"

_CUMULATIVE_FILE = "cumulative.json"


# ---------------------------------------------------------------------------
# Session state — serialize / restore the CumulativeGT accumulator
# ---------------------------------------------------------------------------


def _cumulative_path(session: Path) -> Path:
    return session / _CUMULATIVE_FILE


def load_cumulative(session: Path) -> cl.CumulativeGT:
    """Restore the accumulator, or a fresh one if this is the first round."""
    path = _cumulative_path(session)
    if not path.is_file():
        return cl.CumulativeGT()
    raw = json.loads(path.read_text())
    return cl.CumulativeGT(
        true_positives=[cl.GTEntry(**e) for e in raw.get("true_positives", [])],
        false_positives=[
            cl.GTEntry(**e) for e in raw.get("false_positives", [])
        ],
        contested=[cl.GTEntry(**e) for e in raw.get("contested", [])],
        census=list(raw.get("census", [])),
        per_round_diff=list(raw.get("per_round_diff", [])),
        last_round_census_keys={
            tuple(k) for k in raw.get("last_round_census_keys", [])
        },
        rounds_run=int(raw.get("rounds_run", 0)),
    )


def save_cumulative(session: Path, cumulative: cl.CumulativeGT) -> None:
    """Persist the accumulator. ``last_round_census_keys`` is a set of tuples,
    which JSON cannot hold — store it as a list of lists and rebuild on load.
    """
    payload = {
        "true_positives": [asdict(e) for e in cumulative.true_positives],
        "false_positives": [asdict(e) for e in cumulative.false_positives],
        "contested": [asdict(e) for e in cumulative.contested],
        "census": cumulative.census,
        "per_round_diff": cumulative.per_round_diff,
        "last_round_census_keys": [
            list(k) for k in cumulative.last_round_census_keys
        ],
        "rounds_run": cumulative.rounds_run,
    }
    hl.atomic_write_json(_cumulative_path(session), payload)


# ---------------------------------------------------------------------------
# Dataset / round helpers
# ---------------------------------------------------------------------------


def _load_dataset(dataset_dir: Path, target: str) -> tuple[Path, dict]:
    path = (
        Path(target)
        if target.endswith(".json")
        else dataset_dir / f"{target}.json"
    )
    if not path.is_file():
        raise SystemExit(
            f"error: dataset not found: {path}\n"
            f"  Run `collect.py setup {target}` to harvest it."
        )
    return path, json.loads(path.read_text())


def _harvest_one(
    target: str, *, dataset_dir: Path, repo_map: "hl.RepoMap"
) -> None:
    """Harvest one PR into ``dataset_dir`` by invoking the harvester.

    Runs ``harvest.py --only <target>`` as a subprocess — the harvester is a
    standalone entry point and reusing it keeps the PR-comment fetch / goal
    reconstruction in one tested place. Repo roots are auto-discovered, so
    no ``--repo-root`` is passed.
    """
    res = subprocess.run(
        [sys.executable, str(SCRIPT_DIR / "harvest.py"),
         "--only", target, "--out-dir", str(dataset_dir)],
        capture_output=True, text=True, check=False,
    )
    if res.returncode == 2:  # 2 == nothing harvested
        raise SystemExit(
            f"error: harvest of {target} found nothing — no "
            f"~/.bramble/projects/{target}/ dir.\n{res.stderr.strip()}"
        )


def discover_collectable(
    *, dataset_dir: Path, source_dir: Path
) -> list[dict]:
    """Every PR collection *could* run on, with its collection status.

    A PR is *collectable* when ``~/.bramble/projects/<repo>-<pr>/`` exists
    (it was ``/pr-polish``'d here). Each entry reports whether a frozen
    ``ground_truth_v3`` block already exists (``collected``). Sorted by id.
    """
    out: list[dict] = []
    for _dir, repo, pr in hl.discover_project_dirs(source_dir):
        target = f"{repo}-{pr}"
        ds_path = dataset_dir / f"{target}.json"
        collected = False
        if ds_path.is_file():
            try:
                collected = cl.load_ground_truth(
                    json.loads(ds_path.read_text())
                ) is not None
            except (OSError, json.JSONDecodeError):
                collected = False
        out.append({"target": target, "collected": collected})
    return out


def _canonical_round(dataset: dict) -> dict:
    """The harvested round whose diff collection re-reviews.

    Collection establishes ground truth for one diff — the original PR's
    fresh-eyes diff. That is R1 (or the lone ``r1_only`` round). The final
    round is near-converged code, not the diff we want a bug census for.
    """
    rounds = dataset.get("harvested_rounds") or []
    for r in rounds:
        if r.get("signal_tier") in {"r1", "r1_only"}:
            return r
    if rounds:
        return rounds[0]
    raise SystemExit("error: dataset has no harvested_rounds")


def _finding_dict(f: dict, backend: str) -> dict:
    """One reviewer finding in the judge-prompt shape, tagged ``surfaced_by``.

    The path is run through :func:`collect_lib.normalize_finding_path` —
    bramble emits paths relative to its WORK_DIR worktree, so the prefix must
    be stripped for them to match the dataset's repo-relative paths (and each
    other across rounds).
    """
    return {
        "file": cl.normalize_finding_path(f.get("file")),
        "line": f.get("line"),
        "severity": f.get("severity"),
        "message": f.get("message"),
        "suggestion": f.get("suggestion"),
        "surfaced_by": [backend],
    }


def _findings_from_envelope(path: Path, backend: str) -> list[dict]:
    """Extract reviewer findings from a bramble ``--envelope-file`` JSON.

    Each finding is tagged with ``surfaced_by`` so the judge — and the
    cumulative accumulator — can record which backend(s) caught it.
    """
    try:
        env = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as e:
        print(f"  warning: unreadable envelope {path}: {e}", file=sys.stderr)
        return []
    return [_finding_dict(f, backend) for f in hl.envelope_issues(env)]


def _findings_from_harvested_round(round_data: dict) -> list[dict]:
    """The harvested dataset's own reviewer findings for the canonical round.

    Used as round 1's finding set — the original ``/pr-polish`` review is a
    free first data point, no re-review needed for it.
    """
    out: list[dict] = []
    for rr in round_data.get("review_runs") or []:
        backend = rr.get("backend") or "?"
        for f in rr.get("findings") or []:
            out.append(_finding_dict(f, backend))
    return out


def _dedupe_findings(findings: list[dict]) -> list[dict]:
    """Merge findings naming the same defect, unioning their ``surfaced_by``.

    The judge prompt should show one entry per distinct defect across all
    backends and rounds — not the same off-by-one three times.
    """
    merged: list[dict] = []
    for f in findings:
        for m in merged:
            if cl.same_defect(m.get("file"), m.get("line"),
                              f.get("file"), f.get("line")):
                for src in f.get("surfaced_by") or []:
                    if src and src not in m["surfaced_by"]:
                        m["surfaced_by"].append(src)
                break
        else:
            merged.append({**f, "surfaced_by": list(f.get("surfaced_by") or [])})
    return merged


# ---------------------------------------------------------------------------
# Worktree — pin the whole collection run to ONE commit
# ---------------------------------------------------------------------------
#
# Collection establishes ground truth for ONE diff: the canonical round's
# `merge_base..head_before`. ONE detached worktree, pinned at exactly
# `head_before`, is created at session setup and reused by every round. It
# is both:
#
#   * the cwd for the SKILL's `bramble code-review` re-runs, and
#   * the judge sub-agent's `repo_path`.
#
# `bramble code-review` is read-only and the judge only reads, so a single
# worktree serves both with no conflict. Pinning to `head_before` (not the
# live checkout's HEAD, which drifts) is load-bearing: a judge reading
# today's HEAD would see already-fixed code and wrongly call real findings
# false positives. Created by `setup`, torn down by `freeze`.


def _worktree_path(session: Path) -> Path:
    return session / "worktree"


def _meta_path(session: Path) -> Path:
    return session / "session-meta.json"


def save_session_meta(
    session: Path, *, source_repo: Path, target: str
) -> None:
    """Record session facts so later sub-commands need only ``--session``.

    ``source_repo`` lets ``freeze`` prune the worktree; ``target`` is the
    ``<repo>-<pr>`` id so ``build-prompt`` / ``freeze`` resolve the dataset
    without re-passing it.
    """
    hl.atomic_write_json(
        _meta_path(session),
        {"source_repo": str(source_repo), "target": target},
    )


def load_session_meta(session: Path) -> dict:
    """The facts recorded at session setup, or ``{}`` if absent."""
    path = _meta_path(session)
    if not path.is_file():
        return {}
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return {}


def ensure_worktree(
    session: Path, source_repo: Path, head_before: str
) -> Path:
    """Create (or reuse) the session worktree pinned to ``head_before``.

    Idempotent: an existing worktree already at the right commit is returned
    as-is, so re-running setup does not churn it.
    """
    wt = _worktree_path(session)
    if wt.exists():
        head = hl.git(wt, "rev-parse", "HEAD")
        if head.returncode == 0 and head.stdout.strip().startswith(
            head_before[:12]
        ):
            return wt
        remove_worktree(session, source_repo)
    res = hl.git(
        source_repo, "worktree", "add", "--detach", str(wt), head_before
    )
    if res.returncode != 0:
        raise SystemExit(
            f"error: git worktree add failed: "
            f"{res.stderr.strip() or '(no stderr)'}"
        )
    return wt


def remove_worktree(session: Path, source_repo: Path) -> None:
    """Tear down the session worktree. Best-effort — never fatal."""
    wt = _worktree_path(session)
    if not wt.exists():
        return
    hl.git(source_repo, "worktree", "remove", "--force", str(wt))
    hl.git(source_repo, "worktree", "prune")


# ---------------------------------------------------------------------------
# Sub-command: setup — prepare a PR for the collection round loop
# ---------------------------------------------------------------------------


def setup(
    *,
    target: str,
    dataset_dir: Path,
    session_root: Path,
    repo_map: "hl.RepoMap",
) -> dict:
    """Prepare one PR for collection: harvest if needed, pin a worktree.

    Returns the facts the SKILL needs to drive the round loop: the session
    dir, the pinned worktree path, and the canonical round's diff scope
    (``head_before``, ``merge_base``, ``goal_text``, ``files_changed``).
    Harvests the PR on the fly if it has no dataset file yet.
    """
    dataset_path = dataset_dir / f"{target}.json"
    if not dataset_path.is_file():
        _harvest_one(target, dataset_dir=dataset_dir, repo_map=repo_map)
    if not dataset_path.is_file():
        raise SystemExit(
            f"error: could not harvest {target} — was it /pr-polish'd on "
            "this machine? (no ~/.bramble/projects/ dir)"
        )
    dataset = json.loads(dataset_path.read_text())
    rnd = _canonical_round(dataset)
    pr = dataset.get("pr") or {}
    repo_name = pr.get("repo_name") or ""

    head_before = rnd.get("head_before")
    if not head_before:
        raise SystemExit(
            f"error: {target}'s canonical round has no head_before — the "
            "dataset is unusable for collection"
        )
    source_repo = repo_map.lookup(repo_name)
    if source_repo is None or not source_repo.is_dir():
        raise SystemExit(
            f"error: no local checkout discovered for repo {repo_name!r} "
            f"(need ~/worktrees/{repo_name}/main or ~/g/{repo_name})"
        )

    session = _new_session(session_root, target)
    session.mkdir(parents=True, exist_ok=True)
    save_session_meta(session, source_repo=source_repo, target=target)
    worktree = ensure_worktree(session, source_repo, head_before)

    return {
        "target": target,
        "session": str(session),
        "worktree": str(worktree),
        "canonical_round": {
            "round": rnd.get("round"),
            "head_before": head_before,
            "merge_base_sha": rnd.get("merge_base_sha") or head_before,
            "base_branch": rnd.get("base_branch"),
            "goal_text": rnd.get("goal_text") or "",
            "files_changed": rnd.get("files_changed") or [],
        },
    }


# ---------------------------------------------------------------------------
# Sub-command: build-prompt — write a round's judge prompt-input
# ---------------------------------------------------------------------------

_JUDGE_INSTRUCTIONS = (
    "You are an independent code-review judge building a ground-truth "
    "dataset. Inspect the REAL diff and reach your OWN verdict on each "
    "finding; assign each finding its canonical SEVERITY yourself (the "
    "reviewer's severity is only an input); independently census the real "
    "bugs in the diff; and give a final verdict on every entry in "
    "contested_findings (defects prior rounds disagreed on). The harvested "
    "labels are REFERENCE ONLY. See the /code-review-replay SKILL for the "
    "full prompt and the verdict JSON schema you must write to "
    "verdict_output_path."
)


def build_prompt(
    *,
    session: Path,
    round_n: int,
    envelopes: list[tuple[str, Path]],
    include_harvested: bool,
    dataset_dir: Path,
) -> Path:
    """Write the round's judge prompt-input JSON. Returns its path.

    The prompt carries the *cumulative* union of reviewer findings (this
    round's envelopes + every prior round's, deduped by defect), the running
    census, and any contested findings — so the judge always verdicts the
    full known set. Pass ``include_harvested`` to fold the harvested
    pr-polish review into this round's finding set (the round loop does this
    every round — it is a free data point, not a round-1 special case).
    """
    meta = load_session_meta(session)
    target = meta.get("target")
    if not target:
        raise SystemExit(
            f"error: session {session} has no recorded target — run "
            "`collect.py setup` first"
        )
    dataset_path, dataset = _load_dataset(dataset_dir, target)
    rnd = _canonical_round(dataset)
    pr = dataset.get("pr") or {}
    repo_pr = f"{pr.get('repo_name')}-{pr.get('pr_number')}"
    head_before = rnd.get("head_before")
    merge_base = rnd.get("merge_base_sha") or head_before
    worktree = _worktree_path(session)

    # This round's findings: the SKILL's bramble envelopes, plus — every
    # round — the harvested pr-polish review folded in as a free data point.
    findings: list[dict] = []
    if include_harvested:
        findings.extend(_findings_from_harvested_round(rnd))
    for backend, env_path in envelopes:
        findings.extend(_findings_from_envelope(env_path, backend))

    # Carry forward prior rounds' findings (re-read each prior prompt;
    # re-normalize the path in case an old prompt has an absolute path).
    cumulative = load_cumulative(session)
    prior: list[dict] = []
    for rd in sorted(session.glob("r*/judge-prompt.json")):
        try:
            for f in json.loads(rd.read_text()).get(
                "reviewer_findings"
            ) or []:
                prior.append(
                    {**f, "file": cl.normalize_finding_path(f.get("file"))}
                )
        except (OSError, json.JSONDecodeError):
            continue
    union = _dedupe_findings(prior + findings)

    contested = [
        {
            "file": e.file, "line": e.line,
            "reviewer_severity": e.reviewer_severity,
            "topic": e.topic, "verdict_history": e.verdict_history,
        }
        for e in cumulative.contested if not e.resolved
    ]

    round_dir = session / f"r{round_n}"
    round_dir.mkdir(parents=True, exist_ok=True)
    payload = {
        "instructions": _JUDGE_INSTRUCTIONS,
        "repo_pr": repo_pr,
        # repo_path is the session worktree, pinned at head_before — the
        # judge reads working-tree files here and they match the diff.
        "repo_path": str(worktree),
        "round": round_n,
        "diff_ref": {
            "head_before": head_before,
            "head_after": rnd.get("head_after"),
            "merge_base_sha": rnd.get("merge_base_sha"),
            "base_branch": rnd.get("base_branch"),
            "files_changed": rnd.get("files_changed") or [],
            "diff_command": (
                f"git -C {worktree} diff {merge_base}..{head_before}"
            ),
        },
        "reviewer_findings": union,
        "cumulative_census": cumulative.census,
        "contested_findings": contested,
        "harvested_comment_actions": rnd.get("raw_comment_actions") or [],
        "verdict_output_path": str(round_dir / "judge-verdict.json"),
    }
    return hl.atomic_write_json(round_dir / "judge-prompt.json", payload)


# ---------------------------------------------------------------------------
# Sub-command: fold — merge a round's judge verdict
# ---------------------------------------------------------------------------


def fold_judge_round(
    *, round_n: int, session: Path, round_budget: int
) -> dict:
    """Fold round ``round_n``'s judge verdict into the accumulator.

    Returns a status dict: ``census_converged`` and a ``should_continue``
    hint (more rounds remain in the budget AND the census has not yet
    saturated).
    """
    verdict_path = session / f"r{round_n}" / "judge-verdict.json"
    if not verdict_path.is_file():
        raise SystemExit(
            f"error: no judge verdict at {verdict_path} — the round {round_n} "
            f"judge sub-agent must write it before folding"
        )
    try:
        verdict = json.loads(verdict_path.read_text())
    except json.JSONDecodeError as e:
        raise SystemExit(f"error: verdict JSON unreadable: {e}")
    err = cl.validate_judge_verdict(verdict)
    if err:
        raise SystemExit(f"error: malformed judge verdict: {err}")

    cumulative = load_cumulative(session)
    cl.merge_judge_round(cumulative, round_n, verdict)
    save_cumulative(session, cumulative)

    converged = cl.census_converged(cumulative)
    uncovered = cl.census_uncovered(cumulative)
    unresolved = cl.unresolved_contested(cumulative)
    return {
        "round": round_n,
        "census_converged": converged,
        "census_size": len(cumulative.census),
        "uncovered_census_items": len(uncovered),
        "true_positives": len(cumulative.true_positives),
        "false_positives": len(cumulative.false_positives),
        "contested": len(cumulative.contested),
        # Convergence is blocked while any contested defect is unresolved —
        # the SKILL loop must keep going until the judge re-rules them.
        "unresolved_contested": len(unresolved),
        "should_continue": (not converged) and round_n < round_budget,
    }


# ---------------------------------------------------------------------------
# Sub-command: freeze the ground truth
# ---------------------------------------------------------------------------


def freeze_ground_truth(*, dataset_dir: Path, session: Path) -> dict:
    """Assemble the ``ground_truth_v3`` block and write it into the dataset.

    The target ``<repo>-<pr>`` is read from the session meta — no need to
    re-pass it.
    """
    meta = load_session_meta(session)
    target = meta.get("target")
    if not target:
        raise SystemExit(
            f"error: session {session} has no recorded target — run "
            "`collect.py setup` first"
        )
    dataset_path, dataset = _load_dataset(dataset_dir, target)
    cumulative = load_cumulative(session)
    if cumulative.rounds_run == 0:
        raise SystemExit(
            "error: no rounds folded yet — run `collect.py fold` first"
        )
    gt = cl.build_ground_truth(
        cumulative,
        collector_git_sha=hl.harvester_git_sha(REPO_ROOT),
        harvested_rounds=dataset.get("harvested_rounds") or [],
    )
    cl.freeze(dataset_path, gt)

    # The harvest-time index.json predates this GT block — patch the PR's
    # entry so a consumer sees `ground_truth_collected` / `census_converged`
    # in the index without opening the per-PR file.
    repo_pr = dataset_path.stem
    cl.refresh_index_entry(dataset_path.parent, repo_pr)

    # Collection is done — tear down the pinned worktree.
    src = meta.get("source_repo")
    if src:
        remove_worktree(session, Path(src))

    unresolved = [e for e in gt.contested if not e.resolved]
    return {
        "dataset": str(dataset_path),
        "census_converged": gt.census_converged,
        "rounds_run": gt.rounds_run,
        "true_positives": len(gt.true_positives),
        "false_positives": len(gt.false_positives),
        "contested": len(gt.contested),
        "unresolved_contested": len(unresolved),
        "comment_action_agreement_rate": gt.dataset_xref.get(
            "comment_action_agreement_rate"
        ),
    }


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def _parse_envelope_flags(raw: list[str]) -> list[tuple[str, Path]]:
    out: list[tuple[str, Path]] = []
    for item in raw:
        if "=" not in item:
            raise SystemExit(
                f"error: --envelope expects BACKEND=PATH, got {item!r}"
            )
        backend, path = item.split("=", 1)
        out.append((backend.strip(), Path(path).expanduser()))
    return out


def _new_session(session_root: Path, target: str) -> Path:
    return session_root / f"{target}-{hl.run_id_stamp()}"


# ---------------------------------------------------------------------------
# Sub-command: validate
# ---------------------------------------------------------------------------


def _validate_one(
    dataset_path: Path, *, round_budget: int
) -> tuple[int, list[str]]:
    """Validate one per-PR dataset file. Returns ``(worst_exit, lines)``.

    ``worst_exit`` is 2 (malformed) / 1 (quality warnings) / 0 (clean).
    """
    label = dataset_path.stem
    try:
        dataset = json.loads(dataset_path.read_text())
    except (OSError, json.JSONDecodeError) as e:
        return 2, [f"{label}: ERROR unreadable dataset file: {e}"]
    errors, warnings = cl.validate_dataset(
        dataset, round_budget=round_budget
    )
    lines: list[str] = []
    if errors:
        lines.append(f"{label}: MALFORMED")
        lines += [f"  error: {m}" for m in errors]
        lines += [f"  warn:  {m}" for m in warnings]
        return 2, lines
    if warnings:
        lines.append(f"{label}: OK with warnings")
        lines += [f"  warn:  {m}" for m in warnings]
        return 1, lines
    lines.append(f"{label}: OK")
    return 0, lines


def validate_command(
    *, target: Optional[str], dataset_dir: Path, validate_all: bool,
    round_budget: int,
) -> int:
    """Run `validate` (one PR) or `validate --all`. Returns the exit code."""
    if validate_all:
        index_path = dataset_dir / "index.json"
        if not index_path.is_file():
            print(f"error: no index.json under {dataset_dir}", file=sys.stderr)
            return 2
        index = json.loads(index_path.read_text())
        files = [
            dataset_dir / e["file"]
            for e in index.get("prs") or []
            if e.get("file")
        ]
        if not files:
            print("error: index.json lists no PRs", file=sys.stderr)
            return 2
    else:
        if not target:
            print("error: `validate` needs a <repo>-<pr> target (or --all)",
                  file=sys.stderr)
            return 2
        path = (
            Path(target) if target.endswith(".json")
            else dataset_dir / f"{target}.json"
        )
        files = [path]

    worst = 0
    tally = {0: 0, 1: 0, 2: 0}
    for f in sorted(files):
        code, lines = _validate_one(f, round_budget=round_budget)
        worst = max(worst, code)
        tally[code] += 1
        for ln in lines:
            print(ln)
    if validate_all:
        print(
            f"\n{tally[0]} clean, {tally[1]} with warnings, "
            f"{tally[2]} malformed"
        )
    return worst


# ---------------------------------------------------------------------------
# Sub-command: scan — the default, list collectable work
# ---------------------------------------------------------------------------


def select_targets(
    *, dataset_dir: Path, source_dir: Path, only: list[str],
    sample: Optional[int],
) -> tuple[list[str], list[dict]]:
    """Resolve which PRs collection should process.

    Returns ``(targets, all_collectable)``. With ``--only`` the targets are
    exactly those ids; otherwise the *uncollected* collectable PRs, capped to
    a random ``sample`` if given.
    """
    collectable = discover_collectable(
        dataset_dir=dataset_dir, source_dir=source_dir
    )
    if only:
        targets = list(only)
    else:
        targets = [c["target"] for c in collectable if not c["collected"]]
        if sample is not None and sample < len(targets):
            targets = sorted(random.sample(targets, sample))
    return targets, collectable


# ---------------------------------------------------------------------------
# CLI dispatch
# ---------------------------------------------------------------------------


def main(argv: Optional[list[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--dataset-dir", type=Path, default=DEFAULT_DATASET_DIR)
    p.add_argument("--verbose", "-v", action="store_true")
    sub = p.add_subparsers(dest="cmd")

    # scan (also the no-subcommand default)
    sp_scan = sub.add_parser(
        "scan", help="List collectable PRs and what `collect` would process.")
    for sp in (p, sp_scan):
        sp.add_argument(
            "--source-dir", type=Path, default=DEFAULT_SOURCE_DIR,
            help="pr-polish run history (default ~/.bramble/projects).")

    # setup <repo>-<pr>
    sp_setup = sub.add_parser(
        "setup", help="Harvest if needed + create the session worktree.")
    sp_setup.add_argument("target")
    sp_setup.add_argument(
        "--session-root", type=Path, default=DEFAULT_SESSION_ROOT)
    sp_setup.add_argument(
        "--repo-root", action="append", default=[], metavar="NAME=PATH",
        dest="repo_root",
        help="Override repo-root auto-discovery for one repo. Rarely needed.")

    # build-prompt --session ... --round N --envelope ...
    sp_bp = sub.add_parser(
        "build-prompt", help="Write a round's judge prompt-input JSON.")
    sp_bp.add_argument("--session", type=Path, required=True)
    sp_bp.add_argument("--round", type=int, required=True)
    sp_bp.add_argument(
        "--envelope", action="append", default=[], metavar="BACKEND=PATH",
        help="A bramble code-review envelope from this round. Repeatable.")
    sp_bp.add_argument(
        "--include-harvested", action="store_true",
        help="Fold the harvested pr-polish review into this round's "
        "findings (the round loop passes this every round).")

    # fold --session ... --round N
    sp_fold = sub.add_parser(
        "fold", help="Merge a round's judge verdict; report convergence.")
    sp_fold.add_argument("--session", type=Path, required=True)
    sp_fold.add_argument("--round", type=int, required=True)
    sp_fold.add_argument("--round-budget", type=int, default=10)

    # freeze --session ...
    sp_fr = sub.add_parser(
        "freeze", help="Write the ground_truth_v3 block; tear down worktree.")
    sp_fr.add_argument("--session", type=Path, required=True)

    # validate [<repo>-<pr>] [--all]
    sp_val = sub.add_parser(
        "validate", help="Structural + quality check of a frozen dataset.")
    sp_val.add_argument("target", nargs="?")
    sp_val.add_argument("--all", action="store_true", dest="validate_all")
    sp_val.add_argument("--round-budget", type=int, default=10)

    args = p.parse_args(argv)
    cmd = args.cmd or "scan"

    if cmd == "scan":
        targets, collectable = select_targets(
            dataset_dir=args.dataset_dir, source_dir=args.source_dir,
            only=[], sample=None)
        n_collected = sum(1 for c in collectable if c["collected"])
        print(json.dumps({
            "collectable": len(collectable),
            "already_collected": n_collected,
            "uncollected": len(targets),
            "uncollected_targets": targets,
        }, indent=2))
        return 0

    if cmd == "setup":
        try:
            repo_map = hl.RepoMap.discover(args.repo_root)
        except ValueError as e:
            print(f"error: {e}", file=sys.stderr)
            return 2
        result = setup(
            target=args.target, dataset_dir=args.dataset_dir,
            session_root=args.session_root, repo_map=repo_map)
        print(json.dumps(result, indent=2))
        return 0

    if cmd == "build-prompt":
        prompt = build_prompt(
            session=args.session, round_n=args.round,
            envelopes=_parse_envelope_flags(args.envelope),
            include_harvested=args.include_harvested,
            dataset_dir=args.dataset_dir)
        print(json.dumps({"judge_prompt": str(prompt)}, indent=2))
        return 0

    if cmd == "fold":
        status = fold_judge_round(
            round_n=args.round, session=args.session,
            round_budget=args.round_budget)
        print(json.dumps(status, indent=2))
        return 0

    if cmd == "freeze":
        status = freeze_ground_truth(
            dataset_dir=args.dataset_dir, session=args.session)
        print(json.dumps(status, indent=2))
        return 0

    if cmd == "validate":
        return validate_command(
            target=args.target, dataset_dir=args.dataset_dir,
            validate_all=args.validate_all, round_budget=args.round_budget)

    print(f"error: unknown command {cmd!r}", file=sys.stderr)
    return 2



if __name__ == "__main__":
    raise SystemExit(main())
