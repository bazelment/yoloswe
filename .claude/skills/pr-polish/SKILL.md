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

### Command and tool adapters

Examples below use `python3`, `Monitor`, and `Bash` because those are the common Claude-runner primitives. They are not permission to bypass the target repo's tool or command policy.

- If the repo bans bare Python, rewrite helper invocations to that repo's approved Python launcher. Apply the same rewrite to `git-sync.py`, `scope_gate.py`, `lint_gate.py`, and `bramble_ops.py`.
- If `Monitor` or background `Bash` is unavailable, use the Codex fallback: start each long-running review arm with `exec_command(..., yield_time_ms: 1000)`, keep the returned session id, and poll completion with `write_stdin(session_id, chars: "", yield_time_ms: ...)`.
- When using `exec_command`, wrap each bramble/lint arm with `set +e; ...; CODE=$?; printf '__EXIT_CODE=%s\n' "$CODE"; exit 0`. This keeps one backend failure from cancelling other parallel tool calls while preserving the real exit status for triage.
- Keep the same envelope and stderr paths (`$LOG_DIR/*-envelope.json`, `$LOG_DIR/*-stderr.txt`) in every adapter. Missing envelopes, non-zero `__EXIT_CODE`, or `status: "error"` envelopes are review-stream failures; include the stderr path in triage/round summary, and only use `wont_fix` when a `comment_actions` row is required. Never silently drop them.

**Base-branch syncing is not this skill's job.** Invoke `.claude/skills/git:sync-base/git-sync.py --verbose` directly — that skill owns branch rebasing, precise-lease force-push, and conflict handling.

## Arguments

| Flag | Default | Meaning |
|---|---|---|
| `--rounds N` | `5` | The round number this invocation will *stop at* (inclusive). Resuming a state at `current_round=5` with `--rounds 7` runs rounds 6 and 7 — i.e. "two more rounds". `--rounds 5` on the same state is a no-op (already at the cap). |
| `--fixer-model MODEL` | `sonnet` | Model passed to `Agent(model=…)` spawns when the round's action plan is too large to apply inline. |
| `--gemini` | off | Also run a third bramble reviewer using `--backend gemini --model gemini-3-flash-preview`. Findings from all three backends are merged and deduplicated; a finding agreed on by ≥2 sources (including Gemini) counts as consensus. |
| `--review-recent-commits` | off | Add a 4th bramble Monitor per round that runs codex with `--goal "Focus on changes in commits <head_before>...<head_after>"`. The session's evidence is that codex catches "the bug you just shipped" with high reliability when biased toward new commits. Pairs with the existing full-diff codex/cursor streams; doesn't replace them. Skip on round 1 where there are no "prior round commits" to focus on. |

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
- In Codex, `exec_command` sessions are safe to run in parallel only when each command is self-contained and wrapped to print `__EXIT_CODE` then exit 0. Poll the sessions and inspect the marker yourself.

## Anti-pattern: shell waiters

**Do not** use shell `sleep` or `until` loops to wait for background work.

**Use**:
- `Monitor` tool call for "tell me when X happens" (one notification per event).
- `Bash` with `run_in_background: true` for "tell me when this command exits."
- Codex `exec_command` fallback: start the command with a short `yield_time_ms`, then poll with `write_stdin`; keep all output envelopes on disk.
- Plain turn boundaries: do other work, check next turn. The files will be there.

## Step 0: Identify context

```
python3 $SKILL_DIR/scripts/pr_ops.py identify
```

Returns `{pr_number, title, url, base, head, branch, owner, repo, owner_repo, state_dir, state_file}`. `pr_number` is `null` for branches that don't yet have a PR — the rest of the flow still works, just with PR-comment and CI-failure fetches skipped. Pin `$CTX` to either the PR number or `branch:<head>` for later state subcommand calls.

### Step 0.1: Pick the bramble binary

