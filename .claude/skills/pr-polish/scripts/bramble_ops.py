#!/usr/bin/env python3
"""Bramble-side operations for the pr-polish skill.

Formats the ``bramble code-review`` invocation the orchestrator arms in the
Claude ``Monitor`` tool, parses the stream Monitor captures, and shares the
cross-backend triage helpers with the rest of the skill.

Usage:
    python3 bramble_ops.py format-monitor-command <backend> <model> <round> \\
                                                  --goal <text> --pr <n> \\
                                                  [--repo <slug>] [--work-dir <dir>]
    python3 bramble_ops.py parse-stream <round> --backend <b> <stream_file>
                                                 [--repo <slug>] [--pr <n>]
    python3 bramble_ops.py triage <round> <prior_state_file> --pr <n> [--repo <slug>]
"""

from __future__ import annotations

import argparse
import json
import os
import shlex
import sys
from pathlib import Path
from typing import Any

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

from _common import (  # noqa: E402
    SOURCE_INLINE,
    print_json,
    read_json,
    repo_slug,
    severity_rank,
    topic_of,
)

BACKENDS = ("codex", "cursor", "gemini")


# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------


def envelope_path(repo: str, pr: int | str, backend: str, round_: int) -> Path:
    """Envelope path. ``pr`` is typically the PR number but may also be a
    branch-scoped slug like ``branch-foo`` for branch-only runs.
    """
    return Path("/tmp") / f"pp-{repo}-{pr}-{backend}-r{round_}.json"


def stderr_path(repo: str, pr: int | str, backend: str, round_: int) -> Path:
    return Path("/tmp") / f"pp-{repo}-{pr}-{backend}-r{round_}.stderr.txt"


# ---------------------------------------------------------------------------
# launch: fire-and-forget bramble invocation
# ---------------------------------------------------------------------------


def build_launch_command(backend: str, model: str, goal: str) -> list[str]:
    """Return the canonical bramble CLI invocation. Pure — used in tests."""
    if backend not in BACKENDS:
        raise ValueError(f"unknown backend {backend!r}; expected one of {BACKENDS}")
    return [
        "bramble",
        "code-review",
        "--backend",
        backend,
        "--model",
        model,
        "--json",
        "--skip-test-execution",
        "--verbose",
        "--timeout",
        "10m",
        "--goal",
        goal,
    ]


def launch_env(repo: str, pr: int, backend: str, round_: int, work_dir: str) -> dict[str, str]:
    """Exact env vars bramble expects. Pure — used in tests."""
    return {
        "BRAMBLE_RUN_TAG": f"pr-polish:{repo}:{pr}:{backend}:r{round_}",
        "WORK_DIR": work_dir,
    }


def format_monitor_command(
    backend: str,
    model: str,
    round_: int,
    goal: str,
    *,
    repo: str | None = None,
    pr: int | str | None = None,
    work_dir: str | None = None,
) -> str:
    """Return the shell command the orchestrator passes as Monitor's `command`.

    Monitor execs this string directly, so it must (a) cd into the worktree
    (bramble code-review resolves relative paths there), (b) set
    `BRAMBLE_RUN_TAG` so per-run logs are searchable, and (c) invoke
    `bramble code-review --json ...` with the pr-polish canonical flags.

    Keeping the formatting in one function — rather than embedding it in the
    SKILL.md prose — means the quoting rules for `--goal` (which often
    contains backticks, parentheses, or quotes from a PR summary) are under
    unit test instead of scattered across examples in the skill docs.

    ``pr`` accepts a PR number (int) or a branch slug (str like
    ``"branch-foo"``) for branch-only runs. Passing 0/empty raises.
    """
    if backend not in BACKENDS:
        raise ValueError(f"unknown backend {backend!r}; expected one of {BACKENDS}")
    repo = repo or repo_slug()
    if pr is None:
        env_pr = os.environ.get("PR_NUMBER", "").strip()
        if env_pr:
            try:
                pr = int(env_pr)
            except ValueError:
                pr = env_pr
    if not pr:
        raise ValueError(
            "pr number or branch slug is required (pass --pr or set PR_NUMBER env var)"
        )
    work_dir = work_dir or os.getcwd()

    tag = f"pr-polish:{repo}:{pr}:{backend}:r{round_}"
    # shlex.quote keeps embedded quotes/backticks in the goal intact. The cd
    # is conditional on the dir actually existing so a stale saved command
    # fails loudly (Monitor reports non-zero exit) rather than silently
    # running bramble in the wrong working tree.
    parts = [
        "cd",
        shlex.quote(work_dir),
        "&&",
        f"BRAMBLE_RUN_TAG={shlex.quote(tag)}",
        "bramble",
        "code-review",
        "--backend",
        backend,
        "--model",
        model,
        "--json",
        "--skip-test-execution",
        "--verbose",
        "--timeout",
        "10m",
        "--goal",
        shlex.quote(goal),
    ]
    return " ".join(parts)


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
    """Read Monitor's captured stdout and return findings.

    If the stream exists but contains no envelope line, synthesize a
    high-severity `bramble-empty-envelope` finding so triage surfaces the
    failure instead of treating it as "converged to zero". After the bramble
    deferred guard lands this path is rare, but keeping it as cheap insurance
    means a future bramble regression can't silently poison a /pr-polish loop.
    """
    if not stream_path.exists():
        return []
    try:
        text = stream_path.read_text()
    except OSError:
        return []
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


