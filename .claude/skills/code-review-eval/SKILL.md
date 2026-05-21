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
BRAMBLE_RUN_TAG="code-review-eval:$(git branch --show-current):{NAME}:r1" \
WORK_DIR=$(pwd) bazel-bin/bramble/bramble_/bramble code-review \
  {FLAGS} --verbose --timeout 10m --envelope-file "$ENVELOPE_FILE" \
  2>"$LOG_DIR/{NAME}-stderr-r1.txt"
```

`BRAMBLE_RUN_TAG` mirrors the convention used by `/pr-polish`
(`bramble_ops.py launch_env`): it propagates into `~/.bramble/logs/code-review/`
so eval runs are greppable later. The tag format is
`code-review-eval:{branch}:{name}:r{turn}`.

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

First, defensively rebuild bramble — the `bazel-bin/bramble/bramble_/bramble`
symlink can be invalidated between turns by unrelated bazel activity in the
same workspace, and the resulting "No such file or directory" error is silent
in Monitor stdout:

```bash
bazel build //bramble:bramble  # cheap when up-to-date
```

Then arm three Monitors in the same turn, mirroring Step 2a but with two changes:

- add `--resume-session-id "$SESSION_<NAME>"` to each command
- write to `$LOG_DIR/{NAME}-envelope-r2.json` and `{NAME}-stderr-r2.txt`

Skip a backend whose `$SESSION_<NAME>` is empty.

Do **not** pass `--resume-prompt-style`. The CLI defaults to `follow-up` when a
resume id is set, which matches the path `/pr-polish` uses — the production caller
this eval most needs to characterize. Canonical command (codex; cursor and gemini
mirror it with backend/model swapped):

> **Prompt shape note (2026-05-06, updated round 10):** the follow-up
> prompt was rewritten to drop the redundant **persona and full JSON
> output spec** re-paste (the resumed session already saw them in turn 1)
> and to widen the model's review scope — earlier shapes used a strict
> "(1)(2)(3) only" framing that biased the model toward ratifying the
> prior verdict (cursor turn 2 in the round-2 eval returned 0 issues
> with summary "HEAD is unchanged since the earlier pass"). The new
> shape leads with "Re-review the full diff with fresh eyes — including
> code you previously accepted" and rewards finding new issues over
> confirming the prior verdict.
>
> Round-8 codex+cursor consensus refined the shape: the
> skip-test-execution suffix and scope clauses (test-quality + cross-
> service) are now **conditionally appended** when opts carries them,
> so a silent resume fallback (resume_status="fallback") reading the
> prompt cold still gets the same scope guidance a real fresh review
> would. The persona / full output spec / goal re-statement remain
> dropped.
>
> Compare turn-2 finding counts before/after this date to baseline the
> bias-guard effect: a non-biased follow-up should not systematically
> produce LOWER turn-2 counts than turn-1 across all backends.

```bash
ENVELOPE_FILE="$LOG_DIR/codex-5.4-mini-envelope-r2.json"
BRAMBLE_RUN_TAG="code-review-eval:$(git branch --show-current):codex-5.4-mini:r2" \
WORK_DIR=$(pwd) bazel-bin/bramble/bramble_/bramble code-review \
  --backend codex --model gpt-5.4-mini --effort medium \
  --resume-session-id "$SESSION_CODEX" \
  --verbose --timeout 10m --envelope-file "$ENVELOPE_FILE" \
  2>"$LOG_DIR/codex-5.4-mini-stderr-r2.txt"
```

## Step 2c: Re-run each config — turn 3 (resumed, fresh prompt)

Same as 2b, but also pass `--resume-prompt-style fresh`. Write to
`$LOG_DIR/{NAME}-envelope-r3.json` and `{NAME}-stderr-r3.txt`. Same skip rule
(empty session id → `resume_status: missing`). Rebuild bramble first for the
same invalidation reason as 2b:

```bash
bazel build //bramble:bramble
```

```bash
ENVELOPE_FILE="$LOG_DIR/codex-5.4-mini-envelope-r3.json"
BRAMBLE_RUN_TAG="code-review-eval:$(git branch --show-current):codex-5.4-mini:r3" \
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
| `unverified` | Resume was requested but the backend exited before a Ready event could confirm the session id (mid-startup error, killed process, transport failure) | envelope `resume_status` |
| `missing` | Turn N skipped because turn 1 produced no session id | local — set in eval prose |
| `no-envelope` | Turn N produced no envelope file on disk (CLI crashed before flushing) | local |

Cursor or codex returning `fallback` is surprising — they're expected to always
support resume. Flag prominently in Notes if it happens. Gemini reporting
`fallback` is less surprising but still worth noting. `unverified` on any
backend is a stronger signal than `fallback`: it means the backend never
finished startup, not that the session was unavailable.

## Step 3: Compare findings

After all turns complete, read each envelope and extract findings. The envelope
shape is `{status, backend, model, session_id, resume_status, review: {verdict,
summary, issues: [...]}, schema_version, duration_ms, input_tokens, output_tokens}`.
Findings live at `.review.issues[]`, not `.issues[]`. For each (config, turn) list:

- Issues found (`.review.issues[].file/.line/.severity/.message`)
- Verdict (`.review.verdict`: `accepted` / `rejected`)
- Confidence per issue (`.review.issues[].confidence`)
- Wall clock time (`.duration_ms`); token counts (`.input_tokens` / `.output_tokens`,
  often 0 — only codex reports these reliably)
- Whether the always-emit envelope guard fired (check stderr for "envelope guard" lines)
- For turns 2 and 3: `.resume_status` from the envelope (or local taxonomy value)

A non-zero monitor exit code with `status: "error"` (or no envelope at all) means
the backend failed terminally — record per the taxonomy below.

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

## Eval dataset

`scripts/harvest.py` walks past `/pr-polish` runs in `~/.bramble/projects/`
and emits one ground-truthed JSON record per PR into
`~/.bramble/code-review-eval/dataset/`. Each record carries R1 and the
final round per PR — the highest-signal slices for recall (fresh-eyes
catch rate on the original diff) and precision (false-positive rate on
near-converged code). Findings are matched back to `comment_actions` and
labeled with `is_real_issue` (`fixed`/`wont_fix`→true,
`false_positive`/`stale`→false, `ack`/`flake`/`pre_existing`→null).

GitHub PR comments (human + review-bot) are fetched fresh from GitHub,
verdict-joined, and attributed to the round their `created_at` falls into —
so the dataset carries every PR comment exactly once, not just the subset
that happened to land in a harvested round. See `scripts/README.md` →
"PR comments".

The dataset is stored **outside the repo** — it is derived from real
private PRs and must never be committed. The per-PR JSON carries
`head_before`, `merge_base_sha`, `files_changed`, and the reconstructed
`goal_text` so a future replay driver can re-run `bramble code-review`
against the exact same diff scope. See `scripts/README.md` for the full
schema and matching rules, and the `/code-review-replay` skill for
scoring a reviewer against this dataset.

```bash
python3 .claude/skills/code-review-eval/scripts/harvest.py \
  --repos-root kernel=/home/ubuntu/g/kernel \
  --repos-root yoloswe=/home/ubuntu/worktrees/yoloswe/main \
  --verbose
```