Detect once at the top of the run and export `BRAMBLE_BIN` so every Monitor arm and `bramble_ops` invocation uses the same binary. Prefer the freshly-built `bazel-bin` artifact when it exists in the current worktree (matches the code under review); otherwise fall back to whatever `bramble` is on `PATH`.

```
export BRAMBLE_BIN="$([ -x "$(pwd)/bazel-bin/bramble/bramble_/bramble" ] \
    && echo "$(pwd)/bazel-bin/bramble/bramble_/bramble" \
    || echo bramble)"
```

All later steps (Monitor commands in step 3b, anything that invokes `bramble code-review`) MUST reference `$BRAMBLE_BIN` rather than the bare `bramble` literal.

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

Build a short `$PR_SUMMARY` from `git log --oneline origin/<base>..HEAD` + diff-stat (≤10 lines) to pass to bramble `--goal` on round 1.

> **Goal channel semantics (2026-05-06):** on round 1, `--goal` carries
> the PR-level intent (PR_SUMMARY). On round 2+, `format_monitor_command`
> automatically replaces the caller-supplied goal with an action-history
> string built from the state file (e.g. `"Round 3. Prior rounds fixed:
> a.go:10 (codex); b.py:42 (cursor). Skipped: c.go:8 (cursor) (wont_fix);
> d.go:5 (codex) (stale)."`). Bramble's follow-up prompt builder embeds
> this as `Context for this turn: <history>` so the resumed model knows
> which of its own prior findings the orchestrator already actioned and
> doesn't burn rounds re-flagging them. Pass the same `$PR_SUMMARY` to
> `format_monitor_command` every round — the helper handles the round-1
> vs round-2+ distinction internally. If round 1 produced zero actions,
> the helper falls back to `$PR_SUMMARY` so we still pass *something*
> on round 2 rather than empty.

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

Run it once per round, **before** arming the bramble Monitors. The scope set genuinely grows across rounds (kernel-2755 evidence: a fix introduced new test files in r2 and r4, a refactor extracted a helper module in r3); recomputing per round catches that. The script overwrites `$STATE_DIR/scope-hints.json` each round so storage stays flat — bramble's run log already provides the per-round audit trail via `BRAMBLE_RUN_TAG`. Cost is ~100ms vs 60–400s per backend turn, so it's noise.

```bash
# Foreground; ~100ms. The script always exits 0 (emits an empty hints
# file on git failure or outside-a-repo); bramble's malformed-file
# fallback handles any oddity by reverting to the legacy narrow review.
SCOPE_HINTS=$(python3 $SKILL_DIR/scripts/scope_gate.py \
  --state-dir "$STATE_DIR" 2>"$LOG_DIR/scope-gate-stderr.txt")
```

Then arm Monitors in the same turn — always codex + cursor + lint, plus gemini when `--gemini` was passed. The bramble Monitors all pass `--scope-hints-file "$SCOPE_HINTS"`; the lint Monitor doesn't (lint_gate has its own diff walk).

The bramble launch string itself comes from `bramble_ops.py format-monitor-command`. That helper is the single source of truth for round-specific `--goal` semantics (PR_SUMMARY on round 1, action-history string on round 2+) and resume plumbing (it appends `--resume-session-id <id>` when `prior_session_id()` can find one). Treating its stdout as the launch string — rather than hardcoding flags and grepping out only `--resume-session-id` — is what keeps the continuous-review prompt behavior actually wired through.

If a backend envelope later reports `resume_status: "fallback"`, treat that backend as a best-effort cold-start reviewer for spiral-guard/convergence notes. If `Monitor` is unavailable, use the Codex `exec_command` fallback after the Monitor examples.