def _envelope_ready(path: Path) -> dict[str, Any] | None:
    """Read a pre-written envelope file. Returns the parsed dict or None.

    Used by ``parse_round`` as the fallback when no Monitor stream is supplied
    for a backend.
    """
    if not path.exists() or path.stat().st_size == 0:
        return None
    try:
        obj = json.loads(path.read_text())
    except json.JSONDecodeError:
        return None
    if not isinstance(obj, dict) or "status" not in obj:
        return None
    return obj


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
    round_: int,
    *,
    streams: dict[str, Path] | None = None,
    backends: list[str] | None = None,
    repo: str | None = None,
    pr: int | str | None = None,
) -> list[dict[str, Any]]:
    """Aggregate findings across backends for one pr-polish round.

    ``streams`` maps backend name to the path Monitor captured for that
    backend's ``bramble code-review`` invocation. When a backend's stream is
    absent, fall back to the per-backend envelope file (``envelope_path``).
    ``pr`` accepts a PR number or branch slug; only used by ``envelope_path``
    when ``streams`` doesn't cover a backend.
    """
    repo = repo or repo_slug()
    if pr is None:
        env_pr = os.environ.get("PR_NUMBER", "").strip()
        if env_pr:
            try:
                pr = int(env_pr)
            except ValueError:
                pr = env_pr
        else:
            pr = 0
    backends = backends or list(BACKENDS)
    streams = streams or {}
    out: list[dict[str, Any]] = []
    for b in backends:
        if b in streams and streams[b] is not None:
            out.extend(parse_stream(streams[b], source=b))
            continue
        obj = _envelope_ready(envelope_path(repo, pr, b, round_))
        out.extend(parse_envelope(obj, source=b))
    return out


# ---------------------------------------------------------------------------
# triage: consensus + N+1 diff spiral detection
# ---------------------------------------------------------------------------


def _triage_key(f: dict[str, Any]) -> tuple:
    return (f.get("file"), f.get("line"), f.get("topic"))


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
        "source": c.get("source") or SOURCE_INLINE,
        "severity": severity,
        "file": c.get("path"),
        "line": c.get("line"),
        "message": c.get("body") or "",
        "suggestion": None,
        "topic": topic_of(c.get("body") or ""),
        "comment_id": c.get("id"),
        "author": c.get("author"),
        "is_bot": c.get("is_bot"),
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

    Consensus = same `(file, line, topic)` flagged by >=2 distinct sources.
    Spiral match = a new finding whose `(file, line, topic)` matches a
    prior-round entry whose action was ``fixed``.

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

    by_key: dict[tuple, list[dict[str, Any]]] = {}
    for f in all_findings:
        by_key.setdefault(_triage_key(f), []).append(f)

    consensus: list[dict[str, Any]] = []
    single_critical: list[dict[str, Any]] = []
    single_medium: list[dict[str, Any]] = []
    low_acks: list[dict[str, Any]] = []
    spiral_matches: list[dict[str, Any]] = []

    for key, group in by_key.items():
        sources = {g["source"] for g in group}
        severities = [severity_rank(g.get("severity")) for g in group]
        top = max(severities) if severities else -1
        repr_ = group[0]
        if key in prior_fixed_keys:
            spiral_matches.append({"key": list(key), "findings": group})
        if len(sources) >= 2:
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
    spiral_keys = {tuple(sm["key"]) for sm in spiral_matches}

    def _without_spiral(items: list[dict[str, Any]]) -> list[dict[str, Any]]:
        return [i for i in items if tuple(i["key"]) not in spiral_keys]

    return {
        "consensus": consensus,
        "single_critical": single_critical,
        "single_medium": single_medium,
        "low_acks": low_acks,
        "spiral_matches": spiral_matches,
        "action_plan": {
            "must_fix": _without_spiral(consensus + single_critical),
            "consider_fix": _without_spiral(single_medium),
            "batch_ack": _without_spiral(low_acks),
            "escalate": spiral_matches,
        },
        "total": len(all_findings),
        "unique": len(by_key),
    }


