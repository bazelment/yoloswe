"""Library helpers for the bramble code-review replay scorer.

The replay driver (``replay.py``) re-runs ``bramble code-review`` against a
harvested PR and scores how good the reviewer is. The old design *trusted*
the harvested dataset as ground truth — it joined replay findings to dataset
findings by token overlap and read the ``is_real_issue`` label straight off
the dataset.

That is wrong on three counts, and this module exists to fix all three:

  1. The dataset's ``ground_truth`` labels are a **reference**, not truth.
     They only cover findings the original review surfaced *and* an engineer
     happened to act on. A better reviewer can catch real bugs the original
     missed, and the original triage can itself be wrong. So replay now emits
     a *neutral comparison artifact* and a judge sub-agent assigns the real
     true/false-positive verdicts by reading the actual diff.
  2. The ``--goal`` text should be built independently, not lifted from the
     dataset. ``build_goal`` reconstructs it; the dataset goal is kept only
     as a cross-check (``goal_divergence``).
  3. The reviewer's *execution process* was invisible. ``parse_runlog`` turns
     the klogfmt run log ``bramble code-review`` already writes into a
     structured ``execution_trace`` so the judge can see which files the
     reviewer read, where it spent time, and what it skipped.

The hard parts live here so they can be unit-tested without running bramble:
  * ``parse_runlog`` — klogfmt run log -> execution_trace (cursor / gemini)
  * ``parse_codex_protocol`` — codex protocol JSONL -> execution_trace
    (the codex backend logs tool calls only to the protocol JSONL, so its
    klogfmt run log is near-empty)
  * ``build_goal`` — independent goal reconstruction (R1 via PR body, R2+
    via pr-polish ``goal_for_round``)
  * ``fold_verdicts`` / ``score_from_verdicts`` — turn sub-agent verdict
    JSONs into precision / recall / F1 plus a dataset-agreement signal
"""

from __future__ import annotations

import datetime as _dt
import json
import re
import subprocess
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Optional

import harvest_lib as hl

REPLAY_SCHEMA_VERSION = 2

# Verdict vocabulary the judge sub-agent must use for each replay finding.
VERDICT_TRUE_POSITIVE = "true_positive"
VERDICT_FALSE_POSITIVE = "false_positive"
VERDICT_UNSURE = "unsure"
VALID_VERDICTS = frozenset(
    {VERDICT_TRUE_POSITIVE, VERDICT_FALSE_POSITIVE, VERDICT_UNSURE}
)


# ===========================================================================
# Execution-trace parsing — klogfmt run log -> structured trace
# ===========================================================================

# `bramble code-review` writes a klogfmt log per run to
# ~/.bramble/logs/code-review/code-review-{ts}-{pid}.log. Lines look like:
#
#   I0507 01:11:19.931402 228289 codereview.go:134] code-review run start \
#       run_tag=... backend=cursor model=composer-2 ...
#   D0507 01:11:40.329 228289 backend.go:193] tool call start \
#       run_tag=... tool="read .../foo.py" call_id=tool_abc input_summary=...
#   D0507 01:11:40.427 228289 backend.go:209] tool call end \
#       run_tag=... tool=readToolCall call_id=tool_abc is_error=false ...
#   I0507 01:12:45.717 228289 backend.go:218] reviewer turn complete \
#       run_tag=... success=true duration_ms=84271
#
# The leading token is "<level><MMDD> <HH:MM:SS.micros>". We parse the
# timestamp to millis-of-day and the trailing `key=value` / `key="..."`
# attributes. Cursor logs every tool call here; codex logs far less in
# klogfmt (it emits a richer protocol JSONL via --protocol-log-dir instead).

_KLOG_LINE_RE = re.compile(
    r"^[IWED](?P<mmdd>\d{4})\s+"
    r"(?P<hh>\d{2}):(?P<mm>\d{2}):(?P<ss>\d{2})\.(?P<us>\d{6})\s+"
    r"\d+\s+\S+\]\s+(?P<body>.*)$"
)

# Splits `key=value` and `key="quoted value"` attribute pairs. The quoted
# branch is non-greedy so embedded `"` inside a redaction marker doesn't
# swallow the rest of the line.
_KLOG_ATTR_RE = re.compile(r'(\w+)=("(?:[^"\\]|\\.)*"|\S+)')


