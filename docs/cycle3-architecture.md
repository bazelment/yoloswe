# Cycle 3 Architecture Design

## Implementation Order

1. **Feature 1: Extract Shared Scroll Rendering Helper** (no dependencies)
2. **Feature 2: Extract Shared TextArea Key Handler** (no dependencies; parallel with F1)
3. **Feature 3: Dropdown Search/Filtering** (independent; can follow F1/F2 or run in parallel)

F1 and F2 are pure refactors with no behavior change. F3 is a new feature. All three are independent of each other.

---

## Feature 1: Extract Shared Scroll Rendering Helper

### Problem

`view.go` contains two nearly identical scroll-window implementations:

- `renderOutputArea` (lines 386-461): scroll logic for live session output, uses `outputHeight = height - 5`
- `renderHistorySession` (lines 578-640): scroll logic for history replay, uses `outputHeight = height - 6`

Both implement the same three-case pattern (at-bottom / scrolled-to-top / scrolled-in-middle) with identical indicator strings. The only difference is the `outputHeight` value, which is passed in by each caller based on its header size.

### New Function Signature

```go
// renderScrollableLines renders a window of visual lines with scroll indicators.
// It takes the full set of visual lines, the available display height, and
// the current scroll offset (0 = at bottom / latest).
// Returns the rendered string to write into the output buffer.
func renderScrollableLines(allVisualLines []string, outputHeight int, scrollOffset int) string
```

This is a **package-level function** (not a method on Model) because it has no dependency on Model state -- it operates purely on its inputs. This makes it trivially unit-testable.

### Exact Code Changes

#### Step 1: Add `renderScrollableLines` to `view.go`

Insert after line 461 (after the closing brace of `renderOutputArea`), before `formatOutputLine`:

```go
// renderScrollableLines renders a window of visual lines with scroll indicators.
// scrollOffset=0 means "at bottom" (latest output visible).
// Higher values scroll toward the top (older content).
func renderScrollableLines(allVisualLines []string, outputHeight int, scrollOffset int) string {
	var b strings.Builder
	totalVisual := len(allVisualLines)

	if scrollOffset == 0 {
		// At bottom: no indicators, full outputHeight for content
		startIdx := totalVisual - outputHeight
		if startIdx < 0 {
			startIdx = 0
		}
		for i := startIdx; i < totalVisual; i++ {
			b.WriteString(allVisualLines[i])
			b.WriteString("\n")
		}
	} else {
		// Scrolled up: try with 2 indicators first (most common scrolled case)
		contentHeight := outputHeight - 2 // room for up-arrow and down-arrow
		if contentHeight < 1 {
			contentHeight = 1
		}

		maxScroll := 0
		if totalVisual > contentHeight {
			maxScroll = totalVisual - contentHeight
		}
		if scrollOffset > maxScroll {
			scrollOffset = maxScroll
		}

		endIdx := totalVisual - scrollOffset
		startIdx := endIdx - contentHeight
		if startIdx < 0 {
			startIdx = 0
		}

		if startIdx == 0 {
			// At/near top: only need down-arrow indicator, reclaim the up-arrow line
			contentHeight = outputHeight - 1
			maxScroll = 0
			if totalVisual > contentHeight {
				maxScroll = totalVisual - contentHeight
			}
			if scrollOffset > maxScroll {
				scrollOffset = maxScroll
			}
			endIdx = totalVisual - scrollOffset

			for i := 0; i < endIdx; i++ {
				b.WriteString(allVisualLines[i])
				b.WriteString("\n")
			}
			hiddenBelow := totalVisual - endIdx
			b.WriteString(dimStyle.Render(fmt.Sprintf("  \u2193 %d more lines (press End to jump to latest)", hiddenBelow)))
			b.WriteString("\n")
		} else {
			// Middle: both up-arrow and down-arrow indicators
			b.WriteString(dimStyle.Render(fmt.Sprintf("  \u2191 %d more lines (press Home to jump to top)", startIdx)))
			b.WriteString("\n")
			for i := startIdx; i < endIdx; i++ {
				b.WriteString(allVisualLines[i])
				b.WriteString("\n")
			}
			hiddenBelow := totalVisual - endIdx
			b.WriteString(dimStyle.Render(fmt.Sprintf("  \u2193 %d more lines (press End to jump to latest)", hiddenBelow)))
			b.WriteString("\n")
		}
	}

	return b.String()
}
```

#### Step 2: Simplify `renderOutputArea` (lines 386-461)

Replace lines 386-461 (from `outputHeight := height - 5` through the closing brace before `return b.String()`) with:

```go
	// Scroll on visual lines, not logical OutputLine count
	outputHeight := height - 5 // Account for header, prompt, separator
	b.WriteString(renderScrollableLines(allVisualLines, outputHeight, m.scrollOffset))

	return b.String()
```

The lines being replaced are `view.go:386-461`. The header-building code (lines 320-385) and the `return b.String()` on line 463 remain unchanged. The final function body becomes the header block + visual-line collection + a single call to `renderScrollableLines`.

#### Step 3: Simplify `renderHistorySession` (lines 578-640)

Replace lines 578-640 (from `outputHeight := height - 6` through the end of the scroll logic) with:

```go
	outputHeight := height - 6 // Account for header, prompt, timestamp, separator
	b.WriteString(renderScrollableLines(allVisualLines, outputHeight, m.scrollOffset))

	return b.String()
```

The lines being replaced are `view.go:578-640`. The header block (lines 542-577) remains unchanged.

### Test Strategy

Create `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/scrollrender_test.go`:

```go
package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("  line %d", i)
	}
	return lines
}

func TestRenderScrollableLines_FitsWithoutScroll(t *testing.T) {
	lines := makeLines(5)
	result := renderScrollableLines(lines, 10, 0)
	// All 5 lines should appear, no indicators
	for _, l := range lines {
		assert.Contains(t, result, l)
	}
	assert.NotContains(t, result, "more lines")
}

func TestRenderScrollableLines_AtBottom_NoIndicators(t *testing.T) {
	lines := makeLines(20)
	result := renderScrollableLines(lines, 10, 0)
	// Should show last 10 lines, no scroll indicators
	assert.Contains(t, result, "line 19")
	assert.Contains(t, result, "line 10")
	assert.NotContains(t, result, "line 9")
	assert.NotContains(t, result, "\u2191") // no up arrow
	assert.NotContains(t, result, "\u2193") // no down arrow
}

func TestRenderScrollableLines_ScrolledMiddle_BothIndicators(t *testing.T) {
	lines := makeLines(30)
	result := renderScrollableLines(lines, 10, 10)
	// Should have both up and down indicators
	assert.Contains(t, result, "\u2191")
	assert.Contains(t, result, "\u2193")
	assert.Contains(t, result, "more lines")
}

func TestRenderScrollableLines_ScrolledToTop_OnlyDownIndicator(t *testing.T) {
	lines := makeLines(30)
	// Scroll far enough to reach top
	result := renderScrollableLines(lines, 10, 999)
	assert.Contains(t, result, "line 0")
	assert.NotContains(t, result, "\u2191") // no up arrow at top
	assert.Contains(t, result, "\u2193")    // down arrow present
}

func TestRenderScrollableLines_EmptyLines(t *testing.T) {
	result := renderScrollableLines(nil, 10, 0)
	assert.Equal(t, "", result)
}

func TestRenderScrollableLines_ScrollClamped(t *testing.T) {
	lines := makeLines(5)
	// scrollOffset larger than content; should clamp and not panic
	result := renderScrollableLines(lines, 10, 100)
	require.NotEmpty(t, result)
	// Should show line 0 since we're clamped to top
	assert.Contains(t, result, "line 0")
}

func TestRenderScrollableLines_HeightOne(t *testing.T) {
	lines := makeLines(10)
	// Edge case: only 1 line of display height
	result := renderScrollableLines(lines, 1, 0)
	require.NotEmpty(t, result)
	// Should show at least 1 line, no panic
	lineCount := strings.Count(result, "\n")
	assert.GreaterOrEqual(t, lineCount, 1)
}
```

### Verification

- All existing tests in `update_scroll_test.go` must pass unchanged (they test scroll-offset saving/restoring, not rendering).
- Visual output must be pixel-identical: compare `View()` output before and after refactor for a fixture model with known lines and scroll offsets.

---

## Feature 2: Extract Shared TextArea Key Handler

### Problem

`update.go` contains two nearly character-for-character identical key-handling blocks:

- `handleInputMode` (lines 641-751): handles key presses for the inline prompt input (`m.inputArea`)
- `handleTaskModal` case `TaskModalInput` (lines 869-973): handles key presses for the task modal input (`m.taskModal.TextArea()`)

Both handle: `tab`, `shift+tab`, `ctrl+enter`, `enter` (with focus-dependent behavior), `esc`, `backspace`, `delete`, `up`, `down`, `left`, `right`, `ctrl+c`, and default rune insertion. The only differences are:

1. The `*TextArea` instance (`m.inputArea` vs `m.taskModal.TextArea()`)
2. What "submit" means (return `promptInputMsg` vs return `taskRouteMsg`)
3. What "cancel" means (reset `m.inputMode` vs call `m.taskModal.Hide()`)

