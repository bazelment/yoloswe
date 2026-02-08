# Bramble TUI - Cycle 3 Product Manager Report

## 1. Cycle 1 + 2 Review

### Cycle 1: Discoverability Foundation
Delivered three features that addressed the "blank wall" problem for new and returning users:

- **Help Overlay (`?` key)** - Context-aware keybinding reference layered over the current screen. Scrollable, sections adapt to current focus area (dropdown mode shows dropdown bindings, input mode shows input bindings, etc.).
- **Toast Notification System** - Auto-dismissing success/info/error notifications that stack at the bottom. Maximum 3 visible, each with level-appropriate coloring and duration (3-5 seconds).
- **Welcome / Empty State** - Two variants: (a) no-worktrees onboarding with quick-start hints, (b) worktrees-exist variant showing current worktree summary, session counts, and relevant key hints.

**Code review fixes:** Panic on small terminals, scroll offset not updating, footer scrolled off screen, column misalignment in key hints, inline styles extracted to package-level vars.

### Cycle 2: Polish and Correctness
Closed gaps left by Cycle 1 and fixed width-calculation bugs that had been present since before the improvement cycles:

- **Action Feedback for All Keys** - Added toast notifications for all 10 previously-silent failure paths (e.g., pressing `s` with no session, pressing `e` with no worktree, pressing `Alt-S` in tmux mode).
- **Per-Session Scroll Position Memory** - `scrollPositions map[SessionID]int` saves and restores scroll offset when switching between sessions via the dropdown.
- **Width Calculation Fixes** - Replaced every `len(stripAnsi(s))` with `runewidth.StringWidth(stripAnsi(s))` across view.go, filetree.go, welcome.go, and dropdown.go. Prevents column misalignment with CJK characters and emojis.

**Code review fixes:** ANSI-corrupting truncation in filetree, byte-length bugs in `truncate()`, `truncatePath()`, and `generateDropdownTitle()`, lost toast command in `confirmTask()`.

---

## 2. Remaining Backlog (from original PM report)

| ID   | Item                                        | Priority | Complexity |
|------|---------------------------------------------|----------|------------|
| 3.6  | Keyboard Shortcut Cheat Sheet in Top Bar    | P2       | Low        |
| 3.7  | Session Status Transition Animations        | P2       | Medium     |
| 3.8  | Dropdown Search/Filtering                   | P2       | Medium     |
| 3.9  | Unified Submit Behavior (Enter/Shift+Enter) | P2       | Low        |
| 3.10 | Session Progress Summary in Dropdown        | P2       | Low        |

---

## 3. New Improvement Candidates Identified from Code Review

After reading the current codebase, I identified these additional improvement opportunities:

### N1. Duplicated Scroll Logic in renderOutputArea and renderHistorySession (P1, Medium)

`view.go` lines 320-461 and lines 542-643 contain nearly identical scroll-clamping and indicator-rendering logic (the "at bottom / scrolled to top / scrolled in middle" three-case pattern). This is a correctness risk: any future scroll fix must be applied in two places, and subtle divergence has already happened (outputHeight is `height - 5` in one and `height - 6` in the other, for different header sizes, but the scroll indicator logic is copy-pasted identically). Extracting a shared `renderScrollableContent(allVisualLines, outputHeight, scrollOffset)` helper eliminates this.

### N2. Duplicated Key Handling in handleInputMode and handleTaskModal (P1, Medium)

`update.go` lines 641-751 (`handleInputMode`) and lines 865-973 (`handleTaskModal` in `TaskModalInput` state) are almost character-for-character identical: both handle tab/shift-tab/ctrl+enter/enter/esc/backspace/delete/arrow keys/default rune insertion on a `*TextArea`. The only difference is the source of the `TextArea` pointer (`m.inputArea` vs `m.taskModal.TextArea()`) and the submit/cancel actions. This makes it easy to introduce divergent bugs. A shared method like `handleTextAreaKeys(ta *TextArea, msg tea.KeyMsg) (handled bool, action TextAreaAction)` would eliminate the duplication.

### N3. Scroll Offset Indicator in Status Bar is Inaccurate (P2, Low)

The status bar shows `"(%d lines above)", m.scrollOffset` but `scrollOffset` is a logical line count, while the actual scroll is clamped against visual lines in `renderOutputArea`. When a line wraps to multiple visual lines, the status bar number diverges from reality. This is a minor cosmetic issue but can confuse users who scroll through long markdown output.

### N4. No Visual Indicator for Currently Active Session in Session Dropdown (P2, Low)

When the session dropdown is open, there is no visual distinction between the item that is currently being viewed (`m.viewingSessionID`) and other items. Users must remember which session they were looking at. Adding a small marker (e.g., a dot or "viewing" badge) to the currently-viewed session in the dropdown would reduce cognitive load.

---

## 4. Cycle 3 Recommended Picks

After evaluating impact, risk, and effort, I recommend **3 items** for Cycle 3. The theme is **internal quality and dropdown usability** -- reducing maintenance debt from Cycles 1-2 while delivering a user-visible improvement.

---

### Pick 1: Extract Shared Scroll Rendering Helper (N1)

**Description**
Refactor the duplicated scroll-window logic in `renderOutputArea` and `renderHistorySession` into a single reusable function. The function takes a slice of visual lines, the available display height, and a scroll offset, and returns the rendered string (including scroll indicators). Both call sites become thin wrappers that prepare the header and call the shared function.

