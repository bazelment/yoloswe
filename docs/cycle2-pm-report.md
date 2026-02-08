# Bramble TUI - Cycle 2 Product Research Report

## 1. Cycle 1 Review

Cycle 1 delivered three features targeting first-use discoverability:

1. **Help Overlay (`?` key)** -- `helpoverlay.go` (287 lines). Context-aware modal showing keybindings grouped by Navigation, Sessions, Worktrees, Output, Dropdown, Input, and General. Adapts sections based on `previousFocus` state. Scrollable with `j/k`. Code review fixed scroll offset and footer pinning.

2. **Toast Notification System** -- `toast.go` (165 lines). Three severity levels (success/info/error) with auto-dismiss timers (3/4/5 seconds). Integrated into layout via `toasts.Height()` in `view.go:76`. Applied to worktree operations, session starts, editor failures, tmux mode guidance, and `errMsg` handling. Stack limited to 3 concurrent toasts.

3. **Welcome/Empty State** -- `welcome.go` (163 lines). Two variants: no-worktrees (first-run onboarding) and worktrees-exist (quick-start with worktree summary). Shows PR status, dirty/clean state, file count. Replaces the old `"No session selected"` string.

**What Cycle 1 did well:**
- The three features form a coherent discoverability layer. Help overlay answers "what can I do?", toasts answer "what just happened?", welcome screen answers "how do I start?"
- Clean component isolation: each feature is a self-contained file with its own styles.
- Code review caught real bugs (panic on small width, scroll not scrolling, footer cut off).

**What Cycle 1 left incomplete:**
- **Silent failures remain.** The Cycle 1 report (item 3.2) identified silent `return m, nil` as the "#1 most frustrating UX pattern." Toasts were added for some cases (tmux mode `s`/`f`, worktree ops, session start errors), but pressing `p`/`b`/`e`/`f`/`a`/`d`/`n` when preconditions are not met still returns `m, nil` silently. Specific locations:
  - `p` without worktree: `update.go:387`
  - `b` without worktree: `update.go:398`
  - `e` without worktree: `update.go:409`
  - `f` with non-idle session or no session: `update.go:451`
  - `a` without idle planner with plan: `update.go:475`
  - `n` without repo: `update.go:369`
  - `d` without worktree: `update.go:496`
- **Width calculation bugs remain.** `len(stripAnsi(s))` is used instead of `runewidth.StringWidth(stripAnsi(s))` at six call sites: `view.go:207`, `view.go:534`, `view.go:699`, `welcome.go:104`, `textarea.go:366`, `splitpane.go:113`. The `runewidth` package is already imported in `view.go` and `dropdown.go` but not used consistently.
- **Tmux prompt escaping still broken.** `tmux_runner.go:57` wraps args in single quotes (`"'" + arg + "'"`) but does not escape single quotes within `arg`. The code comment says "escape any single quotes" but the implementation does not.

---

## 2. Updated Pain Point Assessment

### Resolved by Cycle 1
- ~~No discoverability system~~ -- Help overlay covers this.
- ~~No onboarding/empty state~~ -- Welcome screen covers this.
- ~~Silent failures~~ -- *Partially* resolved. Tmux-mode `s`/`f` now show toasts. All other silent failures remain.

### Still Open (re-evaluated priority)

| # | Issue | Original Priority | Revised Priority | Rationale |
|---|-------|-------------------|------------------|-----------|
| 1 | Silent failures for most key actions | P0 (3.2) | **P0** | 7 key actions still fail silently. Toast system exists but was not wired to these. ~15 lines of code. |
| 2 | Width calculations using byte count | P1 (3.6) | **P1** | 6 call sites. Causes misaligned top bar and status bar with emoji/CJK. Very low effort. |
| 3 | Tmux prompt escaping | P1 (3.8) | **P1** | 1-line fix. Still causes data loss for prompts with apostrophes. |
| 4 | Per-session scroll position memory | P2 (3.10) | **P1** | Now that Cycle 1 made session switching more prominent via welcome screen and toasts, losing scroll position on switch is more noticeable. Very low effort. |
| 5 | Dropdown search/filtering | P0 (3.3) | **P1** | Still important for scale, but now that the welcome screen provides direct entry points, dropdown navigation is less of a bottleneck for the first few minutes. |
| 6 | Unified submit (Enter=submit) | P2 (3.9) | **P2** | Still non-standard but Tab-cycle was added, making the current behavior somewhat more discoverable. |
| 7 | Session progress in dropdown | P2 (3.12) | **P2** | Nice-to-have. Data is available in `SessionInfo.Progress`. |
| 8 | Task modal adjust mode broken | P1 (3.7) | **P1** | Still broken. No text input handling in `TaskModalAdjust`. |

### New Issues Discovered in Cycle 1 Code

