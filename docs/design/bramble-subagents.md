# Bramble subagents

## Problem

Claude Code's native subagent (the Task tool) is in-process and ephemeral:
isolated context, one final text report, no persistence, no mid-flight
visibility, one provider family, not resumable, gone when the parent turn ends.

Bramble already has the better substrate — worktrees, five backends selected by
model ID, tmux windows, JSONL persistence, idle notification — and the spawn
flow already existed end to end (`/bramble-spawn` → `bramble new-session`). What
was missing was the *return leg*:

1. **Delivery interrupted.** `send-input` types into a live pane. Mid-turn, the
   text lands in the recipient's next prompt, out of context. The `bramble-peers`
   skill could only warn about this in prose, and the "check for idle first"
   it recommends is a race.
2. **Reporting was opt-in and unbacked.** `<shared>/mail/` and `ledger.md` were
   pure convention with no code behind them. A child that ignored the Reporting
   section in its prompt simply never reported — and a Codex or Agy child,
   which cannot be given a system prompt or tools through this wrapper, usually
   would.
3. **No lineage.** `Session` had no parent, so nothing fired when a child
   finished and `list-sessions` could not show who spawned whom.

## Design

Three pieces. No new command group, no agent-definition files, no mailbox
service. The delegator (`bramble/session/delegator_tools.go`) is untouched.

### 1. Lineage

`Session`, `SessionInfo` and `StoredSession` carry `ParentSessionID`, set at
spawn through `SpawnOpts` and persisted, so a subagent's return address survives
a restart. `new-session --parent` defaults to `$BRAMBLE_SESSION_ID`;
`--no-parent` opts out. With no `--branch` and no `--worktree`, a subagent
inherits its parent's worktree — and must then be filed under its parent's
repo, so a `--repo` naming a different one is refused rather than registering a
session under a repo whose tree it is not in.

**A value the client inferred is not a claim the caller made.** `new-session`
fills in two fields on its own — the parent from `$BRAMBLE_SESSION_ID`, and the
repo from the cwd when `--repo` is omitted — and both carry that provenance to
the server (`ipc.NewSessionParams.ParentInherited`, `RepoInferred`), because
each is weighed differently from a typed one:

- An id passed to `--parent` must resolve and never silently becomes a
  top-level spawn. An inherited one is a default, and the registry only sees
  sessions adopted into an open manager — so an agent whose own repo is closed
  would otherwise lose the ability to spawn anything at all. That case warns and
  spawns top-level.
- A `--repo` the caller typed picks the manager, and `handleNewSession` refuses
  if that then contradicts an inherited worktree. An inferred one loses to a
  resolved parent's repo, which is exact where a cwd is merely wherever the
  agent's worktree happens to be.

The delegator deliberately does *not* set a parent: it runs its own child
watcher and would otherwise be told about every transition twice.

### 2. The notifier — `bramble/session/delivery.go`

The return leg is a **poll**, not a push. A lane records its own completion — a
`.done` file, a commit, a branch — and the orchestrator reads that record and
verifies every claim against git before acting on it. Nothing is delivered, so
nothing can be lost.

On top of that, the notifier drops at most one disposable line into an idle
parent's pane:

    [bramble] subagent activity — check your run directory

It names no child, no status and no path, because anything it carried could go
stale before it was read. It is:

- **droppable** — never queued, never persisted, never retried. A hint that does
  not land costs one poll interval of latency, not a lost report.
- **stateless** — no payload, no history, so a duplicate is harmless.
- **yielding** — any doubt about the pane (a draft in the composer, an
  unreadable frame, a turn in flight) means stay silent.

**Why the queue was removed.** The previous design pushed a generated report
through an at-least-once queue under `~/.bramble/deliveries/`. That queue could
not be both safe for a human's half-typed line and reliable, and it chose
reliability: undeliverable mail accumulated for days and replayed after
restarts, so a stale report and a real failure became indistinguishable. One
queue found in practice held 23 reports spanning 4.5 hours; another held ten
status updates each announcing that it superseded the last. `NewNotifier` sweeps
and *deletes* any such queue at startup rather than delivering it, because
replaying that history would reproduce exactly the noise this design removes.

