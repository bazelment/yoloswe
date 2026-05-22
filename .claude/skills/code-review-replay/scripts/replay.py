#!/usr/bin/env python3
"""Replay mode — score a reviewer-under-test against a frozen ground truth.

This is the *cheap* half of the ``/code-review-replay`` skill. Collection
mode (``collect.py``) does the expensive work once: it runs many review +
judge rounds over a PR's diff and freezes a ``ground_truth_v3`` block — the
complete set of judged true / false positives — into the dataset JSON.

Replay mode then evaluates any reviewer config in a single mechanical pass:

  1. Checks out the recorded ``head_before`` in a temporary git worktree.
  2. Builds the ``--goal`` text *independently* (R1: live PR title/body +
     diffstat; R2+: deterministic pr-polish reconstruction). The dataset's
     recorded goal is kept only as a cross-check (``goal_divergence``).
  3. Runs ``bramble code-review`` once per configured backend.
  4. Scores each run's findings **mechanically** against the frozen
     ``ground_truth_v3`` — matched true positive / matched false positive /
     unmatched — and computes precision / recall / F1. No judge sub-agents,
     so a replay is fast and repeatable.

Requires the dataset JSON to carry a ``ground_truth_v3`` block. If it does
not, run collection mode first (``/code-review-replay collect <repo>-<pr>``).

The dataset lives outside the repo (``~/.bramble/code-review-eval/``) — it
holds private PR data and must never be committed.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import shutil
import subprocess
import sys
import tempfile
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Optional

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import collect_lib as cl  # noqa: E402
import harvest_lib as hl  # noqa: E402
import replay_lib as rl  # noqa: E402

# Dataset + scored results live OUTSIDE the repo — they are derived from
# real private PRs and must never be committed. See harvest.py.
EVAL_ROOT = Path.home() / ".bramble" / "code-review-eval"
DEFAULT_DATASET_DIR = EVAL_ROOT / "dataset"
DEFAULT_OUT_DIR = EVAL_ROOT / "replays"
DEFAULT_BRAMBLE_BIN = "bramble"
CODE_REVIEW_LOG_DIR = Path.home() / ".bramble" / "logs" / "code-review"
BRAMBLE_OPS_PATH = (
    SCRIPT_DIR.parents[1] / "pr-polish" / "scripts" / "bramble_ops.py"
)


# ---------------------------------------------------------------------------
# Backend configs
# ---------------------------------------------------------------------------


@dataclass
class BackendConfig:
    name: str  # display name, e.g. "codex-5.4-mini"
    backend: str  # bramble --backend value
    model: str
    extra_args: list[str] = field(default_factory=list)


# Mirrors the configs in the existing code-review-eval SKILL.md.
CONFIGS: dict[str, BackendConfig] = {
    "codex-5.4-mini": BackendConfig(
        name="codex-5.4-mini",
        backend="codex",
        model="gpt-5.4-mini",
        extra_args=["--effort", "medium"],
    ),
    "cursor-composer2": BackendConfig(
        name="cursor-composer2",
        backend="cursor",
        model="composer-2",
    ),
    "gemini-3.1-flash-lite-preview": BackendConfig(
        name="gemini-3.1-flash-lite-preview",
        backend="gemini",
        model="gemini-3.1-flash-lite-preview",
    ),
}


# ---------------------------------------------------------------------------
# Bramble invocation
# ---------------------------------------------------------------------------


def run_bramble_code_review(
    *,
    bramble_bin: str,
    cfg: BackendConfig,
    goal: str,
    cwd: Path,
    envelope_file: Path,
    protocol_log_dir: Path,
    log_dir: Path,
    run_tag: str,
    timeout_seconds: int = 900,
    verbose: bool = False,
) -> tuple[int, str, float]:
    """Run bramble code-review once.

    Returns ``(exit_code, stderr_tail, started_at_epoch)``. ``started_at`` is
    used afterwards to locate the klogfmt run log by mtime + tag.
    """
    args = [
        bramble_bin,
        "code-review",
        "--backend",
        cfg.backend,
        "--model",
        cfg.model,
        "--skip-test-execution",
        "--verbose",
        "--timeout",
        f"{timeout_seconds}s",
        "--goal",
        goal,
        "--envelope-file",
        str(envelope_file),
        "--protocol-log-dir",
        str(protocol_log_dir),
        *cfg.extra_args,
    ]
    env = os.environ.copy()
    env["BRAMBLE_RUN_TAG"] = run_tag
    env["WORK_DIR"] = str(cwd)

    log_dir.mkdir(parents=True, exist_ok=True)
    protocol_log_dir.mkdir(parents=True, exist_ok=True)
    stderr_path = log_dir / f"{cfg.name}-stderr.txt"
    stdout_path = log_dir / f"{cfg.name}-stdout.txt"

    if verbose:
        print(
            f"  $ {bramble_bin} code-review --backend {cfg.backend} "
            f"--model {cfg.model} [...] (cwd={cwd}, tag={run_tag})",
            file=sys.stderr,
        )
    started_at = time.time()
    with open(stderr_path, "wb") as ferr, open(stdout_path, "wb") as fout:
        try:
            proc = subprocess.run(
                args,
                cwd=str(cwd),
                env=env,
                stdout=fout,
                stderr=ferr,
                check=False,
                timeout=timeout_seconds + 60,  # CLI's own --timeout is the inner clock
            )
            rc = proc.returncode
        except subprocess.TimeoutExpired:
            rc = -1

    tail = ""
    try:
        tail = stderr_path.read_text(errors="replace")[-2000:]
    except OSError:
        pass
    return rc, tail, started_at


def parse_envelope_file(path: Path) -> Optional[dict]:
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return None


def _protocol_log_for_run(
    round_log: Path, started_at: float
) -> Optional[Path]:
    """The codex protocol JSONL written during this run, if any.

    ``--protocol-log-dir`` is the round dir, shared by both configs in the
    round. Caller must already have established this is a codex run — a
    cursor/gemini run writes no protocol JSONL, and the codex sibling's file
    falls inside its mtime window, so calling this for a non-codex run would
    mis-attribute the sibling's trace. Pick the newest file modified at/after
    this run's start.
    """
    best: Optional[tuple[float, Path]] = None
    for p in round_log.glob("reviewer-session-*.jsonl"):
        try:
            mt = p.stat().st_mtime
        except OSError:
            continue
        if mt + 1.0 < started_at:  # 1s slack for clock skew
            continue
        if best is None or mt > best[0]:
            best = (mt, p)
    return best[1] if best else None


def collect_execution_trace(
    *,
    run_tag: str,
    started_at: float,
    round_log: Path,
    config_name: str,
    backend: str,
    files_changed: list[str],
) -> rl.ExecutionTrace:
    """Find + parse this run's execution log into a structured trace.

    The codex backend logs tool calls only to its protocol JSONL (klogfmt
    stays near-empty), so codex runs are parsed from the protocol JSONL and
    cursor/gemini from the klogfmt run log. The source is chosen by
    ``backend`` — NOT by which log has more rows — because the round dir is
    shared and a cursor run would otherwise pick up the codex sibling's
    protocol file. Logs are copied into the round dir so the artifact is
    self-contained.
    """
    # --- klogfmt run log (all backends; authoritative for cursor/gemini) ---
    runlog = rl.find_runlog_by_tag(
        CODE_REVIEW_LOG_DIR, run_tag, after_mtime=started_at
    )
    text = ""
    runlog_copy: Optional[Path] = None
    if runlog is not None:
        try:
            text = runlog.read_text(errors="replace")
            runlog_copy = round_log / f"{config_name}-runlog.log"
            runlog_copy.write_text(text)
        except OSError:
            runlog_copy = None
    klog_trace = rl.parse_runlog(text)

    # --- codex protocol JSONL (codex runs only) ----------------------------
    protocol_copy: Optional[Path] = None
    codex_trace: Optional[rl.ExecutionTrace] = None
    if backend == "codex":
        protocol_path = _protocol_log_for_run(round_log, started_at)
        if protocol_path is not None:
            try:
                ptext = protocol_path.read_text(errors="replace")
                protocol_copy = round_log / f"{config_name}-protocol.jsonl"
                protocol_copy.write_text(ptext)
                codex_trace = rl.parse_codex_protocol(ptext)
            except OSError:
                codex_trace = None

    # codex -> protocol trace (klogfmt is near-empty); else -> klogfmt.
    trace = codex_trace if codex_trace is not None else klog_trace
    trace.runlog_path = str(runlog_copy) if runlog_copy else None
    trace.protocol_log_path = str(protocol_copy) if protocol_copy else None

    rl.annotate_files_coverage(trace, files_changed)
    return trace


# ---------------------------------------------------------------------------
# Worktree management
# ---------------------------------------------------------------------------


class TempWorktree:
    """A scratch git worktree at a specific commit, auto-removed on exit."""

    def __init__(self, repo_path: Path, sha: str, label: str):
        self.repo_path = repo_path
        self.sha = sha
        self.path = Path(tempfile.mkdtemp(prefix=f"replay-{label}-"))
        # Pre-empt the mkdtemp's empty dir — git worktree add requires the
        # target dir not to exist.
        self.path.rmdir()

    def __enter__(self) -> Path:
        res = hl.git(
            self.repo_path, "worktree", "add", "--detach",
            str(self.path), self.sha,
        )
        if res.returncode != 0:
            raise RuntimeError(
                f"git worktree add failed: {res.stderr.strip() or '(no stderr)'}"
            )
        return self.path

    def __exit__(self, *exc):
        # Best-effort cleanup: --force in case bramble left dirty files.
        hl.git(self.repo_path, "worktree", "remove", "--force", str(self.path))
        if self.path.exists():
            shutil.rmtree(self.path, ignore_errors=True)


def select_dataset_rounds(
    dataset: dict, tier_filter: Optional[str]
) -> list[dict]:
    rounds = dataset.get("harvested_rounds") or []
    if tier_filter:
        rounds = [
            r
            for r in rounds
            if r.get("signal_tier") == tier_filter
            or (
                tier_filter == "r1"
                and r.get("signal_tier") in {"r1", "r1_only"}
            )
            or (
                tier_filter == "final"
                and r.get("signal_tier") in {"final", "final_incomplete"}
            )
        ]
    return rounds


def _load_pr_polish_state(repo_pr: str) -> Optional[dict]:
    """Load ~/.bramble/projects/<repo>-<pr>/pr-polish-state.json if present.

    Needed for R2+ goal reconstruction. R1 doesn't need it (PR body suffices).
    """
    path = (
        Path.home()
        / ".bramble"
        / "projects"
        / repo_pr
        / "pr-polish-state.json"
    )
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return None


# Replay mode — run the reviewer-under-test, score mechanically vs frozen GT
# ---------------------------------------------------------------------------
#
# Replay mode is the cheap path: it spawns NO judge sub-agents. It runs
# `bramble code-review` once per (round, config) and scores each run's
# findings mechanically against the `ground_truth_v3` block collection mode
# already froze into the dataset JSON. This is what makes evaluating a new
# reviewer config a fast scoring pass instead of a multi-agent session.


@dataclass
class ReplayResult:
    schema_version: int
    phase: str  # "replay-scored"
    generated_at: str
    pr: dict
    dataset_file: str
    bramble_bin: str
    ground_truth_frozen_at: str
    ground_truth_census_converged: bool
    rounds: list[dict] = field(default_factory=list)


def run_replay(
    dataset_path: Path,
    *,
    repos_root: hl.RepoMap,
    configs: list[BackendConfig],
    tier_filter: Optional[str],
    bramble_bin: str,
    goal_source: str,
    timeout_seconds: int,
    log_root: Path,
    verbose: bool,
    strict: bool = False,
) -> tuple[ReplayResult, Path]:
    """Run the reviewer-under-test and score it against the frozen GT.

    Requires the dataset JSON to carry a ``ground_truth_v3`` block (built by
    collection mode). Each (round, config) run is scored with
    ``replay_lib.score_against_frozen_gt`` — purely mechanical, no sub-agents.

    The frozen GT is run through ``collect_lib.validate_dataset`` before any
    scoring: structural ``errors`` always abort (the metrics would be
    meaningless), and quality ``warnings`` (unconverged census, unresolved
    contested rows, low harvest agreement) are printed to stderr — under
    ``strict`` they abort too, so a benchmark never silently reports
    precision/recall against a weak ground truth.
    """
    dataset = json.loads(dataset_path.read_text())
    gt = cl.load_ground_truth(dataset)
    if gt is None:
        raise RuntimeError(
            f"{dataset_path.name} has no '{cl.GROUND_TRUTH_KEY}' block — run "
            f"collection mode first: /code-review-replay collect "
            f"{dataset_path.stem}"
        )

    errors, warnings = cl.validate_dataset(dataset)
    if errors:
        joined = "; ".join(errors)
        raise RuntimeError(
            f"{dataset_path.name} frozen ground truth is malformed — "
            f"scoring would be meaningless: {joined}"
        )
    if warnings:
        for w in warnings:
            print(f"  warn: {dataset_path.name}: {w}", file=sys.stderr)
        if strict:
            raise RuntimeError(
                f"{dataset_path.name} frozen ground truth has "
                f"{len(warnings)} quality warning(s) and --strict is set"
            )

    pr = dataset.get("pr") or {}
    repo_name = pr.get("repo_name") or ""
    pr_number = pr.get("pr_number") or ""
    repo_pr = f"{repo_name}-{pr_number}"
    repo_path = repos_root.lookup(repo_name)
    if repo_path is None or not repo_path.exists():
        raise RuntimeError(
            f"no local checkout discovered for repo {repo_name!r} "
            f"(need ~/worktrees/{repo_name}/main or ~/g/{repo_name})"
        )

    rounds_to_replay = select_dataset_rounds(dataset, tier_filter)
    if not rounds_to_replay:
        raise RuntimeError(
            f"no rounds match --tier {tier_filter!r} in {dataset_path.name}"
        )

    # Round 1's recorded goal_text is the PR summary; pr-polish's
    # goal_for_round falls back to it for a pristine R2+ round, so thread it
    # into build_goal so R2+ reconstruction matches the frozen goal.
    pr_summary = next(
        (
            r.get("goal_text")
            for r in (dataset.get("harvested_rounds") or [])
            if int(r.get("round") or 0) == 1 and r.get("goal_text")
        ),
        None,
    )

    state = _load_pr_polish_state(repo_pr)
    log_root = log_root / f"{repo_pr}-{hl.run_id_stamp()}"
    rounds_out: list[dict] = []

    for dr in rounds_to_replay:
        head_before = dr.get("head_before")
        if not head_before:
            print(
                f"  round {dr.get('round')}: no head_before, skipping",
                file=sys.stderr,
            )
            continue

        goal = rl.build_goal(
            dr,
            repo_path=repo_path,
            pr_number=str(pr_number),
            state=state,
            bramble_ops_path=BRAMBLE_OPS_PATH,
            prefer=goal_source,
            pr_summary=pr_summary,
        )
        round_n = dr.get("round")
        signal_tier = dr.get("signal_tier")
        round_label = f"{repo_pr}-r{round_n}"
        round_log = log_root / f"r{round_n}"
        files_changed = list(dr.get("files_changed") or [])

        if verbose:
            print(
                f"-> round {round_n} ({signal_tier}) "
                f"head_before={head_before[:10]} goal_source={goal.source}",
                file=sys.stderr,
            )

        run_dicts: list[dict] = []
        with TempWorktree(repo_path, head_before, round_label) as wt:
            for cfg in configs:
                envelope_path = round_log / f"{cfg.name}-envelope.json"
                round_log.mkdir(parents=True, exist_ok=True)
                run_tag = (
                    f"code-review-replay:{repo_pr}:r{round_n}:{cfg.name}"
                )
                if verbose:
                    print(f"   config {cfg.name}...", file=sys.stderr)
                rc, stderr_tail, started_at = run_bramble_code_review(
                    bramble_bin=bramble_bin,
                    cfg=cfg,
                    goal=goal.text,
                    cwd=wt,
                    envelope_file=envelope_path,
                    protocol_log_dir=round_log,
                    log_dir=round_log,
                    run_tag=run_tag,
                    timeout_seconds=timeout_seconds,
                    verbose=verbose,
                )
                env = parse_envelope_file(envelope_path)
                if env is None:
                    scored = rl.ScoredRunV3(
                        backend=cfg.backend,
                        model=cfg.model,
                        config=cfg.name,
                        envelope_status="missing",
                        verdict=None,
                        duration_ms=None,
                        n_findings_replay=0,
                        matched_tp=0,
                        matched_fp=0,
                        unmatched=0,
                        gt_true_positives=len(gt.get("true_positives") or []),
                        missed_tp=len(gt.get("true_positives") or []),
                        severity_mismatches=0,
                        precision=None,
                        recall=None,
                        f1=None,
                        score_error=(
                            f"no envelope (rc={rc}); stderr tail: "
                            f"{stderr_tail[-400:]}"
                        ),
                    )
                    run_dicts.append(rl.scored_run_v3_to_dict(scored))
                    continue

                review = env.get("review") or {}
                replay_findings = [
                    f
                    for f in (review.get("issues") or [])
                    if isinstance(f, dict)
                ]
                env_status = env.get("status") or (
                    "ok" if review else "error"
                )
                scored = rl.score_against_frozen_gt(
                    backend=env.get("backend") or cfg.backend,
                    model=env.get("model") or cfg.model,
                    config=cfg.name,
                    envelope_status=env_status,
                    verdict=review.get("verdict"),
                    duration_ms=env.get("duration_ms"),
                    replay_findings=replay_findings,
                    ground_truth=gt,
                )
                run_dicts.append(rl.scored_run_v3_to_dict(scored))

        rounds_out.append(
            {
                "round": round_n,
                "signal_tier": signal_tier,
                "head_before": head_before,
                "merge_base_sha": dr.get("merge_base_sha"),
                "files_changed": len(files_changed),
                "goal_source": goal.source,
                "goal_divergence": goal.goal_divergence,
                "runs": run_dicts,
            }
        )

    result = ReplayResult(
        schema_version=rl.REPLAY_SCHEMA_VERSION,
        phase="replay-scored",
        generated_at=hl.iso_utc_now(),
        pr=pr,
        dataset_file=dataset_path.name,
        bramble_bin=bramble_bin,
        ground_truth_frozen_at=gt.get("frozen_at") or "",
        ground_truth_census_converged=bool(gt.get("census_converged")),
        rounds=rounds_out,
    )
    log_root.mkdir(parents=True, exist_ok=True)
    return result, log_root


# ---------------------------------------------------------------------------
# Output writing + rendering
# ---------------------------------------------------------------------------


def write_json(out_dir: Path, name: str, obj: dict) -> Path:
    return hl.atomic_write_json(out_dir / name, obj)



def render_replay_markdown(result: "ReplayResult") -> str:
    """Pretty-print the replay-mode scored result (mechanical, no judges)."""
    out: list[str] = []
    pr = result.pr
    out.append(
        f"# Replay scored — {pr.get('repo_name')}-{pr.get('pr_number')}"
    )
    out.append("")
    out.append(f"- Generated: {result.generated_at}")
    out.append(
        f"- Ground truth: frozen {result.ground_truth_frozen_at or '?'}"
        + (
            ""
            if result.ground_truth_census_converged
            else "  ⚠ census did NOT converge — recall denominator may be"
            " incomplete"
        )
    )
    out.append(
        "- Metrics are computed mechanically against the frozen "
        "`ground_truth_v3` set — no judge sub-agents."
    )
    out.append(
        "- `Sev✗` = matched true positives the reviewer reported at the "
        "wrong severity (a separate signal — it does not move P/R/F1)."
    )
    out.append("")

    def _pct(x: Optional[float]) -> str:
        return "—" if x is None else f"{x:.2f}"

    for rd in result.rounds:
        out.append(
            f"## Round {rd['round']} ({rd['signal_tier']}) — "
            f"head_before={(rd['head_before'] or '')[:10]}, "
            f"{rd['files_changed']} files changed"
        )
        if rd.get("goal_divergence"):
            out.append("- ⚠ Reconstructed goal diverged from the dataset goal.")
        out.append("")
        out.append(
            "| Config | Status | Findings | TP/FP/unmatched | Missed | "
            "Sev✗ | Precision | Recall | F1 | Time |"
        )
        out.append(
            "|--------|--------|----------|-----------------|--------|"
            "------|-----------|--------|----|------|"
        )
        for r in rd["runs"]:
            t_s = (
                "—"
                if r.get("duration_ms") is None
                else f"{r['duration_ms'] / 1000:.0f}s"
            )
            sev = r.get("severity_mismatches")
            sev_s = "—" if sev is None else str(sev)
            out.append(
                f"| {r['config']} | {r['envelope_status']} | "
                f"{r['n_findings_replay']} | "
                f"{r['matched_tp']}/{r['matched_fp']}/{r['unmatched']} | "
                f"{r['missed_tp']} | {sev_s} | {_pct(r.get('precision'))} | "
                f"{_pct(r.get('recall'))} | {_pct(r.get('f1'))} | {t_s} |"
            )
        out.append("")
        for r in rd["runs"]:
            if r.get("score_error"):
                out.append(f"### {r['config']} — ⚠ {r['score_error']}")
                out.append("")
                continue
            missed = r.get("missed_true_positives") or []
            if not missed:
                continue
            out.append(f"### {r['config']} — missed real issues")
            out.append("")
            for m in missed:
                loc = f"{m.get('file', '?')}:{m.get('line', '?')}"
                out.append(
                    f"- [{m.get('severity', '?')}] {loc} — "
                    f"{m.get('topic', '')}"
                )
            out.append("")
    return "\n".join(out)


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def diagnose_missing_dataset(dataset_path: Path, target: str) -> str:
    """Build an actionable message when the dataset file is absent."""
    lines = [f"error: dataset file not found: {dataset_path}", ""]

    parsed = (
        hl.parse_project_dir_name(target)
        if not target.endswith(".json")
        else None
    )
    if parsed is None and not target.endswith(".json"):
        lines.append(
            f"  '{target}' is not a <repo>-<pr> id (expected e.g. kernel-3945)."
        )
        return "\n".join(lines)

    projects_dir = Path.home() / ".bramble" / "projects"
    source_dir = projects_dir / target if parsed else None

    if source_dir is not None and (source_dir / "pr-polish-state.json").is_file():
        lines.append(
            f"  The pr-polish source for {target} still exists at {source_dir}."
        )
        lines.append(
            "  Run collection mode to build the dataset + frozen ground "
            "truth, then re-run this replay:"
        )
        lines.append("")
        lines.append(f"    /code-review-replay collect {target}")
        lines.append("")
        lines.append("  (or harvest the raw dataset directly:)")
        lines.append("")
        lines.append(
            "    python3 .claude/skills/code-review-replay/scripts/harvest.py"
            f" --only {target} --verbose"
        )
        return "\n".join(lines)

    if not projects_dir.exists():
        lines.append(
            f"  No pr-polish data at all under {projects_dir} — nothing to harvest."
        )
    else:
        lines.append(
            f"  No pr-polish source dir for {target} under {projects_dir}."
        )
        available = sorted(
            d.name
            for d in projects_dir.iterdir()
            if d.is_dir()
            and hl.parse_project_dir_name(d.name)
            and (d / "pr-polish-state.json").is_file()
        )
        if available:
            lines.append("  PRs that CAN be harvested right now:")
            for name in available[:25]:
                lines.append(f"    {name}")
            if len(available) > 25:
                lines.append(f"    ... and {len(available) - 25} more")
    lines.append("")
    lines.append(
        "  The dataset is built from `/pr-polish` run history; without that "
        "source data the PR cannot be replayed."
    )
    return "\n".join(lines)


def select_replay_targets(
    *, dataset_dir: Path, sample: int
) -> list[str]:
    """Randomly sample ``sample`` PRs that have a frozen ground truth.

    The pool is ``index.json``'s ``ground_truth_collected`` flag — no
    per-file scan. Used when ``replay.py`` is given no positional target.
    """
    index_path = dataset_dir / "index.json"
    if not index_path.is_file():
        raise SystemExit(f"error: no index.json under {dataset_dir}")
    index = json.loads(index_path.read_text())
    pool = [
        e["file"][:-5]  # strip ".json"
        for e in index.get("prs") or []
        if e.get("ground_truth_collected") and e.get("file", "").endswith(
            ".json"
        )
    ]
    if not pool:
        raise SystemExit(
            "error: no PR in the dataset has a frozen ground truth — run "
            "`/code-review-replay collect` first"
        )
    return sorted(random.sample(pool, min(sample, len(pool))))


def main(argv: Optional[list[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument(
        "target",
        nargs="?",
        help="Dataset target (e.g. kernel-3834) or path to a JSON file. "
        "Omit to randomly sample --sample PRs from the dataset.",
    )
    p.add_argument(
        "--sample",
        type=int,
        default=5,
        help="When no target is given, how many GT-collected PRs to "
        "randomly score (default 5).",
    )
    p.add_argument("--dataset-dir", type=Path, default=DEFAULT_DATASET_DIR)
    p.add_argument("--out-dir", type=Path, default=DEFAULT_OUT_DIR)
    p.add_argument(
        "--repo-root",
        action="append",
        default=[],
        metavar="NAME=PATH",
        dest="repo_root",
        help="Override repo-root auto-discovery for one repo. Rarely needed.",
    )
    p.add_argument(
        "--config",
        action="append",
        default=[],
        choices=sorted(CONFIGS.keys()),
        help="Backend config to run. Repeatable. Default: codex-5.4-mini + cursor-composer2.",
    )
    p.add_argument(
        "--tier",
        choices=["r1", "final"],
        help="Filter rounds by signal_tier. Default: both.",
    )
    p.add_argument(
        "--bramble-bin",
        default=DEFAULT_BRAMBLE_BIN,
        help="Path to the bramble binary.",
    )
    p.add_argument(
        "--goal-source",
        choices=["auto", "dataset"],
        default="auto",
        help=(
            "auto (default): build the --goal independently (R1 from the "
            "live PR, R2+ reconstructed); dataset: use the dataset's "
            "recorded goal verbatim (pre-redesign behaviour, for debugging)."
        ),
    )
    p.add_argument(
        "--timeout-seconds",
        type=int,
        default=900,
        help="Per-bramble-call timeout (default 900s = 15m).",
    )
    p.add_argument(
        "--log-root",
        type=Path,
        default=Path(tempfile.gettempdir()) / "code-review-replay",
    )
    p.add_argument("--verbose", "-v", action="store_true")
    p.add_argument(
        "--strict",
        action="store_true",
        help="Treat frozen-GT quality warnings (unconverged census, "
        "unresolved contested rows, low harvest agreement) as fatal. "
        "Structural errors always abort regardless of this flag.",
    )
    p.add_argument(
        "--print-markdown",
        action="store_true",
        help="Print a Markdown summary to stdout.",
    )
    args = p.parse_args(argv)

    # ---- resolve which PR(s) to score ------------------------------------
    if args.target:
        targets = [args.target]
    else:
        try:
            targets = select_replay_targets(
                dataset_dir=args.dataset_dir, sample=args.sample
            )
        except SystemExit as e:
            print(e, file=sys.stderr)
            return 2
        print(
            f"no target given — sampling {len(targets)} GT-collected PR(s): "
            f"{', '.join(targets)}",
            file=sys.stderr,
        )

    try:
        repo_map = hl.RepoMap.discover(args.repo_root)
    except ValueError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2

    if not args.config:
        configs = [CONFIGS["codex-5.4-mini"], CONFIGS["cursor-composer2"]]
    else:
        configs = [CONFIGS[name] for name in args.config]

    # ---- score each target ----------------------------------------------
    worst = 0
    results = []
    for target in targets:
        dataset_path = (
            Path(target) if target.endswith(".json")
            else args.dataset_dir / f"{target}.json"
        )
        if not dataset_path.exists():
            print(
                diagnose_missing_dataset(dataset_path, target),
                file=sys.stderr,
            )
            worst = 2
            continue
        try:
            result, _ = run_replay(
                dataset_path,
                repos_root=repo_map,
                configs=configs,
                tier_filter=args.tier,
                bramble_bin=args.bramble_bin,
                goal_source=args.goal_source,
                timeout_seconds=args.timeout_seconds,
                log_root=args.log_root,
                verbose=args.verbose,
                strict=args.strict,
            )
        except RuntimeError as e:
            print(f"error scoring {target}: {e}", file=sys.stderr)
            worst = 2
            continue
        repo_pr = f"{result.pr.get('repo_name')}-{result.pr.get('pr_number')}"
        stamp = hl.run_id_stamp()
        path = write_json(
            args.out_dir, f"{repo_pr}-{stamp}-scored.json", asdict(result)
        )
        print(f"wrote {path}", file=sys.stderr)
        results.append(result)

    if args.print_markdown:
        for result in results:
            print(render_replay_markdown(result))
            print()
    return worst


if __name__ == "__main__":
    raise SystemExit(main())