def _klog_ts_ms(mmdd: str, hh: str, mm: str, ss: str, us: str) -> int:
    """Wall-clock millis-of-day. The log never spans a date boundary in a
    single review run, so millis-of-day is a fine monotonic clock here and
    avoids needing the year."""
    return (
        (int(hh) * 3600 + int(mm) * 60 + int(ss)) * 1000 + int(us) // 1000
    )


def _klog_attrs(body: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for k, v in _KLOG_ATTR_RE.findall(body):
        if len(v) >= 2 and v[0] == '"' and v[-1] == '"':
            v = v[1:-1]
        out[k] = v
    return out


@dataclass
class ToolCall:
    """One reviewer tool invocation, joined from its start + end log lines."""

    kind: str  # read | grep | glob | shell | other
    tool: str  # raw tool string, e.g. 'read .../deploy.py'
    call_id: Optional[str]
    start_ms: Optional[int]
    end_ms: Optional[int]
    duration_ms: Optional[int]
    is_error: bool
    target: Optional[str]  # best-effort file/path the call touched


@dataclass
class ExecutionTrace:
    """Structured view of one reviewer run, parsed from its klogfmt log."""

    runlog_path: Optional[str]
    protocol_log_path: Optional[str]
    parsed: bool  # False when no usable log was found
    backend: Optional[str]
    model: Optional[str]
    session_started: bool
    total_duration_ms: Optional[int]
    first_tool_latency_ms: Optional[int]  # session start -> first tool call
    n_tool_calls: int
    tool_kind_counts: dict[str, int]
    n_tool_errors: int
    tool_calls: list[ToolCall] = field(default_factory=list)
    # files_changed[] split by whether the reviewer's read calls touched them.
    files_read: list[str] = field(default_factory=list)
    files_changed_not_read: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)


def _tool_kind(tool: str) -> str:
    """Coarse tool family from the raw tool string."""
    t = tool.lower()
    for kind in ("read", "grep", "glob", "shell"):
        if t.startswith(kind) or t.endswith(f"{kind}toolcall"):
            return kind
    return "other"


def _tool_target(tool: str) -> Optional[str]:
    """Best-effort path/pattern a tool call touched.

    klogfmt redacts long paths to `.../<tail>` so we can only recover the
    tail. Good enough to basename-match against files_changed[].
    """
    # 'read .../scripts/deploy.py' -> 'scripts/deploy.py'
    m = re.match(r"\w+\s+(.+)$", tool.strip())
    if not m:
        return None
    tail = m.group(1).strip()
    while tail.startswith(".../") or tail.startswith("./"):
        tail = tail[tail.index("/") + 1 :]
    return tail or None