| # | Issue | Priority | Details |
|---|-------|----------|---------|
| N1 | **Welcome screen uses `len(stripAnsi())` for column alignment** | P1 | `welcome.go:104` uses `len(stripAnsi(keyCol))` instead of `runewidth.StringWidth()`. Since key hints include `[Alt-W]` (ASCII-only), this currently works by accident, but will break if any key label includes non-ASCII characters. Part of the broader width fix. |
| N2 | **Toast width truncation uses byte length** | P1 | `toast.go:141` uses `len(content) > tm.width-4` -- byte count, not display columns. Emoji/CJK in toast messages will cause misaligned rendering. |
| N3 | **`splitpane.go:113` truncation is approximate** | P1 | Comment says "doesn't handle ANSI perfectly" and the truncation `line[:width]` cuts at byte position, potentially splitting ANSI escape sequences or multi-byte runes. |

---

## 3. Cycle 2 Recommendations

### Selection Criteria
For Cycle 2, I prioritize improvements that:
1. **Complete unfinished Cycle 1 work** -- The silent failures fix was identified as the #1 issue but was only partially implemented.
2. **Leverage infrastructure already built** -- The toast system exists and just needs to be wired to more call sites.
3. **Fix correctness bugs** -- Width calculation bugs affect rendering correctness; fixing them is a one-pass mechanical change.
4. **Maximize impact-per-engineering-hour** -- All three picks are low complexity with high daily-use impact.

---

### Pick 1: Action Feedback for All Unavailable Keys

**Description:** Wire the existing toast notification system to every key action in `handleKeyPress()` that currently fails silently. Each early `return m, nil` should instead call `m.addToast()` with a helpful message explaining the precondition.

**User story:** As a user, when I press `p` with no worktree selected, I see a brief toast `"Select a worktree first (Alt-W)"` instead of nothing happening. This tells me exactly what to do next.

**Priority:** P0 -- This was the #1 item in the Cycle 1 report but was only partially implemented. The toast infrastructure is already built and tested. This is the highest-impact, lowest-effort remaining work.

**Complexity:** Very Low (~25 lines of added `addToast` calls). No new components, no new state. Pure wiring.

**Detailed specification:**

| Key | Condition | Toast Message | Level |
|-----|-----------|---------------|-------|
| `p` | No worktree selected | `"Select a worktree first (Alt-W)"` | Info |
| `b` | No worktree selected | `"Select a worktree first (Alt-W)"` | Info |
| `e` | No worktree selected | `"Select a worktree first (Alt-W)"` | Info |
| `d` | No worktree selected | `"Select a worktree first (Alt-W)"` | Info |
| `n` | No repo name | `"No repository loaded"` | Error |
| `s` | No session selected | `"No active session to stop (Alt-S to select)"` | Info |
| `f` | No session or not idle | `"No idle session for follow-up"` | Info |
| `a` | No idle planner with plan | `"No plan ready to approve"` | Info |
| `alt+s` | In tmux mode | `"Sessions are in tmux windows; use prefix+w to list"` | Info |

