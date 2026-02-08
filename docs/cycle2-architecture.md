# Bramble TUI - Cycle 2 Architecture Document

## Overview

This document specifies the exact code changes for three Cycle 2 features:
1. **Action Feedback for All Unavailable Keys** -- Wire toasts to silent failures
2. **Per-Session Scroll Position Memory** -- Save/restore scroll on session switch
3. **Fix Width Calculations** -- Replace `len(stripAnsi(s))` with `runewidth.StringWidth(stripAnsi(s))`

All line numbers reference the current codebase state.

---

## Feature 1: Action Feedback for All Unavailable Keys

### Summary

Replace every silent `return m, nil` in `handleKeyPress()` (and related handlers) with
`m.addToast(message, ToastInfo)` so the user always gets visual feedback when a precondition
is not met.

### File: `bramble/app/update.go`

#### Change 1a: `"n"` key -- no repo (line 369)

Current code (line 369):
```go
	return m, nil
```

Replace with:
```go
	toastCmd := m.addToast("No repository loaded", ToastError)
	return m, toastCmd
```

#### Change 1b: `"p"` key -- no worktree (line 387)

Current code (line 387):
```go
	return m, nil
```

Replace with:
```go
	toastCmd := m.addToast("Select a worktree first (Alt-W)", ToastInfo)
	return m, toastCmd
```

#### Change 1c: `"b"` key -- no worktree (line 398)

Current code (line 398):
```go
	return m, nil
```

Replace with:
```go
	toastCmd := m.addToast("Select a worktree first (Alt-W)", ToastInfo)
	return m, toastCmd
```

#### Change 1d: `"e"` key -- no worktree (line 409)

Current code (line 409):
```go
	return m, nil
```

Replace with:
```go
	toastCmd := m.addToast("Select a worktree first (Alt-W)", ToastInfo)
	return m, toastCmd
```

#### Change 1e: `"s"` key -- no session selected (line 433)

Current code (lines 432-433):
```go
	}
	return m, nil
```

Add toast before the final return:
```go
	}
	toastCmd := m.addToast("No active session to stop (Alt-S to select)", ToastInfo)
	return m, toastCmd
```

#### Change 1f: `"f"` key -- no session or not idle (line 451)

Current code (line 451):
```go
	return m, nil
```

Replace with:
```go
	toastCmd := m.addToast("No idle session for follow-up", ToastInfo)
	return m, toastCmd
```

#### Change 1g: `"a"` key -- no idle planner with plan (line 475)

Current code (line 475):
```go
	return m, nil
```

Replace with:
```go
	toastCmd := m.addToast("No plan ready to approve", ToastInfo)
	return m, toastCmd
```

#### Change 1h: `"d"` key -- no worktree (line 496)

Current code (line 496):
```go
	return m, nil
```

Replace with:
```go
	toastCmd := m.addToast("Select a worktree first (Alt-W)", ToastInfo)
	return m, toastCmd
```

#### Change 1i: `"enter"` in tmux mode -- no sessions (lines 327-342)

Current code (lines 327-342): when `m.selectedSessionIndex` is out of range or no
`currentSessions`, the code falls through to `return m, nil` at line 342.

After the `if m.selectedSessionIndex >= 0 && m.selectedSessionIndex < len(currentSessions)` block,
add an else clause:
```go
		} else {
			toastCmd := m.addToast("No sessions to switch to", ToastInfo)
			return m, toastCmd
		}
```

Note: The inner check `if sess.TmuxWindowName != ""` at line 330 also has a comment
"No toast for missing tmux window name". This is fine to leave as-is since it is a
rare internal edge case. However, the outer condition where `currentSessions` is empty
should now produce a toast.

#### Change 1j: `"alt+s"` in tmux mode (line 266)

Current code (line 266):
```go
		return m, nil
```

Replace with:
```go
		toastCmd := m.addToast("Sessions are in tmux windows; use prefix+w to list", ToastInfo)
		return m, toastCmd
```

### New Function Signatures

No new functions are introduced. All changes call the existing `m.addToast(message string, level ToastLevel) tea.Cmd` method.

### Test Strategy

Create a new test file `bramble/app/update_feedback_test.go` with the following structure.
This follows the existing test pattern from `toast_test.go` and `welcome_test.go`:

```go
package app

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)
```