def parse_runlog(text: str) -> ExecutionTrace:
    """Parse a klogfmt code-review run log into an ExecutionTrace.

    Resilient to partial logs: missing start/end lines just leave the
    corresponding fields ``None``. ``parsed`` is True whenever at least one
    structured line was recognised.
    """
    trace = ExecutionTrace(
        runlog_path=None,
        protocol_log_path=None,
        parsed=False,
        backend=None,
        model=None,
        session_started=False,
        total_duration_ms=None,
        first_tool_latency_ms=None,
        n_tool_calls=0,
        tool_kind_counts={},
        n_tool_errors=0,
    )

    session_start_ms: Optional[int] = None
    pending: dict[str, ToolCall] = {}  # call_id -> open ToolCall
    ordered: list[ToolCall] = []

    for raw in text.splitlines():
        m = _KLOG_LINE_RE.match(raw)
        if not m:
            continue
        ts = _klog_ts_ms(
            m["mmdd"], m["hh"], m["mm"], m["ss"], m["us"]
        )
        body = m["body"]
        attrs = _klog_attrs(body)

        if body.startswith("code-review run start"):
            trace.parsed = True
            trace.backend = attrs.get("backend") or trace.backend
            trace.model = attrs.get("model") or trace.model
        elif body.startswith("reviewer session started"):
            trace.parsed = True
            trace.session_started = True
            session_start_ms = ts
            trace.model = trace.model or attrs.get("model")
        elif body.startswith("tool call start"):
            trace.parsed = True
            tool = attrs.get("tool") or "?"
            call_id = attrs.get("call_id")
            tc = ToolCall(
                kind=_tool_kind(tool),
                tool=tool,
                call_id=call_id,
                start_ms=ts,
                end_ms=None,
                duration_ms=None,
                is_error=False,
                target=_tool_target(tool),
            )
            ordered.append(tc)
            if call_id:
                pending[call_id] = tc
        elif body.startswith("tool call end"):
            trace.parsed = True
            call_id = attrs.get("call_id")
            tc = pending.pop(call_id, None) if call_id else None
            if tc is not None:
                tc.end_ms = ts
                if tc.start_ms is not None:
                    tc.duration_ms = ts - tc.start_ms
                tc.is_error = attrs.get("is_error") == "true"
        elif body.startswith("reviewer turn complete") or body.startswith(
            "code-review run exit"
        ):
            trace.parsed = True
            dur = attrs.get("duration_ms") or attrs.get("total_duration_ms")
            if dur and dur.isdigit():
                trace.total_duration_ms = int(dur)

    trace.tool_calls = ordered
    trace.n_tool_calls = len(ordered)
    trace.n_tool_errors = sum(1 for t in ordered if t.is_error)
    counts: dict[str, int] = {}
    for t in ordered:
        counts[t.kind] = counts.get(t.kind, 0) + 1
    trace.tool_kind_counts = counts

    if session_start_ms is not None and ordered:
        first = min(
            (t.start_ms for t in ordered if t.start_ms is not None),
            default=None,
        )
        if first is not None:
            trace.first_tool_latency_ms = first - session_start_ms

    return trace


# ---------------------------------------------------------------------------
# Codex protocol JSONL parsing
# ---------------------------------------------------------------------------
#
# The codex backend logs almost nothing to the klogfmt run log — its tool
# calls live only in the protocol JSONL written via --protocol-log-dir.
# Parsing the klogfmt for a codex run therefore yields n_tool_calls=0 and
# an empty files_read, which badly understates what the reviewer did (every
# judge in the eval session flagged this). parse_codex_protocol reads that
# JSONL and produces the SAME ExecutionTrace shape as parse_runlog.
#
# Protocol shape (one JSON object per line):
#   {"format":"codex","version":"1.0",...}                       header
#   {"timestamp":"<iso>","direction":"sent|received","message":{  }}
# Tool calls arrive as `item/started` + `item/completed` notifications whose
# params.item.type == "commandExecution":
#   item.command           — raw '/bin/bash -lc "..."'
#   item.commandActions[]  — [{type: read|search|..., path, name}]
#   item.durationMs / exitCode / status (on item/completed)

# codex commandActions[].type -> our coarse tool kind. The codex protocol
# emits both `list` and `listFiles` for directory enumeration depending on
# the action shape; an `unknown` type (bare git/shell commands) maps to no
# kind and leaves the call as a generic shell call.
_CODEX_ACTION_KIND = {
    "read": "read",
    "search": "grep",
    "list": "glob",
    "listFiles": "glob",
}


def _iso_ms(ts: Optional[str]) -> Optional[int]:
    """Parse an RFC3339/ISO timestamp to epoch millis, or None."""
    if not ts:
        return None
    s = ts.strip()
    # Python's fromisoformat rejects 'Z' before 3.11 and >6-digit fractions.
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    s = re.sub(r"\.(\d{6})\d+", r".\1", s)
    try:
        return int(_dt.datetime.fromisoformat(s).timestamp() * 1000)
    except ValueError:
        return None


