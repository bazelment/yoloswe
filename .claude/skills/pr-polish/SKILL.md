---
name: pr-polish
description: Fully autonomous PR polish loop. Runs N rounds of local bramble review (codex + cursor, optionally + gemini), folds in any existing PR comments and CI failures as round-1 input, fixes findings locally, pushes once at the end.
argument-hint: "[--rounds N] [--fixer-model MODEL] [--gemini]"
disable-model-invocation: true
---

# PR Polish Loop

Autonomous orchestrator that brings a branch from "has issues" to "ready to merge." Each round runs code review agents as the authoritative review signal, triages findings against the action-plan rules, applies fixes locally, and commits. **No pushes happen until the loop exits** — this keeps each round's bramble review scoped to local code without triggering repeated GitHub-bot re-reviews that would only add N+1 diff spiral noise.

The loop exits when the review has **converged** (see "Ownership and convergence") or when the round cap is hit. On exit the orchestrator force-pushes the accumulated commits so the PR's bot/CI review sees one polished tree instead of N intermediate ones.

All shell plumbing lives in Python helper modules bundled with this skill at `scripts` directory. Use `SKILL_DIR=.claude/skills/pr-polish` and call them as `python3 $SKILL_DIR/scripts/pr_ops.py ...`.

- `pr_ops.py` — PR/branch identity, comment fetch/reply, CI failure detail, state I/O.
- `bramble_ops.py` — Bramble launch, per-round temp files, envelope parse, consensus/triage/N+1 spiral, PR-comment + CI-failure merging.
- `lint_gate.py` — Deterministic ruff/golangci/eslint pass on the diff; closes the CodeQL noise gap.
- `scope_gate.py` — Computes `scope-hints.json` for `bramble code-review --scope-hints-file`: co-located test paths + multi-package detection. Runs once per round, before bramble Monitors.

**Failed review streams are findings, not silence.** Missing envelopes, non-zero exit codes, or `status: "error"` envelopes must be surfaced in the round summary with the stderr path cited; never silently drop them.

**Base-branch syncing is not this skill's job.** Invoke `.claude/skills/git:sync-base/git-sync.py --verbose` directly — that skill owns branch rebasing, precise-lease force-push, and conflict handling.

## Arguments

| Flag | Default | Meaning |
|---|---|---|
| `--rounds N` | `5` | The round number this invocation will *stop at* (inclusive). Resuming a state at `current_round=5` with `--rounds 7` runs rounds 6 and 7 — i.e. "two more rounds". `--rounds 5` on the same state is a no-op (already at the cap). |
| `--fixer-model MODEL` | `sonnet` | Model passed to `Agent(model=…)` spawns when the round's action plan is too large to apply inline. |
| `--gemini` | off | Also run a third bramble reviewer using `--backend gemini --model gemini-3-flash-preview`. Findings from all three backends are merged and deduplicated; a finding agreed on by ≥2 sources (including Gemini) counts as consensus. |

No positional arguments. PR/branch context is auto-detected by `pr_ops.py identify`.

## Ownership and convergence

**Fix, don't dodge.**
- Pre-existing code in touched files: own it.
- Consensus findings (two reviewers agree on same `(path, line, topic)`): mandatory fix.
- High/medium: fix unless provably false positive (cite refuting file:line).
- Low/nit: fix if trivial, else skip with one-line justification.
- Only valid skip reasons: `false_positive` (with evidence), `wont_fix` (design tradeoff), `stale` (cited code gone).

**Stop when converged.** Any one:
- Zero findings, or all remaining are low/nit.
- Top-rated finding is a documented false positive.
- Empty action plan after triage (nothing to fix, nothing to skip).

**Hard stop at `--rounds N`.** When round N completes and the loop has not converged, STOP. Produce Final Summary, then `AskUserQuestion` whether to continue.

## Auto-decision rules

Bias for action and your own judgement with research. Only three things ever pause the loop with `AskUserQuestion`:

1. **Integrity gate** — stale state file with PR mismatch (see Step 0.5).
2. **Budget gate** — `--rounds N` reached without convergence.
3. **Regression gate** — `spiral_matches` non-empty (a prior-round `fixed` finding re-surfaced).

