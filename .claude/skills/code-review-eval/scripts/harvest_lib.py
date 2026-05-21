"""Library helpers for the bramble code-review eval-dataset harvester.

The harvester walks ``~/.bramble/projects/<repo>-<pr>/`` directories
left behind by the ``/pr-polish`` skill and emits one structured JSON
record per PR, suitable for replaying ``bramble code-review`` against
the same commits and the same ``--goal`` text.

The hard parts live here:
  * matching envelope findings back to ``comment_actions`` entries so
    each finding gets a ground-truth label;
  * reconstructing the per-round ``--goal`` text via the pr-polish
    skill's own ``bramble_ops.goal_for_round`` (the goal body is not
    persisted anywhere on disk — it must be regenerated from state);
  * computing the ``git merge-base origin/main <head_before>`` so a
    replay script knows the exact diff scope the reviewer saw.
"""

from __future__ import annotations

import importlib.util
import json
import os
import re
import subprocess
from contextlib import contextmanager
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any, Iterable, Literal, Optional

SCHEMA_VERSION = 1

Action = Literal[
    "fixed", "false_positive", "wont_fix", "ack", "stale", "flake", "pre_existing"
]
SignalTier = Literal["r1", "final", "final_incomplete", "r1_only"]
MatchStrategy = Literal[
    "exact", "topic_path_line", "topic_path", "topic_only", "none"
]
EnvelopeStatus = Literal["ok", "error", "missing"]

# Backends pr-polish runs. ``lint`` writes its findings through the same
# envelope schema even though it isn't a bramble model backend, so we
# treat it as a first-class review_run source here.
BACKENDS = ("codex", "cursor", "gemini", "lint")

# Sources in comment_actions that are NOT bramble findings; they're PR
# comments / CI failures recorded for the audit trail. We don't try to
# match envelope findings against them, but we keep them in
# ``raw_comment_actions`` so the dataset is self-describing.
NON_BACKEND_SOURCES = frozenset(
    {"github-inline", "github-issue", "github-review", "ci"}
)

# Sources that pr-polish uses when consensus-merging findings from
# multiple backends. Treated as a wildcard backend in Tier 1 matching.
WILDCARD_BACKEND_SOURCES = frozenset({"sweep", "consensus"})


# ---------------------------------------------------------------------------
# Dataclasses
# ---------------------------------------------------------------------------


@dataclass
class GroundTruth:
    matched_comment_action: bool
    match_strategy: MatchStrategy
    action: Optional[str]
    reason: Optional[str]
    is_real_issue: Optional[bool]
    fixed_in_commit: Optional[str]
    comment_actions_source: Optional[str]


@dataclass
class Finding:
    severity: Optional[str]
    message: str
    suggestion: Optional[str]
    file: Optional[str]
    line: Optional[int]
    confidence: Optional[float]
    invariant: Optional[str]
    sites: Optional[list[dict]]
    ground_truth: GroundTruth


@dataclass
class ReviewRun:
    backend: str
    model: Optional[str]
    session_id: Optional[str]
    review_mode: Optional[str]
    resume_status: Optional[str]
    envelope_status: EnvelopeStatus
    envelope_error: Optional[str]
    verdict: Optional[str]
    summary: Optional[str]
    duration_ms: Optional[int]
    input_tokens: Optional[int]
    output_tokens: Optional[int]
    schema_version: Optional[int]
    findings: list[Finding] = field(default_factory=list)


@dataclass
class HarvestedRound:
    round: int
    signal_tier: SignalTier
    head_before: Optional[str]
    head_after: Optional[str]
    base_branch: str
    merge_base_sha: Optional[str]
    merge_base_resolved: bool
    merge_base_error: Optional[str]
    files_changed: list[str]
    goal_text: Optional[str]
    goal_recoverable: bool
    scope_hints_present: bool
    raw_comment_actions: list[dict]
    review_runs: list[ReviewRun] = field(default_factory=list)