def parse_codex_protocol(text: str) -> ExecutionTrace:
    """Parse a codex reviewer-session-*.jsonl protocol log into an
    ExecutionTrace — the codex equivalent of parse_runlog.

    Resilient to partial logs and unknown notification types. ``parsed`` is
    True once the codex header or any structured item is recognised.
    """
    trace = ExecutionTrace(
        runlog_path=None,
        protocol_log_path=None,
        parsed=False,
        backend="codex",
        model=None,
        session_started=False,
        total_duration_ms=None,
        first_tool_latency_ms=None,
        n_tool_calls=0,
        tool_kind_counts={},
        n_tool_errors=0,
    )

    turn_started_ms: Optional[int] = None
    pending: dict[str, ToolCall] = {}  # codex item id -> open ToolCall
    ordered: list[ToolCall] = []

    for raw in text.splitlines():
        raw = raw.strip()
        if not raw:
            continue
        try:
            obj = json.loads(raw)
        except ValueError:
            continue
        if not isinstance(obj, dict):
            continue
        if obj.get("format") == "codex":
            trace.parsed = True
            continue
        msg = obj.get("message") or {}
        method = msg.get("method")
        params = msg.get("params") or {}
        ts_ms = _iso_ms(obj.get("timestamp"))

        if method in ("thread/started", "turn/started"):
            trace.parsed = True
            trace.session_started = True
            if method == "turn/started" and turn_started_ms is None:
                turn_started_ms = ts_ms
            continue
        if method == "turn/completed":
            trace.parsed = True
            if turn_started_ms is not None and ts_ms is not None:
                trace.total_duration_ms = ts_ms - turn_started_ms
            continue
        if method not in ("item/started", "item/completed"):
            continue

        item = params.get("item") or {}
        if item.get("type") != "commandExecution":
            continue
        trace.parsed = True
        item_id = item.get("id")

        if method == "item/started":
            actions = item.get("commandActions") or []
            # An item can carry several actions (e.g. a `sed` that codex
            # classifies both as a generic shell op and a typed `read`).
            # Prefer a `read` action so the file target is recovered for
            # files coverage; otherwise take the first recognised kind.
            kind = "shell"
            target: Optional[str] = None
            chosen = None
            for ca in actions:
                k = _CODEX_ACTION_KIND.get(ca.get("type") or "")
                if not k:
                    continue
                if k == "read":
                    chosen = (k, ca)
                    break
                if chosen is None:
                    chosen = (k, ca)
            if chosen is not None:
                kind, ca = chosen
                p = ca.get("path") or ca.get("name")
                if p:
                    target = p
            tc = ToolCall(
                kind=kind,
                tool=(item.get("command") or "?")[:200],
                call_id=item_id,
                start_ms=ts_ms,
                end_ms=None,
                duration_ms=None,
                is_error=False,
                target=target,
            )
            ordered.append(tc)
            if item_id:
                pending[item_id] = tc
        else:  # item/completed
            tc = pending.pop(item_id, None) if item_id else None
            if tc is not None:
                tc.end_ms = ts_ms
                dur = item.get("durationMs")
                if isinstance(dur, int):
                    tc.duration_ms = dur
                elif tc.start_ms is not None and ts_ms is not None:
                    tc.duration_ms = ts_ms - tc.start_ms
                exit_code = item.get("exitCode")
                tc.is_error = (
                    item.get("status") == "failed"
                    or (isinstance(exit_code, int) and exit_code != 0)
                )

    trace.tool_calls = ordered
    trace.n_tool_calls = len(ordered)
    trace.n_tool_errors = sum(1 for t in ordered if t.is_error)
    counts: dict[str, int] = {}
    for t in ordered:
        counts[t.kind] = counts.get(t.kind, 0) + 1
    trace.tool_kind_counts = counts

    if turn_started_ms is not None and ordered:
        first = min(
            (t.start_ms for t in ordered if t.start_ms is not None),
            default=None,
        )
        if first is not None:
            trace.first_tool_latency_ms = first - turn_started_ms

    return trace


def _strip_replay_cwd(path: str) -> str:
    """Drop the ephemeral replay-checkout prefix from a read target.

    Codex protocol logs record absolute paths inside the throwaway
    `/tmp/replay-<pr>-<round>-<rand>/` checkout. Strip that prefix so
    ``files_read`` shows repo-relative paths the judge can act on; paths
    outside any replay checkout (skill docs, /home/...) are left intact.
    """
    m = re.match(r"^/tmp/replay-[^/]+/(.+)$", path)
    return m.group(1) if m else path


