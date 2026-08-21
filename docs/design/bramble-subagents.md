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
   section in its prompt simply never reported — and a Codex or Gemini child,
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
inherits its parent's worktree.

The delegator deliberately does *not* set a parent: it runs its own child
watcher and would otherwise be told about every transition twice.

### 2. The courier — `bramble/session/delivery.go`

One place that can address a session without the caller knowing how it runs:

| Runner | Write path |
|---|---|
| `tui` | `Manager.SendFollowUp` — a real prompt in the turn loop |
| `tmux` / `tmux-tracked` | `tmuxctl` paste + Enter into the pane |

Before this, `SendFollowUp` reached only TUI sessions and `send-input` only tmux
ones, and neither was reachable from the other's caller.

`send-input --queue` routes through the courier. If the recipient is idle the
message is written now; otherwise it is queued under `~/.bramble/deliveries/`
and written on the next idle transition, driven by
`Manager.SubscribeStateChanges` — no polling. A recipient in a terminal state is
refused rather than queued, and a queue whose session dies is reclaimed.

**One delivery per idle transition.** Writing a message starts the recipient's
next turn, so a second write in the same drain would land mid-turn — the very
thing the queue prevents. The rest ride the transition after.

The unqueued `send-input` path is unchanged, and stays the right tool for a
deliberate interrupt and for raw pane targets.

### 3. Automatic reporting

The same watcher reports a child's progress to its parent: a one-line headline
plus a path to the child's full output — pointer, not payload, the same shape as
the delegator reading a `research_file` instead of scraping a screen.

Because the report is generated from bramble's own view of the session, it
arrives whatever backend ran and whether or not the agent inside cooperated.
That is what makes Codex a first-class subagent: it has no system prompt, no MCP
and no tool restrictions in this wrapper, so a reporting instruction in its
prompt is a suggestion it may ignore.

Reporting is deliberately quiet: at most once per (child, status). A completed
or stopped that follows a report adds nothing and is suppressed; a failure is
always reported, because it changes what the parent should do next. A child that
messages its parent itself replaces the generated report.

Reporting is **per turn, not per lifetime**: any message written to a session
re-arms its idle report, so a conversation keeps flowing instead of going silent
after the first exchange.

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

Two fixes: the courier reads the pane back after pasting and retries once before
giving up (a failure leaves the delivery queued rather than dropping it), and
`tmuxctl`'s pane writer leaves copy mode first. `send-keys -X cancel` exits
non-zero on a pane that is *not* in a mode, so the mode is queried first —
cancelling blindly would fail every delivery.

There is deliberately **no** read-back check that the Enter was taken. An agent
CLI echoes the submitted prompt into its transcript directly above the composer,
so a pane scrape cannot distinguish "still pending" from "just submitted". A
false negative would re-queue a message the recipient already answered, which is
worse than the case it guards.

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
  running turn as *idle*, releasing queued mail into it. The hint lives on the
  composer line, so that line is found first and only it is examined.
- **`Add a follow-up` is not an idle marker.** Cursor shows it the whole time.
  Only `ctrl+c to stop`, on that same line, means a turn is in flight. This is
  recorded in the probe table because it is exactly the wrong guess to make.

Two consecutive agreeing polls are required, the probe reports *unknown* rather
than guessing when it cannot find the composer line, and a provider is only
listed once its chrome has been checked against the real CLI. Claude and codex
are deliberately absent: they report their own turn ends, and a second, weaker
signal could only contradict them.

## Known limitations

- The cursor probe keys on that CLI's chrome and will go stale when cursor
  changes it. The failure is fail-safe — an unrecognized footer reports
  *unknown*, so the session simply stops being detected as idle rather than
  being detected wrongly — and `TestLiveCursorSubagentTwoWay` is there to fail
  loudly when it happens.
- Gemini and Agy have neither a hook nor a probe, so subagents on those backends
  still report only when their window dies.
- A subagent's output reaches its parent as ordinary prompt text. A parent
  should treat it as data, not instructions — during testing a Claude parent
  correctly flagged a captured cursor transcript as a possible prompt-injection
  attempt.
- Paste verification costs up to ~1.8s per delivery when a TUI is slow to
  render, and passes vacuously if the pasted text is under a few characters.
- A modal in the recipient's TUI (Codex's rate-limit prompt, for instance)
  blocks delivery. This is correctly reported as an error rather than being
  Enter-ed through — pressing Enter would select a menu item — but it needs a
  human.

## Files

| Path | Role |
|---|---|
| `bramble/session/delivery.go` | courier: queue, persistence, mode-aware write, paste verification |
| `bramble/session/subagent_report.go` | report composition and the quiet/re-arm rules |
| `bramble/session/manager.go` | `SpawnOpts`, `SetSessionRunning`, result file for any subagent |
| `bramble/session/tmux_runner.go` | Codex notify hook |
| `bramble/session/pane_idle.go` | pane-read idleness for hookless providers (cursor) |
| `bramble/integration/` | end-to-end tests: a real bramble in tmux, stubbed and live backends |

## Tests

`bramble/integration/` runs a real bramble binary, in tmux mode, on a throwaway
worktree repo, against a private tmux server and a private HOME. It is tagged
`manual` so `bazel test //...` skips it — it needs tmux, a terminal, and real
agent CLIs — and is run with:

    bazel test //bramble/integration:integration_test --test_output=all

Two layers:

- **Stubbed.** A scripted stand-in for an agent CLI, installed on PATH as
  `codex`, exercises bramble's own logic deterministically and with no
  credentials: lineage, the notify hook, queued delivery, delivery into a pane
  left in copy mode, and a full two-round conversation.
- **Live.** `TestLiveSubagentTwoWay` drives the real claude, codex and cursor
  CLIs, one subtest each, with a Claude parent. These run by default and skip
  only when a backend is missing or logged out, because every bug this feature
  shipped with was invisible without a real CLI in a real pane.

The live cases answer the CLIs' first-run dialogs themselves — Claude's folder
trust, codex's directory trust, its model-deprecation and rate-limit prompts —
because a fresh worktree puts one in front of every backend, and an unanswered
one looks exactly like a bramble that never reached its prompt. Each entry names
the option it takes: pressing Enter on an unrecognized menu is how a test
silently changes a setting.

Two traps are worth knowing before adding to these tests. A unix socket path is
capped at ~107 bytes, and bazel's tmpdir plus a descriptive test name exceeds it
— both the tmux socket and bramble's runtime dir live under a short temp root
for that reason. And the stand-in deliberately waits before its first reply: an
agent that answers in under a millisecond lands inside `runner.Start()`'s settle
window, which is a real race (fixed, and pinned by
`TestFastIdleIsNotClobberedByStartup`) rather than anything a real CLI does.
| `bramble/tmuxctl/panewriter.go` | `session.PaneWriter` adapter; exits copy mode |
| `bramble/control/{proto,dispatcher}.go` | `Queue`/`From` on `SendInputReq` |
| `bramble/ipc/protocol.go`, `bramble/main.go` | `--parent`, `--no-parent`, `--queue`, `--from` |
