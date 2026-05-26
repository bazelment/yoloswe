---
name: pr-polish
description: Fully autonomous PR polish loop. Runs N rounds of local bramble review (codex + cursor, optionally + gemini), folds in any existing PR comments and CI failures as round-1 input, fixes findings locally, pushes once at the end.
argument-hint: "[--rounds N] [--gemini]"
disable-model-invocation: true
---

# PR Polish Loop

Autonomous orchestrator that brings a branch from "has issues" to "ready to merge." Each round runs code review agents as the authoritative review signal, triages findings, applies fixes locally, and commits. **No pushes happen until the loop exits** — batching keeps each round's bramble review scoped to local code without triggering repeated GitHub-bot re-reviews on intermediate commits.

The loop exits when the review has converged (see "Convergence") or when the round cap is hit. On exit the orchestrator force-pushes the accumulated commits so bots and CI see one polished tree.

Shell plumbing lives in Python helpers under `scripts/`. Use `SKILL_DIR=.claude/skills/pr-polish` and call them as `python3 $SKILL_DIR/scripts/<helper>.py …`.

- `pr_ops.py` — PR/branch identity, comment fetch/reply, CI failure detail, state I/O.
- `bramble_ops.py` — `--goal` / action-history text, session-resume id selection with series-boundary detection, envelope parsing, triage, and PR-comment / CI-failure → finding adapters. The `bramble code-review` invocation itself lives in this skill body.
- `lint_gate.py` — Deterministic ruff/golangci/eslint pass on the diff; closes the CodeQL noise gap.
- `scope_gate.py` — Computes `scope-hints.json` for `bramble code-review --scope-hints-file`. Runs once per round before bramble Monitors.

**Failed review streams are findings, not silence.** Missing envelopes, non-zero exits, or `status: "error"` envelopes must surface in the round summary with the stderr path cited.

**Base-branch syncing is delegated.** Invoke `.claude/skills/git:sync-base/git-sync.py --verbose` directly — that skill owns rebasing, precise-lease force-push, and conflict handling.

## Arguments

| Flag | Default | Meaning |
|---|---|---|
| `--rounds N` | `5` | Run up to N additional rounds from current state. Resuming at `current_round=5` with `--rounds 2` runs rounds 6 and 7. `--rounds 0` is a no-op. The budget is per-invocation: re-invoking with the same `--rounds` after a converged or capped exit gives N fresh rounds. |
| `--gemini` | off | Also run a third reviewer with `--backend gemini --model gemini-3-flash-preview`. A finding agreed on by ≥2 sources counts as consensus. |

PR/branch context is auto-detected by `pr_ops.py identify`.

## Ownership and convergence

**Fix, don't dodge.**
- Pre-existing code in touched files: own it.
- Consensus findings (two reviewers at the same `(file, line)`, regardless of wording): mandatory fix.
- High/medium: fix unless provably false positive (cite refuting file:line).
- Low/nit: fix if trivial, else skip with one-line justification.
- Valid skip reasons: `false_positive` (with evidence), `wont_fix` (design tradeoff), `stale` (cited code gone).

**A finding is a symptom; the fix is the cure.** Each cited finding observes one underlying problem. Before patching, identify the actual invariant being violated, look for sibling sites in the same module, and decide whether the right fix is at the cited line or upstream. A defensive guard at every consumer often signals a producer should normalize once. If docs or tests pin the contract that just changed, they're part of the same fix. A cascade of medium-severity rounds is usually one missed invariant.

**The reviewer now names invariants for you.** Bramble code-review v2 lets the model emit `issues[].invariant` + `issues[].sites[]` when it finds N ≥ 2 sibling sites of one rule. When a finding carries `invariant`, the goal-builder mirrors the name into next round's prompt so the resumed reviewer folds new sites into the same finding rather than re-flagging per-site. Two reviewers naming the same invariant — even at different sites — also form consensus, routing every site to `must_fix` in one triage pass. Read `comment_actions[].invariant` when triaging: if it's set, the producer-side fix is usually the right move; record any consumer site you intentionally leave alone as `ack` with the reason.

Class-level fixes beyond the cited line are logged in `comment_actions` as `source: "sweep"`, `comment_id: null`, `topic: "<original-topic> — class-level fix"`.

