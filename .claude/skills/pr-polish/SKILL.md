---
name: pr-polish
description: Fully autonomous PR polish loop. Runs N rounds of local bramble review (codex + cursor, optionally + gemini), folds in any existing PR comments and CI failures as round-1 input, fixes findings locally, pushes once at the end.
argument-hint: "[--rounds N] [--gemini] [--ask]"
disable-model-invocation: true
---

# PR Polish Loop

Review → triage → fix → commit locally each round. Exit when converged (Step 3.g) or round cap hit, then force-push once. No mid-loop pushes.

Helpers: `python3 $SKILL_DIR/scripts/<helper>.py`. `$SKILL_DIR` = directory containing this `SKILL.md`.

| Script | Role |
|---|---|
| `pr_ops.py` | Identity, comments, CI, state I/O, `round-bundle`, `remote-head` |
| `bramble_ops.py` | Goal text, resume ids, triage, envelope recovery |
| `lint_gate.py` | Diff lint (ruff/golangci/eslint) |
| `scope_gate.py` | `scope-hints.json` for bramble |

Missing/error review streams → log as findings with stderr path cited.

## Arguments

| Flag | Default | Meaning |
|---|---|---|
| `--rounds N` | `5` | Up to N additional rounds this invocation. Budget resets on re-invoke. `--rounds 0` = no-op. |
| `--gemini` | off | Third reviewer (`gemini-3-flash-preview`). ≥2 sources = consensus. |
| `--ask` / `--interactive` | off | Enable `AskUserQuestion` at gates (Step 3.g). Default: never block. |

## State tracking

`~/.bramble/projects/<repo>-<pr>/pr-polish-state.json` (or `…-branch-<slug>/…`). Never deleted.

`state-*` subcommands take `ctx` = PR number or `branch:<name>`.

| Command | When |
|---|---|
| `state-load` | Read |
| `state-append-round <ctx> <n> <head_before>` | Round start (`--no-verify-head` only when resuming interrupted round) |
| `state-finalize-round <ctx> <n> <head_after> <actions.json> [--envelope …]` | Round end |
| `state-mark-complete <ctx> <reason>` | Exit |

Key fields: `rounds[n].comment_actions` (audit trail), `low_only_streak` (convergence), `session_ids` (resume). Actions: `fixed`, `false_positive`, `wont_fix`, `ack`, `stale`, `pre_existing`/`flake` (CI only). Optional: `spiral_refix`, `invariant` (v2). See schema in repo if shape unclear.

## Step 0: Bootstrap

```bash
PREFLIGHT=$(python3 $SKILL_DIR/scripts/pr_ops.py preflight)
export BRAMBLE_BIN=$(echo "$PREFLIGHT" | jq -r .bramble_bin)
export SKILL_DIR=$(echo "$PREFLIGHT" | jq -r .skill_dir)
GIT_SYNC=$(echo "$PREFLIGHT" | jq -r .git_sync_path)
if [ "$(echo "$PREFLIGHT" | jq -r '.errors | length')" != "0" ]; then
  echo "$PREFLIGHT" | jq -r '.errors[]' >&2; exit 1
fi
python3 $SKILL_DIR/scripts/pr_ops.py identify
```

Pin: `$CTX`, `$STATE_DIR`, `$STATE_FILE`, `$BRANCH`, `$PR_NUMBER`, `$REPO`. 

`pr_number: null` → skip PR-comment/CI fetch.

## Step 0.5: Resume check

```bash
python3 $SKILL_DIR/scripts/pr_ops.py state-load $CTX
IS_NEW_SERIES=$(python3 $SKILL_DIR/scripts/pr_ops.py state-is-new-series $CTX $ROUND)
```

`IS_NEW_SERIES=1` before `state-append-round`: re-fetch comments/CI, fresh bramble sessions.

| Condition | Action |
|---|---|
| No state | Fresh run |
| `pr_number` mismatch | Step 3.g integrity gate → default `pr-mismatch-abort` |
| Heartbeat stale (>2h) + not completed | `state-mark-abandoned $CTX` |
| HEAD == `last_commit_at_round_start` | Resume interrupted round |
| HEAD differs (fresh heartbeat) | Next round on current HEAD |

`additional_rounds_run = 0` at start; increment each finalized round.

## Step 1: Sync base

Use `$GIT_SYNC` from preflight (not a hardcoded path):

```bash
python3 "$GIT_SYNC" --verbose --no-push
```

`--no-push` required — push only at Step 4.

Dirty tree (no in-progress round to resume) → `state-mark-complete <ctx> dirty-tree-preflight`, exit.
Conflict (exit 2) → `state-mark-complete <ctx> sync-conflict`, Final Summary, exit.

Build `$PR_SUMMARY` (≤10 lines): `git log --oneline origin/<base>..HEAD` + diff-stat. Round 1 `--goal` = `$PR_SUMMARY`; later rounds use `round-bundle` / `bramble_ops.py goal` (prior fixed/skipped + files changed + inter-round diff).

