# Plan: Fix bramble command-center selection drift

Branch: `fix/bramble-new-session-wrong-worktree`

## Bug

User opens the bramble command center (Alt-C), highlights a non-first
session, presses `p` / `b` / `c` to start a planner / builder / codetalk
session — but the new session is created against the **wrong** worktree.
In the typical reproduction the wrong worktree is the one belonging to
whichever session is currently first in the list.

The handler that consumes the selection is correct — `startNewSessionFromOverlay`
at `bramble/app/update.go:1400` takes a `*session.SessionInfo` and reads
`sess.WorktreePath` / `sess.RepoName` straight off it. The bug lives one
level up, in the selection bookkeeping that produces that `*SessionInfo`.

## Verified call graph

For the command center (Alt-C):

1. User presses `p` / `b` / `c` while `FocusCommandCenter`
   → `bramble/app/update.go:2271` calls
   `m.startNewSessionFromOverlay(m.commandCenter.SelectedSession(), st, ...)`.
2. `commandCenter.SelectedSession()` (`bramble/app/commandcenter.go:164`)
   returns `&cc.sessions[cc.selectedIdx]` — purely a numeric-index lookup
   into the sorted slice it stores internally.
3. `startNewSessionFromOverlay` (`update.go:1400-1440`) captures
   `sess.WorktreePath` and `sess.RepoName` and threads them into the
   `startSessionMsg`. No further selection logic.

For the all-sessions overlay (Alt-S):

1. Same shape: `bramble/app/update.go:2064-2066` calls
   `m.startNewSessionFromOverlay(m.allSessionsOverlay.SelectedSession(), st, ...)`.
2. `AllSessionsOverlay.SelectedSession()`
   (`bramble/app/allsessions.go:81-86`) is also a numeric-index lookup.

So the bug reduces to: under what circumstances does the overlay's
internal `selectedIdx` point at a different session than the one the
user thinks they highlighted?

## Root cause

Two interacting issues in `bramble/app/commandcenter.go`. Both are in
the selection bookkeeping, and together they produce the "first session
in the list" failure mode the user sees.

### Issue 1 — `Show()` does not reset `selectedIdx`

`Show()` at `commandcenter.go:48-55` resets `scrollY`, `previewIdx`,
`previewText`, and `previewSessionID`, but **not** `selectedIdx`:

```go
func (cc *CommandCenter) Show(sessions []session.SessionInfo, w, h int) {
    cc.visible = true
    cc.scrollY = 0
    cc.previewIdx = -1
    cc.previewText = nil
    cc.previewSessionID = ""
    cc.loadSessions(sessions, w, h)
}
```

`Hide()` (line 86-91) also leaves `selectedIdx` alone. So across an
Alt-C → Esc → Alt-C cycle, `selectedIdx` carries over from the previous
visit. The session list, however, has been re-fetched and re-sorted
afresh — index N very likely points to a different session, or is out
of range entirely.

### Issue 2 — `loadSessions` clamps a stale index to `0` instead of preserving by ID

`loadSessions` (`commandcenter.go:35-45`) is the shared core of `Show()`
and `UpdateSessions()`:

```go
func (cc *CommandCenter) loadSessions(sessions []session.SessionInfo, w, h int) {
    cc.sessions = make([]session.SessionInfo, len(sessions))
    copy(cc.sessions, sessions)
    sortSessionsByPriority(cc.sessions)
    cc.width = w
    cc.height = h
    if cc.selectedIdx >= len(cc.sessions) {
        cc.selectedIdx = 0
    }
    cc.clampScrollY()
}
```

After `sortSessionsByPriority` reorders the slice, the same numeric
`selectedIdx` points at a different session — silent drift. And when
the list shrinks below the stale index, the clamp jumps the cursor all
the way back to **`0`** (the first session) rather than `len-1` or
something closer to the user's intent. That clamp produces exactly the
"wrong worktree is whichever session is first in the list" symptom in
the task description.

