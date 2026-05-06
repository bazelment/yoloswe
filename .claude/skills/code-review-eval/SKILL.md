---
name: code-review-eval
description: "Compare bramble code-review output across reviewer configs (cursor with composer-2, codex with gpt-5.4-mini, gemini with gemini-3.1-flash-lite-preview). Runs each config three times against the same branch — turn 1 fresh, turn 2 resumed with default follow-up prompt, turn 3 resumed with fresh prompt — to also characterize backend resume behavior, then compares findings side-by-side and logs results."
argument-hint: "[branch]"
---

# Code Review Eval

Run `bramble code-review` with multiple backend/model configs against the same branch,
then compare their findings side-by-side. Each config runs **three turns** so the eval
also characterizes the resume path that `/pr-polish` uses across rounds.

## Configs to evaluate

| Name | Backend | Model | Flags |
|------|---------|-------|-------|
| codex-5.4-mini | codex | gpt-5.4-mini | `--backend codex --model gpt-5.4-mini --effort medium` |
| cursor-composer2 | cursor | composer-2 | `--backend cursor --model composer-2` |
| gemini-3.1-flash-lite-preview | gemini | gemini-3.1-flash-lite-preview | `--backend gemini --model gemini-3.1-flash-lite-preview` |

## Three-turn flow per config

For each backend/model pair, run three turns in sequence so the eval characterizes
both the initial review and the resume paths:

| Turn | Flags added | What it exercises |
|------|-------------|-------------------|
| 1 | (none) | Fresh review. Captures `session_id` for later turns. `resume_status` is empty. |
| 2 | `--resume-session-id $SESSION` | Resumed session, default `follow-up` prompt. This is the path `/pr-polish` uses across rounds. |
| 3 | `--resume-session-id $SESSION --resume-prompt-style fresh` | Resumed session, explicitly fresh prompt. Valid CLI shape with no current production caller; eval-only. |

A turn-2/turn-3 envelope's `resume_status` field tells us whether the backend
actually resumed (`"ok"`) or silently degraded to a cold start (`"fallback"`). The
status is finalized only after the backend's `ReadyEvent` confirms the session id,
so `ok` is trustworthy. Cursor and codex are expected to always report `ok`; gemini
may legitimately report `fallback` depending on session lifetime.

## Step 1: Build and identify target

```bash
bazel build //bramble:bramble
```

Identify the branch to review. If an argument is given, `git checkout` that branch
first. Otherwise use the current branch. Get the diff summary:

```bash
git diff $(git merge-base origin/main HEAD)..HEAD --stat
```

## Step 2a: Run each config — turn 1 (fresh)

Use the **Monitor tool** for each config. `--envelope-file` writes the final
ResultEnvelope to a file; stdout carries plain-text progress lines for Monitor to
stream. Codex defaults to `--read-only`. Cursor and Gemini have no read-only mode,
so they run with default permissions.

Create a fresh `$LOG_DIR` under `/tmp/code-review-eval-{timestamp}/`. For each config:

```bash
ENVELOPE_FILE="$LOG_DIR/{NAME}-envelope-r1.json"
WORK_DIR=$(pwd) bazel-bin/bramble/bramble_/bramble code-review \
  {FLAGS} --verbose --timeout 10m --envelope-file "$ENVELOPE_FILE" \
  2>"$LOG_DIR/{NAME}-stderr-r1.txt"
```

Arm all three Monitors in the same turn so configs run in parallel. Set Monitor
`timeout_ms=600000`. After all Monitors complete, extract each turn's `session_id`
into shell variables before arming turn 2:

```bash
SESSION_CODEX=$(jq -r '.session_id // empty'   "$LOG_DIR/codex-5.4-mini-envelope-r1.json"  2>/dev/null || true)
SESSION_CURSOR=$(jq -r '.session_id // empty'  "$LOG_DIR/cursor-composer2-envelope-r1.json" 2>/dev/null || true)
SESSION_GEMINI=$(jq -r '.session_id // empty'  "$LOG_DIR/gemini-3.1-flash-lite-preview-envelope-r1.json" 2>/dev/null || true)
```

If any `SESSION_*` is empty (no envelope, malformed envelope, or backend never
emitted a session id), record `resume_status: missing` for that backend in the
comparison table and skip its turn 2 and turn 3. Don't run a fresh review and call
it resumed — that contaminates the eval signal.

Note: codex only reports shell command tool calls on stdout; file reads are internal
to the codex SDK and not surfaced. Gemini reports tool calls via ACP.