```
ENVELOPE_CODEX="$LOG_DIR/codex-envelope.json"
ENVELOPE_CURSOR="$LOG_DIR/cursor-envelope.json"
ENVELOPE_LINT="$LOG_DIR/lint-envelope.json"

# format-monitor-command emits the full `cd ... && bramble code-review ...`
# string with the round-correct --goal and (when applicable) --resume-session-id
# already substituted. Append --envelope-file and stderr redirection at the
# call site since those are per-arm bookkeeping, not part of the canonical
# launch string.
CODEX_CMD=$(python3 $SKILL_DIR/scripts/bramble_ops.py format-monitor-command \
  codex gpt-5.4-mini {ROUND} \
  --goal "{PR_SUMMARY}" --pr $PR_NUMBER --work-dir "$(pwd)" \
  --state-file "$STATE_FILE" --scope-hints-file "$SCOPE_HINTS")

Monitor({
  description: "bramble codex r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "$CODEX_CMD --envelope-file \"$ENVELOPE_CODEX\" 2>\"$LOG_DIR/codex-stderr.txt\""
})

CURSOR_CMD=$(python3 $SKILL_DIR/scripts/bramble_ops.py format-monitor-command \
  cursor composer-2 {ROUND} \
  --goal "{PR_SUMMARY}" --pr $PR_NUMBER --work-dir "$(pwd)" \
  --state-file "$STATE_FILE" --scope-hints-file "$SCOPE_HINTS")

Monitor({
  description: "bramble cursor r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "$CURSOR_CMD --envelope-file \"$ENVELOPE_CURSOR\" 2>\"$LOG_DIR/cursor-stderr.txt\""
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
ENVELOPE_GEMINI="$LOG_DIR/gemini-envelope.json"
GEMINI_CMD=$(python3 $SKILL_DIR/scripts/bramble_ops.py format-monitor-command \
  gemini gemini-3-flash-preview {ROUND} \
  --goal "{PR_SUMMARY}" --pr $PR_NUMBER --work-dir "$(pwd)" \
  --state-file "$STATE_FILE" --scope-hints-file "$SCOPE_HINTS")

Monitor({
  description: "bramble gemini r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "$GEMINI_CMD --envelope-file \"$ENVELOPE_GEMINI\" 2>\"$LOG_DIR/gemini-stderr.txt\""
})

// Only when --review-recent-commits was passed AND ROUND >= 2.
// Round 1 has no "prior round commits" to focus on.
// Use bramble_ops.recent_commits_goal to build the --goal text.
ENVELOPE_RECENT="$LOG_DIR/codex-recent-envelope.json"
RECENT_GOAL=$(python3 -c "import sys; sys.path.insert(0, '$SKILL_DIR/scripts'); import bramble_ops; print(bramble_ops.recent_commits_goal('$HEAD_BEFORE', '$HEAD_AFTER_WIP'))")
Monitor({
  description: "bramble codex recent r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "WORK_DIR=$(pwd) $BRAMBLE_BIN code-review \
    --backend codex --model gpt-5.4-mini --effort medium \
    --goal \"$RECENT_GOAL\" --skip-test-execution \
    --scope-hints-file \"$SCOPE_HINTS\" \
    --verbose --timeout 10m --envelope-file \"$ENVELOPE_RECENT\" \
    2>\"$LOG_DIR/codex-recent-stderr.txt\""
})
```

**Why a separate codex stream on recent commits.** The session that
shipped session-resume support empirically showed: codex catches "the
bug you just shipped" with high reliability when its review is biased
toward the new commits rather than the full diff. The full-diff
codex/cursor streams provide breadth; this fourth Monitor adds a
focused depth-pass on what's new. Findings from `--review-recent-commits`
are wired through `triage` as `source: "codex"` (same as the full-diff
codex stream), so they participate in normal consensus / single-source
bucketing — including the new (file, line) consensus dedup that
collapses different phrasings of the same finding.

Each Monitor runs independently; a crash in one does not affect the others. `timeout_ms=720000` is bramble's own 10-minute `--timeout` plus two minutes of slack; the lint Monitor uses a 2-minute timeout because deterministic linters complete in seconds. `--skip-test-execution` tells the reviewer not to run tests — quality gates will.

