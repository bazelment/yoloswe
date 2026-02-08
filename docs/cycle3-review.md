# Cycle 3 Code Review

**Reviewer:** Claude Opus 4.6
**Date:** 2026-02-07
**Scope:** Shared scroll rendering, TextArea HandleKey refactor, Dropdown search/filtering

## Files Reviewed

| File | Feature | Verdict |
|------|---------|---------|
| `bramble/app/view.go` | `renderScrollableLines()` | Bug fixed |
| `bramble/app/scrollrender_test.go` | Scroll rendering tests | Test strengthened |
| `bramble/app/update.go` | `handleInputMode`, `handleTaskModal`, `handleDropdownMode` | Clean |
| `bramble/app/textarea.go` | `HandleKey()` method | Clean |
| `bramble/app/textarea_test.go` | TextArea tests | Good coverage |
| `bramble/app/dropdown.go` | Filter/search functionality | Bug fixed |
| `bramble/app/dropdown_test.go` | Dropdown filter tests | Test added |

## Bugs Found and Fixed

### Bug 1: Phantom "0 more lines" indicator in scroll rendering (view.go)

**Severity:** Low (cosmetic)
**Location:** `renderScrollableLines()`, line ~430 (at-top branch)

**Problem:** When `scrollOffset > 0` but content fits entirely within the viewport (e.g., output was cleared while user was scrolled up), the function clamped `scrollOffset` to 0, rendered all lines, then unconditionally displayed a down-arrow indicator saying "0 more lines (press End to jump to latest)". This was misleading -- there were no hidden lines below.

**Root cause:** The at-top branch always rendered the down-arrow indicator without checking whether `hiddenBelow > 0`.

**Fix:** Added `if hiddenBelow > 0` guard before rendering the down-arrow indicator.

**Test:** Strengthened `TestRenderScrollableLines_ScrollClamped` to assert no down-arrow appears when all content fits in the viewport.

### Bug 2: ClearFilter loses selected item (dropdown.go)

**Severity:** Low (UX regression)
**Location:** `Dropdown.ClearFilter()`

**Problem:** When a filter was active, `selectedIdx` was relative to the filtered list (e.g., index 1 in the filtered list might correspond to original item index 3). When `ClearFilter()` was called (via Esc key or `Open()`), it set `filteredIndices = nil` but kept `selectedIdx` unchanged. The index that was valid in the filtered context (e.g., 1) was now incorrectly interpreted as an index into the full items list, selecting the wrong item.

**Example:** Filter reduces to [beta(orig 1), delta(orig 3)]. User navigates to delta (filtered index 1). Pressing Esc to clear filter preserves `selectedIdx=1`, which now points to "beta" in the full list instead of "delta".

**Root cause:** `ClearFilter` did not map the filtered index back to the original index before clearing the filter mapping.

**Fix:** Before clearing `filteredIndices`, map the current `selectedIdx` through `filteredIndices` to get the original index:
```go
if d.filteredIndices != nil && d.selectedIdx >= 0 && d.selectedIdx < len(d.filteredIndices) {
    d.selectedIdx = d.filteredIndices[d.selectedIdx]
}
```

**Test:** Added `TestDropdownFilter_ClearFilterPreservesSelection` which filters to 2 items, navigates to the second, clears the filter, and verifies the same item remains selected.

## Design Review

### F1: renderScrollableLines (view.go)

The extraction is well done. The function correctly handles three states: at-bottom (no indicators), at-top (down arrow only), and middle (both arrows). The `scrollOffset=0 means bottom` convention is preserved.

**Minor observation:** The function uses two separate `maxScroll` computations in the scrolled branch -- first with `contentHeight = outputHeight - 2`, then potentially re-computing with `contentHeight = outputHeight - 1` when startIdx reaches 0. This is necessary to reclaim the up-arrow line when at the top, but adds complexity. The logic is correct but could benefit from a clarifying comment about why the re-computation is needed.

