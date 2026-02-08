# Bramble Cycle 1 -- Architecture Design

This document specifies the exact technical design for three UX improvements identified in the PM report. Each section contains type definitions, function signatures, modification targets with line numbers, BubbleTea message flows, and test strategies. The designs reference real types and functions from the current codebase.

**Source files referenced** (all under `bramble/app/`):
- `model.go` -- Model struct, FocusArea enum, message types
- `update.go` -- Update handler, key dispatch per mode
- `view.go` -- View rendering, layout, styles
- `dropdown.go` -- Dropdown component
- `taskmodal.go` -- Task modal component
- `textarea.go` -- TextArea component
- `session/types.go` -- Session types, OutputLine

---

## Feature 1: Help Overlay (`?` key)

### 1.1 Overview

A context-aware full-screen overlay that shows all available key bindings for the current mode. Opens with `?`, closes with `?` or `Esc`. The content adapts based on: which FocusArea was active before opening, whether a session is selected, whether the session is idle, and whether tmux mode is active.

### 1.2 New Types

**File: `bramble/app/helpoverlay.go`** (new file)

```go
package app

import (
    "strings"

    "github.com/charmbracelet/lipgloss"
)

// HelpBinding represents a single key binding entry.
type HelpBinding struct {
    Key         string // e.g. "Alt-W", "?", "Enter"
    Description string // e.g. "Open worktree dropdown"
}

// HelpSection groups related key bindings.
type HelpSection struct {
    Title    string
    Bindings []HelpBinding
}

// HelpOverlay renders a context-aware help screen.
type HelpOverlay struct {
    sections       []HelpSection
    width          int
    height         int
    scrollOffset   int
    previousFocus  FocusArea // remember what was focused before opening help
}
```

### 1.3 Model Changes

**File: `model.go`**

Add `FocusHelp` to the FocusArea enum (after line 24):

```go
const (
    FocusOutput           FocusArea = iota // Main center area (default)
    FocusInput                             // Input line at bottom
    FocusWorktreeDropdown                  // Alt-W dropdown open
    FocusSessionDropdown                   // Alt-S dropdown open
    FocusTaskModal                         // Task modal open
    FocusHelp                              // Help overlay open   <-- NEW
)
```

Add `helpOverlay` field to the Model struct (after line 38, `taskModal` field):

```go
type Model struct {
    // ... existing fields ...
    taskModal             *TaskModal
    helpOverlay           *HelpOverlay    // <-- NEW
    // ... rest of fields ...
}
```

Initialize in `NewModel()` (after line 85, `taskModal` init):

```go
helpOverlay: &HelpOverlay{},
```

### 1.4 HelpOverlay Methods

**File: `bramble/app/helpoverlay.go`**

```go
// NewHelpOverlay creates a new help overlay.
func NewHelpOverlay() *HelpOverlay {
    return &HelpOverlay{}
}

// SetSize updates the overlay dimensions.
func (h *HelpOverlay) SetSize(w, ht int) {
    h.width = w
    h.height = ht
}

// SetSections replaces the help content.
func (h *HelpOverlay) SetSections(sections []HelpSection) {
    h.sections = sections
    h.scrollOffset = 0
}

// ScrollUp scrolls the help content up by one line.
func (h *HelpOverlay) ScrollUp() {
    if h.scrollOffset > 0 {
        h.scrollOffset--
    }
}

// ScrollDown scrolls the help content down by one line.
func (h *HelpOverlay) ScrollDown() {
    h.scrollOffset++
    // Clamped during rendering based on content height
}

// View renders the help overlay as a centered full-screen box.
func (h *HelpOverlay) View() string {
    // Implementation renders all sections into a two-column layout:
    //   KEY (right-aligned, dimStyle)  DESCRIPTION (left-aligned)
    // Grouped under bold section titles with blank-line separators.
    // Uses lipgloss.Place() to center the box, same pattern as TaskModal.View().
    //
    // The box width is min(h.width - 10, 72).
    // Scrolling is supported for terminals shorter than the content.
    //
    // Footer: dimStyle.Render("Press ? or Esc to close")
}
```

The `View()` method layout:

```
    ╭──────────────────────────────────────────────────────╮
    │                  Bramble Key Bindings                 │
    │                                                      │
    │  Navigation                                          │
    │     Alt-W   Open worktree selector                   │
    │     Alt-S   Open session selector                    │
    │      F2     Toggle file tree split                   │
    │     Tab     Switch pane focus (when split)           │
    │      ?      Toggle this help                         │
    │                                                      │
    │  Sessions                                            │
    │       t     New task (AI-routed)                     │
    │       p     Start planner session                    │
    │       b     Start builder session                    │
    │       f     Follow-up on idle session                │
    │       s     Stop session                             │
    │       a     Approve plan & start builder             │
    │                                                      │
    │  Worktrees                                           │
    │       n     Create new worktree                      │
    │       d     Delete worktree                          │
    │       e     Open in editor                           │
    │       r     Refresh worktrees                        │
    │                                                      │
    │  Output                                              │
    │     ↑/k     Scroll up                                │
    │     ↓/j     Scroll down                              │
    │   PgUp      Scroll up 10 lines                       │
    │   PgDn      Scroll down 10 lines                     │
    │    Home     Scroll to top                            │
    │    End      Scroll to bottom                         │
    │                                                      │
    │  General                                             │
    │     Esc     Clear error / close overlay              │
    │      q      Quit                                     │
    │                                                      │
    │              Press ? or Esc to close                  │
    ╰──────────────────────────────────────────────────────╯
```

### 1.5 Context-Aware Section Builder

**File: `bramble/app/helpoverlay.go`**