@dataclass
class PRRecord:
    schema_version: int
    harvested_at: str
    harvester_git_sha: str
    pr: dict
    harvested_rounds: list[HarvestedRound] = field(default_factory=list)


# ---------------------------------------------------------------------------
# Project-dir parsing
# ---------------------------------------------------------------------------


# Match e.g. "kernel-3945", "yoloswe-236", "nebula-81". Reject the doc / branch
# variants: "kernel-doc-naming-rethink-cb9650558e82",
# "yoloswe-branch-feature-meeting-bot".
_PROJECT_DIR_RE = re.compile(r"^(?P<repo>[a-z][a-z0-9]+)-(?P<pr>\d+)$")


def parse_project_dir_name(name: str) -> Optional[tuple[str, str]]:
    """Return ``(repo_name, pr_number)`` or ``None`` for non-PR dirs."""
    m = _PROJECT_DIR_RE.match(name)
    if not m:
        return None
    return m.group("repo"), m.group("pr")


def discover_project_dirs(source_dir: Path) -> list[tuple[Path, str, str]]:
    """List PR-numbered project dirs that contain pr-polish-state.json.

    Returns ``[(dir_path, repo_name, pr_number), ...]`` sorted by name.
    """
    out: list[tuple[Path, str, str]] = []
    if not source_dir.exists():
        return out
    for entry in sorted(source_dir.iterdir()):
        if not entry.is_dir():
            continue
        parsed = parse_project_dir_name(entry.name)
        if parsed is None:
            continue
        if not (entry / "pr-polish-state.json").is_file():
            continue
        repo, pr = parsed
        out.append((entry, repo, pr))
    return out


# ---------------------------------------------------------------------------
# State + envelope parsing
# ---------------------------------------------------------------------------


def parse_state_file(path: Path) -> dict:
    """Load pr-polish-state.json and assert minimal shape."""
    state = json.loads(path.read_text())
    if not isinstance(state, dict):
        raise ValueError(f"state file is not a JSON object: {path}")
    if "rounds" not in state or not isinstance(state["rounds"], list):
        raise ValueError(f"state file missing 'rounds' list: {path}")
    return state


def parse_envelope(path: Path) -> tuple[Optional[dict], Optional[str]]:
    """Return ``(envelope_dict, error_message)``.

    A missing file returns ``(None, "envelope file missing")``.
    Malformed JSON returns ``(None, "envelope JSON parse error: ...")``.
    Note: ``envelope["status"] == "error"`` is a *valid* envelope (the
    reviewer ran but failed); the caller surfaces it as
    ``envelope_status="error"``, not as a parse failure.
    """
    if not path.exists():
        return None, "envelope file missing"
    try:
        obj = json.loads(path.read_text())
    except json.JSONDecodeError as e:
        return None, f"envelope JSON parse error: {e}"
    if not isinstance(obj, dict):
        return None, "envelope is not a JSON object"
    return obj, None


# ---------------------------------------------------------------------------
# Round selection
# ---------------------------------------------------------------------------


def select_rounds_to_harvest(state: dict) -> list[tuple[int, SignalTier]]:
    """Pick which rounds carry the highest signal.

    Per the locked-in plan, we only harvest R1 and the final round:
      - R1 = fresh-eyes recall on the original diff
      - Final = precision signal on near-converged code

    Single-round PRs are emitted once with ``r1_only``.
    """
    rounds = state.get("rounds") or []
    if not rounds:
        return []
    ns = sorted({int(r.get("n") or 0) for r in rounds if r.get("n")})
    if not ns:
        return []
    completed = bool(state.get("completed"))
    if len(ns) == 1:
        return [(ns[0], "r1_only")]
    first, last = ns[0], ns[-1]
    if first == last:
        return [(first, "r1_only")]
    final_tier: SignalTier = "final" if completed else "final_incomplete"
    return [(first, "r1"), (last, final_tier)]