**Convergence — stop when any one holds:**
- Zero findings.
- 2 consecutive rounds where top severity is low across all sources, AND every low has been fixed or recorded as `ack`/`wont_fix` with a written reason. The streak is persisted in `rounds[n].low_only_streak` (incremented when this round's `top_severity ∈ {low, nit, null}`, reset to 0 otherwise). Read it via `state-load`.
- Top finding is a documented false positive AND the prior round had no `must_fix`.
- Empty action plan after triage.

If the last 2 rounds were low-only, treat as converged regardless of remaining `--rounds` budget — the streak rule wins over the budget. The motivation: the reviewer is incentivized to find *something* every round, and one persistent low keeps the loop alive forever otherwise. After two rounds where everything triaged was low/nit, the next round of fixes is rounding error.

**Hard stop after N additional rounds.** When `additional_rounds_run` reaches `--rounds` without convergence, produce Final Summary, then `AskUserQuestion` whether to continue.

## Auto-decision rules

Bias for action and your own judgement with research. Only three things pause the loop with `AskUserQuestion`:

1. **Integrity gate** — stale state file with PR mismatch (Step 0.5).
2. **Budget gate** — `--rounds N` reached without convergence.
3. **Regression gate** — **unverified** `spiral_matches` non-empty. Single-source spirals auto-demote to `batch_stale` when *either* of two heuristics fires: (a) the cited evidence is no longer at HEAD within ±10 lines of the cited line, or (b) the cited file:line falls inside a hunk that the **immediately-prior** round modified (read from `head_before..head_after` of round N-1 only — not any ancestor round, which would let a long audit suppress real regressions on any file an early round happened to touch). Both are pure git/state lookups, no judgment. Multi-source spirals (≥2 backends agree) always escalate. The audit row records `action: "stale"` with the demote reason. Cost of being wrong: a real regression that gets mis-demoted will resurface next round, and the heuristic catches it on the second occurrence.

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
      "low_only_streak": 0,
      "noise_filtered": 2,
      "noise_samples": [
        {"id": 4300306871, "author": "linear[bot]", "pattern": "linear-linkback"}
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
          "invariant": "ambient env vars shadow explicit proxy keys",
          "action": "fixed",
          "reason": null,
          "commit_sha": "abc123f",
          "reply_url": "https://github.com/owner/repo/pull/2318#discussion_r2034881234"
        }
      ],
      "sufficiency_claims": {
        "codex": {"is_confident_complete": true, "evidence": "all named invariants addressed; remaining medium issues are doc-only"},
        "cursor": {"is_confident_complete": false, "evidence": "suspect more sites in src/streaming/"}
      }
    }
  ]
}
```

`{codex,cursor,gemini}_findings` hold raw issues from each backend's envelope, hydrated by `state-finalize-round`. The verbatim envelope is copied to `<state_dir>/reviews/r<n>-<backend>.json`. `gemini_findings` is omitted when `--gemini` was not passed.

`noise_filtered` / `noise_samples` count bot process-noise dropped at fetch time (linear linkbacks, claude-bot progress posts). These never enter `comment_actions` — kept as a round-level audit trail. Samples capped at 5 entries.

`low_only_streak` is incremented at finalize time when this round's `top_severity` is `low`, `nit`, or `null` (zero findings counts), reset to 0 otherwise. The convergence rule reads `>= 2` to trigger early exit; the goal-builder reads `>= 2` to inject a one-sentence pressure note (see "The goal channel" below).

**`comment_actions` schema — load-bearing, other tooling depends on exact strings:**

- `source`: one of `github-inline`, `github-issue`, `github-review`, `codex`, `cursor`, `gemini`, `lint`, `ci`, `sweep`. `lint` rows route through triage like any other source: a `(file, line)` match with codex or cursor counts as consensus.
- `comment_id`: GitHub id for `github-*`; `null` for bramble/CI/lint/sweep findings (bramble dedupes by `(path, line, topic)`; CI dedupes by `(job_id, test_name)` where `path=job_id` and `topic=test_name`).
- `path` / `line`: `null` for top-level PR + review-level comments.
- `severity`: `high`, `medium`, `low`, `nit`, or `null`.
- `action`:
  - `fixed` — code change applied; `commit_sha` required.
  - `false_positive` — `reason` required (refuting file:line).
  - `wont_fix` — `reason` required (design tradeoff).
  - `ack` — low/nit batch-acknowledged. Counts as skipped.
  - `stale` — cited code no longer exists.
  - `pre_existing` — `source: "ci"` only. Failure also fails on base branch; `reason` cites `ci-compare-base` output. Counts as skipped.
  - `flake` — `source: "ci"` only. `reason` names the `flake_reason` (ETXTBSY, bazel cache, `ci_deadline`). Counts as skipped.
- `invariant` (optional, v2): name of the class-level rule the reviewer claimed when it emitted an `invariant + sites[]` finding. Surfaced verbatim from the envelope. Reviewer-emitted; the orchestrator does not synthesize. Used by the next round's goal-builder to remind the resumed reviewer to fold new sites into the same finding rather than re-flag.

**`sufficiency_claims` (optional, v2)**: per-backend dict capturing each reviewer's self-assessment for this round. Each entry: `{"is_confident_complete": bool, "evidence": "..."}`. Populated by `state-finalize-round` when the envelope's `review.sufficiency` is present. Absence means the reviewer didn't claim either way — do not infer. The orchestrator surfaces consensus claims in round summaries and final reports as audit-trail context; this is NOT a new exit gate, the existing convergence rules still decide.

**Context token for state subcommands.** All `state-*` take a single positional `ctx` — the bare PR number when one exists, otherwise `branch:<name>`.

**State writes go through the module, never hand-rolled file I/O.** All writes are atomic:

- `state-append-round <ctx> <n> <head_before>` at round start. Verifies `git rev-parse HEAD == head_before`; pass `--no-verify-head` only when resuming an interrupted round.
- `state-finalize-round <ctx> <n> <head_after> <actions.json>` at round end.
- `state-mark-complete <ctx> <reason>` on exit.
- `state-load <ctx>` to read.

## Step 0: Identify context

```
python3 $SKILL_DIR/scripts/pr_ops.py identify
```

Returns `{pr_number, title, url, base, head, branch, owner, repo, owner_repo, state_dir, state_file}`. `pr_number` is `null` for branches without a PR — the rest of the flow still works, just with PR-comment and CI-failure fetches skipped. Pin `$CTX` to either the PR number or `branch:<head>`.

### Step 0.1: Preflight — resolve binaries + helpers in one call

```bash
PREFLIGHT=$(python3 $SKILL_DIR/scripts/pr_ops.py preflight)
export BRAMBLE_BIN=$(echo "$PREFLIGHT" | jq -r .bramble_bin)
GIT_SYNC=$(echo "$PREFLIGHT" | jq -r .git_sync_path)
# Non-empty errors[] means a hard precondition failed (bramble missing
# --resume-session-id support, git-sync not on disk). Surface the list
# and exit before launching any Monitor.
if [ "$(echo "$PREFLIGHT" | jq -r '.errors | length')" != "0" ]; then
  echo "preflight errors:" >&2
  echo "$PREFLIGHT" | jq -r '.errors[]' >&2
  exit 1