Anything else: perform your own research and decide and record.

## State tracking

Path with PR: `~/.bramble/projects/<repo>-<pr>/pr-polish-state.json`
Path branch-only: `~/.bramble/projects/<repo>-branch-<slug>/pr-polish-state.json`

Survives context compaction. **Never deleted** — humans read it post-loop for the per-comment audit trail.

Schema:

```json
{
  "pr_number": 2318,
  "branch": "feature/foo",
  "started_at": "2026-04-21T17:00:00Z",
  "current_round": 2,
  "last_commit_at_round_start": "abc123f",
  "completed": false,
  "exit_reason": null,
  "rounds": [
    {
      "n": 1,
      "head_before": "parent-sha",
      "head_after": "commit-sha",
      "codex_findings": [{"severity": "high", "topic": "missing purpose param"}],
      "cursor_findings": [{"severity": "medium", "topic": "no tests for path"}],
      "ci_findings": [{"job_id": 222, "is_flake": false, "failed_tests": ["TestFoo"]}],
      "fixed_count": 3,
      "skipped_count": 1,
      "top_severity": "high",
      "top_was_false_positive": false,
      "noise_filtered": 2,
      "noise_samples": [
        {"id": 4300306871, "author": "linear[bot]", "pattern": "linear-linkback"},
        {"id": 4300307985, "author": "claude[bot]", "pattern": "claude-progress"}
      ],
      "comment_actions": [
        {
          "comment_id": 2034881234,
          "source": "github-inline",
          "author": "coderabbitai[bot]",
          "path": "services/python/agent/handlers/provision.py",
          "line": 142,
          "severity": "high",
          "topic": "missing error handling for BUILDER_LITE",
          "action": "fixed",
          "reason": null,
          "commit_sha": "abc123f",
          "reply_url": "https://github.com/owner/repo/pull/2318#discussion_r2034881234"
        }
      ]
    }
  ]
}
```

**`codex_findings` / `cursor_findings` / `gemini_findings`** hold the raw issues list from each backend's bramble envelope for round `n`, hydrated by `state-finalize-round`. The verbatim envelope is copied into `<state_dir>/reviews/r<n>-<backend>.json` so post-loop audits don't depend on the `/tmp` envelope surviving. `gemini_findings` is omitted when `--gemini` was not passed.

