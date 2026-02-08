# Cycle 4 PM Report -- Bramble TUI

## Context

Three cycles have been completed:
- Cycle 1: Help overlay, toast notifications, welcome screen
- Cycle 2: Action feedback, scroll memory, width fixes
- Cycle 3: Scroll rendering refactor, TextArea key handler refactor, dropdown search/filtering

Remaining items from the original PM backlog:
- 3.6 Keyboard Shortcut Cheat Sheet in Top Bar (P2)
- 3.7 Session Status Transition Animations (P2)
- 3.9 Unified Submit Behavior (Enter=submit, Shift+Enter=newline) (P2)
- 3.10 Session Progress Summary in Dropdown (P2)

## Assessment of Remaining Backlog Items

**3.6 Keyboard Shortcut Cheat Sheet in Top Bar** -- Deprioritized. The `?` help overlay (Cycle 1) already provides comprehensive, context-aware key binding help. Adding inline hints to the top bar would clutter the already-dense top bar layout. The status bar at the bottom already shows contextual key hints that change based on state (see `renderStatusBar()` in view.go). This item is effectively done.

**3.7 Session Status Transition Animations** -- Deprioritized. The codebase already has a 100ms tick timer for running tool timers. Adding animation frames for status transitions (pending -> running -> idle) would increase visual noise without adding information. The status icons (circle, filled circle, half-circle) already convey state clearly. Low value for the implementation cost.

**3.9 Unified Submit Behavior** -- Already completed in Cycle 3. The TextArea now uses Ctrl+Enter for submit and Enter for newline when the text input is focused. Tab cycles focus to the Send/Cancel buttons where Enter activates them. This is clean and consistent.

**3.10 Session Progress Summary in Dropdown** -- Worth doing. The session dropdown currently shows only the session icon, a truncated title, and a status badge. It has no indication of cost, turn count, or how long the session has been running. This data is already available in `SessionProgressSnapshot` and just needs to be surfaced into the dropdown subtitle. Low effort, high information density gain.

## New Opportunities Identified

After reading the full codebase, three additional opportunities stand out:

### Opportunity A: Stale Session Auto-Cleanup in Dropdown

The session dropdown mixes live sessions with history sessions, separated by a `---separator---` item. However, sessions that have reached terminal states (completed, failed, stopped) still appear in the "live" section indefinitely until the process exits. They should either be automatically moved to history after a timeout, or at minimum be visually distinguished from truly active sessions.

### Opportunity B: Inline Status Bar Session Cost Tracker

The status bar (bottom) currently shows `Running: N  Idle: N` counts. For a tool focused on parallel AI agent sessions, the aggregate cost is crucial information. The data is already tracked per-session in `SessionProgressSnapshot.TotalCostUSD` -- summing across all active sessions and displaying `Total: $X.XX` in the status bar would give the user constant cost visibility without needing to click into each session.

### Opportunity C: File Tree "Open in Editor" Action

The file tree panel (F2) shows changed files with git status indicators and supports cursor navigation. However, pressing Enter on a selected file does nothing. The `e` key already opens the editor at the worktree root level. Adding Enter-to-open-file in the file tree would make the split pane genuinely useful for navigating changes, turning it from a read-only display into an actionable tool.

---

## Recommended Picks for Cycle 4

### Pick 1: Session Progress Summary in Dropdown (Backlog 3.10)

**Description**: Enrich the session dropdown subtitle to show turn count, total cost, and elapsed time for live sessions. Currently the subtitle only shows a truncated prompt. After this change, each live session item would show something like `T:5 $0.0312 3m ago | Fix auth bug in login...` instead of just `Fix auth bug in login flow for the...`.

**User Story**: As a developer managing multiple parallel sessions, I want to see at a glance how much each session has cost and how many turns it has taken, so I can decide which sessions need attention without switching to each one individually.

**Priority**: P2 -- Low effort, high information value for the core use case (parallel session management).

**Complexity**: Low. The data is already available in `SessionInfo.Progress` (turn count, cost). The elapsed time is computable from `SessionInfo.CreatedAt`. Only the dropdown item construction in `updateSessionDropdown()` needs to change.

**Files to modify**:
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/model.go` -- Modify `updateSessionDropdown()` to build richer subtitle strings for live sessions using `SessionInfo.Progress` fields.
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/view.go` -- Possibly adjust the session dropdown width if the enriched subtitles need more room.

