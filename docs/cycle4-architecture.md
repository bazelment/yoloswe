# Cycle 4 Architecture -- Bramble TUI

## Implementation Order

1. **Feature 2: Aggregate Cost in Status Bar** -- smallest scope, zero cross-cutting concerns, provides immediate verification that cost data flows correctly.
2. **Feature 1: Session Progress in Dropdown** -- depends on the same `SessionInfo.Progress` data as Feature 2; building on proven data flow.
3. **Feature 3: File Tree Enter-to-Open** -- largest scope (touches update.go, filetree.go, helpoverlay.go); benefits from having Features 1-2 already merged so merge conflicts are minimal.

---

## Feature 2: Aggregate Cost in Status Bar

### Summary

Sum `Progress.TotalCostUSD` across all sessions in `m.sessions` and display `Cost: $X.XXXX` in the right side of the status bar, after the `Running: N  Idle: N` text. Omit when the total is zero.

### Code Changes

**File: `bramble/app/view.go` -- `renderStatusBar()` (line 587)**

After the `right` variable is assigned on line 632, insert cost computation:

```go
// --- CURRENT (lines 629-632) ---
counts := m.sessionManager.CountByStatus()
running := counts[session.StatusRunning]
idle := counts[session.StatusIdle]
right := fmt.Sprintf("Running: %d  Idle: %d", running, idle)

// --- NEW (insert between lines 632 and 634) ---
totalCost := m.aggregateCost()
if totalCost > 0 {
    right += fmt.Sprintf("  Cost: $%.4f", totalCost)
}
```

**File: `bramble/app/model.go` -- new helper method**

Add a new method on `Model` after the `selectedSession()` method (after line 196):

```go
// aggregateCost returns the sum of TotalCostUSD across all sessions.
func (m *Model) aggregateCost() float64 {
    var total float64
    for i := range m.sessions {
        total += m.sessions[i].Progress.TotalCostUSD
    }
    return total
}
```

### Function Signatures

```go
func (m *Model) aggregateCost() float64
```

### Width Handling

The cost string `"  Cost: $X.XXXX"` adds at most 20 characters. The existing padding calculation (line 640) already dynamically computes remaining space:

```go
padding := m.width - runewidth.StringWidth(stripAnsi(left)) - runewidth.StringWidth(stripAnsi(right)) - 2
```

If the terminal is too narrow, padding clamps to 1 and the left-side hints get pushed against the cost display. This matches existing behavior and is acceptable -- the PM spec says cost is first to be truncated if space is tight. No additional truncation logic is needed because `padding < 1` already collapses naturally.

### Test Strategy

**Unit test** in a new file `bramble/app/aggregate_cost_test.go`:

1. `TestAggregateCost_Empty` -- `m.sessions` is nil; returns 0.0.
2. `TestAggregateCost_MultipleSessions` -- three sessions with costs 0.0100, 0.0250, 0.0000; returns 0.0350.
3. `TestRenderStatusBar_CostOmittedWhenZero` -- call `renderStatusBar()` with zero-cost sessions; output must NOT contain `"Cost:"`.
4. `TestRenderStatusBar_CostShownWhenNonZero` -- call `renderStatusBar()` with non-zero cost; output must contain `"Cost: $0.0350"`.

For tests 3-4, construct a minimal `Model` with a mock `sessionManager` (the existing `CountByStatus()` needs to return something valid). The `renderStatusBar()` method is a pure view function once the model is populated, so it can be tested directly.

---

## Feature 1: Session Progress in Dropdown

### Summary

Enrich the session dropdown subtitle for live sessions to show turn count, cost, and elapsed time before the truncated prompt. Format: `T:{turns} ${cost} {elapsed} | {prompt_truncated}`. Omit the progress prefix when both turns and cost are zero (pending sessions).

### Code Changes

**File: `bramble/app/model.go` -- `updateSessionDropdown()` (lines 238-325)**

Replace the subtitle construction for live sessions (line 261):

```go
// --- CURRENT (line 261) ---
subtitle := truncate(sess.Prompt, 40)

// --- NEW (replace line 261) ---
subtitle := formatSessionSubtitle(sess)
```

**File: `bramble/app/model.go` -- new helper function**

Add after `updateSessionDropdown()` (after line 325):

