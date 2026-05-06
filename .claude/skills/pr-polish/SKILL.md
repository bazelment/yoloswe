---
name: pr-polish
description: Fully autonomous PR polish loop. Runs N rounds of local bramble review (codex + cursor, optionally + gemini), folds in any existing PR comments and CI failures as round-1 input, fixes findings locally, pushes once at the end. Works on both PRs and branches-without-PRs.
argument-hint: "[--rounds N] [--fixer-model MODEL] [--gemini]"
disable-model-invocation: true
---

# PR Polish Loop

Autonomous orchestrator that brings a branch from "has issues" to "ready to merge." Each round runs bramble codex + cursor as the authoritative review signal, triages findings against the action-plan rules, applies fixes locally, and commits. **No pushes happen until the loop exits** — this keeps each round's bramble review scoped to local code without triggering repeated GitHub-bot re-reviews that would only add N+1 diff spiral noise.

The loop exits when the review has **converged** (see "Ownership and convergence") or when the round cap is hit. On exit the orchestrator force-pushes the accumulated commits so the PR's bot/CI review sees one polished tree instead of N intermediate ones.

All shell plumbing lives in two Python modules bundled with this skill at `scripts` directory. Use `SKILL_DIR=~/.claude/skills/pr-polish` and call them as `python3 $SKILL_DIR/scripts/pr_ops.py ...`.

- `pr_ops.py` — PR/branch identity, comment fetch/reply, CI failure detail, state I/O.
- `bramble_ops.py` — Bramble launch, per-round temp files, envelope parse, consensus/triage/N+1 spiral, PR-comment + CI-failure merging.


**Base-branch syncing is not this skill's job.** Invoke `~/.claude/skills/git:sync-base/git-sync.py --verbose` directly — that skill owns branch rebasing, precise-lease force-push, and conflict handling.

## Arguments

| Flag | Default | Meaning |
|---|---|---|
| `--rounds N` | `5` | Maximum rounds before hitting the hard-stop gate. Replaces the old `MAX_ROUNDS` constant. |
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

- `source`: one of `github-inline`, `github-issue`, `github-review`, `codex`, `cursor`, `gemini`, `ci`.
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

**Context token for state subcommands.** All `state-*` subcommands take a single positional `ctx` — either the bare PR number (`2318`) or `branch:<name>` (`branch:feature-foo`). `identify` tells you which one applies:

- PR exists → use the `pr_number` it returned.
- PR absent → use `f"branch:{pr['branch']}"`.

**Writing state goes through the module, not hand-rolled file I/O.** All writes are atomic:

- `state-append-round <ctx> <n> <head_before>` at round start. Verifies `git rev-parse HEAD == head_before`; non-zero exit if the orchestrator raced a commit. Pass `--no-verify-head` only when resuming an interrupted round.
- `state-finalize-round <ctx> <n> <head_after> <actions.json>` at round end.
- `state-mark-complete <ctx> <reason>` on exit.
- Reading: `state-load <ctx>`.

## Parallel Call Safety

When multiple Bash tool calls are sent in a single message, a non-zero exit code from ANY call cancels ALL other calls in the same batch (including background tasks). Rules:
- **Never batch a command that may fail** (lint, test) with background tasks (bramble).
- Launch background tasks in one message, then run quality gates in a subsequent message.
- When running lint in parallel with tests, append `|| true` to lint and check results yourself.

## Anti-pattern: shell waiters

**Do not** use shell `sleep` or `until` loops to wait for background work.

**Use**:
- `Monitor` tool call for "tell me when X happens" (one notification per event).
- `Bash` with `run_in_background: true` for "tell me when this command exits."
- Plain turn boundaries: do other work, check next turn. The files will be there.

## Step 0: Identify context

```
python3 $SKILL_DIR/scripts/pr_ops.py identify
```

Returns `{pr_number, title, url, base, head, branch, owner, repo, owner_repo, state_dir, state_file}`. `pr_number` is `null` for branches that don't yet have a PR — the rest of the flow still works, just with PR-comment and CI-failure fetches skipped. Pin `$CTX` to either the PR number or `branch:<head>` for later state subcommand calls.

## Step 0.5: Resume check

```
python3 $SKILL_DIR/scripts/pr_ops.py state-load $CTX
```

Compare against `git rev-parse HEAD`:

- **No state file / empty load**: fresh run. Proceed.
- **`pr_number` mismatches current PR**: stale state (integrity gate). Show user and `AskUserQuestion` whether to discard. This is one of only three sanctioned pauses — see "Auto-decision rules".
- **HEAD matches `last_commit_at_round_start`**: prior round was interrupted (compaction/manual stop). Resume the in-progress round.
- **HEAD differs from `last_commit_at_round_start`**: prior round committed or user made manual changes. Auto-start a new round on current HEAD (round N+1). Announce the decision in one line and proceed — re-invocation is the user's signal that they want another round.
- **`current_round` ≥ `--rounds`**: hard-stop (budget gate). User must explicitly authorize further rounds via `AskUserQuestion`.

**Compaction awareness**: with a state file present, trust it and apply the rules above — the state file survives compaction. With no state file present, start a fresh run. Do not ask.

## Step 1: Sync base via /git:sync-base

```
python3 ~/.claude/skills/git:sync-base/git-sync.py --verbose
```

That script owns rebasing onto `origin/<base>` and (when a PR exists) force-pushing the rebased branch back with precise-lease. Do not reimplement any of it here. On conflict (exit 2) **abort this polish run with `state-mark-complete <ctx> sync-conflict`** and emit the Final Summary pointing at the conflict — do not pause mid-run with `AskUserQuestion`. The user resolves the conflict and re-invokes to pick up.

Build a short `$PR_SUMMARY` from `git log --oneline origin/<base>..HEAD` + diff-stat (≤10 lines) to pass to bramble `--goal`.

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

### b) Launch bramble

Create a fresh `$LOG_DIR=$STATE_DIR/r$ROUND/`. Arm Monitors in the same turn — always codex + cursor, plus gemini when `--gemini` was passed:

```
ENVELOPE_CODEX="$LOG_DIR/codex-envelope.json"
ENVELOPE_CURSOR="$LOG_DIR/cursor-envelope.json"

Monitor({
  description: "bramble codex r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "WORK_DIR=$(pwd) bramble code-review \
    --backend codex --model gpt-5.4-mini \
    --goal \"{PR_SUMMARY}\" --skip-test-execution \
    --verbose --timeout 10m --envelope-file \"$ENVELOPE_CODEX\" \
    2>\"$LOG_DIR/codex-stderr.txt\""
})

Monitor({
  description: "bramble cursor r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "WORK_DIR=$(pwd) bramble code-review \
    --backend cursor --model composer-2 \
    --goal \"{PR_SUMMARY}\" --skip-test-execution \
    --verbose --timeout 10m --envelope-file \"$ENVELOPE_CURSOR\" \
    2>\"$LOG_DIR/cursor-stderr.txt\""
})

// Only when --gemini flag was passed:
ENVELOPE_GEMINI="$LOG_DIR/gemini-envelope.json"
Monitor({
  description: "bramble gemini r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "WORK_DIR=$(pwd) bramble code-review \
    --backend gemini --model gemini-3-flash-preview \
    --goal \"{PR_SUMMARY}\" --skip-test-execution \
    --verbose --timeout 10m --envelope-file \"$ENVELOPE_GEMINI\" \
    2>\"$LOG_DIR/gemini-stderr.txt\""
})
```

Each Monitor runs independently; a crash in one does not affect the other. `timeout_ms=720000` is bramble's own 10-minute `--timeout` plus two minutes of slack. `--skip-test-execution` tells the reviewer not to run tests — quality gates will.

### c) Triage

```
python3 $SKILL_DIR/scripts/bramble_ops.py triage {ROUND} $STATE_FILE \
    --stream codex=$ENVELOPE_CODEX \
    --stream cursor=$ENVELOPE_CURSOR \
    $( [ "$USE_GEMINI" = "1" ] && echo --stream gemini=$ENVELOPE_GEMINI ) \
    $( [ "$ROUND" = "1" ] && [ "$PR_NUMBER" != "null" ] && \
       echo --pr-comments $STATE_DIR/pp-comments.json --ci-failures $STATE_DIR/pp-ci.json )
```

`triage` reads envelopes, merges the round-1 PR-comment and CI-failure feeds, dedupes by `(path, line, topic)` / `(job_id, test_name)`, and emits:

- `consensus` — same key flagged by ≥2 sources. Route to `must_fix`.
- `single_critical` — single-source high/critical, or CI failure that isn't a flake. Route to `must_fix`.
- `single_medium` — single-source medium, or GitHub comment without severity-keyword. Route to `consider_fix`.
- `low_acks` — single-source low/nit, or flake CI failure. Route to `batch_ack`.
- `spiral_matches` — new findings whose key matches a prior-round `fixed` action. Route to `escalate`.