## Step 2: Fetch PR comments + CI

When `pr_number` not null (also re-fetch when `IS_NEW_SERIES=1` in round loop):

```bash
python3 $SKILL_DIR/scripts/pr_ops.py fetch-comments > $STATE_DIR/pp-comments.json
python3 $SKILL_DIR/scripts/pr_ops.py ci-failed-tests > $STATE_DIR/pp-ci.json
```

Triage reads these only when `IS_NEW_SERIES=1`. Still run bramble every round.

## Step 3: Round loop

```
additional_rounds_run = 0
while additional_rounds_run < --rounds:
  a) WIP commit if dirty
  b) scope_gate → round-bundle → Monitors (codex+cursor+lint[+gemini]) → barrier
  c) triage → action plan
  d) apply fixes
  e) quality gates + local commit if changed (NO push)
  f) finalize round state
  g) convergence check
  additional_rounds_run += 1
```

Header: `## Round N (M / --rounds)` — N absolute, M = `additional_rounds_run + 1`.

**Orchestrator vars** (`$LOG_DIR`, `$CTX`, etc.): substitute concrete values into each Bash call — fresh shell every time, no persistent `$VAR`.

### a) WIP commit

If dirty: `git add -A && git commit -m "pr-polish: round N snapshot"`. Bramble snapshots working tree at launch.

### b) Launch reviewers

Always use `round-bundle` for `$LOG_DIR`, `$GOAL`, resume ids — do not hand-roll attempt index.

```bash
BUNDLE=$(python3 $SKILL_DIR/scripts/pr_ops.py round-bundle "$CTX" {ROUND})
LOG_DIR=$(echo "$BUNDLE" | jq -r .log_dir)
GOAL=$(echo "$BUNDLE" | jq -r .goal_text)
CODEX_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.codex')
CURSOR_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.cursor')
[ "{ROUND}" = "1" ] && GOAL="$PR_SUMMARY"
mkdir -p "$LOG_DIR"

SCOPE_HINTS=$(python3 $SKILL_DIR/scripts/scope_gate.py --state-dir "$STATE_DIR" 2>"$LOG_DIR/scope-gate-stderr.txt")

[ "$IS_NEW_SERIES" = "1" ] && [ "$PR_NUMBER" != "null" ] && {
  python3 $SKILL_DIR/scripts/pr_ops.py fetch-comments > $STATE_DIR/pp-comments.json
  python3 $SKILL_DIR/scripts/pr_ops.py ci-failed-tests > $STATE_DIR/pp-ci.json
}
```

Arm Monitors (same turn), then **one** background Bash barrier until all `$LOG_DIR/<backend>-envelope.json` are non-empty or 780s elapsed. Steps b→c in **one turn** — no tool calls between arm and barrier.

```
Monitor({ description: "bramble codex r{ROUND}", timeout_ms: 720000, persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:codex:r{ROUND} \
    $BRAMBLE_BIN code-review --backend codex --model gpt-5.4-mini \
    --skip-test-execution --verbose --timeout 10m \
    --goal \"$GOAL\" --scope-hints-file \"$SCOPE_HINTS\" \
    ${CODEX_RESUME:+--resume-session-id \"$CODEX_RESUME\"} \
    --envelope-file \"$LOG_DIR/codex-envelope.json\" 2>\"$LOG_DIR/codex-stderr.txt\"" })

Monitor({ description: "bramble cursor r{ROUND}", timeout_ms: 720000, persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:cursor:r{ROUND} \
    $BRAMBLE_BIN code-review --backend cursor --model composer-2.5 \
    --skip-test-execution --verbose --timeout 10m \
    --goal \"$GOAL\" --scope-hints-file \"$SCOPE_HINTS\" \
    ${CURSOR_RESUME:+--resume-session-id \"$CURSOR_RESUME\"} \
    --envelope-file \"$LOG_DIR/cursor-envelope.json\" 2>\"$LOG_DIR/cursor-stderr.txt\"" })

Monitor({ description: "lint gate r{ROUND}", timeout_ms: 120000, persistent: false,
  command: "python3 $SKILL_DIR/scripts/lint_gate.py \
    --state-dir \"$STATE_DIR\" --round {ROUND} --log-dir \"$LOG_DIR\" \
    2>\"$LOG_DIR/lint-stderr.txt\"" })

// --gemini only:
GEMINI_RESUME=$(echo "$BUNDLE" | jq -r '.resume_ids.gemini')
Monitor({ description: "bramble gemini r{ROUND}", timeout_ms: 720000, persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=pr-polish:$REPO:$PR_NUMBER:gemini:r{ROUND} \
    $BRAMBLE_BIN code-review --backend gemini --model gemini-3-flash-preview \
    --skip-test-execution --verbose --timeout 10m \
    --goal \"$GOAL\" --scope-hints-file \"$SCOPE_HINTS\" \
    ${GEMINI_RESUME:+--resume-session-id \"$GEMINI_RESUME\"} \
    --envelope-file \"$LOG_DIR/gemini-envelope.json\" 2>\"$LOG_DIR/gemini-stderr.txt\"" })
```