Compare with how `previewSessionID` is preserved across re-sort
(`commandcenter.go:62-76`): `UpdateSessions` captures the ID before
sorting and re-finds it after. Selection has no such guard inside
`loadSessions`.

### Why the existing wrapper does not fully save us

`refreshCommandCenter` (`update.go:985-996`) does wrap `UpdateSessions`
with an ID-based restoration:

```go
var prevID session.SessionID
if sel := m.commandCenter.SelectedSession(); sel != nil {
    prevID = sel.ID
}
m.commandCenter.UpdateSessions(m.gatherActiveSessions(), m.width, m.height)
if prevID != "" {
    m.commandCenter.RestoreSelectionByID(prevID)
}
```

This saves the **live-update** path — refreshes triggered by
`repoSessionEventMsg` (line 337) and `sessionsUpdated` (line 350). But
the **`Show()` path is unwrapped**: `update.go:809` does
`m.commandCenter.Show(activeSessions, m.width, m.height)` straight up,
no `RestoreSelectionByID`. And because `Show()` does not reset
`selectedIdx`, the stale cursor from the previous open survives,
gets re-sorted around, and — when the list is shorter than the stale
index — clamps to `0`. Repro: open command center, navigate to last
card, Esc, reopen later when the list is shorter, press `p`. The
cursor visually appears on the first card (because of the clamp), but
the user's mental model says "I had this session selected". They press
`p` and a session lands on the first card's worktree.

A subtler variant happens within a single open: if a refresh tick
arrives between the user's last navigation keypress and their `p`
keypress, the wrapped path runs `RestoreSelectionByID`, which **does**
work correctly — so this in-flight case is *not* the source of the
bug. The fix only needs to address the structural hole, not this
in-flight path. (We do, however, harden `loadSessions` itself so that
any future caller that forgets the wrapper inherits correct behavior
by default — see fix design below.)

### `AllSessionsOverlay` — verified clean

`AllSessionsOverlay` (`bramble/app/allsessions.go`) has only `Show()`
(line 32-38) which **does** reset `selectedIdx = 0`, and has no
`UpdateSessions` path. The overlay is therefore static between
`Show()` and `Hide()`, and `selectedIdx` cannot drift while it is
visible. No fix is needed there. (Kept in scope only as something to
verify in code review.)

## Fix design

**Primary fix — make selection survive in `loadSessions` itself.**

Push the ID-based selection restoration *into* `loadSessions`, mirroring
the pattern already used for `previewSessionID`. The contract becomes:
"after `loadSessions` returns, the cursor still points at the same
session it pointed at before, if that session is still in the list."

Concretely:

```go
func (cc *CommandCenter) loadSessions(sessions []session.SessionInfo, w, h int) {
    // Capture the selected session ID before we mutate the slice.
    var prevSelectedID session.SessionID
    if cc.selectedIdx >= 0 && cc.selectedIdx < len(cc.sessions) {
        prevSelectedID = cc.sessions[cc.selectedIdx].ID
    }

    cc.sessions = make([]session.SessionInfo, len(sessions))
    copy(cc.sessions, sessions)
    sortSessionsByPriority(cc.sessions)
    cc.width = w
    cc.height = h

    // Restore by ID if possible.
    if prevSelectedID != "" {
        for i := range cc.sessions {
            if cc.sessions[i].ID == prevSelectedID {
                cc.selectedIdx = i
                cc.clampScrollY()
                return
            }
        }
    }
    // Selected session is gone (or there was no selection): clamp into
    // the new range. Use len-1 instead of 0 so the cursor lands closer
    // to the user's last position when the bottom of the list shrinks
    // away from them.
    if cc.selectedIdx >= len(cc.sessions) {
        cc.selectedIdx = len(cc.sessions) - 1
    }
    if cc.selectedIdx < 0 {
        cc.selectedIdx = 0
    }
    cc.clampScrollY()
}
```

Why fold this into `loadSessions` rather than just hardening `Show()`:

- Defense in depth — once the contract is "selection follows the
  session", nobody downstream needs to remember to wrap with
  `RestoreSelectionByID`. The existing
  `refreshCommandCenter` wrapper becomes redundant; we can either leave
  it (harmless) or remove it as a follow-up cleanup. **In scope: leave
  it.** It is a no-op once the underlying behavior is correct, and
  removing it widens the diff into `update.go` for no functional gain.
  We will note it as a candidate for a tidy-up follow-up.
- It mirrors the existing `previewSessionID` pattern in the same file,
  so the code reads consistently.

**Secondary fix — `Show()` resets `selectedIdx` to `0`.**

`Show()` is the entry point — if the user re-opens the overlay, "land
on the first card" is the right default. Add an explicit
`cc.selectedIdx = 0` to `Show()` right next to the existing
`cc.scrollY = 0`. This must happen *before* `loadSessions` so that
`loadSessions`'s ID-preserving logic captures `prevSelectedID = ""`
(no carry-over) and falls through to the clamp branch, which now lands
the cursor at `0` for an empty/sized-down list.

Trade-off considered: should `Show()` instead remember the previously
selected session ID across visibility toggles, so the user comes back
to the same card? Rejected — too magical. Users open the command
center to survey state; defaulting to the top is unsurprising and
matches `AllSessionsOverlay` already.

**Clamp direction (`len-1` vs `0`).**

Changed from `→ 0` to `→ len-1`. Rationale: when the bottom of the list
shrinks away from a user whose cursor was below the new tail, landing
at the new tail matches their last intent more closely than yanking
all the way to the top. The "land at the top" semantics are still
correct in the only place that explicitly needs them — `Show()` —
because `Show()` now sets `selectedIdx = 0` itself before
`loadSessions` runs.

This change is observable, but only in a narrow case: terminal-state
sessions disappearing while the user has the very last card selected.
In that case the cursor moves to the new last card rather than the
first. Acceptable.

### Edge cases

| Case                                       | Behavior after fix |
|--------------------------------------------|--------------------|
| Empty list                                 | `selectedIdx = 0` (sentinel; `SelectedSession()` returns `nil`). |
| Selected session disappears entirely       | Falls through to clamp; lands on `len-1` (new tail).           |
| Selected session moves due to status sort  | Cursor follows it by ID — visual position changes, identity preserved. |
| `Show()` with a never-touched overlay      | `selectedIdx = 0` (already is, now explicit).                  |
| `Show()` after a previous Esc               | `Show()` resets to 0 → clamp branch → land on first card.      |
| Refresh tick during a single open          | `loadSessions` preserves ID directly; `refreshCommandCenter`'s wrapper becomes a redundant double-restore (harmless). |

## Scope boundaries

In scope:

- `bramble/app/commandcenter.go` — modify `loadSessions` and `Show()`.
- `bramble/app/commandcenter_test.go` — add tests below.

Explicitly out of scope:

- `bramble/app/update.go` — `startNewSessionFromOverlay`, the `p`/`b`/`c`
  key sites, and `refreshCommandCenter` are correct given a correct
  `SelectedSession()`. Do not touch.
- `bramble/app/allsessions.go` — verified to not have the same bug.
- IPC layer, `session.Manager`, sort order — bug is purely TUI
  selection bookkeeping.
- Removing the now-redundant `RestoreSelectionByID` call inside
  `refreshCommandCenter` — note as a follow-up cleanup, do not bundle.

## Test plan

All new tests go in `bramble/app/commandcenter_test.go`. The existing
helper `makeSessions()` (line 14) already produces a 4-session fixture
with mixed statuses that re-sort into a known order; reuse it.

1. **Selection survives `UpdateSessions` when sort order changes.**
   - Setup: `Show()` four sessions, navigate to a non-first index
     (e.g. `cc.selectedIdx = 2`), capture
     `selID := cc.SelectedSession().ID`.
   - Mutate one session's status so the next sort reorders it.
   - Call `cc.UpdateSessions(modifiedSessions, w, h)`.
   - Assert `cc.SelectedSession().ID == selID` — same session by ID,
     possibly different numeric index.

