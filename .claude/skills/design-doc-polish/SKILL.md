---
name: design-doc-polish
description: Iterative review of a markdown design document. The orchestrator reads the doc, proposes a grilling rubric, asks the user to confirm, then runs N rounds of codex+cursor (optionally +gemini) against the doc. Edits the doc between rounds, commits locally each round. Never pushes.
argument-hint: "<doc-path> [--rounds N] [--gemini] [--rubric-file <path>]"
disable-model-invocation: true
---

# Design Doc Polish Loop

Drives a markdown design doc from rough draft to grilled-and-tightened. Each round runs `bramble code-review --review-mode design-doc` against the doc with a caller-supplied rubric, triages section/dimension-keyed findings, edits the doc, and commits locally. **Never pushes** — the doc may live on any branch.

The loop exits on convergence (zero findings, all low/nit, or empty action plan) or at the round cap (`--rounds N`, default `5`). Doc reviews typically converge in 2–3 rounds.

Helpers live under `scripts/`:

- `doc_ops.py` — doc identity, rubric reader, state I/O.
- The triage / envelope-parsing layer is **shared with /pr-polish**: `doc_ops.py` imports `bramble_ops.py` and `_common.py` from `.claude/skills/pr-polish/scripts/`. A bug fix to consensus / triage logic lands once and applies to both skills.

Each round runs fresh bramble sessions (no `--resume-session-id`). The rubric is in the prompt every turn so accumulated context isn't load-bearing.

## Arguments

| Flag | Default | Meaning |
|---|---|---|
| `<doc-path>` | required | Markdown design doc, must live under the repo worktree. |
| `--rounds N` | `5` | Max rounds. Doc reviews usually converge in 2–3. |
| `--gemini` | off | Add a third reviewer (`bramble … --backend gemini`). |
| `--rubric-file <path>` | off | Skip orchestrator-driven rubric proposal; use this rubric verbatim (one question per non-blank line, `#` lines are comments). User still confirms before round 1. |

## Ownership rules

- **Consensus** (≥2 reviewers at the same `(section, dimension)`): mandatory fix.
- **High/medium**: fix unless provably false positive (cite a section that refutes it).
- **Low/nit**: fix if trivial, else `wont_fix` with a one-line reason.
- **Author-input findings** (genuine open questions only the doc author knows): drop a `> **TODO**: …` blockquote in the relevant section, log as `wont_fix` with `reason: "needs author input"`. Don't invent author intent.

A finding is a symptom; the fix is the cure. Group findings by underlying systemic issue and fix at the framing level — five "milestone X is risky" findings usually mean one milestone-strategy problem, not five caveats.

## Step 0: Identify the doc

```
python3 $SKILL_DIR/scripts/doc_ops.py identify <doc-path>
```

Returns `{doc_path, doc_path_abs, doc_slug, state_dir, state_file, ctx}`. Pin `$CTX = doc:<slug>`; pin `$STATE_DIR`, `$STATE_FILE`, `$DOC_PATH` (repo-relative), `$DOC_PATH_ABS` (absolute).

The first `bramble code-review --review-mode design-doc` call (Step 2b) surfaces missing flag support cleanly; no separate probe step. The skill assumes the bramble binary in this worktree's `bazel-bin` is built; otherwise build it first or `bramble` on `$PATH` must be recent enough.

```bash
export BRAMBLE_BIN="$([ -x "$(pwd)/bazel-bin/bramble/bramble_/bramble" ] \
    && echo "$(pwd)/bazel-bin/bramble/bramble_/bramble" \
    || echo bramble)"
```

## Step 1: Rubric

The rubric is what makes this skill different from a code review. The orchestrator (you, running this skill) reads the doc itself, classifies it, proposes 3–7 grilling questions appropriate to its kind, and asks the user to confirm before round 1.

### Path A — `--rubric-file <path>` was passed

Read it with `python3 $SKILL_DIR/scripts/doc_ops.py read-rubric-file <path>` (validates the same caps bramble does: ≤20 entries, ≤500 UTF-8 bytes per line, no leading markdown control chars). Show the rubric to the user and `AskUserQuestion` to confirm or edit.