**Codex `exec_command` fallback.** Launch one session per backend, preferably in parallel if the environment supports parallel tool calls. Keep the envelope/stderr paths identical to the Monitor path so Step 3c does not change.

```bash
set +e
CODEX_CMD=$(python3 "$SKILL_DIR/scripts/bramble_ops.py" format-monitor-command \
  codex gpt-5.4-mini "$ROUND" \
  --goal "$PR_SUMMARY" --pr "$PR_NUMBER" --work-dir "$(pwd)" \
  --state-file "$STATE_FILE" --scope-hints-file "$SCOPE_HINTS")
eval "$CODEX_CMD --envelope-file \"$ENVELOPE_CODEX\" 2>\"$LOG_DIR/codex-stderr.txt\""
CODE=$?
printf '__EXIT_CODE=%s\n' "$CODE"
exit 0
```

Use the same wrapper for `cursor` and optional `gemini` by changing `--backend`, `--model`, envelope, and stderr paths. For the lint arm:

```bash
set +e
python3 "$SKILL_DIR/scripts/lint_gate.py" \
  --state-dir "$STATE_DIR" --round "$ROUND" \
  >"$LOG_DIR/lint-stdout.txt" 2>"$LOG_DIR/lint-stderr.txt"
CODE=$?
printf '__EXIT_CODE=%s\n' "$CODE"
exit 0
```

Replace `python3` with the repo-approved launcher when required. Poll each session with `write_stdin` until it prints `__EXIT_CODE=...`, then inspect both the marker and the envelope. A non-zero marker, missing envelope, malformed envelope, or `status: "error"` envelope must be included in triage/round summary with the stderr path cited; do not treat it as "no findings."

### c) Triage

```
# When --review-recent-commits was passed, pre-merge the focused codex
# stream's findings into the full-diff codex envelope so triage sees one
# combined codex source. triage stores --stream entries by backend and
# would otherwise clobber the first codex envelope with the second.
if [ -f "$ENVELOPE_RECENT" ]; then
  jq -s '.[0] as $a | .[1] as $b
        | $a + {review: ($a.review + {issues: (($a.review.issues // []) + ($b.review.issues // []))})}' \
     "$ENVELOPE_CODEX" "$ENVELOPE_RECENT" > "$LOG_DIR/codex-merged-envelope.json"
  CODEX_TRIAGE_ENVELOPE="$LOG_DIR/codex-merged-envelope.json"
else
  CODEX_TRIAGE_ENVELOPE="$ENVELOPE_CODEX"
fi

python3 $SKILL_DIR/scripts/bramble_ops.py triage {ROUND} $STATE_FILE \
    --stream codex=$CODEX_TRIAGE_ENVELOPE \
    --stream cursor=$ENVELOPE_CURSOR \
    --stream lint=$ENVELOPE_LINT \
    $( [ "$USE_GEMINI" = "1" ] && echo --stream gemini=$ENVELOPE_GEMINI ) \
    $( [ "$ROUND" = "1" ] && [ "$PR_NUMBER" != "null" ] && \
       echo --pr-comments $STATE_DIR/pp-comments.json --ci-failures $STATE_DIR/pp-ci.json )
```

`triage` reads envelopes, merges the round-1 PR-comment and CI-failure feeds, and emits:

- `consensus` — same `(file, line)` flagged by ≥2 distinct sources, or same `(file, line, topic)` for sourceless paths. Route to `must_fix`. The location-only key (added 2026-05-06) collapses different phrasings of the same finding so two reviewers wording it differently still consolidate.
- `single_critical` — single-source high/critical, or CI failure that isn't a flake. Route to `must_fix`.
- `single_medium` — single-source medium, or GitHub comment without severity-keyword. Route to `consider_fix`.
- `low_acks` — single-source low/nit, or flake CI failure. Route to `batch_ack`.
- `spiral_matches` — new findings whose `(file, line, topic)` matches a prior-round `fixed` action. Route to `escalate`. (Spiral keeps the topic component because exact recurrence matters here — a fix-then-recurrence has identical wording, not just identical location.)

