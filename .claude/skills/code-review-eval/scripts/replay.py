#!/usr/bin/env python3
"""Replay a harvested PR through `bramble code-review`, judged independently.

Given a per-PR JSON in ``~/.bramble/code-review-eval/dataset/<repo>-<pr>.json``
(the dataset lives outside the repo — it holds private PR data), this driver
runs in two phases:

  PHASE A — mechanical (this script, default):
    1. Checks out the recorded ``head_before`` in a temporary git worktree.
    2. Builds the ``--goal`` text *independently* (R1: live PR title/body +
       diffstat; R2+: deterministic pr-polish reconstruction). The dataset's
       recorded goal is kept only as a cross-check (``goal_divergence``).
    3. Runs ``bramble code-review`` for each configured backend, capturing
       the result envelope AND the reviewer's klogfmt execution trace
       (which files it read, where it spent time, what it skipped).
    4. Emits a NEUTRAL comparison artifact — no true/false-positive verdicts,
       no precision/recall. The dataset findings are included as a labelled
       *reference*; a token-overlap ``mechanical_match`` is included only as
       a hint. Plus one prompt-input JSON per (round, config) run.

  PHASE B — judgment (``--fold-verdicts DIR``):
    A judge sub-agent (driven by the SKILL) reads each prompt-input, inspects
    the real diff, and writes a verdict JSON per run. This phase folds those
    verdicts into the final scored result: precision / recall / F1 computed
    from the *judge's* verdicts, plus ``dataset_agreement_rate`` — a quality
    signal on the dataset itself.

Why two phases: the harvested dataset is a *reference*, not ground truth. It
only labels findings the original review surfaced and an engineer acted on,
and that triage can be wrong. Trusting it blindly (the old design) measured
the dataset, not the reviewer. See ``replay_lib`` for the rationale in full.
"""

from __future__ import annotations

import argparse
import datetime as _dt
import json
import os
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
# Mechanical hint matcher (NOT a scorer)
# ---------------------------------------------------------------------------
#
# This is the old token-overlap matcher, DEMOTED. It no longer feeds any
# metric. It only produces a *hint* for the judge sub-agent ("the dataset's
# closest finding to replay finding #3 is dataset finding #7") and a
# dataset-label guess used to compute dataset_agreement_rate. The judge is
# explicitly told to ignore it unless the code supports it.

_HINT_TIER_RANK = {
    "exact": 5,
    "same_path_line": 4,
    "same_path_tokens": 3,
    "tokens_only": 2,
    "none": 0,
}


def _real_rank(df: dict) -> int:
    gt = df.get("ground_truth") or {}
    is_real = gt.get("is_real_issue")
    if is_real is True:
        return 2
    if is_real is False:
        return 1
    return 0


def match_replay_to_dataset(
    replay_finding: dict,
    dataset_findings: list[dict],
) -> tuple[Optional[dict], Optional[int], str]:
    """Best token-overlap guess of which dataset finding a replay finding
    corresponds to. Returns ``(dataset_finding_or_None, index_or_None,
    strategy)``. Used as a HINT only — never to score.
    """
    f_path = hl.normalize_path(replay_finding.get("file"))
    f_line = replay_finding.get("line")
    f_sev = replay_finding.get("severity")
    f_msg = replay_finding.get("message") or ""

    best: Optional[tuple[int, int, int, dict, int, str]] = None

    for idx, df in enumerate(dataset_findings):
        if not isinstance(df, dict):
            continue
        d_path = hl.normalize_path(df.get("file"))
        d_line = df.get("line")
        d_sev = df.get("severity")
        d_msg = df.get("message") or ""

        strategy = "none"
        if (
            d_path
            and f_path
            and d_path == f_path
            and d_line is not None
            and f_line is not None
            and int(d_line) == int(f_line)
            and d_sev
            and f_sev
            and d_sev == f_sev
        ):
            strategy = "exact"
        elif (
            d_path
            and f_path
            and d_path == f_path
            and d_line is not None
            and f_line is not None
            and abs(int(d_line) - int(f_line)) <= 5
        ):
            strategy = "same_path_line"
        else:
            d_lede = d_msg[:200]
            f_lede = f_msg[:200]
            if (
                d_path
                and f_path
                and d_path == f_path
                and hl.topic_token_overlap(d_lede, f_lede) >= 0.40
            ):
                strategy = "same_path_tokens"
            elif hl.topic_token_overlap(d_lede, f_lede) >= 0.55:
                strategy = "tokens_only"

        if strategy == "none":
            continue
        key = (_HINT_TIER_RANK[strategy], _real_rank(df), -idx)
        cur = (key[0], key[1], key[2], df, idx, strategy)
        if best is None or key > best[:3]:
            best = cur

    if best is None:
        return None, None, "none"
    return best[3], best[4], best[5]