**Test cases** (each follows the same pattern -- create a Model with specific preconditions
not met, send the key via `handleKeyPress`, and assert a toast was added):

| Test Function | Key | Setup | Expected Toast Substring |
|---|---|---|---|
| `TestKeyFeedback_P_NoWorktree` | `p` | No worktrees | `"Select a worktree first"` |
| `TestKeyFeedback_B_NoWorktree` | `b` | No worktrees | `"Select a worktree first"` |
| `TestKeyFeedback_E_NoWorktree` | `e` | No worktrees | `"Select a worktree first"` |
| `TestKeyFeedback_D_NoWorktree` | `d` | No worktrees | `"Select a worktree first"` |
| `TestKeyFeedback_N_NoRepo` | `n` | `repoName=""` | `"No repository loaded"` |
| `TestKeyFeedback_S_NoSession` | `s` | No session selected | `"No active session to stop"` |
| `TestKeyFeedback_F_NoIdleSession` | `f` | No session or non-idle | `"No idle session"` |
| `TestKeyFeedback_A_NoPlan` | `a` | No idle planner with plan | `"No plan ready"` |
| `TestKeyFeedback_Enter_TmuxNoSessions` | `enter` | Tmux mode, no sessions | `"No sessions to switch to"` |
| `TestKeyFeedback_AltS_TmuxMode` | `alt+s` | Tmux mode | `"Sessions are in tmux windows"` |

**Test helper pattern:**
```go
func setupModel(t *testing.T, mode session.SessionMode, worktrees []wt.Worktree, repoName string) Model {
	t.Helper()
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: mode})
	t.Cleanup(func() { mgr.Close() })
	m := NewModel(ctx, "/tmp/wt", repoName, "", mgr, worktrees, 80, 24)
	return m
}
```

Each test:
1. Creates a Model using `setupModel` with appropriate conditions
2. Calls `m.handleKeyPress(tea.KeyMsg{...})` directly
3. Casts the returned `tea.Model` to `Model`
4. Asserts `m2.toasts.HasToasts() == true`
5. Asserts `m2.toasts.toasts[0].Message` contains the expected substring

---

## Feature 2: Per-Session Scroll Position Memory

### Summary

Save the current scroll offset when switching away from a session, and restore it when
switching back. This prevents the user from losing their reading position.

### File: `bramble/app/model.go`

#### Change 2a: Add field to Model struct (after line 57)

Add a new field to the `Model` struct:
```go
scrollPositions   map[session.SessionID]int
```

Insert between `scrollOffset` (line 57) and `selectedSessionIndex` (line 58).

#### Change 2b: Initialize map in NewModel (line 76)

Inside the `Model` literal in `NewModel()`, add:
```go
scrollPositions: make(map[session.SessionID]int),
```

### File: `bramble/app/update.go`

We introduce a helper method to encapsulate the save/switch/restore pattern. This avoids
duplicating the 4-line save/restore block at each call site.

#### Change 2c: Add helper method (new, after `scrollToBottom` at line 530)

```go
// switchViewingSession saves the scroll position for the current session,
// sets the viewing session to newID, and restores the saved scroll position
// (or 0 if none was saved).
func (m *Model) switchViewingSession(newID session.SessionID) {
	if m.viewingSessionID != "" {
		m.scrollPositions[m.viewingSessionID] = m.scrollOffset
	}
	m.viewingSessionID = newID
	m.scrollOffset = m.scrollPositions[newID] // zero-value (0) if not found
	m.viewingHistoryData = nil
}
```

#### Change 2d: Worktree dropdown Enter (lines 572-575)

Current code:
```go
		// Clear viewing session when switching worktrees
		m.viewingSessionID = ""
		m.viewingHistoryData = nil
		m.scrollOffset = 0
```

Replace with:
```go
		// Save scroll position and clear viewing session when switching worktrees
		m.switchViewingSession("")
```

Note: When switching worktrees, we save the old session's scroll position and set the
new session to `""` (no session). The restored offset for `""` will be 0, which is correct.

#### Change 2e: Session dropdown Enter (lines 586-587)

Current code:
```go
		m.viewingSessionID = session.SessionID(item.ID)
		m.scrollOffset = 0 // Reset scroll when switching sessions
```

Replace with:
```go
		m.switchViewingSession(session.SessionID(item.ID))
```