**`noise_filtered` / `noise_samples`** (int / array, default 0 / `[]`) count bot process-noise dropped at fetch time by `pr_ops.py fetch-comments` — linear linkbacks, claude-bot "Reviewing PR..." progress posts. These never become findings (they aren't review feedback) so they never enter `comment_actions`; keep them as a round-level audit trail instead. Samples are capped at 5 `{id, author, pattern}` entries for post-hoc debugging. Populated by `state-append-round --noise-filtered N --noise-samples file.json` on round 1.

**`comment_actions` schema — load-bearing, other tooling depends on exact strings:**

- `source`: one of `github-inline`, `github-issue`, `github-review`, `codex`, `cursor`, `gemini`, `lint`, `ci`. (`lint` rows come from `lint_gate.py`'s deterministic ruff/golangci-lint/eslint pass — see Step 3b. They route through triage like any other source: a `(file, line, topic)` match with codex or cursor counts as consensus.)
- `comment_id`: GitHub id for `github-*`; `null` for bramble and CI findings (bramble dedupes by `(path, line, topic)`; CI dedupes by `(job_id, test_name)` where `path=job_id` and `topic=test_name`).
- `path` / `line`: `null` for top-level PR + review-level comments.
- `severity`: `high`, `medium`, `low`, `nit`, or `null`.
- `action`: exactly one of:
  - `fixed` — code change applied; `commit_sha` required.
  - `false_positive` — incorrect finding; `reason` required (file:line that refutes it).
  - `wont_fix` — valid point, deliberate skip; `reason` required (design tradeoff).
  - `ack` — low/nit batch-acknowledged; `reason` optional. Counts as skipped.
  - `stale` — cited code no longer exists (see Stale Finding Guard); `reason` optional.
  - `pre_existing` — `source: "ci"` only. Failure also fails on base branch; `reason` must cite `ci-compare-base` output. Counts as skipped.
  - `flake` — `source: "ci"` only. Classified as a known flake class (ETXTBSY, bazel cache, `ci_deadline`); `reason` must name the `flake_reason`. Counts as skipped.

**Context token for state subcommands.** All `state-*` subcommands take a single positional `ctx` — the bare PR number (`2318`) when one exists, otherwise `branch:<name>` (`branch:feature-foo`).

**Writing state goes through the module, not hand-rolled file I/O.** All writes are atomic:

- `state-append-round <ctx> <n> <head_before>` at round start. Verifies `git rev-parse HEAD == head_before`; non-zero exit if the orchestrator raced a commit. Pass `--no-verify-head` only when resuming an interrupted round.
- `state-finalize-round <ctx> <n> <head_after> <actions.json>` at round end.
- `state-mark-complete <ctx> <reason>` on exit.
- Reading: `state-load <ctx>`.

## Step 0: Identify context

```
python3 $SKILL_DIR/scripts/pr_ops.py identify
```

Returns `{pr_number, title, url, base, head, branch, owner, repo, owner_repo, state_dir, state_file}`. `pr_number` is `null` for branches that don't yet have a PR — the rest of the flow still works, just with PR-comment and CI-failure fetches skipped. Pin `$CTX` to either the PR number or `branch:<head>` for later state subcommand calls.

### Step 0.1: Pick the bramble binary

Export `BRAMBLE_BIN` once at the top of the run. Prefer the freshly-built worktree artifact when it exists (matches the code under review); otherwise fall back to `PATH`:

```
export BRAMBLE_BIN="$([ -x "$(pwd)/bazel-bin/bramble/bramble_/bramble" ] \
    && echo "$(pwd)/bazel-bin/bramble/bramble_/bramble" \
    || echo bramble)"
```

Every `bramble code-review` invocation in later steps must reference `$BRAMBLE_BIN`, not the bare `bramble` literal.

### Step 0.2: Bramble must support `--resume-session-id`

Continuous review is the whole reason this skill exists; without resume the loop devolves into N independent cold reviews and the don't-reflag signal in round 2+ goal is wasted. Probe once, fail fast, and tell the user how to upgrade:

```bash
"$BRAMBLE_BIN" code-review --help 2>&1 | grep -q -- '--resume-session-id' || {
  echo "error: '$BRAMBLE_BIN' does not support --resume-session-id." >&2
  echo "Upgrade your bramble (e.g. 'bazel build //bramble:bramble' in this repo, or reinstall) and re-run." >&2
  exit 1
}
```

Don't paper over with sed-strip workarounds. An old binary is a real environment problem the user needs to fix once, not something the skill can compensate for transparently.

## Step 0.5: Resume check

```
python3 $SKILL_DIR/scripts/pr_ops.py state-load $CTX
```

`state-load` returns the persisted state plus a derived `is_heartbeat_stale` boolean (computed at read time from `last_heartbeat_at`; never persisted). Compare against `git rev-parse HEAD`:

- **No state file / empty load**: fresh run. Proceed.
- **`pr_number` mismatches current PR**: stale state (integrity gate). Show user and `AskUserQuestion` whether to discard. This is one of only three sanctioned pauses — see "Auto-decision rules".
- **`is_heartbeat_stale: true`** AND `completed: false` (heartbeat older than 2h, or missing entirely on a pre-heartbeat state file): the prior run was abandoned, not interrupted. Tombstone it and start fresh:
  ```
  python3 $SKILL_DIR/scripts/pr_ops.py state-mark-abandoned $CTX
  ```
  Then proceed as a fresh run on current HEAD. Announce the abandonment in one line; do not ask.
- **HEAD matches `last_commit_at_round_start`** (and heartbeat is fresh): prior round was interrupted (compaction/manual stop). Resume the in-progress round.
- **HEAD differs from `last_commit_at_round_start`** (and heartbeat is fresh): prior round committed or user made manual changes. Auto-start a new round on current HEAD (round N+1). Announce the decision in one line and proceed — re-invocation is the user's signal that they want another round. The new `state-append-round` call automatically clears any stale `completed: true` / `exit_reason` set by the prior loop's `state-mark-complete`, so mid-loop reads of the state file aren't contradictory.
- **`current_round` ≥ `--rounds`**: hard-stop (budget gate). `--rounds N` means "round N is the last round this invocation runs"; if `current_round == N`, this invocation has nothing to do and must explicitly authorize more rounds via `AskUserQuestion`. Concretely: a state at `current_round=5` invoked with `/pr-polish --rounds 7` runs rounds 6 and 7. The same state with `/pr-polish --rounds 5` is a no-op pause.

**Compaction awareness**: with a state file present, trust it and apply the rules above — the state file survives compaction, and the heartbeat distinguishes a recent compaction (resume) from a real abandonment (fresh start). With no state file present, start a fresh run. Do not ask.

## Step 1: Sync base via /git:sync-base

```
python3 .claude/skills/git:sync-base/git-sync.py --verbose
```

That script owns rebasing onto `origin/<base>` and (when a PR exists) force-pushing the rebased branch back with precise-lease. Do not reimplement any of it here. On conflict (exit 2) **abort this polish run with `state-mark-complete <ctx> sync-conflict`** and emit the Final Summary pointing at the conflict — do not pause mid-run with `AskUserQuestion`. The user resolves the conflict and re-invokes to pick up.

Build a short `$PR_SUMMARY` from `git log --oneline origin/<base>..HEAD` + diff-stat (≤10 lines). Pass it to `bramble_ops.py goal` every round; the helper returns the right text for the round.

### The goal channel as continuous-conversation context

Each round resumes the same bramble session, so the model accumulates context across turns. The `--goal` field is the only orchestrator-controlled per-turn message bramble's follow-up prompt builder injects (as `Context for this turn: <text>`). Treat it as a short, structured update — what the human reviewer would say at the top of a follow-up review — not as a restated brief.

What it carries:

| Round | Goal text | Why |
|---|---|---|
| 1 | `$PR_SUMMARY` (commit list + diffstat) | First turn: model has nothing yet. Establish PR-level intent and surface area. |
| 2+, prior actions exist | Action-history sentence built from the state file | Tells the resumed model which of its own findings the orchestrator already actioned, so it doesn't re-raise them. |
| 2+, no prior actions yet | Falls back to `$PR_SUMMARY` | Round 1 was empty; better to re-anchor than send nothing. |

Action-history shape (emitted by `action_history_goal`):

```
Round 3. Prior rounds fixed: a.go:10 codex; b.py:42 cursor.
Skipped: c.go:8 wont_fix (design tradeoff); d.go:5 stale.
```

Why this beats restating the PR intent every round:

- **Don't-reflag cue.** Resumed models otherwise tend to re-surface their own prior findings, especially after a fix changes neighboring code. A literal "fixed: X; skipped: Y" line costs ~50 tokens and reliably suppresses recurrence-noise. This same biasing is also why there's no separate "review the new commits" Monitor: continuous review with session resume already gives the new turn a recency-weighted view; adding another arm just doubles cost.
- **Skip-reason transparency.** When the orchestrator wrote `wont_fix` or `stale`, the model needs to know *why* — otherwise it will keep arguing the original point. Including the bracketed reason (`(design tradeoff)`, `(stale)`) closes that loop.
- **Recency anchor.** The round number is a simple turn counter that helps the model frame its response ("on this turn I'll focus on …") rather than re-relitigating round 1.
- **Bounded length.** Capped at `_ACTION_HISTORY_CAP` per bucket with a `(N more)` suffix; the full audit trail lives in `comment_actions`, not the prompt.

What the goal channel deliberately does **not** carry:

- The full diff or new commit shas — bramble re-snapshots the worktree on each turn, so the model sees the post-fix code directly.
- Per-finding rationale text — that belongs in the inline reply on the PR (see Step 3d), not in the model context.
- Round-1 PR_SUMMARY repeated — the session already has it. Re-stating wastes tokens and dilutes the don't-reflag signal.

## Step 2: Fetch existing PR comments + failing CI jobs

**Only when `pr_number` is not null.** These are supplementary round-1 triage input — they do **not** replace the local bramble review. Even if remote bots found nothing or only stale issues, always proceed to the round loop and run bramble.

```
python3 $SKILL_DIR/scripts/pr_ops.py fetch-comments > $STATE_DIR/pp-comments.json
python3 $SKILL_DIR/scripts/pr_ops.py ci-failed-tests > $STATE_DIR/pp-ci.json
```

`fetch-comments` emits the wrapped shape `{"comments": [...], "noise_filtered": N, "noise_samples": [...]}`. Three filters run at intake: replies dropped, bot review-summary boilerplate dropped, and bot process-noise (linear linkbacks, claude-bot "Reviewing PR..." progress posts) dropped into `noise_*`. Only `comments` flows into triage; the orchestrator passes `noise_filtered` / `noise_samples` into `state-append-round --noise-filtered N --noise-samples <file>` on round 1 so the audit trail records what was dropped. `bramble_ops.py triage --pr-comments` accepts both the wrapped shape and the legacy bare-list shape, so older state files keep working. `ci-failed-tests` classifies each failing job as flake vs real (ETXTBSY, bazel-cache, `ci_deadline` → flake; anything else → real). Branch-only mode skips both files; triage reads them only when round == 1.

## Step 3: Round loop

```
for round = 1..ROUNDS:
  a) commit any pending changes (WIP ok)
  b) launch bramble codex + cursor via Monitor (parallel)
  c) triage findings (+ pr-comments + ci-failures if round 1)
  d) apply fixes — spawn fixer Agent if >8 findings or many files
  e) if fixes applied: run quality gates, commit locally (NO push)
  f) finalize round state
  g) check convergence; exit if met
```

Header per round: `## Round N / ROUNDS`.

### a) Pending-change WIP commit

Bramble snapshots the working tree at launch. Uncommitted changes won't be reviewed. Make a cheap `git add -A && git commit -m "pr-polish: round N snapshot"` if there's anything dirty. Later commits (step e) amend or replace — the summary is rebuilt from the final state file anyway.

### b) Launch bramble + lint gate

Create a fresh `$LOG_DIR=$STATE_DIR/r$ROUND/`.

**First, compute the scope-hints file for this round.** `scope_gate.py` walks the diff, enumerates co-located test files, and detects multi-package PRs; `bramble code-review --scope-hints-file <path>` then widens its prompt with a test-quality clause and (when triggered) a cross-service contract sweep. The kernel-PR audit showed ~5/14 of substantive post-push bot comments would have been caught with that wider scope (tautological asserts in tests, broad `Exception` catches, route-table desync).

Run it once per round, **before** arming the bramble Monitors — the scope set grows as new tests/helpers land in earlier rounds. The script overwrites `$STATE_DIR/scope-hints.json` each round and always exits 0 (bramble's malformed-file fallback covers any edge). Cost is ~100ms vs 60–400s per backend turn, so it's noise.

```bash
SCOPE_HINTS=$(python3 $SKILL_DIR/scripts/scope_gate.py \
  --state-dir "$STATE_DIR" 2>"$LOG_DIR/scope-gate-stderr.txt")
```

Then arm Monitors in the same turn — always codex + cursor + lint, plus gemini when `--gemini` was passed. The bramble Monitors all pass `--scope-hints-file "$SCOPE_HINTS"`; the lint Monitor doesn't (lint_gate has its own diff walk).

Compute the round's `--goal` text (and the resume id, when there is one) from the state file:

```bash
GOAL=$(python3 $SKILL_DIR/scripts/bramble_ops.py goal {ROUND} \
        --pr-summary "$PR_SUMMARY" --state-file "$STATE_FILE")
CODEX_RESUME=$(python3 -c "import sys, json; sys.path.insert(0, '$SKILL_DIR/scripts'); \
  from bramble_ops import prior_session_id; print(prior_session_id(json.load(open('$STATE_FILE')) if '$STATE_FILE' else None, 'codex', {ROUND}))")
CURSOR_RESUME=$(python3 -c "...same for cursor...")
```

Each Monitor runs `bramble code-review` directly with the round's goal and (round 2+) resume flag. Step 0.2 already proved `--resume-session-id` is supported.

```
ENVELOPE_CODEX="$LOG_DIR/codex-envelope.json"
ENVELOPE_CURSOR="$LOG_DIR/cursor-envelope.json"
ENVELOPE_LINT="$LOG_DIR/lint-envelope.json"

Monitor({
  description: "bramble codex r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:codex:r{ROUND} \
    $BRAMBLE_BIN code-review --backend codex --model gpt-5.4-mini \
    --skip-test-execution --verbose --timeout 10m \
    --goal \"$GOAL\" --scope-hints-file \"$SCOPE_HINTS\" \
    ${CODEX_RESUME:+--resume-session-id \"$CODEX_RESUME\"} \
    --envelope-file \"$ENVELOPE_CODEX\" 2>\"$LOG_DIR/codex-stderr.txt\""
})

Monitor({
  description: "bramble cursor r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:cursor:r{ROUND} \
    $BRAMBLE_BIN code-review --backend cursor --model composer-2 \
    --skip-test-execution --verbose --timeout 10m \
    --goal \"$GOAL\" --scope-hints-file \"$SCOPE_HINTS\" \
    ${CURSOR_RESUME:+--resume-session-id \"$CURSOR_RESUME\"} \
    --envelope-file \"$ENVELOPE_CURSOR\" 2>\"$LOG_DIR/cursor-stderr.txt\""
})

// Always: lint gate runs deterministic linters on the diff. ~1-2s typical.
// Closes the post-push CodeQL gap (empty except, unused imports, etc.) that
// kernel-PR analysis showed accounts for ~40% of substantive bot comments
// arriving after we converged. lint_gate.py auto-skips any linter whose
// binary is not on PATH, so this never fails the round.
Monitor({
  description: "lint gate r{ROUND}",
  timeout_ms: 120000,
  persistent: false,
  command: "python3 $SKILL_DIR/scripts/lint_gate.py \
    --state-dir \"$STATE_DIR\" --round {ROUND} \
    2>\"$LOG_DIR/lint-stderr.txt\""
})

// Only when --gemini flag was passed:
Monitor({
  description: "bramble gemini r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:gemini:r{ROUND} \
    $BRAMBLE_BIN code-review --backend gemini --model gemini-3-flash-preview \
    --skip-test-execution --verbose --timeout 10m \
    --goal \"$GOAL\" --scope-hints-file \"$SCOPE_HINTS\" \
    ${GEMINI_RESUME:+--resume-session-id \"$GEMINI_RESUME\"} \
    --envelope-file \"$LOG_DIR/gemini-envelope.json\" 2>\"$LOG_DIR/gemini-stderr.txt\""
})
```

Each Monitor runs independently; a crash in one does not affect the others. `timeout_ms=720000` is bramble's own 10-minute `--timeout` plus two minutes of slack; the lint Monitor uses a 2-minute timeout because deterministic linters complete in seconds. `--skip-test-execution` tells the reviewer not to run tests — quality gates will.

If a backend envelope reports `status: "error"` but `review.raw_text` contains a fenced ```json``` block (cursor occasionally returns malformed JSON that bramble can't unmarshal even though the underlying model output was structured), recover by extracting the inner JSON and synthesizing a clean envelope, then write the recovered envelope to `<backend>-envelope-recovered.json` and pass that path to `triage --stream`. Don't silently drop the round just because the wrapper failed JSON validation — the findings inside `raw_text` are still the model's review.

### c) Triage

```
python3 $SKILL_DIR/scripts/bramble_ops.py triage {ROUND} $STATE_FILE \
    --stream codex=$ENVELOPE_CODEX \
    --stream cursor=$ENVELOPE_CURSOR \
    --stream lint=$ENVELOPE_LINT \
    $( [ "$USE_GEMINI" = "1" ] && echo --stream gemini=$LOG_DIR/gemini-envelope.json ) \
    $( [ "$ROUND" = "1" ] && [ "$PR_NUMBER" != "null" ] && \
       echo --pr-comments $STATE_DIR/pp-comments.json --ci-failures $STATE_DIR/pp-ci.json )
```

`triage` reads envelopes, merges the round-1 PR-comment and CI-failure feeds, and emits:

- `consensus` — same `(file, line)` flagged by ≥2 distinct sources, or same `(file, line, topic)` for sourceless paths. Route to `must_fix`. The location-only key collapses different phrasings of the same finding so two reviewers wording it differently still consolidate.
- `single_critical` — single-source high/critical, or CI failure that isn't a flake. Route to `must_fix`.
- `single_medium` — single-source medium, or GitHub comment without severity-keyword. Route to `consider_fix`.
- `low_acks` — single-source low/nit, or flake CI failure. Route to `batch_ack`.
- `spiral_matches` — new findings whose `(file, line, topic)` matches a prior-round `fixed` action. Route to `escalate`. (Spiral keeps the topic component because exact recurrence matters here — a fix-then-recurrence has identical wording, not just identical location.)

If `spiral_matches` is non-empty, **don't auto-fix** — call `AskUserQuestion` with the spiralling findings. The prior fix may have regressed or the reviewer is re-flagging something we thought we resolved.

If the empty action-plan case fires (nothing in `must_fix`/`consider_fix`, and `batch_ack` is all we have), exit the loop without touching files further.

### d) Apply fixes

Apply the rules from "Ownership and convergence" up top. Two cases need extra rigor:

- **Stale-on-prior-commit PR comments.** Triage's `batch_stale` bucket holds inline comments whose `original_commit_id` no longer matches `pr["head_sha"]` (cursor[bot] / coderabbit comments superseded by amended commits). Auto-acknowledge each as `action: "stale"` with `reason: "Superseded by <short_sha>; comment was anchored to <short_old_sha>."` — no fix attempt, no further triage. Auto-reply per the templates below.
- **Stale finding guard.** Before fixing any bramble finding, verify the cited code still matches the current file; if you made changes between launching bramble and reading results the finding may reference code that no longer exists. When the guard fires, record the finding with `action: "stale"` — silently dropping it blinds the N+1 spiral guard.

Log every triaged finding to `comment_actions`. For bramble findings: `source` is `"codex"` / `"cursor"` / `"gemini"` / `"lint"`, `comment_id: null`. For CI: `source: "ci"`, `path: job_id`, `topic: test_name`. For PR comments: `source: "github-inline"` etc. with `comment_id` from the fetch.

**Who applies the fixes.** Orchestrator applies fixes directly for small action plans. Spawn a fixer `Agent` when:
- More than 5 actionable findings AND they span many files, OR
- Findings require reading large amounts of unfamiliar code.

Fixer agent call:

```
Agent({
  description: "pr-polish fix round {ROUND}",
  subagent_type: "general-purpose",
  model: "{FIXER_MODEL}",  // from --fixer-model, default "sonnet"
  prompt: `<ownership rules> <action plan JSON> <envelope + stderr paths>
           Round 1 also includes unresolved PR comments — post inline replies
           via pr_ops.py reply-inline after fixing.
           Return a fenced ```json``` block of comment_actions as last content.`,
})
```

The `--fixer-model` flag threads straight through to `Agent(model=...)`. Default is `sonnet`; try `opus` for gnarly architectural fixes.

**Auto-reply to PR comments.** For every inline `comment_actions` row whose `comment_id` is non-null AND `action` is one of `fixed`, `stale`, `false_positive`, `wont_fix`, post a reply via `pr_ops.py reply-inline <comment_id> "<body>"` so the bot/human author sees a closure signal in the thread. Templates (one short paragraph each — bots don't need prose, humans skim):

- `fixed`: `Fixed in <short_sha>.`
- `stale`: `Superseded by <short_sha> — the cited code was changed/removed in a later commit. (Auto-reply from /pr-polish.)`
- `false_positive`: `Marked false positive: <reason>. (Auto-reply from /pr-polish.)`
- `wont_fix`: `Won't fix: <reason>. (Auto-reply from /pr-polish.)`

Skip replies for `ack` (low/nit batch acks would spam the thread) and for non-inline rows (`comment_id` null — no thread to reply to). Batch-reply nits at the top level if you must — don't fan out one reply per trivial finding.

### e) Quality gates + commit (only if fixes applied)

Skip this whole step if step d produced zero file changes. No point running lint/tests again if nothing moved.

Follow project quality gates (separate turn from any Monitor arm).

On pass, commit locally with subject `pr-polish round {ROUND}: <summary>` and a body listing fixed/skipped findings. **Do NOT push.**

### f) Finalize round state

Write accumulated `comment_actions` to a temp JSON file, then:

```
python3 $SKILL_DIR/scripts/pr_ops.py state-finalize-round $CTX $ROUND $(git rev-parse HEAD) \
    $STATE_DIR/actions-r$ROUND.json \
    --envelope codex=$ENVELOPE_CODEX \
    --envelope cursor=$ENVELOPE_CURSOR \
    --envelope lint=$ENVELOPE_LINT \
    $( [ "$USE_GEMINI" = "1" ] && echo --envelope gemini=$ENVELOPE_GEMINI )
```

The `--envelope` flags tell finalize where to read each backend's envelope so `rounds[n].session_ids` and `rounds[n].resume_status` get populated for the next round's resume plumbing. Pass `--envelope lint=...` too — the lint backend doesn't carry a session id, but its envelope still feeds `lint_findings` into the persisted reviews directory. Without these, finalize falls back to `bramble_ops.envelope_path()`'s `/tmp` legacy convention and silently misses operator-controlled paths under `$STATE_DIR/r$ROUND/`.

`state-finalize-round` also auto-populates `rounds[n].ci_findings` from `ci_failed_tests` when a PR exists; branch-only runs leave it empty.

### g) Convergence check

Apply the "Stop when converged" rules from the top. If converged, break out of the loop. If `round == ROUNDS` and not converged, produce Final Summary and `AskUserQuestion` for explicit approval to continue.

Track progress concisely:
```
Round 1: codex=3 (2h,1m), cursor=4 (1h,3m), pr_comments=2, ci=0 -> fixed 7, skipped 1 -> continue
Round 2: codex=1 (1m), cursor=1 (1m, same) -> consensus, fixed 1 -> continue
Round 3: codex=0, cursor=0 -> EXIT (converged)
```

## Step 4: Push once on loop exit

**Why defer push.** Every push to a PR's branch triggers whatever GitHub bots are configured (CodeRabbit, Cursor Bugbot, coderabbit, etc.) to re-review. If we pushed after every round the bots would spend their budget scanning intermediate commits — review N+1 sees the round-N-fix diff and reliably generates new comments on it, even when the round-N fix was correct. By batching all commits and pushing once at loop exit, the bots see the polished tree. CI likewise runs once on the final state. This is the single most important reason this skill doesn't push mid-loop.

```
git push --force-with-lease --force-if-includes origin HEAD
```

Branch-only mode on first push: `git push -u origin <branch>` (no prior remote to protect with `--force-with-lease`).

## Step 5: Final summary + mark complete

```
python3 $SKILL_DIR/scripts/pr_ops.py state-mark-complete $CTX <reason>
```

**Reason values**: `converged`, `all-low`, `false-positive-top`, `trend-down`, `capped-at-max`, `user-paused`, `spiral-escalated`, `sync-conflict`, `abandoned` (set by `state-mark-abandoned` from Step 0.5 when the prior heartbeat went stale; never passed to `state-mark-complete` directly).

Print a Markdown summary with these sections:

- Top metrics — rounds completed, comments addressed, commits pushed, convergence signal, verdict (Ready / Not ready).
- Round-by-round table — codex/cursor findings, fixed counts, one-line note per round.
- Comment Actions table — every `comment_actions` row across rounds, sorted by round then severity desc; columns `Round | Source | Path:Line | Severity | Action | Notes`. Use `-` for null path/line.

Tell the user the state file path so they can read the raw per-finding decisions, and that it is preserved, not deleted. If converged, say the PR is ready to merge; otherwise list the remaining issues.
