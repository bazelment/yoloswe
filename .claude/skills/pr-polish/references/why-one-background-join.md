# Why reviewers run under one background join

All reviewers launch inside a single `run_in_background` Bash job that `&`s each
one, collects PIDs, and `wait`s. That shape is load-bearing:

**One notification, not N.** The orchestrator has nothing useful to do between
"reviewer 1 finished" and "all reviewers finished" — triage needs every
envelope. Acting on a single reviewer's completion strands the round.

**Inspection between Monitor-arm and that notification is polling.** Do not
`tail` / `cat` / `ls` / `stat` / `date` / read any envelope, log, or task-output
file to see if the review is done. If the reason for a read-only tool call is
"check if the review is done," that is polling — stop. The completion
notification is the sole trigger. Stdout phase and heartbeat events are push;
there is no progress file to open. `sleep`, `ScheduleWakeup`, or ending the
turn with a "standing by" reply are the same failure.

**Yielding the turn can be fatal.** This skill may run non-interactively — e.g.
driven by jiradozer inside one bounded agent turn. There is no harness to
re-invoke the orchestrator on a wakeup or task notification, so a yielded turn
does not resume: the round is simply abandoned. The background join is the only
sanctioned wait because it blocks inside a single tool call.

**`wait` is the true all-done signal.** It returns when every child has *exited*,
including one that crashed without writing an envelope — so a dead reviewer
returns promptly instead of hanging to the timeout ceiling.

**But the join's exit status is not a failure signal.** `wait` with multiple PIDs
returns only the *last* PID's status, so a crashed reviewer is masked by a later
success. Failure detection happens after the join, in triage: a crashed or
timed-out reviewer leaves no (or an empty) envelope → `recover-envelope` → a
`stream-missing` finding.

**The `PIDS` array is a safety rail.** A hand-maintained `wait $A $B $C` line
desyncs from the launches the first time someone adds or gates a reviewer.
Appending `PIDS+=($!)` after every launch makes an omitted optional reviewer just
one fewer element instead of a broken wait.

**Timeouts are layered.** `--idle-timeout 5m` kills only a *stalled* backend, so a
review making steady progress runs as long as it needs; the outer `timeout 1200`
is an absolute backstop so a wedged process can't outlive the round. Lint gets
`timeout 120` — it is a fast static pass and must not hold the join for twenty
minutes.