def annotate_files_coverage(
    trace: ExecutionTrace, files_changed: list[str]
) -> None:
    """Populate ``files_read`` and ``files_changed_not_read`` on the trace.

    ``files_read`` is the full set of distinct paths the reviewer read —
    *not* a subset of ``files_changed`` (an earlier version incorrectly
    intersected the two, so a reviewer that read 30+ files showed only the
    handful that happened to be in the diff). ``files_changed_not_read`` is
    the diagnostic subset: changed files the reviewer never opened.

    Paths from the klogfmt source may be redacted to `.../<tail>`; codex
    protocol paths are absolute inside the ephemeral replay checkout. We
    normalise both and basename-match changed files against the read set.
    Mutates ``trace`` in place.
    """
    read_targets = [
        t.target for t in trace.tool_calls if t.kind == "read" and t.target
    ]

    # Full distinct read set, replay-checkout prefix stripped, order-stable.
    seen: set[str] = set()
    files_read: list[str] = []
    for rt in read_targets:
        norm = _strip_replay_cwd(rt.strip())
        if norm and norm not in seen:
            seen.add(norm)
            files_read.append(norm)
    trace.files_read = files_read

    # Coverage diagnostic: which changed files were never read.
    read_basenames = {Path(p).name.lower() for p in files_read}
    not_read: list[str] = []
    for fc in files_changed:
        base = Path(fc).name.lower()
        if base not in read_basenames:
            not_read.append(fc)
    trace.files_changed_not_read = not_read

    if not trace.parsed:
        trace.notes.append(
            "no usable execution log found for this run; "
            "execution trace unavailable"
        )
    elif trace.n_tool_calls == 0:
        trace.notes.append(
            "execution log parsed but has no tool-call records"
        )
    if not_read and trace.n_tool_calls > 0:
        trace.notes.append(
            f"{len(not_read)} of {len(files_changed)} changed files have no "
            "matching read call in the trace"
        )


# ===========================================================================
# Run-log discovery — find the klogfmt log for a tagged run
# ===========================================================================


def find_runlog_by_tag(
    log_dir: Path,
    run_tag: str,
    *,
    after_mtime: float = 0.0,
) -> Optional[Path]:
    """Locate the code-review klogfmt log written for ``run_tag``.

    ``replay.py`` sets a unique ``BRAMBLE_RUN_TAG`` per run; bramble writes
    it into the run-start line as ``run_tag=<tag>``. We scan logs modified at
    or after ``after_mtime`` (the run's start time) and return the newest one
    whose first lines contain the tag. Returns ``None`` if nothing matches.
    """
    if not log_dir.exists():
        return None
    candidates: list[tuple[float, Path]] = []
    for p in log_dir.glob("code-review-*.log"):
        try:
            st = p.stat()
        except OSError:
            continue
        if st.st_mtime + 1.0 < after_mtime:  # 1s slack for clock skew
            continue
        candidates.append((st.st_mtime, p))
    # Newest first — the tagged run is the most recent matching log.
    for _, p in sorted(candidates, reverse=True):
        try:
            head = p.read_text(errors="replace")[:4000]
        except OSError:
            continue
        if f"run_tag={run_tag}" in head:
            return p
    return None


# ===========================================================================
# Independent goal construction
# ===========================================================================


@dataclass
class GoalResult:
    """Goal text to pass to ``bramble code-review``, plus provenance."""

    text: str
    source: str  # pr_body | reconstructed | dataset_fallback | empty
    dataset_goal: Optional[str]
    goal_divergence: bool  # reconstructed goal materially differs from dataset
    notes: list[str] = field(default_factory=list)


def _gh_pr_body(repo_path: Path, pr_number: str) -> Optional[str]:
    """Fetch '<title>\\n\\n<body>' for a PR via `gh pr view`, or None.

    Run with ``cwd`` inside the repo so ``gh`` resolves the right remote.
    """
    res = subprocess.run(
        ["gh", "pr", "view", str(pr_number), "--json", "title,body"],
        cwd=str(repo_path),
        capture_output=True,
        text=True,
        check=False,
    )
    if res.returncode != 0:
        return None
    try:
        obj = json.loads(res.stdout)
    except json.JSONDecodeError:
        return None
    title = (obj.get("title") or "").strip()
    body = (obj.get("body") or "").strip()
    if not title and not body:
        return None
    return f"{title}\n\n{body}".strip()