fi
```

`preflight` resolves the bramble binary (preferring the worktree-local `bazel-bin/` artifact over `$PATH`), probes for `--resume-session-id` support, locates `git:sync-base/git-sync.py` (skill-local → installed fallback), and probes for `--no-push` support. Every `bramble code-review` invocation references `$BRAMBLE_BIN`.

## Step 0.5: Resume check

```
python3 $SKILL_DIR/scripts/pr_ops.py state-load $CTX
```

`state-load` returns the persisted state plus two derived booleans (computed at read time): `is_heartbeat_stale` and `is_first_round_of_series`. The second is true when this round is round 1 with no prior history or follows a `completed: true` state — in both cases treat it as a real round 1 (re-fetch PR comments + CI failures, skip bramble session resume). Capture before `state-append-round` clears `completed`:

```bash
IS_NEW_SERIES=$(python3 $SKILL_DIR/scripts/pr_ops.py state-is-new-series $CTX $ROUND)
```

Compare against `git rev-parse HEAD`:

- **No state file**: fresh run. Proceed.
- **`pr_number` mismatch**: integrity gate. `AskUserQuestion` whether to discard.
- **`is_heartbeat_stale: true`** AND `completed: false` (heartbeat older than 2h, or missing): prior run abandoned. Tombstone with `state-mark-abandoned $CTX` and start fresh on current HEAD. Announce in one line; do not ask.
- **HEAD matches `last_commit_at_round_start`** (heartbeat fresh): prior round interrupted. Resume in-progress round.
- **HEAD differs from `last_commit_at_round_start`** (heartbeat fresh): prior round committed or user made manual changes. Auto-start next round on current HEAD. Re-invocation is the user's signal that they want another round. The new `state-append-round` clears any stale `completed: true` set by a prior `state-mark-complete`.

Budget tracking is **per-invocation**, not derived from `current_round`. Initialize `additional_rounds_run = 0` at the top of this invocation; increment after every finalized round. The budget gate hard-stops when `additional_rounds_run >= --rounds` and `AskUserQuestion`s to authorize more rounds. State persistence does not record this counter — re-invoking after a converged or capped exit gives a fresh `--rounds` budget by definition.

With a state file present, trust it and apply the rules — heartbeat distinguishes recent compaction from real abandonment. With no state file, start fresh. Do not ask.

When you need to know whether the local branch and remote are in sync (e.g. surfacing "git-sync already pushed; nothing to push at end" in the round summary), use `pr_ops.py remote-head <branch>` — it routes through `git ls-remote`, not `git rev-parse origin/<branch>` (which lags in worktrees).

## Step 1: Sync base via /git:sync-base

```
python3 .claude/skills/git:sync-base/git-sync.py --verbose --no-push
```

That script owns rebasing onto `origin/<base>`. Pass `--no-push` so the rebase stays local — the loop accumulates commits and pushes once at Step 4, matching the "no mid-loop push" promise at the top of this file. Without `--no-push` the script force-pushes the rebased branch immediately and triggers GitHub bots before round 1's review even starts.

**Clean-tree preflight.** Before invoking git-sync, if `git status --porcelain` is non-empty AND state has no in-progress round to resume, print one line ("uncommitted changes — commit, stash, or rebase manually before /pr-polish") and call `state-mark-complete <ctx> dirty-tree-preflight`. Do NOT `AskUserQuestion`. Snapshot-commit-before-push races (codex-feedback 2026-05-19, item #2) come from skipping this gate; the local snapshot + base-sync push showed bots an intermediate tree.

On conflict (exit 2) **abort with `state-mark-complete <ctx> sync-conflict`** and emit Final Summary pointing at the conflict — do not pause mid-run.

Build a short `$PR_SUMMARY` from `git log --oneline origin/<base>..HEAD` + diff-stat (≤10 lines). Pass it to `bramble_ops.py goal` every round.

### The goal channel as continuous-conversation context

Each round resumes the same bramble session, so the model accumulates context across turns. `--goal` is the only orchestrator-controlled per-turn message bramble injects (as `Context for this turn: …`). Treat it as a short structured update — what a human reviewer would say at the top of a follow-up — not a restated brief.

| Round | Goal text | Why |
|---|---|---|
| 1 | `$PR_SUMMARY` | First turn: establish PR-level intent and surface area. |
| 2+ | Per-turn briefing: prior round's fixed/skipped + files changed; falls back to `$PR_SUMMARY` when there's nothing to say | Tells the resumed model what was actioned (so it doesn't re-flag fixes) and which files moved. |

Edge cases: when round 2+ has no prior-round actions but files did change, the goal opens with `Round N.` plus the files-changed line; when both are empty (pristine round, no diff since prior), we re-anchor to `$PR_SUMMARY` rather than send a goal that's just "Round N."

Action-history shape (`action_history_goal`, only the immediately-prior round):

```
Round 6. Prior round fixed: a.go:10 — null check missing on BUILDER_LITE.
Skipped: b.py:42 wont_fix: design tradeoff; d.go:8 ack: rename helper.
Files changed since round 5: a.go, b.py.
```

The `fixed: X — topic; skipped: Y verb: reason` shape stops the resumed model from re-flagging its own prior findings. Source labels (`(codex)`/`(cursor)`) are deliberately omitted. Each entry capped at `_TOPIC_CHAR_CAP=80`; bucket capped at `_ACTION_HISTORY_CAP=20` with a `(N more)` suffix. The "Files changed" line is the diff between the prior round's `head_after` (falling back to `head_before` for an interrupted prior round) and current HEAD; omitted when empty.

**Inter-round diff (D1).** `bramble_ops.py goal` also appends `git diff <prior_head_after>..<HEAD>` (truncated at 200 lines with `...elided N lines` footer when over) under a `Diff since round N-1:` header. Skipped when the prior round never finalized or the SHAs are unreachable. The reviewer is resuming the same session and re-reading the worktree at launch; the diff between rounds is the actual delta worth scanning.

**Convergence pressure when low-only streak ≥ 2 (B1).** When the prior round's `low_only_streak >= 2`, the goal-builder appends one sentence: "The last N rounds returned only low-severity findings. The fixer treats your output as authoritative, and every finding costs a round; if the diff has no structural issue, returning zero findings is the right call." One sentence, one trigger — not a table of phrasings. The reviewer is incentivized to find *something* every round and one persistent low keeps the loop alive forever otherwise; this just states the cost frame.

The goal channel deliberately does **not** carry: `stale` actions (already absent from the snapshot), per-finding rationale text written into PR replies, earlier rounds' actions (already in session history), or repeated round-1 PR_SUMMARY.

## Step 2: Fetch existing PR comments + failing CI jobs

**Only when `pr_number` is not null.** Supplementary triage input for the *first round of a series* — they don't replace the local bramble review. Always proceed to the round loop and run bramble even if remote bots found nothing.

```
python3 $SKILL_DIR/scripts/pr_ops.py fetch-comments > $STATE_DIR/pp-comments.json
python3 $SKILL_DIR/scripts/pr_ops.py ci-failed-tests > $STATE_DIR/pp-ci.json
```

`fetch-comments` emits `{"comments": [...], "noise_filtered": N, "noise_samples": [...]}`. Three filters at intake: replies, bot review-summary boilerplate, and bot process-noise (linear linkbacks, claude-bot progress posts). Only `comments` flows into triage; pass `noise_filtered` / `noise_samples` into `state-append-round --noise-filtered N --noise-samples <file>` on round 1. `ci-failed-tests` classifies each failing job as flake vs real (ETXTBSY, bazel-cache, `ci_deadline` → flake; else real). Branch-only mode skips both. Triage reads them only when `IS_NEW_SERIES=1`. Re-fetch on every series start so newly-arrived bot comments aren't dropped.

## Step 3: Round loop

```
additional_rounds_run = 0
while additional_rounds_run < --rounds:
  ROUND = current_round_from_state + 1   # absolute round number for state
  a) commit any pending changes (WIP ok)
  b) launch bramble codex + cursor + lint via Monitor (parallel)
  c) triage findings (+ pr-comments + ci-failures if new series)
  d) read findings holistically, cross-reference code, apply fixes
  e) if fixes applied: run quality gates, commit locally (NO push)
  f) finalize round state
  g) check convergence; exit if met
  additional_rounds_run += 1