Then update the history loading block below (lines 588-604). Currently, `m.viewingHistoryData`
is set to `nil` on line 591, but `switchViewingSession` already does that. The history loading
code needs to re-set `viewingHistoryData` after `switchViewingSession`:

```go
		m.switchViewingSession(session.SessionID(item.ID))
		// Check if this is a live session or history
		if _, ok := m.sessionManager.GetSessionInfo(m.viewingSessionID); ok {
			// Live session -- viewingHistoryData already nil from switchViewingSession
		} else {
			// History session - load from store
			wt := m.selectedWorktree()
			if wt != nil {
				histData, err := m.sessionManager.LoadSessionFromHistory(wt.Branch, m.viewingSessionID)
				if err == nil {
					m.viewingHistoryData = histData
				}
			}
		}
```

#### Change 2f: `startSession()` (line 755)

Current code:
```go
	m.viewingSessionID = sessionID
```

Add a save before the assignment:
```go
	if m.viewingSessionID != "" {
		m.scrollPositions[m.viewingSessionID] = m.scrollOffset
	}
	m.viewingSessionID = sessionID
	m.scrollOffset = 0 // New session starts at bottom
```

Note: We do NOT use `switchViewingSession` here because a new session should always start
at scroll offset 0 (bottom), not restore a previously-saved position. The key difference
is that we still want to save the *outgoing* session's position.

#### Change 2g: `deleteWorktree()` (lines 807-810)

Current code:
```go
	if w := m.selectedWorktree(); w != nil && w.Branch == branch {
		m.viewingSessionID = ""
		m.viewingHistoryData = nil
		m.scrollOffset = 0
	}
```

Replace with:
```go
	if w := m.selectedWorktree(); w != nil && w.Branch == branch {
		// Save scroll position before clearing (session being deleted,
		// so the saved position will be stale, but that's fine -- it's
		// a no-op to save for a soon-to-be-deleted session).
		m.switchViewingSession("")
	}
```

#### Change 2h: `approve plan` handler, `"a"` key (line 470)

Current code:
```go
		m.viewingSessionID = sessionID
```

Add scroll save before switching:
```go
		if m.viewingSessionID != "" {
			m.scrollPositions[m.viewingSessionID] = m.scrollOffset
		}
		m.viewingSessionID = sessionID
		m.scrollOffset = 0 // New builder session starts at bottom
```

### New Function Signatures

```go
// switchViewingSession saves the scroll position for the current session,
// sets the viewing session to newID, and restores the saved scroll position
// (or 0 if none was saved).
func (m *Model) switchViewingSession(newID session.SessionID)
```

### Test Strategy

Create test cases in `bramble/app/update_scroll_test.go`:

**Test 1: `TestScrollPositionPreservedOnSessionSwitch`**
1. Create a Model with a session manager in TUI mode
2. Create two sessions (A and B) with output lines
3. Set `m.viewingSessionID = A`, `m.scrollOffset = 10`
4. Simulate session dropdown Enter selecting session B (via `handleDropdownMode`)
5. Assert `m.scrollOffset` is 0 (or restored for B)
6. Assert `m.scrollPositions[A] == 10`
7. Simulate session dropdown Enter selecting session A again
8. Assert `m.scrollOffset == 10` (restored)

**Test 2: `TestScrollPositionClearedOnWorktreeSwitch`**
1. Create a Model with session A on worktree 1, scrollOffset=15
2. Simulate worktree dropdown Enter selecting worktree 2
3. Assert `m.scrollPositions[A] == 15` (saved)
4. Assert `m.scrollOffset == 0` (cleared for new worktree)
5. Assert `m.viewingSessionID == ""` (no session on new worktree)

**Test 3: `TestNewSessionStartsAtBottom`**
1. Create a Model viewing session A at scrollOffset=20
2. Call `m.startSession(SessionTypePlanner, "prompt")`
3. Assert `m.scrollPositions[A] == 20` (old session saved)
4. Assert `m.scrollOffset == 0` (new session at bottom)

---

## Feature 3: Fix Width Calculations

### Summary

Replace all occurrences of `len(stripAnsi(s))` with `runewidth.StringWidth(stripAnsi(s))`
across the codebase. Additionally, fix byte-based truncation in `splitpane.go` and `toast.go`
by reusing the existing `truncateVisual()` function from `dropdown.go`.