```go
// buildHelpSections returns help sections appropriate for the given context.
// It reads the model state to determine which keys are relevant.
func buildHelpSections(m *Model) []HelpSection {
    inTmux := m.sessionManager.IsInTmuxMode()
    hasWorktree := m.selectedWorktree() != nil
    hasSession := m.viewingSessionID != ""
    var sessIdle, sessRunning, sessIsPlanner bool
    if sess := m.selectedSession(); sess != nil {
        sessIdle = sess.Status == session.StatusIdle
        sessRunning = sess.Status == session.StatusRunning
        sessIsPlanner = sess.Type == session.SessionTypePlanner
    }

    // The previousFocus field tells us what mode the user was in
    // before pressing '?'. We use this to highlight the relevant section
    // and to show mode-specific bindings (dropdown, input, etc.).

    var sections []HelpSection

    // Always show navigation
    nav := HelpSection{Title: "Navigation"}
    nav.Bindings = append(nav.Bindings,
        HelpBinding{"Alt-W", "Open worktree selector"},
        HelpBinding{"?", "Toggle this help"},
    )
    if !inTmux {
        nav.Bindings = append(nav.Bindings,
            HelpBinding{"Alt-S", "Open session selector"},
            HelpBinding{"F2", "Toggle file tree split"},
            HelpBinding{"Tab", "Switch pane focus (when split)"},
        )
    }
    sections = append(sections, nav)

    // Session actions -- vary based on context
    sess := HelpSection{Title: "Sessions"}
    if hasWorktree {
        sess.Bindings = append(sess.Bindings,
            HelpBinding{"t", "New task (AI picks worktree)"},
            HelpBinding{"p", "Start planner session"},
            HelpBinding{"b", "Start builder session"},
        )
    }
    if hasSession && sessIdle && !inTmux {
        sess.Bindings = append(sess.Bindings,
            HelpBinding{"f", "Follow-up on idle session"},
        )
        if sessIsPlanner {
            sess.Bindings = append(sess.Bindings,
                HelpBinding{"a", "Approve plan & start builder"},
            )
        }
    }
    if hasSession && (sessRunning || sessIdle) && !inTmux {
        sess.Bindings = append(sess.Bindings,
            HelpBinding{"s", "Stop session"},
        )
    }
    if len(sess.Bindings) > 0 {
        sections = append(sections, sess)
    }

    // Worktree actions
    wt := HelpSection{Title: "Worktrees"}
    wt.Bindings = append(wt.Bindings,
        HelpBinding{"n", "Create new worktree"},
    )
    if hasWorktree {
        wt.Bindings = append(wt.Bindings,
            HelpBinding{"d", "Delete worktree"},
            HelpBinding{"e", "Open in editor"},
        )
    }
    wt.Bindings = append(wt.Bindings,
        HelpBinding{"r", "Refresh worktrees"},
    )
    sections = append(sections, wt)

    // Output scrolling (non-tmux only for scroll; tmux has navigate)
    if inTmux {
        tmux := HelpSection{Title: "Session List"}
        tmux.Bindings = append(tmux.Bindings,
            HelpBinding{"Up/k", "Navigate up"},
            HelpBinding{"Down/j", "Navigate down"},
            HelpBinding{"Enter", "Switch to tmux window"},
        )
        sections = append(sections, tmux)
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

    // Dropdown mode bindings (shown if user opened help from dropdown)
    if m.helpOverlay.previousFocus == FocusWorktreeDropdown ||
       m.helpOverlay.previousFocus == FocusSessionDropdown {
        dd := HelpSection{Title: "Dropdown"}
        dd.Bindings = append(dd.Bindings,
            HelpBinding{"Up/k", "Move selection up"},
            HelpBinding{"Down/j", "Move selection down"},
            HelpBinding{"Enter", "Confirm selection"},
            HelpBinding{"Esc", "Close dropdown"},
        )
        sections = append(sections, dd)
    }

    // Input mode bindings (shown if user opened help from input)
    if m.helpOverlay.previousFocus == FocusInput {
        inp := HelpSection{Title: "Input Mode"}
        inp.Bindings = append(inp.Bindings,
            HelpBinding{"Tab", "Cycle focus (text/send/cancel)"},
            HelpBinding{"Ctrl+Enter", "Submit prompt"},
            HelpBinding{"Esc", "Cancel input"},
        )
        sections = append(sections, inp)
    }

    // General
    gen := HelpSection{Title: "General"}
    gen.Bindings = append(gen.Bindings,
        HelpBinding{"Esc", "Clear error / close overlay"},
        HelpBinding{"q", "Quit Bramble"},
        HelpBinding{"Ctrl-C", "Force quit"},
    )
    sections = append(sections, gen)

    return sections
}
```

### 1.6 Update Handler Changes

**File: `update.go`**

**1.6a. Add help overlay handling in Update() switch on KeyMsg (line 21-36).**

Insert a new check before the task modal check. The help overlay has the highest visual priority (it covers everything), so it should be checked first:

```go
case tea.KeyMsg:
    // Handle help overlay first (highest visual priority)
    if m.focus == FocusHelp {
        return m.handleHelpOverlay(msg)
    }
    // Handle task modal
    if m.taskModal.IsVisible() {
        return m.handleTaskModal(msg)
    }
    // ... rest unchanged ...
```

**1.6b. Add `?` key in `handleKeyPress()` (insert after line 210, before `case "q", "ctrl+c":`).**

```go
case "?":
    // Open help overlay
    m.helpOverlay.previousFocus = m.focus
    m.helpOverlay.SetSize(m.width, m.height)
    sections := buildHelpSections(&m)
    m.helpOverlay.SetSections(sections)
    m.focus = FocusHelp
    return m, nil
```

**1.6c. Also allow `?` from dropdown mode and input mode.** In `handleDropdownMode()` (around line 508) and `handleInputMode()` (around line 586), add a `"?"` case that opens help the same way. This ensures help is accessible from any mode.

In `handleDropdownMode()`, add before the `"q"` case (line 576):

```go
case "?":
    m.helpOverlay.previousFocus = m.focus
    m.helpOverlay.SetSize(m.width, m.height)
    sections := buildHelpSections(&m)
    m.helpOverlay.SetSections(sections)
    m.focus = FocusHelp
    return m, nil
```

**1.6d. New handler function:**

```go
// handleHelpOverlay handles key presses when the help overlay is visible.
func (m Model) handleHelpOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
    switch msg.String() {
    case "?", "esc":
        // Close help, restore previous focus
        m.focus = m.helpOverlay.previousFocus
        return m, nil
    case "up", "k":
        m.helpOverlay.ScrollUp()
        return m, nil
    case "down", "j":
        m.helpOverlay.ScrollDown()
        return m, nil
    case "q", "ctrl+c":
        return m, tea.Quit
    }
    // Ignore all other keys while help is open
    return m, nil
}
```

### 1.7 View Changes

**File: `view.go`**

In `View()` (line 63), add the help overlay check. Insert before the task modal check (line 136):