```bash
# 3 envelopes (no --gemini):
end=$((SECONDS+780)); until [ -s "$LOG_DIR/codex-envelope.json" ] \
  && [ -s "$LOG_DIR/cursor-envelope.json" ] \
  && [ -s "$LOG_DIR/lint-envelope.json" ] \
  || [ $SECONDS -ge $end ]; do sleep 2; done

# 4 envelopes (--gemini): add && [ -s "$LOG_DIR/gemini-envelope.json" ] before || 
```

Before triage: `recover-envelope` on each stream path (idempotent). Missing envelopes → `stream-missing` findings, not deadlock.

### c) Triage

```bash
python3 $SKILL_DIR/scripts/bramble_ops.py triage $STATE_FILE \
  --stream codex=$LOG_DIR/codex-envelope.json \
  --stream cursor=$LOG_DIR/cursor-envelope.json \
  --stream lint=$LOG_DIR/lint-envelope.json \
  $( [ "$USE_GEMINI" = "1" ] && echo --stream gemini=$LOG_DIR/gemini-envelope.json ) \
  $( [ "$IS_NEW_SERIES" = "1" ] && [ "$PR_NUMBER" != "null" ] && \
     echo --pr-comments $STATE_DIR/pp-comments.json --ci-failures $STATE_DIR/pp-ci.json )
```

Buckets → `must_fix` / `consider_fix` / `batch_ack` / `escalate` (`spiral_matches`).

**Ownership:** own pre-existing code in touched files. `must_fix` unless false positive (cite file:line). Low/nit → fix if trivial else `ack`. Skips: `false_positive`, `wont_fix`, `stale`.

**Invariants:** same `invariant` from ≥2 reviewers → consensus on all sites. Prefer producer-side fix.

**Spirals:** single-source may auto-demote to stale if evidence gone (±10 lines) or cited line was in prior round's diff. Multi-source → escalate. Default (no `--ask`): re-fix once (`spiral_refix: true`), stop on 2nd recurrence.

Empty plan (`must_fix`/`consider_fix` empty) → converged, Step 3.g.

### d) Apply fixes

Fix the invariant, not the citation: name rule → scan sibling sites → fix at shallowest layer (line, helper, producer). Group cross-backend findings by underlying problem. Update docs/tests in same commit when contract changes. Log extra sites as `source: "sweep"`. Record every finding in `comment_actions`; don't silently drop stale items. GitHub inline replies happen in `state-finalize-round`.

### e) Quality gates + commit

Skip if no file changes. Run project gates, then commit locally (`pr-polish round {ROUND}: …`). **No push.** Check sibling sites/tests/docs before commit; record intentional gaps as `ack`.

### f) Finalize

```bash
python3 $SKILL_DIR/scripts/pr_ops.py finalize-and-report $CTX $ROUND $(git rev-parse HEAD) \
  $STATE_DIR/actions-r$ROUND.json \
  --envelope codex=$LOG_DIR/codex-envelope.json \
  --envelope cursor=$LOG_DIR/cursor-envelope.json \
  --envelope lint=$LOG_DIR/lint-envelope.json \
  $( [ "$USE_GEMINI" = "1" ] && echo --envelope gemini=$LOG_DIR/gemini-envelope.json )
```

(`state-finalize-round` works too; `finalize-and-report` adds round summary hints.)

### g) Convergence

Stop when any:
- Zero findings
- Empty triage plan
- `low_only_streak >= 2` (every low fixed or `ack`/`wont_fix` with reason)
- Top finding documented false positive + prior round had no `must_fix`

Budget exhausted → Final Summary; `--ask` to continue, else `capped-at-max`.

| Gate | `--ask` | Default |
|---|---|---|
| PR mismatch | Ask | Abort `pr-mismatch-abort` |
| Rounds exhausted | Ask | Stop `capped-at-max` |
| Spiral | Ask | Re-fix once; 2nd or multi-source → `spiral-escalated` |

## Step 4: Push

```bash
SYNC=$(python3 $SKILL_DIR/scripts/pr_ops.py remote-head "$BRANCH")
if [ "$(echo "$SYNC" | jq -r .in_sync)" != "true" ]; then
  git push --force-with-lease --force-if-includes origin HEAD   # or -u on first push
fi
```

Use `remote-head` not `git rev-parse origin/<branch>` (worktree lag).

## Step 5: Summary

```bash
python3 $SKILL_DIR/scripts/pr_ops.py state-mark-complete $CTX <reason>
```

Reasons: `converged`, `all-low`, `false-positive-top`, `capped-at-max`, `spiral-escalated`, `pr-mismatch-abort`, `sync-conflict`, `dirty-tree-preflight`, `user-paused`, `abandoned`.

Print: metrics, round table, full `comment_actions` table (`Round | Source | Path:Line | Severity | Action | Notes`), state file path, ready/not-ready verdict.