### Prerequisite: Move `truncateVisual` to a shared location

`truncateVisual` is currently defined in `dropdown.go` (lines 265-295). Since it will be
used by `splitpane.go`, `toast.go`, `view.go`, and `output.go`, it should remain in
`dropdown.go` (which is in the same package `app`) -- no move required. All files in
`package app` can call it directly.

### Prerequisite: Add `runewidth` import

The following files need `"github.com/mattn/go-runewidth"` added to their import blocks:
- `bramble/app/welcome.go`
- `bramble/app/textarea.go`
- `bramble/app/splitpane.go`
- `bramble/app/output.go`
- `bramble/app/toast.go`

Files that already import it (no change needed):
- `bramble/app/view.go`
- `bramble/app/dropdown.go`

Note: `filetree.go` line 264 also has `len(stripped) > width` with byte-based truncation
at line 265 (`line[:width-3]`). This is a related bug but is NOT in the PM's specified
scope. We note it here for Cycle 3.

### File: `bramble/app/view.go`

#### Change 3a: `renderTopBar()` padding calculation (line 207)

Current:
```go
padding := m.width - len(stripAnsi(left)) - len(stripAnsi(right)) - 4
```

Replace with:
```go
padding := m.width - runewidth.StringWidth(stripAnsi(left)) - runewidth.StringWidth(stripAnsi(right)) - 4
```

#### Change 3b: `formatOutputLine()` width truncation (line 534)

Current:
```go
if line.Type != session.OutputTypeText && line.Type != session.OutputTypePlanReady && len(stripAnsi(formatted)) > width-2 {
	formatted = formatted[:width-5] + "..."
}
```

Replace with:
```go
if line.Type != session.OutputTypeText && line.Type != session.OutputTypePlanReady && runewidth.StringWidth(stripAnsi(formatted)) > width-2 {
	formatted = truncateVisual(formatted, width-2)
}
```

This fix has two parts:
1. Use `runewidth.StringWidth` for the width check
2. Use `truncateVisual` for the truncation itself (instead of byte-slicing `formatted[:width-5]`,
   which can split ANSI sequences and multi-byte runes)

#### Change 3c: `renderStatusBar()` padding calculation (line 699)

Current:
```go
padding := m.width - len(stripAnsi(left)) - len(stripAnsi(right)) - 2
```

Replace with:
```go
padding := m.width - runewidth.StringWidth(stripAnsi(left)) - runewidth.StringWidth(stripAnsi(right)) - 2
```

### File: `bramble/app/welcome.go`

#### Change 3d: `renderKeyHint()` key column visual width (line 104)

Add import: `"github.com/mattn/go-runewidth"`

Current:
```go
keyVisual := len(stripAnsi(keyCol))
```

Replace with:
```go
keyVisual := runewidth.StringWidth(stripAnsi(keyCol))
```

### File: `bramble/app/textarea.go`

#### Change 3e: `View()` button status padding (line 366)

Add import: `"github.com/mattn/go-runewidth"`

Current:
```go
statusPadding := contentWidth - len(stripAnsi(status))
```

Replace with:
```go
statusPadding := contentWidth - runewidth.StringWidth(stripAnsi(status))
```

### File: `bramble/app/splitpane.go`

#### Change 3f: `padToSize()` width comparison and padding (lines 113-117)

Add import: `"github.com/mattn/go-runewidth"`

Current:
```go
	for i, line := range lines {
		stripped := stripAnsi(line)
		if len(stripped) < width {
			lines[i] = line + strings.Repeat(" ", width-len(stripped))
		} else if len(stripped) > width {
			// Truncate (approximate - doesn't handle ANSI perfectly)
			lines[i] = line[:width]
		}
	}
```

Replace with:
```go
	for i, line := range lines {
		stripped := stripAnsi(line)
		visualWidth := runewidth.StringWidth(stripped)
		if visualWidth < width {
			lines[i] = line + strings.Repeat(" ", width-visualWidth)
		} else if visualWidth > width {
			lines[i] = truncateVisual(line, width)
		}
	}
```

This fixes three issues:
1. Width comparison now uses display columns instead of byte length
2. Padding calculation uses display columns
3. Truncation uses `truncateVisual` which correctly handles ANSI escapes and wide characters

### File: `bramble/app/output.go`