def build_mechanical_match(
    replay_findings: list[dict],
    dataset_findings: list[dict],
) -> list[dict]:
    """One hint row per replay finding, index-aligned with replay_findings.

    Each row carries the closest dataset finding (by token overlap), the
    match strategy, and that dataset finding's ``is_real_issue`` label —
    explicitly flagged as a HINT for the judge, not a verdict.
    """
    out: list[dict] = []
    for rf in replay_findings:
        df, didx, strategy = match_replay_to_dataset(rf, dataset_findings)
        if df is None:
            out.append(
                {
                    "dataset_index": None,
                    "match_strategy": "none",
                    "dataset_is_real_issue": None,
                    "dataset_action": None,
                    "dataset_message": None,
                    "hint": (
                        "no dataset finding resembles this one — it may be a "
                        "real issue the original review missed; judge it on "
                        "the code alone"
                    ),
                }
            )
            continue
        gt = df.get("ground_truth") or {}
        out.append(
            {
                "dataset_index": didx,
                "match_strategy": strategy,
                "dataset_is_real_issue": gt.get("is_real_issue"),
                "dataset_action": gt.get("action"),
                "dataset_message": (df.get("message") or "")[:300],
                "hint": (
                    "token-overlap guess only — confirm against the diff "
                    "before trusting the dataset label"
                ),
            }
        )
    return out


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
        res = subprocess.run(
            [
                "git",
                "-C",
                str(self.repo_path),
                "worktree",
                "add",
                "--detach",
                str(self.path),
                self.sha,
            ],
            capture_output=True,
            text=True,
            check=False,
        )
        if res.returncode != 0:
            raise RuntimeError(
                f"git worktree add failed: {res.stderr.strip() or '(no stderr)'}"
            )
        return self.path

    def __exit__(self, *exc):
        # Best-effort cleanup: --force in case bramble left dirty files.
        subprocess.run(
            [
                "git",
                "-C",
                str(self.repo_path),
                "worktree",
                "remove",
                "--force",
                str(self.path),
            ],
            capture_output=True,
            check=False,
        )
        if self.path.exists():
            shutil.rmtree(self.path, ignore_errors=True)


# ---------------------------------------------------------------------------
# Phase A — neutral artifact
# ---------------------------------------------------------------------------


@dataclass
class ArtifactRun:
    """One (round, config) run in the Phase-A neutral artifact.

    Carries everything the judge sub-agent needs — and explicitly NO
    true/false-positive verdict and NO precision/recall.
    """

    backend: str
    model: str
    config: str
    envelope_status: str  # ok | error | missing
    envelope_error: Optional[str]
    verdict: Optional[str]  # bramble's accepted/rejected (not a TP/FP verdict)
    summary: Optional[str]
    duration_ms: Optional[int]
    replay_findings: list[dict]  # verbatim envelope issues
    mechanical_match: list[dict]  # token-overlap HINTS, index-aligned
    execution_trace: dict
    prompt_input_path: Optional[str]