Set `RUBRIC_SOURCE="--rubric-file <path>"`.

### Path B — Orchestrator proposes

1. Read the doc directly (`Read` tool).
2. Classify it roughly: architecture / detailed-design / migration / API-contract / RFC / one-pager / other.
3. Propose 3–7 short grilling questions tailored to that kind. Defaults if you can't classify confidently:
   - "Is this the best long-term choice?"
   - "Can we make it simpler?"
   - "Does the milestone plan create clear boundaries?"
   - "Does the milestone plan frontload risk discovery?"
4. `AskUserQuestion` showing the proposed rubric, with options to accept / edit / use the 4 starter questions verbatim.

Set `RUBRIC_SOURCE="orchestrator-proposed"` or `"user-edited"` depending on what the user picked.

### Persist the rubric

Write the chosen questions to `$STATE_DIR/rubric.txt`, one per line, no leading whitespace:

```bash
mkdir -p "$STATE_DIR"
printf '%s\n' "${RUBRIC[@]}" > "$STATE_DIR/rubric.txt"
```

This file is passed via `--review-rubric-file` on every round and persisted into the state file at finalize-round.

## Step 2: Round loop

For round = 1..N:

### a) Snapshot any uncommitted doc edits

Bramble snapshots the worktree at launch. If the doc has uncommitted edits:

```bash
if ! git diff --quiet "$DOC_PATH"; then
    git add "$DOC_PATH"
    git commit -m "design-doc-polish: round $ROUND snapshot"
fi
```

Doc-only `git add` — don't sweep up unrelated work.

### b) Launch reviewers

Round 1 goal: short — `"Reviewing design doc <path>"`. Round 2+: build a per-turn briefing of what last round fixed/skipped, e.g. `"Round 2. Last round fixed: Milestone 2 (q3, milestone strategy); Intro (q1, long-term fit). Skipped: API surface (q2) wont_fix: design tradeoff."` Keep it under ~500 chars.

Launch two `Monitor` calls in parallel (three with `--gemini`). The `--review-rubric-file` is the same rubric file every round; no `--resume-session-id`; no `--scope-hints-file`; no `--skip-test-execution`.

```
Monitor({
  description: "bramble codex r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=design-doc-polish:$DOC_SLUG:codex:r{ROUND} \
    $BRAMBLE_BIN code-review --backend codex --model gpt-5.4-mini \
    --review-mode design-doc --review-rubric-file \"$STATE_DIR/rubric.txt\" \
    --verbose --timeout 10m \
    --goal \"$GOAL\" \
    --envelope-file \"$STATE_DIR/r$ROUND/codex-envelope.json\" \
    2>\"$STATE_DIR/r$ROUND/codex-stderr.txt\""
})

Monitor({
  description: "bramble cursor r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=design-doc-polish:$DOC_SLUG:cursor:r{ROUND} \
    $BRAMBLE_BIN code-review --backend cursor --model composer-2 \
    --review-mode design-doc --review-rubric-file \"$STATE_DIR/rubric.txt\" \
    --verbose --timeout 10m \
    --goal \"$GOAL\" \
    --envelope-file \"$STATE_DIR/r$ROUND/cursor-envelope.json\" \
    2>\"$STATE_DIR/r$ROUND/cursor-stderr.txt\""
})

// Optional, only when --gemini was passed:
Monitor({
  description: "bramble gemini r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=design-doc-polish:$DOC_SLUG:gemini:r{ROUND} \
    $BRAMBLE_BIN code-review --backend gemini --model gemini-3-flash-preview \
    --review-mode design-doc --review-rubric-file \"$STATE_DIR/rubric.txt\" \
    --verbose --timeout 10m \
    --goal \"$GOAL\" \
    --envelope-file \"$STATE_DIR/r$ROUND/gemini-envelope.json\" \
    2>\"$STATE_DIR/r$ROUND/gemini-stderr.txt\""
})
```

A missing envelope, non-zero exit, or `status: "error"` envelope is a finding (high severity, addressless) — surface it in the round summary with the stderr path; don't treat it as "no findings."

### c) Triage

