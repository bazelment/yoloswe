---
name: design-doc-polish
description: Iterative review of a markdown design document. Round 0 infers a grilling rubric from the doc and asks the user to confirm. N rounds of codex+cursor (optionally +gemini) grill the doc against that rubric. Orchestrator edits the doc between rounds; commits locally each round. Never pushes.
argument-hint: "<doc-path> [--rounds N] [--gemini] [--rubric-file <path>]"
disable-model-invocation: true
---

# Design Doc Polish Loop

Autonomous orchestrator that drives a markdown design doc from "rough draft" to "design grilled and tightened." Each round runs `bramble code-review --review-mode design-doc` against the doc with a caller-supplied rubric, triages section/dimension-keyed findings, edits the doc, and commits locally. **No pushes happen, ever** — the skill never assumes the doc lives on a branch with a remote.

The loop exits when the review has converged (zero findings, all low/nit, empty action plan, or the user accepts a `spiral_matches` escalation) or when the round cap (`--rounds N`, default 10) is hit.

Shell plumbing lives in Python helpers under `scripts/`. Use `SKILL_DIR=.claude/skills/design-doc-polish` and call them as `python3 $SKILL_DIR/scripts/<helper>.py …`.

- `doc_ops.py` — doc identity (slug + state paths), state I/O (load/append-round/finalize-round/mark-complete/mark-abandoned), rubric file reader.
- The triage / envelope-parsing / session-id machinery is **shared with /pr-polish** — `doc_ops.py` imports `bramble_ops.py` and `_common.py` from `.claude/skills/pr-polish/scripts/` directly. A bug fix to consensus or spiral logic lands once and applies to both skills. The mode-aware dispatch was added in the same change as this skill (see `bramble_ops._KEY_CONSTRUCTORS`).

**Failed review streams are findings, not silence.** Missing envelopes, non-zero exits, or `status: "error"` envelopes surface in the round summary with the stderr path cited. The triage layer treats them as high-severity findings keyed off `(None, None)` so they route to `single_critical` rather than vanishing.

## Arguments

| Flag | Default | Meaning |
|---|---|---|
| `<doc-path>` | required | Path to a markdown design doc, relative or absolute. Must live under the current repo worktree. |
| `--rounds N` | `10` | Round number to stop at (inclusive). Resuming at `current_round=10` with `--rounds 12` runs rounds 11 and 12. |
| `--gemini` | off | Also run a third reviewer with `--backend gemini --model gemini-3-flash-preview`. A finding agreed on by ≥2 sources counts as consensus. |
| `--rubric-file <path>` | off | Skip Round-0 rubric inference and use a caller-supplied rubric file (one grilling question per non-blank line; `#` lines are comments). The user is still asked to confirm before round 1. |

## Ownership and convergence

**Fix, don't dodge.**
- Consensus findings (two reviewers at the same `(section, dimension)`, regardless of wording): mandatory fix. Edit the doc.
- High/medium: fix unless provably false positive (cite a section/passage that refutes the finding).
- Low/nit: fix if trivial, else skip with a one-line `wont_fix` reason.
- Valid skip reasons: `false_positive` (with evidence), `wont_fix` (design tradeoff or "needs author input" with a `> **TODO**: …` blockquote in the doc), `stale` (cited section gone after a prior fix).

**A finding is a symptom; the fix is the cure.** Each cited finding observes one underlying problem. Before patching, identify the systemic issue the rubric question is pointing at, look for sibling sites in the same doc, and decide whether the right fix is at the cited section or at the doc's overall framing. Two reviewers wording the same milestone-strategy concern differently are pointing at one design problem. A finding that says "milestone 2 doesn't frontload risk" usually implies milestone 1 should be re-scoped, not just milestone 2 caveated.

Class-level fixes beyond the cited section are logged in `comment_actions` as one row per *swept site* — `source: "sweep"`, `topic: "<original-topic> — class-level fix"`, `section: <swept site's section>` (NOT the original cited section), `dimension: <swept site's dimension>`. Recording each swept site separately keeps the spiral guard accurate: a round-N+1 finding at a section the orchestrator already touched in a sweep would otherwise look like a fresh issue rather than a regression.