@dataclass
class ArtifactRound:
    round: int
    signal_tier: str
    head_before: str
    head_after: Optional[str]
    merge_base_sha: Optional[str]
    base_branch: Optional[str]
    files_changed: list[str]
    goal_used: str
    goal_source: str
    dataset_goal: Optional[str]
    goal_divergence: bool
    goal_notes: list[str]
    # Dataset findings are REFERENCE ONLY — not ground truth.
    dataset_findings_reference: list[dict] = field(default_factory=list)
    runs: list[ArtifactRun] = field(default_factory=list)


@dataclass
class ReplayArtifact:
    schema_version: int
    phase: str  # "A-neutral"
    generated_at: str
    pr: dict
    dataset_file: str
    bramble_bin: str
    repo_path: str
    prompt_input_dir: str
    rounds: list[ArtifactRound] = field(default_factory=list)


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


def _write_prompt_input(
    *,
    prompt_dir: Path,
    repo_pr: str,
    repo_path: Path,
    artifact_round: ArtifactRound,
    run: ArtifactRun,
) -> Path:
    """Write the per-run JSON a judge sub-agent consumes.

    The SKILL hands this file to a sub-agent whose job is to (a) judge each
    replay finding TP/FP/unsure against the real diff, (b) census missed real
    issues, and (c) analyse the execution trace. The sub-agent writes its
    verdict to ``r{round}-{config}-verdict.json`` in the same dir.
    """
    prompt_dir.mkdir(parents=True, exist_ok=True)
    payload = {
        "instructions": (
            "You are an independent code-review judge. Inspect the REAL diff "
            "and reach your OWN verdict on each finding. The dataset labels "
            "and mechanical_match hints are REFERENCE ONLY — agree with them "
            "only when the code supports it. See the SKILL for the full "
            "prompt and the verdict JSON schema you must produce."
        ),
        "repo_pr": repo_pr,
        "repo_path": str(repo_path),
        "round": artifact_round.round,
        "signal_tier": artifact_round.signal_tier,
        "config": run.config,
        "backend": run.backend,
        "model": run.model,
        "diff_ref": {
            "head_before": artifact_round.head_before,
            "head_after": artifact_round.head_after,
            "merge_base_sha": artifact_round.merge_base_sha,
            "base_branch": artifact_round.base_branch,
            "files_changed": artifact_round.files_changed,
            "diff_command": (
                f"git -C {repo_path} diff "
                f"{artifact_round.merge_base_sha or artifact_round.head_before}.."
                f"{artifact_round.head_before}"
            ),
        },
        "goal_used": artifact_round.goal_used,
        "goal_source": artifact_round.goal_source,
        "envelope_status": run.envelope_status,
        "bramble_verdict": run.verdict,
        "replay_findings": run.replay_findings,
        "mechanical_match_hints": run.mechanical_match,
        "dataset_findings_reference": artifact_round.dataset_findings_reference,
        "execution_trace": run.execution_trace,
        "verdict_output_path": str(
            prompt_dir / f"r{artifact_round.round}-{run.config}-verdict.json"
        ),
    }
    path = prompt_dir / f"r{artifact_round.round}-{run.config}-prompt.json"
    path.write_text(json.dumps(payload, indent=2) + "\n")
    return path