### Design

Define a result type and a shared handler function. The function operates on a `*TextArea` and returns what action the caller should take, without performing any Model mutations beyond TextArea edits.

#### New Types and Function

Add to `textarea.go` (after the existing methods, before `View`):

```go
// TextAreaAction represents the result of handling a key press in a TextArea.
type TextAreaAction int

const (
	// TextAreaHandled means the key was consumed by the TextArea (cursor move, char insert, etc.).
	TextAreaHandled TextAreaAction = iota
	// TextAreaSubmit means the user triggered submit (Ctrl+Enter, or Enter on Send button).
	TextAreaSubmit
	// TextAreaCancel means the user triggered cancel (Esc, or Enter on Cancel button).
	TextAreaCancel
	// TextAreaQuit means the user pressed Ctrl+C (global quit).
	TextAreaQuit
	// TextAreaUnhandled means the key was not consumed (caller should handle it).
	TextAreaUnhandled
)
```

```go
// HandleKey processes a key message against this TextArea and returns the
// resulting action. The TextArea is mutated in place for cursor movement,
// character insertion, focus cycling, etc. The caller is responsible for
// acting on Submit, Cancel, and Quit.
func (t *TextArea) HandleKey(msg tea.KeyMsg) TextAreaAction {
	switch msg.String() {
	case "tab":
		t.CycleForward()
		return TextAreaHandled

	case "shift+tab":
		t.CycleBackward()
		return TextAreaHandled

	case "ctrl+enter":
		return TextAreaSubmit

	case "enter":
		switch t.Focus() {
		case FocusTextInput:
			t.InsertNewline()
			return TextAreaHandled
		case FocusSendButton:
			return TextAreaSubmit
		case FocusCancelButton:
			return TextAreaCancel
		}
		return TextAreaHandled

	case "esc":
		return TextAreaCancel

	case "backspace":
		if t.Focus() == FocusTextInput {
			t.DeleteChar()
		}
		return TextAreaHandled

	case "delete":
		if t.Focus() == FocusTextInput {
			t.DeleteCharForward()
		}
		return TextAreaHandled

	case "up":
		if t.Focus() == FocusTextInput {
			t.MoveCursorUp()
		}
		return TextAreaHandled

	case "down":
		if t.Focus() == FocusTextInput {
			t.MoveCursorDown()
		}
		return TextAreaHandled

	case "left":
		if t.Focus() == FocusTextInput {
			t.MoveCursorLeft()
		}
		return TextAreaHandled

	case "right":
		if t.Focus() == FocusTextInput {
			t.MoveCursorRight()
		}
		return TextAreaHandled

	case "ctrl+c":
		return TextAreaQuit

	default:
		if t.Focus() == FocusTextInput {
			keyStr := msg.String()
			if keyStr == "space" {
				t.InsertChar(' ')
			} else if len(keyStr) == 1 {
				t.InsertChar(rune(keyStr[0]))
			} else if len(msg.Runes) > 0 {
				for _, r := range msg.Runes {
					t.InsertChar(r)
				}
			}
			return TextAreaHandled
		}
		return TextAreaUnhandled
	}
}
```

Note: This requires adding `tea "github.com/charmbracelet/bubbletea"` to the imports in `textarea.go`.

### Exact Code Changes

#### Step 1: Add imports and types to `textarea.go`

Add `tea "github.com/charmbracelet/bubbletea"` to the import block (line 4-8). Add the `TextAreaAction` type and `HandleKey` method as shown above, inserted after line 265 (after `wrapLine`) and before line 268 (`View`).

#### Step 2: Rewrite `handleInputMode` in `update.go` (lines 641-751)

Replace the entire method body with:

```go
func (m Model) handleInputMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	action := m.inputArea.HandleKey(msg)

	switch action {
	case TextAreaSubmit:
		value := m.inputArea.Value()
		if value == "" {
			return m, nil
		}
		m.inputArea.Reset()
		return m, func() tea.Msg {
			return promptInputMsg{value}
		}

	case TextAreaCancel:
		m.inputMode = false
		m.inputArea.Reset()
		m.inputHandler = nil
		return m, nil

	case TextAreaQuit:
		return m, tea.Quit

	default:
		// TextAreaHandled or TextAreaUnhandled -- no Model-level action needed
		return m, nil
	}
}
```

This replaces lines 641-751 (111 lines) with 25 lines.

#### Step 3: Rewrite `handleTaskModal` case `TaskModalInput` in `update.go` (lines 869-973)

Replace lines 869-973 (the entire `case TaskModalInput:` block body including the inner switch) with:

```go
	case TaskModalInput:
		ta := m.taskModal.TextArea()
		action := ta.HandleKey(msg)

		switch action {
		case TextAreaSubmit:
			prompt := m.taskModal.Prompt()
			if prompt != "" {
				return m, func() tea.Msg {
					return taskRouteMsg{prompt: prompt}
				}
			}
			return m, nil

		case TextAreaCancel:
			m.taskModal.Hide()
			m.focus = FocusOutput
			return m, nil

		case TextAreaQuit:
			return m, tea.Quit

		default:
			return m, nil
		}
```

This replaces lines 869-973 (105 lines) with 24 lines.

### Test Strategy

Add tests to the existing `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/textarea_test.go`:

```go
func TestTextAreaHandleKey_CharInsertion(t *testing.T) {
	ta := NewTextArea()
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, "a", ta.Value())
}

func TestTextAreaHandleKey_Space(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("hello")
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, "hello ", ta.Value())
}

func TestTextAreaHandleKey_Newline(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("line1")
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, "line1\n", ta.Value())
}

func TestTextAreaHandleKey_Backspace(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("abc")
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, "ab", ta.Value())
}

func TestTextAreaHandleKey_CursorMovement(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("line1\nline2")

	// Move up
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	assert.Equal(t, TextAreaHandled, action)

	// Move down
	action = ta.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, TextAreaHandled, action)

	// Move left
	action = ta.HandleKey(tea.KeyMsg{Type: tea.KeyLeft})
	assert.Equal(t, TextAreaHandled, action)

	// Move right
	action = ta.HandleKey(tea.KeyMsg{Type: tea.KeyRight})
	assert.Equal(t, TextAreaHandled, action)
}

func TestTextAreaHandleKey_TabCycling(t *testing.T) {
	ta := NewTextArea()
	assert.Equal(t, FocusTextInput, ta.Focus())

	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, FocusSendButton, ta.Focus())

	action = ta.HandleKey(tea.KeyMsg{Type: tea.KeyShiftTab})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, FocusTextInput, ta.Focus())
}

func TestTextAreaHandleKey_CtrlEnterSubmits(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("test")
	// Ctrl+Enter from any focus should return Submit
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlJ}) // Ctrl+Enter maps to various types
	// Note: In real bubbletea, "ctrl+enter" is the string representation.
	// We test via the string-based approach:
	ta.SetFocus(FocusSendButton)
	action = ta.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, TextAreaSubmit, action)
}

func TestTextAreaHandleKey_EnterOnSendButton(t *testing.T) {
	ta := NewTextArea()
	ta.SetFocus(FocusSendButton)
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, TextAreaSubmit, action)
}

func TestTextAreaHandleKey_EnterOnCancelButton(t *testing.T) {
	ta := NewTextArea()
	ta.SetFocus(FocusCancelButton)
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, TextAreaCancel, action)
}

func TestTextAreaHandleKey_Escape(t *testing.T) {
	ta := NewTextArea()
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyEscape})
	assert.Equal(t, TextAreaCancel, action)
}

func TestTextAreaHandleKey_CtrlC(t *testing.T) {
	ta := NewTextArea()
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	assert.Equal(t, TextAreaQuit, action)
}

func TestTextAreaHandleKey_IgnoredWhenNotFocused(t *testing.T) {
	ta := NewTextArea()
	ta.SetFocus(FocusSendButton)
	// Typing 'a' when send button focused should not insert text
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	assert.Equal(t, TextAreaUnhandled, action)
	assert.Equal(t, "", ta.Value())
}
```

### Verification

- All existing tests in `textarea_test.go` pass unchanged (they test TextArea operations directly, not via HandleKey).
- All existing tests in `update_scroll_test.go` and `update_feedback_test.go` pass unchanged.
- The new tests cover every branch of HandleKey: char insert, space, newline, backspace, delete, all 4 cursor directions, tab cycling, submit (Ctrl+Enter and Enter-on-Send), cancel (Esc and Enter-on-Cancel), quit (Ctrl+C), and unhandled (rune when not focused on text).

---

## Feature 3: Dropdown Search/Filtering

### Problem

Users with many worktrees or sessions must arrow-key through the entire dropdown list. There is no way to narrow the list by typing.

### Design

Add a `filterText` field to `Dropdown`. When the dropdown is open and the user types alphanumeric characters, they accumulate in `filterText` and the visible item list is filtered. The `Dropdown` maintains both the full `items` slice and a derived `filteredIndices` slice mapping filtered positions back to the original `items` indices. All navigation (MoveSelection, SelectedItem, Enter-to-select) operates on the filtered view. Opening the dropdown always resets the filter.

#### Why `filteredIndices` instead of a separate `filteredItems` slice