## Step 2b: Re-run each config — turn 2 (resumed, follow-up prompt)

Arm three Monitors in the same turn, mirroring Step 2a but with two changes:

- add `--resume-session-id "$SESSION_<NAME>"` to each command
- write to `$LOG_DIR/{NAME}-envelope-r2.json` and `{NAME}-stderr-r2.txt`

Skip a backend whose `$SESSION_<NAME>` is empty.

Do **not** pass `--resume-prompt-style`. The CLI defaults to `follow-up` when a
resume id is set, which matches the path `/pr-polish` uses — the production caller
this eval most needs to characterize. Canonical command (codex; cursor and gemini
mirror it with backend/model swapped):

```bash
ENVELOPE_FILE="$LOG_DIR/codex-5.4-mini-envelope-r2.json"
WORK_DIR=$(pwd) bazel-bin/bramble/bramble_/bramble code-review \
  --backend codex --model gpt-5.4-mini --effort medium \
  --resume-session-id "$SESSION_CODEX" \
  --verbose --timeout 10m --envelope-file "$ENVELOPE_FILE" \
  2>"$LOG_DIR/codex-5.4-mini-stderr-r2.txt"
```

## Step 2c: Re-run each config — turn 3 (resumed, fresh prompt)

Same as 2b, but also pass `--resume-prompt-style fresh`. Write to
`$LOG_DIR/{NAME}-envelope-r3.json` and `{NAME}-stderr-r3.txt`. Same skip rule
(empty session id → `resume_status: missing`).

```bash
ENVELOPE_FILE="$LOG_DIR/codex-5.4-mini-envelope-r3.json"
WORK_DIR=$(pwd) bazel-bin/bramble/bramble_/bramble code-review \
  --backend codex --model gpt-5.4-mini --effort medium \
  --resume-session-id "$SESSION_CODEX" --resume-prompt-style fresh \
  --verbose --timeout 10m --envelope-file "$ENVELOPE_FILE" \
  2>"$LOG_DIR/codex-5.4-mini-stderr-r3.txt"
```

### Resume status taxonomy

| Status | Meaning | Source |
|--------|---------|--------|
| `ok` | Backend confirmed the resumed session id matches the requested id | envelope `resume_status` |
| `fallback` | Backend ran fresh after resume failed (session expired / not found) | envelope `resume_status` |
| `missing` | Turn N skipped because turn 1 produced no session id | local — set in eval prose |
| `missing-envelope` | Turn N envelope file absent on disk | local |
| `error` | Turn N envelope present but `status: "error"` | local |

Cursor or codex returning `fallback` is surprising — they're expected to always
support resume. Flag prominently in Notes if it happens. Gemini reporting
`fallback` is less surprising but still worth noting.

## Step 3: Compare findings

After all turns complete, read each envelope and extract findings. For each
(config, turn) list:

- Issues found (file, line, severity, description)
- Verdict (correct/incorrect)
- Confidence score
- Wall clock time (token counts are only reported by codex backends, not cursor)
- Whether the always-emit envelope guard fired (check stderr for "envelope guard" lines)
- For turns 2 and 3: `resume_status` from the envelope (or local taxonomy value)

Then produce two tables.

**Comparison of turn-1 findings (the canonical review):**

| Finding | cursor-composer2 | codex-5.4-mini | gemini-3.1-flash-lite-preview |
|---------|-----------------|----------------|----------------|
| Issue X | found (medium) | missed | found (high) |
| Issue Y | missed | found (low) | missed |
| FP: ... | flagged | — | flagged |

Identify:
- **Consensus findings**: flagged by all three configs (high confidence these are real)
- **Majority findings**: flagged by two of three configs (likely real)
- **Unique findings**: only one config caught it (investigate — real issue or FP?)
- **False positives**: findings that are clearly not issues
- **Disagreements**: findings where configs differ on severity or applicability

**Follow-up review (turn 2 and turn 3) findings delta** — for each config, compare
turn N against turn 1 on `(file, line, severity, message)` keys:

| Config | Turn | Resume | New | Dropped | Severity changes |
|--------|------|--------|-----|---------|------------------|
| cursor-composer2 | 2 | ok | N | N | N |
| cursor-composer2 | 3 | ok | N | N | N |
| codex-5.4-mini | 2 | ok | N | N | N |
| codex-5.4-mini | 3 | ok | N | N | N |
| gemini-3.1-flash-lite-preview | 2 | fallback | N | N | N |
| gemini-3.1-flash-lite-preview | 3 | missing | — | — | — |