```go
// Show help overlay if active
if m.focus == FocusHelp {
    return m.helpOverlay.View()
}

// Show task modal if visible
if m.taskModal.IsVisible() {
    return m.taskModal.View()
}
```

### 1.8 Status Bar Update

**File: `view.go`**

In `renderStatusBar()`, ensure `[?]help` is always shown as the last hint in every mode. Add it to the end of each `hints` slice construction (lines 663-693).

For the normal mode (no session selected, line 686):
```go
hints = []string{"[Alt-W]worktree", "[Alt-S]session", "[t]ask", "[F2]split"}
// ... existing conditional hints ...
hints = append(hints, "[?]help", "[q]uit")
```

Apply the same pattern to all hint lists: always include `[?]help` before `[q]uit`.

### 1.9 WindowSizeMsg Forwarding

**File: `update.go`**

In the `tea.WindowSizeMsg` handler (line 37-49), forward the size to the help overlay:

```go
case tea.WindowSizeMsg:
    m.width = msg.Width
    m.height = msg.Height
    m.helpOverlay.SetSize(msg.Width, msg.Height)  // <-- NEW
    // ... rest unchanged ...
```

### 1.10 Edge Cases

1. **Terminal too small**: If height < 10, render a minimal single-column list instead of the boxed layout. Check in `View()`.
2. **Help from task modal**: `?` is NOT intercepted inside the task modal since text input mode captures all single characters. This is by design -- the task modal has its own `[Esc: cancel]` hints. If desired later, a help icon could be added to the modal footer.
3. **Scrolling**: If the help content is taller than `height - 6` (box chrome), enable scrolling with up/down keys and show `(scroll for more)` at the bottom.
4. **Focus restoration**: The `previousFocus` field ensures that closing help returns to exactly the mode the user was in. If help was opened from dropdown mode, closing it should return to the dropdown still open.

### 1.11 BubbleTea Message Flow

```
User presses '?' in normal mode
  -> handleKeyPress() matches "?"
  -> sets m.focus = FocusHelp, builds sections, returns (m, nil)

User presses '?' or 'Esc' in help overlay
  -> Update() routes to handleHelpOverlay()
  -> sets m.focus = m.helpOverlay.previousFocus, returns (m, nil)

User presses 'up'/'down' in help overlay
  -> handleHelpOverlay() calls ScrollUp()/ScrollDown()
  -> returns (m, nil), View() re-renders with new scroll offset
```

No tea.Cmd is needed for this feature -- it is entirely synchronous UI state.

### 1.12 Test Strategy

**File: `bramble/app/helpoverlay_test.go`** (new file)

```go
func TestHelpOverlayContextAwareness(t *testing.T) {
    // Test that buildHelpSections returns different sections based on model state

    // Case 1: No worktree, no session -> Sessions section should only show 't'
    // Case 2: Worktree selected, session running -> 's' (stop) should appear
    // Case 3: Worktree selected, planner idle -> 'a' (approve) should appear
    // Case 4: Tmux mode -> no Alt-S, no split, tmux-specific nav
    // Case 5: Previous focus was dropdown -> Dropdown section should appear
}

func TestHelpOverlayFocusRestoration(t *testing.T) {
    // Open help from FocusOutput -> close -> focus should be FocusOutput
    // Open help from FocusWorktreeDropdown -> close -> focus should be FocusWorktreeDropdown
}

func TestHelpOverlayKeyHandling(t *testing.T) {
    // '?' closes overlay
    // 'Esc' closes overlay
    // 'q' quits
    // Up/Down scroll
    // Other keys are ignored (no state change)
}

func TestHelpOverlayRendering(t *testing.T) {
    // Verify View() output contains section titles
    // Verify View() output contains key bindings
    // Verify footer text is present
    // Verify narrow terminal (width=40) still renders without panic
}

func TestHelpOverlayScrolling(t *testing.T) {
    // Create overlay with many sections
    // Verify scrollOffset clamps correctly
    // Verify ScrollUp at offset=0 stays at 0
}
```

Tests should use the pattern from `output_test.go`: create a Model with `NewModel()`, set state, call `buildHelpSections()`, and assert on the returned sections. For rendering tests, call `View()` and assert on string content.

---

## Feature 2: Toast Notification System

### 2.1 Overview

A transient notification area rendered between the center content and the status bar. Notifications auto-dismiss after a configurable duration. Up to 3 notifications stack vertically (newest at bottom). This replaces the broken `m.lastError` single-string pattern.

### 2.2 New Types

**File: `bramble/app/toast.go`** (new file)

```go
package app

import (
    "strings"
    "time"

    "github.com/charmbracelet/lipgloss"
    tea "github.com/charmbracelet/bubbletea"
)

// ToastLevel determines the notification style and auto-dismiss duration.
type ToastLevel int

const (
    ToastSuccess ToastLevel = iota
    ToastInfo
    ToastError
)

// Toast represents a single transient notification.
type Toast struct {
    Message   string
    Level     ToastLevel
    CreatedAt time.Time
    Duration  time.Duration // auto-dismiss after this duration
    ID        int           // monotonic ID for dismissal targeting
}

// IsExpired returns true if the toast has exceeded its duration.
func (t Toast) IsExpired(now time.Time) bool {
    return now.After(t.CreatedAt.Add(t.Duration))
}

// maxToasts is the maximum number of visible toasts.
const maxToasts = 3

// ToastManager manages the notification stack.
type ToastManager struct {
    toasts  []Toast
    nextID  int
    width   int
}

// NewToastManager creates a new toast manager.
func NewToastManager() *ToastManager {
    return &ToastManager{}
}

// SetWidth sets the rendering width.
func (tm *ToastManager) SetWidth(w int) {
    tm.width = w
}

// Add adds a new toast notification. If the stack exceeds maxToasts,
// the oldest toast is evicted.
func (tm *ToastManager) Add(message string, level ToastLevel) {
    var duration time.Duration
    switch level {
    case ToastSuccess:
        duration = 3 * time.Second
    case ToastInfo:
        duration = 4 * time.Second
    case ToastError:
        duration = 5 * time.Second
    }

    toast := Toast{
        Message:   message,
        Level:     level,
        CreatedAt: time.Now(),
        Duration:  duration,
        ID:        tm.nextID,
    }
    tm.nextID++
    tm.toasts = append(tm.toasts, toast)

    // Evict oldest if over max
    if len(tm.toasts) > maxToasts {
        tm.toasts = tm.toasts[len(tm.toasts)-maxToasts:]
    }
}

// Tick removes expired toasts. Returns true if any were removed
// (caller should schedule next tick if toasts remain).
func (tm *ToastManager) Tick(now time.Time) bool {
    var remaining []Toast
    changed := false
    for _, t := range tm.toasts {
        if t.IsExpired(now) {
            changed = true
        } else {
            remaining = append(remaining, t)
        }
    }
    tm.toasts = remaining
    return changed
}

// HasToasts returns true if there are active toasts.
func (tm *ToastManager) HasToasts() bool {
    return len(tm.toasts) > 0
}

// Count returns the number of active toasts.
func (tm *ToastManager) Count() int {
    return len(tm.toasts)
}

// Height returns the number of lines the toast area will consume.
// Returns 0 if no toasts are active.
func (tm *ToastManager) Height() int {
    if len(tm.toasts) == 0 {
        return 0
    }
    return len(tm.toasts) // Each toast is one line
}

// View renders all active toasts stacked vertically.
// Returns empty string if no toasts are active.
func (tm *ToastManager) View() string {
    if len(tm.toasts) == 0 {
        return ""
    }

    var lines []string
    for _, t := range tm.toasts {
        var style lipgloss.Style
        var icon string
        switch t.Level {
        case ToastSuccess:
            style = toastSuccessStyle
            icon = " ✓ "
        case ToastInfo:
            style = toastInfoStyle
            icon = " i "
        case ToastError:
            style = toastErrorStyle
            icon = " ! "
        }
        content := icon + t.Message
        // Truncate to width
        if tm.width > 0 && len(content) > tm.width-4 {
            content = content[:tm.width-7] + "..."
        }
        lines = append(lines, style.Width(tm.width).Render(content))
    }
    return strings.Join(lines, "\n")
}
```

