# Bramble Cycle 1 -- Code Review Summary

**Reviewer**: Code review agent
**Date**: 2026-02-07
**Files reviewed**: toast.go, toast_test.go, helpoverlay.go, helpoverlay_test.go, welcome.go, welcome_test.go, model.go, update.go, view.go
**Reference**: docs/cycle1-architecture.md

---

## Issues Found and Fixes Applied

### Critical (bugs that would cause panics or incorrect behavior)

#### 1. Toast truncation panic on small terminal widths
**File**: `bramble/app/toast.go`, line 141-142
**Problem**: The truncation guard checks `len(content) > tm.width-4` but then slices to `content[:tm.width-7]`. When `tm.width < 7`, this produces a negative slice index, causing a runtime panic.
**Fix**: Changed the condition from `tm.width > 0` to `tm.width > 7` so truncation is only attempted when there is enough room.
**Test added**: `TestToastSmallWidth` in toast_test.go verifies no panic with `SetWidth(5)`.

#### 2. HelpOverlay scroll offset never applied in View()
**File**: `bramble/app/helpoverlay.go`, View() method
**Problem**: `ScrollDown()` increments `scrollOffset` and the comment says "Clamped during rendering", but the original `View()` method never reads or applies `scrollOffset`. All content is rendered unconditionally. Scrolling was a complete no-op.
**Fix**: Rewrote `View()` to:
- Build content as a slice of lines
- Clamp `scrollOffset` to valid range based on content height vs. available terminal height
- Slice the visible window of lines based on `scrollOffset`
- Show scroll indicators ("scroll up/down for more") when content extends beyond the viewport
- Keep the footer "Press ? or Esc to close" always visible (outside the scrollable area) for UX

#### 3. Footer text scrolled off-screen in help overlay
**File**: `bramble/app/helpoverlay.go`, View() method
**Problem**: After fixing scrolling, the footer "Press ? or Esc to close" was part of the scrollable content. With typical help content (~34 lines) and a standard 24-row terminal, the footer would be cut off at the initial scroll position.
**Fix**: Separated the footer from the scrollable content area. The footer is appended after the visible slice and scroll indicators, ensuring it is always displayed regardless of scroll position.

### Medium (incorrect behavior, not a crash)

#### 4. `renderKeyHint` uses `%-18s` format on ANSI-styled strings
**File**: `bramble/app/welcome.go`, line 114
**Problem**: The format verb `%-18s` pads based on byte length, which includes ANSI escape sequences from `welcomeDescStyle.Render(action)`. Since escape codes add ~15 bytes, the visual padding is too short, resulting in misaligned columns in the welcome screen.
**Fix**: Pad the plain-text `action` string to 18 characters before applying the ANSI style, then use `%s` (no width directive) for the styled result.

### Low (performance, style)

#### 5. Style objects allocated inside render loop
**File**: `bramble/app/helpoverlay.go`, lines 75-76, 82-83 (original)
**Problem**: `lipgloss.NewStyle()` was called inside `View()` on every render, creating new style objects each time. In a TUI that re-renders on every keypress, this causes unnecessary GC pressure.
**Fix**: Extracted `helpSectionTitleStyle`, `helpKeyStyle`, `helpKeyAlignStyle`, and `helpBoxStyle` to package-level `var` declarations, matching the pattern used by `toast.go` and `view.go`.

---

## Issues Noted (not fixed -- minor/nitpick)

### 6. `len(content)` for truncation uses byte length, not rune width
**File**: `bramble/app/toast.go`, line 141
**Detail**: The toast truncation uses `len(content)` which counts bytes, not visual character width. Since the toast icons are multi-byte UTF-8 characters, the truncation point may be slightly off for very narrow terminals. In practice, this is harmless since the icons are only 3-4 bytes and the width guard (`> 7`) prevents issues.

### 7. `renderSessionListView` mutates value-receiver Model
**File**: `bramble/app/view.go`, lines 245-249
**Detail**: `renderSessionListView` is called on a value receiver `Model` and mutates `m.selectedSessionIndex` for bounds clamping. The mutation is lost since Go copies value receivers. This is a pre-existing issue (not introduced by Cycle 1) and doesn't cause incorrect rendering since the clamped value is only used locally within the same function call.

### 8. `welcomeSummaryStyle` is defined but never used
**File**: `bramble/app/welcome.go`, lines 26-29
**Detail**: The style variable `welcomeSummaryStyle` is defined at package level but no code references it. It should either be used or removed.

### 9. Help overlay `?` key cannot be triggered from input mode
**File**: `bramble/app/update.go`, `handleInputMode()`
**Detail**: When the user is in input mode (typing a prompt), pressing `?` inserts the character into the text area rather than opening help. The architecture doc acknowledges this as by-design for the task modal but doesn't mention input mode. This is acceptable behavior -- users can always press `Esc` first then `?`.

---

## Test Coverage Assessment

### Toast (toast_test.go)
- **Good**: Tests Add, MaxStack, Expiry, IsExpired, Height, Rendering, DurationByLevel, and integration via Model.Update
- **Added**: TestToastSmallWidth for the panic fix
- **Missing**: No test for the `scheduleToastExpiry` helper returning correct delay timing (would require inspecting the tea.Cmd, which is complex in BubbleTea)

### Help Overlay (helpoverlay_test.go)
- **Good**: Tests context awareness, focus restoration, key handling, rendering, scrolling, and session-based sections
- **Note**: Tests now correctly pass with the scroll fixes

### Welcome (welcome_test.go)
- **Good**: Tests both variants (no worktrees, with worktrees), worktree status, sessions, operation messages, and integration with renderOutputArea
- **Missing**: No test for tmux mode variant (noted in test file as a TODO)

### Overall
The test suite is solid for unit testing. Each feature has tests for its core logic, rendering output, and integration with the Model. The test patterns are consistent with the existing `output_test.go` style.

---

## Integration Assessment

The three features integrate cleanly:

1. **Toast + Help Overlay**: Toasts auto-dismiss via timers while help overlay is open. Toasts are not visible during help (overlay replaces entire view), but they continue to expire. No stale toasts after closing help.

2. **Toast + Welcome**: Toast area is accounted for in the `View()` layout height calculation. Welcome screen shrinks when toasts are visible. No conflicts.

3. **Help Overlay + Welcome**: Independent rendering paths. Help overlay replaces the entire view. Welcome is rendered in the center area when no session is selected.

4. **Focus management**: All three features use the existing `FocusArea` enum correctly. `FocusHelp` is checked first in `Update()` (highest priority), consistent with the architecture doc.

5. **BubbleTea patterns**: Cmd returns are correct throughout. `addToast` returns a `tea.Cmd` that schedules the expiry tick. Help overlay returns `nil` (no async work). Welcome is pure rendering.

---

## Overall Assessment

The Cycle 1 implementation is well-structured and closely follows the architecture doc. The code is clean, well-organized into separate files per feature, and follows established codebase patterns (package-level styles, component structs with View() methods, etc.).

**4 issues were fixed** (1 panic bug, 1 non-functional feature, 1 UX bug, 1 performance issue). The remaining items are minor style notes. After fixes, all tests pass and the full `bazel build //...` succeeds.

**Verdict**: Ready to merge after fixes applied.