def get_round(state: dict, n: int) -> Optional[dict]:
    for r in state.get("rounds") or []:
        if int(r.get("n") or 0) == n:
            return r
    return None


# ---------------------------------------------------------------------------
# Path / topic normalisation
# ---------------------------------------------------------------------------


def normalize_path(p: Optional[str]) -> Optional[str]:
    """Strip leading './', collapse backslashes, lower drive letters.

    Returns ``None`` if input is None or empty after stripping.
    """
    if p is None:
        return None
    s = p.strip().replace("\\", "/")
    while s.startswith("./"):
        s = s[2:]
    return s or None


_TOKEN_RE = re.compile(r"[a-z0-9]+")


def _tokens(text: str) -> set[str]:
    return {t for t in _TOKEN_RE.findall(text.lower()) if len(t) > 3}


def topic_token_overlap(topic: str, message: str) -> float:
    """Jaccard overlap of >3-char lowercased tokens."""
    if not topic or not message:
        return 0.0
    a, b = _tokens(topic), _tokens(message)
    if not a or not b:
        return 0.0
    return len(a & b) / len(a | b)


def _topic_substring(topic: str, message: str, *, limit: int = 100) -> bool:
    if not topic or not message:
        return False
    t = topic.lower().strip()
    m = message.lower()[:limit]
    return t in m


# ---------------------------------------------------------------------------
# Finding ↔ comment_action matching
# ---------------------------------------------------------------------------

# Tier priorities and action-fix preference for tie-breaking.
_TIER_RANK = {
    "exact": 5,
    "topic_path_line": 4,
    "topic_path": 3,
    "topic_only": 2,
    "none": 0,
}
_ACTION_RANK = {
    "fixed": 3,
    "wont_fix": 2,
    "false_positive": 1,
    "stale": 0,
    "ack": -1,
    "flake": -2,
    "pre_existing": -3,
}


def _candidate_actions(round_data: dict) -> list[dict]:
    """Comment_actions eligible for envelope-finding matching.

    Drops github-* and ci sources — they're audit-trail entries, not
    reviewer findings. Kept ordering as in the state file so ties
    break toward earliest.
    """
    actions = round_data.get("comment_actions") or []
    return [a for a in actions if a.get("source") not in NON_BACKEND_SOURCES]


def match_finding_to_action(
    finding: dict,
    backend: str,
    candidate_actions: list[dict],
) -> tuple[Optional[dict], MatchStrategy]:
    """Best-match strategy for an envelope finding against this round's actions.

    The 5 tiers (highest to lowest precision):
      1. ``exact``           — same path + line + severity + (source==backend
                                or source in wildcard set).
      2. ``topic_path_line`` — same normalized path, line within ±3,
                                topic substring of message[:100].
      3. ``topic_path``      — same normalized path, topic substring of message.
      4. ``topic_only``      — topic-token-overlap > 0.5 (no path requirement).
      5. ``none``            — no match.

    Ties broken by: (tier, action-rank, earliest in list).
    """
    f_path = normalize_path(finding.get("file"))
    f_line = finding.get("line")
    f_sev = finding.get("severity")
    f_msg = finding.get("message") or ""

    best: Optional[tuple[int, int, int, dict, MatchStrategy]] = None

    for idx, a in enumerate(candidate_actions):
        a_path = normalize_path(a.get("path"))
        a_line = a.get("line")
        a_sev = a.get("severity")
        a_src = a.get("source")
        a_topic = a.get("topic") or ""

        strategy: MatchStrategy = "none"

        # Tier 1 — exact
        backend_ok = a_src == backend or a_src in WILDCARD_BACKEND_SOURCES
        if (
            backend_ok
            and a_path
            and f_path
            and a_path == f_path
            and a_line is not None
            and f_line is not None
            and int(a_line) == int(f_line)
            and a_sev
            and f_sev
            and a_sev == f_sev
        ):
            strategy = "exact"
        elif (
            a_path
            and f_path
            and a_path == f_path
            and a_line is not None
            and f_line is not None
            and abs(int(a_line) - int(f_line)) <= 3
            and _topic_substring(a_topic, f_msg, limit=100)
        ):
            strategy = "topic_path_line"
        elif (
            a_path
            and f_path
            and a_path == f_path
            and _topic_substring(a_topic, f_msg)
        ):
            strategy = "topic_path"
        elif topic_token_overlap(a_topic, f_msg) > 0.5:
            strategy = "topic_only"

        if strategy == "none":
            continue

        tier_rank = _TIER_RANK[strategy]
        action_rank = _ACTION_RANK.get(a.get("action") or "", -10)
        # Negate idx so earlier wins on ties (higher tuple is better).
        key = (tier_rank, action_rank, -idx)
        cur = (tier_rank, action_rank, -idx, a, strategy)
        if best is None or key > best[:3]:
            best = cur

    if best is None:
        return None, "none"
    _, _, _, action, strategy = best
    return action, strategy