### 2.3 Toast Styles

**File: `bramble/app/toast.go`** (same file, below the types)

```go
var (
    toastSuccessStyle = lipgloss.NewStyle().
        Background(lipgloss.Color("22")).  // dark green background
        Foreground(lipgloss.Color("10")).  // bright green text
        Padding(0, 1)

    toastInfoStyle = lipgloss.NewStyle().
        Background(lipgloss.Color("17")).  // dark blue background
        Foreground(lipgloss.Color("14")).  // cyan text
        Padding(0, 1)

    toastErrorStyle = lipgloss.NewStyle().
        Background(lipgloss.Color("52")).  // dark red background
        Foreground(lipgloss.Color("9")).   // bright red text
        Padding(0, 1)
)
```

### 2.4 New Message Type

**File: `model.go`**

Add to the message types block (after line 557, `deferredRefreshMsg`):

```go
// toastExpireMsg is sent when a toast timer fires to check for expired toasts.
toastExpireMsg struct{}
```

### 2.5 Model Changes

**File: `model.go`**

Add `toasts` field to Model (after line 38):

```go
type Model struct {
    // ... existing fields ...
    taskModal             *TaskModal
    helpOverlay           *HelpOverlay
    toasts                *ToastManager   // <-- NEW
    // ... rest of fields ...
}
```

Remove the `lastError` field (line 48). All uses of `lastError` will be migrated to toast notifications.

Initialize in `NewModel()` (after helpOverlay init):

```go
toasts: NewToastManager(),
```

### 2.6 Helper Method on Model

**File: `model.go`** (add at bottom)

```go
// addToast adds a notification and schedules expiry if this is the first toast.
func (m *Model) addToast(message string, level ToastLevel) tea.Cmd {
    m.toasts.Add(message, level)
    // Schedule a tick to check for expiration.
    // We schedule at the earliest expiration time of any active toast.
    return m.scheduleToastExpiry()
}

// scheduleToastExpiry schedules a tea.Tick at the earliest toast expiration time.
func (m *Model) scheduleToastExpiry() tea.Cmd {
    if !m.toasts.HasToasts() {
        return nil
    }
    // Find the earliest expiration
    earliest := m.toasts.toasts[0].CreatedAt.Add(m.toasts.toasts[0].Duration)
    for _, t := range m.toasts.toasts[1:] {
        exp := t.CreatedAt.Add(t.Duration)
        if exp.Before(earliest) {
            earliest = exp
        }
    }
    delay := time.Until(earliest)
    if delay < 0 {
        delay = 0
    }
    return tea.Tick(delay, func(time.Time) tea.Msg {
        return toastExpireMsg{}
    })
}
```

### 2.7 Update Handler Changes

**File: `update.go`**

**2.7a. Handle `toastExpireMsg` (add new case in the Update switch, after `tickMsg` at line 200):**

```go
case toastExpireMsg:
    m.toasts.Tick(time.Now())
    // If toasts remain, schedule the next expiry check
    if m.toasts.HasToasts() {
        return m, m.scheduleToastExpiry()
    }
    return m, nil
```

**2.7b. Replace all `m.lastError = ...` assignments with toast calls.**

Each replacement follows this pattern:

| Location | Old code | New code |
|----------|----------|----------|
| `update.go:133` (`errMsg` handler) | `m.lastError = msg.Error()` | `cmd := m.addToast(msg.Error(), ToastError); return m, cmd` |
| `update.go:175` (`worktreeOpResultMsg`) | `m.lastError = msg.err.Error()` | `cmds = append(cmds, m.addToast(msg.err.Error(), ToastError))` |
| `update.go:183` (`editorResultMsg`) | `m.lastError = "Failed to open editor: " + msg.err.Error()` | `cmds = append(cmds, m.addToast("Failed to open editor: "+msg.err.Error(), ToastError))` |
| `update.go:313` (session stop in tmux) | `m.lastError = "Close tmux windows directly..."` | `cmd := m.addToast("Close tmux windows directly...", ToastInfo); return m, cmd` |
| `update.go:388-389` (follow-up in tmux) | `m.lastError = "Follow-ups must be done..."` | `cmd := m.addToast("Follow-ups must be done...", ToastInfo); return m, cmd` |
| `update.go:716-718` (`startSession` error) | `m.lastError = err.Error()` | `return m, m.addToast(err.Error(), ToastError)` (note: must return the cmd) |
| `update.go:733-734` (`createWorktree` no repo) | `m.lastError = "No repository selected"` | `return m, m.addToast("No repository selected", ToastError)` |
| `update.go:1018-1019` (`confirmTask` no repo) | `m.lastError = "No repository selected"` | `return m, m.addToast("No repository selected", ToastError)` |