```

Header per round: `## Round N (M / --rounds in this invocation)` where N is the absolute round number persisted in state and M is `additional_rounds_run + 1`.

### a) Pending-change WIP commit

Bramble snapshots the working tree at launch — uncommitted changes won't be reviewed. `git add -A && git commit -m "pr-polish: round N snapshot"` if dirty. Later commits in step e replace this; the summary is rebuilt from the final state file anyway.

### b) Launch bramble + lint gate

Create a fresh `$LOG_DIR=$STATE_DIR/r$ROUND/`.

**First, compute the scope-hints file.** `scope_gate.py` walks the diff, enumerates co-located test files, detects multi-package PRs, and writes `$STATE_DIR/scope-hints.json`. `bramble code-review --scope-hints-file <path>` widens its prompt with a test-quality clause and (when triggered) a cross-service contract sweep. Run once per round, **before** arming bramble Monitors. Always exits 0.

```bash
SCOPE_HINTS=$(python3 $SKILL_DIR/scripts/scope_gate.py \
  --state-dir "$STATE_DIR" 2>"$LOG_DIR/scope-gate-stderr.txt")
```

Then arm Monitors in the same turn — codex + cursor + lint always, plus gemini when `--gemini`. Bramble Monitors pass `--scope-hints-file "$SCOPE_HINTS"`; lint has its own diff walk.