#### Change 3g: `formatOutputLine()` truncation check (line 200)

Add import: `"github.com/mattn/go-runewidth"`

Current:
```go
if line.Type != session.OutputTypeText && len(stripAnsi(formatted)) > width-2 {
	formatted = formatted[:width-5] + "..."
}
```

Replace with:
```go
if line.Type != session.OutputTypeText && runewidth.StringWidth(stripAnsi(formatted)) > width-2 {
	formatted = truncateVisual(formatted, width-2)
}
```

### File: `bramble/app/toast.go`

#### Change 3h: `View()` toast message truncation (line 141)

Add import: `"github.com/mattn/go-runewidth"`

Current:
```go
	content := icon + t.Message
	// Truncate to width (guard against small widths to avoid negative slice)
	if tm.width > 7 && len(content) > tm.width-4 {
		content = content[:tm.width-7] + "..."
	}
```

Replace with:
```go
	content := icon + t.Message
	// Truncate to width (guard against small widths to avoid negative slice)
	if tm.width > 7 && runewidth.StringWidth(content) > tm.width-4 {
		content = truncateVisual(content, tm.width-4)
	}
```

Note: The `truncateVisual` function already handles the `"..."` suffix internally
(it appends `"..."` after truncating to `maxCols-3`). So `tm.width-4` is the correct
parameter (leaves 4 columns of margin, and `truncateVisual` will use 3 of those for `"..."`).

### New Function Signatures

No new functions. The existing `truncateVisual(s string, maxCols int) string` from
`dropdown.go` is reused at new call sites.

### Test Strategy

Create `bramble/app/width_test.go`:

**Test 1: `TestStripAnsiWidth_Emoji`**
```go
func TestStripAnsiWidth_Emoji(t *testing.T) {
	// Emoji is 2 display columns wide
	s := "📋 planner"
	stripped := stripAnsi(s)
	assert.Equal(t, 11, runewidth.StringWidth(stripped)) // 2 (emoji) + 1 (space) + 7 (planner) + 1...
	assert.NotEqual(t, len(stripped), runewidth.StringWidth(stripped)) // byte len != display width
}
```

**Test 2: `TestStripAnsiWidth_CJK`**
```go
func TestStripAnsiWidth_CJK(t *testing.T) {
	s := "Hello \\u4e16\\u754c" // "Hello 世界"
	assert.Equal(t, 11, runewidth.StringWidth(s)) // 5 (Hello) + 1 (space) + 4 (2 CJK chars * 2 cols)
}
```

**Test 3: `TestTruncateVisual_Emoji`**
```go
func TestTruncateVisual_Emoji(t *testing.T) {
	s := "📋 planner session"
	result := truncateVisual(s, 10)
	// Should truncate to ~7 visual columns + "..."
	assert.LessOrEqual(t, runewidth.StringWidth(stripAnsi(result)), 10)
	assert.True(t, strings.HasSuffix(result, "..."))
}
```

**Test 4: `TestTruncateVisual_ANSI`**
```go
func TestTruncateVisual_ANSI(t *testing.T) {
	// ANSI-styled string: the escape codes should not count toward width
	s := "\\x1b[32mGreen text\\x1b[0m"
	result := truncateVisual(s, 8)
	// Visual width of result (excluding ANSI) should be <= 8
	assert.LessOrEqual(t, runewidth.StringWidth(stripAnsi(result)), 8)
}
```

**Test 5: `TestPadToSize_WideChars`**
Test `padToSize` with strings containing emoji to verify correct padding:
```go
func TestPadToSize_WideChars(t *testing.T) {
	content := "📋 plan"
	result := padToSize(content, 20, 1)
	lines := strings.Split(result, "\\n")
	// Each line should have visual width of exactly 20
	stripped := stripAnsi(lines[0])
	assert.Equal(t, 20, runewidth.StringWidth(stripped))
}
```

**Test 6: `TestTopBarPadding_Emoji`**
Integration test: create a Model with a session that has emoji icons and verify
`renderTopBar()` output has correct visual width:
```go
func TestTopBarPadding_Emoji(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()
	// ... setup model with session containing emoji icons
	topBar := m.renderTopBar()
	// Visual width of the top bar content should not exceed m.width
	stripped := stripAnsi(topBar)
	assert.LessOrEqual(t, runewidth.StringWidth(stripped), m.width+2) // +2 for lipgloss padding
}
```