def run_phase_a(
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
) -> tuple[ReplayArtifact, Path]:
    """Run bramble per (round, config), emit the neutral artifact + prompts."""
    dataset = json.loads(dataset_path.read_text())
    pr = dataset.get("pr") or {}
    repo_name = pr.get("repo_name") or ""
    pr_number = pr.get("pr_number") or ""
    repo_pr = f"{repo_name}-{pr_number}"
    repo_path = repos_root.lookup(repo_name)
    if repo_path is None or not repo_path.exists():
        raise RuntimeError(
            f"no --repos-root entry maps {repo_name!r} to a usable local path"
        )

    rounds_to_replay = select_dataset_rounds(dataset, tier_filter)
    if not rounds_to_replay:
        raise RuntimeError(
            f"no rounds match --tier {tier_filter!r} in {dataset_path.name}"
        )

    state = _load_pr_polish_state(repo_pr)

    run_id = _dt.datetime.now(_dt.timezone.utc).strftime("%Y%m%d-%H%M%S")
    log_root = log_root / f"{repo_pr}-{run_id}"
    prompt_dir = log_root / "prompts"

    artifact = ReplayArtifact(
        schema_version=rl.REPLAY_SCHEMA_VERSION,
        phase="A-neutral",
        generated_at=_dt.datetime.now(_dt.timezone.utc).strftime(
            "%Y-%m-%dT%H:%M:%SZ"
        ),
        pr=pr,
        dataset_file=dataset_path.name,
        bramble_bin=bramble_bin,
        repo_path=str(repo_path),
        prompt_input_dir=str(prompt_dir),
    )

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
        )
        if not goal.text:
            print(
                f"  round {dr.get('round')}: empty goal "
                f"(source={goal.source}); proceeding with empty --goal",
                file=sys.stderr,
            )

        ds_findings: list[dict] = []
        for rr in dr.get("review_runs") or []:
            for f in rr.get("findings") or []:
                ds_findings.append(f)

        round_n = dr.get("round")
        signal_tier = dr.get("signal_tier")
        round_label = f"{repo_pr}-r{round_n}"
        round_log = log_root / f"r{round_n}"

        a_round = ArtifactRound(
            round=round_n,
            signal_tier=signal_tier,
            head_before=head_before,
            head_after=dr.get("head_after"),
            merge_base_sha=dr.get("merge_base_sha"),
            base_branch=dr.get("base_branch"),
            files_changed=list(dr.get("files_changed") or []),
            goal_used=goal.text,
            goal_source=goal.source,
            dataset_goal=goal.dataset_goal,
            goal_divergence=goal.goal_divergence,
            goal_notes=goal.notes,
            dataset_findings_reference=ds_findings,
        )

        if verbose:
            print(
                f"-> round {round_n} ({signal_tier}) "
                f"head_before={head_before[:10]} "
                f"goal_source={goal.source} goal_len={len(goal.text)} "
                f"ds_findings(ref)={len(ds_findings)}",
                file=sys.stderr,
            )

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
                trace = collect_execution_trace(
                    run_tag=run_tag,
                    started_at=started_at,
                    round_log=round_log,
                    config_name=cfg.name,
                    backend=cfg.backend,
                    files_changed=a_round.files_changed,
                )

                env = parse_envelope_file(envelope_path)
                if env is None:
                    a_run = ArtifactRun(
                        backend=cfg.backend,
                        model=cfg.model,
                        config=cfg.name,
                        envelope_status="missing",
                        envelope_error=(
                            f"no envelope (rc={rc}); stderr tail: "
                            f"{stderr_tail[-400:]}"
                        ),
                        verdict=None,
                        summary=None,
                        duration_ms=None,
                        replay_findings=[],
                        mechanical_match=[],
                        execution_trace=asdict(trace),
                        prompt_input_path=None,
                    )
                    a_round.runs.append(a_run)
                    continue

                review = env.get("review") or {}
                replay_findings = [
                    f
                    for f in (review.get("issues") or [])
                    if isinstance(f, dict)
                ]
                env_status = env.get("status") or ("ok" if review else "error")
                mech = build_mechanical_match(replay_findings, ds_findings)

                a_run = ArtifactRun(
                    backend=env.get("backend") or cfg.backend,
                    model=env.get("model") or cfg.model,
                    config=cfg.name,
                    envelope_status=env_status,
                    envelope_error=env.get("error"),
                    verdict=review.get("verdict"),
                    summary=review.get("summary"),
                    duration_ms=env.get("duration_ms"),
                    replay_findings=replay_findings,
                    mechanical_match=mech,
                    execution_trace=asdict(trace),
                    prompt_input_path=None,
                )
                a_round.runs.append(a_run)

        # Write prompt inputs after the worktree closes — the judge uses the
        # persistent repo checkout, not the scratch worktree.
        for a_run in a_round.runs:
            ppath = _write_prompt_input(
                prompt_dir=prompt_dir,
                repo_pr=repo_pr,
                repo_path=repo_path,
                artifact_round=a_round,
                run=a_run,
            )
            a_run.prompt_input_path = str(ppath)

        artifact.rounds.append(a_round)

    artifact_path = log_root / "artifact.json"
    log_root.mkdir(parents=True, exist_ok=True)
    artifact_path.write_text(json.dumps(asdict(artifact), indent=2) + "\n")
    return artifact, artifact_path