Compute the round's `--goal` text and per-backend resume id. Either run the helpers individually, or pull everything in one call via `round-bundle` and split with `jq`:

```bash
BUNDLE=$(python3 $SKILL_DIR/scripts/pr_ops.py round-bundle "$CTX" {ROUND})
GOAL=$(echo "$BUNDLE" | jq -r .goal_text)
CODEX_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.codex')
CURSOR_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.cursor')
# round-bundle threads its own PR_SUMMARY-free goal; on round 1 you still
# want `$PR_SUMMARY` as the goal, so override:
[ "{ROUND}" = "1" ] && GOAL="$PR_SUMMARY"
```

Or the long form, one helper per piece:

```bash
GOAL=$(python3 $SKILL_DIR/scripts/bramble_ops.py goal {ROUND} \
        --pr-summary "$PR_SUMMARY" --state-file "$STATE_FILE" \
        --head-before "$(git rev-parse HEAD)" \
        --is-new-series "$IS_NEW_SERIES")
CODEX_RESUME=$(python3 $SKILL_DIR/scripts/bramble_ops.py prior-session-id codex {ROUND} \
                --state-file "$STATE_FILE" --is-new-series "$IS_NEW_SERIES")
CURSOR_RESUME=$(python3 $SKILL_DIR/scripts/bramble_ops.py prior-session-id cursor {ROUND} \
                --state-file "$STATE_FILE" --is-new-series "$IS_NEW_SERIES")
```

`prior-session-id` returns empty across series boundaries so a new audit gets a fresh session, and every K=4 rounds within a series (E2) — accumulated session context compounds staleness across long audits. Override with `--session-reset-k N`; pass 0 to disable. Spiral demote also fires (E1) when the cited file:line falls inside a hunk any prior round modified — see "Auto-decision rules → Regression gate". Both are pure git+state lookups; no judgment.

