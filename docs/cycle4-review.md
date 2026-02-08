# Cycle 4 Code Review

**Reviewer:** Code Reviewer Agent
**Scope:** Session progress in dropdown, aggregate cost in status bar, file tree Enter-to-open
**Files reviewed:**
- `bramble/app/model.go` (formatSessionSubtitle, aggregateCost, updateSessionDropdown)
- `bramble/app/view.go` (renderStatusBar cost display)
- `bramble/app/update.go` (Enter key handler)
- `bramble/app/filetree.go` (AbsSelectedPath)
- `bramble/app/helpoverlay.go` (Enter binding in help)
- `bramble/app/session_subtitle_test.go`
- `bramble/app/aggregate_cost_test.go`
- `bramble/app/filetree_open_test.go`

## Summary

The Cycle 4 implementation is well-structured overall. The three features integrate cleanly into the existing TUI framework. Seven issues were identified and fixed -- one security concern (path traversal), one correctness bug (column width measurement), two robustness improvements, and three test quality improvements.

## Issues Found and Fixed

### 1. [Bug] `formatSessionSubtitle` used `len(prefix)` instead of `runewidth.StringWidth(prefix)` (model.go:358)

**Severity:** Low (latent bug, not triggered by current ASCII-only content)

The column budget calculation for how much space remains for the prompt excerpt used `len(prefix)` which counts bytes, not visual columns. The rest of the codebase consistently uses `runewidth.StringWidth` for visual column calculations. While the prefix is currently pure ASCII (so byte length equals column width), this would silently produce incorrect truncation if the prefix ever contained multi-byte characters (e.g., internationalized time labels).

**Fix:** Changed `len(prefix)` to `runewidth.StringWidth(prefix)`.

### 2. [Robustness] Zero `CreatedAt` produces nonsensical elapsed time (model.go:347)

**Severity:** Low

When `SessionInfo.CreatedAt` is the zero `time.Time` (which happens when a session hasn't been fully initialized), `time.Since(zeroTime)` produces a duration of ~56 years, and `timeAgo` would render something like `"489835d ago"`. The `IsZero()` check prevents this for the zero value itself, but any time older than a year is likely stale metadata rather than meaningful elapsed time.

**Fix:** Added a guard `time.Since(sess.CreatedAt) < 365*24*time.Hour` to skip elapsed time display when the creation timestamp is unreasonably old.

**Test added:** `TestFormatSessionSubtitle_ZeroCreatedAt` verifies no stale "d ago" timestamp appears.

### 3. [Security] `AbsSelectedPath` lacked path traversal protection (filetree.go:219-225)

**Severity:** Medium

`AbsSelectedPath` computed `filepath.Join(ft.root, rel)` where `rel` comes from `git diff --name-only HEAD` output. While git output is generally trusted, a malicious repository could contain filenames like `../../etc/passwd`. The `filepath.Join` function resolves `..` components, so the result could escape the worktree root. This path is then passed to `exec.Command(editor, filePath)` which would open an arbitrary file.

**Fix:** Added a containment check after `filepath.Clean` that verifies the resolved absolute path has the worktree root as a prefix. If the path escapes, returns empty string (which triggers the "No file selected" toast instead of opening the file).

**Test added:** `TestAbsSelectedPath_PathTraversal` verifies that `../../etc/passwd` returns empty string.

### 4. [Test Quality] Floating-point comparison using `!=` (aggregate_cost_test.go:27)

**Severity:** Low

`TestAggregateCost_MultipleSessions` compared float64 values with `!=`. While the specific values 0.01, 0.025, and 0.0 happen to sum to exactly 0.035 in IEEE 754, this pattern is fragile -- a future change to different cost values could cause spurious test failures due to floating-point imprecision.

**Fix:** Changed to tolerance-based comparison: `math.Abs(cost-expected) > 1e-9`.

### 5. [Test Quality] Custom `contains` helper reimplemented `strings.Contains` (aggregate_cost_test.go:58-72)

**Severity:** Low (code style)

The test helper `contains` and `findSubstring` were a manual reimplementation of `strings.Contains`. Using the standard library is more readable and idiomatic.

**Fix:** Replaced the two functions with a single-line wrapper: `strings.Contains(s, substr)`.

### 6. [Test Quality] `TestHandleKeyPress_EnterInSplitPane_NoFile` had silent pass path (filetree_open_test.go:124)

**Severity:** Low

The test checked `if m.fileTree.entries[0].IsDir` and only ran assertions inside the if-block. If the test setup changed such that the first entry was NOT a directory, the test would silently pass without validating anything. This is a classic "silent success" anti-pattern.

**Fix:** Changed the `if` guard to `if !entries[0].IsDir { t.Fatal(...) }` so the test fails loudly if the precondition is violated, then runs the assertions unconditionally.

## Items Reviewed Without Issues

### `aggregateCost` (model.go:199-205)
Correct. Simple summation over `m.sessions` slice. Edge cases handled: empty slice returns 0.0 (zero value). The `m.sessions` field is populated from `sessionManager.GetAllSessions()` on every session event, so it stays in sync.

### Status bar cost display (view.go:634-638)
Correct. Shows cost only when `totalCost > 0`, avoiding a distracting "$0.0000" when no sessions have incurred cost. Format uses `$%.4f` which provides 4 decimal places -- appropriate for API cost granularity.

### Enter key handler (update.go:317-363)
Well-structured. The handler correctly:
- Checks split pane is active AND left pane has focus before attempting file open
- Shows a toast when no file is selected (directory or empty tree)
- Uses `cmd.Start()` (not `cmd.Run()`) so the editor launches asynchronously
- Falls through to tmux mode handling when not in split pane mode

### Help overlay binding (helpoverlay.go:248-253)
Correctly adds the Enter binding only when `m.splitPane.IsSplit()` is true, keeping help context-sensitive.

### `updateSessionDropdown` (model.go:247-334)
Well-structured separation of live sessions (with progress) and history sessions (with dim badge). Correctly deduplicates using `liveIDs` map. The separator item with ID `"---separator---"` is correctly rejected in the dropdown Enter handler.

## Test Coverage Summary

| Feature | Tests | Verdict |
|---------|-------|---------|
| `formatSessionSubtitle` | 5 tests (pending, active, long prompt, zero time, zero cost) | Good |
| `aggregateCost` | 2 tests (empty, multiple sessions) | Good |
| Status bar cost display | 2 tests (zero omitted, nonzero shown) | Good |
| `AbsSelectedPath` | 5 tests (file, dir, empty, no root, traversal) | Good |
| Enter key handler | 3 tests (file, dir, not-in-split) | Good |
| Help bindings | 2 tests (split active, split inactive) | Good |

Total: 19 tests covering the three features.

## Build and Test Verification

```
bazel build //bramble/app:app          -- PASSED
bazel test //bramble/app:app_test      -- PASSED (all 19 cycle 4 tests green)
```