**Edge cases verified:**
- Empty input (nil/empty lines) -- returns empty string
- outputHeight=1 -- renders 1 line (contentHeight clamped to 1)
- scrollOffset exceeding content -- clamped correctly
- Content smaller than viewport -- no spurious indicators (after fix)

### F2: TextArea HandleKey (textarea.go)

The `HandleKey` method cleanly centralizes key handling. The `TextAreaAction` return type is well-designed with five distinct values: `Handled`, `Submit`, `Cancel`, `Quit`, `Unhandled`.

**Behavior preservation verified:**
- `handleInputMode` (update.go:682) delegates to `HandleKey` and correctly maps actions to model-level operations (promptInputMsg, reset, tea.Quit)
- `handleTaskModal` (update.go:823) delegates to `HandleKey` for the `TaskModalInput` state and correctly maps actions
- Space handling (`"space"` key string) is correctly special-cased
- Focus-dependent behavior (keys ignored when not on TextInput) is correct
- The `Unhandled` action allows callers to layer additional key handling

**No behavior changes detected** from the refactoring.

### F3: Dropdown Filter (dropdown.go, update.go)

The type-to-filter implementation is clean:
- `filteredIndices` nil vs empty-slice distinguishes "no filter" from "filter with no matches"
- Case-insensitive matching via `strings.ToLower`
- Separators are excluded from filter results
- `effectiveItems()` / `effectiveLen()` provide a consistent abstraction over filtered/unfiltered state
- `SelectedItem()` correctly maps through `filteredIndices` to return the original item
- `MoveSelection` respects the effective list and skips separators

**Esc key behavior in handleDropdownMode:** Two-stage Esc (clear filter first, then close dropdown) is a good UX pattern. The pointer semantics (`dd := m.worktreeDropdown` copies the pointer, not the struct) are correct.

**Thread safety:** Not a concern. All dropdown mutations happen in the Bubbletea Update path, which is single-threaded.

## Test Coverage Assessment

### scrollrender_test.go
Good coverage of the key states: fits-without-scroll, at-bottom, scrolled-middle, scrolled-to-top, empty, clamped, height=1. The tests are data-driven and verify both presence and absence of indicators.

### textarea_test.go
Thorough coverage: basic operations, cursor movement (up/down/left/right), delete-forward, word wrap, multi-line, insert-string, line count, prompt, focus cycling, custom labels, view rendering, and all HandleKey actions. The `HandleKey_IgnoredWhenNotFocused` test is a good edge case.

### dropdown_test.go
Good filter-specific coverage: reduces list, case insensitive, backspace extends, empty shows all, correct ID from filtered list, open resets, separators excluded, no-matches message, filter indicator in view, navigation on filtered list, clear-filter preserves selection (added).

### Suggestions for additional tests (non-blocking)
1. **Scroll rendering with `outputHeight <= 0`** -- degenerate case worth documenting behavior
2. **TextArea HandleKey with `ctrl+enter`** -- explicit test for the submit shortcut
3. **Dropdown filter with unicode characters** -- verify case-insensitive matching works with non-ASCII

## Style Notes (Non-blocking)

1. `view.go:534`: The `truncateVisual` function is defined in `dropdown.go` but used from `view.go`. Consider moving it to a shared utility file for clarity.

2. `textarea.go:246-265`: The `wrapLine` method uses byte-length comparison (`len(line) <= width`) which is incorrect for multi-byte/wide characters. The visual width check should use `runewidth.StringWidth(line) <= width`. This is a pre-existing issue, not introduced in this cycle.

3. `update.go:661-666`: The type-to-filter rune extraction in `handleDropdownMode` has two paths: `len(keyStr) == 1` and `len(msg.Runes) == 1`. This is correct but could be simplified to just check `msg.Runes` first.

## Verification

```
$ bazel build //bramble/app:app        # PASSED
$ bazel test //bramble/app:app_test --test_timeout=60  # PASSED (all tests)
```