def derive_is_real_issue(action: Optional[str]) -> Optional[bool]:
    """Coarse true/false/unknown signal derived from raw action verbatim."""
    if action in {"fixed", "wont_fix"}:
        return True
    if action in {"false_positive", "stale"}:
        return False
    return None  # ack, flake, pre_existing, None → insufficient signal


# ---------------------------------------------------------------------------
# Git helpers
# ---------------------------------------------------------------------------


@dataclass
class RepoMap:
    """Maps repo name (kernel/yoloswe/nebula) → local checkout path."""

    mapping: dict[str, Path] = field(default_factory=dict)

    @classmethod
    def from_flags(cls, flags: Iterable[str]) -> "RepoMap":
        mapping: dict[str, Path] = {}
        for f in flags:
            if "=" not in f:
                raise ValueError(
                    f"--repos-root expects NAME=PATH, got: {f!r}"
                )
            name, path_s = f.split("=", 1)
            mapping[name.strip()] = Path(path_s.strip()).expanduser()
        return cls(mapping=mapping)

    def lookup(self, repo_name: str) -> Optional[Path]:
        return self.mapping.get(repo_name)


def _git(repo_path: Path, *args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["git", "-C", str(repo_path), *args],
        capture_output=True,
        text=True,
        check=False,
    )


def normalize_remote_url(url: str) -> str:
    """Normalize SSH and HTTPS git remotes to canonical https://host/org/repo."""
    s = url.strip()
    # Strip trailing .git
    if s.endswith(".git"):
        s = s[: -len(".git")]
    # Strip auth user
    if s.startswith("git@"):
        # git@github.com:org/repo
        rest = s[len("git@") :]
        if ":" in rest:
            host, path = rest.split(":", 1)
            return f"https://{host}/{path}"
    if s.startswith("ssh://git@"):
        rest = s[len("ssh://git@") :]
        return f"https://{rest}"
    if s.startswith("https://"):
        return s
    if s.startswith("http://"):
        return "https://" + s[len("http://") :]
    return s


def get_repo_url(repo_path: Optional[Path]) -> Optional[str]:
    """Return the normalized origin URL of the local repo, or None."""
    if repo_path is None or not repo_path.exists():
        return None
    res = _git(repo_path, "config", "--get", "remote.origin.url")
    if res.returncode != 0:
        return None
    url = res.stdout.strip()
    if not url:
        return None
    return normalize_remote_url(url)


def compute_merge_base(
    repo_path: Optional[Path],
    head_sha: str,
    base_branch: str = "origin/main",
) -> tuple[Optional[str], bool, Optional[str]]:
    """Return (merge_base_sha, resolved, error_message)."""
    if repo_path is None:
        return None, False, "no repo mapping"
    if not repo_path.exists():
        return None, False, f"repo path does not exist: {repo_path}"
    # Verify head_sha is present
    res = _git(repo_path, "rev-parse", "--verify", f"{head_sha}^{{commit}}")
    if res.returncode != 0:
        return None, False, "head commit not in local repo"
    res = _git(repo_path, "merge-base", base_branch, head_sha)
    if res.returncode != 0:
        return None, False, (res.stderr.strip() or "merge-base failed")
    sha = res.stdout.strip()
    return (sha or None), bool(sha), None if sha else "merge-base returned empty"