**2.7c. Add success toasts to operations.**

In `startSession()` (after line 722, after `m.updateSessionDropdown()`):
```go
cmd := m.addToast("Session started: "+string(sessionID)[:12], ToastSuccess)
return m, cmd
```

In `createWorktree()`, inside the async function's `worktreeOpResultMsg` return: success is indicated by `msg.err == nil`. In the `worktreeOpResultMsg` handler (line 173), add:
```go
case worktreeOpResultMsg:
    if msg.err != nil {
        cmds = append(cmds, m.addToast(msg.err.Error(), ToastError))
    } else if len(msg.messages) > 0 {
        cmds = append(cmds, m.addToast("Worktree operation completed", ToastSuccess))
    }
    m.worktreeOpMessages = msg.messages
    return m, tea.Batch(append(cmds, m.refreshWorktrees())...)
```

In `confirmTask()`, after `m.taskModal.Hide()` (line 1013):
```go
cmds = append(cmds, m.addToast("Task confirmed, starting session...", ToastSuccess))
```

In `deleteWorktree()`, the result goes through `worktreeOpResultMsg` which is already handled above.

**2.7d. Remove the `esc` clear-error behavior (line 477-479).**

The old code:
```go
case "esc":
    m.lastError = ""
    m.scrollOffset = 0
```

Replace with just:
```go
case "esc":
    m.scrollOffset = 0
```

Toasts auto-dismiss, so there is no need for manual error clearing.

### 2.8 View Layout Changes

**File: `view.go`**

**2.8a. Adjust layout calculation in `View()` (lines 73-92).**

The toast area goes between the center border and the status bar. Modify the height calculation:

```go
// Layout: top bar (1 line) + center + toast area (dynamic) + input area (dynamic) + status bar (1 line)
topBarHeight := 1
statusBarHeight := 1
toastHeight := m.toasts.Height() // <-- NEW
inputHeight := 0
if m.inputMode {
    // ... existing input height calculation unchanged ...
}
centerHeight := m.height - topBarHeight - statusBarHeight - toastHeight - inputHeight - 2 // borders
```

**2.8b. Insert toast rendering in `View()` layout assembly (after line 112).**

```go
// Build layout
parts := []string{topBar, centerBordered}

// Add toast notifications if any
if m.toasts.HasToasts() {
    m.toasts.SetWidth(m.width)
    parts = append(parts, m.toasts.View())
}

// Add input area if in input mode
if m.inputMode {
    // ... existing code ...
}

parts = append(parts, statusBar)
```

**2.8c. Remove the error display from `renderStatusBar()` (lines 708-711).**

Delete this block:
```go
// Error message if any
if m.lastError != "" {
    right = errorStyle.Render("Error: " + truncate(m.lastError, 40))
}
```

The right side of the status bar now always shows session counts, which is cleaner.

### 2.9 Toast Expiry with Existing Tick

The existing `tickMsg` fires every 100ms (line 122). We could piggyback on it to expire toasts, but that would couple the tick to toast logic and cause unnecessary re-renders. Instead, we use dedicated `tea.Tick` commands scheduled exactly at toast expiration times. This is more efficient and follows the BubbleTea idiom.

The `toastExpireMsg` handler calls `m.toasts.Tick(time.Now())` which removes expired toasts, then schedules the next expiry if toasts remain.

### 2.10 BubbleTea Message Flow

```
Operation succeeds (e.g. session started)
  -> handler calls m.addToast("Session started: xxx", ToastSuccess)
  -> ToastManager.Add() appends toast, sets CreatedAt=now, Duration=3s
  -> addToast() returns tea.Tick(3s) -> toastExpireMsg
  -> View() renders toast area (1 line) above status bar

3 seconds later:
  -> toastExpireMsg arrives
  -> m.toasts.Tick(time.Now()) removes expired toast
  -> if toasts remain, schedule next expiry
  -> View() re-renders without toast area (0 lines), center area grows

Error occurs:
  -> handler calls m.addToast("error message", ToastError)
  -> same flow, but Duration=5s and red styling

Multiple rapid operations:
  -> each calls addToast(), stack grows (max 3, oldest evicted)
  -> each toast has independent expiry timer
  -> toastExpireMsg checks all toasts, removes all expired ones
```

### 2.11 Edge Cases

1. **Rapid toast emission**: If 5 toasts are added before any expire, only the newest 3 are kept. The `Add()` method handles eviction.
2. **Window resize during toast**: The `toasts.SetWidth()` is called in `View()` before rendering, so it always uses current width.
3. **Toast during input mode**: Toasts render above the input area, so they remain visible while the user types.
4. **Toast during help overlay**: The help overlay replaces the entire View output. Toasts are not visible while help is open. They continue to expire via their timers, so the user won't see stale toasts after closing help.
5. **Zero-width terminal**: `View()` guards on `m.width == 0` already; toasts inherit the same guard.

### 2.12 Test Strategy

**File: `bramble/app/toast_test.go`** (new file)