# ---------------------------------------------------------------------------
# Phase B — fold verdicts into the scored result
# ---------------------------------------------------------------------------


@dataclass
class ScoredResult:
    schema_version: int
    phase: str  # "B-scored"
    generated_at: str
    pr: dict
    dataset_file: str
    bramble_bin: str
    artifact_file: str
    rounds: list[dict] = field(default_factory=list)


def run_phase_b(
    artifact_path: Path,
    verdict_dir: Path,
) -> ScoredResult:
    """Fold judge verdicts into the final scored result."""
    artifact = json.loads(artifact_path.read_text())
    scored_runs = rl.fold_verdicts(artifact, verdict_dir)

    # fold_verdicts walks rounds then runs in artifact order, so the flat
    # scored_runs list re-groups by slicing per round's run count.
    rounds_out: list[dict] = []
    si = 0
    for rnd in artifact.get("rounds") or []:
        n_runs = len(rnd.get("runs") or [])
        run_dicts = [
            rl.scored_run_to_dict(s) for s in scored_runs[si : si + n_runs]
        ]
        si += n_runs
        rounds_out.append(
            {
                "round": rnd.get("round"),
                "signal_tier": rnd.get("signal_tier"),
                "head_before": rnd.get("head_before"),
                "merge_base_sha": rnd.get("merge_base_sha"),
                "files_changed": len(rnd.get("files_changed") or []),
                "goal_source": rnd.get("goal_source"),
                "goal_divergence": rnd.get("goal_divergence"),
                "runs": run_dicts,
            }
        )

    return ScoredResult(
        schema_version=rl.REPLAY_SCHEMA_VERSION,
        phase="B-scored",
        generated_at=_dt.datetime.now(_dt.timezone.utc).strftime(
            "%Y-%m-%dT%H:%M:%SZ"
        ),
        pr=artifact.get("pr") or {},
        dataset_file=artifact.get("dataset_file") or "",
        bramble_bin=artifact.get("bramble_bin") or "",
        artifact_file=str(artifact_path),
        rounds=rounds_out,
    )


# ---------------------------------------------------------------------------
# Output writing + rendering
# ---------------------------------------------------------------------------


def write_json(out_dir: Path, name: str, obj: dict) -> Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    final = out_dir / name
    final.write_text(json.dumps(obj, indent=2) + "\n")
    return final