def compute_files_changed(
    repo_path: Optional[Path],
    base_sha: str,
    head_sha: str,
) -> tuple[list[str], Optional[str]]:
    """List of repo-relative paths changed between two commits."""
    if repo_path is None or not repo_path.exists():
        return [], "no repo mapping"
    res = _git(repo_path, "diff", "--name-only", f"{base_sha}..{head_sha}")
    if res.returncode != 0:
        return [], (res.stderr.strip() or "git diff failed")
    return [ln.strip() for ln in res.stdout.splitlines() if ln.strip()], None


def harvester_git_sha(repo_path: Path) -> str:
    """SHA of the harvester repo at run time (yoloswe). Best-effort."""
    res = _git(repo_path, "rev-parse", "HEAD")
    if res.returncode != 0:
        return ""
    return res.stdout.strip()


# ---------------------------------------------------------------------------
# Goal-text reconstruction
# ---------------------------------------------------------------------------


def _load_bramble_ops(bramble_ops_path: Path):
    """Dynamic import of pr-polish's bramble_ops module.

    pr-polish isn't a package, so we import it by file path. Side effect:
    it inserts its own directory onto ``sys.path`` (to resolve ``_common``).
    """
    spec = importlib.util.spec_from_file_location(
        "_bramble_ops_for_harvester", bramble_ops_path
    )
    if spec is None or spec.loader is None:
        raise ImportError(f"cannot load spec for {bramble_ops_path}")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@contextmanager
def _chdir(path: Path):
    prev = Path.cwd()
    os.chdir(path)
    try:
        yield
    finally:
        os.chdir(prev)


def reconstruct_goal_text(
    state: dict,
    round_n: int,
    head_before: Optional[str],
    pr_summary: Optional[str],
    *,
    bramble_ops_path: Path,
    repo_path: Optional[Path],
) -> tuple[Optional[str], bool]:
    """Return (goal_text, goal_recoverable).

    R1 returns ``pr_summary`` verbatim — unrecoverable if pr_summary is None.
    R2+ calls ``bramble_ops.goal_for_round`` which is deterministic given
    state. ``repo_path`` is the cwd for the git subprocess calls
    ``goal_for_round`` makes internally; if missing, we still try (with the
    process's existing cwd) and treat any failure as unrecoverable.
    """
    if round_n < 2:
        if pr_summary:
            return pr_summary, True
        return None, False
    try:
        mod = _load_bramble_ops(bramble_ops_path)
    except Exception:
        return None, False
    try:
        if repo_path is not None and repo_path.exists():
            with _chdir(repo_path):
                text = mod.goal_for_round(
                    round_n,
                    pr_summary or "",
                    state,
                    head_before=head_before,
                )
        else:
            text = mod.goal_for_round(
                round_n,
                pr_summary or "",
                state,
                head_before=head_before,
            )
    except Exception:
        return None, False
    if not text:
        return None, False
    return text, True


# ---------------------------------------------------------------------------
# Per-round assembly
# ---------------------------------------------------------------------------


def _envelope_path(state_dir: Path, round_n: int, backend: str) -> Path:
    return state_dir / f"r{round_n}" / f"{backend}-envelope.json"


def _scope_hints_present(state_dir: Path, round_n: int) -> bool:
    # pr-polish writes scope-hints.json into the round dir when scope
    # exploration produced hints. Absence = single-package PR.
    return (state_dir / f"r{round_n}" / "scope-hints.json").exists()