```
python3 $SKILL_DIR/scripts/bramble_ops.py triage \
    --mode design-doc \
    --stream codex=$STATE_DIR/r$ROUND/codex-envelope.json \
    --stream cursor=$STATE_DIR/r$ROUND/cursor-envelope.json \
    $( [ "$USE_GEMINI" = "1" ] && echo --stream gemini=$STATE_DIR/r$ROUND/gemini-envelope.json )
```

Pass `--mode design-doc` explicitly: when every backend fails before producing an envelope (rare but possible), the auto-detect fallback would default to code mode. Don't pass `--pr-comments` or `--ci-failures` — they're rejected in design-doc mode.

Triage emits the same shape as pr-polish but keys on `(section, dimension)`:

- `consensus` (≥2 sources at same key) → `must_fix`
- `single_critical` (single-source high) → `must_fix`
- `single_medium` → `consider_fix`
- `low_acks` → `batch_ack` (skipped with one-line `wont_fix` reason — no `ack` verb in this skill)

Empty action plan after triage → loop is converged, jump to Step 3.

### d) Apply fixes

Read all findings holistically before editing. Group by underlying systemic problem; one cited finding is usually a symptom of a doc-wide framing issue.

Edit the markdown directly. For each triaged finding, record one entry in the round's actions JSON:

```json
{
  "source": "codex",                                 // codex | cursor | gemini | sweep
  "section": "Milestone 2: Multi-tenant rollout",    // heading the reviewer cited (or "(whole document)")
  "dimension": "q4",                                 // rubric question id
  "severity": "high",                                // high | medium | low | nit
  "topic": "milestone 2 doesn't frontload risk",     // short label
  "action": "fixed",                                 // fixed | false_positive | wont_fix | stale
  "reason": null,                                    // required for wont_fix / false_positive / stale
  "commit_sha": "abc1234"                            // required for fixed
}
```

For class-level fixes that touch sections beyond the cited one, log one row per *swept site* with `source: "sweep"` and that site's section/dimension. Recording each swept site separately keeps an accurate per-section audit trail.

### e) Commit (only if doc changed)

No quality gates — markdown isn't tested. Local commit, no push:

```bash
git add "$DOC_PATH"
git commit -m "design-doc-polish round $ROUND: <one-line summary>" -m "$BODY"
```

`$BODY` lists fixed/skipped findings, one per line.

### f) Finalize round

```
python3 $SKILL_DIR/scripts/doc_ops.py state-finalize-round $CTX $ROUND \
    $STATE_DIR/actions-r$ROUND.json \
    --doc-path "$DOC_PATH" --doc-path-abs "$DOC_PATH_ABS" \
    --rubric-file "$STATE_DIR/rubric.txt" \
    --rubric-source "$RUBRIC_SOURCE" \
    --envelope codex=$STATE_DIR/r$ROUND/codex-envelope.json \
    --envelope cursor=$STATE_DIR/r$ROUND/cursor-envelope.json \
    $( [ "$USE_GEMINI" = "1" ] && echo --envelope gemini=$STATE_DIR/r$ROUND/gemini-envelope.json )
```

Round 1 seeds the state file (doc identity, rubric, started_at). Subsequent rounds append. The state file is the audit trail; it's never deleted.

### g) Convergence

Stop when any of:
- Zero findings, or all are low/nit.
- Action plan is empty.
- `round == ROUNDS` — print Final Summary and `AskUserQuestion` whether to keep going.

## Step 3: Final summary

```
python3 $SKILL_DIR/scripts/doc_ops.py state-mark-complete $CTX <reason>
```

`<reason>`: `converged` / `all-low` / `capped-at-max` / `user-paused`.

Print a Markdown summary:

- Top metrics: rounds completed, findings addressed, commits made, verdict (Ready / Needs revision / Major revision based on the last round's worst severity).
- Round-by-round table: codex/cursor finding counts, fixed counts, dominant dimension per round.
- Comment Actions table: every `comment_actions` row across rounds, sorted by round then severity desc; columns `Round | Source | Section | Dimension | Severity | Action | Notes`.

Tell the user the state file path is preserved, and that nothing was pushed — they decide what to do with the local commits.