```go
// formatSessionSubtitle builds a rich subtitle for a live session dropdown item.
// Shows progress (turns, cost, elapsed) when available, followed by prompt excerpt.
func formatSessionSubtitle(sess *session.SessionInfo) string {
    var parts []string

    // Progress prefix: only show when session has started doing work
    if sess.Progress.TurnCount > 0 || sess.Progress.TotalCostUSD > 0 {
        parts = append(parts, fmt.Sprintf("T:%d $%.4f", sess.Progress.TurnCount, sess.Progress.TotalCostUSD))
    }

    // Elapsed time since creation
    if !sess.CreatedAt.IsZero() {
        parts = append(parts, timeAgo(sess.CreatedAt))
    }

    // Build prefix
    prefix := ""
    if len(parts) > 0 {
        prefix = strings.Join(parts, " ") + " | "
    }

    // Remaining budget for prompt
    maxPromptLen := 40 - len(prefix)
    if maxPromptLen < 10 {
        maxPromptLen = 10
    }

    return prefix + truncate(sess.Prompt, maxPromptLen)
}
```

### Function Signatures

```go
func formatSessionSubtitle(sess *session.SessionInfo) string
```

### Dropdown Width

The PM spec suggests the session dropdown width may need to increase. Currently set in `update.go` line 48:

```go
m.sessionDropdown.SetWidth(m.width / 2)
```

The enriched subtitle content fits within 40 characters (the existing truncation limit). The current `m.width / 2` on an 80-column terminal yields 40 columns, and dropdown rendering adds padding. No width change is needed -- the subtitle already truncates to 40 characters. If wider terminals are common, this can be revisited, but no change is required for correctness.

### Test Strategy

**Unit test** in a new file `bramble/app/session_subtitle_test.go`:

1. `TestFormatSessionSubtitle_PendingSession` -- TurnCount=0, TotalCostUSD=0, CreatedAt is set; subtitle should contain elapsed time and prompt but NOT `"T:0"`.
2. `TestFormatSessionSubtitle_ActiveSession` -- TurnCount=5, TotalCostUSD=0.0312, CreatedAt 3 minutes ago; subtitle should match `"T:5 $0.0312 3m ago | Fix auth bug..."`.
3. `TestFormatSessionSubtitle_LongPrompt` -- prompt is 80 chars; subtitle truncates prompt to fit within budget.
4. `TestFormatSessionSubtitle_ZeroCostZeroTurns` -- both zero, CreatedAt set; no `T:` or `$` prefix, just elapsed + prompt.
5. `TestUpdateSessionDropdown_LiveSessionSubtitle` -- integration-level: populate model with live sessions and call `updateSessionDropdown()`, then verify dropdown items have enriched subtitles.

Tests 1-4 test `formatSessionSubtitle()` directly. Test 5 requires a mock session manager.

---

## Feature 3: File Tree Enter-to-Open

### Summary

When the split pane is active and the left (file tree) pane has focus, pressing Enter on a file entry opens that file in the configured editor. Pressing Enter on a directory header or "(no changes)" shows a toast. The help overlay is updated to show the new binding.

### Code Changes

#### 3a. Add `AbsSelectedPath()` to FileTree

**File: `bramble/app/filetree.go` -- after `SelectedPath()` (after line 215)**

```go
// AbsSelectedPath returns the absolute path of the currently selected file,
// or empty string if the selection is a directory or nothing is selected.
func (ft *FileTree) AbsSelectedPath() string {
    rel := ft.SelectedPath()
    if rel == "" || ft.root == "" {
        return ""
    }
    return filepath.Join(ft.root, rel)
}
```

This keeps the path construction inside `FileTree` where it belongs, rather than leaking `root` to callers. `SelectedPath()` returns relative paths from git (e.g., `"auth.go"`, `"pkg/config.go"`), and `root` is the worktree absolute path (e.g., `"/home/user/worktrees/repo/my-branch"`).

#### 3b. Wire Enter key in handleKeyPress

**File: `bramble/app/update.go` -- `handleKeyPress()`, `case "enter":` block (lines 315-346)**

The existing `case "enter":` handler (line 315) only handles tmux mode. We need to add a branch for split-pane file tree focus. Insert the new branch BEFORE the tmux check so it takes priority when the split pane is focused:

```go
case "enter":
    // Split pane: open selected file in editor
    if m.splitPane.IsSplit() && m.splitPane.FocusLeft() {
        filePath := m.fileTree.AbsSelectedPath()
        if filePath == "" {
            toastCmd := m.addToast("No file selected", ToastInfo)
            return m, toastCmd
        }
        fileName := filepath.Base(filePath)
        editor := m.editor
        toastCmd := m.addToast("Opening "+fileName+" in editor", ToastSuccess)
        return m, tea.Batch(toastCmd, func() tea.Msg {
            cmd := exec.Command(editor, filePath)
            err := cmd.Start()
            return editorResultMsg{err: err}
        })
    }
    // In tmux mode, Enter switches to the selected window
    if m.sessionManager.IsInTmuxMode() {
        // ... existing tmux handling unchanged ...
    }
    return m, nil
```

The complete replacement for lines 315-346:

```go
case "enter":
    // Split pane: open selected file in editor
    if m.splitPane.IsSplit() && m.splitPane.FocusLeft() {
        filePath := m.fileTree.AbsSelectedPath()
        if filePath == "" {
            toastCmd := m.addToast("No file selected", ToastInfo)
            return m, toastCmd
        }
        fileName := filepath.Base(filePath)
        editor := m.editor
        toastCmd := m.addToast("Opening "+fileName+" in editor", ToastSuccess)
        return m, tea.Batch(toastCmd, func() tea.Msg {
            cmd := exec.Command(editor, filePath)
            err := cmd.Start()
            return editorResultMsg{err: err}
        })
    }
    // In tmux mode, Enter switches to the selected window
    if m.sessionManager.IsInTmuxMode() {
        // Get the currently selected session
        var currentSessions []session.SessionInfo
        if wt := m.selectedWorktree(); wt != nil {
            allSessions := m.sessionManager.GetAllSessions()
            for i := range allSessions {
                if allSessions[i].WorktreePath == wt.Path {
                    currentSessions = append(currentSessions, allSessions[i])
                }
            }
        }

        if m.selectedSessionIndex >= 0 && m.selectedSessionIndex < len(currentSessions) {
            sess := currentSessions[m.selectedSessionIndex]
            if sess.TmuxWindowName != "" {
                return m, func() tea.Msg {
                    cmd := exec.Command("tmux", "select-window", "-t", sess.TmuxWindowName)
                    if err := cmd.Run(); err != nil {
                        return errMsg{fmt.Errorf("failed to switch to tmux window: %w", err)}
                    }
                    return nil
                }
            }
        } else {
            toastCmd := m.addToast("No sessions to switch to", ToastInfo)
            return m, toastCmd
        }
    }
    return m, nil
```

Note: `"path/filepath"` is already imported in `update.go`? No -- check the imports. The file imports `"strings"`, `"fmt"`, `"bytes"`, `"os/exec"`, `"time"`, and the Bubble Tea / session / wt / taskrouter packages. `"path/filepath"` is NOT imported. It must be added.

**File: `bramble/app/update.go` -- import block (lines 3-15)**

Add `"path/filepath"` to the import block:

```go
import (
    "bytes"
    "fmt"
    "os/exec"
    "path/filepath"
    "strings"
    "time"

    tea "github.com/charmbracelet/bubbletea"

    "github.com/bazelment/yoloswe/bramble/session"
    "github.com/bazelment/yoloswe/wt"
    "github.com/bazelment/yoloswe/wt/taskrouter"
)
```

#### 3c. Update Help Overlay

**File: `bramble/app/helpoverlay.go` -- `buildHelpSections()`, Output section (lines 239-250)**

Add "Enter: Open file in editor" to the Output section when the split pane is active. Insert after the existing scroll bindings (after line 248, before `sections = append(sections, out)`):

```go
// --- CURRENT (lines 239-250) ---
} else {
    out := HelpSection{Title: "Output"}
    out.Bindings = append(out.Bindings,
        HelpBinding{"Up/k", "Scroll up"},
        HelpBinding{"Down/j", "Scroll down"},
        HelpBinding{"PgUp", "Scroll up 10 lines"},
        HelpBinding{"PgDn", "Scroll down 10 lines"},
        HelpBinding{"Home", "Scroll to top"},
        HelpBinding{"End", "Scroll to bottom"},
    )
    sections = append(sections, out)
}

// --- NEW (replace lines 239-250) ---
} else {
    out := HelpSection{Title: "Output"}
    out.Bindings = append(out.Bindings,
        HelpBinding{"Up/k", "Scroll up"},
        HelpBinding{"Down/j", "Scroll down"},
        HelpBinding{"PgUp", "Scroll up 10 lines"},
        HelpBinding{"PgDn", "Scroll down 10 lines"},
        HelpBinding{"Home", "Scroll to top"},
        HelpBinding{"End", "Scroll to bottom"},
    )
    if m.splitPane.IsSplit() {
        out.Bindings = append(out.Bindings,
            HelpBinding{"Enter", "Open file in editor (file tree)"},
        )
    }
    sections = append(sections, out)
}
```