def _build_finding(raw: dict, backend: str, candidates: list[dict]) -> Finding:
    action, strategy = match_finding_to_action(raw, backend, candidates)
    if action is None:
        gt = GroundTruth(
            matched_comment_action=False,
            match_strategy="none",
            action=None,
            reason=None,
            is_real_issue=None,
            fixed_in_commit=None,
            comment_actions_source=None,
        )
    else:
        gt = GroundTruth(
            matched_comment_action=True,
            match_strategy=strategy,
            action=action.get("action"),
            reason=action.get("reason"),
            is_real_issue=derive_is_real_issue(action.get("action")),
            fixed_in_commit=action.get("commit_sha"),
            comment_actions_source=action.get("source"),
        )
    return Finding(
        severity=raw.get("severity"),
        message=raw.get("message") or "",
        suggestion=raw.get("suggestion"),
        file=raw.get("file"),
        line=raw.get("line"),
        confidence=raw.get("confidence"),
        invariant=raw.get("invariant"),
        sites=raw.get("sites"),
        ground_truth=gt,
    )


def _build_review_run(
    state_dir: Path,
    round_n: int,
    backend: str,
    candidates: list[dict],
) -> Optional[ReviewRun]:
    """Returns None when the envelope is absent (skip silently)."""
    env_path = _envelope_path(state_dir, round_n, backend)
    env, err = parse_envelope(env_path)
    if env is None:
        # Older PRs don't have gemini envelopes; we treat missing envelopes
        # as "this backend didn't run for this round" and drop them rather
        # than littering the dataset with empty placeholders.
        return None

    env_status: EnvelopeStatus
    raw_status = env.get("status")
    if raw_status == "ok":
        env_status = "ok"
    elif raw_status == "error":
        env_status = "error"
    else:
        env_status = "ok" if env.get("review") else "error"

    review = env.get("review") or {}
    raw_findings = review.get("issues") or []

    findings: list[Finding] = []
    if env_status == "ok":
        for raw in raw_findings:
            if not isinstance(raw, dict):
                continue
            findings.append(_build_finding(raw, backend, candidates))

    return ReviewRun(
        backend=backend,
        model=env.get("model"),
        session_id=env.get("session_id"),
        review_mode=env.get("review_mode"),
        resume_status=env.get("resume_status") or None,
        envelope_status=env_status,
        envelope_error=env.get("error") if env_status == "error" else None,
        verdict=review.get("verdict"),
        summary=review.get("summary"),
        duration_ms=env.get("duration_ms"),
        input_tokens=env.get("input_tokens"),
        output_tokens=env.get("output_tokens"),
        schema_version=env.get("schema_version"),
        findings=findings,
    )


def build_harvested_round(
    state: dict,
    state_dir: Path,
    round_n: int,
    signal_tier: SignalTier,
    *,
    repo_path: Optional[Path],
    pr_summary: Optional[str],
    bramble_ops_path: Path,
    base_branch: str = "origin/main",
) -> HarvestedRound:
    round_data = get_round(state, round_n) or {}
    head_before = round_data.get("head_before")
    head_after = round_data.get("head_after")

    mb_sha, mb_resolved, mb_err = (None, False, "no head_before")
    files_changed: list[str] = []
    if head_before:
        mb_sha, mb_resolved, mb_err = compute_merge_base(
            repo_path, head_before, base_branch
        )
        if mb_resolved and mb_sha:
            files, _ = compute_files_changed(repo_path, mb_sha, head_before)
            files_changed = files

    goal_text, goal_recoverable = reconstruct_goal_text(
        state,
        round_n,
        head_before,
        pr_summary,
        bramble_ops_path=bramble_ops_path,
        repo_path=repo_path,
    )

    candidates = _candidate_actions(round_data)

    review_runs: list[ReviewRun] = []
    for backend in BACKENDS:
        run_ = _build_review_run(state_dir, round_n, backend, candidates)
        if run_ is not None:
            review_runs.append(run_)

    return HarvestedRound(
        round=round_n,
        signal_tier=signal_tier,
        head_before=head_before,
        head_after=head_after,
        base_branch=base_branch,
        merge_base_sha=mb_sha,
        merge_base_resolved=mb_resolved,
        merge_base_error=mb_err,
        files_changed=files_changed,
        goal_text=goal_text,
        goal_recoverable=goal_recoverable,
        scope_hints_present=_scope_hints_present(state_dir, round_n),
        raw_comment_actions=list(round_data.get("comment_actions") or []),
        review_runs=review_runs,
    )