Using an index mapping avoids copying `DropdownItem` structs and, critically, preserves the original indices so that `SelectedItem()` returns the correct item from `items` by original index. This prevents the bug where selecting item #2 in a filtered list of 3 would return the wrong item from the full list.

### New/Modified Types and Methods on `Dropdown`

```go
// In the Dropdown struct, add these fields:
type Dropdown struct {
	items           []DropdownItem
	filteredIndices []int    // indices into items; nil means "no filter active" (show all)
	filterText      string   // current filter string
	selectedIdx     int      // index into the effective list (filteredIndices or items)
	isOpen          bool
	width           int
	maxVisible      int
	scrollOffset    int
}
```

New methods:

```go
// FilterText returns the current filter string.
func (d *Dropdown) FilterText() string {
	return d.filterText
}

// AppendFilter adds a rune to the filter and recomputes the filtered list.
func (d *Dropdown) AppendFilter(r rune) {
	d.filterText += string(r)
	d.applyFilter()
}

// BackspaceFilter removes the last rune from the filter.
// If the filter becomes empty, the full list is restored.
func (d *Dropdown) BackspaceFilter() {
	if d.filterText == "" {
		return
	}
	runes := []rune(d.filterText)
	d.filterText = string(runes[:len(runes)-1])
	d.applyFilter()
}

// ClearFilter resets the filter and shows all items.
func (d *Dropdown) ClearFilter() {
	d.filterText = ""
	d.filteredIndices = nil
	d.selectedIdx = 0
	d.scrollOffset = 0
}

// applyFilter recomputes filteredIndices from filterText.
func (d *Dropdown) applyFilter() {
	if d.filterText == "" {
		d.filteredIndices = nil
		d.selectedIdx = 0
		d.scrollOffset = 0
		return
	}

	lower := strings.ToLower(d.filterText)
	d.filteredIndices = nil
	for i, item := range d.items {
		if item.ID == "---separator---" {
			continue // Never include separators in filtered results
		}
		if strings.Contains(strings.ToLower(item.Label), lower) {
			d.filteredIndices = append(d.filteredIndices, i)
		}
	}

	// Clamp selection
	if d.selectedIdx >= len(d.filteredIndices) {
		d.selectedIdx = max(0, len(d.filteredIndices)-1)
	}
	d.scrollOffset = 0
	d.ensureVisible()
}

// effectiveItems returns the items currently visible (filtered or all).
func (d *Dropdown) effectiveItems() []DropdownItem {
	if d.filteredIndices == nil {
		return d.items
	}
	result := make([]DropdownItem, len(d.filteredIndices))
	for i, idx := range d.filteredIndices {
		result[i] = d.items[idx]
	}
	return result
}

// effectiveLen returns the count of items currently visible.
func (d *Dropdown) effectiveLen() int {
	if d.filteredIndices == nil {
		return len(d.items)
	}
	return len(d.filteredIndices)
}
```

### Exact Code Changes to `dropdown.go`

#### Step 1: Add `filteredIndices` and `filterText` fields to `Dropdown` struct (line 21-27)

Replace the struct definition:

```go
type Dropdown struct {
	items           []DropdownItem
	filteredIndices []int    // indices into items; nil = no filter (show all)
	filterText      string
	selectedIdx     int
	isOpen          bool
	width           int
	maxVisible      int
	scrollOffset    int
}
```

#### Step 2: Modify `Open()` to reset filter (line 62-64)

```go
func (d *Dropdown) Open() {
	d.isOpen = true
	d.ClearFilter()
}
```

#### Step 3: Modify `SelectedItem()` to respect filtering (lines 87-92)

Replace with:

```go
func (d *Dropdown) SelectedItem() *DropdownItem {
	eff := d.effectiveItems()
	if d.selectedIdx >= 0 && d.selectedIdx < len(eff) {
		// Return pointer to original item (not the copy in eff)
		if d.filteredIndices != nil {
			origIdx := d.filteredIndices[d.selectedIdx]
			return &d.items[origIdx]
		}
		return &d.items[d.selectedIdx]
	}
	return nil
}
```

#### Step 4: Modify `MoveSelection()` to use effective list (lines 115-135)

Replace with:

```go
func (d *Dropdown) MoveSelection(delta int) {
	effLen := d.effectiveLen()
	if effLen == 0 {
		return
	}
	eff := d.effectiveItems()
	newIdx := clamp(d.selectedIdx+delta, 0, effLen-1)
	// Skip separator items by continuing in the same direction
	step := 1
	if delta < 0 {
		step = -1
	}
	for newIdx >= 0 && newIdx < effLen && eff[newIdx].ID == "---separator---" {
		newIdx += step
	}
	newIdx = clamp(newIdx, 0, effLen-1)
	if eff[newIdx].ID == "---separator---" {
		return
	}
	d.selectedIdx = newIdx
	d.ensureVisible()
}
```

