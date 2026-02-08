# Cycle 2 Code Review: Bramble TUI

## Scope

Reviewed all files from the Cycle 2 implementation covering three features:
1. **F1 - Action Feedback**: Toast notifications for silent-failure key paths
2. **F2 - Per-Session Scroll Position Memory**: Save/restore scroll on session switch
3. **F3 - Width Calculation Fixes**: Replace byte-length with visual-width calculations

## Bugs Fixed

### BUG 1 (Critical): `filetree.go` - Byte-length truncation with ANSI corruption

**File**: `bramble/app/filetree.go`, lines 262-266

**Problem**: The file tree truncation code had two compounding issues:
- Used `len(stripped) > width` (byte count) instead of visual column width
- Truncated the *ANSI-containing* `line` by byte index (`line[:width-3]`), which would slice through ANSI escape sequences and corrupt terminal output

**Before**:
```go
stripped := stripAnsi(line)
if len(stripped) > width {
    line = line[:width-3] + "..."
}
```

**After**:
```go
if runewidth.StringWidth(stripAnsi(line)) > width {
    line = truncateVisual(line, width)
}
```

`truncateVisual` correctly skips ANSI escape sequences and measures CJK/emoji widths. Added `go-runewidth` import.

### BUG 2 (Moderate): `view.go` `truncate()` - Byte-length string truncation

**File**: `bramble/app/view.go`, lines 728-737

**Problem**: The `truncate()` function used `len(s)` for comparison and `s[:max-3]` for slicing. This:
- Miscounts multi-byte UTF-8 characters (emoji, CJK, accented chars)
- Can split a multi-byte rune mid-byte, producing invalid UTF-8 output
- This function is called ~15 times across the codebase for prompts, tool inputs, names

**Fix**: Rewrote to iterate runes using `runewidth.RuneWidth()` for visual column counting, ensuring truncation respects character boundaries and display width.

### BUG 3 (Moderate): `view.go` `truncatePath()` - Byte-length path comparison

**File**: `bramble/app/view.go`, lines 780-795

**Problem**: Used `len(path)` and `len(suffix)` for byte-length comparison against visual column limit. Fixed to use `runewidth.StringWidth()`.

### BUG 4 (Minor): `update.go` `confirmTask()` - Dropped toast expiry command

**File**: `bramble/app/update.go`, line 1077

**Problem**: `m.addToast("Task confirmed, starting session...", ToastSuccess)` was called but its return value (a `tea.Cmd` that schedules toast expiry) was discarded. The toast would appear but never auto-dismiss because the expiry timer was never scheduled.

**Fix**: Captured the returned command and included it in `tea.Batch()` for both the new-worktree path and the existing-worktree path.

### BUG 5 (Minor): `model.go` `generateDropdownTitle()` - Byte-length word fitting

**File**: `bramble/app/model.go`, lines 449-469

**Problem**: Used `b.Len()` (byte count of builder) and `len(w)` (byte count of word) for width fitting, and `prompt[:maxLen-3]` for truncation. Produces incorrect results for non-ASCII prompts and can produce invalid UTF-8.

**Fix**: Rewrote to use `runewidth.StringWidth()` for column counting and delegate the single-long-word case to the now-fixed `truncate()` function.

## Issues Noted (Not Fixed)

### Style: `update.go` "a" handler - Manual scroll save instead of `switchViewingSession()`

**File**: `bramble/app/update.go`, lines 480-484

The approve-plan handler manually does:
```go
if m.viewingSessionID != "" {
    m.scrollPositions[m.viewingSessionID] = m.scrollOffset
}
m.viewingSessionID = sessionID
m.scrollOffset = 0
```

This duplicates the logic in `switchViewingSession()` and doesn't clear `viewingHistoryData`. In practice this is harmless (the user is viewing a live planner session, so `viewingHistoryData` is already nil), but it's an inconsistency that could cause a bug in future refactors. Consider using `switchViewingSession(sessionID)` followed by resetting scroll to 0 if needed.

### Style: `welcome.go` - `len(action)` for padding

**File**: `bramble/app/welcome.go`, line 113

Uses `len(action) < 18` for byte-length padding. Since all action strings in practice are ASCII ("New task", "Plan", "Build", etc.), this works correctly but is inconsistent with the runewidth approach used elsewhere.

### Style: `textarea.go` - Byte-based cursor/wrapping

**File**: `bramble/app/textarea.go`

Multiple places use `len(line)` for word-wrapping and cursor positioning. This is acceptable because the textarea operates on raw user input and cursor position is tracked by byte offset into the string, which is consistent. However, word-wrapping at `len(line) <= width` could produce visual overflow for lines with wide characters. This is pre-existing behavior not introduced by Cycle 2.

## Feature Review

### F1 - Action Feedback

**Coverage**: All silent-failure paths are now covered with appropriate toast notifications:

| Key | Condition | Toast Level | Message |
|-----|-----------|-------------|---------|
| p | No worktree | Info | "Select a worktree first (Alt-W)" |
| b | No worktree | Info | "Select a worktree first (Alt-W)" |
| e | No worktree | Info | "Select a worktree first (Alt-W)" |
| d | No worktree | Info | "Select a worktree first (Alt-W)" |
| n | No repo | Error | "No repository loaded" |
| s | No session | Info | "No active session to stop (Alt-S to select)" |
| s | Tmux mode | Info | "Close tmux windows directly..." |
| f | No idle | Info | "No idle session for follow-up" |
| f | Tmux mode | Info | "Follow-ups must be done in the tmux window directly" |
| a | No plan | Info | "No plan ready to approve" |
| Enter | Tmux, no sessions | Info | "No sessions to switch to" |
| Alt-S | Tmux mode | Info | "Sessions are in tmux windows..." |

Toast levels are appropriate: Error for hard failures, Info for guidance.

### F2 - Per-Session Scroll Position Memory

**Implementation**: Correct. `scrollPositions` map is initialized in `NewModel`. `switchViewingSession()` correctly:
1. Saves current scroll to `scrollPositions[old]`
2. Restores saved scroll from `scrollPositions[new]` (defaults to 0)
3. Clears `viewingHistoryData`

**Switch sites covered**:
- Worktree dropdown enter (clears session and scroll)
- Session dropdown enter (saves/restores between sessions)
- `startSession()` (saves old, starts new at 0)
- `deleteWorktree()` (clears if deleting current worktree)
- `confirmTask()` existing-worktree path (via `startSession()`)

**Edge case**: The `"a"` (approve plan) handler does manual scroll save instead of using `switchViewingSession()`. Functionally correct but inconsistent (noted above).

### F3 - Width Calculation Fixes

**Completed conversions**:
- `renderTopBar()`: Uses `runewidth.StringWidth(stripAnsi(...))` for padding
- `renderStatusBar()`: Same
- `formatOutputLine()`: Uses `runewidth.StringWidth(stripAnsi(formatted))` for width check, `truncateVisual()` for truncation
- `padToSize()`: Uses `runewidth.StringWidth(stripped)` and `truncateVisual()`
- `dropdown.go` `ViewList()`: Uses `lipgloss.Width()` for width check, `truncateVisual()` for truncation
- `toast.go` `View()`: Uses `runewidth.StringWidth(content)` for width check, `truncateVisual()` for truncation
- `textarea.go` `View()`: Uses `runewidth.StringWidth(stripAnsi(status))` for button alignment
- `welcome.go` `renderKeyHint()`: Uses `runewidth.StringWidth(stripAnsi(keyCol))` for alignment
- `output.go` `formatOutputLine()` standalone: Uses `runewidth.StringWidth(stripAnsi(formatted))` and `truncateVisual()`
- `spliceAtColumn()`: Uses `runewidth.RuneWidth()` for visual column tracking

**Remaining byte-length issues found and fixed**: See Bugs 1-3, 5 above.

**`truncateVisual()`**: Well-implemented. Correctly iterates runes, skips ANSI escape sequences, and uses `runewidth.RuneWidth()` for column counting. Handles edge case of `maxCols <= 3`.

## Test Quality

### `update_feedback_test.go`
- Tests all 10 feedback paths covering no-worktree, no-repo, no-session, no-idle, no-plan, tmux-mode variants
- Uses `setupModel` helper with proper cleanup
- Assertions check both toast existence and message content
- **Good**: Uses `t.Cleanup` for manager lifecycle

### `update_scroll_test.go`
- Tests session switch round-trip (save/restore scroll)
- Tests worktree switch (clears session and scroll)
- Tests new session starts at scroll offset 0
- **Good**: Tests the full dropdown interaction flow, not just the helper function

### `width_test.go`
- Tests emoji width, CJK width, `truncateVisual` with ANSI and emoji
- Tests `padToSize` with wide characters
- Tests top bar padding calculation
- **Coverage gap**: No test for `truncate()` with multi-byte input, `truncatePath()` with non-ASCII, or `generateDropdownTitle()` with wide characters. The bugs found above would have been caught by such tests.

## Build and Test Results

```
bazel build //bramble/app:app     -- PASSED
bazel test //bramble/app:app_test -- PASSED (0.0s, all tests pass)
```

## Summary

The Cycle 2 implementation is solid overall. The action feedback coverage is comprehensive, the scroll memory mechanism is correct, and most width calculations were properly converted. The review found and fixed 5 bugs:
- 1 critical (ANSI corruption in file tree truncation)
- 2 moderate (byte-length truncation in `truncate()` and `truncatePath()`)
- 1 minor (dropped toast command in `confirmTask`)
- 1 minor (byte-length word fitting in `generateDropdownTitle`)

All fixes maintain backward compatibility and pass existing tests.