def _diff_stat(
    repo_path: Path, base_sha: str, head_sha: str
) -> Optional[str]:
    """`git diff --stat base..head`, or None on failure."""
    res = subprocess.run(
        ["git", "-C", str(repo_path), "diff", "--stat", f"{base_sha}..{head_sha}"],
        capture_output=True,
        text=True,
        check=False,
    )
    if res.returncode != 0:
        return None
    return res.stdout.strip() or None


def _materially_diverges(a: str, b: str) -> bool:
    """True when two goal strings differ beyond whitespace + token overlap.

    Used only to flag ``goal_divergence`` for human attention — not a hard
    gate. Identical-after-normalisation goals never diverge; goals sharing
    most >3-char tokens are treated as the same intent.
    """
    na, nb = " ".join(a.split()), " ".join(b.split())
    if na == nb:
        return False
    return hl.topic_token_overlap(na, nb) < 0.6


def build_goal(
    dataset_round: dict,
    *,
    repo_path: Path,
    pr_number: str,
    state: Optional[dict],
    bramble_ops_path: Path,
    prefer: str = "auto",
) -> GoalResult:
    """Build the ``--goal`` text independently of the dataset.

    ``prefer``:
      * ``auto``    — reconstruct (R1: PR body + diffstat; R2+: goal_for_round)
                      and keep the dataset goal only as a cross-check.
      * ``dataset`` — escape hatch: use the dataset's recorded goal verbatim
                      (reproduces the pre-redesign behaviour for debugging).

    The dataset goal is always loaded into ``dataset_goal`` so the caller can
    surface ``goal_divergence``.
    """
    dataset_goal = dataset_round.get("goal_text") or None
    round_n = int(dataset_round.get("round") or 1)
    head_before = dataset_round.get("head_before")
    head_after = dataset_round.get("head_after")
    merge_base = dataset_round.get("merge_base_sha")
    notes: list[str] = []

    if prefer == "dataset":
        return GoalResult(
            text=dataset_goal or "",
            source="dataset_fallback" if dataset_goal else "empty",
            dataset_goal=dataset_goal,
            goal_divergence=False,
            notes=["--goal-source=dataset: using dataset goal verbatim"],
        )

    text: Optional[str] = None
    source = "empty"

    if round_n < 2:
        # R1: rebuild from the live PR — title/body are the freshest signal.
        body = _gh_pr_body(repo_path, pr_number)
        if body:
            text = body
            source = "pr_body"
            base_for_stat = merge_base or head_before
            if base_for_stat and head_after:
                stat = _diff_stat(repo_path, base_for_stat, head_after)
                if stat:
                    text = f"{body}\n\nDiff stat:\n{stat}"
        else:
            notes.append(
                "gh pr view failed or PR has no title/body; "
                "falling back to dataset goal for R1"
            )
    else:
        # R2+: deterministic reconstruction from pr-polish state.
        if state is not None:
            recon, ok = hl.reconstruct_goal_text(
                state,
                round_n,
                head_before,
                None,
                bramble_ops_path=bramble_ops_path,
                repo_path=repo_path,
            )
            if ok and recon:
                text = recon
                source = "reconstructed"
            else:
                notes.append(
                    "goal_for_round reconstruction failed; "
                    "falling back to dataset goal"
                )
        else:
            notes.append(
                "no pr-polish state available; "
                "falling back to dataset goal for R2+"
            )

    if text is None:
        text = dataset_goal or ""
        source = "dataset_fallback" if dataset_goal else "empty"

    divergence = bool(
        dataset_goal and source != "dataset_fallback"
        and _materially_diverges(text, dataset_goal)
    )
    if divergence:
        notes.append(
            "reconstructed goal materially diverges from the dataset's "
            "recorded goal — see goal_used vs dataset_goal"
        )

    return GoalResult(
        text=text,
        source=source,
        dataset_goal=dataset_goal,
        goal_divergence=divergence,
        notes=notes,
    )