def prior_fixed_keys(state: dict[str, Any] | None) -> set[tuple]:
    """Collect `(path, line, topic)` keys of every prior-round `fixed` action."""
    keys: set[tuple] = set()
    if not state:
        return keys
    for rnd in state.get("rounds") or []:
        for a in rnd.get("comment_actions") or []:
            if a.get("action") != "fixed":
                continue
            keys.add((a.get("path"), a.get("line"), a.get("topic")))
    return keys


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="bramble_ops")
    sub = p.add_subparsers(dest="cmd", required=True)

    sp = sub.add_parser(
        "format-monitor-command",
        help="Print the bramble code-review invocation the orchestrator arms in Monitor.",
    )
    sp.add_argument("backend", choices=BACKENDS)
    sp.add_argument("model")
    sp.add_argument("round_", type=int)
    sp.add_argument("--goal", required=True)
    sp.add_argument("--repo")
    sp.add_argument("--pr", type=int)
    sp.add_argument("--work-dir")

    sp = sub.add_parser(
        "parse-stream",
        help="Parse a Monitor stdout capture and emit findings for one backend.",
    )
    sp.add_argument("stream_file")
    sp.add_argument("--backend", required=True, choices=BACKENDS)

    sp = sub.add_parser(
        "parse",
        help="Aggregate findings across backends for one round; pass --stream per backend for the new Monitor-direct flow.",
    )
    sp.add_argument("round_", type=int)
    sp.add_argument("--backend", action="append", choices=BACKENDS)
    sp.add_argument("--repo")
    sp.add_argument("--pr", type=int)
    sp.add_argument(
        "--stream",
        action="append",
        default=[],
        metavar="BACKEND=PATH",
        help="Use the given Monitor capture as the source for a backend; may be repeated.",
    )

    sp = sub.add_parser("triage")
    sp.add_argument("round_", type=int)
    sp.add_argument("prior_state_file", nargs="?")
    sp.add_argument("--repo")
    sp.add_argument("--pr", type=int)
    sp.add_argument(
        "--stream",
        action="append",
        default=[],
        metavar="BACKEND=PATH",
        help="Same shape as parse --stream; used when findings live in Monitor captures.",
    )
    sp.add_argument(
        "--pr-comments",
        metavar="FILE",
        help="JSON file with classify_comments output (round 1 input). "
        "Merged into the finding set with source=github-* tags.",
    )
    sp.add_argument(
        "--ci-failures",
        metavar="FILE",
        help="JSON file with ci_failed_tests output. Merged into the finding "
        "set with source=ci and flake-aware severity routing.",
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
        if args.cmd == "format-monitor-command":
            cmd = format_monitor_command(
                args.backend,
                args.model,
                args.round_,
                args.goal,
                repo=args.repo,
                pr=args.pr,
                work_dir=args.work_dir,
            )
            # Print the raw command string (not JSON) so the orchestrator can
            # drop it into Monitor's `command` field verbatim.
            print(cmd)
        elif args.cmd == "parse-stream":
            findings = parse_stream(Path(args.stream_file), source=args.backend)
            print_json(findings)
        elif args.cmd == "parse":
            streams = _parse_stream_args(args.stream)
            findings = parse_round(
                args.round_,
                streams=streams,
                backends=args.backend,
                repo=args.repo,
                pr=args.pr,
            )
            print_json(findings)
        elif args.cmd == "triage":
            streams = _parse_stream_args(args.stream)
            findings = parse_round(args.round_, streams=streams, repo=args.repo, pr=args.pr)
            prior = None
            if args.prior_state_file:
                prior = read_json(Path(args.prior_state_file), default=None)
            pr_comments = None
            if args.pr_comments:
                pr_comments = read_json(Path(args.pr_comments), default=[])
                # pr_ops.py fetch-comments emits {"comments": [...], "noise_filtered": N, "noise_samples": [...]}.
                # Legacy callers still write a bare list; accept both.
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