**Convergence — stop when any one holds:**
- Zero findings, or all remaining are low/nit.
- Top-rated finding is a documented false positive.
- Empty action plan after triage.

**Hard stop at `--rounds N`.** When round N completes without convergence, produce Final Summary, then `AskUserQuestion` whether to continue.

## Auto-decision rules

Bias for action and your own judgement with research. Only three things pause the loop with `AskUserQuestion`:

1. **Integrity gate** — state file's `doc_path` doesn't match the requested doc (slug collision).
2. **Budget gate** — `--rounds N` reached without convergence.
3. **Regression gate** — `spiral_matches` non-empty (a prior-round `fixed` finding re-surfaced at the same `(section, dimension)`).

The rubric-confirmation gate at the end of Step 0.7 is a deliberate fourth pause — the user controls the grilling axes for the run.

## State tracking

Path: `~/.bramble/projects/<repo>-doc-<slug>/design-doc-polish-state.json`

`<slug>` = `<basename-without-ext>-<sha256(abs_path)[:12]>`. The hash prefix avoids cross-directory basename collisions; the basename keeps the directory human-readable. Context token: `doc:<slug>` (mirrors pr-polish's `branch:<name>`).

State survives context compaction and is **never deleted** post-loop — humans read it for the per-finding audit trail.

Schema:

```json
{
  "doc_path": "docs/design/sessionmodel-architecture.md",
  "doc_path_abs": "/abs/path/to/the/doc.md",
  "doc_slug": "sessionmodel-architecture-3a7f2c1b9d4e",
  "rubric": [
    "Is this the best long-term choice?",
    "Can we make it simpler?",
    "Does milestone create clear boundary?",
    "Does milestone frontload risk discovery in the early phase?"
  ],
  "rubric_source": "inferred",
  "started_at": "2026-05-08T17:00:00Z",
  "current_round": 2,
  "last_commit_at_round_start": "abc123f",
  "last_heartbeat_at": "2026-05-08T17:14:00Z",
  "completed": false,
  "exit_reason": null,
  "rounds": [
    {
      "n": 1,
      "head_before": "parent-sha",
      "head_after": "commit-sha",
      "codex_findings": [{"severity": "high", "section": "Milestone 2", "dimension": "q4", "topic": "milestone 2 doesn't frontload risk"}],
      "cursor_findings": [{"severity": "medium", "section": "Intro", "dimension": "q1", "topic": "long-term fit unclear"}],
      "fixed_count": 3,
      "skipped_count": 1,
      "top_severity": "high",
      "comment_actions": [
        {
          "source": "codex",
          "comment_id": null,
          "section": "Milestone 2: Multi-tenant rollout",
          "dimension": "q4",
          "severity": "high",
          "topic": "milestone 2 doesn't frontload tenant-isolation risk",
          "action": "fixed",
          "reason": null,
          "commit_sha": "abc123f"
        }
      ],
      "session_ids": {"codex": "...", "cursor": "..."},
      "resume_status": {"codex": "ok", "cursor": "ok"}
    }
  ]
}
```

`comment_actions` schema:
- `source`: one of `codex`, `cursor`, `gemini`, `sweep`. (No `github-*`/`ci`/`lint` — those are pr-polish-only.)
- `comment_id`: always `null` (no PR comments to track).
- `section`: the reviewer's cited heading (or `(whole document)` for doc-wide issues).
- `dimension`: the rubric question id, e.g. `q1`, `q2`, ...
- `severity`: `high`, `medium`, `low`, `nit`, or `null`.
- `action`: `fixed` (commit_sha required) | `false_positive` (reason required, citing the refuting section) | `wont_fix` (reason required, e.g. "needs author input" with a `> **TODO**:` blockquote dropped in the doc) | `stale` (cited section was rewritten/removed in an earlier round).

## Step 0: Identify doc + state paths

```
python3 $SKILL_DIR/scripts/doc_ops.py identify <doc-path>
```

Returns `{doc_path, doc_path_abs, doc_slug, state_dir, state_file, ctx}`. `ctx` is `doc:<slug>`. Pin `$CTX` to it for every later state-* call.

If the path doesn't exist, isn't a regular file, or lives outside the repo worktree, the helper exits non-zero and prints to stderr — surface that to the user and stop.

Non-`.md` extensions (`.txt`, `.markdown`, no extension) are accepted; the orchestrator should warn-only when the basename doesn't end in `.md` since some teams write design docs without it.

### Step 0.1: Pick the bramble binary

Same as pr-polish — prefer the freshly-built worktree artifact over PATH:

```bash
export BRAMBLE_BIN="$([ -x "$(pwd)/bazel-bin/bramble/bramble_/bramble" ] \
    && echo "$(pwd)/bazel-bin/bramble/bramble_/bramble" \
    || echo bramble)"
```

### Step 0.2: Probe `--resume-session-id` support

```bash
"$BRAMBLE_BIN" code-review --help 2>&1 | grep -q -- '--resume-session-id' || {
  echo "error: '$BRAMBLE_BIN' does not support --resume-session-id." >&2
  exit 1
}
```

### Step 0.3: Probe `--review-mode design-doc` support

This skill requires the design-doc-mode bramble extension. Fail fast if it's not present:

```bash
"$BRAMBLE_BIN" code-review --help 2>&1 | grep -q -- '--review-mode' || {
  echo "error: '$BRAMBLE_BIN' does not support --review-mode design-doc. Run from a worktree where the bramble design-doc patch is built (bazel build //bramble/cmd/codereview/...)." >&2
  exit 1
}
```

## Step 0.5: Resume check

```
python3 $SKILL_DIR/scripts/doc_ops.py state-load $CTX
```

Decorates the persisted state with `is_heartbeat_stale` and `is_first_round_of_series`. Capture the new-series decision **before** `state-append-round` clears the `completed` flag:

```bash
IS_NEW_SERIES=$(python3 $SKILL_DIR/scripts/doc_ops.py state-is-new-series $CTX $ROUND)
```

Compare against `git rev-parse HEAD`:

- **No state file**: fresh run. Proceed.
- **`doc_path_abs` mismatch**: integrity gate. The slug collided with another doc, or the user resolved a relative path under a different worktree. `AskUserQuestion` whether to discard the existing state.
- **`is_heartbeat_stale: true`** AND `completed: false`: prior run abandoned. Tombstone with `state-mark-abandoned $CTX` and start fresh on current HEAD. Announce in one line; do not ask.
- **HEAD matches `last_commit_at_round_start`** (heartbeat fresh): prior round interrupted. Resume in-progress round.
- **HEAD differs from `last_commit_at_round_start`** (heartbeat fresh): prior round committed or the user made manual edits. Auto-start next round on current HEAD.
- **`current_round` ≥ `--rounds`**: budget gate hard-stop. `AskUserQuestion` to authorize more rounds.

## Step 0.7: Infer or load rubric

**Skipped on resumed series** (rubric is locked at round 1 and persisted in state).

### Path A: `--rubric-file <path>` was passed

Read it:

```bash
RUBRIC_FILE="$(realpath --no-symlinks "$1")"   # the user-supplied path
```

Show the contents to the user and `AskUserQuestion` to confirm or edit. Per user choice, the confirmation gate fires even on file-supplied rubrics for UX consistency.

### Path B: No `--rubric-file` — infer

Run a single bramble call with a meta-prompt to classify the doc and propose a rubric. The model output is one JSON object:

```
$BRAMBLE_BIN code-review --backend cursor --model composer-2 \
  --review-mode code \
  --goal "Read the design document at <doc-path>. Classify it roughly as one of (architecture / detailed-design / migration / API-contract / RFC / one-pager / other). Then propose 3-7 short grilling questions a staff engineer reviewing this kind of doc should ask. Output JSON only: {\"kind\": \"...\", \"rubric\": [\"q1\", \"q2\", ...]}." \
  --skip-test-execution --timeout 5m \
  --envelope-file "$STATE_DIR/rubric-inference-envelope.json"
```

(Code mode is correct here — we're not grilling the doc yet, we're asking for a JSON classification. The actual review uses design-doc mode.)

Read the JSON, surface to the user via `AskUserQuestion` with options:
1. **Accept** — use the proposed rubric verbatim.
2. **Edit** — let the user replace it.
3. **Use the 4 starter questions** — fall back to:
   - "Is this the best long-term choice?"
   - "Can we make it simpler?"
   - "Does milestone create clear boundary?"
   - "Does milestone frontload risk discovery in the early phase?"

If inference fails (malformed JSON, empty rubric, status=error): retry once. On second failure, print a one-line note and offer the 4 starter questions as the recovery default.

### After Path A or B: persist the rubric

Write the chosen rubric to `$STATE_DIR/rubric.txt` (one question per line, no comments, no leading whitespace) — this is the file passed via `--review-rubric-file` to every round's bramble call.

```bash
mkdir -p "$STATE_DIR"
printf '%s\n' "${RUBRIC[@]}" > "$STATE_DIR/rubric.txt"
```

The rubric also gets persisted into the state file at `state-append-round` (next step) for the audit trail.

## Step 1: Round loop

```
for round = 1..ROUNDS:
  a) commit any pending changes to the doc (WIP ok)
  b) launch bramble codex + cursor (+gemini) Monitors in parallel
  c) triage findings (no PR comments, no CI failures, no lint)
  d) read findings holistically, edit the doc
  e) if changes: commit locally (NO push)
  f) finalize round state
  g) check convergence; exit if met
```

Header per round: `## Round N / ROUNDS`.

### a) Pending-change WIP commit

Bramble snapshots the working tree at launch — uncommitted changes won't be reviewed. Commit the doc only (don't sweep up unrelated work):