---

## Implementation Order

The features should be implemented in this order to minimize conflicts and maximize
testability at each step:

### Phase 1: Feature 1 -- Action Feedback (0.5 day)

**Why first:** Pure additive changes -- only adds `addToast` calls at existing `return` sites.
Zero risk of breaking existing behavior. Tests can be written immediately since the toast
infrastructure is already proven.

**Steps:**
1. Edit `bramble/app/update.go` -- apply changes 1a through 1j
2. Create `bramble/app/update_feedback_test.go` with the 10 test cases
3. Run `bazel run //:gazelle` to update BUILD files for the new test file
4. Run `bazel test //bramble/app:app_test --test_timeout=60`

### Phase 2: Feature 2 -- Scroll Position Memory (0.25 day)

**Why second:** Requires a new field on `Model` and a new helper method, but no changes to
rendering logic. The Feature 1 changes (which touched the same `return m, nil` sites) are
already settled.

**Steps:**
1. Edit `bramble/app/model.go` -- apply changes 2a, 2b
2. Edit `bramble/app/update.go` -- add helper (2c), then apply changes 2d through 2h
3. Create `bramble/app/update_scroll_test.go` with the 3 test cases
4. Run `bazel run //:gazelle`
5. Run `bazel test //bramble/app:app_test --test_timeout=60`

### Phase 3: Feature 3 -- Width Calculations (0.5 day)

**Why third:** Mechanical find-and-replace across 6 files. Done last because:
- It touches rendering code which is the most sensitive to regressions
- The new test file needs `runewidth` imports in BUILD, so running gazelle once at the end
  is more efficient
- Features 1 and 2 do not depend on width correctness

**Steps:**
1. Edit `bramble/app/view.go` -- apply changes 3a, 3b, 3c
2. Edit `bramble/app/welcome.go` -- add import, apply change 3d
3. Edit `bramble/app/textarea.go` -- add import, apply change 3e
4. Edit `bramble/app/splitpane.go` -- add import, apply change 3f
5. Edit `bramble/app/output.go` -- add import, apply change 3g
6. Edit `bramble/app/toast.go` -- add import, apply change 3h
7. Create `bramble/app/width_test.go` with the 6 test cases
8. Run `bazel run //:gazelle` to pick up new imports
9. Run `bazel test //bramble/app:app_test --test_timeout=60`

### Phase 4: Integration Verification (0.25 day)

1. Run full build: `bazel build //...`
2. Run full test suite: `bazel test //... --test_timeout=120`
3. Manual spot-check: launch bramble with a repo that has emoji branch names or CJK
   characters and verify top bar alignment
4. Manual spot-check: press `p`, `b`, `e`, `d` with no worktree selected, verify toast appears
5. Manual spot-check: scroll up in session A, switch to B, switch back to A, verify scroll
   position restored

---

## Risk Assessment

| Feature | Risk | Mitigation |
|---------|------|------------|
| Action Feedback | Very Low -- additive only, no existing behavior changed | Each toast message is tested individually |
| Scroll Memory | Low -- new field + helper, 5 existing code paths modified | Helper method centralizes logic; 3 targeted tests |
| Width Calculations | Low -- mechanical replacement, but touches rendering | `truncateVisual` is already battle-tested in dropdown; new unit tests cover edge cases |

## Files Modified Summary

| File | Feature 1 | Feature 2 | Feature 3 |
|------|-----------|-----------|-----------|
| `bramble/app/update.go` | 10 changes | 6 changes | -- |
| `bramble/app/model.go` | -- | 2 changes | -- |
| `bramble/app/view.go` | -- | -- | 3 changes |
| `bramble/app/welcome.go` | -- | -- | 1 change + import |
| `bramble/app/textarea.go` | -- | -- | 1 change + import |
| `bramble/app/splitpane.go` | -- | -- | 1 change + import |
| `bramble/app/output.go` | -- | -- | 1 change + import |
| `bramble/app/toast.go` | -- | -- | 1 change + import |
| `bramble/app/update_feedback_test.go` | NEW | -- | -- |
| `bramble/app/update_scroll_test.go` | -- | NEW | -- |
| `bramble/app/width_test.go` | -- | -- | NEW |

Total: 8 existing files modified, 3 new test files created.