If `spiral_matches` is non-empty, **don't auto-fix** — call `AskUserQuestion` with the spiralling findings. The prior fix may have regressed or the reviewer is re-flagging something we thought we resolved.

If the empty action-plan case fires (nothing in `must_fix`/`consider_fix`, and `batch_ack` is all we have), exit the loop without touching files further.

### d) Apply fixes

Triage Rules:
1. **Consensus findings**: **must fix**, no exceptions.
2. **Single-reviewer critical/high**: **must fix** unless demonstrably false positive.
3. **Single-reviewer medium**: fix if it identifies a real gap. Skip only if incorrect or in unrelated code.
4. **Low findings**: fix if trivial (<5 min). Skip with one-line justification otherwise.
5. **CI failures**: fix unless it's completely out the scope of this PR.
6. **Stale-on-prior-commit PR comments**: triage's `stale_prior_commit` bucket / `action_plan.batch_stale` holds inline comments whose `original_commit_id` no longer matches `pr["head_sha"]` (cursor[bot] / coderabbit comments superseded by amended commits). Auto-acknowledge each as `action: "stale"` with `reason: "Superseded by <short_sha>; comment was anchored to <short_old_sha>."` — no fix attempt, no further triage. Auto-reply too (see below).
7. **Log every triaged finding** to `comment_actions`. For bramble findings: `source` = `"codex"` or `"cursor"`; `comment_id: null`. For CI: `source: "ci"`, `path: job_id`, `topic: test_name`. For PR comments: `source: "github-inline"` etc., `comment_id` from the fetch.

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

**Auto-reply to PR comments.** For every inline `comment_actions` row whose `comment_id` is non-null AND `action` is one of `fixed`, `stale`, `false_positive`, `wont_fix`, post a reply via `pr_ops.py reply-inline <comment_id> "<body>"` so the bot/human author sees a closure signal in the thread. Templates (one short paragraph each — bots don't need prose, humans skim):

- `fixed`: `Fixed in <short_sha>.`
- `stale`: `Superseded by <short_sha> — the cited code was changed/removed in a later commit. (Auto-reply from /pr-polish.)`
- `false_positive`: `Marked false positive: <reason>. (Auto-reply from /pr-polish.)`
- `wont_fix`: `Won't fix: <reason>. (Auto-reply from /pr-polish.)`

Skip replies for `ack` (low/nit batch acks would spam the thread) and for non-inline rows (`comment_id` null — no thread to reply to). Batch-reply nits at the top level if you must — don't fan out one reply per trivial finding.

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
python3 $SKILL_DIR/scripts/pr_ops.py state-finalize-round $CTX $ROUND $(git rev-parse HEAD) \
    $STATE_DIR/actions-r$ROUND.json \
    --envelope codex=$ENVELOPE_CODEX \
    --envelope cursor=$ENVELOPE_CURSOR \
    $( [ "$USE_GEMINI" = "1" ] && echo --envelope gemini=$ENVELOPE_GEMINI )
```

The `--envelope` flags tell finalize where to read each backend's
envelope so `rounds[n].session_ids` and `rounds[n].resume_status` get
populated for next-round resume. Without these, finalize falls back to
`bramble_ops.envelope_path()`'s `/tmp` legacy convention and silently
misses operator-controlled paths under `$STATE_DIR/r$ROUND/`. Until
2026-05-06 this gap left pr-polish doing cold-start reviews on every
round despite session-resume being the headline feature; the explicit
`--envelope` plumbing closes it.

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

**Reason values**: `converged`, `all-low`, `false-positive-top`, `trend-down`, `capped-at-max`, `user-paused`, `spiral-escalated`, `sync-conflict`, `abandoned` (set by `state-mark-abandoned` from Step 0.5 when the prior heartbeat went stale; never passed to `state-mark-complete` directly).

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
