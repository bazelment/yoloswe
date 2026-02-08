# Bramble TUI - Cycle 1 Product Research Report

## 1. Competitive Analysis

### 1.1 lazygit
**What makes it great:**
- **Panel-based architecture with instant access**: Every panel (Status, Files, Branches, Commits, Stash) is always visible with focus indicated by color. Users jump to any panel by pressing its number key.
- **Context-sensitive key legend**: Bottom of the focused panel shows all available keys for the current context, updating dynamically as focus changes.
- **Type-to-filter (`/`) in every list**: Incrementally filter any list. Essential for repos with many branches.
- **Inline diff preview**: Selecting a file immediately shows the diff in the right panel without extra keypresses.
- **Undo/redo with `z`**: Most destructive actions can be undone, reducing anxiety.
- **`?` key for contextual help**: Shows keybindings relevant to the focused panel.
- **Strong consistency**: `j/k` always move, `enter` always activates, `q` always goes back. Same mental model everywhere.

**Applicable to bramble:** Contextual `?` help overlay, type-to-filter in dropdowns, instant preview on selection, consistent key semantics.

### 1.2 k9s
**What makes it great:**
- **`:` command palette**: Type `:pod`, `:deploy`, `:svc` to instantly switch views. Supports abbreviations and autocomplete.
- **`?` for context-aware help**: Shows all available keybindings for the current view, adapting dynamically to the selected resource type.
- **Breadcrumb navigation**: Header always shows the path: `cluster > namespace > resource type > resource name`.
- **Plugin system**: Custom actions defined in YAML, surfaced in the TUI with keyboard shortcuts.
- **Skins/themes per cluster**: Visual differentiation of environments via color schemes.
- **Color-coded status indicators**: Resources are green/yellow/red based on health, making scanning instant.

**Applicable to bramble:** `?` help overlay (gold standard for TUI discoverability), `:` command palette for power users, breadcrumb trail for context.

### 1.3 aider
**What makes it great:**
- **Conversational interface**: No modes to learn; just type and go. Prompt is always at the bottom.
- **`/` commands for actions**: `/add`, `/drop`, `/diff`, `/undo` -- discoverable via `/help`.
- **Auto-commit with descriptive messages**: Every AI edit is automatically committed, making undo trivial via `git reset`.
- **Lint-after-edit loop**: Runs linters and tests after every edit, auto-fixes errors in tight feedback loop.
- **Cost tracking per session**: Shows running cost prominently.
- **Zero-config entry point**: `aider <file1> <file2>` -- no setup required.

**Applicable to bramble:** `/` command system for discoverability, always-visible input prompt, auto-commit safety net, zero-config first-run.

### 1.4 claude code
**What makes it great:**
- **Single unified prompt**: No separate plan/build modes. AI decides when to plan vs execute.
- **Streaming output with tool indicators**: Shows which tools are running in real-time with elapsed time.
- **`Ctrl+Enter` to queue follow-ups**: Can queue messages while agent is still working.
- **Parallel sub-agent execution**: Up to 8 agents work simultaneously with automatic coordination.
- **Rich tool display**: Tool invocations with real-time progress, cost tracking, and turn counts.
- **`/` commands**: `/help`, `/clear`, `/compact` for session management.

**Applicable to bramble:** Unified prompt (simplify planner/builder distinction), tool display patterns (already partially adopted), permission model.

### 1.5 cursor
**What makes it great:**
- **Inline diff acceptance**: AI suggestions appear as diffs; Accept/Reject per-hunk.
- **Background agents**: Launch agents that work in separate VMs, freeing the user.
- **Composer mode**: Multi-file edits orchestrated from a single prompt.
- **Tab completion**: Predictive, contextually-aware completions.

**Applicable to bramble:** Background agent model aligns with bramble's parallel worktree concept. Diff review UX relevant for plan approval.

### 1.6 workmux (closest direct competitor)
**What makes it great:**
- **One command to create worktree + tmux window**: `workmux add feature-name` handles everything.
- **One command to merge + cleanup**: `workmux merge` merges, removes worktree, closes window, deletes branch.
- **YAML-based layout config**: `.workmux.yaml` defines tmux pane layouts and post-creation hooks.
- **Zero UI overhead**: CLI-only, no TUI. The simplicity is the feature.

**Applicable to bramble:** Bramble's TUI adds value over workmux through session monitoring, history, and AI routing. But the one-command create/merge workflow should be equally frictionless.