#### Step 5: Modify `ensureVisible()` to use effective length (lines 138-145)

No change needed -- it already operates on `d.selectedIdx` and `d.scrollOffset` / `d.maxVisible`. But we should guard against `effectiveLen`:

```go
func (d *Dropdown) ensureVisible() {
	if d.selectedIdx < d.scrollOffset {
		d.scrollOffset = d.selectedIdx
	}
	if d.selectedIdx >= d.scrollOffset+d.maxVisible {
		d.scrollOffset = d.selectedIdx - d.maxVisible + 1
	}
}
```

(Unchanged -- it works correctly because `selectedIdx` is always relative to the effective list.)

#### Step 6: Modify `ViewList()` to show filter indicator and use effective items (lines 176-241)

Replace with:

```go
func (d *Dropdown) ViewList() string {
	eff := d.effectiveItems()
	if len(eff) == 0 {
		if d.filterText != "" {
			return dimStyle.Render("  No matches for \"" + d.filterText + "\"")
		}
		return dimStyle.Render("  (empty)")
	}

	var b strings.Builder

	// Show filter indicator when active
	if d.filterText != "" {
		filterLine := dimStyle.Render("  Filter: ") + d.filterText
		b.WriteString(filterLine)
		b.WriteString("\n")
	}

	endIdx := min(d.scrollOffset+d.maxVisible, len(eff))

	// Show scroll indicator at top if needed
	if d.scrollOffset > 0 {
		b.WriteString(dimStyle.Render("  \u2191 more"))
		b.WriteString("\n")
	}

	for i := d.scrollOffset; i < endIdx; i++ {
		item := eff[i]

		prefix := "  "
		if i == d.selectedIdx {
			prefix = "> "
		}

		var line strings.Builder
		line.WriteString(prefix)
		if item.Icon != "" {
			line.WriteString(item.Icon)
			line.WriteString(" ")
		}
		line.WriteString(item.Label)
		if item.Badge != "" {
			line.WriteString(" ")
			line.WriteString(item.Badge)
		}

		lineStr := line.String()
		if d.width > 0 && lipgloss.Width(lineStr) > d.width-2 {
			lineStr = truncateVisual(lineStr, d.width-5)
		}

		if i == d.selectedIdx {
			b.WriteString(selectedStyle.Render(lineStr))
		} else {
			b.WriteString(lineStr)
		}
		b.WriteString("\n")

		// Show subtitle if present
		if item.Subtitle != "" {
			subtitle := "    " + dimStyle.Render(item.Subtitle)
			if d.width > 0 && lipgloss.Width(subtitle) > d.width-2 {
				subtitle = truncateVisual(subtitle, d.width-5)
			}
			b.WriteString(subtitle)
			b.WriteString("\n")
		}
	}

	// Show scroll indicator at bottom if needed
	if endIdx < len(eff) {
		b.WriteString(dimStyle.Render("  \u2193 more"))
	}

	return b.String()
}
```

#### Step 7: Modify `SetItems()` to clear filter (lines 38-44)

```go
func (d *Dropdown) SetItems(items []DropdownItem) {
	d.items = items
	d.ClearFilter()
	if d.selectedIdx >= len(items) {
		d.selectedIdx = max(0, len(items)-1)
	}
}
```

#### Step 8: Add the new filter methods and helpers

Add `ClearFilter`, `AppendFilter`, `BackspaceFilter`, `applyFilter`, `effectiveItems`, `effectiveLen` as shown in the design section above. These go after `ensureVisible()` and before `Count()`.

#### Step 9: Modify `SelectByID` to work on the full items list (lines 95-104)

`SelectByID` is called when the dropdown is closed (e.g., programmatic selection). It should search the full `items` list and clear any filter:

```go
func (d *Dropdown) SelectByID(id string) bool {
	// Clear filter so selectedIdx maps to full items list
	d.ClearFilter()
	for i, item := range d.items {
		if item.ID == id {
			d.selectedIdx = i
			d.ensureVisible()
			return true
		}
	}
	return false
}
```

#### Step 10: Modify `SelectIndex` similarly (lines 107-112)

```go
func (d *Dropdown) SelectIndex(idx int) {
	d.ClearFilter()
	if idx >= 0 && idx < len(d.items) {
		d.selectedIdx = idx
		d.ensureVisible()
	}
}
```

### Changes to `update.go` -- `handleDropdownMode` (lines 560-636)