**User Story**
As a developer maintaining Bramble, I want the scroll rendering logic to exist in exactly one place, so that scroll-related bug fixes and improvements apply uniformly to both live sessions and history replay.

**Priority**: P1 (internal quality, prevents future scroll bugs)
**Complexity**: Low-Medium (pure refactor, no behavior change, testable in isolation)

**Files to Modify**:
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/view.go` -- extract shared function, simplify `renderOutputArea` and `renderHistorySession`
- (Optionally) a new test file or additions to existing tests to validate the extracted function independently

**Acceptance Criteria**:
1. A function like `renderScrollableLines(lines []string, height int, scrollOffset int) string` exists and is used by both `renderOutputArea` and `renderHistorySession`.
2. The scroll indicator logic ("N more lines", up/down arrows) is implemented exactly once.
3. Existing scroll-related tests continue to pass without modification (pure refactor).
4. The visual output is pixel-identical before and after the refactor.

---

### Pick 2: Extract Shared TextArea Key Handler (N2)

**Description**
Factor out the common text-area key-handling logic from `handleInputMode` and `handleTaskModal`'s `TaskModalInput` branch into a shared method. The method accepts a `*TextArea` and a `tea.KeyMsg`, and returns a structured result indicating what action to take (none, submit, cancel, quit, or "handled internally"). The two call sites then only need to handle the action-specific logic (what "submit" means in each context).

**User Story**
As a developer maintaining Bramble, I want text input key handling defined once, so that any fix to cursor movement, character insertion, or focus cycling applies to both the inline prompt input and the task modal input without duplication.

**Priority**: P1 (internal quality, prevents divergent input handling bugs)
**Complexity**: Low-Medium (pure refactor with well-defined interface boundary)

**Files to Modify**:
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/update.go` -- extract shared handler, simplify `handleInputMode` and `handleTaskModal`
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/textarea.go` -- optionally move the key handling into a method on TextArea itself (e.g., `TextArea.HandleKey(msg tea.KeyMsg) TextAreaAction`)

**Acceptance Criteria**:
1. The rune-insertion, cursor-movement, backspace/delete, tab/shift-tab, and ctrl+enter logic exists in exactly one place.
2. `handleInputMode` and `handleTaskModal` (TaskModalInput) delegate to the shared handler and only implement their own submit/cancel semantics.
3. All existing behavior is preserved (no user-visible change).
4. Unit tests verify the shared handler covers: character insertion, newline, backspace, cursor movement (all 4 directions), tab cycling, and ctrl+enter detection.

---

### Pick 3: Dropdown Search/Filtering (Backlog Item 3.8)

**Description**
Add type-to-filter capability to both the worktree and session dropdowns. When a dropdown is open, typing alphanumeric characters progressively filters the visible items. Backspace removes the last filter character. Esc (or clearing the filter entirely) restores the full list. A small "filter: xyz" indicator appears at the top of the dropdown when a filter is active.

This is the highest-impact remaining UX item because users with many worktrees (10+) or many sessions currently must arrow-key through the entire list. Competitive tools (lazygit, k9s, fzf) all provide instant filtering in their list views.

**User Story**
As a user with 15+ worktrees, I want to type a few characters while the worktree dropdown is open to instantly narrow the list to matching entries, so I can switch worktrees in under 2 seconds instead of pressing Down repeatedly.

**Priority**: P2 (user-facing, high usability impact for power users)
**Complexity**: Medium

**Files to Modify**:
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/dropdown.go` -- add `filterText string`, `filteredItems []DropdownItem`, filtering logic, filter display in `ViewList`
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/update.go` -- in `handleDropdownMode`, route alphanumeric keys and backspace to the dropdown's filter methods instead of ignoring them
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/model.go` -- no structural changes needed (Dropdown already referenced via pointer)

**Acceptance Criteria**:
1. When a dropdown is open, typing letters/digits progressively filters items. Matching is case-insensitive substring on the item's `Label` field.
2. Backspace removes the last filter character. If filter becomes empty, the full list is restored.
3. A "Filter: xyz" indicator is shown at the top of the dropdown when filter is non-empty.
4. Navigation (j/k/up/down/enter/esc) continues to work on the filtered list.
5. Selecting an item from a filtered list correctly selects the right item (by ID, not filtered index).
6. Opening a dropdown always starts with an empty filter.
7. Unit tests cover: filter reduces list, backspace extends list, empty filter shows all, selection from filtered list returns correct ID, filter is case-insensitive.

---

## 5. Summary Table

| #  | Item                               | Source   | Priority | Complexity  | Type          |
|----|------------------------------------|----------|----------|-------------|---------------|
| 1  | Extract Shared Scroll Helper       | New (N1) | P1       | Low-Medium  | Refactor      |
| 2  | Extract Shared TextArea Key Handler| New (N2) | P1       | Low-Medium  | Refactor      |
| 3  | Dropdown Search/Filtering          | 3.8      | P2       | Medium      | Feature       |

**Implementation order**: Picks 1 and 2 are independent refactors and can be done in parallel. Pick 3 depends on neither and can follow. The refactors (Picks 1-2) reduce code surface area, making Pick 3's integration into `handleDropdownMode` cleaner.

**Total estimated scope**: ~400-500 lines changed (refactored, not net-new), plus ~150 lines of new test code. Comparable to Cycle 2 in size.