### 1.7 gh-dash
**What makes it great:**
- **Configurable sections**: Users define dashboard sections using GitHub filter syntax.
- **Custom keybindings with Go templates**: Bind keys to bash commands using template variables.
- **Preview pane toggle**: Key to toggle rich markdown preview of selected PR/issue.
- **Built on the same stack** (bubbletea + lipgloss + glamour): Proves bramble's tech stack can support these patterns.

**Applicable to bramble:** Configurable filter-based views, preview pane toggle for detail-on-demand.

### 1.8 continue.dev
**What makes it great:**
- **Multiple modes (Agent, Chat, Edit, Autocomplete)**: Each mode purpose-built for a workflow.
- **Model role assignment**: Configure different models for different roles.
- **Open YAML configuration**: Transparent, version-controllable.

**Applicable to bramble:** Model selection per session type, configurable provider settings.

---

## 2. Current UX Assessment

### 2.1 Strengths

1. **Performance-conscious startup**: Pre-loading worktrees and terminal size eliminates the "Loading..." flash. Deferred refresh shows branch names immediately, then loads git statuses, file trees, and history asynchronously. Two-phase worktree status (fast local git first, slow PR info from GitHub) keeps the UI responsive.

2. **Rich information density**: The worktree dropdown shows dirty/clean state, ahead/behind counts, PR number + status + review state, session count, and relative time -- all in a single subtitle line with color coding (`model.go:376-426`).

3. **Dual execution modes**: TUI mode (in-process SDK with streaming output) and tmux mode (external windows with session table) serve different workflows. Auto-detection of tmux environment is smart (`manager.go:166-169`).

4. **Session lifecycle management**: The turn-based interaction loop (running -> idle -> follow-up) is well-designed. Idle hints tell the user exactly what to do next: `"plan ready - 'a' approve & build / 'f' iterate"`. Cost and turn tracking are inline.

5. **Session persistence and replay**: Sessions are persisted to disk as JSON with full output, browsable via the session dropdown with a "History" separator. Genuinely useful for reviewing past work.

6. **Clean component architecture**: Dropdown, TextArea, FileTree, SplitPane, TaskModal are well-separated BubbleTea components with clear interfaces.

7. **Real-time tool display**: Running tools show live elapsed time with spinner animation (100ms tick), tool states update in-place (running -> complete), and cost tracking is inline in headers.

### 2.2 Weaknesses

#### Critical Issues

1. **Silent failures for impossible actions**: Pressing `p`, `b`, `e`, `s`, `f`, `a` when their conditions are not met does absolutely nothing -- no error message, no status bar update, no visual feedback. Each handler in `handleKeyPress()` returns `m, nil` when conditions are not met. This is the most frustrating UX pattern -- users have no way to know why nothing happened.

2. **No discoverability system**: No help screen, no `?` key, no onboarding flow, no tutorial. The only discoverability is the contextual status bar hints, which are truncated on narrow terminals and change without warning. ~20 key bindings across 4+ modes with no way to learn them.

3. **Task router is non-functional**: `routeTask()` (`update.go:986-1009`) always calls `MockRouteForTesting()` -- the AI routing feature is completely stubbed out. The `[t]ask` flow always proposes creating a new branch.

4. **Task modal adjust mode is broken**: `handleTaskModal()` for `TaskModalAdjust` (`update.go:958-979`) only handles Enter and Esc -- there is NO text input handling. Users cannot edit the proposed worktree/parent names despite the UI suggesting they can.

#### Major Issues

5. **No dropdown filtering**: With many worktrees or sessions, navigation is one-by-one with up/down. No type-to-filter, no search, no fuzzy matching. Unusable beyond ~15 items.

6. **Width calculation inconsistencies**: `renderTopBar()` uses `len(stripAnsi())` (byte count) instead of `runewidth.StringWidth()` (display columns). `padToSize()` in splitpane.go has the same issue. Session icons (📋, 🔨) render as 2 columns but are counted wrong, causing persistent misalignment.

7. **No onboarding/empty state guidance**: Empty state shows `"No session selected"` with hints for `[Alt-S]`, `[p]lan`, `[b]uild` -- but doesn't mention `[t]ask`, the intended primary workflow. No first-run tutorial or welcome screen.

8. **Tmux prompt escaping vulnerability**: Single quotes in prompts are not escaped in `tmux_runner.go`, causing cryptic tmux errors for prompts containing apostrophes.