When `IS_NEW_SERIES=1`, re-fetch PR comments and CI failures (prior series' fetch is now stale):

```bash
[ "$IS_NEW_SERIES" = "1" ] && [ "$PR_NUMBER" != "null" ] && {
  python3 $SKILL_DIR/scripts/pr_ops.py fetch-comments > $STATE_DIR/pp-comments.json
  python3 $SKILL_DIR/scripts/pr_ops.py ci-failed-tests > $STATE_DIR/pp-ci.json
}
```

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

Monitor({
  description: "lint gate r{ROUND}",
  timeout_ms: 120000,
  persistent: false,
  command: "python3 $SKILL_DIR/scripts/lint_gate.py \
    --state-dir \"$STATE_DIR\" --round {ROUND} \
    2>\"$LOG_DIR/lint-stderr.txt\""
})

// Only when --gemini was passed:
ENVELOPE_GEMINI="$LOG_DIR/gemini-envelope.json"
GEMINI_RESUME=$(python3 $SKILL_DIR/scripts/bramble_ops.py prior-session-id gemini {ROUND} \
                --state-file "$STATE_FILE" --is-new-series "$IS_NEW_SERIES")

Monitor({
  description: "bramble gemini r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:gemini:r{ROUND} \
    $BRAMBLE_BIN code-review --backend gemini --model gemini-3-flash-preview \
    --skip-test-execution --verbose --timeout 10m \
    --goal \"$GOAL\" --scope-hints-file \"$SCOPE_HINTS\" \
    ${GEMINI_RESUME:+--resume-session-id \"$GEMINI_RESUME\"} \
    --envelope-file \"$ENVELOPE_GEMINI\" 2>\"$LOG_DIR/gemini-stderr.txt\""
})
```

Each Monitor runs independently; a crash in one doesn't affect the others. `timeout_ms=720000` is bramble's 10-minute `--timeout` plus two minutes of slack. `--skip-test-execution` defers tests to quality gates.

If a backend envelope reports a verdict-validation `status: "error"` (cursor returning `approve_with_notes`, `request_changes`, etc.) with populated `review.issues` underneath, run `python3 $SKILL_DIR/scripts/bramble_ops.py recover-envelope <path>` and pass the printed path to `triage --stream`. The helper is idempotent — it returns the original path unchanged when no recovery applies, so it's safe to wrap every `--stream` argument unconditionally. Recovery vocabulary is documented in the helper's docstring.

**Back-compat note (v2 envelopes).** The bramble code-review wrapper now normalizes common verdict aliases (`approve_with_notes`/`approve`/`lgtm`/`request_changes`/etc.) inside `validateReviewBody` so envelopes emitted by an up-to-date bramble already carry the canonical `accepted`/`rejected` token — `recover-envelope` becomes a no-op there. Keep wrapping `--stream` arguments unconditionally so older envelopes (e.g. when an operator runs an unbuilt bramble) still recover; the cost is one stat() per stream.

**Wait for the Monitor barrier with one tool call (mandatory).** Steps b→c must complete in a single turn. After arming Monitors, issue ONE `Bash` call with `run_in_background: true` whose body is an `until`-loop that exits when every armed envelope is non-empty OR a bounded timeout elapses. When `--gemini` is off, set `ENVELOPE_GEMINI` to any always-non-empty path (`/etc/hostname` works) so the same four-clause check works for both modes:

```bash
bash -c 'GC="${ENVELOPE_GEMINI:-/etc/hostname}"; end=$((SECONDS+780)); \
  until [ -s "$ENVELOPE_CODEX" ] && [ -s "$ENVELOPE_CURSOR" ] \
        && [ -s "$ENVELOPE_LINT" ] && [ -s "$GC" ] \
        || [ $SECONDS -ge $end ]; \
  do sleep 2; done'
```

The harness delivers exactly one completion notification when that command exits. Proceed to step c on that notification — even if the loop hit its timeout and some envelopes are still missing or empty. Step c runs `recover-envelope` (idempotent) and `triage` synthesizes a high-severity `stream-missing` finding for any envelope that is absent or unparseable (`bramble_ops.py:666`), so missing envelopes become findings rather than deadlocks.

**Do not emit intermediate `tool_use` blocks between Monitor arm and the barrier notification.** The single `run_in_background` call IS the wait; an "echo waiting" loop is the polling antipattern this rule exists to prevent. The 780s timeout = bramble's `--timeout 10m` + 3 minutes of slack for envelope flush and harness teardown (Monitors themselves use `timeout_ms: 720000`).

Why the `${ENVELOPE_GEMINI:-/etc/hostname}` shape instead of a conditional inside the loop: `${VAR:+&& [ -s "$VAR" ]}` *looks* idiomatic but the parameter expansion produces one string that `[` parses as positional arguments (`[: too many arguments`), not as shell operators. Pre-resolving the path keeps the test syntax static and predictable.

### c) Triage

```
python3 $SKILL_DIR/scripts/bramble_ops.py triage $STATE_FILE \
    --stream codex=$ENVELOPE_CODEX \
    --stream cursor=$ENVELOPE_CURSOR \
    --stream lint=$ENVELOPE_LINT \
    $( [ "$USE_GEMINI" = "1" ] && echo --stream gemini=$LOG_DIR/gemini-envelope.json ) \
    $( [ "$IS_NEW_SERIES" = "1" ] && [ "$PR_NUMBER" != "null" ] && \
       echo --pr-comments $STATE_DIR/pp-comments.json --ci-failures $STATE_DIR/pp-ci.json )
```

`triage` reads envelopes, merges series-start PR-comment and CI-failure feeds, and emits:

- `consensus` — same `(file, line)` from ≥2 sources, or same `(file, line, topic)` for sourceless paths. Route to `must_fix`. Location-only key collapses different phrasings.
- `single_critical` — single-source high/critical, or non-flake CI failure. Route to `must_fix`.
- `single_medium` — single-source medium, or GitHub comment without severity-keyword. Route to `consider_fix`.
- `low_acks` — single-source low/nit, or flake CI failure. Route to `batch_ack`.
- `spiral_matches` — new findings matching a prior-round `fixed` action by `(file, line, topic)` (exact recurrence) or `(file, line)` alone (rewording-resilient). Route to `escalate`.

If `spiral_matches` is non-empty, **don't auto-fix** — `AskUserQuestion` with the spiralling findings.

If the action plan is empty (nothing in `must_fix`/`consider_fix`, only `batch_ack`), exit the loop.

### d) Apply fixes

You apply fixes yourself — your continuity from triage and prior rounds is more valuable than parallelism on a plan this size.

The single most important thing in this step happens *before* you touch any file: read the review report holistically and cross-reference with the code.

The findings are evidence, not a checklist. Two reviewers wording the same problem differently are pointing at one problem. A cited line is a symptom; the underlying invariant may live elsewhere, and the cure often belongs at a producer or shared helper rather than at every consumer the reviewer happened to notice. `action_plan.cluster_hint` shows where findings concentrate, but the codebase tells you where the fix belongs. Reviewers who see one site of a class-level problem will keep finding the next site round after round if you only patch what they cited.

What good looks like by the end of this step:

- Every finding read across all backends as one body of evidence, grouped by underlying problem.
- Cited files opened, sibling sites of each problem checked in the same module (and obviously-related modules).
- Durable fix applied — possibly upstream of the cited line, possibly broader than any single finding.
- When the fix changes behavior, vocabulary, or an invariant, docs and tests that pin it are updated in the same commit.
- Every triaged finding has a `comment_actions` entry. Sites fixed beyond a cited line are logged as `source: "sweep"`.
- Stale buckets honored: `batch_stale` entries auto-acked; bramble findings whose cited code no longer matches are recorded as `action: "stale"` rather than silently dropped (silent drops blind the spiral guard).
- Inline-reply posting on github-inline rows whose action ∈ {`fixed`, `stale`, `false_positive`, `wont_fix`} is handled inside `state-finalize-round`. The orchestrator does not call `reply-inline` directly. `ack` and non-inline rows are intentionally not replied to (notification spam vs signal).

`comment_actions` field shapes by source: bramble (`codex` / `cursor` / `gemini` / `lint`) → `comment_id: null`; CI → `source: "ci"`, `path: <job_id>`, `topic: <test_name>`; PR comments → `source: "github-inline"` / `-issue` / `-review` with `comment_id` from fetch; sweep → `source: "sweep"`, `comment_id: null`, `topic: "<original-topic> — class-level fix"`.

### e) Quality gates + commit (only if fixes applied)

Skip if step d produced zero file changes.

#### Pre-commit fix-completeness checklist

Reviewers who see one site of a class-level problem will keep finding the next site round after round. Before running quality gates, read the diff against your fixes and answer five questions:

1. **Sibling sites** — are there other call sites of the same producer / consumers of the same invariant? If yes, extend the fix or record the asymmetry below.
2. **Tests** — will a reviewer ask for a regression test for this fix? If yes, add it now in the same commit.
3. **Docs** — did this fix change a contract described in a godoc, README, or example file? Update those too.
4. **Type contract** — did you add a new exported helper / method? Document its semantics in a godoc.
5. **Symmetry** — did you fix this in N of M places (e.g. codex but not gemini)? List the M places explicitly and either fix all or record the asymmetry as `ack` with reasoning.

These are questions, not MUSTs — the codebase decides. Recording an intentional asymmetry as `ack` with a one-line reason is fine and beats next round's finding. Silent partial fixes are the failure mode.

Follow project quality gates (separate turn from any Monitor arm). On pass, commit locally with subject `pr-polish round {ROUND}: <summary>` and a body listing fixed/skipped findings. **Do NOT push.**

**Before committing, ask whether the fix is durable.** For each finding addressed: would a reviewer running the same review on the new tree raise the same finding at a different site? If yes, that's a missed sibling — extend the fix. If you deliberately left a sibling unfixed (different semantics, different invariant), record it as `action: "ack"` with a one-line reason. Visible intentional non-uniformity beats next round's finding.

### f) Finalize round state

Write accumulated `comment_actions` to a temp JSON, then:

```
python3 $SKILL_DIR/scripts/pr_ops.py state-finalize-round $CTX $ROUND $(git rev-parse HEAD) \
    $STATE_DIR/actions-r$ROUND.json \
    --envelope codex=$ENVELOPE_CODEX \
    --envelope cursor=$ENVELOPE_CURSOR \
    --envelope lint=$ENVELOPE_LINT \
    $( [ "$USE_GEMINI" = "1" ] && echo --envelope gemini=$ENVELOPE_GEMINI )
```

`--envelope` flags tell finalize where to read each backend's envelope so `rounds[n].session_ids`, `resume_status`, and `sufficiency_claims` populate for next round's resume plumbing and the final report. Pass `--envelope lint=...` too — it has no session id, but its envelope feeds `lint_findings` into the persisted reviews directory. Backends not passed are skipped (no automatic fallback).

`state-finalize-round` auto-populates `rounds[n].ci_findings` from `ci_failed_tests` when a PR exists; branch-only runs leave it empty.

**One-shot variant for the round-summary line.** When you want the orchestrator-visible audit-trail digest in the same call, use `finalize-and-report` instead. Same finalize semantics; emits `{converged_signal, exit_reason_hint, low_only_streak, top_severity, sufficiency_consensus, next_round_n, round_summary}` so the loop printout in Step 3.g doesn't need separate `jq` calls into the state file:

```bash
python3 $SKILL_DIR/scripts/pr_ops.py finalize-and-report $CTX $ROUND $(git rev-parse HEAD) \
    $STATE_DIR/actions-r$ROUND.json \
    --envelope codex=$ENVELOPE_CODEX --envelope cursor=$ENVELOPE_CURSOR \
    --envelope lint=$ENVELOPE_LINT \
    $( [ "$USE_GEMINI" = "1" ] && echo --envelope gemini=$ENVELOPE_GEMINI )
```

`converged_signal` mirrors the existing convergence rules (low_only_streak ≥ 2, or top_severity ∈ {low, nit, null} with no fix/skip activity). It's a *hint*, not a gate — convergence prose at the top of this file is still authoritative. Sufficiency consensus is purely audit-trail context; do NOT treat it as a new exit rule.

### g) Convergence check

Apply the convergence rules from the top. If converged, break. If `additional_rounds_run + 1 == --rounds` and not converged, produce Final Summary and `AskUserQuestion`.

Track progress concisely:

```
Round 1: codex=3 (2h,1m), cursor=4 (1h,3m), pr_comments=2, ci=0 -> fixed 7, skipped 1 -> continue
Round 2: codex=1 (1m), cursor=1 (1m, same) -> consensus, fixed 1 -> continue
Round 3: codex=0, cursor=0 -> EXIT (converged)
```

## Step 4: Push once on loop exit

The loop accumulates commits locally and pushes once at the end so GitHub bots see the polished tree, not intermediate fix diffs. Full rationale lives at `references/why-defer-push.md`.

Before pushing, check whether the remote already holds local HEAD — git-sync may have pushed during the run, and `origin/<branch>` can lag in worktrees. Use `pr_ops.py remote-head <branch>` (which routes through `git ls-remote`, not `git rev-parse origin/<branch>`) so the diagnostic reflects what the remote actually holds:

```bash
SYNC=$(python3 $SKILL_DIR/scripts/pr_ops.py remote-head "$BRANCH")
echo "$SYNC" | jq -r '"local=\(.local_head[0:7])  remote=\(.remote_head[0:7])  in_sync=\(.in_sync)"'
if [ "$(echo "$SYNC" | jq -r .in_sync)" = "true" ]; then
  echo "remote already has local HEAD — skip push"
else
  git push --force-with-lease --force-if-includes origin HEAD
fi
```

Branch-only first push: `git push -u origin <branch>` (no prior remote to protect; `remote_present: false` from `remote-head` is the signal).

## Step 5: Final summary + mark complete

```
python3 $SKILL_DIR/scripts/pr_ops.py state-mark-complete $CTX <reason>
```

**Reason values**: `converged`, `all-low`, `false-positive-top`, `trend-down`, `capped-at-max`, `user-paused`, `spiral-escalated`, `sync-conflict`, `abandoned` (set by `state-mark-abandoned`; never passed to `state-mark-complete` directly).

Print a Markdown summary:

- Top metrics — rounds completed, comments addressed, commits pushed, convergence signal, verdict (Ready / Not ready).
- Round-by-round table — codex/cursor findings, fixed counts, one-line note per round.
- Comment Actions table — every `comment_actions` row across rounds, sorted by round then severity desc; columns `Round | Source | Path:Line | Severity | Action | Notes`. Use `-` for null path/line.

Tell the user the state file path (preserved, not deleted). If converged, say the PR is ready to merge; otherwise list remaining issues.