**`send-input --queue` is refused, not downgraded.** A caller that asked to wait
for an idle recipient must not silently get a mid-turn interrupt instead. The
unqueued path is unchanged and stays the right tool for a deliberate interrupt
and for raw pane targets; it now leaves copy mode first, because a pane someone
scrolled back in swallows the Enter that would submit the text.

**No state change reaches the notifier through a buffer.** `SubscribeStateChanges`
takes a function called on the goroutine that made the transition, and
`watchStateChanges` puts a growable queue behind it. A bounded channel would have
to drop or block: drop loses exactly the fan-out case this feature is for (and a
tight burst fills any buffer before the reader is scheduled once, so a bigger one
is not the fix), and block would stall a status transition behind a pane capture.

### 3. Status is the report

There is no generated report and no composition step. What a parent reads is the
child's *status* — accurate because the manager that owns a session is the one
that records its transitions — plus whatever the lane wrote into the run
directory.

This is what makes Codex a first-class subagent: status comes from bramble's own
view of the session, so it is right whatever backend ran and whether or not the
agent inside cooperated. Codex has no system prompt, no MCP and no tool
restrictions in this wrapper, so a reporting instruction in its prompt is a
suggestion it may ignore — but its window dying, or its notify hook firing, is
not.

A tmux-mode child has no result file: `writeResearchFile` runs in the TUI turn
loop, which tmux sessions never enter. A tmux lane reports by writing the literal
path its brief names, which is what `subagent-swarm` briefs every lane to do.

## Three bugs this surfaced

All three were found by running a real Claude parent against a real Codex
subagent, and none are visible from unit tests alone.

### Codex never reported idle

The `notify` Stop hook was injected for the Claude provider only, so bramble
never learned a Codex window had finished. The session sat at `running` forever
after answering: nothing drained its queue, and its parent heard only when the
window finally died.

Codex has a top-level `notify` config — the exact analogue of Claude's Stop
hook — so `tmuxRunner.buildCommand` now passes
`-c notify=["<bramble>","notify","--silent","--session-id","<id>"]`. Codex
appends its own JSON payload as a trailing argument, which `bramble notify`
ignores.

### Nothing ever marked a tmux session running again

A tmux session's status comes entirely from outside: the agent's notify hook
reports idleness and nothing reports the opposite. A session bramble typed into
stayed `idle` for the whole turn, so the notify that *ended* that turn hit the
`StatusRunning` guard in `SetSessionIdle` and was dropped — no state change, no
drain, no report. Every two-way conversation died after one round.

`Manager.SetSessionRunning` closes this: submitting a delivery records that a
turn started.

### Pastes were silently dropped, and Enter silently swallowed

An agent CLI announces idleness the moment its turn ends, but its TUI can still
be finalizing and will drop a paste that arrives in the gap. tmux reports
success either way. Worse, a pane someone scrolled back in sits in **tmux copy
mode**, where the pager eats the Enter — the message lands in the composer and
sits there, delivered by every measure bramble can see and never read.

The fix that survives is leaving copy mode first — `tmuxctl`'s pane writer does
it, and so does `send_input`'s direct paste. `send-keys -X cancel` exits non-zero
on a pane that is *not* in a mode, so the mode is queried first; cancelling
blindly would fail every write.

The read-back-and-retry half is gone with the queue. There is deliberately **no**
check that a paste or its Enter was taken: an agent CLI echoes the submitted
prompt into its transcript directly above the composer, so a pane scrape cannot
distinguish "still pending" from "just submitted". For a droppable hint the
question does not arise — a write that did not land is simply a hint not given.

### Cursor has no hook at all, so its idleness is read off the pane