Then a short prose section per backend listing notable deltas — does the resumed
follow-up meaningfully refine, contradict, or just restate? Does the explicit
fresh prompt on a resumed session behave differently from a brand-new fresh run?

Check `.claude/skills/code-review-eval/references/known-blind-spots.md` for previously
identified blind spots and note if any recur.

## Step 4: Log results

Append to `.claude/skills/code-review-eval/data/eval-runs.log`:

```
## {DATE} — {BRANCH}

Diff: {N} files, {+/-} lines

| Config | Turn | Resume | Findings | FPs | Verdict | Confidence | Time | Tokens (in/out) |
|--------|------|--------|----------|-----|---------|------------|------|-----------------|
| cursor-composer2 | 1 | — | N | N | ... | ... | ...s | — |
| cursor-composer2 | 2 | ok | N | N | ... | ... | ...s | — |
| cursor-composer2 | 3 | ok | N | N | ... | ... | ...s | — |
| codex-5.4-mini | 1 | — | N | N | ... | ... | ...s | N/N |
| codex-5.4-mini | 2 | ok | N | N | ... | ... | ...s | N/N |
| codex-5.4-mini | 3 | ok | N | N | ... | ... | ...s | N/N |
| gemini-3.1-flash-lite-preview | 1 | — | N | N | ... | ... | ...s | — |
| gemini-3.1-flash-lite-preview | 2 | fallback | N | N | ... | ... | ...s | — |
| gemini-3.1-flash-lite-preview | 3 | missing | — | — | ... | ... | ...s | — |

Resume incidents: gemini reported "fallback" on turn 2 (cold-restarted instead of
resuming session id <id>); turn 3 was skipped because the prior session id was empty.

Consensus (all 3, turn 1): ...
Majority (2 of 3, turn 1): ...
Unique to cursor (turn 1): ...
Unique to codex-5.4-mini (turn 1): ...
Unique to gemini-3.1-flash-lite-preview (turn 1): ...
Disagreements: ...

Follow-up deltas:
- cursor-composer2: turn 2 +N new, -N dropped, N severity changes; turn 3 ...
- codex-5.4-mini: ...
- gemini-3.1-flash-lite-preview: ...
```

Only emit the "Resume incidents" line when at least one backend reported
`fallback`, `missing`, `missing-envelope`, or `error`. Otherwise omit it — the
table alone tells the story.

Schema notes:
- The `Tokens (in/out)` column was added when Gemini support was introduced
  (2026-04-21). Historical rows without this column remain valid — treat missing as `—`.
- The `Turn` and `Resume` columns and the Follow-up Deltas block were added
  2026-05-06 to capture three-turn resume behavior. Historical rows have one row
  per config (implicitly turn 1) and no resume column; treat missing as `—`.

## Final Summary

```
## Code Review Eval Summary

| Config | Turn | Findings | FPs | Verdict | Time |
|--------|------|----------|-----|---------|------|
| cursor-composer2 | 1 | | | | |
| codex-5.4-mini | 1 | | | | |
| gemini-3.1-flash-lite-preview | 1 | | | | |

Resume health: cursor=<status>, codex=<status>, gemini=<status>

Best config: ...
Recommendation: ...
```

`Resume health` summarizes turn-2 status per backend (the path that actually
matters for `/pr-polish`). If turn 3 disagrees with turn 2 in an interesting way
(e.g. turn 2 ok, turn 3 fallback), call that out in Recommendation.

## Key Files

| Area | Files |
|------|-------|
| Review CLI | `bramble/cmd/codereview/codereview.go` |
| Resume CLI flags | `bramble/cmd/codereview/codereview.go` (`--resume-session-id`, `--resume-prompt-style`) |
| Reviewer impl | `yoloswe/reviewer/reviewer.go` |
| Resume helpers | `yoloswe/reviewer/resume.go` (`ResumeStatus`, fallback classification) |
| Envelope schema | `yoloswe/reviewer/json_output.go` (`session_id`, `resume_status`) |
| Cursor backend | `yoloswe/reviewer/backend_cursor.go` |
| Codex backend | `yoloswe/reviewer/backend_codex.go` |
| Gemini backend | `yoloswe/reviewer/backend_gemini.go` |
| Run history | `.claude/skills/code-review-eval/data/eval-runs.log` |