If `spiral_matches` is non-empty, **don't auto-fix** — call `AskUserQuestion` with the spiralling findings. The prior fix may have regressed or the reviewer is re-flagging something we thought we resolved.

If the empty action-plan case fires (nothing in `must_fix`/`consider_fix`, and `batch_ack` is all we have), exit the loop without touching files further.

### d) Apply fixes

Triage Rules:
1. **Consensus findings**: **must fix**, no exceptions.
2. **Single-reviewer critical/high**: **must fix** unless demonstrably false positive.
3. **Single-reviewer medium**: fix if it identifies a real gap. Skip only if incorrect or in unrelated code.
4. **Low findings**: fix if trivial (<5 min). Skip with one-line justification otherwise.
5. **CI failures**: fix unless it's completely out the scope of this PR.
6. **Log every triaged finding** to `comment_actions`. For bramble findings: `source` = `"codex"` or `"cursor"`; `comment_id: null`. For CI: `source: "ci"`, `path: job_id`, `topic: test_name`. For PR comments: `source: "github-inline"` etc., `comment_id` from the fetch.

**Stale Finding Guard**: before fixing any finding, verify the cited code still matches the current file. If you made changes between launching bramble and reading results the finding may reference code that no longer exists. Read the cited line first. When the guard fires record the finding with `action: "stale"` — silently dropping blinds the N+1 spiral guard.

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

For PR comments that require a reply, the orchestrator (or fixer) posts via `pr_ops.py reply-inline <id> <body>` after the fix is applied. Batch-reply nits — don't fan out one reply per trivial finding.

### e) Quality gates + commit (only if fixes applied)

Skip this whole step if step d produced zero file changes. No point running lint/tests again if nothing moved.

Follow Project quality gates.(separate turn from any Monitor arm)

On pass, commit locally. **Do NOT push.**

```
git add <files>
git commit -m "pr-polish round {ROUND}: <summary>

Findings fixed:
- <source>: <desc>

Findings skipped:
- <source>: <reason>"
```

### f) Finalize round state

Write accumulated `comment_actions` to a temp JSON file, then:

```
python3 $SKILL_DIR/scripts/pr_ops.py state-finalize-round $CTX $ROUND $(git rev-parse HEAD) $STATE_DIR/actions-r$ROUND.json
```

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
# Matches git-sync.py's precise-lease pattern (git-sync.py:401-421).
git push --force-with-lease --force-if-includes origin HEAD
```

Branch-only mode: just `git push -u origin <branch>` on first push (no prior remote to protect with `--force-with-lease`).

## Step 5: Final summary + mark complete

```
python3 $SKILL_DIR/scripts/pr_ops.py state-mark-complete $CTX <reason>
```

**Reason values**: `converged`, `all-low`, `false-positive-top`, `trend-down`, `capped-at-max`, `user-paused`, `spiral-escalated`, `sync-conflict`.

```
## PR Polish Summary

| Metric              | Value          |
|---------------------|----------------|
| Rounds completed    | N              |
| Comments addressed  | N              |
| Commits pushed      | N              |
| Convergence signal  | converged / all-low / false-positive-top / trend-down / capped-at-max / user-paused / sync-conflict |
| Verdict             | Ready / Not ready |

### Round-by-Round
| Round | Changes | Codex Findings | Cursor Findings | Comments Fixed | Summary |
|-------|---------|----------------|-----------------|----------------|---------|
| 1     | yes/no  | N (N fixed)    | N (N fixed)     | N              | ...     |

### Comment Actions
| Round | Source | Path:Line | Severity | Action | Notes |
|-------|--------|-----------|----------|--------|-------|
| 1     | github-inline (coderabbitai) | provision.py:142 | high | fixed | commit abc123f |
| 1     | codex | auth.py:88 | medium | false_positive | validated at auth.py:72 |
| 2     | cursor | api.ts:44 | low | ack | batch-replied |
```

Populate the Comment Actions table by concatenating `comment_actions` across rounds. Sort by round, then severity desc. Use `-` for null path/line.

Before ending, tell the user the state file path so they can read the raw per-finding decisions; mention it is preserved, not deleted. If ready, tell them the PR is good to merge. If not, list remaining issues clearly.