2. **Selection survives `Show()` re-entry when nothing has changed.**
   This documents the "Show resets to 0" decision. Open with sessions,
   move to index 2, `Hide()`, `Show()` again with the same sessions.
   Assert `cc.selectedIdx == 0` and `SelectedSession()` is the
   highest-priority session (idle).

3. **Selected session disappears: cursor lands at `len-1`.**
   - `Show()` with 4 sessions, `selectedIdx = 3` (last).
   - `UpdateSessions` with a list that omits the previously selected
     session and is also shorter (e.g. 2 items left).
   - Assert `cc.selectedIdx == 1` (new tail), and `SelectedSession()`
     is non-nil.

4. **Empty list keeps `SelectedSession()` nil-safe.**
   - `Show()` with 4 sessions, navigate, then `UpdateSessions(nil, ...)`.
   - Assert `cc.SelectedSession() == nil` and no panic.

5. **Live drift repro at the keypress level.** Build on the existing
   `TestCommandCenter_NewSession_PBC` (line 299): start with a
   two-session fixture where index 0 ≠ user's choice, navigate to
   index 1, fire an `UpdateSessions` (simulating a status-driven
   re-sort that would *previously* have drifted the cursor to index 0),
   then dispatch the `p` keypress and assert the resulting
   `pendingPrompt`/captured worktree path matches the **navigated** session,
   not the first one. This is the regression test that most directly
   pins the user-visible bug.

Existing tests that should still pass unchanged:

- `TestCommandCenter_PrioritySorting`
- `TestCommandCenter_NavigationGrid`
- `TestCommandCenter_RestoreSelectionByID` (still useful as a primitive)
- `TestCommandCenter_SelectByNumber`
- `TestCommandCenter_UpdateSessionsPreservesPreview` (preview path unchanged)
- `TestCommandCenter_UpdateSessionsClearsPreviewIfGone`
- `TestCommandCenter_HideClearsPreviewState`
- `TestCommandCenter_TogglePreviewTracksSessionID`
- `TestCommandCenter_NewSession_PBC` and `_NoSession`

Integration-test note: I checked
`bramble/app/integration/` for an existing test that drives the
new-session keypath end-to-end. There is no direct match — the
existing integration tests focus on session lifecycle and tmux mode,
not the overlay key handlers. **Do not add an integration test in this
PR.** Unit tests above are sufficient because the bug is purely in the
overlay's internal bookkeeping; the integration boundary
(`startNewSessionFromOverlay` → `StartSession`) is untouched.

## Out-of-scope alternatives considered

- **Switch the overlay to a stable session-ID cursor instead of a
  numeric index.** Cleaner long-term, but a much larger refactor —
  every navigation/preview/render site reads `selectedIdx` directly
  (`commandcenter.go:138, 148, 158, 165, 196, 204, 240, 246, 330`) and
  would need to translate through a lookup. Out of scope for a
  surgical fix.
- **Pause live updates while the overlay is open.** Bad UX — the
  whole point of the command center is real-time status.
- **Sort once at `Show()` and never re-sort on `UpdateSessions()`.**
  Loses the "newly idle sessions bubble to the top" affordance, which
  is a deliberate part of the priority sort and a real reason the
  overlay is useful. Rejected on UX grounds.
- **Make `refreshCommandCenter`'s wrapper the *only* fix and leave
  `loadSessions` alone.** Doesn't cover the `Show()` path, which is
  the actual repro mode — the wrapper is bypassed there. Also makes
  every future caller of `UpdateSessions` a footgun.

## Rollout

None. Internal TUI behavior only. No flag, no schema change, no
migration. Lands as a normal bug-fix PR.

## Verification

Per `CLAUDE.md`, before any code lands the builder must run:

- `scripts/lint.sh` — must pass.
- `bazel test //bramble/app/...` — must pass (with the new tests).

Manual smoke (recommended, not gating): in a multi-worktree bramble
session, open command center, navigate to a non-first session, watch
status ticks reorder the list, press `p`, confirm the new session
prompt names the **navigated** session's worktree.