# ---------------------------------------------------------------------------
# Top-level builder
# ---------------------------------------------------------------------------


def build_pr_record(
    state_dir: Path,
    repo_name: str,
    pr_number: str,
    *,
    repo_map: RepoMap,
    pr_summary: Optional[str],
    harvester_sha: str,
    harvested_at: str,
    bramble_ops_path: Path,
    include_incomplete: bool = True,
) -> Optional[PRRecord]:
    """Build a per-PR record. Returns None if the PR should be skipped."""
    state_path = state_dir / "pr-polish-state.json"
    state = parse_state_file(state_path)

    completed = bool(state.get("completed"))
    if not completed and not include_incomplete:
        return None

    rounds_to_harvest = select_rounds_to_harvest(state)
    if not rounds_to_harvest:
        return None

    repo_path = repo_map.lookup(repo_name)
    repo_url = get_repo_url(repo_path)
    pr_url = f"{repo_url}/pull/{pr_number}" if repo_url else None

    harvested_rounds = [
        build_harvested_round(
            state,
            state_dir,
            n,
            tier,
            repo_path=repo_path,
            pr_summary=pr_summary,
            bramble_ops_path=bramble_ops_path,
        )
        for n, tier in rounds_to_harvest
    ]

    return PRRecord(
        schema_version=SCHEMA_VERSION,
        harvested_at=harvested_at,
        harvester_git_sha=harvester_sha,
        pr={
            "repo_name": repo_name,
            "repo_url": repo_url,
            "pr_number": pr_number,
            "pr_url": pr_url,
            "branch": state.get("branch"),
            "started_at": state.get("started_at"),
            "completed": completed,
            "exit_reason": state.get("exit_reason"),
            "total_rounds": len(state.get("rounds") or []),
        },
        harvested_rounds=harvested_rounds,
    )


# ---------------------------------------------------------------------------
# Output writing
# ---------------------------------------------------------------------------


def record_to_dict(record: PRRecord) -> dict:
    return asdict(record)


def write_pr_record(out_dir: Path, record: PRRecord) -> Path:
    """Atomic write of <repo>-<pr>.json. Returns the final path."""
    out_dir.mkdir(parents=True, exist_ok=True)
    name = f"{record.pr['repo_name']}-{record.pr['pr_number']}.json"
    final = out_dir / name
    tmp = final.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(record_to_dict(record), indent=2) + "\n")
    tmp.replace(final)
    return final


def build_index(
    records: list[PRRecord], *, generated_at: str, harvester_sha: str
) -> dict:
    return {
        "schema_version": SCHEMA_VERSION,
        "generated_at": generated_at,
        "harvester_git_sha": harvester_sha,
        "prs": [
            {
                "repo_name": r.pr["repo_name"],
                "repo_url": r.pr["repo_url"],
                "pr_number": r.pr["pr_number"],
                "pr_url": r.pr["pr_url"],
                "file": f"{r.pr['repo_name']}-{r.pr['pr_number']}.json",
                "completed": r.pr["completed"],
                "total_rounds": r.pr["total_rounds"],
                "harvested_rounds": len(r.harvested_rounds),
            }
            for r in records
        ],
    }


def write_index(out_dir: Path, index: dict) -> Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    final = out_dir / "index.json"
    tmp = final.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(index, indent=2) + "\n")
    tmp.replace(final)
    return final