```go
func TestToastManagerAdd(t *testing.T) {
    tm := NewToastManager()
    tm.Add("hello", ToastSuccess)
    assert.Equal(t, 1, tm.Count())
    assert.True(t, tm.HasToasts())
}

func TestToastManagerMaxStack(t *testing.T) {
    tm := NewToastManager()
    for i := 0; i < 5; i++ {
        tm.Add(fmt.Sprintf("toast %d", i), ToastInfo)
    }
    // Should only keep 3
    assert.Equal(t, maxToasts, tm.Count())
    // Oldest should be evicted: "toast 2", "toast 3", "toast 4" remain
}

func TestToastExpiry(t *testing.T) {
    tm := NewToastManager()
    // Manually set CreatedAt in the past
    tm.toasts = append(tm.toasts, Toast{
        Message:   "old",
        Level:     ToastSuccess,
        CreatedAt: time.Now().Add(-10 * time.Second),
        Duration:  3 * time.Second,
    })
    tm.toasts = append(tm.toasts, Toast{
        Message:   "new",
        Level:     ToastSuccess,
        CreatedAt: time.Now(),
        Duration:  3 * time.Second,
    })

    changed := tm.Tick(time.Now())
    assert.True(t, changed)
    assert.Equal(t, 1, tm.Count()) // "old" expired, "new" remains
}

func TestToastIsExpired(t *testing.T) {
    toast := Toast{
        CreatedAt: time.Now().Add(-5 * time.Second),
        Duration:  3 * time.Second,
    }
    assert.True(t, toast.IsExpired(time.Now()))

    toast2 := Toast{
        CreatedAt: time.Now(),
        Duration:  3 * time.Second,
    }
    assert.False(t, toast2.IsExpired(time.Now()))
}

func TestToastHeight(t *testing.T) {
    tm := NewToastManager()
    assert.Equal(t, 0, tm.Height())

    tm.Add("a", ToastSuccess)
    assert.Equal(t, 1, tm.Height())

    tm.Add("b", ToastError)
    assert.Equal(t, 2, tm.Height())
}

func TestToastRendering(t *testing.T) {
    tm := NewToastManager()
    tm.SetWidth(80)

    // No toasts -> empty string
    assert.Equal(t, "", tm.View())

    // Success toast
    tm.Add("Worktree created", ToastSuccess)
    view := tm.View()
    assert.Contains(t, view, "Worktree created")
    assert.Contains(t, view, "✓") // success icon

    // Error toast
    tm.Add("Failed to start session", ToastError)
    view = tm.View()
    assert.Contains(t, view, "Failed to start session")
    assert.Contains(t, view, "!") // error icon
}

func TestToastDurationByLevel(t *testing.T) {
    tm := NewToastManager()

    tm.Add("success", ToastSuccess)
    assert.Equal(t, 3*time.Second, tm.toasts[0].Duration)

    tm.toasts = nil
    tm.Add("info", ToastInfo)
    assert.Equal(t, 4*time.Second, tm.toasts[0].Duration)

    tm.toasts = nil
    tm.Add("error", ToastError)
    assert.Equal(t, 5*time.Second, tm.toasts[0].Duration)
}
```

Integration test using the full Model:

```go
func TestToastViaModelUpdate(t *testing.T) {
    ctx := context.Background()
    mgr := session.NewManager()
    defer mgr.Close()

    m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, 80, 24)

    // Simulate an error message
    newModel, cmd := m.Update(errMsg{fmt.Errorf("test error")})
    m2 := newModel.(Model)

    // Should have a toast
    assert.True(t, m2.toasts.HasToasts())
    assert.Equal(t, 1, m2.toasts.Count())

    // cmd should be a tea.Tick for expiry
    assert.NotNil(t, cmd)

    // View should contain the error message
    view := m2.View()
    assert.Contains(t, view, "test error")
}
```

---

## Feature 3: Welcome/Empty State

### 3.1 Overview

Replace the terse "No session selected" empty state with a rich, context-aware welcome screen. Two variants:

1. **No worktrees**: Guides the user to create their first worktree or task.
2. **Worktrees exist, no session selected**: Shows quick-start hints and current worktree summary.

### 3.2 New Types

No new types are needed. This feature modifies the existing `renderOutputArea()` function in `view.go`.

### 3.3 New Rendering Function

**File: `bramble/app/welcome.go`** (new file)

```go
package app

import (
    "fmt"
    "strings"

    "github.com/charmbracelet/lipgloss"
)

// Styles specific to the welcome screen
var (
    welcomeTitleStyle = lipgloss.NewStyle().
        Bold(true).
        Foreground(lipgloss.Color("12")).
        MarginBottom(1)

    welcomeKeyStyle = lipgloss.NewStyle().
        Bold(true).
        Foreground(lipgloss.Color("14"))

    welcomeDescStyle = lipgloss.NewStyle().
        Foreground(lipgloss.Color("252"))

    welcomeSummaryStyle = lipgloss.NewStyle().
        Foreground(lipgloss.Color("242")).
        MarginTop(1).
        PaddingLeft(2)
)

// renderWelcome renders the welcome/empty state for the center area.
// It adapts based on whether worktrees exist and what state they're in.
func (m Model) renderWelcome(width, height int) string {
    var b strings.Builder

    hasWorktrees := len(m.worktrees) > 0
    wt := m.selectedWorktree()
    inTmux := m.sessionManager.IsInTmuxMode()

    // Show worktree operation messages if any (e.g. "Creating worktree...")
    if len(m.worktreeOpMessages) > 0 {
        b.WriteString("\n")
        for _, msg := range m.worktreeOpMessages {
            b.WriteString("  ")
            b.WriteString(msg)
            b.WriteString("\n")
        }
        return b.String()
    }

    b.WriteString("\n")

    if !hasWorktrees {
        // Variant 1: No worktrees at all
        b.WriteString(welcomeTitleStyle.Render("  Welcome to Bramble"))
        b.WriteString("\n\n")
        b.WriteString(dimStyle.Render("  No worktrees found for " + m.repoName))
        b.WriteString("\n\n")
        b.WriteString("  Get started:\n\n")
        b.WriteString(renderKeyHint("t", "New task", "Describe what you want; AI picks the branch"))
        b.WriteString(renderKeyHint("n", "New worktree", "Create a branch manually"))
        b.WriteString(renderKeyHint("Alt-W", "Worktrees", "Browse and select worktrees"))
        b.WriteString(renderKeyHint("?", "Help", "Show all keyboard shortcuts"))
        b.WriteString("\n")
    } else {
        // Variant 2: Worktrees exist, no session selected
        b.WriteString(welcomeTitleStyle.Render("  Bramble"))
        b.WriteString("\n\n")
        b.WriteString("  Quick start:\n\n")
        b.WriteString(renderKeyHint("t", "New task", "Describe what you want; AI picks the worktree"))
        b.WriteString(renderKeyHint("p", "Plan", "Start a planning session on current worktree"))
        b.WriteString(renderKeyHint("b", "Build", "Start a builder session on current worktree"))
        if !inTmux {
            b.WriteString(renderKeyHint("Alt-S", "Sessions", "Browse and switch sessions"))
        }
        b.WriteString(renderKeyHint("?", "Help", "Show all keyboard shortcuts"))
        b.WriteString("\n")

        // Current worktree summary
        if wt != nil {
            b.WriteString(renderWorktreeSummary(m, wt))
        }

        // Session summary
        sessions := m.currentWorktreeSessions()
        if len(sessions) > 0 {
            b.WriteString("\n")
            b.WriteString(dimStyle.Render(fmt.Sprintf("  %d active session(s) on this worktree", len(sessions))))
            b.WriteString("\n")
            if !inTmux {
                b.WriteString(dimStyle.Render("  Press [Alt-S] to view them"))
            } else {
                b.WriteString(dimStyle.Render("  Press [Enter] to switch to a session window"))
            }
            b.WriteString("\n")
        }
    }

    return b.String()
}

// renderKeyHint renders a single key hint line for the welcome screen.
func renderKeyHint(key, action, description string) string {
    // Fixed-width columns for alignment:
    //   "    [t]  New task         Describe what you want; AI picks the branch"
    keyCol := fmt.Sprintf("  %s", welcomeKeyStyle.Render(fmt.Sprintf("[%s]", key)))
    // Pad key column to 14 chars visual width for alignment
    keyVisual := len(stripAnsi(keyCol))
    padding := 14 - keyVisual
    if padding < 1 {
        padding = 1
    }
    return fmt.Sprintf("%s%s%-18s %s\n",
        keyCol,
        strings.Repeat(" ", padding),
        welcomeDescStyle.Render(action),
        dimStyle.Render(description),
    )
}

// renderWorktreeSummary renders a summary of the current worktree.
func renderWorktreeSummary(m Model, wt *wt.Worktree) string {
    var b strings.Builder
    b.WriteString(dimStyle.Render("  Current worktree: "))
    b.WriteString(titleStyle.Render(wt.Branch))

    // Add status details if available
    if m.worktreeStatuses != nil {
        if status, ok := m.worktreeStatuses[wt.Branch]; ok {
            var details []string
            if status.IsDirty {
                details = append(details, failedStyle.Render("dirty"))
            } else {
                details = append(details, completedStyle.Render("clean"))
            }
            if status.Ahead > 0 {
                details = append(details, runningStyle.Render(fmt.Sprintf("↑%d ahead", status.Ahead)))
            }
            if status.Behind > 0 {
                details = append(details, pendingStyle.Render(fmt.Sprintf("↓%d behind", status.Behind)))
            }
            if status.PRNumber > 0 {
                prText := fmt.Sprintf("PR#%d %s", status.PRNumber, status.PRState)
                details = append(details, dimStyle.Render(prText))
            }
            if len(details) > 0 {
                b.WriteString(" (")
                b.WriteString(strings.Join(details, ", "))
                b.WriteString(")")
            }
        }
    }

    // File tree summary
    if m.fileTree != nil && m.fileTree.FileCount() > 0 {
        b.WriteString(dimStyle.Render(fmt.Sprintf(" -- %d files changed", m.fileTree.FileCount())))
    }

    b.WriteString("\n")
    return b.String()
}
```