# ===========================================================================
# Verdict folding — sub-agent judgments -> precision / recall / F1
# ===========================================================================
#
# A judge sub-agent inspects the real diff at head_before and returns one
# verdict JSON per (round, config) run. The schema it must produce:
#
#   {
#     "round": 1,
#     "config": "codex-5.4-mini",
#     "finding_verdicts": [
#       {
#         "index": 0,                       # index into replay_findings[]
#         "verdict": "true_positive",       # true_positive|false_positive|unsure
#         "reason": "off-by-one confirmed at deploy.py:679",
#         "dataset_disagreement": false     # judge verdict != dataset label
#       }, ...
#     ],
#     "missed_real_issues": [
#       {"file": "...", "line": 12, "description": "...", "severity": "high"}
#     ],
#     "execution_analysis": [
#       {"observation": "...", "improvement": "...", "severity": "medium"}
#     ]
#   }
#
# fold_verdicts() validates that shape; score_from_verdicts() turns it into
# metrics. Recall's denominator is the judge's *independent* census
# (judged_TP + missed_real_issues), NOT the dataset's count.


@dataclass
class ScoredRunV2:
    backend: str
    model: str
    config: str
    envelope_status: str
    verdict: Optional[str]
    duration_ms: Optional[int]
    n_findings_replay: int
    judged_tp: int
    judged_fp: int
    judged_unsure: int
    n_missed_real: int
    precision: Optional[float]
    recall: Optional[float]
    f1: Optional[float]
    # Quality signal on the DATASET itself: how often the judge agreed with
    # the dataset's is_real_issue label where both had an opinion.
    dataset_comparisons: int
    dataset_agreements: int
    dataset_agreement_rate: Optional[float]
    finding_verdicts: list[dict] = field(default_factory=list)
    missed_real_issues: list[dict] = field(default_factory=list)
    execution_analysis: list[dict] = field(default_factory=list)
    fold_error: Optional[str] = None


def validate_verdict(obj: object) -> Optional[str]:
    """Return an error string if ``obj`` is not a well-formed verdict JSON."""
    if not isinstance(obj, dict):
        return "verdict is not a JSON object"
    fv = obj.get("finding_verdicts")
    if not isinstance(fv, list):
        return "missing 'finding_verdicts' list"
    for i, v in enumerate(fv):
        if not isinstance(v, dict):
            return f"finding_verdicts[{i}] is not an object"
        if not isinstance(v.get("index"), int):
            return f"finding_verdicts[{i}] missing integer 'index'"
        if v.get("verdict") not in VALID_VERDICTS:
            return (
                f"finding_verdicts[{i}].verdict must be one of "
                f"{sorted(VALID_VERDICTS)}"
            )
    for key in ("missed_real_issues", "execution_analysis"):
        if key in obj and not isinstance(obj[key], list):
            return f"'{key}' must be a list when present"
    return None


def _safe_div(num: float, den: float) -> Optional[float]:
    return (num / den) if den else None


def score_from_verdicts(
    *,
    backend: str,
    model: str,
    config: str,
    envelope_status: str,
    verdict: Optional[str],
    duration_ms: Optional[int],
    replay_findings: list[dict],
    mechanical_match: list[dict],
    judge_verdict: dict,
) -> ScoredRunV2:
    """Compute metrics for one run from its judge verdict.

    ``mechanical_match`` is the hint join from the artifact: a list aligned
    with ``replay_findings`` carrying the dataset label the token-overlap
    matcher *guessed* for each finding (``dataset_is_real_issue``). We use it
    only to compute ``dataset_agreement_rate`` — never to score.
    """
    finding_verdicts = judge_verdict.get("finding_verdicts") or []
    missed = judge_verdict.get("missed_real_issues") or []
    exec_analysis = judge_verdict.get("execution_analysis") or []

    by_index: dict[int, dict] = {}
    for v in finding_verdicts:
        idx = v.get("index")
        if isinstance(idx, int):
            by_index[idx] = v

    tp = fp = unsure = 0
    ds_cmp = ds_agree = 0
    for idx, _rf in enumerate(replay_findings):
        v = by_index.get(idx)
        if v is None:
            # Judge skipped a finding — treat as unsure, never as TP/FP.
            unsure += 1
            continue
        verdict_kind = v.get("verdict")
        if verdict_kind == VERDICT_TRUE_POSITIVE:
            tp += 1
        elif verdict_kind == VERDICT_FALSE_POSITIVE:
            fp += 1
        else:
            unsure += 1

        # Dataset-agreement signal: compare judge verdict to the dataset
        # label the mechanical matcher attached to this finding.
        hint = mechanical_match[idx] if idx < len(mechanical_match) else {}
        ds_label = hint.get("dataset_is_real_issue")
        if ds_label is not None and verdict_kind in (
            VERDICT_TRUE_POSITIVE,
            VERDICT_FALSE_POSITIVE,
        ):
            ds_cmp += 1
            judged_real = verdict_kind == VERDICT_TRUE_POSITIVE
            if judged_real == bool(ds_label):
                ds_agree += 1

    n_missed = len(missed)
    precision = _safe_div(tp, tp + fp)
    recall = _safe_div(tp, tp + n_missed)
    if precision is not None and recall is not None and (precision + recall) > 0:
        f1: Optional[float] = 2 * precision * recall / (precision + recall)
    else:
        f1 = None

    return ScoredRunV2(
        backend=backend,
        model=model,
        config=config,
        envelope_status=envelope_status,
        verdict=verdict,
        duration_ms=duration_ms,
        n_findings_replay=len(replay_findings),
        judged_tp=tp,
        judged_fp=fp,
        judged_unsure=unsure,
        n_missed_real=n_missed,
        precision=precision,
        recall=recall,
        f1=f1,
        dataset_comparisons=ds_cmp,
        dataset_agreements=ds_agree,
        dataset_agreement_rate=_safe_div(ds_agree, ds_cmp),
        finding_verdicts=finding_verdicts,
        missed_real_issues=missed,
        execution_analysis=exec_analysis,
    )