**Spec**:
- For live sessions, the dropdown subtitle format becomes: `T:{turns} ${cost} {elapsed} | {prompt_truncated}`
- Turns and cost are omitted when zero (for pending sessions that have not started yet)
- Elapsed is computed from `CreatedAt` using the existing `timeAgo()` helper
- History sessions retain their current format (no progress data available)
- The session dropdown width may need to increase from `m.width / 2` to `m.width * 2 / 3` to accommodate the longer subtitles

---

### Pick 2: Aggregate Cost in Status Bar

**Description**: Add a running total cost display to the right side of the status bar, showing the sum of `TotalCostUSD` across all active sessions. The status bar currently shows `Running: N  Idle: N`. After this change it would show `Running: N  Idle: N  Cost: $X.XXXX`.

**User Story**: As a developer running multiple AI sessions in parallel, I want to see my total spend at all times without clicking into individual sessions, so I can stay aware of costs and stop sessions if spending exceeds my budget.

**Priority**: P2 -- Tiny implementation, directly addresses the core value proposition of Bramble (parallel session management + cost awareness).

**Complexity**: Very low. The `renderStatusBar()` function already computes session counts via `m.sessionManager.CountByStatus()`. Adding a cost sum just requires iterating `m.sessions` and summing `Progress.TotalCostUSD`.

**Files to modify**:
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/view.go` -- Modify `renderStatusBar()` to compute and display aggregate cost.
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/model.go` -- No changes needed; `m.sessions` already contains `SessionInfo` with progress data.

**Spec**:
- Add `Cost: $X.XXXX` to the right side of the status bar, after the Running/Idle counts
- Sum `Progress.TotalCostUSD` across all sessions in `m.sessions` (not just current worktree)
- Omit the cost display when the total is zero (no sessions have started yet)
- Use 4 decimal places to match the per-turn cost format already used in the output area
- When terminal width is small, the cost display should be the first thing truncated (keep Running/Idle counts as higher priority)

---

### Pick 3: File Tree Enter-to-Open Action

**Description**: When the split pane is active and the file tree has focus, pressing Enter on a selected file opens that file in the configured editor (the same editor used by the `e` key for worktree-level opening). This turns the file tree from a passive display into an actionable navigation tool.

**User Story**: As a developer reviewing changes in the split pane file tree, I want to press Enter on a changed file to open it directly in my editor, so I can quickly inspect or modify specific files without leaving Bramble.

**Priority**: P2 -- Moderate effort, but completes the file tree feature which is currently half-built (you can see files and navigate but cannot act on them).

**Complexity**: Low-medium. The infrastructure exists: the `e` key handler already opens the editor, `FileTree.SelectedPath()` already returns the selected file path, and `SplitPane.FocusLeft()` already indicates when the file tree has focus. The main work is wiring Enter in the file tree focus state to open `editor <worktreePath>/<selectedFile>`.

**Files to modify**:
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/update.go` -- Add Enter key handling in `handleKeyPress()` when split pane is active and left pane is focused. Open `m.editor` with the full file path.
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/filetree.go` -- Potentially add a method to return the absolute path (combining `root` + relative `SelectedPath()`).
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/helpoverlay.go` -- Add "Enter: Open file in editor" to the Output section bindings when split pane is active.

**Spec**:
- When split pane is active (`m.splitPane.IsSplit()`) and left pane has focus (`m.splitPane.FocusLeft()`), Enter opens the selected file
- The file path is constructed as `filepath.Join(worktreePath, selectedPath)`
- If `SelectedPath()` returns empty (cursor is on a directory header or "(no changes)"), show a toast: "No file selected"
- The editor is launched with `exec.Command(m.editor, filePath)` followed by `cmd.Start()` (non-blocking, same as existing `e` handler)
- A toast confirms the action: "Opening {filename} in editor"
- The help overlay Output section adds the binding when split pane is active

---

## Summary

| # | Item | Source | Priority | Complexity | Key Files |
|---|------|--------|----------|------------|-----------|
| 1 | Session Progress Summary in Dropdown | Backlog 3.10 | P2 | Low | model.go |
| 2 | Aggregate Cost in Status Bar | New | P2 | Very Low | view.go |
| 3 | File Tree Enter-to-Open Action | New | P2 | Low-Medium | update.go, filetree.go, helpoverlay.go |

All three picks share a theme: **making existing data actionable**. The progress data exists but is hidden in the session output view. The file tree exists but is read-only. These improvements surface information where users need it and add agency where the UI is currently passive.