### 3.4 Modification to renderOutputArea

**File: `view.go`**

Replace lines 315-334 (the empty state block in `renderOutputArea()`) with a call to the new function:

**Before** (lines 315-334):
```go
if m.viewingSessionID == "" {
    // No session selected - show worktree operation messages if any
    if len(m.worktreeOpMessages) > 0 {
        b.WriteString("\n")
        for _, msg := range m.worktreeOpMessages {
            b.WriteString("  ")
            b.WriteString(msg)
            b.WriteString("\n")
        }
        return b.String()
    }

    // Default empty state
    b.WriteString("\n")
    b.WriteString(dimStyle.Render("  No session selected"))
    b.WriteString("\n\n")
    b.WriteString(dimStyle.Render("  Choose a session with [Alt-S]"))
    b.WriteString("\n")
    b.WriteString(dimStyle.Render("  Or start a new session with [p]lan or [b]uild"))
    return b.String()
}
```

**After**:
```go
if m.viewingSessionID == "" {
    return m.renderWelcome(width, height)
}
```

This is a one-line replacement. All the logic moves to `welcome.go`.

### 3.5 Tmux Mode Variant

In tmux mode, `renderCenter()` at line 294 returns `renderSessionListView()` directly, bypassing `renderOutputArea()`. The session list view already has its own empty state (line 228-233):

```go
if len(currentSessions) == 0 {
    b.WriteString("\n")
    b.WriteString(dimStyle.Render("  No sessions for this worktree\n"))
    b.WriteString("\n")
    b.WriteString(dimStyle.Render("  Press [p] to start a planner session or [b] to start a builder session\n"))
    return b.String()
}
```

This should be enhanced similarly. Modify `renderSessionListView()` to call `renderWelcome()` when there are no sessions:

```go
if len(currentSessions) == 0 {
    return m.renderWelcome(width, height)
}
```

### 3.6 Edge Cases

1. **Narrow terminal**: The `renderKeyHint()` function uses fixed-width columns. If `width < 50`, the descriptions should be omitted (show only key + action).
2. **No repo selected**: If `m.repoName == ""`, show a message directing the user to select a repo first. This shouldn't normally happen in practice since the launcher selects a repo.
3. **Worktrees loading**: During the first render before `worktreesMsg` arrives, `m.worktrees` may be empty even though the repo has worktrees. The "no worktrees" variant will flash briefly, then be replaced when worktrees load. This is acceptable since the `deferredRefreshCmd` makes the transition very fast (1ms). If pre-populated worktrees are passed to `NewModel()`, this doesn't happen at all.
4. **Status not yet loaded**: The worktree summary gracefully handles missing status data (the `if m.worktreeStatuses != nil` check).

### 3.7 BubbleTea Message Flow

No new messages are needed. The welcome screen is purely a rendering change. It appears whenever `m.viewingSessionID == ""` and is replaced by session output as soon as the user selects or starts a session.

```
App starts with pre-loaded worktrees
  -> View() calls renderCenter() -> renderOutputArea()
  -> viewingSessionID == "" -> renderWelcome()
  -> shows "Quick start" hints + worktree summary

User presses 't' (new task)
  -> task modal opens, focus changes
  -> user submits task, session starts
  -> viewingSessionID = newSessionID
  -> renderOutputArea() now shows session output (welcome screen gone)

User presses 'd' to delete last worktree
  -> worktrees becomes empty
  -> renderWelcome() shows "No worktrees" variant
```

### 3.8 Test Strategy

**File: `bramble/app/welcome_test.go`** (new file)