def render_artifact_markdown(artifact: ReplayArtifact, artifact_path: Path) -> str:
    """Summarise the Phase-A artifact + the next-step instructions."""
    out: list[str] = []
    pr = artifact.pr
    out.append(
        f"# Replay Phase A (neutral) — {pr.get('repo_name')}-{pr.get('pr_number')}"
    )
    out.append("")
    out.append(f"- Artifact: `{artifact_path}`")
    out.append(f"- Prompt inputs: `{artifact.prompt_input_dir}`")
    out.append("")
    for rd in artifact.rounds:
        out.append(
            f"## Round {rd.round} ({rd.signal_tier}) — "
            f"head_before={rd.head_before[:10]}, "
            f"{len(rd.files_changed)} files changed"
        )
        out.append(
            f"- Goal source: `{rd.goal_source}`"
            + (
                "  ⚠ DIVERGES from dataset goal"
                if rd.goal_divergence
                else ""
            )
        )
        out.append("")
        out.append("| Config | Status | Replay# | Verdict | Tool calls | Files read | Time |")
        out.append("|--------|--------|---------|---------|-----------|-----------|------|")
        for r in rd.runs:
            tr = r.execution_trace or {}
            n_tools = tr.get("n_tool_calls", 0)
            n_read = len(tr.get("files_read") or [])
            n_changed = n_read + len(tr.get("files_changed_not_read") or [])
            t_s = (
                "—"
                if r.duration_ms is None
                else f"{r.duration_ms / 1000:.0f}s"
            )
            out.append(
                f"| {r.config} | {r.envelope_status} | "
                f"{len(r.replay_findings)} | {r.verdict or '—'} | "
                f"{n_tools} | {n_read}/{n_changed} | {t_s} |"
            )
        out.append("")
    out.append("## Next step — Phase B (judgment)")
    out.append("")
    out.append(
        "Spawn one judge sub-agent per prompt-input JSON in the prompt dir. "
        "Each writes its verdict to the `verdict_output_path` named inside "
        "the prompt. Then fold:"
    )
    out.append("")
    out.append("```bash")
    out.append(
        "python3 .claude/skills/code-review-eval/scripts/replay.py "
        f"{pr.get('repo_name')}-{pr.get('pr_number')} \\"
    )
    out.append(f"  --fold-verdicts {artifact.prompt_input_dir} \\")
    out.append(f"  --artifact {artifact_path} --print-markdown")
    out.append("```")
    return "\n".join(out)