9. **Repo picker misleading hint**: Shows `[ Tab: next  Enter: open  q: quit ]` but Tab is NOT bound -- only j/k/up/down work (`repopicker.go:185`).

#### Minor Issues

10. **Scroll position lost on session switch**: `m.scrollOffset = 0` on every session switch (`update.go:553`). No per-session scroll memory.

11. **No session output search**: With potentially hundreds of output lines, no way to search/filter within a session's output.

12. **No model selection**: Planner=Opus, Builder=Sonnet hardcoded (`manager.go:252-255`). No user choice.

13. **Input mode submission confusion**: `Enter` inserts newlines (web form behavior), `Ctrl+Enter` submits. Non-standard for terminal tools where Enter typically submits.

14. **No session renaming** beyond auto-generated 20-character titles from prompt.

15. **Follow-up channel buffer is 1**: Excess follow-ups error out with no queuing.

---

## 3. Prioritized Improvements

### P0 -- Must Fix

#### 3.1 Help/Keybinding Overlay (`?` key)
**Description**: Add a `?` key that shows a modal overlay with all available keybindings for the current context. Context-aware: shows different keys depending on whether user is in normal mode, has a session selected, is in dropdown mode, etc. Like k9s `?` and lazygit's panel legends.

**User story**: As a new Bramble user, I press `?` at any time to see what actions are available, so I can discover features without reading documentation.

**Priority**: P0 -- #1 barrier to adoption. Without discoverability, all other features are invisible.

**Complexity**: Low-Medium (1-2 days). Add `FocusHelp` state, `renderHelpOverlay()` function. Key hints already exist in `renderStatusBar()` -- reuse that data in a modal format with grouping and descriptions.

**Implementation notes**:
- Add `FocusHelp` to `FocusArea` in `model.go`
- Add `"?"` case in `handleKeyPress()` setting `m.focus = FocusHelp`
- Render as centered modal (same pattern as TaskModal). Show bindings grouped by: Navigation, Sessions, Worktrees, Output
- Gray out unavailable bindings based on current state
- `Esc` or `?` again to close

#### 3.2 Action Feedback for Unavailable Keys
**Description**: When a user presses a key that requires conditions not currently met, show a brief explanatory message. For example, pressing `p` with no worktree selected shows `"Select a worktree first (Alt+W)"`.

**User story**: As a user, when I press a key and nothing happens, I see a brief explanatory message so I understand what precondition I'm missing.

**Priority**: P0 -- Silent failures are the most frustrating UX pattern. Every "impossible" keypress should explain itself.

**Complexity**: Very Low (~20 lines). Each early-return in `handleKeyPress()` changes from `return m, nil` to setting `m.lastError` with a helpful message. Examples:
- `p`/`b` without worktree: `"Select a worktree first (Alt+W)"`
- `s` without session: `"No active session to stop"`
- `f` with non-idle session: `"Session is not idle (currently running)"`
- `a` without plan: `"No plan ready to approve"`
- `s` in tmux mode: `"Close tmux windows directly"` (already done)

**Implementation notes**: Use a distinct style (e.g., yellow/info) for "not available" vs red/error. Auto-clear after 3 seconds via tick.

### P0-P1

#### 3.3 Dropdown Type-to-Filter
**Description**: When a dropdown is open, typing characters filters the list to matching items. Like lazygit's `/` filter or k9s's command palette.

**User story**: As a user with 20+ worktrees, I press `Alt+W`, type `feat`, and immediately see only worktrees containing "feat".

**Priority**: P0 for worktree dropdown (primary navigation), P1 for session dropdown.

**Complexity**: Medium (1-2 days). Add `filterText` field to Dropdown, intercept letter keys in `handleDropdownMode()`, filter items in `ViewList()`. Handle backspace (clear last char), Esc (clear filter first, second Esc closes dropdown).

**Implementation notes**: Show filter text in dropdown header: `"Worktrees [feat▌]"`. Use case-insensitive `strings.Contains` matching.

### P1 -- High Priority

#### 3.4 Unified "Start Session" Flow
**Description**: Replace the separate `[p]lan` and `[b]uild` keybindings with a single `Enter` key that opens a prompt input with a session-type toggle. Keep `p`/`b` as power-user shortcuts.

**User story**: As a user, I press `Enter` on a worktree, describe what I want to do, and bramble starts the right kind of session without me needing to know the planner/builder distinction.

**Priority**: P1 -- Reduces cognitive load. The planner/builder distinction is an implementation detail leaking into UX.