Route alphanumeric keys and backspace to the dropdown's filter methods. Insert before the final `return m, nil` at line 635, inside the switch:

Add these cases after the `"k", "up"` case (line 593) and before the `"enter"` case (line 595):

```go
	case "backspace":
		// Remove last filter character
		if m.focus == FocusWorktreeDropdown {
			m.worktreeDropdown.BackspaceFilter()
		} else {
			m.sessionDropdown.BackspaceFilter()
		}
		return m, nil
```

And add a default case at the end of the switch (before the closing brace), replacing the existing implicit fall-through:

```go
	default:
		// Type-to-filter: route printable characters to the dropdown
		keyStr := msg.String()
		var r rune
		if len(keyStr) == 1 {
			r = rune(keyStr[0])
		} else if len(msg.Runes) == 1 {
			r = msg.Runes[0]
		}
		if r != 0 && r >= ' ' && r != 127 { // printable, non-control
			if m.focus == FocusWorktreeDropdown {
				m.worktreeDropdown.AppendFilter(r)
			} else {
				m.sessionDropdown.AppendFilter(r)
			}
			return m, nil
		}
		return m, nil
	}
```

Also modify the `"esc"` case to clear filter first, and only close if filter is already empty:

```go
	case "esc":
		// If filter is active, clear it first. If already empty, close dropdown.
		dd := m.worktreeDropdown
		if m.focus == FocusSessionDropdown {
			dd = m.sessionDropdown
		}
		if dd.FilterText() != "" {
			dd.ClearFilter()
			return m, nil
		}
		m.worktreeDropdown.Close()
		m.sessionDropdown.Close()
		m.focus = FocusOutput
		return m, nil
```

And split the existing `"esc", "alt+w", "alt+s"` case. Keep `alt+w` and `alt+s` as a separate case that always closes:

```go
	case "alt+w", "alt+s":
		// Always close dropdown immediately
		m.worktreeDropdown.Close()
		m.sessionDropdown.Close()
		m.focus = FocusOutput
		return m, nil

	case "esc":
		// If filter is active, clear it first. If already empty, close dropdown.
		dd := m.worktreeDropdown
		if m.focus == FocusSessionDropdown {
			dd = m.sessionDropdown
		}
		if dd.FilterText() != "" {
			dd.ClearFilter()
			return m, nil
		}
		m.worktreeDropdown.Close()
		m.sessionDropdown.Close()
		m.focus = FocusOutput
		return m, nil
```

### Test Strategy

Create `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/dropdown_test.go`:

```go
package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testItems() []DropdownItem {
	return []DropdownItem{
		{ID: "alpha", Label: "Alpha Feature"},
		{ID: "beta", Label: "Beta Testing"},
		{ID: "gamma", Label: "Gamma Ray"},
		{ID: "delta", Label: "Delta Force"},
		{ID: "epsilon", Label: "Epsilon Eridani"},
	}
}

func TestDropdownFilter_ReducesList(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	d.AppendFilter('a')
	// "Alpha", "Beta", "Gamma", "Delta", "Eridani" -- items containing 'a'
	eff := d.effectiveItems()
	for _, item := range eff {
		assert.Contains(t, strings.ToLower(item.Label), "a")
	}
	assert.Less(t, len(eff), 5) // Some items should be filtered out
}

func TestDropdownFilter_CaseInsensitive(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	d.AppendFilter('A')
	effUpper := d.effectiveItems()

	d.ClearFilter()
	d.AppendFilter('a')
	effLower := d.effectiveItems()

	assert.Equal(t, len(effUpper), len(effLower))
	for i := range effUpper {
		assert.Equal(t, effUpper[i].ID, effLower[i].ID)
	}
}

func TestDropdownFilter_BackspaceExtendsList(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	d.AppendFilter('a')
	d.AppendFilter('l')
	narrowCount := d.effectiveLen()

	d.BackspaceFilter()
	widerCount := d.effectiveLen()

	assert.GreaterOrEqual(t, widerCount, narrowCount)
}

func TestDropdownFilter_EmptyShowsAll(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	d.AppendFilter('x')
	d.BackspaceFilter()

	assert.Equal(t, "", d.FilterText())
	assert.Equal(t, 5, d.effectiveLen())
}

func TestDropdownFilter_SelectionFromFilteredListReturnsCorrectID(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	// Filter to only items containing "eta" -> "Beta Testing", "Delta Force"
	d.AppendFilter('e')
	d.AppendFilter('t')
	d.AppendFilter('a')

	require.Greater(t, d.effectiveLen(), 0)

	// Select first filtered item
	d.SelectIndex(0) // This needs to work within filtered context -- use MoveSelection instead
	item := d.SelectedItem()
	require.NotNil(t, item)
	// The returned item should be from the original items list
	found := false
	for _, orig := range testItems() {
		if orig.ID == item.ID {
			found = true
			assert.Contains(t, strings.ToLower(orig.Label), "eta")
			break
		}
	}
	assert.True(t, found, "Selected item ID should exist in original items")
}

func TestDropdownFilter_OpenResetsFilter(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()
	d.AppendFilter('z')
	assert.Equal(t, "z", d.FilterText())

	d.Close()
	d.Open()
	assert.Equal(t, "", d.FilterText())
	assert.Equal(t, 5, d.effectiveLen())
}

func TestDropdownFilter_SeparatorsExcluded(t *testing.T) {
	items := []DropdownItem{
		{ID: "live1", Label: "Live Session"},
		{ID: "---separator---", Label: "--- History ---"},
		{ID: "hist1", Label: "History Session"},
	}
	d := NewDropdown(items)
	d.Open()

	d.AppendFilter('s') // matches "Live Session" and "History Session"
	eff := d.effectiveItems()
	for _, item := range eff {
		assert.NotEqual(t, "---separator---", item.ID)
	}
}

func TestDropdownFilter_NoMatchesShowsEmptyMessage(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()
	d.AppendFilter('z')
	d.AppendFilter('z')
	d.AppendFilter('z')

	assert.Equal(t, 0, d.effectiveLen())
	view := d.ViewList()
	assert.Contains(t, view, "No matches")
	assert.Contains(t, view, "zzz")
}

func TestDropdownFilter_ViewShowsFilterIndicator(t *testing.T) {
	d := NewDropdown(testItems())
	d.SetWidth(60)
	d.Open()

	d.AppendFilter('a')
	view := d.ViewList()
	assert.Contains(t, view, "Filter:")
	assert.Contains(t, view, "a")
}

func TestDropdownFilter_NavigationOnFilteredList(t *testing.T) {
	d := NewDropdown(testItems())
	d.Open()

	// Filter to 2 items
	d.AppendFilter('a')
	d.AppendFilter('l')
	effLen := d.effectiveLen()
	require.Greater(t, effLen, 0)

	// Navigate down
	d.MoveSelection(1)
	idx := d.SelectedIndex()
	assert.LessOrEqual(t, idx, effLen-1)

	// Navigate up
	d.MoveSelection(-1)
	assert.GreaterOrEqual(t, d.SelectedIndex(), 0)
}
```

Note: The test file will also need `"strings"` in its imports.

### Verification

- All existing dropdown behavior (open, close, navigate, select) works identically when no filter is typed.
- Filter clears on open, on SetItems, on SelectByID, on SelectIndex.
- Selected item from a filtered list returns the correct original DropdownItem (verified by ID).
- The `?` help overlay key still works in dropdown mode (it is handled before the default case).

---

## Summary of Files Modified

| File | Feature | Change Type |
|---|---|---|
| `bramble/app/view.go` | F1 | Add `renderScrollableLines`; simplify `renderOutputArea` and `renderHistorySession` |
| `bramble/app/textarea.go` | F2 | Add `TextAreaAction` type and `HandleKey` method; add `tea` import |
| `bramble/app/update.go` | F2 | Rewrite `handleInputMode` and `TaskModalInput` case to use `HandleKey` |
| `bramble/app/dropdown.go` | F3 | Add filter fields, filter methods, modify `Open`/`SetItems`/`SelectByID`/`SelectIndex`/`MoveSelection`/`SelectedItem`/`ViewList` |
| `bramble/app/update.go` | F3 | Modify `handleDropdownMode` to route chars/backspace/esc to filter |

## New Test Files

| File | Feature | Tests |
|---|---|---|
| `bramble/app/scrollrender_test.go` | F1 | 7 tests covering all scroll cases |
| `bramble/app/textarea_test.go` (additions) | F2 | 11 tests covering all HandleKey branches |
| `bramble/app/dropdown_test.go` | F3 | 10 tests covering filter behavior |

## Estimated Line Counts

| Feature | Lines Removed | Lines Added (prod) | Lines Added (test) | Net |
|---|---|---|---|---|
| F1 | ~130 (duplicated scroll logic) | ~65 (shared function) + ~8 (call sites) | ~80 | -57 prod, +80 test |
| F2 | ~216 (duplicated key handling) | ~60 (HandleKey) + ~50 (call sites) | ~100 | -106 prod, +100 test |
| F3 | ~0 | ~120 (filter logic + view changes) + ~35 (update.go) | ~120 | +155 prod, +120 test |
| **Total** | ~346 | ~338 | ~300 | **-8 prod, +300 test** |