def fold_verdicts(
    artifact: dict,
    verdict_dir: Path,
) -> list[ScoredRunV2]:
    """Join Phase-A artifact runs to Phase-B verdict JSONs and score them.

    Verdict files are named ``r{round}-{config}-verdict.json`` in
    ``verdict_dir`` (the SKILL instructs each sub-agent to write that path).
    A run with no verdict file becomes a ScoredRunV2 with ``fold_error`` set
    rather than being silently dropped.
    """
    scored: list[ScoredRunV2] = []
    for rnd in artifact.get("rounds") or []:
        round_n = rnd.get("round")
        for run in rnd.get("runs") or []:
            config = run.get("config") or run.get("backend") or "?"
            vpath = verdict_dir / f"r{round_n}-{config}-verdict.json"
            replay_findings = run.get("replay_findings") or []
            mechanical_match = run.get("mechanical_match") or []
            base = dict(
                backend=run.get("backend") or "?",
                model=run.get("model") or "?",
                config=config,
                envelope_status=run.get("envelope_status") or "missing",
                verdict=run.get("verdict"),
                duration_ms=run.get("duration_ms"),
            )
            if not vpath.exists():
                scored.append(
                    ScoredRunV2(
                        **base,
                        n_findings_replay=len(replay_findings),
                        judged_tp=0,
                        judged_fp=0,
                        judged_unsure=len(replay_findings),
                        n_missed_real=0,
                        precision=None,
                        recall=None,
                        f1=None,
                        dataset_comparisons=0,
                        dataset_agreements=0,
                        dataset_agreement_rate=None,
                        fold_error=f"no verdict file at {vpath}",
                    )
                )
                continue
            try:
                judge_verdict = json.loads(vpath.read_text())
            except (OSError, json.JSONDecodeError) as e:
                judge_verdict = {}
                err: Optional[str] = f"verdict JSON unreadable: {e}"
            else:
                err = validate_verdict(judge_verdict)
            if err:
                scored.append(
                    ScoredRunV2(
                        **base,
                        n_findings_replay=len(replay_findings),
                        judged_tp=0,
                        judged_fp=0,
                        judged_unsure=len(replay_findings),
                        n_missed_real=0,
                        precision=None,
                        recall=None,
                        f1=None,
                        dataset_comparisons=0,
                        dataset_agreements=0,
                        dataset_agreement_rate=None,
                        fold_error=err,
                    )
                )
                continue
            scored.append(
                score_from_verdicts(
                    backend=base["backend"],
                    model=base["model"],
                    config=config,
                    envelope_status=base["envelope_status"],
                    verdict=base["verdict"],
                    duration_ms=base["duration_ms"],
                    replay_findings=replay_findings,
                    mechanical_match=mechanical_match,
                    judge_verdict=judge_verdict,
                )
            )
    return scored


def scored_run_to_dict(s: ScoredRunV2) -> dict:
    return asdict(s)
