#!/usr/bin/env python3
"""Bramble-side operations for the pr-polish skill.

Pure helpers the orchestrator composes inline:

  - ``goal_for_round`` builds the ``--goal`` text (PR_SUMMARY on round 1,
    action-history string on round 2+).
  - ``prior_session_id`` looks up the resume id for round N+1.
  - ``parse_stream`` / ``parse_envelope`` / ``triage`` digest captured
    Monitor output into the consensus + N+1 spiral pipeline.
  - ``bramble_bin`` returns the binary picked at the top of the run.

Usage:
    python3 bramble_ops.py goal <round> --state-file <path> --pr-summary <text>
    python3 bramble_ops.py parse-stream <stream_file> --backend <b>
    python3 bramble_ops.py triage <round> <prior_state_file>
                                  [--stream BACKEND=PATH ...]
                                  [--pr-comments FILE] [--ci-failures FILE]
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

from _common import (  # noqa: E402
    print_json,
    read_json,
    severity_rank,
    topic_of,
)

# Source labels that flow through parse/triage. ``lint`` is here even though
# it isn't a bramble backend — lint_gate.py emits findings under the same
# envelope schema and triage treats them as just another source for
# consensus/spiral matching.
BACKENDS = ("codex", "cursor", "gemini", "lint")


def bramble_bin() -> str:
    """Path to the bramble CLI. The SKILL exports ``BRAMBLE_BIN`` at the
    top of a run after sniffing the worktree; all invocations route through
    here so dev-tree builds and installed binaries stay interchangeable.
    """
    return os.environ.get("BRAMBLE_BIN") or "bramble"


# Cap on number of action entries surfaced in the goal text. Long lists
# don't help the model and inflate token cost. The orchestrator's full
# state file remains the canonical record for human audit.
_ACTION_HISTORY_CAP = 10


def action_history_goal(state: dict[str, Any] | None, round_: int) -> str:
    """Build the --goal text for round 2+: tell the resumed model what
    prior rounds already fixed and skipped, so it doesn't waste a round
    re-flagging things it already raised.

    On round 1 (or with no state / no prior actions), returns "" — the
    caller passes the PR_SUMMARY as goal instead, and bramble's follow-up
    prompt builder treats empty goal as "PR-level intent already in
    session context, no per-turn metadata to inject".

    On round 2+ with prior actions, returns a short summary:

        Round 3. Prior rounds fixed: a.go:10 codex; b.py:42 cursor.
        Skipped: c.go:8 wont_fix (design tradeoff); d.go:5 stale.

    Bramble's BuildFollowUpJSONPromptWithScope embeds this as
    "Context for this turn: <text>" so the resumed model reads it as
    orchestrator-supplied per-turn state rather than as a re-statement
    of the session goal.

    Capped at _ACTION_HISTORY_CAP entries each for fixed and skipped to
    keep the goal short. The full audit trail lives in the state file's
    rounds[*].comment_actions; this is just the model-facing prompt.
    """
    if round_ < 2 or not state:
        return ""
    rounds = state.get("rounds") or []
    fixed: list[str] = []
    skipped: list[str] = []
    for rnd in sorted(rounds, key=lambda r: r.get("n") or 0):
        n = rnd.get("n") or 0
        if n >= round_:
            break
        for action in rnd.get("comment_actions") or []:
            label = _action_label(action)
            if not label:
                continue
            verb = action.get("action")
            if verb == "fixed":
                fixed.append(label)
            elif verb in ("false_positive", "wont_fix", "stale", "ack"):
                skipped.append(f"{label} ({verb})")
    if not fixed and not skipped:
        return ""
    parts = [f"Round {round_}."]
    if fixed:
        truncated = fixed[:_ACTION_HISTORY_CAP]
        suffix = f"; ({len(fixed) - len(truncated)} more)" if len(fixed) > len(truncated) else ""
        parts.append("Prior rounds fixed: " + "; ".join(truncated) + suffix + ".")
    if skipped:
        truncated = skipped[:_ACTION_HISTORY_CAP]
        suffix = f"; ({len(skipped) - len(truncated)} more)" if len(skipped) > len(truncated) else ""
        parts.append("Skipped: " + "; ".join(truncated) + suffix + ".")
    return " ".join(parts)


def _action_label(action: dict[str, Any]) -> str:
    """Format a single comment_actions entry as a one-line label for the
    goal-string. Uses path:line plus source for bramble findings; topic
    when present is dropped from the label since it inflates length and
    the model can re-derive specifics from session context.
    """
    path = action.get("path")
    line = action.get("line")
    source = action.get("source") or "?"
    if path and line is not None:
        return f"{path}:{line} ({source})"
    if path:
        return f"{path} ({source})"
    return ""


def goal_for_round(
    round_: int,
    pr_summary: str,
    state: dict[str, Any] | None,
) -> str:
    """Return the ``--goal`` text bramble should see for this round.

    Round 1: the PR_SUMMARY (commit list + diffstat). Bramble embeds it as
    the PR-level intent in the fresh-prompt builder.

    Round 2+: an action-history string telling the resumed model what
    prior rounds already fixed and skipped. Bramble embeds it as
    ``Context for this turn: <text>`` so the model doesn't re-flag its
    own fixes. Falls back to PR_SUMMARY when state has no prior actions
    (e.g. round 1 produced an empty action plan).
    """
    if round_ < 2:
        return pr_summary
    history = action_history_goal(state, round_)
    return history or pr_summary


# ---------------------------------------------------------------------------
# parse-stream: extract the terminal envelope from a Monitor stdout capture
# ---------------------------------------------------------------------------


def extract_terminal_envelope(stream_text: str) -> dict[str, Any] | None:
    """Return the last envelope JSON line in the stream, or None.

    The stream is NDJSON: zero or more `{"event":"progress",...}` lines
    followed by exactly one `{"schema_version":...,"status":...}` envelope
    line (see bramble/cmd/codereview/codereview.go deferred guard). We scan
    bottom-up for the envelope so progress-line parse failures don't derail
    us, and we identify the envelope by the presence of the `schema_version`
    top-level key — the most unique marker that also survives minor schema
    additions.
    """
    for line in reversed(stream_text.splitlines()):
        line = line.strip()
        if not line or not line.startswith("{"):
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(obj, dict) and "schema_version" in obj and "status" in obj:
            return obj
    return None


def parse_stream(stream_path: Path, *, source: str) -> list[dict[str, Any]]:
    """Read Monitor's captured stdout (or a standalone envelope file) and return findings.

    Tries whole-file ``json.loads`` first so producers that write a single
    pretty-printed envelope (e.g. ``lint_gate.py`` via ``atomic_write_json``
    with ``indent=2``) parse correctly. Falls back to the NDJSON line-scan for
    real Monitor streams (progress lines + a final envelope line). If neither
    yields an envelope, synthesize a high-severity ``bramble-empty-envelope``
    finding so triage surfaces the failure instead of treating it as
    "converged to zero".
    """
    if not stream_path.exists():
        return []
    try:
        text = stream_path.read_text()
    except OSError:
        return []
    env: dict[str, Any] | None = None
    stripped = text.strip()
    if stripped.startswith("{"):
        try:
            obj = json.loads(stripped)
        except json.JSONDecodeError:
            obj = None
        if isinstance(obj, dict) and "schema_version" in obj and "status" in obj:
            env = obj
    if env is None:
        env = extract_terminal_envelope(text)
    if env is None:
        return [
            {
                "source": source,
                "severity": "high",
                "file": None,
                "line": None,
                "message": "bramble stream ended without producing an envelope",
                "suggestion": "re-launch the Monitor arm; see bramble logs under ~/.bramble/logs/code-review/",
                "topic": "bramble-empty-envelope",
                "status": "exited-empty",
            }
        ]
    return parse_envelope(env, source=source)


# ---------------------------------------------------------------------------
# parse: envelope -> findings
# ---------------------------------------------------------------------------


def parse_envelope(obj: dict[str, Any] | None, *, source: str) -> list[dict[str, Any]]:
    """Extract findings from one bramble envelope dict. Pure — used in tests."""
    if obj is None:
        return []
    status = obj.get("status")
    if status != "ok":
        # Emit a synthetic "stale" finding so triage can surface the failure.
        return [
            {
                "source": source,
                "severity": None,
                "file": None,
                "line": None,
                "message": (obj.get("error") or "bramble run failed"),
                "suggestion": None,
                "topic": topic_of(obj.get("error") or "bramble run failed"),
                "status": status,
            }
        ]
    issues = (obj.get("review") or {}).get("issues") or []
    out = []
    for i in issues:
        msg = i.get("message") or ""
        out.append(
            {
                "source": source,
                "severity": i.get("severity"),
                "file": i.get("file"),
                "line": i.get("line"),
                "message": msg,
                "suggestion": i.get("suggestion"),
                "topic": topic_of(msg),
            }
        )
    return out


def parse_round(
    streams: dict[str, Path],
    backends: list[str] | None = None,
) -> list[dict[str, Any]]:
    """Aggregate findings across backends for one pr-polish round.

    ``streams`` maps backend name to the Monitor-captured stream file for
    that backend. Backends not in the mapping are silently skipped (the
    backend simply wasn't enabled). Backends in the mapping whose path
    doesn't exist yield a synthetic high-severity ``stream-missing``
    finding so a typo'd ``--stream cursor=/typo/path`` surfaces in
    triage instead of disappearing.
    """
    backends = backends or list(BACKENDS)
    out: list[dict[str, Any]] = []
    for b in backends:
        path = streams.get(b)
        if path is None:
            continue
        if not path.exists():
            out.append(
                {
                    "source": b,
                    "severity": "high",
                    "file": None,
                    "line": None,
                    "message": f"--stream {b}={path} does not exist on disk",
                    "suggestion": "verify the Monitor capture path; check stderr for the failed bramble run",
                    "topic": "stream-missing",
                    "status": "missing",
                }
            )
            continue
        out.extend(parse_stream(path, source=b))
    return out


# ---------------------------------------------------------------------------
# triage: consensus + N+1 diff spiral detection
# ---------------------------------------------------------------------------


def _triage_key(f: dict[str, Any]) -> tuple:
    """Spiral-detection key. Includes topic so a finding that was fixed and
    later re-flagged with the same wording matches the prior-round entry,
    while a different topic on the same line is treated as a new issue.
    """
    return (f.get("file"), f.get("line"), f.get("topic"))


def _consensus_key(f: dict[str, Any]) -> tuple:
    """Consensus-grouping key. Drops topic so two reviewers wording the same
    issue differently still collapse into one consensus entry. Two unrelated
    findings that happen to land on the same line will also collapse — but
    that's much rarer than the false-negative case (codex says
    "TestEmitEarlyFailure does not assert resume_status=unverified", cursor
    says "TestEmitEarlyFailure does not set resumeSessionID", same finding,
    same line, prior code keyed on topic and routed both to single_medium
    instead of must_fix consensus).
    """
    return (f.get("file"), f.get("line"))


_HIGH_SEVERITY_KEYWORDS = (
    "critical",
    "must fix",
    "must-fix",
    "security",
    "vulnerab",
    "crash",
    "data loss",
)


def pr_comment_to_finding(c: dict[str, Any]) -> dict[str, Any]:
    """Convert a classify_comments output row into a triage-ready finding.

    GitHub comments don't carry an explicit severity; we infer ``high`` when the
    body contains urgent-tone keywords (security, critical, must fix, etc.) and
    otherwise default to ``medium``. The orchestrator can override per-comment
    by pre-tagging ``severity`` on the dict before passing it in.
    """
    body = (c.get("body") or "").lower()
    severity = c.get("severity")
    if severity is None:
        severity = "high" if any(k in body for k in _HIGH_SEVERITY_KEYWORDS) else "medium"
    return {
        "source": c.get("source") or "github-inline",
        "severity": severity,
        "file": c.get("path"),
        "line": c.get("line"),
        "message": c.get("body") or "",
        "suggestion": None,
        "topic": topic_of(c.get("body") or ""),
        "comment_id": c.get("id"),
        "author": c.get("author"),
        "is_bot": c.get("is_bot"),
        "original_commit_id": c.get("original_commit_id"),
        "is_stale_prior_commit": bool(c.get("is_stale_prior_commit")),
    }


def ci_failure_to_finding(f: dict[str, Any]) -> dict[str, Any]:
    """Convert a ``ci_failed_tests`` entry into a triage-ready finding.

    Flake-classified failures are routed to ``low`` severity (they still get
    logged as skipped for the audit trail). Genuine assertion failures come in
    as ``high`` so they land in ``single_critical``.
    """
    is_flake = bool(f.get("is_flake"))
    severity = "low" if is_flake else "high"
    test_name = (f.get("failed_tests") or [None])[0] or (f.get("job_name") or "unknown")
    msg = f.get("assertion_snippet") or test_name
    return {
        "source": "ci",
        "severity": severity,
        "file": str(f.get("job_id")) if f.get("job_id") is not None else None,
        "line": None,
        "message": msg,
        "suggestion": None,
        "topic": test_name,
        "job_id": f.get("job_id"),
        "is_flake": is_flake,
        "flake_reason": f.get("flake_reason"),
    }


def triage(
    findings: list[dict[str, Any]],
    prior_fixed_keys: set[tuple],
    *,
    pr_comments: list[dict[str, Any]] | None = None,
    ci_failures: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """Group findings, surface consensus, detect N+1 spiral matches.

    Pure — used directly in tests.

    Two-level keying:
    - ``_consensus_key`` = ``(file, line)`` — drives location-based consensus
      so two reviewers wording the same finding differently still consolidate.
      For sourceless paths (no file/line) we fall back to the topic-based
      ``_triage_key`` so cross-cutting findings can still pair up.
    - ``_triage_key`` = ``(file, line, topic)`` — drives the N+1 spiral guard
      where exact recurrence (same wording at same site) matters.

    Consensus = ``_consensus_key`` flagged by >=2 distinct sources (or
    ``_triage_key`` matched for sourceless paths).
    Spiral match = a new finding whose ``_triage_key`` matches a prior-round
    entry whose action was ``fixed``.

    ``pr_comments`` are classify_comments rows — round 1 only typically.
    ``ci_failures`` are ci_failed_tests rows. Both are converted to findings
    via ``pr_comment_to_finding`` / ``ci_failure_to_finding`` and merged into
    the same grouping pipeline so bramble-only consensus and cross-source
    consensus (e.g. codex + CI agreeing on a broken test) both surface.
    """
    all_findings = list(findings)
    if pr_comments:
        all_findings.extend(pr_comment_to_finding(c) for c in pr_comments)
    if ci_failures:
        all_findings.extend(ci_failure_to_finding(f) for f in ci_failures)

    # Partition off stale-on-prior-commit PR comments before key grouping. They
    # were posted against superseded code, so they must not pair with a fresh
    # codex/cursor finding to form spurious consensus, and they must skip the
    # severity buckets entirely — the orchestrator records them as `stale` and
    # auto-replies with a "Superseded by …" note.
    stale_prior_commit: list[dict[str, Any]] = []
    fresh_findings: list[dict[str, Any]] = []
    for f in all_findings:
        if f.get("is_stale_prior_commit"):
            stale_prior_commit.append({"key": list(_triage_key(f)), "finding": f})
        else:
            fresh_findings.append(f)

    # Two-level keying: triage_key (file, line, topic) drives spiral
    # detection and single-source bucketing; consensus_key (file, line)
    # drives cross-source consensus so two reviewers wording the same
    # location differently still collapse into one must_fix entry.
    by_triage_key: dict[tuple, list[dict[str, Any]]] = {}
    by_consensus_key: dict[tuple, list[dict[str, Any]]] = {}
    for f in fresh_findings:
        by_triage_key.setdefault(_triage_key(f), []).append(f)
        by_consensus_key.setdefault(_consensus_key(f), []).append(f)

    # First pass: identify (file, line) groups with >=2 distinct sources.
    consensus: list[dict[str, Any]] = []
    consensus_triage_keys: set[tuple] = set()
    for ckey, group in by_consensus_key.items():
        if ckey == (None, None) or ckey[0] is None:
            # Top-level / file-less findings (PR-level comments) can't form
            # location-based consensus. Leave them to the triage_key pipeline.
            continue
        sources = {g["source"] for g in group}
        if len(sources) >= 2:
            consensus.append(
                {"key": list(ckey), "sources": sorted(sources), "findings": group}
            )
            for g in group:
                consensus_triage_keys.add(_triage_key(g))

    single_critical: list[dict[str, Any]] = []
    single_medium: list[dict[str, Any]] = []
    low_acks: list[dict[str, Any]] = []
    spiral_matches: list[dict[str, Any]] = []

    for key, group in by_triage_key.items():
        severities = [severity_rank(g.get("severity")) for g in group]
        top = max(severities) if severities else -1
        repr_ = group[0]
        # Spiral check: strict (path, line, topic) match, or location-only
        # fallback so a fix-then-rewording regression at the same site
        # still escalates even when the stored action's topic string and
        # the new finding's topic_of(message) drift apart.
        location_key = (key[0], key[1], None)
        if key in prior_fixed_keys or location_key in prior_fixed_keys:
            spiral_matches.append({"key": list(key), "findings": group})
        if key in consensus_triage_keys:
            # Already routed to consensus by location-based grouping;
            # don't double-list it under a single-source bucket.
            continue
        sources = {g["source"] for g in group}
        if len(sources) >= 2:
            # Same triage key (incl. topic) flagged by >=2 sources — also
            # consensus, even when location-based grouping didn't catch it
            # (e.g. file-less PR-level comments).
            consensus.append({"key": list(key), "sources": sorted(sources), "findings": group})
        elif top >= severity_rank("high"):
            single_critical.append({"key": list(key), "finding": repr_})
        elif top == severity_rank("medium"):
            single_medium.append({"key": list(key), "finding": repr_})
        elif top <= severity_rank("low"):
            low_acks.append({"key": list(key), "finding": repr_})

    # action_plan is a dispatch hint derived from the groupings above. Triage
    # rules in SKILL.md: consensus + high = must_fix; medium = consider_fix;
    # low/nit = batch_ack; spiral_matches = escalate (prior fix may have
    # regressed, or reviewer is re-flagging something we thought resolved).
    # A spiral match wins over its severity bucket — escalate and stop, so the
    # orchestrator doesn't auto-fix something that already round-tripped.
    # spiral_matches use triage keys (file, line, topic); consensus entries
    # may use either consensus keys (file, line, two-element) or triage keys
    # (file, line, topic, three-element) depending on which path created them.
    # Match a consensus entry as "in spiral" if any spiral key shares its
    # location prefix (file, line) — the same shape as the consensus key.
    spiral_triage_keys = {tuple(sm["key"]) for sm in spiral_matches}
    spiral_locations = {(k[0], k[1]) for k in spiral_triage_keys}

    def _without_spiral(items: list[dict[str, Any]]) -> list[dict[str, Any]]:
        out = []
        for i in items:
            k = tuple(i["key"])
            if k in spiral_triage_keys:
                continue
            if len(k) >= 2 and (k[0], k[1]) in spiral_locations:
                continue
            out.append(i)
        return out

    return {
        "consensus": consensus,
        "single_critical": single_critical,
        "single_medium": single_medium,
        "low_acks": low_acks,
        "spiral_matches": spiral_matches,
        "stale_prior_commit": stale_prior_commit,
        "action_plan": {
            "must_fix": _without_spiral(consensus + single_critical),
            "consider_fix": _without_spiral(single_medium),
            "batch_ack": _without_spiral(low_acks),
            "batch_stale": stale_prior_commit,
            "escalate": spiral_matches,
        },
        # ``total`` covers all post-merge findings (bramble + pr_comments +
        # ci_failures). Reporting only ``len(findings)`` would undercount
        # comment/CI-only triage runs (zero bramble issues, populated buckets).
        "total": len(all_findings),
        "unique": len(by_triage_key),
    }


def prior_fixed_keys(state: dict[str, Any] | None) -> set[tuple]:
    """Collect spiral-match keys for every prior-round ``fixed`` action.

    Returns both the strict ``(path, line, topic)`` triage key and a softer
    ``(path, line, None)`` fallback. Spiral detection looks for either:

    - The strict form catches exact recurrence (same wording at same site).
    - The fallback catches "fix at same site, reviewer reworded it"
      regressions where the persisted ``topic`` string and the new
      finding's ``topic_of(message)`` happen not to collide. Stored
      actions whose ``topic`` is missing or got rewritten by a human
      auditor would otherwise silently disable spiral detection.

    Routing remains unchanged: callers test ``key in prior_fixed_keys``;
    we now also emit the ``(path, line, None)`` companion so a new
    finding's ``_triage_key`` of ``(path, line, None)`` (sourceless or
    different topic) still triggers escalation.
    """
    keys: set[tuple] = set()
    if not state:
        return keys
    for rnd in state.get("rounds") or []:
        for a in rnd.get("comment_actions") or []:
            if a.get("action") != "fixed":
                continue
            path, line, topic = a.get("path"), a.get("line"), a.get("topic")
            keys.add((path, line, topic))
            keys.add((path, line, None))
    return keys


def prior_session_id(state: dict[str, Any] | None, backend: str, round_: int) -> str:
    """Return the newest prior session id for backend before ``round_``.

    State files have evolved over time, so accept both explicit round metadata
    (``session_ids`` / ``<backend>_session_id``) and persisted raw envelopes
    under ``reviews`` when present.
    """
    if not state or round_ < 2:
        return ""
    rounds = state.get("rounds") or []
    for rnd in sorted(rounds, key=lambda r: r.get("n") or 0, reverse=True):
        n = rnd.get("n") or 0
        if n >= round_:
            continue
        session_ids = rnd.get("session_ids") or {}
        sid = session_ids.get(backend) or rnd.get(f"{backend}_session_id")
        if sid:
            return str(sid)
        reviews = rnd.get("reviews") or {}
        env = reviews.get(backend) if isinstance(reviews, dict) else None
        if isinstance(env, dict) and env.get("session_id"):
            return str(env["session_id"])
    return ""


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="bramble_ops")
    sub = p.add_subparsers(dest="cmd", required=True)

    sp = sub.add_parser(
        "goal",
        help="Print the --goal text for round N (PR_SUMMARY or action-history).",
    )
    sp.add_argument("round_", type=int)
    sp.add_argument("--pr-summary", required=True)
    sp.add_argument("--state-file")

    sp = sub.add_parser(
        "prior-session-id",
        help="Print the resume session id for a backend at round N (empty if none).",
    )
    sp.add_argument("backend", choices=BACKENDS)
    sp.add_argument("round_", type=int)
    sp.add_argument("--state-file", required=True)

    sp = sub.add_parser(
        "parse-stream",
        help="Parse a Monitor stdout capture and emit findings for one backend.",
    )
    sp.add_argument("stream_file")
    sp.add_argument("--backend", required=True, choices=BACKENDS)

    sp = sub.add_parser("triage")
    sp.add_argument("round_", type=int)
    sp.add_argument("prior_state_file", nargs="?")
    sp.add_argument(
        "--stream",
        action="append",
        default=[],
        metavar="BACKEND=PATH",
        help="Monitor capture per backend; may be repeated.",
    )
    sp.add_argument(
        "--pr-comments",
        metavar="FILE",
        help="JSON file with classify_comments output (round 1 input).",
    )
    sp.add_argument(
        "--ci-failures",
        metavar="FILE",
        help="JSON file with ci_failed_tests output.",
    )

    return p


def _parse_stream_args(pairs: list[str]) -> dict[str, Path]:
    """Parse repeated --stream BACKEND=PATH options into a mapping.

    Argparse doesn't natively support dict-valued options, so we split on the
    first "=" per token. Invalid entries surface as ValueError so the CLI
    fails loudly instead of silently dropping a misspelled backend.
    """
    out: dict[str, Path] = {}
    for entry in pairs:
        if "=" not in entry:
            raise ValueError(f"--stream must be BACKEND=PATH, got {entry!r}")
        backend, path = entry.split("=", 1)
        if backend not in BACKENDS:
            raise ValueError(f"unknown backend in --stream: {backend!r}")
        out[backend] = Path(path)
    return out


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    try:
        if args.cmd == "goal":
            state = read_json(Path(args.state_file), default=None) if args.state_file else None
            print(goal_for_round(args.round_, args.pr_summary, state))
        elif args.cmd == "prior-session-id":
            state = read_json(Path(args.state_file), default=None)
            print(prior_session_id(state, args.backend, args.round_))
        elif args.cmd == "parse-stream":
            findings = parse_stream(Path(args.stream_file), source=args.backend)
            print_json(findings)
        elif args.cmd == "triage":
            streams = _parse_stream_args(args.stream)
            findings = parse_round(streams)
            prior = None
            if args.prior_state_file:
                prior = read_json(Path(args.prior_state_file), default=None)
            pr_comments = None
            if args.pr_comments:
                pr_comments = read_json(Path(args.pr_comments), default=[])
                if isinstance(pr_comments, dict):
                    pr_comments = pr_comments.get("comments", [])
                if not isinstance(pr_comments, list):
                    raise ValueError(
                        "--pr-comments must point to a JSON array "
                        "or an object with a 'comments' array"
                    )
            ci_failures = None
            if args.ci_failures:
                ci_failures = read_json(Path(args.ci_failures), default=[])
                if not isinstance(ci_failures, list):
                    raise ValueError("--ci-failures must point to a JSON array")
            result = triage(
                findings,
                prior_fixed_keys(prior),
                pr_comments=pr_comments,
                ci_failures=ci_failures,
            )
            print_json(result)
        else:  # pragma: no cover
            raise ValueError(f"unknown cmd: {args.cmd}")
    except Exception as e:  # noqa: BLE001
        print(f"error: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
