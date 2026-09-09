# swarm-queen

A deterministic harness for `/subagent-swarm` runs.

The skill works — real runs merged 20 PRs in one wave — but the orchestrator is a
chat session holding the loop in its own context. It re-derives state by shell
every tick, and its judgment degrades as that context fills. Mining five real
runs (2026-09-02 → 09-08) measured the cost:

| Symptom | Measured |
|---|---|
| Bash calls in one 29h run | 1,843 (98% of all tool calls) |
| Pane screen-scraping | 2,540 calls — 38% of shell activity |
| Redundant `gh` polling | 2,223 calls — 36%, re-asking 3-4 CI runs |
| Compactions | 7 in 29h; 18 in 17.7h under agy |
| Live sessions absent from the ledger | 3 of 3 |
| `brief` field populated, run over run | 13/13 → 9/9 → 1/10 → 0/12 |
| Lanes non-terminal at "shutdown" | 6 of 21, incl. 3 p1 never staffed |
| Orphaned agent processes | 62, found only when a human asked |

Two facts explain most of it: **nothing in the loop is enforced by a mechanism**,
and **state is prose**, so it must be re-derived by shell after every compaction.

## The loop

```
swarm-queen tick   (fresh process, state read from disk)
  1. RECONCILE  git + gh + tmux + bramble  ->  observed truth   [no LLM]
  2. VERIFY     every .done/idle/review claim against it        [no LLM]
  3. DECIDE     rules first; unsettled questions escalate       [rules]
  4. APPLY      spawn / advance / rework / reap                 [no LLM]
```

Dry run by default. Steps 1, 2 and 4 never call a model. An LLM may propose
decisions, but `decide.Vet` re-checks each one against the same invariants the
deterministic path uses, so a fluent-but-wrong suggestion is refused rather than
executed.

## Commands

    swarm-queen doctor <run>              # drift between ledger and reality (read-only)
    swarm-queen tick <run> [--apply]      # one full loop
    swarm-queen reap <run> [--apply]      # transactional teardown
    swarm-queen nudge <run> <text> [--standing]
    swarm-queen escalations <run>
    swarm-queen answer <run> <id> <text>

## What it refuses to do

Each refusal below corresponds to a failure that cost real time in a live run.

- **Advance a `.done` on an empty branch.** A completion claim with no commits is
  not a completion.
- **Merge on a stale approval.** `reviewDecision: APPROVED` is a PR-level field;
  it must be pinned to the current head. Caught stale three times in one run.
- **Treat absence as success.** Unknown checks, unknown approval, missing verdict
  — all refusals, never permission.
- **Reap a lane holding unsnapshotted work.** Untracked files are protected by no
  branch; a worktree removal destroys them outright.
- **Reap on ancestry alone.** Branches are squash-merged, so integration is
  verified by content (a two-dot diff), not by ancestry.
- **Kill a tmux window it cannot prove is not itself.** See the 2026-08-27
  incident in `lifecycle/tmuxsafe.go`: 18 healthy sessions killed, including the
  orchestrator's own.
- **Remove a worktree with a live session on it.** Sessions are resolved from
  bramble, never from the ledger's `window_id`, which decayed to 1-of-12
  populated.
- **Overwrite a rework round.** `sessions` is keyed by phase, so a naive rework
  destroys the previous attempt's session id — rounds 2-6 of an 11-round lane
  were lost this way.

## Shared state

`state.json` is shared with the skill's `ledger.py`. Both take the same
advisory `flock` on `<run>/state.json.lock` — `LOCK_EX` across the whole
read-modify-write, `LOCK_SH` for reads so the watcher's polling never blocks a
writer — and both write via temp file plus atomic rename. Without it, 7 of 8
concurrent updates were silently lost. Cross-language interop is asserted in
`state/integration/`.

Unknown JSON keys round-trip in both directions, so neither tool deletes the
other's fields.
