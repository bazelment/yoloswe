---
name: subagent-swarm
description: Orchestrate prompt-configured bramble lanes with durable goal tracking, priority gap dispatch, phase gates, and recurring status loops. Use when the user types `/subagent-swarm`.
disable-model-invocation: true
---

# subagent-swarm

Own the goal, work graph, dispatch, and integration. Delegate lane work; perform only
orchestrator-owned or prompt-reserved actions yourself.

## 1. Lanes and phases

Treat the invocation as the run contract. Capture its goal and terminal proof, canonical
context/progress files, phase overrides, concurrency, authority boundaries, human gates,
and recurring schedules. Do not replace explicit instructions with skill defaults.

When no lifecycle is supplied, use `swe -> clean -> review -> integrate`:

| Phase | Outcome |
|---|---|
| `swe` | Implement, prove, and commit the lane's scope. |
| `clean` | A fresh session simplifies the lane's own diff. |
| `review` | A fresh session applies the requested review gate. |
| `integrate` | Create/update a PR, merge, or hand off only as authorized. |

Models, effort, tools, skip rules, review thresholds, and approvals come from the prompt.
A major review finding returns the same lane to `swe` with the finding recorded. If the
task itself changed shape, create a new lane instead. Read
[references/pr-lane.md](references/pr-lane.md) when using this lifecycle.

A lane owns one task, branch/worktree, dependency list, priority, and current phase.
Priorities are:

- `p0`: blocks the terminal proof, integration, or several lanes;
- `p1`: removes major uncertainty or builds the proof path;
- `p2`: remaining ready work.

Keep phase separate from status (`planned/running/done/blocked/failed`). Recurring audits,
rebases, artifact refreshes, and measurements are schedules; materialize a lane when due
instead of occupying a slot indefinitely.

When the remaining path is readable in source but failures would surface one per run,
start with a read-only look-ahead lane. It ranks likely walls by firing order, cites both
sides of each claim, names the expected live symptom and confidence, and lists what it did
not examine. Its output prioritizes lanes; the real journey still proves the prediction.

Before spawning, read [references/bramble-mechanics.md](references/bramble-mechanics.md)
and run its preflight. Initialize the ledger with the prompt's phase names, then add lanes:

~~~bash
SW=~/.claude/skills/subagent-swarm/scripts
python3 "$SW/ledger.py" init "$RUN" --goal "<goal>" \
  --phases "swe:,clean:,review:,integrate:" --base "$BASE" --target "$TARGET"
python3 "$SW/ledger.py" add "$RUN" --id <id> --title "<task>" \
  --branch <branch> --priority p1 --depends-on "<lane ids>"
~~~

## 2. The orchestrator

`$RUN/OBJECTIVE.md` is the compact control page that survives compaction. Record:

- the goal, terminal proof, uncovered goal dimensions, and current proof boundary;
- pointers to user-named context/progress artifacts rather than copies of them;
- lifecycle overrides, authorities, approval gates, schedules, and next due times;
- literal recovery coordinates (`$RUN/env.sh` holds the derivable ones) and the command
  that recreates the loop after compaction;
- current integrated work, prioritized gaps, and the next action.

Before dispatch, check that the terminal proof covers every material part of the stated
goal. Keep uncovered dimensions as explicit proof gaps until evidence closes them or the
user narrows the goal; never treat the label "terminal proof" as broader than its probe.

On every tick, compare current evidence with the terminal proof. Turn each gap into a
briefable task and ledger lane, or record why it cannot yet be staffed. A finding or notes
file is not a queue; partial deliveries become follow-up lanes. Work that is merely large
is not blocked.

Dispatch dependency-ready lanes by priority and refill freed slots immediately. Keep
concurrent ownership independent: lanes must not edit the same files or mutate the same
live system unless the run contract explicitly serializes them. Brief only the mission,
non-derivable context, ownership boundaries, prompt-reserved actions, exit gate, and
literal report paths.

Lanes signal through files in the run directory, never through pane text:
`<lane>.<phase>.done` claims a phase is finished, `<lane>.<phase>.needs-swe` returns the
lane to `swe` with a finding, `<lane>.<phase>.md` carries the report, and
`<lane>.<phase>.spawn.json` records the session id and worktree at spawn. The watcher
wakes on the first two; write `.done` last, after committing.

Treat every idle, `.done`, blocked, test, review, and merge report as a claim. Verify the
artifact or live state before advancing, reworking, integrating, or reaping. The
orchestrator performs live-system operations reserved by the prompt and feeds measured
results back to lanes; lanes must not acquire that authority by implication.

External writes, pushes, PR creation, merges, and human notifications require prompt
authorization. Satisfy the prompt's approval gate before integrating. Never generalize a
narrow proof beyond the path it exercised.

## 3. Loop reminder

After the first dispatch, arm the watcher and schedule a health tick every 20 minutes
unless the prompt specifies another interval. **Never schedule a tick under 10 minutes.**
The watcher is the wake mechanism; polling for a lane yourself is not a faster tick, it
is a busy-wait — one run that degraded from 20-minute ticks to 60-second timers spent
320 shell commands an hour against 67 for the same work properly paced. A tick with
nothing to decide re-arms the watcher and ends.

Your own recurring reminder is a relay channel with no expiry. When you retract a value,
correct the reminder text too, or it re-injects the stale framing on every wakeup — and
record who else holds the old value, because a retraction is complete only when it
reaches them, not when it is written down. Track longer recurring jobs separately and
run them when due. The reminder must carry literal recovery state:

~~~text
/loop Orchestrating <goal>. SELF=<session id>. RUN=<absolute run dir>.
Source <absolute run dir>/env.sh for the socket and coordinates -- do not carry a
literal socket path, it changes on every bramble restart.
Read <absolute run dir>/OBJECTIVE.md and run the tick in
~/.claude/skills/subagent-swarm/SKILL.md. Terminal proof: <observable>.
Before ending a nonterminal tick, persist gaps and priorities, arm one watcher,
and schedule the next tick.
~~~

Each tick:

1. `. "$RUN/env.sh"`, then run `poll_panes.sh "$RUN"` **before reading any report or
   dispatching anything**. A lane blocked on a question emits nothing, moves no git
   state, and looks identical to one that is merely slow; five at once have gone
   unnoticed until a human asked. One paste in a composer: `bramble send-key
   --session-id <id> Enter`. A trust dialog: send `1`, then Enter. Many stacked
   pastes: stop sending, write `HANDOVER-<lane>.md`, and replace the session.
2. Re-read `OBJECTIVE.md`; run `ledger.py doctor "$RUN"` and resolve what it reports.
   Identify due schedules and the shortest path to proof.
3. Drain reports and `.done`/`.needs-swe` files; verify each completion claim against
   commits and artifacts.
4. Advance, rework, integrate, or mark lanes; snapshot uncommitted work before reaping.
5. Reassess proof gaps, materialize due work, reprioritize, and fill free slots.
6. Update the objective and ledger, then run `audit_cleanup.sh "$RUN"` — cleanup is
   done when it exits zero, not when you have gone through the steps. Arm one watcher
   and schedule the next tick.

Reap the old watcher and arm the new one in separate calls; see
[references/bramble-mechanics.md](references/bramble-mechanics.md). After compaction,
first confirm the loop still exists and recreate
it if needed. Stop only when the terminal proof holds, every material uncovered
goal dimension is evidenced closed or explicitly removed by the user, required integration
or approval gates are satisfied, all lanes are terminal, and cleanup is accounted for. If
lanes finish before then, staff the next gap.

Report integrated outcomes, verified evidence, blockers with unblock conditions,
follow-ups, and the ledger path.