**Complexity**: Medium (1-2 days). Modify `handleKeyPress()` for `Enter`, add session-type toggle to input area. Default to planner for new prompts.

#### 3.5 Welcome / Empty State with Guided Actions
**Description**: When no session is selected, show a rich empty state with quick-start instructions and inline action hints.

**User story**: As a first-time user, I see clear instructions on how to get started without external documentation.

**Priority**: P1 -- First 30 seconds determine retention.

**Complexity**: Low (0.5 days). Rewrite the empty state block in `renderOutputArea()` (`view.go:316-334`).

#### 3.6 Fix Width Calculations
**Description**: Replace all `len(stripAnsi(s))` with `runewidth.StringWidth(stripAnsi(s))` for display-column-aware width. Affects `renderTopBar()`, `padToSize()`, `renderStatusBar()`, `FileTree.Render()`.

**User story**: As a user, the UI renders correctly without misaligned columns regardless of emoji or non-ASCII content.

**Priority**: P1 -- Not blocking adoption but degrades visual quality.

**Complexity**: Very Low (~10 call sites). The `runewidth` package is already imported.

#### 3.7 Fix Task Modal Adjust Mode
**Description**: Add actual text input handling to `TaskModalAdjust` state so users can edit proposed worktree/parent names. Either fix it or remove it.

**Priority**: P1 -- The adjust UI is surfaced but broken.

**Complexity**: Low-Medium. Add text input handling or reuse TextArea component.

#### 3.8 Fix Tmux Prompt Escaping
**Description**: Escape single quotes in prompts before passing to tmux shell commands.

**Priority**: P1 -- Data loss from common input.

**Complexity**: Very Low. One-line fix: replace `'` with `'\''` in `tmux_runner.go`.

### P2 -- Nice to Have

#### 3.9 Session Output Search
**Description**: Add `Ctrl+F` or `/` to search within session output. Highlight matches, `n`/`N` to navigate.

**Priority**: P2 -- Useful for debugging but not blocking core workflows.

**Complexity**: Medium-High.

#### 3.10 Per-Session Scroll Position Memory
**Description**: Remember scroll offset per session, restore on switch.

**Priority**: P2 -- Quality of life improvement.

**Complexity**: Very Low. Add `scrollPositions map[SessionID]int` to Model.

#### 3.11 Toast Notification System
**Description**: Transient notification area for success/error messages with auto-dismiss.

**Priority**: P2 -- Replaces the single `lastError` string.

**Complexity**: Medium.

#### 3.12 Session Progress in Dropdown Subtitle
**Description**: Show turn count, cost, elapsed time in session dropdown subtitle.

**Priority**: P2 -- Decision-making aid when multiple sessions running.

**Complexity**: Very Low. Data already available in `SessionInfo.Progress`.

---

## 4. Recommended Focus for Cycle 1

Based on impact/effort analysis, implement these **3 improvements** in Cycle 1:

### Pick 1: Help/Keybinding Overlay (3.1)
**Why**: Highest-impact change for adoption. Every competitor analyzed has contextual help (`?` in k9s, panel legends in lazygit, `/help` in aider). Without this, all other improvements are less discoverable. Low-medium complexity -- read-only modal reusing existing hint data.

**Effort**: 1-2 days

### Pick 2: Action Feedback for Unavailable Keys (3.2)
**Why**: Eliminates the most frustrating pattern in the current UX -- silent failures. Pure behavioral change with no new UI components. Very low complexity (~20 lines of added messages). Combined with the help overlay, transforms "learn by guessing" into "learn by doing."

**Effort**: 0.5 days

### Pick 3: Dropdown Type-to-Filter (3.3)
**Why**: The worktree dropdown is the primary navigation element. With >10 worktrees, it becomes the bottleneck of every interaction. Filtering is table-stakes for list navigation (lazygit, k9s, fzf -- every good TUI has it). Medium complexity but high daily-use impact.

**Effort**: 1-2 days

### Why These 3 Together
They form a cohesive "ease of use" upgrade:
1. **Help overlay** answers "what can I do?"
2. **Action feedback** answers "why didn't that work?"
3. **Dropdown filter** answers "how do I find what I want quickly?"

Together, they take bramble from "power users only" to "anyone can pick it up." The bug fixes (3.6, 3.7, 3.8) should be done opportunistically as they are very low-effort, but the 3 picks above deliver the most user-visible improvement per engineering hour.

### Total Cycle 1 Estimated Effort: ~4 days

This mirrors the standard user journey: discoverability -> action -> efficiency.
