# Bramble mechanics

These are the swarm-specific mechanics whose failure is silent or destructive. Keep run
policy in `SKILL.md` and lane semantics in `pr-lane.md`.

## Preflight and run location

Run before the first spawn:

~~~bash
SW=~/.claude/skills/subagent-swarm/scripts
. "$SW/swarm_bramble.sh"
sw_doctor                 # assert the tool surface before trusting it
sw_preflight "$RUN"       # resolves SELF/TARGET/BASE/socket, writes $RUN/env.sh
. "$SW/tmux_safe.sh"; resolve_self
~~~

Every later command begins `. "$RUN/env.sh"` instead of re-deriving coordinates. Shell
state does not persist between calls, and hand-retyping this preamble has cost 30-40
repetitions per run.

The socket is `bramble-<uid>-<pid>.sock` — it carries the TUI's pid, so it changes on
every restart. `sw_socket` resolves it by globbing and **fails loudly on more than one
match** rather than picking the newest; silently attaching the swarm to the wrong TUI is
the "select live evidence by identity, never by list position" mistake. Never hardcode
the path: an earlier version of this file documented `bramble-$(id -u).sock`, which does
not exist, so `test -S` failed on step one of every run.

Use the wrapper rather than raw `bramble` calls, and prefer reading it over `--help`:
the help text has been stale before (it listed only `claude or codex` long after `cursor`
and `agy` worked, and two runs wrongly ruled agy out because of it). `sw_doctor` asserts
the facts the wrapper depends on, so drift breaks a check instead of a live run.

`BRAMBLE_SESSION_ID` is normally empty inside a session. Pass `--parent "$SELF"` on
every spawn; otherwise completed lanes report nowhere. If the client accepts `--parent`
but new sessions still arrive orphaned, the running TUI is stale. Restarting it kills live
sessions, so that decision belongs to the user.

Resolve the run against the current orchestrator branch:

~~~bash
TARGET=$(git rev-parse --abbrev-ref HEAD)
BASE=$(git symbolic-ref --quiet refs/remotes/origin/HEAD | sed 's#refs/remotes/origin/##')
BASE=${BASE:-main}
git fetch origin "$BASE"
SHARED=$(realpath -m "$(git rev-parse --path-format=absolute --git-common-dir)/../.shared")
RUN="$SHARED/subagent-swarm/$(date +%Y%m%d-%H%M%S)"
~~~

Require a clean orchestrator worktree. Every lane must fork from a commit containing the
current `TARGET`. Bramble's `-f` resolves against the remote; when `TARGET` has local
commits, create the worktree from local `TARGET` and spawn with `-w` instead.

## Spawn and record

For a remote-backed first phase:

~~~bash
bramble new-session -r "$REPO" --create-worktree -b "$BRANCH" -f "$BASE" \
  --parent "$SELF" -t "$TYPE" -m "$MODEL" -g "$ID" -p "$BRIEF"
~~~

Always pass `-r`; repository inference can choose the TUI's unrelated repository.
Dependent or locally based lanes use an explicit worktree:

~~~bash
git worktree add -b "$BRANCH" "$WORKTREE" "$TARGET"
bramble new-session -w "$WORKTREE" --parent "$SELF" \
  -t "$TYPE" -m "$MODEL" -p "$BRIEF"
~~~

`sw_spawn <lane> <phase> <model> <brief-file> [branch] [worktree]` performs both spawn
forms and records the result in the same call — ledger row, `spawn.json`, and the brief.
Recording used to be a separate step, and the fields that need that step are exactly the
ones that decayed across runs (`brief` from 13/13 to 0/12, `window_id` from 6/13 to 1/12).
A hand step that costs nothing to skip gets skipped.

If you spawn by hand, record immediately — the literal phase, session id, `realpath`
worktree, and fork SHA:

~~~bash
python3 "$SW/ledger.py" set "$RUN" --id "$ID" --status running \
  --phase "$PHASE" --session "$SESSION" --worktree "$(realpath "$WORKTREE")" \
  --phase-start-sha "$(git -C "$WORKTREE" rev-parse HEAD)"
~~~

`--phase-start-sha` pins what "this phase changed" means. Measured against a moving
TARGET, an empty branch can pass a `.done`.

Briefs must contain literal report paths; child environments do not point back to the run.
Confirm the wave with `bramble list-sessions --parent "$SELF"` and inspect fresh panes.
Codex commonly stops on the directory-trust dialog; submit its displayed trust choice
and then submit the composer before treating the lane as live.

## Track and communicate

`<lane>.<phase>.done` files are claims, and a pane hint is only a nudge that
something happened — it names no lane and may never arrive. The run directory and
git are the record. Before transitioning:

~~~bash
git -C "$WORKTREE" log --oneline "$PHASE_START_SHA..HEAD"
git -C "$WORKTREE" status --porcelain
~~~

For a phase expected to edit, no commit means it is not complete. For a read-only phase,
verify its specified artifact or review gate. Recheck HEAD immediately before integration;
cleanup and review sessions can amend after an early idle signal.

For a live deploy or measurement, verify the authoritative release revision advanced and
the intended change is present on the running target. A successful deploy command,
readiness, or a downstream endpoint can all describe the previous revision.

Run `snapshot_at_risk.sh "$RUN"` every tick. It backs up lanes with uncommitted work to
`refs/backup/<lane>` without changing their index or HEAD. Never use an empty branch or
an idle report as evidence that no backup is needed.

`bramble send-key --session-id <id> <Key>` submits a composer or answers a dialog; a run
spent 29 hours fighting raw `tmux send-keys` before noticing it. `sw_nudge` sends, refuses
to stack onto pending pastes, and confirms the pane went busy before returning.

A session whose `list-sessions` row has no `tmux_target` has no pane: it is **gone**, not
idle. That is decidable immediately, without waiting out a stall timeout, and piping an
empty target into `capture-pane` errors on precisely the lane most in need of being
reported dead. Rows carry `worktree_name`, never `worktree_path`, and `list-sessions`
returns `{"sessions": [...]}` — a dict, not a bare list.

Use the run directory for reports. Send a live-session nudge only to an idle
session — `--queue` is refused — then inspect the pane:
Codex can fire idle mid-turn and Cursor can leave pasted instructions unsubmitted.
`poll_panes.sh "$RUN"` detects questions, trust prompts, and stacked pastes. Do not resend
while an earlier paste is still visible. Confirm a nudge started work via the pane timer or
moving git state; replace a wedged session on the same worktree rather than debugging its
composer indefinitely.

## Watch and reap

Use one watcher. Reap and arm it in separate calls so the kill pattern cannot match the
new watcher's own argv:

~~~bash
pkill -f '[w]atch_lanes[.]sh'
pgrep -f '[w]atch_lanes[.]sh' | wc -l
"$SW/watch_lanes.sh" "$RUN" &
~~~

The count must be zero before arming. The watcher wakes on a new `.done` or static lane,
not on every commit.

There is no `bramble kill-session`. Reaping is three independent layers — process,
worktree (`wt remove` or `git worktree remove`), and tmux window — and each must be
verified separately. Kill a lane session before removing its worktree.

**Resolve the session from `bramble list-sessions` by `worktree_name`, never from the
ledger's `window_id`.** That field decays — one live run has it populated for 1 of 12
lanes — so a reap plan built from the ledger silently omits the kill step and removes a
worktree out from under a running agent. Seen live: a lane marked `done` with
`window_id` empty while an idle session still held pane `@1380` on its worktree.
`ledger.py doctor --sessions` reports this case by name.

**Gate deletion on a two-dot diff, not three.** `git diff target...branch` compares
against the merge-base, which predates a squash commit, so a squash-merged branch still
shows its own changes and reads as unmerged. In a squash-merging repo that makes a reaper
refuse every completed lane and leak worktrees forever. Compare tips (`target..branch`),
or better, verify the change is present on the base by content. Resolve the tmux window from the session
id and use the fail-closed helper:

~~~bash
. "$SW/tmux_safe.sh"
safe_kill_window "$(window_for_session "$SESSION")" "$ID"
~~~

Never kill by window name, index, or worktree path. Before deleting a branch or worktree,
verify the authorized integration artifact directly; ancestry alone is insufficient after
squash or rewritten history. Then remove the worktree, branch, and `refs/backup/<lane>`.
`audit_cleanup.sh "$RUN"` reports leaked sessions, panes, worktrees, branches, and refs.

## Recover

The loop reminder carries the only required coordinates:

~~~bash
. "$RUN/env.sh"                       # socket and coordinates, re-derived
cat "$RUN/OBJECTIVE.md"
python3 "$SW/ledger.py" show "$RUN"
python3 "$SW/ledger.py" doctor "$RUN" # where the ledger disagrees with reality
ls "$RUN"/*.done "$RUN"/*.needs-swe 2>/dev/null
bramble list-sessions --parent "$SELF"
~~~

A running ledger lane with no live session either finished without reporting or died.
Inspect its branch, worktree, pane history, and report files before deciding which.
Select live evidence by identity and recency, never by list position; record which instance
and timestamp produced any result used to advance a lane.