### Function Signatures

```go
func (ft *FileTree) AbsSelectedPath() string
```

No new exported functions in update.go or helpoverlay.go -- just modifications to existing `handleKeyPress()` and `buildHelpSections()`.

### Test Strategy

**Unit tests** in a new file `bramble/app/filetree_open_test.go`:

1. `TestAbsSelectedPath_FileSelected` -- create FileTree with root `/tmp/wt` and files `["auth.go", "pkg/config.go"]`; navigate cursor to `auth.go`; `AbsSelectedPath()` returns `"/tmp/wt/auth.go"`.
2. `TestAbsSelectedPath_DirectorySelected` -- cursor on a directory header entry; `AbsSelectedPath()` returns `""`.
3. `TestAbsSelectedPath_EmptyTree` -- no files; `AbsSelectedPath()` returns `""`.
4. `TestAbsSelectedPath_NoRoot` -- root is empty string; `AbsSelectedPath()` returns `""`.

**Integration-level tests** (in `bramble/app/filetree_open_test.go`):

5. `TestHandleKeyPress_EnterInSplitPane` -- construct a Model with split pane active, focus left, file tree with a file selected. Send Enter key. Verify the returned `tea.Cmd` is non-nil (we cannot easily assert the exec.Command without running it, but we can verify a command is returned and that a toast was added).
6. `TestHandleKeyPress_EnterInSplitPane_NoFile` -- same as above but cursor on directory header. Verify toast message is "No file selected".
7. `TestHandleKeyPress_EnterNotInSplitPane` -- split pane is not active, not in tmux mode. Send Enter key. Verify no command returned (passthrough).
8. `TestBuildHelpSections_SplitPaneActive` -- split pane is active; verify Output section contains the "Enter" binding.
9. `TestBuildHelpSections_SplitPaneInactive` -- split pane is inactive; verify Output section does NOT contain the "Enter" binding.

---

## Cross-Cutting Concerns

### Data Flow Verification

All three features depend on `SessionInfo.Progress` being populated. The data flow is:

```
Session.Progress (mutex-protected)
  -> Session.ToInfo() clones to SessionProgressSnapshot
    -> SessionInfo.Progress (mutex-free snapshot)
      -> m.sessions (via sessionManager.GetAllSessions())
```

This flow is already exercised by the output header display (view.go lines 352-354) which shows `T:{turns} ${cost}`. The features reuse this existing flow; no new data plumbing is needed.

### Existing Test Coverage

The `session/types.go` types have no unit tests for `Clone()` or `ToInfo()`. These are well-tested implicitly through the output rendering. Adding explicit tests is out of scope for Cycle 4 but worth noting for a future hardening pass.

### Import Changes Summary

| File | Added Import |
|------|-------------|
| `bramble/app/update.go` | `"path/filepath"` |
| `bramble/app/filetree.go` | (already imports `"path/filepath"`) |
| `bramble/app/model.go` | (no new imports) |
| `bramble/app/view.go` | (no new imports) |
| `bramble/app/helpoverlay.go` | (no new imports) |

### Files Modified Summary

| File | Feature(s) | Nature of Change |
|------|-----------|-----------------|
| `bramble/app/model.go` | 1, 2 | Add `aggregateCost()` method; add `formatSessionSubtitle()` helper; modify `updateSessionDropdown()` to use it |
| `bramble/app/view.go` | 2 | Modify `renderStatusBar()` to show aggregate cost |
| `bramble/app/update.go` | 3 | Add Enter handler for split pane file tree; add `"path/filepath"` import |
| `bramble/app/filetree.go` | 3 | Add `AbsSelectedPath()` method |
| `bramble/app/helpoverlay.go` | 3 | Add Enter binding to Output section when split pane is active |

### New Test Files

| File | Feature(s) | Tests |
|------|-----------|-------|
| `bramble/app/aggregate_cost_test.go` | 2 | 4 tests for cost aggregation and status bar rendering |
| `bramble/app/session_subtitle_test.go` | 1 | 5 tests for subtitle formatting and dropdown integration |
| `bramble/app/filetree_open_test.go` | 3 | 9 tests for path resolution, key handling, and help overlay |