def render_scored_markdown(result: ScoredResult) -> str:
    """Pretty-print the Phase-B scored result."""
    out: list[str] = []
    pr = result.pr
    out.append(
        f"# Replay scored result — {pr.get('repo_name')}-{pr.get('pr_number')}"
    )
    out.append("")
    out.append(f"- Artifact: `{result.artifact_file}`")
    out.append(f"- Generated: {result.generated_at}")
    out.append(
        "- Metrics are computed from INDEPENDENT judge verdicts, not the "
        "dataset labels."
    )
    out.append("")
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
            "| Config | Status | Replay# | TP/FP/Unsure | Missed | "
            "Precision | Recall | F1 | DS-agree | Time |"
        )
        out.append(
            "|--------|--------|---------|--------------|--------|"
            "-----------|--------|----|----------|------|"
        )
        for r in rd["runs"]:

            def _pct(x: Optional[float]) -> str:
                return "—" if x is None else f"{x:.2f}"

            t_s = (
                "—"
                if r.get("duration_ms") is None
                else f"{r['duration_ms'] / 1000:.0f}s"
            )
            dsr = r.get("dataset_agreement_rate")
            dsr_s = (
                "—"
                if dsr is None
                else f"{dsr:.2f} ({r['dataset_agreements']}/{r['dataset_comparisons']})"
            )
            out.append(
                f"| {r['config']} | {r['envelope_status']} | "
                f"{r['n_findings_replay']} | "
                f"{r['judged_tp']}/{r['judged_fp']}/{r['judged_unsure']} | "
                f"{r['n_missed_real']} | "
                f"{_pct(r.get('precision'))} | {_pct(r.get('recall'))} | "
                f"{_pct(r.get('f1'))} | {dsr_s} | {t_s} |"
            )
        out.append("")
        # Execution analysis + missed issues per run.
        for r in rd["runs"]:
            if r.get("fold_error"):
                out.append(f"### {r['config']} — ⚠ {r['fold_error']}")
                out.append("")
                continue
            ea = r.get("execution_analysis") or []
            missed = r.get("missed_real_issues") or []
            if not ea and not missed:
                continue
            out.append(f"### {r['config']} — judge notes")
            out.append("")
            if missed:
                out.append("**Missed real issues:**")
                for m in missed:
                    loc = f"{m.get('file', '?')}:{m.get('line', '?')}"
                    out.append(
                        f"- [{m.get('severity', '?')}] {loc} — "
                        f"{m.get('description', '')}"
                    )
                out.append("")
            if ea:
                out.append("**Execution analysis:**")
                for e in ea:
                    out.append(
                        f"- [{e.get('severity', '?')}] "
                        f"{e.get('observation', '')} → "
                        f"{e.get('improvement', '')}"
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
        lines.append("  Harvest it into the dataset, then re-run this replay:")
        lines.append("")
        lines.append(
            "    python3 .claude/skills/code-review-eval/scripts/harvest.py \\"
        )
        lines.append(f"      --only {target} \\")
        lines.append("      --repos-root kernel=/home/ubuntu/worktrees/kernel/main \\")
        lines.append("      --repos-root yoloswe=/home/ubuntu/worktrees/yoloswe/main \\")
        lines.append("      --repos-root nebula=/home/ubuntu/worktrees/nebula/main \\")
        lines.append("      --verbose")
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


def main(argv: Optional[list[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument(
        "target",
        help="Dataset target (e.g. yoloswe-248, kernel-3834) or path to JSON file.",
    )
    p.add_argument("--dataset-dir", type=Path, default=DEFAULT_DATASET_DIR)
    p.add_argument("--out-dir", type=Path, default=DEFAULT_OUT_DIR)
    p.add_argument(
        "--repos-root",
        action="append",
        default=[],
        metavar="NAME=PATH",
        help="Map repo dir name to local checkout path. Repeatable.",
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
        "--print-markdown",
        action="store_true",
        help="Print a Markdown summary to stdout.",
    )
    p.add_argument(
        "--fold-verdicts",
        type=Path,
        metavar="VERDICT_DIR",
        help=(
            "Phase B: skip running bramble. Fold judge verdict JSONs from "
            "VERDICT_DIR (the prompt-input dir) into the final scored "
            "result. Requires --artifact."
        ),
    )
    p.add_argument(
        "--artifact",
        type=Path,
        help="Phase-A artifact JSON to fold verdicts against (with --fold-verdicts).",
    )
    args = p.parse_args(argv)

    target = args.target
    if target.endswith(".json"):
        dataset_path = Path(target)
    else:
        dataset_path = args.dataset_dir / f"{target}.json"

    # ---- Phase B: fold verdicts ------------------------------------------
    if args.fold_verdicts:
        if not args.artifact or not args.artifact.exists():
            print(
                "error: --fold-verdicts requires --artifact pointing at the "
                "Phase-A artifact.json",
                file=sys.stderr,
            )
            return 2
        if not args.fold_verdicts.exists():
            print(
                f"error: verdict dir not found: {args.fold_verdicts}",
                file=sys.stderr,
            )
            return 2
        result = run_phase_b(args.artifact, args.fold_verdicts)
        repo_pr = f"{result.pr.get('repo_name')}-{result.pr.get('pr_number')}"
        stamp = _dt.datetime.now(_dt.timezone.utc).strftime("%Y%m%d-%H%M%S")
        path = write_json(
            args.out_dir, f"{repo_pr}-{stamp}-scored.json", asdict(result)
        )
        print(f"wrote {path}", file=sys.stderr)
        if args.print_markdown:
            print(render_scored_markdown(result))
        return 0

    # ---- Phase A: run bramble + emit neutral artifact --------------------
    if not dataset_path.exists():
        print(diagnose_missing_dataset(dataset_path, target), file=sys.stderr)
        return 2

    try:
        repos_root = hl.RepoMap.from_flags(args.repos_root)
    except ValueError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2

    if not args.config:
        configs = [CONFIGS["codex-5.4-mini"], CONFIGS["cursor-composer2"]]
    else:
        configs = [CONFIGS[name] for name in args.config]

    try:
        artifact, artifact_path = run_phase_a(
            dataset_path,
            repos_root=repos_root,
            configs=configs,
            tier_filter=args.tier,
            bramble_bin=args.bramble_bin,
            goal_source=args.goal_source,
            timeout_seconds=args.timeout_seconds,
            log_root=args.log_root,
            verbose=args.verbose,
        )
    except RuntimeError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2

    print(f"wrote {artifact_path}", file=sys.stderr)
    if args.print_markdown:
        print(render_artifact_markdown(artifact, artifact_path))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