**Files to modify:**
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/update.go` -- Add `addToast()` calls at each identified `return m, nil` location.

**Test approach:**
- Unit test in `update_test.go`: call `handleKeyPress` with each key when preconditions are not met, verify that the returned model has a toast in `m.toasts`.

---

### Pick 2: Per-Session Scroll Position Memory

**Description:** Remember the scroll offset for each session. When the user switches sessions and comes back, restore their previous scroll position instead of resetting to 0.

**User story:** As a user reviewing output from session A, I scroll up to a specific tool invocation. I switch to session B to check its status, then switch back to A. My scroll position is exactly where I left it, so I do not lose my place.

**Priority:** P1 -- Now that the welcome screen and toast system make session switching more prominent and frequent, losing scroll position on every switch is a notable quality-of-life regression. Very low effort.

**Complexity:** Very Low (~15 lines). Add a `map[SessionID]int` to `Model`, save/restore around session switches.

**Detailed specification:**

1. Add `scrollPositions map[session.SessionID]int` field to `Model` in `model.go`.
2. Initialize the map in `NewModel()`.
3. Before switching sessions (everywhere `m.viewingSessionID` is reassigned and `m.scrollOffset = 0` is set), save the current scroll offset:
   ```
   if m.viewingSessionID != "" {
       m.scrollPositions[m.viewingSessionID] = m.scrollOffset
   }
   ```
4. After switching to a new session, restore the saved offset:
   ```
   m.scrollOffset = m.scrollPositions[newSessionID]
   ```
5. Locations to modify:
   - `update.go:573-575` -- worktree dropdown Enter (session cleared on worktree switch)
   - `update.go:587` -- session dropdown Enter
   - `update.go:755` -- `startSession()` (new session starts at bottom, offset 0 is correct here, but save the *previous* session's offset)
   - `update.go:808-810` -- `deleteWorktree()` (session cleared)
   - `update.go:471-473` -- approve plan (`a` key, new builder session started)

**Files to modify:**
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/model.go` -- Add `scrollPositions` field.
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/update.go` -- Save/restore at session switch points.

**Test approach:**
- Unit test: simulate switching between two sessions, verify scroll offset is preserved for the first session after switching back.

---

### Pick 3: Fix Width Calculations (Byte Count to Display Columns)

**Description:** Replace all uses of `len(stripAnsi(s))` with `runewidth.StringWidth(stripAnsi(s))` across the codebase. This fixes misaligned rendering when emoji, CJK characters, or other wide characters appear in the UI.

**User story:** As a user with a branch name containing Unicode or whose session output includes emoji, the top bar, status bar, split pane, and toast areas render with correct alignment instead of visual misalignment.

**Priority:** P1 -- Not blocking adoption but causes visual glitches in common scenarios (session icons are emoji, PR status contains Unicode). The fix is mechanical, low-risk, and the correct function (`runewidth.StringWidth`) is already imported in two files.

**Complexity:** Very Low (~10 call-site changes across 5 files). No logic changes, only swapping the measurement function.

**Detailed specification:**

All call sites to change from `len(stripAnsi(s))` to `runewidth.StringWidth(stripAnsi(s))`:

| File | Line | Context |
|------|------|---------|
| `view.go` | 207 | `renderTopBar()` padding calculation |
| `view.go` | 534 | `formatOutputLine()` width truncation check |
| `view.go` | 699 | `renderStatusBar()` padding calculation |
| `welcome.go` | 104 | `renderKeyHint()` key column visual width |
| `textarea.go` | 366 | `View()` button status padding |
| `splitpane.go` | 113 | `padToSize()` line width comparison |
| `splitpane.go` | 115 | `padToSize()` line padding calculation |
| `output.go` | 200 | `formatOutputLine()` duplicate truncation check |
| `toast.go` | 141 | `View()` toast message truncation |

Additionally, for `splitpane.go:115` the truncation `line[:width]` must be replaced with a rune-aware truncation that does not split multi-byte characters or ANSI escape sequences. The `truncateVisual()` function in `dropdown.go:265-295` already implements this correctly and should be reused.

**Files to modify:**
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/view.go` -- 3 call sites
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/welcome.go` -- 1 call site
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/textarea.go` -- 1 call site
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/splitpane.go` -- 2 call sites + truncation fix
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/output.go` -- 1 call site
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/toast.go` -- 1 call site

**Imports to add:** `runewidth` needs to be imported in `welcome.go`, `textarea.go`, `splitpane.go`, `output.go`, and `toast.go`. It is already imported in `view.go` and `dropdown.go`.

**Test approach:**
- Unit test with strings containing emoji (e.g., session icons "📋", "🔨") and CJK characters, verifying that padding calculations produce correct visual widths.
- Visual spot-check: the top bar should align correctly when the selected worktree has emoji in its status line.

---

## 4. Why These 3 Together

These three picks form a "correctness and polish" cycle that completes Cycle 1's unfinished work:

1. **Action feedback** completes the silent-failure fix that was Cycle 1's #2 priority but was only partially wired up. Uses the toast system built in Cycle 1.
2. **Scroll position memory** eliminates the most noticeable quality-of-life issue now that session switching (via welcome screen hints and toast confirmations) is more frequent.
3. **Width calculations** fixes a correctness bug that affects every screen (top bar, status bar, output area, split pane, toasts, welcome screen) and has been present since the original implementation.

All three are low complexity with no new UI components, no new state machines, and no architectural changes. They polish the existing foundation rather than adding new features.

### Total Cycle 2 Estimated Effort: ~2 days

| Pick | Effort |
|------|--------|
| Action feedback | 0.5 day |
| Scroll position memory | 0.25 day |
| Width calculation fixes | 0.5 day |
| Testing + integration | 0.5 day |

### What to Defer to Cycle 3

The following items are important but should wait until the correctness foundation is solid:

- **Dropdown search/filtering (3.3)** -- Medium complexity, requires new state in Dropdown component. Best tackled as a standalone feature.
- **Unified submit behavior (3.9)** -- Behavioral change that may surprise existing users. Needs careful UX testing.
- **Task modal adjust mode fix (3.7)** -- Requires either text input component integration or rethinking the adjust flow.
- **Tmux prompt escaping (3.8)** -- Very low effort but narrow impact; can be fixed opportunistically.
- **Session progress in dropdown (3.12)** -- Nice-to-have; data is available but the dropdown rendering needs to accommodate variable-width content.