```bash
if ! git diff --quiet "$DOC_PATH"; then
    git add "$DOC_PATH"
    git commit -m "design-doc-polish: round $ROUND snapshot"
fi
```

Restricted to the target doc deliberately — `git add -A` would commit unrelated edits the user happens to have in the worktree.

### b) Launch monitors

Compute the round's `--goal` text and per-backend resume id:

```bash
GOAL=$(python3 $SKILL_DIR/scripts/bramble_ops.py goal {ROUND} \
        --pr-summary "Reviewing design doc $DOC_PATH" \
        --state-file "$STATE_FILE" \
        --head-before "$(git rev-parse HEAD)")
CODEX_RESUME=$(python3 $SKILL_DIR/scripts/bramble_ops.py prior-session-id codex {ROUND} \
                --state-file "$STATE_FILE" --is-new-series "$IS_NEW_SERIES")
CURSOR_RESUME=$(python3 $SKILL_DIR/scripts/bramble_ops.py prior-session-id cursor {ROUND} \
                --state-file "$STATE_FILE" --is-new-series "$IS_NEW_SERIES")
```

(Yes, we call `bramble_ops.py` from the pr-polish skill directly. The shared module pattern is intentional.)

`bramble_ops.py goal` round 1: prints `$PR_SUMMARY` (`Reviewing design doc <path>`). Round 2+: prints the action-history goal — prior round's fixed/skipped findings + a "Files changed since round N-1" line. The "Files changed" line will report just the doc itself, which is fine.