`cursor-agent` has no `--notify` flag, and its plugin `stop` hook does not fire
from the CLI — checked both interactively and with `--print` against
2026.08.11, in a `--plugin-dir` plugin and in a workspace `.cursor/hooks.json`.
So there is nothing to inject, and bramble would only ever learn a cursor
subagent finished when its window died.

`bramble/session/pane_idle.go` reads the state off the pane instead, for
providers with no hook only. Two things make that safe enough to act on:

- **It keys on the composer line, not a window of trailing lines.** Cursor's
  footer grows a mode line in plan mode — which is what a codetalk subagent
  runs in — and a fixed trailing window then misses the working hint and reads a
  running turn as *idle*, which is what would put a line into it. The hint lives
  on the
  composer line, so that line is found first and only it is examined.
- **`Add a follow-up` is not an idle marker.** Cursor shows it the whole time.
  Only `ctrl+c to stop`, on that same line, means a turn is in flight. This is
  recorded in the probe table because it is exactly the wrong guess to make.

Two consecutive agreeing polls are required, the probe reports *unknown* rather
than guessing when it cannot find the composer line, and a provider is only
listed once its chrome has been checked against the real CLI. Claude and codex
are deliberately absent: they report their own turn ends, and a second, weaker
signal could only contradict them.

Both tmux monitor loops poll it — the one `runSession` starts, and the one a
session re-adopted after a restart gets — through
`Manager.newPaneIdleTrackerForModel`, and
`TestReadoptedCursorSubagentIsStillSeenToFinish` drives the second of those for
real: it restarts bramble under tmux and asserts the re-adopted cursor session
is still seen to finish a turn. A loop that skipped it would leave a cursor
subagent that outlived a bramble restart reporting *running* forever — and since
status is what the orchestrator polls, its lane would never be seen to finish,
which is the whole failure this section exists to fix.

## Known limitations

- The cursor probe keys on that CLI's chrome and will go stale when cursor
  changes it. The failure is fail-safe — an unrecognized footer reports
  *unknown*, so the session simply stops being detected as idle rather than
  being detected wrongly — and `TestLiveCursorSubagentTwoWay` is there to fail
  loudly when it happens.
- Agy has neither a hook nor a probe, so subagents on that backend
  still report only when their window dies.
- Only claude's composer can be read, so only claude's pane is protected from a
  hint landing on a half-typed line. Cursor and codex render placeholder text
  that vanishes the moment a user types, making a draft indistinguishable from a
  CLI still booting. This is a documented gap rather than a solved case — but
  the exposure is one disposable line, and nothing retries it.
- A hint is dropped whenever the parent is not idle at that instant, which for a
  busy parent can be most of the time. That is by design: the poll is the
  delivery path, and the hint only shortens the wait.

## Files

| Path | Role |
|---|---|
| `bramble/session/delivery.go` | notifier: the droppable pane hint and the legacy-queue sweep |
| `bramble/session/manager.go` | `SpawnOpts`, `SetSessionRunning`, result file for any subagent |
| `bramble/session/tmux_runner.go` | Codex notify hook |
| `bramble/session/pane_idle.go` | pane-read idleness for hookless providers (cursor) |
| `bramble/integration/` | end-to-end tests: a real bramble in tmux, stubbed and live backends |
| `bramble/tmuxctl/panewriter.go` | `session.PaneWriter` adapter; exits copy mode |
| `bramble/control/{proto,dispatcher}.go` | `send_input`; `Queue` is refused |
| `bramble/ipc/protocol.go`, `bramble/main.go` | `--parent`, `--no-parent`, `--from` |

## Tests

`bramble/integration/` runs a real bramble binary, in tmux mode, on a throwaway
worktree repo, against a private tmux server and a private HOME. It is tagged
`manual` so `bazel test //...` skips it — it needs tmux, a terminal, and real
agent CLIs — and is run with:

    bazel test //bramble/integration:integration_test --test_output=all

Two layers:

- **Stubbed.** Scripted stand-ins for the agent CLIs, installed on PATH as
  `codex` and as `agent` (cursor's binary), exercise bramble's own logic
  deterministically and with no credentials: lineage, the notify hook, a refused
  `--queue`, delivery into a pane left in copy mode, and a full two-round
  conversation. The cursor stand-in is faithful about one thing only — the
  composer footer, which is the entire idle signal for a backend with no hook.
  - `TestReadoptedCursorSubagentIsStillSeenToFinish` restarts bramble (by
    signal, so its sessions are written to the store the way a real quit writes
    them) and takes another turn on the re-adopted cursor child. It is the only
    test that runs `monitorTrackedTmuxWindow`, the loop a session gets after a
    restart, and neither loop can be driven without a tmux server.
- **Live.** Two tests drive the real claude, codex and cursor CLIs, one subtest
  each, with a Claude parent. They run by default and skip only when a backend
  is missing or logged out, because every bug this feature shipped with was
  invisible without a real CLI in a real pane.
  - `TestLiveSubagentTwoWay` — a two-round conversation, asserting that each of
    the child's turns is observable as a status transition. It deliberately does
    *not* assert that a hint arrived: a hint is dropped whenever the parent is
    not idle at that instant, so requiring one would be requiring the guarantee
    this design removed.
  - `TestLiveBusyChildIsNeverWrittenInto` — the harmful direction for the pane
    probes. The subagent is given a twenty-second shell command, and the test
    asserts nothing is typed into the running turn and that the session is not
    mistaken for idle while it works. A false idle is what would put a line into
    a live turn. The unit tests pin this against a synthetic pane; only this
    pins it against the real chrome.

  `TestSubagentOnItsOwnWorktree*` cover `--create-worktree`: the isolation is
  asserted against git, not against the path bramble reports — the branch it is
  on, the base it came from, and a file written on one tree not appearing on the
  other. The return path is checked across that boundary too, since lineage
  travels by session ID rather than by tree.

  `TestConcurrentSubagentsCoalesceIntoOneNudge` and
  `TestABusyParentAccumulatesNothing` cover a fan-out: several subagents working
  at once with the same parent. Coalescing is what this pins — many children
  finishing must not put one line per child into the parent's pane — and the
  second holds the parent mid-turn, where the correct outcome is that *nothing*
  accumulates: with no queue there is nowhere for a hint to wait, so a busy
  parent simply misses them and reads the run directory instead.

  A backend is occupied with `sleep` rather than a long answer because generated
  text is not a clock — a model told to count slowly may emit the whole list at
  once, leaving no live turn to write against.

The live cases answer the CLIs' first-run dialogs themselves — Claude's folder
trust, codex's directory trust, its model-deprecation and rate-limit prompts —
because a fresh worktree puts one in front of every backend, and an unanswered
one looks exactly like a bramble that never reached its prompt. Each entry names
the option it takes: pressing Enter on an unrecognized menu is how a test
silently changes a setting.

The stubbed cases put a stand-in `gh` on PATH beside the stand-in agent.
Creating a worktree goes through `wt.FetchOrigin`, which gates on `gh auth
status`; under the isolated HOME the real gh cannot find its credentials, so
every worktree creation would fail on an authentication error unrelated to what
is under test. The fetch itself is against a local path and needs no
credentials. The seed repo is a real clone with remote-tracking refs for the
same reason — `wt` branches from `origin/<base>`, and a bare repo holding only
local branches fails with "invalid reference: origin/main", which says nothing
about why.

Two traps are worth knowing before adding to these tests. A unix socket path is
capped at ~107 bytes, and bazel's tmpdir plus a descriptive test name exceeds it
— both the tmux socket and bramble's runtime dir live under a short temp root
for that reason. And the stand-in deliberately waits before its first reply: an
agent that answers in under a millisecond lands inside `runner.Start()`'s settle
window, which is a real race (fixed, and pinned by
`TestFastIdleIsNotClobberedByStartup`) rather than anything a real CLI does.