```go
func TestWelcomeNoWorktrees(t *testing.T) {
    ctx := context.Background()
    mgr := session.NewManager()
    defer mgr.Close()

    // No worktrees, no sessions
    m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, 80, 24)

    view := m.renderWelcome(80, 20)

    assert.Contains(t, view, "Welcome to Bramble")
    assert.Contains(t, view, "No worktrees found")
    assert.Contains(t, view, "[t]")  // task hint
    assert.Contains(t, view, "[n]")  // new worktree hint
    assert.Contains(t, view, "[?]")  // help hint
}

func TestWelcomeWithWorktrees(t *testing.T) {
    ctx := context.Background()
    mgr := session.NewManager()
    defer mgr.Close()

    worktrees := []wt.Worktree{
        {Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
        {Branch: "fix-bug", Path: "/tmp/wt/fix-bug"},
    }
    m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, worktrees, 80, 24)

    view := m.renderWelcome(80, 20)

    assert.Contains(t, view, "Bramble")
    assert.Contains(t, view, "Quick start")
    assert.Contains(t, view, "[t]")   // task hint
    assert.Contains(t, view, "[p]")   // plan hint
    assert.Contains(t, view, "[b]")   // build hint
    assert.Contains(t, view, "feature-auth") // current worktree
}

func TestWelcomeWithWorktreeStatus(t *testing.T) {
    ctx := context.Background()
    mgr := session.NewManager()
    defer mgr.Close()

    worktrees := []wt.Worktree{
        {Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
    }
    m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, worktrees, 80, 24)
    m.worktreeStatuses = map[string]*wt.WorktreeStatus{
        "feature-auth": {
            IsDirty:  true,
            Ahead:    2,
            PRNumber: 42,
            PRState:  "OPEN",
        },
    }

    view := m.renderWelcome(80, 20)

    assert.Contains(t, view, "dirty")
    assert.Contains(t, view, "↑2")
    assert.Contains(t, view, "PR#42")
}

func TestWelcomeWithSessions(t *testing.T) {
    ctx := context.Background()
    mgr := session.NewManager()
    defer mgr.Close()

    worktrees := []wt.Worktree{
        {Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
    }
    m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, worktrees, 80, 24)

    // Add a session for this worktree
    mgr.AddSession(&session.Session{
        ID:           "sess-1",
        Type:         session.SessionTypePlanner,
        Status:       session.StatusRunning,
        WorktreePath: "/tmp/wt/feature-auth",
        Prompt:       "plan auth",
    })

    view := m.renderWelcome(80, 20)

    assert.Contains(t, view, "1 active session")
}

func TestWelcomeWorktreeOpMessages(t *testing.T) {
    ctx := context.Background()
    mgr := session.NewManager()
    defer mgr.Close()

    m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, 80, 24)
    m.worktreeOpMessages = []string{"Creating worktree feature-new..."}

    view := m.renderWelcome(80, 20)

    // Should show operation messages instead of welcome
    assert.Contains(t, view, "Creating worktree")
    assert.NotContains(t, view, "Welcome")
}

func TestWelcomeInTmuxMode(t *testing.T) {
    // Tmux mode should not show Alt-S hint
    // (would need a mock session manager with IsInTmuxMode() returning true)
}

func TestRenderOutputAreaDelegatesToWelcome(t *testing.T) {
    ctx := context.Background()
    mgr := session.NewManager()
    defer mgr.Close()

    worktrees := []wt.Worktree{
        {Branch: "main", Path: "/tmp/wt/main"},
    }
    m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, worktrees, 80, 24)
    // viewingSessionID is "" by default

    output := m.renderOutputArea(80, 20)

    // Should contain welcome content, not the old "No session selected"
    assert.NotContains(t, output, "No session selected")
    assert.Contains(t, output, "Quick start")
}
```

---

## Cross-Cutting Concerns

### File Summary

| Feature | New Files | Modified Files |
|---------|-----------|----------------|
| Help Overlay | `bramble/app/helpoverlay.go`, `bramble/app/helpoverlay_test.go` | `model.go` (FocusArea enum, Model struct, NewModel), `update.go` (Update, handleKeyPress, handleDropdownMode, WindowSizeMsg), `view.go` (View, renderStatusBar) |
| Toast Notifications | `bramble/app/toast.go`, `bramble/app/toast_test.go` | `model.go` (Model struct, NewModel, message types, remove lastError), `update.go` (all lastError sites, new toastExpireMsg handler), `view.go` (View layout, renderStatusBar remove error display) |
| Welcome/Empty State | `bramble/app/welcome.go`, `bramble/app/welcome_test.go` | `view.go` (renderOutputArea, renderSessionListView) |

### Gazelle / BUILD.bazel

All new `.go` files are in the existing `bramble/app/` package. Running `bazel run //:gazelle` will automatically pick them up and add them to the existing `go_library` and `go_test` targets in `bramble/app/BUILD.bazel`. No manual BUILD file edits needed.

New test files follow the `_test.go` naming convention and import only existing dependencies (`testing`, `github.com/stretchr/testify/assert`, `github.com/charmbracelet/lipgloss`, existing package types). No new external dependencies are introduced by any of the three features.

### Implementation Order

1. **Toast Notifications** (Feature 2) -- implement first because it replaces `lastError`, which is referenced throughout. All subsequent changes can emit toasts instead of setting `lastError`.
2. **Help Overlay** (Feature 1) -- implement second. The status bar `[?]help` hint requires the help overlay to exist.
3. **Welcome/Empty State** (Feature 3) -- implement last. It is self-contained and only modifies rendering.

This order minimizes merge conflicts and ensures each feature can be tested independently.

### Shared Patterns

All three features follow the same BubbleTea patterns established in the codebase:

- **Full-screen overlays** use `lipgloss.Place()` for centering (same as `TaskModal.View()` at `taskmodal.go:232`).
- **Focus management** uses the `FocusArea` enum and is checked in `Update()` before any key dispatch.
- **State isolation**: Each feature is a separate struct (`HelpOverlay`, `ToastManager`) owned by Model, following the `Dropdown` and `TaskModal` patterns.
- **Rendering** is done in dedicated `View()` methods on the component structs.
- **Tests** use the patterns from `output_test.go`: construct Model/component, set state, call render, assert on string content.

### No New Dependencies

All three features use only:
- `github.com/charmbracelet/lipgloss` (already imported)
- `github.com/charmbracelet/bubbletea` (already imported)
- Standard library (`time`, `strings`, `fmt`)

No new Go modules or Bazel dependencies needed. No `go mod tidy` or gazelle re-runs beyond the standard `bazel run //:gazelle` after adding new source files.