Create a fresh `$LOG_DIR=$STATE_DIR/r$ROUND/` and arm the Monitors:

```
ENVELOPE_CODEX="$LOG_DIR/codex-envelope.json"
ENVELOPE_CURSOR="$LOG_DIR/cursor-envelope.json"

Monitor({
  description: "bramble codex r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=design-doc-polish:$DOC_SLUG:codex:r{ROUND} \
    $BRAMBLE_BIN code-review --backend codex --model gpt-5.4-mini \
    --review-mode design-doc --review-rubric-file \"$STATE_DIR/rubric.txt\" \
    --verbose --timeout 10m \
    --goal \"$GOAL\" \
    ${CODEX_RESUME:+--resume-session-id \"$CODEX_RESUME\"} \
    --envelope-file \"$ENVELOPE_CODEX\" 2>\"$LOG_DIR/codex-stderr.txt\""
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
    ${CURSOR_RESUME:+--resume-session-id \"$CURSOR_RESUME\"} \
    --envelope-file \"$ENVELOPE_CURSOR\" 2>\"$LOG_DIR/cursor-stderr.txt\""
})

// Only when --gemini was passed:
ENVELOPE_GEMINI="$LOG_DIR/gemini-envelope.json"
GEMINI_RESUME=$(python3 $SKILL_DIR/scripts/bramble_ops.py prior-session-id gemini {ROUND} \
                --state-file "$STATE_FILE" --is-new-series "$IS_NEW_SERIES")

Monitor({
  description: "bramble gemini r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=design-doc-polish:$DOC_SLUG:gemini:r{ROUND} \
    $BRAMBLE_BIN code-review --backend gemini --model gemini-3-flash-preview \
    --review-mode design-doc --review-rubric-file \"$STATE_DIR/rubric.txt\" \
    --verbose --timeout 10m \
    --goal \"$GOAL\" \
    ${GEMINI_RESUME:+--resume-session-id \"$GEMINI_RESUME\"} \
    --envelope-file \"$ENVELOPE_GEMINI\" 2>\"$LOG_DIR/gemini-stderr.txt\""
})
```

