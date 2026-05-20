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
    python3 bramble_ops.py goal <round> --pr-summary <text> --state-file <path>
                                        [--head-before <sha>]
    python3 bramble_ops.py prior-session-id <backend> <round> --state-file <path>
                                            [--is-new-series 0|1]
    python3 bramble_ops.py parse-stream <stream_file> --backend <b>
    python3 bramble_ops.py triage [<prior_state_file>]
                                  [--stream BACKEND=PATH ...]
                                  [--pr-comments FILE] [--ci-failures FILE]
"""

from __future__ import annotations

import argparse
import json
import os
import re
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

# Review mode constants. Defined up here (rather than down by the
# per-mode key constructors) so they're available as default-argument
# values for parse_stream / parse_round below — those callers need a
# default well before _normalize_mode lands. The two strings must match
# yoloswe/reviewer.ReviewMode so the wire format stays stable across
# the Python triage layer and the Go envelope writer.
REVIEW_MODE_CODE = "code"
REVIEW_MODE_DESIGN_DOC = "design-doc"
REVIEW_MODES = (REVIEW_MODE_CODE, REVIEW_MODE_DESIGN_DOC)


def bramble_bin() -> str:
    """Path to the bramble CLI. The SKILL exports ``BRAMBLE_BIN`` at the
    top of a run after sniffing the worktree; all invocations route through
    here so dev-tree builds and installed binaries stay interchangeable.
    """
    return os.environ.get("BRAMBLE_BIN") or "bramble"


# Cap on number of action entries surfaced in the goal text. We emit only
# the immediately-prior round's actions (not a walk across all rounds), so
# 20 covers a fairly busy round; pathological rounds get truncated with a
# "(K more)" suffix. The full audit trail lives in rounds[*].comment_actions.
_ACTION_HISTORY_CAP = 20

# Per-entry topic length cap. Topics from triage already pass through
# topic_of() which folds long messages into shorter labels, but reviewer-
# supplied messages can still run long; truncate so a single noisy entry
# can't blow the whole prompt.
_TOPIC_CHAR_CAP = 80

# Default truncation cap for the inter-round diff appended to the goal
# channel (D1). 200 lines is roughly the same order of magnitude as a
# typical round's commit; bigger diffs almost always indicate a rebase
# or tooling-generated change that the reviewer doesn't need to re-read
# inline. Defined here at module top because ``goal_for_round`` (defined
# above ``round_diff``) consumes it as a default-argument value.
_ROUND_DIFF_DEFAULT_MAX_LINES = 200


def _is_first_round_of_series(state: dict[str, Any] | None, n: int) -> bool:
    """Mirror of pr_ops._is_first_round_of_series.

    Duplicated to keep bramble_ops free of pr_ops imports (pr_ops already
    depends on bramble_ops via _persist_round_findings; the inverse import
    would create a cycle).
    """
    if state is None or not state.get("rounds"):
        return True
    if state.get("completed"):
        return True
    return n == 1


def _files_changed_between(a: str | None, b: str | None) -> list[str]:
    """Repo-relative paths changed between two commits.

    Returns ``[]`` when either input is falsy, the SHAs are equal, or
    git fails (no remote, shallow clone, or the commits aren't reachable
    from the worktree). The caller treats an empty list as "no signal,
    omit the line" rather than "definitely no changes".

    Writes a one-line stderr warning when git rejects the range: a
    silently-empty result on a 200-file diff was the symptom that
    surfaced the new-series goal-text bug, and the caller can't see
    the unreachable SHA without it.
    """
    if not a or not b or a == b:
        return []
    # Lazy import: keep _common dependencies aligned with the rest of the
    # module's import block, but avoid pulling subprocess into helpers that
    # might be hot-path one day.
    from _common import run  # noqa: PLC0415

    try:
        res = run(["git", "diff", "--name-only", f"{a}..{b}"], check=False)
    except Exception as e:  # noqa: BLE001 — best-effort git invocation
        print(
            f"bramble_ops: git diff --name-only {a[:7]}..{b[:7]} failed: {e}",
            file=sys.stderr,
        )
        return []
    if res.returncode != 0:
        print(
            f"bramble_ops: git diff --name-only {a[:7]}..{b[:7]} returned "
            f"{res.returncode}; cited SHA may be unreachable. stderr: "
            f"{res.stderr.strip() or '(empty)'}",
            file=sys.stderr,
        )
        return []
    return [line.strip() for line in res.stdout.splitlines() if line.strip()]


def action_history_goal(
    state: dict[str, Any] | None,
    round_: int,
    *,
    head_before: str | None = None,
) -> str:
    """Build the --goal text for round 2+: a per-turn briefing telling the
    resumed model what the immediately-prior round actioned plus which
    files have changed since that round closed.

    Returns "" on round 1, missing state, or when there's nothing to say.
    Otherwise the shape is (matches ``_action_label`` / ``_skipped_label``):

        Round 6. Prior round fixed: a.go:10 — null check missing on BUILDER_LITE;
        b.py:42 — race in cache invalidation.
        Skipped: c.go:8 wont_fix: caller already validates;
        d.go:5 ack: rename helper.
        Files changed since round 5: a.go, b.py.

    Bramble's BuildFollowUpJSONPromptWithScope embeds this as
    ``Context for this turn: <text>`` so the resumed model reads it as
    per-turn metadata, not as a re-statement of the session goal.

    Only the immediately-prior round's actions are surfaced — the model
    has earlier turns in conversation context, so re-listing them is
    wasted tokens. ``stale`` actions are excluded entirely: bot comments
    anchored to superseded code aren't actionable for the resumed model
    (their cited code isn't in the worktree snapshot). The "Files
    changed since round N-1" line is the diff between the prior round's
    head_after (or head_before if never finalized) and ``head_before``
    (this round's HEAD). Caller passes ``head_before`` explicitly
    because the SKILL computes the goal text before
    ``state_append_round`` records this round's head_before.
    """
    if round_ < 2 or not state:
        return ""
    rounds = state.get("rounds") or []
    prior = [r for r in rounds if (r.get("n") or 0) < round_]
    if not prior:
        return ""
    prev = max(prior, key=lambda r: r.get("n") or 0)

    fixed: list[str] = []
    skipped: list[str] = []
    for action in prev.get("comment_actions") or []:
        verb = action.get("action")
        if verb == "fixed":
            label = _action_label(action)
            if label:
                fixed.append(label)
        elif verb in ("false_positive", "wont_fix", "ack", "pre_existing", "flake"):
            # Note: ``stale`` is deliberately excluded. Stale entries are
            # bot comments anchored to superseded code that the resumed
            # model doesn't see in its worktree snapshot anyway —
            # surfacing them adds N×80 chars of bot-comment body without
            # changing model behavior. The orchestrator still records
            # them in comment_actions for the audit trail and posts
            # auto-replies; they just don't enter the goal channel.
            label = _skipped_label(action, verb)
            if label:
                skipped.append(label)

    parts: list[str] = [f"Round {round_}."]
    if fixed:
        truncated = fixed[:_ACTION_HISTORY_CAP]
        suffix = f"; ({len(fixed) - len(truncated)} more)" if len(fixed) > len(truncated) else ""
        parts.append("Prior round fixed: " + "; ".join(truncated) + suffix + ".")
    if skipped:
        truncated = skipped[:_ACTION_HISTORY_CAP]
        suffix = f"; ({len(skipped) - len(truncated)} more)" if len(skipped) > len(truncated) else ""
        parts.append("Skipped: " + "; ".join(truncated) + suffix + ".")

    if head_before:
        prev_anchor = prev.get("head_after") or prev.get("head_before")
        files = _files_changed_between(prev_anchor, head_before)
        if files:
            prev_n = prev.get("n") or "?"
            # Cap files list at a few entries; pathological churn shouldn't blow the prompt.
            shown = files[:_ACTION_HISTORY_CAP]
            tail = f" (and {len(files) - len(shown)} more)" if len(files) > len(shown) else ""
            parts.append(f"Files changed since round {prev_n}: " + ", ".join(shown) + tail + ".")

    if len(parts) == 1:  # only the "Round N." stub — nothing to say
        return ""
    return " ".join(parts)


def _truncate(s: str) -> str:
    """Cap a string at _TOPIC_CHAR_CAP chars with an ellipsis tail."""
    s = (s or "").strip()
    if len(s) <= _TOPIC_CHAR_CAP:
        return s
    return s[: _TOPIC_CHAR_CAP - 1].rstrip() + "…"


def _action_address(action: dict[str, Any]) -> str:
    """Mode-agnostic address string for an action.

    Code-mode actions carry ``path``/``line``; design-doc-mode actions
    carry ``section``/``dimension``. Pick whichever pair the row has.
    Returns empty when neither is populated (top-level findings,
    misformed rows). Centralising this here keeps the goal-text
    label functions agnostic of the source-of-truth field name.
    """
    path = action.get("path")
    line = action.get("line")
    if path:
        return f"{path}:{line}" if line is not None else f"{path}"
    section = action.get("section")
    dimension = action.get("dimension")
    if section:
        return f"{section} ({dimension})" if dimension else f"{section}"
    return ""


def _action_label(action: dict[str, Any]) -> str:
    """Format a fixed comment_actions entry for the goal text.

    Shape: ``<address> — topic`` when topic is present, bare ``<address>``
    when absent. The address is ``path:line`` for code-mode actions or
    ``section (dimension)`` for design-doc actions — see
    ``_action_address``. Source labels (codex/cursor/etc.) are
    deliberately omitted: triage routes by source but the resumed
    model treats every finding identically once it lands in the prompt.
    """
    base = _action_address(action)
    if not base:
        return ""
    topic = (action.get("topic") or "").strip()
    if topic:
        return f"{base} — {_truncate(topic)}"
    return base


def _skipped_label(action: dict[str, Any], verb: str) -> str:
    """Format a skipped action: ``<address> verb: <description>``.

    Reason takes precedence over topic when both are present — the
    reason is what the orchestrator decided, and the model needs that
    decision (and not the original finding's topic) to avoid re-arguing
    the skip. The whole description is capped at _TOPIC_CHAR_CAP so
    a long reason can't bloat the goal text. Address shape matches
    ``_action_label``.
    """
    base = _action_address(action)
    if not base:
        return ""
    description = (action.get("reason") or action.get("topic") or "").strip()
    if description:
        return f"{base} {verb}: {_truncate(description)}"
    return f"{base} {verb}"


# Streak threshold above which the goal channel injects a one-sentence
# convergence-pressure note. Two consecutive low-only rounds is the
# earliest point at which "every finding costs a round, returning zero
# is a real option" stops sounding presumptuous; at one low-only round
# it's normal noise floor, at three+ the streak rule itself fires.
_LOW_STREAK_PRESSURE_THRESHOLD = 2


def _prior_invariants_note(state: dict[str, Any] | None, round_: int) -> str:
    """Return a one-line "prior invariants" signal for the goal, or "".

    Walks the immediately-prior round's ``comment_actions`` for entries
    that carry an ``invariant`` field. Emits a single line listing each
    distinct invariant name so the resumed reviewer sees them as
    structured data, not free-form action history. The reviewer is then
    told (by the wrapper's follow-up prompt) to fold new sites into the
    existing invariant's sites[] array rather than re-flagging.

    Reads from prior round only — same scoping as the spiral guard's
    modified-hunk heuristic. A long audit shouldn't drag every old
    invariant into every turn's goal.
    """
    if not state or round_ < 2:
        return ""
    rounds = state.get("rounds") or []
    prior = [r for r in rounds if (r.get("n") or 0) < round_]
    if not prior:
        return ""
    prev = max(prior, key=lambda r: r.get("n") or 0)
    seen: list[str] = []
    for action in prev.get("comment_actions") or []:
        inv = action.get("invariant")
        if inv and inv not in seen:
            seen.append(inv)
    if not seen:
        return ""
    bulleted = "".join(f"\n- {inv}" for inv in seen)
    return (
        "Invariants named in the prior round (fold new sites into the "
        "existing finding's sites[] array, do not re-flag as separate "
        f"issues):{bulleted}"
    )


def _low_only_streak_pressure(state: dict[str, Any] | None, round_: int) -> str:
    """Return the convergence-pressure sentence when streak >= 2, else "".

    Reads the immediately-prior round's ``low_only_streak`` rather than
    walking history — the counter is finalize-time persistent. Falls back
    to reconstructing the streak from ``top_severity`` history when the
    field is missing (state file from before low_only_streak existed) so
    upgraded mid-loop runs don't lose streak continuity. The sentence is
    appended to whatever ``goal_for_round`` produces, so triage routing
    and action-history are unaffected.

    Wording is fixed: it states a fact ("the last N rounds returned only
    low-severity findings") and a frame ("every finding costs a round")
    rather than prescribing a verdict. Per the skill's framing: don't
    encode a table of phrasings keyed to streak length.
    """
    if not state or round_ < 2:
        return ""
    rounds = state.get("rounds") or []
    prior = [r for r in rounds if (r.get("n") or 0) < round_]
    if not prior:
        return ""
    prev = max(prior, key=lambda r: r.get("n") or 0)
    streak = prev.get("low_only_streak")
    if streak is None:
        # Lazy import to avoid pulling pr_ops at module load (pr_ops
        # already lazy-imports bramble_ops; the inverse is fine when
        # gated to one fallback path).
        from pr_ops import _backfill_low_only_streak  # noqa: PLC0415

        streak = _backfill_low_only_streak(prior)
    if streak < _LOW_STREAK_PRESSURE_THRESHOLD:
        return ""
    return (
        f"The last {streak} rounds returned only low-severity findings. "
        "The fixer treats your output as authoritative, and every finding "
        "costs a round; if the diff has no structural issue, returning "
        "zero findings is the right call."
    )


def goal_for_round(
    round_: int,
    pr_summary: str,
    state: dict[str, Any] | None,
    *,
    head_before: str | None = None,
    is_new_series: bool | None = None,
    include_round_diff: bool = True,
    round_diff_max_lines: int = _ROUND_DIFF_DEFAULT_MAX_LINES,
) -> str:
    """Return the ``--goal`` text bramble should see for this round.

    Round 1: PR_SUMMARY (commit list + diffstat).

    Round 2+: per-turn action-history briefing built by
    ``action_history_goal``. Falls back to PR_SUMMARY when there's
    nothing to say (e.g. round 1 produced an empty action plan).

    Round 2+ also gets:
      - When the prior round's ``low_only_streak`` is >= 2, a one-line
        convergence-pressure sentence appended (B1).
      - When ``include_round_diff`` is True and the prior round's
        ``head_after`` is reachable, the diff between the prior round's
        HEAD and this round's HEAD is appended under a "Diff since
        round N-1" header (D1). Truncated at ``round_diff_max_lines``.

    ``head_before`` is this round's HEAD, used to compute the
    files-changed-since-prior-round line and the round diff.

    ``is_new_series`` is the orchestrator's series-boundary decision
    captured at Step 0.5 (before ``state_append_round`` clears
    ``completed: true``). When true, the round is a real round 1 from
    the model's perspective even when ``round_`` is high — the prior
    series' state is unreachable and walking it produces a noisy
    goal blob. PR_SUMMARY is the right anchor for a fresh series, and
    the streak / diff additions are skipped.
    """
    if round_ < 2 or is_new_series:
        return pr_summary
    history = action_history_goal(state, round_, head_before=head_before)
    body = history or pr_summary
    parts = [body]
    invariants = _prior_invariants_note(state, round_)
    if invariants:
        parts.append(invariants)
    pressure = _low_only_streak_pressure(state, round_)
    if pressure:
        parts.append(pressure)
    if include_round_diff:
        diff_text = round_diff(
            state, round_, head_before=head_before, max_lines=round_diff_max_lines
        )
        if diff_text:
            rounds = state.get("rounds") or []
            prior = [r for r in rounds if (r.get("n") or 0) < round_]
            prev_n = max((r.get("n") or 0) for r in prior) if prior else round_ - 1
            parts.append(
                f"Diff since round {prev_n} (truncated at {round_diff_max_lines} lines):\n{diff_text}"
            )
    return "\n\n".join(parts)


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


def parse_stream(stream_path: Path, *, source: str, fallback_mode: str = REVIEW_MODE_CODE) -> list[dict[str, Any]]:
    """Read Monitor's captured stdout (or a standalone envelope file) and return findings.

    Tries whole-file ``json.loads`` first so producers that write a single
    pretty-printed envelope (e.g. ``lint_gate.py`` via ``atomic_write_json``
    with ``indent=2``) parse correctly. Falls back to the NDJSON line-scan for
    real Monitor streams (progress lines + a final envelope line). If neither
    yields an envelope, synthesize a high-severity ``bramble-empty-envelope``
    finding so triage surfaces the failure instead of treating it as
    "converged to zero".

    ``fallback_mode`` tags the synthetic empty-envelope finding with the
    review mode the caller's other backends are running. Without this, a
    crashed design-doc backend would emit a code-mode synthetic finding
    that triage rejects as "mixed review_mode". The orchestrator passes
    ``--mode design-doc`` (CLI) which threads through here.
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
        # No envelope means we don't know the mode either, so we trust
        # the caller's ``fallback_mode``. Without this, a crashed
        # design-doc backend would synthesize a code-mode high finding
        # that triage rejects as "mixed review_mode" — silently turning
        # a Monitor failure into "all envelopes mismatched, abort."
        finding: dict[str, Any] = {
            "source": source,
            "severity": "high",
            "message": "bramble stream ended without producing an envelope",
            "suggestion": "re-launch the Monitor arm; see bramble logs under ~/.bramble/logs/code-review/",
            "topic": "bramble-empty-envelope",
            "status": "exited-empty",
            "review_mode": fallback_mode,
        }
        if fallback_mode == REVIEW_MODE_DESIGN_DOC:
            finding["section"] = None
            finding["dimension"] = None
        else:
            finding["file"] = None
            finding["line"] = None
        return [finding]
    return parse_envelope(env, source=source)


# ---------------------------------------------------------------------------
# parse: envelope -> findings
# ---------------------------------------------------------------------------


def parse_envelope(obj: dict[str, Any] | None, *, source: str) -> list[dict[str, Any]]:
    """Extract findings from one bramble envelope dict. Pure — used in tests.

    The returned findings carry a ``review_mode`` field (``"code"`` or
    ``"design-doc"``) read from the envelope's top-level
    ``review_mode``. Pre-mode envelopes have no such field and are
    treated as code mode. The mode propagates to ``triage`` via the
    findings themselves so a single triage call can mix-and-match
    backends as long as they all reported the same mode (the CLI
    ``--mode`` flag below enforces consistency or auto-detects).

    Mode-specific addressing: code mode emits ``file``/``line``;
    design-doc mode emits ``section``/``dimension``. Both shapes share
    ``severity``/``message``/``suggestion``/``topic``/``source``, which
    is what every triage code path actually keys on.
    """
    if obj is None:
        return []
    mode = obj.get("review_mode") or REVIEW_MODE_CODE
    status = obj.get("status")
    if status != "ok":
        # A failed bramble run is a real signal — the orchestrator must
        # surface it, not silently drop it. ``high`` routes through
        # ``single_critical`` → ``must_fix``; ``severity: None`` here
        # would land in ``low_acks`` (the routing rule is "missing
        # severity is treated as low/nit"), letting Monitor failures
        # masquerade as batch-ackable nits.
        msg = obj.get("error") or "bramble run failed"
        finding: dict[str, Any] = {
            "source": source,
            "severity": "high",
            "message": msg,
            "suggestion": None,
            "topic": topic_of(msg),
            "status": status,
            "review_mode": mode,
        }
        # Tag with the addressing-field shape the mode expects so later
        # triage's _consensus_key/_triage_key returns a stable
        # (None, None, ...) tuple rather than KeyError-shaped output.
        if mode == REVIEW_MODE_DESIGN_DOC:
            finding["section"] = None
            finding["dimension"] = None
        else:
            finding["file"] = None
            finding["line"] = None
        return [finding]
    issues = (obj.get("review") or {}).get("issues") or []
    out = []
    for i in issues:
        msg = i.get("message") or ""
        # v2 schema: a code-mode issue may carry an "invariant" + "sites"
        # array when the reviewer found N sibling sites of one class-level
        # rule. We expand to N findings sharing the same invariant and
        # topic so the existing (file, line) consensus and per-site fix
        # routing keeps working unchanged — the orchestrator sees N
        # actionable rows, each labeled with the invariant they belong to.
        # File/line at the top of the issue is the representative site
        # (validated to match one entry in sites[]); we don't double-emit
        # it. Design-doc mode doesn't carry sites[].
        invariant = i.get("invariant") or None
        sites = i.get("sites") or []
        topic = topic_of(msg)

        def _build_base() -> dict[str, Any]:
            base: dict[str, Any] = {
                "source": source,
                "severity": i.get("severity"),
                "message": msg,
                "suggestion": i.get("suggestion"),
                "topic": topic,
                "review_mode": mode,
            }
            if invariant:
                base["invariant"] = invariant
            return base

        if mode == REVIEW_MODE_DESIGN_DOC:
            finding = _build_base()
            finding["section"] = i.get("section")
            finding["dimension"] = i.get("dimension")
            out.append(finding)
            continue

        if sites:
            # Expand sites[] to one finding per site. The validator
            # guarantees file/line at the top matches one entry, so the
            # representative-site row is already covered by the loop.
            for site in sites:
                finding = _build_base()
                finding["file"] = site.get("file")
                finding["line"] = site.get("line")
                note = site.get("note")
                if note:
                    finding["site_note"] = note
                out.append(finding)
        else:
            finding = _build_base()
            finding["file"] = i.get("file")
            finding["line"] = i.get("line")
            out.append(finding)
    return out


def parse_sufficiency(obj: dict[str, Any] | None) -> dict[str, Any] | None:
    """Return the reviewer's per-turn sufficiency claim, or None.

    Reviewer schema v2 lets the model emit a top-level
    ``review.sufficiency`` object with ``is_confident_complete`` (bool)
    and ``evidence`` (string). It's an audit-trail signal — the
    orchestrator surfaces it in round summaries and the final report but
    does NOT use it as a new exit gate. Absence means no signal; do not
    synthesize a default.
    """
    if obj is None:
        return None
    if (obj.get("status") or "") != "ok":
        return None
    suff = (obj.get("review") or {}).get("sufficiency")
    if not isinstance(suff, dict):
        return None
    if "is_confident_complete" not in suff:
        return None
    return {
        "is_confident_complete": bool(suff.get("is_confident_complete")),
        "evidence": suff.get("evidence") or "",
    }


def parse_round(
    streams: dict[str, Path],
    backends: list[str] | None = None,
    *,
    fallback_mode: str = REVIEW_MODE_CODE,
) -> list[dict[str, Any]]:
    """Aggregate findings across backends for one pr-polish round.

    ``streams`` maps backend name to the Monitor-captured stream file for
    that backend. Backends not in the mapping are silently skipped (the
    backend simply wasn't enabled). Backends in the mapping whose path
    doesn't exist yield a synthetic high-severity ``stream-missing``
    finding so a typo'd ``--stream cursor=/typo/path`` surfaces in
    triage instead of disappearing.

    ``fallback_mode`` tags the synthetic stream-missing and empty-
    envelope findings (the latter via ``parse_stream``) with the review
    mode the caller's other envelopes are running. Mixing a code-mode
    synthetic into a design-doc batch (or vice versa) would otherwise
    trip triage's mixed-mode guard.
    """
    backends = backends or list(BACKENDS)
    out: list[dict[str, Any]] = []
    for b in backends:
        path = streams.get(b)
        if path is None:
            continue
        if not path.exists():
            finding: dict[str, Any] = {
                "source": b,
                "severity": "high",
                "message": f"--stream {b}={path} does not exist on disk",
                "suggestion": "verify the Monitor capture path; check stderr for the failed bramble run",
                "topic": "stream-missing",
                "status": "missing",
                "review_mode": fallback_mode,
            }
            if fallback_mode == REVIEW_MODE_DESIGN_DOC:
                finding["section"] = None
                finding["dimension"] = None
            else:
                finding["file"] = None
                finding["line"] = None
            out.append(finding)
            continue
        out.extend(parse_stream(path, source=b, fallback_mode=fallback_mode))
    return out


# ---------------------------------------------------------------------------
# triage: consensus + N+1 diff spiral detection
# ---------------------------------------------------------------------------


# ---------------------------------------------------------------------------
# Per-mode key construction
# ---------------------------------------------------------------------------
#
# Code review and design-doc review use different *addressing* schemes for
# findings: code review keys off (file, line) — a precise textual address
# the model was asked to cite — while design-doc review keys off
# (section, dimension) — a heading + the rubric question the issue
# answers. The triage logic (consensus, spiral guard, single-source
# bucketing, cluster_hint) is otherwise mode-agnostic, so we abstract the
# key construction into a per-mode dispatch table and parameterise the
# triage internals on a ``mode`` argument with a code-default for
# backward compat.
#
# Adding a future mode (e.g. security-review) becomes one new entry in
# this map plus matching adapter wiring, without touching the triage
# pipeline itself.

def _normalize_mode(mode: str | None) -> str:
    if not mode:
        return REVIEW_MODE_CODE
    if mode not in REVIEW_MODES:
        raise ValueError(f"unknown review mode {mode!r}; want one of {REVIEW_MODES}")
    return mode


def _triage_key(f: dict[str, Any], mode: str = REVIEW_MODE_CODE) -> tuple:
    """Spiral-detection key. Includes topic so a finding that was fixed and
    later re-flagged with the same wording matches the prior-round entry,
    while a different topic on the same site is treated as a new issue.

    Code mode keys on ``(file, line, topic)``. Design-doc mode keys on
    ``(section, dimension, topic)`` — the section heading is the durable
    address of a doc finding and the rubric dimension distinguishes
    "milestone 2 is wrong on long-term fit" from "milestone 2 is wrong
    on risk frontloading".
    """
    if mode == REVIEW_MODE_DESIGN_DOC:
        return (f.get("section"), f.get("dimension"), f.get("topic"))
    return (f.get("file"), f.get("line"), f.get("topic"))


def _consensus_key(f: dict[str, Any], mode: str = REVIEW_MODE_CODE) -> tuple:
    """Consensus-grouping key. Drops topic so two reviewers wording the same
    issue differently still collapse into one consensus entry. Two unrelated
    findings that happen to land on the same site will also collapse — but
    that's much rarer than the false-negative case (codex says
    "TestEmitEarlyFailure does not assert resume_status=unverified", cursor
    says "TestEmitEarlyFailure does not set resumeSessionID", same finding,
    same line, prior code keyed on topic and routed both to single_medium
    instead of must_fix consensus).

    Code mode: ``(file, line)``.
    Design-doc mode: ``(section, dimension)`` — same insight, the rubric
    dimension partitions two unrelated systemic issues that landed on the
    same heading.
    """
    if mode == REVIEW_MODE_DESIGN_DOC:
        return (f.get("section"), f.get("dimension"))
    return (f.get("file"), f.get("line"))


def _has_address(ckey: tuple, mode: str) -> bool:
    """Return True when the consensus key carries an actual address (not
    all-Nones). Used to skip top-level / sourceless findings during
    location-based consensus grouping; those still pair up via
    ``_triage_key`` in the second pass.
    """
    if mode == REVIEW_MODE_DESIGN_DOC:
        return ckey[0] is not None
    return ckey[0] is not None


def _cluster_field(mode: str) -> str:
    """Return the field name used to bucket findings in cluster_hint.
    Code mode buckets by file (the "this module has six findings" hint);
    design-doc mode buckets by section (the "this milestone has six
    findings" hint).
    """
    return "section" if mode == REVIEW_MODE_DESIGN_DOC else "file"


_HIGH_SEVERITY_KEYWORDS = (
    "critical",
    "must fix",
    "must-fix",
    "security",
    "vulnerab",
    "crash",
    "data loss",
)


# Window around the cited line searched for evidence of a spiral candidate.
# ±10 lines tolerates small drift from a fix that nudged surrounding lines
# without removing the cited code; bigger windows would risk false positives
# on long files where another instance of the same identifier is unrelated.
_SPIRAL_EVIDENCE_WINDOW = 10

# Minimum length of a quoted phrase or identifier extracted from the
# finding's message before we accept it as evidence. 8 chars rules out
# common stop-words (like "function", "missing") that appear everywhere.
_SPIRAL_EVIDENCE_MIN_LEN = 8


def _evidence_tokens(text: str) -> list[str]:
    """Extract candidate substrings from a finding's message that, if
    present near the cited line, count as the cited evidence still being
    at HEAD.

    Returns a deduplicated list of: any backtick/quote-quoted phrase ≥
    _SPIRAL_EVIDENCE_MIN_LEN chars, plus any bare identifier-like token
    (Letters / digits / underscore / dot, ≥ _SPIRAL_EVIDENCE_MIN_LEN
    chars). Conservative — we'd rather miss a token and let the
    multi-source spiral fallback escalate than coin a false-evidence
    match from a generic English word.
    """
    if not text:
        return []
    out: list[str] = []
    seen: set[str] = set()

    def _add(tok: str) -> None:
        s = (tok or "").strip().lower()
        if len(s) < _SPIRAL_EVIDENCE_MIN_LEN:
            return
        if s in seen:
            return
        seen.add(s)
        out.append(s)

    # Quoted/back-ticked phrases first — reviewers typically quote the
    # specific code they want changed, so these are the strongest signal.
    for m in re.findall(r"`([^`]{%d,})`" % _SPIRAL_EVIDENCE_MIN_LEN, text):
        _add(m)
    for m in re.findall(r'"([^"]{%d,})"' % _SPIRAL_EVIDENCE_MIN_LEN, text):
        _add(m)
    for m in re.findall(r"'([^']{%d,})'" % _SPIRAL_EVIDENCE_MIN_LEN, text):
        _add(m)
    # Identifier-shaped tokens (camelCase, snake_case, dotted names).
    for m in re.findall(r"[A-Za-z_][A-Za-z0-9_.]{%d,}" % (_SPIRAL_EVIDENCE_MIN_LEN - 1), text):
        _add(m)
    return out


def _spiral_evidence_present(
    finding: dict[str, Any],
    head: Path | None = None,
    *,
    window: int = _SPIRAL_EVIDENCE_WINDOW,
) -> bool:
    """True when the finding's cited evidence is still at HEAD.

    ``head`` is the worktree root (defaults to CWD). The check is:

        for each token derived from finding.message + finding.suggestion,
            if any token (lowercased, whitespace-collapsed) appears in
            <path> at lines [line-window .. line+window] (inclusive),
            the evidence is present.

    Returns True (conservative — keep the spiral escalation) when the
    file can't be read, the finding has no addressable file/line, or
    no tokens of sufficient length could be extracted. Callers
    interpret False as "definitely-absent enough to demote", and a
    permissive default protects against silently auto-demoting real
    regressions when the heuristic can't say either way.
    """
    path = finding.get("file") or finding.get("path")
    line = finding.get("line")
    if not path or line is None:
        return True
    try:
        line = int(line)
    except (TypeError, ValueError):
        return True
    root = head or Path(".")
    file_path = root / path
    try:
        text = file_path.read_text(errors="replace")
    except (OSError, UnicodeDecodeError):
        return True
    lines = text.splitlines()
    if not lines:
        return True
    # Convert to 0-based and clamp to file extent.
    start = max(0, line - 1 - window)
    end = min(len(lines), line + window)  # exclusive
    window_text = "\n".join(lines[start:end]).lower()
    # Collapse whitespace so a multi-line quoted phrase still matches when
    # the file's wrapping changed.
    window_norm = re.sub(r"\s+", " ", window_text)
    tokens = _evidence_tokens(
        " ".join(
            s for s in (finding.get("message"), finding.get("suggestion")) if s
        )
    )
    if not tokens:
        return True
    for tok in tokens:
        norm = re.sub(r"\s+", " ", tok)
        if norm in window_norm:
            return True
    return False


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


def _cluster_hint(items: list[dict[str, Any]], mode: str = REVIEW_MODE_CODE) -> list[dict[str, Any]]:
    """Group action-plan items by location bucket. Buckets with >=2
    actionable items become sweep candidates the fixer should treat as
    one task — most defensive cascades come from finding only the first
    site of a class-level issue and missing the rest in the same area.

    Code mode buckets by file (".../module.go has six findings"); design-
    doc mode buckets by section (the milestone heading concentrates the
    issue). The bucket field name is dictated by ``_cluster_field(mode)``.

    Accepts a heterogeneous list mixing raw findings, single-finding
    wrappers (``{finding: ...}``), and consensus wrappers
    (``{findings: [...]}``). One consensus entry counts as one item —
    both reviewers spotted the same site.

    Returns ``{<bucket-field>, count, lines, topics}`` entries sorted by
    count desc, bucket name asc. Single-item buckets are omitted. Items
    without a bucket address are dropped.
    """
    def _unwrap(it: dict[str, Any]) -> dict[str, Any]:
        if "finding" in it and isinstance(it["finding"], dict):
            return it["finding"]
        if "findings" in it and isinstance(it["findings"], list) and it["findings"]:
            return it["findings"][0]
        return it

    bucket_field = _cluster_field(mode)
    by_bucket: dict[str, dict[str, Any]] = {}
    for raw in items:
        f = _unwrap(raw)
        path = f.get(bucket_field)
        if not path:
            continue
        bucket = by_bucket.setdefault(
            path, {bucket_field: path, "count": 0, "lines": [], "topics": []},
        )
        bucket["count"] += 1
        # Code mode tracks line numbers; design-doc mode tracks
        # dimensions (which rubric question concentrated here).
        if mode == REVIEW_MODE_DESIGN_DOC:
            dim = f.get("dimension")
            if dim is not None:
                bucket["lines"].append(dim)
        else:
            line = f.get("line")
            if line is not None:
                bucket["lines"].append(line)
        topic = f.get("topic")
        if topic:
            bucket["topics"].append(topic)
    clusters = [b for b in by_bucket.values() if b["count"] >= 2]
    clusters.sort(key=lambda b: (-b["count"], b[bucket_field]))
    return clusters


def triage(
    findings: list[dict[str, Any]],
    prior_fixed_keys: set[tuple],
    *,
    pr_comments: list[dict[str, Any]] | None = None,
    ci_failures: list[dict[str, Any]] | None = None,
    mode: str | None = None,
    head_path: Path | None = None,
    prior_modified_hunks: dict[str, list[tuple[int, int]]] | None = None,
) -> dict[str, Any]:
    """Group findings, surface consensus, detect N+1 spiral matches.

    Pure — used directly in tests.

    Two-level keying (per-mode addressing fields, see _consensus_key /
    _triage_key):

    - Code mode: ``_consensus_key`` = ``(file, line)``; ``_triage_key``
      = ``(file, line, topic)``.
    - Design-doc mode: ``_consensus_key`` = ``(section, dimension)``;
      ``_triage_key`` = ``(section, dimension, topic)``.

    The grouping logic itself is mode-agnostic: ``_consensus_key`` drives
    cross-source consensus so two reviewers wording the same finding
    differently still consolidate; ``_triage_key`` drives the N+1
    spiral guard where exact recurrence matters.

    ``pr_comments`` and ``ci_failures`` are code-mode-only inputs; they
    are converted via ``pr_comment_to_finding`` / ``ci_failure_to_finding``
    (which always produce code-shaped findings). Passing them in
    design-doc mode is rejected — those signals don't apply to a doc.

    ``mode`` defaults to whatever the findings themselves carry in
    ``review_mode``. When the findings are a mix (e.g. a PR with one
    code-mode and one design-doc envelope, which shouldn't happen but
    might), the code-mode default wins and a ValueError is raised so
    the caller doesn't silently key one half on the wrong fields.
    """
    all_findings = list(findings)
    if pr_comments:
        all_findings.extend(pr_comment_to_finding(c) for c in pr_comments)
    if ci_failures:
        all_findings.extend(ci_failure_to_finding(f) for f in ci_failures)

    # Mode resolution. Three sources of truth in priority order:
    #   1. Explicit ``mode`` argument (CLI flag, test override).
    #   2. ``review_mode`` carried on the findings themselves.
    #   3. Default to code mode for backward-compat.
    # Mismatch between sources is a hard error so misrouted envelopes
    # fail loud.
    finding_modes = {f.get("review_mode") for f in all_findings if f.get("review_mode")}
    if mode is not None:
        resolved_mode = _normalize_mode(mode)
        if finding_modes and resolved_mode not in finding_modes:
            raise ValueError(
                f"explicit mode {resolved_mode!r} doesn't match envelope modes {sorted(finding_modes)!r}"
            )
    elif len(finding_modes) > 1:
        raise ValueError(f"findings carry mixed review_mode values: {sorted(finding_modes)!r}")
    elif finding_modes:
        resolved_mode = _normalize_mode(next(iter(finding_modes)))
    else:
        resolved_mode = REVIEW_MODE_CODE
    if resolved_mode == REVIEW_MODE_DESIGN_DOC and (pr_comments or ci_failures):
        raise ValueError(
            "pr_comments / ci_failures are not supported in design-doc mode"
        )

    # Partition off stale-on-prior-commit PR comments before key grouping. They
    # were posted against superseded code, so they must not pair with a fresh
    # codex/cursor finding to form spurious consensus, and they must skip the
    # severity buckets entirely — the orchestrator records them as `stale` and
    # auto-replies with a "Superseded by …" note. (Code-mode only.)
    stale_prior_commit: list[dict[str, Any]] = []
    fresh_findings: list[dict[str, Any]] = []
    for f in all_findings:
        if f.get("is_stale_prior_commit"):
            stale_prior_commit.append({"key": list(_triage_key(f, resolved_mode)), "finding": f})
        else:
            fresh_findings.append(f)

    # Two-level keying: triage_key drives spiral detection and
    # single-source bucketing; consensus_key drives cross-source
    # consensus so two reviewers wording the same site differently
    # still collapse into one must_fix entry.
    by_triage_key: dict[tuple, list[dict[str, Any]]] = {}
    by_consensus_key: dict[tuple, list[dict[str, Any]]] = {}
    for f in fresh_findings:
        by_triage_key.setdefault(_triage_key(f, resolved_mode), []).append(f)
        by_consensus_key.setdefault(_consensus_key(f, resolved_mode), []).append(f)

    # First pass: identify groups with >=2 distinct sources at the same
    # consensus key.
    consensus: list[dict[str, Any]] = []
    consensus_triage_keys: set[tuple] = set()
    for ckey, group in by_consensus_key.items():
        if not _has_address(ckey, resolved_mode):
            # Top-level / addressless findings (PR-level comments,
            # whole-document doc findings without a section heading)
            # can't form location-based consensus. Leave them to the
            # triage_key pipeline.
            continue
        sources = {g["source"] for g in group}
        if len(sources) >= 2:
            consensus.append(
                {"key": list(ckey), "sources": sorted(sources), "findings": group}
            )
            for g in group:
                consensus_triage_keys.add(_triage_key(g, resolved_mode))

    # Invariant-tier consensus (v2 schema, code mode only). Two reviewers
    # naming the same invariant — even at different sites — are claiming
    # the same class-level rule. Reward that: route every finding sharing
    # the invariant to must_fix as one consensus row, even when no two
    # findings shared a (file, line). Codex saying "ambient env vars
    # shadow explicit proxy keys" at site A and cursor saying the same
    # invariant at site B is exactly the signal we want to fold.
    #
    # Skipped in design-doc mode: the rubric dimension already partitions
    # class-level claims there, and the invariant field is reserved for
    # code mode in the wrapper schema.
    if resolved_mode == REVIEW_MODE_CODE:
        by_invariant: dict[str, list[dict[str, Any]]] = {}
        for f in fresh_findings:
            inv = f.get("invariant")
            if not inv:
                continue
            by_invariant.setdefault(inv, []).append(f)
        for inv, group in by_invariant.items():
            sources = {g["source"] for g in group}
            if len(sources) < 2:
                # Single-source named invariant: leave it to per-site
                # dispatch. Each site already routes individually, so the
                # invariant name surfaces in the action_plan via the
                # finding's invariant field without a forced consensus
                # row.
                continue
            # Mark every site-finding's triage_key as handled so the
            # per-key loop below doesn't double-list these under
            # single_critical/medium when they have N sites.
            already_consensus = False
            for g in group:
                tk = _triage_key(g, resolved_mode)
                if tk in consensus_triage_keys:
                    already_consensus = True
                consensus_triage_keys.add(tk)
            if already_consensus:
                # Location-based pass already counted this; don't double-
                # list, but the triage_key set is now wider so per-key
                # dispatch won't re-route the additional sites.
                continue
            consensus.append({
                "key": ["invariant", inv],
                "sources": sorted(sources),
                "invariant": inv,
                "findings": group,
            })

    single_critical: list[dict[str, Any]] = []
    single_medium: list[dict[str, Any]] = []
    low_acks: list[dict[str, Any]] = []
    spiral_matches: list[dict[str, Any]] = []

    for key, group in by_triage_key.items():
        severities = [severity_rank(g.get("severity")) for g in group]
        top = max(severities) if severities else -1
        repr_ = group[0]
        # Spiral check: strict (addr0, addr1, topic) match, or
        # location-only fallback so a fix-then-rewording regression at
        # the same site still escalates even when the stored action's
        # topic string and the new finding's topic_of(message) drift
        # apart.
        location_key = (key[0], key[1], None)
        is_spiral = key in prior_fixed_keys or location_key in prior_fixed_keys
        if is_spiral:
            sources_in_group = {g.get("source") for g in group if g.get("source")}
            is_multi_source = len(sources_in_group) >= 2
            # Single-source spirals whose cited evidence isn't at HEAD
            # anymore are auto-demoted to stale_prior_commit: the prior
            # round fixed it and the resumed model is re-flagging stale
            # context. Multi-source spirals always escalate — two
            # backends agreeing the regression is real is a stronger
            # signal than a heuristic file read.
            #
            # Code mode only: design-doc spirals don't have a file/line
            # to grep, so the evidence check would always be conservative-
            # True and the demote would never fire anyway. Keep the
            # branch explicit so the heuristic doesn't quietly leak.
            if (
                resolved_mode == REVIEW_MODE_CODE
                and not is_multi_source
            ):
                # Two demote heuristics, both safe to run when the
                # finding has a (file, line):
                #   1) Evidence-at-HEAD: distinctive tokens from the
                #      finding's message no longer appear within ±10
                #      lines of the cited line.
                #   2) Modified-hunk: the cited line lies inside a hunk
                #      that any prior round of this series modified —
                #      reviewer is reading cached context against code
                #      we already moved (e.g. doc-comment fix shifted
                #      words within the same function).
                # Either signal alone is enough to demote; both are
                # noisier on multi-source spirals (real regressions
                # often coincide with prior fixes), so multi-source
                # spirals always escalate.
                evidence_absent = not _spiral_evidence_present(repr_, head=head_path)
                in_modified_hunk = bool(prior_modified_hunks) and _line_in_modified_hunk(
                    repr_, prior_modified_hunks
                )
                if evidence_absent or in_modified_hunk:
                    demoted = dict(repr_)
                    if in_modified_hunk:
                        demoted["stale_reason"] = (
                            "spiral candidate auto-demoted: cited line inside a "
                            "hunk modified by a prior round"
                        )
                    else:
                        demoted["stale_reason"] = (
                            "spiral candidate auto-demoted: cited evidence absent at HEAD"
                        )
                    stale_prior_commit.append(
                        {"key": list(_triage_key(repr_, resolved_mode)), "finding": demoted}
                    )
                    # Skip the rest of the per-key dispatch — this finding
                    # is now in batch_stale and must not also land in any
                    # severity bucket.
                    continue
            spiral_matches.append({"key": list(key), "findings": group})
        if key in consensus_triage_keys:
            # Already routed to consensus by location-based grouping;
            # don't double-list it under a single-source bucket.
            continue
        sources = {g["source"] for g in group}
        if len(sources) >= 2:
            # Same triage key (incl. topic) flagged by >=2 sources — also
            # consensus, even when location-based grouping didn't catch it
            # (e.g. addressless PR-level comments).
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
    # spiral_matches use triage keys; consensus entries may use either
    # consensus keys (two-element) or triage keys (three-element)
    # depending on which path created them. Match a consensus entry as
    # "in spiral" if any spiral key shares its location prefix.
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
        "review_mode": resolved_mode,
        "action_plan": {
            "must_fix": _without_spiral(consensus + single_critical),
            "consider_fix": _without_spiral(single_medium),
            "batch_ack": _without_spiral(low_acks),
            "batch_stale": stale_prior_commit,
            "escalate": spiral_matches,
            # Sweep candidates: locations concentrating >=2 actionable
            # findings. Read by the fixer prompt so co-located issues
            # are planned holistically rather than as N independent
            # line-level patches. See SKILL.md "A finding is a symptom".
            "cluster_hint": _cluster_hint(
                _without_spiral(consensus + single_critical + single_medium),
                resolved_mode,
            ),
        },
        # ``total`` covers all post-merge findings (bramble + pr_comments +
        # ci_failures). Reporting only ``len(findings)`` would undercount
        # comment/CI-only triage runs (zero bramble issues, populated buckets).
        "total": len(all_findings),
        "unique": len(by_triage_key),
    }


def prior_fixed_keys(state: dict[str, Any] | None, mode: str = REVIEW_MODE_CODE) -> set[tuple]:
    """Collect spiral-match keys for every prior-round ``fixed`` action.

    Returns both the strict triage key and a softer location-only
    fallback. Spiral detection looks for either:

    - The strict form catches exact recurrence (same wording at same site).
    - The fallback catches "fix at same site, reviewer reworded it"
      regressions where the persisted ``topic`` string and the new
      finding's ``topic_of(message)`` happen not to collide. Stored
      actions whose ``topic`` is missing or got rewritten by a human
      auditor would otherwise silently disable spiral detection.

    Mode dispatches the addressing fields read from each
    ``comment_actions`` row:
    - Code mode: ``(path, line, topic)``.
    - Design-doc mode: ``(section, dimension, topic)``.

    Routing remains unchanged: callers test ``key in prior_fixed_keys``;
    we also emit the location-only companion so a new finding whose
    ``_triage_key`` of ``(addr0, addr1, None)`` (sourceless or different
    topic) still triggers escalation.
    """
    keys: set[tuple] = set()
    if not state:
        return keys
    mode = _normalize_mode(mode)
    for rnd in state.get("rounds") or []:
        for a in rnd.get("comment_actions") or []:
            if a.get("action") != "fixed":
                continue
            if mode == REVIEW_MODE_DESIGN_DOC:
                addr0, addr1 = a.get("section"), a.get("dimension")
            else:
                addr0, addr1 = a.get("path"), a.get("line")
            topic = a.get("topic")
            # Address-less fixes (review-summary acks, doc-wide
            # findings without a section) would otherwise emit
            # (None, None, ...) which matches every sourceless finding
            # ever — too broad for spiral detection. Require at least
            # the first addressing field before recording.
            if addr0 is None:
                continue
            keys.add((addr0, addr1, topic))
            keys.add((addr0, addr1, None))
    return keys


# Default: force a fresh bramble session every K rounds so accumulated
# session context can't compound staleness across a long audit. Each
# resume keeps the reviewer reading the same conversation history;
# after ~4 rounds the older turns describe code that no longer exists,
# and the model starts re-flagging stale context. K=4 is the
# round-budget cliff PR #240's session showed: r5–r10 were mostly
# chasing consequences of r1–r4 fixes. Override with --session-reset-k.
SESSION_RESET_K_DEFAULT = 4


def prior_session_id(
    state: dict[str, Any] | None,
    backend: str,
    round_: int,
    *,
    is_new_series: bool | None = None,
    session_reset_k: int = SESSION_RESET_K_DEFAULT,
) -> str:
    """Return the newest prior session id for backend before ``round_``.

    State files have evolved over time, so accept both explicit round metadata
    (``session_ids`` / ``<backend>_session_id``) and persisted raw envelopes
    under ``reviews`` when present.

    Returns ``""`` at series boundaries: a new audit (prior loop hit
    completed=true) gets a fresh bramble session rather than dragging the
    prior series' conversation context into a review of different code.

    Series detection is sticky across the round: the SKILL captures the
    decision at Step 0.5 *before* ``state_append_round`` clears the
    ``completed`` flag, then passes ``is_new_series`` here. When the
    caller doesn't pass it, fall back to deriving from the live state
    (works only when called before ``state_append_round`` runs).

    Also returns ``""`` when the candidate session id was last written
    ``session_reset_k`` rounds ago or more — long-running audits drift
    further from current code with every resume, and forcing a fresh
    session every K rounds clears accumulated stale context. Set
    ``session_reset_k=0`` to disable the periodic reset.
    """
    if not state or round_ < 2:
        return ""
    boundary = is_new_series if is_new_series is not None else _is_first_round_of_series(state, round_)
    if boundary:
        return ""
    rounds = state.get("rounds") or []
    for rnd in sorted(rounds, key=lambda r: r.get("n") or 0, reverse=True):
        n = rnd.get("n") or 0
        if n >= round_:
            continue
        session_ids = rnd.get("session_ids") or {}
        sid = session_ids.get(backend) or rnd.get(f"{backend}_session_id")
        if not sid:
            reviews = rnd.get("reviews") or {}
            env = reviews.get(backend) if isinstance(reviews, dict) else None
            if isinstance(env, dict) and env.get("session_id"):
                sid = env["session_id"]
        if sid:
            # Periodic-reset gate: forcing a fresh session every K
            # rounds clears accumulated stale context. The check is
            # round-distance, not wall-clock, so an interrupted-and-
            # resumed loop still gets the same reset cadence.
            if session_reset_k > 0 and (round_ - n) >= session_reset_k:
                return ""
            return str(sid)
    return ""


# ---------------------------------------------------------------------------
# Envelope recovery: salvage envelopes whose verdict the wrapper rejected
# ---------------------------------------------------------------------------
#
# Cursor (and occasionally codex) sometimes return verdicts the bramble
# wrapper doesn't recognize — most commonly ``approve_with_notes``,
# ``approve``, ``request_changes``, ``comment``. The wrapper writes
# ``status: "error"`` with a verdict-validation message and refuses to
# expose the issues, but ``review.issues`` is fully populated underneath.
# Throwing the round away over a vocabulary mismatch wastes a review
# cycle, so we map the variant verdicts onto the canonical ``accepted`` /
# ``rejected`` pair and re-emit a clean envelope.
#
# Recovery vocabulary (case-insensitive, substring match on the verdict
# token extracted from the error message):
#
#   approve, approve_with_notes, accept, lgtm, ship           -> accepted
#   request_changes, reject, block, needs_changes, changes    -> rejected
#
# ``comment`` is intentionally NOT mapped: a "just commenting" verdict
# carries no merge signal, and triage routes the issues regardless of
# verdict. We surface unknown verdicts as a no-op (return original path)
# so the orchestrator's existing error-envelope handling kicks in.

_RECOVERY_ACCEPTED_TOKENS = (
    "approve_with_notes",
    "approve",
    "accept",
    "lgtm",
    "ship",
)
_RECOVERY_REJECTED_TOKENS = (
    "request_changes",
    "needs_changes",
    "needs-changes",
    "reject",
    "block",
    "changes",
)
# Order matters when matching: longer/more-specific tokens must come
# first so ``approve_with_notes`` doesn't get demoted to ``approve``
# and ``request_changes`` doesn't get demoted to ``changes``.


def _classify_recovery_verdict(error_text: str) -> str | None:
    """Return ``"accepted"`` / ``"rejected"`` / ``None`` for an error message.

    Looks for verdict-validation language. Anything ambiguous (no verdict
    keyword, or a recognized non-merge verdict like ``comment``) returns
    None so the caller treats it as "no recovery applicable".

    Token matching is word-boundary aware (``\\b`` regex), not raw
    substring: bare ``token in t`` would classify ``disapprove`` →
    ``accepted`` (because ``approve`` is a substring) and ``exchanges``
    → ``rejected`` (because ``changes`` is a substring), silently
    salvaging envelopes that should stay rejected. Word boundaries
    treat both ``-`` and ``_`` as separators (so ``approve_with_notes``
    and ``needs-changes`` still match), which is what we want for
    verdict tokens that come in either spelling.
    """
    if not error_text:
        return None
    t = error_text.lower()
    if not re.search(r"\bverdict\b", t):
        return None
    for token in _RECOVERY_ACCEPTED_TOKENS:
        if re.search(rf"(?<![A-Za-z0-9]){re.escape(token)}(?![A-Za-z0-9])", t):
            return "accepted"
    for token in _RECOVERY_REJECTED_TOKENS:
        if re.search(rf"(?<![A-Za-z0-9]){re.escape(token)}(?![A-Za-z0-9])", t):
            return "rejected"
    return None


def recover_envelope(path: Path, *, suffix: str = "-recovered") -> Path:
    """Salvage an error envelope whose only problem was an unrecognized verdict.

    Returns the path of an envelope ready to feed into ``triage --stream``:

    - When the input envelope is already ``status: "ok"``, returns ``path``
      unchanged. Idempotent: re-running on a recovered envelope is a no-op.
    - When the input is ``status: "error"`` but the error message indicates
      a verdict-validation problem AND ``review.issues`` is populated,
      writes a sibling file ``<stem><suffix>.json`` with ``status: "ok"``
      and verdict remapped per ``_classify_recovery_verdict``. Returns the
      sibling path.
    - When the input is an error envelope with no recognizable verdict
      keyword, or with empty issues, returns ``path`` unchanged so the
      orchestrator falls through to the existing high-severity synthetic
      finding path.

    Pure mechanical recovery — no judgment calls about whether the issues
    "are good." If the inner JSON has issues, we trust them; if it doesn't,
    we don't synthesize anything.
    """
    if not path.exists():
        return path
    obj = read_json(path, default=None)
    if not isinstance(obj, dict):
        return path
    if obj.get("status") == "ok":
        return path
    error_text = obj.get("error") or ""
    verdict = _classify_recovery_verdict(error_text)
    if verdict is None:
        return path
    issues = ((obj.get("review") or {}).get("issues")) or []
    if not issues:
        # Vocabulary problem on an empty review = no salvageable signal.
        return path
    recovered = dict(obj)
    recovered["status"] = "ok"
    recovered["error"] = None
    review = dict(recovered.get("review") or {})
    review["verdict"] = verdict
    recovered["review"] = review
    recovered.setdefault("recovery", {})["original_error"] = error_text
    recovered["recovery"]["mapped_verdict"] = verdict
    out_path = path.with_name(f"{path.stem}{suffix}.json")
    # Atomic write — same shape as state writes; readers tolerate either
    # the original or recovered file showing up first if a concurrent
    # process is watching the directory.
    from _common import atomic_write_json  # noqa: PLC0415

    atomic_write_json(out_path, recovered)
    return out_path


# ---------------------------------------------------------------------------
# Round diff: prior_round.head_after .. this_round.HEAD
# ---------------------------------------------------------------------------


def round_diff(
    state: dict[str, Any] | None,
    round_: int,
    *,
    head_before: str | None = None,
    max_lines: int = _ROUND_DIFF_DEFAULT_MAX_LINES,
) -> str:
    """Return ``git diff <prior_head_after>..<head_before>`` text, truncated.

    Returns ``""`` when:
      - ``round_`` < 2 (no prior round to diff against),
      - state has no rounds before ``round_``,
      - the prior round's ``head_after`` is None (interrupted prior round
        that never finalized),
      - ``head_before`` is None or equals the prior anchor,
      - git rejects the range (unreachable SHAs in shallow clones / worktrees).

    Truncation appends a ``...elided N lines`` footer when the diff exceeds
    ``max_lines``. The footer is on its own line so callers can grep for
    "elided" to detect truncation.

    Pure git plumbing. Used by the goal-builder to feed the resumed
    reviewer the diff between rounds rather than re-snapshotting the
    whole PR.
    """
    if round_ < 2 or not state:
        return ""
    rounds = state.get("rounds") or []
    prior = [r for r in rounds if (r.get("n") or 0) < round_]
    if not prior:
        return ""
    prev = max(prior, key=lambda r: r.get("n") or 0)
    prev_anchor = prev.get("head_after")
    if not prev_anchor or not head_before or prev_anchor == head_before:
        return ""
    from _common import run  # noqa: PLC0415

    try:
        res = run(
            ["git", "diff", f"{prev_anchor}..{head_before}"],
            check=False,
        )
    except Exception as e:  # noqa: BLE001 — best-effort
        print(
            f"bramble_ops: round-diff git diff {prev_anchor[:7]}..{head_before[:7]} "
            f"failed: {e}",
            file=sys.stderr,
        )
        return ""
    if res.returncode != 0:
        print(
            f"bramble_ops: round-diff git diff {prev_anchor[:7]}..{head_before[:7]} "
            f"returned {res.returncode}; cited SHA may be unreachable. "
            f"stderr: {res.stderr.strip() or '(empty)'}",
            file=sys.stderr,
        )
        return ""
    text = res.stdout
    if not text.strip():
        return ""
    lines = text.splitlines()
    if len(lines) > max_lines:
        elided = len(lines) - max_lines
        return "\n".join(lines[:max_lines]) + f"\n...elided {elided} lines"
    return text


# ---------------------------------------------------------------------------
# Modified-hunk lookup for spiral-stale demote (E1)
# ---------------------------------------------------------------------------


_HUNK_HEADER_RE = re.compile(
    r"^@@\s+-\d+(?:,\d+)?\s+\+(?P<start>\d+)(?:,(?P<count>\d+))?\s+@@"
)


def _parse_hunk_ranges(diff_text: str) -> list[tuple[int, int]]:
    """Extract ``(start, end)`` line ranges (inclusive, 1-based, on the +
    side) from a unified-diff body. Used to test whether a cited line
    falls inside a hunk a prior round modified.

    A hunk header ``@@ -a,b +c,d @@`` covers lines ``[c, c+d-1]``. When the
    count is omitted (``@@ -a +c @@``), it defaults to 1 per unified-diff
    spec. A pure deletion (``+c,0``) yields no covered lines on the +
    side; we drop those (caller can't have a spiral on a line that
    doesn't exist post-fix).
    """
    ranges: list[tuple[int, int]] = []
    for line in diff_text.splitlines():
        m = _HUNK_HEADER_RE.match(line)
        if not m:
            continue
        start = int(m.group("start"))
        count_str = m.group("count")
        count = int(count_str) if count_str is not None else 1
        if count <= 0:
            continue
        ranges.append((start, start + count - 1))
    return ranges


def prior_round_modified_hunks(
    state: dict[str, Any] | None,
    *,
    only_round: int | None = None,
) -> dict[str, list[tuple[int, int]]]:
    """Map each file path to ``[(start, end), ...]`` line ranges modified by
    a prior round's commit (``head_before..head_after``).

    With ``only_round=N``, only that round's hunks are returned. Default
    behavior unions across all prior rounds. Triage passes the
    immediately-prior round (current_round - 1) so the demote heuristic
    only fires on findings that landed inside the most recent fix's
    edited region — older rounds' edits are part of the stable
    background and a finding there is more likely a real lingering bug
    than stale session context. Without this narrowing, a long audit
    accumulates touched ranges and starts demoting real regressions
    just because a long-ago round happened to edit the same file.

    Returns ``{}`` when state is missing, or no matching round has both
    head_before and head_after. Pure git plumbing — runs
    ``git diff -U0 <head_before>..<head_after>`` once per matching
    round, parses hunk headers, unions ranges per file.
    """
    if not state:
        return {}
    from _common import run  # noqa: PLC0415

    out: dict[str, list[tuple[int, int]]] = {}
    rounds = state.get("rounds") or []
    for rnd in rounds:
        if only_round is not None and (rnd.get("n") or 0) != only_round:
            continue
        head_before = rnd.get("head_before")
        head_after = rnd.get("head_after")
        if not head_before or not head_after or head_before == head_after:
            continue
        try:
            res = run(
                ["git", "diff", "-U0", f"{head_before}..{head_after}"],
                check=False,
            )
        except Exception as e:  # noqa: BLE001
            print(
                f"bramble_ops: prior-round-modified-hunks git diff "
                f"{head_before[:7]}..{head_after[:7]} failed: {e}",
                file=sys.stderr,
            )
            continue
        if res.returncode != 0:
            # Unreachable SHAs (shallow clone / pruned worktree) — skip,
            # don't mask other prior rounds.
            continue
        # Walk the diff splitting on file headers. A unified diff lists
        # each file with ``diff --git`` then ``+++ b/<path>`` then hunk
        # headers; the ``+++`` line gives us the post-fix path which is
        # what triage's findings are addressed against.
        current_path: str | None = None
        for line in res.stdout.splitlines():
            if line.startswith("+++ "):
                # ``+++ b/path/to/file`` or ``+++ /dev/null`` for deletions
                addr = line[len("+++ ") :].strip()
                if addr == "/dev/null":
                    current_path = None
                elif addr.startswith("b/"):
                    current_path = addr[2:]
                else:
                    current_path = addr
                continue
            m = _HUNK_HEADER_RE.match(line)
            if not m or not current_path:
                continue
            start = int(m.group("start"))
            count_str = m.group("count")
            count = int(count_str) if count_str is not None else 1
            if count <= 0:
                continue
            out.setdefault(current_path, []).append((start, start + count - 1))
    return out


def _line_in_modified_hunk(
    finding: dict[str, Any],
    modified_hunks: dict[str, list[tuple[int, int]]],
) -> bool:
    """Return True when finding's cited (file, line) falls inside any range
    in ``modified_hunks``. Returns False for findings with no addressable
    line (top-level / sourceless) — those don't qualify for the
    modified-hunk demote heuristic.
    """
    path = finding.get("file") or finding.get("path")
    line = finding.get("line")
    if not path or line is None:
        return False
    try:
        line = int(line)
    except (TypeError, ValueError):
        return False
    ranges = modified_hunks.get(path)
    if not ranges:
        return False
    return any(start <= line <= end for start, end in ranges)


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
    sp.add_argument(
        "--head-before",
        help="This round's HEAD; used to compute the files-changed-since-prior-round line.",
    )
    sp.add_argument(
        "--is-new-series",
        choices=["0", "1"],
        help=(
            "Captured at Step 0.5 before state_append_round clears "
            "completed=true. When 1, return PR_SUMMARY regardless of "
            "round number — round N of a fresh series is a real round 1 "
            "from the model's perspective, and walking the prior series' "
            "head_after produces a noisy goal blob (the SHA may be unreachable)."
        ),
    )
    sp.add_argument(
        "--no-round-diff",
        dest="include_round_diff",
        action="store_false",
        default=True,
        help="Skip the inter-round diff append (default: include).",
    )
    sp.add_argument(
        "--round-diff-max-lines",
        type=int,
        default=_ROUND_DIFF_DEFAULT_MAX_LINES,
    )

    sp = sub.add_parser(
        "prior-session-id",
        help="Print the resume session id for a backend at round N (empty if none).",
    )
    sp.add_argument("backend", choices=BACKENDS)
    sp.add_argument("round_", type=int)
    sp.add_argument("--state-file", required=True)
    sp.add_argument(
        "--is-new-series",
        choices=["0", "1"],
        help="Captured at Step 0.5 before state_append_round mutates state; pass 1 to force empty.",
    )
    sp.add_argument(
        "--session-reset-k",
        type=int,
        default=SESSION_RESET_K_DEFAULT,
        help=(
            "Force a fresh session every K rounds to clear accumulated stale "
            f"context (default {SESSION_RESET_K_DEFAULT}). Set to 0 to disable."
        ),
    )

    sp = sub.add_parser(
        "recover-envelope",
        help=(
            "Salvage a status:error envelope whose only problem was an "
            "unrecognized verdict (approve_with_notes, request_changes, etc). "
            "Prints the recovered path on stdout, or the original path if no "
            "recovery applied (idempotent — safe to wrap every --stream)."
        ),
    )
    sp.add_argument("envelope_path")

    sp = sub.add_parser(
        "round-diff",
        help=(
            "Print git diff <prior_round.head_after>..<head_before> for "
            "round N, truncated at --max-lines. Empty when no prior round "
            "or SHAs are unreachable. Pure git plumbing — used by the "
            "goal builder to feed the resumed reviewer the inter-round diff."
        ),
    )
    sp.add_argument("state_file")
    sp.add_argument("round_", type=int)
    sp.add_argument(
        "--head-before",
        help="This round's HEAD; if omitted, falls back to git rev-parse HEAD.",
    )
    sp.add_argument(
        "--max-lines",
        type=int,
        default=_ROUND_DIFF_DEFAULT_MAX_LINES,
    )

    sp = sub.add_parser(
        "parse-stream",
        help="Parse a Monitor stdout capture and emit findings for one backend.",
    )
    sp.add_argument("stream_file")
    sp.add_argument("--backend", required=True, choices=BACKENDS)

    sp = sub.add_parser("triage")
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
        help="JSON file with classify_comments output (round 1 input). Code mode only.",
    )
    sp.add_argument(
        "--ci-failures",
        metavar="FILE",
        help="JSON file with ci_failed_tests output. Code mode only.",
    )
    sp.add_argument(
        "--mode",
        choices=REVIEW_MODES,
        help="Review mode override. Default: read from envelopes (each "
             "envelope's review_mode field), falling back to code. Pass "
             "explicitly to assert a mode and reject mismatched envelopes.",
    )
    sp.add_argument(
        "--head-path",
        help=(
            "Worktree root used to grep for spiral-candidate evidence at HEAD. "
            "Defaults to CWD. Single-source spirals whose cited evidence is "
            "absent within ±10 lines of the cited line auto-demote to "
            "batch_stale; multi-source spirals always escalate."
        ),
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
            is_new = (args.is_new_series == "1") if args.is_new_series is not None else None
            print(
                goal_for_round(
                    args.round_,
                    args.pr_summary,
                    state,
                    head_before=args.head_before,
                    is_new_series=is_new,
                    include_round_diff=args.include_round_diff,
                    round_diff_max_lines=args.round_diff_max_lines,
                )
            )
        elif args.cmd == "prior-session-id":
            state = read_json(Path(args.state_file), default=None)
            is_new = (args.is_new_series == "1") if args.is_new_series is not None else None
            print(
                prior_session_id(
                    state,
                    args.backend,
                    args.round_,
                    is_new_series=is_new,
                    session_reset_k=args.session_reset_k,
                )
            )
        elif args.cmd == "recover-envelope":
            print(str(recover_envelope(Path(args.envelope_path))))
        elif args.cmd == "round-diff":
            state = read_json(Path(args.state_file), default=None)
            head_before = args.head_before
            if head_before is None:
                from _common import run as _run  # noqa: PLC0415

                res = _run(["git", "rev-parse", "HEAD"], check=False)
                if res.returncode == 0:
                    head_before = res.stdout.strip() or None
            print(
                round_diff(
                    state,
                    args.round_,
                    head_before=head_before,
                    max_lines=args.max_lines,
                )
            )
        elif args.cmd == "parse-stream":
            findings = parse_stream(Path(args.stream_file), source=args.backend)
            print_json(findings)
        elif args.cmd == "triage":
            streams = _parse_stream_args(args.stream)
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
            # Mode resolution decides which addressing fields synthetic
            # findings (stream-missing, empty-envelope) carry. Order of
            # preference:
            #   1. Explicit --mode flag.
            #   2. Mode read off real envelopes (do a code-default parse
            #      first, then look at the non-synthetic findings).
            #   3. Code default.
            # Without #2, a design-doc orchestrator that omits --mode
            # would get code-mode synthetics that triage rejects as
            # "mixed review_mode" any time a backend produces a real
            # design-doc envelope.
            if args.mode is not None:
                resolved_mode = args.mode
            else:
                preliminary = parse_round(streams, fallback_mode=REVIEW_MODE_CODE)
                real_modes = {
                    f.get("review_mode")
                    for f in preliminary
                    if f.get("review_mode") and f.get("status") not in ("missing", "exited-empty")
                }
                if len(real_modes) > 1:
                    raise ValueError(
                        f"findings carry mixed review_mode values: {sorted(real_modes)!r}; "
                        "pass --mode explicitly to disambiguate"
                    )
                resolved_mode = next(iter(real_modes), None) or REVIEW_MODE_CODE
            findings = parse_round(streams, fallback_mode=resolved_mode)
            head_path = Path(args.head_path) if args.head_path else None
            # Modified-hunk demote (E1) is code-mode-only: design-doc
            # findings address sections, not file lines, so a hunk lookup
            # would be a no-op. Skip the git work outright in that case
            # so a doc-mode triage on a worktree without remote SHAs
            # doesn't spam stderr with "unreachable SHA" warnings.
            modified_hunks: dict[str, list[tuple[int, int]]] | None = None
            if resolved_mode == REVIEW_MODE_CODE:
                # Narrow to the immediately-prior round so the demote
                # heuristic doesn't accumulate touched ranges across a
                # long audit (which would suppress real regressions on
                # any file an early round happened to touch). The
                # use case it's designed for — doc-comment fix shifts
                # words within the same function — is N→N+1, not "any
                # ancestor round".
                last_round = max(
                    (r.get("n") or 0 for r in (prior.get("rounds") or [])),
                    default=0,
                ) if prior else 0
                if last_round > 0:
                    modified_hunks = prior_round_modified_hunks(
                        prior, only_round=last_round
                    )
            result = triage(
                findings,
                prior_fixed_keys(prior, resolved_mode),
                pr_comments=pr_comments,
                ci_failures=ci_failures,
                mode=resolved_mode,
                head_path=head_path,
                prior_modified_hunks=modified_hunks,
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