Differences from pr-polish: no `--scope-hints-file` (diff-derived, useless on a doc), no `--skip-test-execution` (irrelevant for design-doc mode; bramble silent-ignores it but we don't pass noise the model has to read past), no `lint_gate.py` Monitor (no diff-based lint applies), and `--review-mode design-doc --review-rubric-file ...` are required.

### c) Triage

```
python3 $SKILL_DIR/scripts/bramble_ops.py triage $STATE_FILE \
    --stream codex=$ENVELOPE_CODEX \
    --stream cursor=$ENVELOPE_CURSOR \
    $( [ "$USE_GEMINI" = "1" ] && echo --stream gemini=$ENVELOPE_GEMINI )
```

`--mode` is omitted; the triage CLI reads the `review_mode` field off each envelope and dispatches automatically. The result envelope's `review_mode` field will be `"design-doc"`. Do not pass `--pr-comments` or `--ci-failures` — they're rejected in design-doc mode.

Triage emits the same buckets as pr-polish, but consensus and spiral keys are `(section, dimension)` instead of `(file, line)`:

- `consensus` — same `(section, dimension)` from ≥2 sources. Route to `must_fix`.
- `single_critical` — single-source high. Route to `must_fix`.
- `single_medium` — single-source medium. Route to `consider_fix`.
- `low_acks` — single-source low/nit. Route to `batch_ack` (skipped with a one-line `wont_fix` reason — there is no `ack` verb in this skill).
- `spiral_matches` — new findings matching a prior-round `fixed` action by `(section, dimension, topic)` (exact recurrence) or `(section, dimension)` alone (rewording-resilient). Route to `escalate`.

If `spiral_matches` is non-empty, **don't auto-fix** — `AskUserQuestion` with the spiralling findings.

If the action plan is empty (nothing in `must_fix`/`consider_fix`, only `batch_ack`), exit the loop.

### d) Apply fixes — orchestrator edits the doc

You apply edits yourself — your continuity from triage and prior rounds is more valuable than parallelism on a single file.

Read the review report holistically *before* you touch the doc:

- The findings are evidence, not a checklist. Two reviewers wording the same systemic problem differently are pointing at one issue.
- A cited section is a symptom; the underlying invariant may live in the doc's overall framing, not at one heading.
- `action_plan.cluster_hint` shows where findings concentrate by section. If "Milestone 2" has 3 findings, the milestone is probably mis-scoped — fix the section's framing, don't sprinkle three caveats.

What good looks like by the end of this step:

- Every finding read across all backends as one body of evidence, grouped by underlying systemic problem.
- The doc edited at the systemic level — milestone restructure, scope cut, alternative considered, risk reframed — not a list of inline footnotes.
- For findings the user must answer (genuine open questions, undocumented context only the author has): drop a `> **TODO**: <question>` blockquote in the relevant section. Log as `wont_fix` with `reason: "needs author input"`. **Do not invent author intent.**
- Every triaged finding has a `comment_actions` entry. Each *swept site* fixed beyond a cited section is logged as its own `source: "sweep"` row with that site's `section`/`dimension` and `topic: "<original-topic> — class-level fix"` — one row per swept site, not one row per sweep operation. This keeps the spiral guard's view of "what's already been fixed" accurate per-section.

`comment_actions` field shapes for design-doc mode:
- All entries: `comment_id: null` (no PR comments).
- Bramble findings (`codex` / `cursor` / `gemini`) carry `section`, `dimension`, `severity`, `topic`, `action`, `reason` (when not fixed), `commit_sha` (when fixed).
- Sweep entries: `source: "sweep"`, `topic: "<original-topic> — class-level fix"`, `section`/`dimension` of the *swept site* (one row per site, not the original cited finding's section).

### e) Quality gates + commit (only if doc changed)

Skip if step d produced zero changes.

There are **no quality gates** for a markdown doc — we're not building or testing anything. Commit locally:

```bash
git add "$DOC_PATH"
git commit -m "design-doc-polish round $ROUND: <one-line summary>" -m "$BODY"
```

Body lists fixed/skipped findings, one per line. **Do NOT push, do NOT call /git:sync-base** — the doc may live on any branch and the skill never touches the remote.

**Before committing, ask whether the fix is durable.** For each finding addressed: would a reviewer running the same review on the new doc raise the same systemic concern at a different section? If yes, that's a missed sibling — extend the fix. Visible intentional non-uniformity beats next round's finding.

### f) Finalize round state

Write accumulated `comment_actions` to a temp JSON, then:

```
python3 $SKILL_DIR/scripts/doc_ops.py state-finalize-round $CTX $ROUND $(git rev-parse HEAD) \
    $STATE_DIR/actions-r$ROUND.json \
    --envelope codex=$ENVELOPE_CODEX \
    --envelope cursor=$ENVELOPE_CURSOR \
    $( [ "$USE_GEMINI" = "1" ] && echo --envelope gemini=$ENVELOPE_GEMINI )
```

`--envelope` flags hydrate `rounds[n].session_ids` and `resume_status` for next round's resume plumbing, plus copy each envelope to `$STATE_DIR/reviews/r<n>-<backend>.json` for the audit trail. Backends not passed have their per-round state cleared (so a partial re-finalize doesn't carry stale session ids).

### g) Convergence check

Apply the convergence rules from the top. If converged, break. If `round == ROUNDS` and not converged, produce Final Summary and `AskUserQuestion`.

Track progress concisely:

```
Round 1: codex=3 (1h, 2m), cursor=4 (2h, 2m) -> consensus=2, fixed 4, skipped 1 -> continue
Round 2: codex=1 (1m), cursor=1 (1m, same)   -> consensus, fixed 1            -> continue
Round 3: codex=0, cursor=0                   -> EXIT (converged)
```

## Step 2: Final summary + mark complete

```
python3 $SKILL_DIR/scripts/doc_ops.py state-mark-complete $CTX <reason>
```

**Reason values**: `converged`, `all-low`, `false-positive-top`, `capped-at-max`, `user-paused`, `spiral-escalated`, `abandoned` (set by `state-mark-abandoned`; never passed to `state-mark-complete` directly).

Print a Markdown summary:

- Top metrics — rounds completed, findings addressed (fixed + skipped), commits made, convergence signal, verdict (Ready / Needs revision / Major revision).
- Round-by-round table — codex/cursor finding counts, fixed counts, dominant dimension per round, one-line note.
- Comment Actions table — every `comment_actions` row across rounds, sorted by round then severity desc; columns `Round | Source | Section | Dimension | Severity | Action | Notes`.

Tell the user the state file path (preserved, not deleted) and that nothing was pushed — they decide what to do with the local commits next.
