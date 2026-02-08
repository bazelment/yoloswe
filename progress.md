# Bramble TUI Improvement Progress

## Mission
Bramble is a productivity tool to speed up parallel and efficient development by leveraging git worktrees and parallel Claude/Codex/Gemini sessions.

## Improvement Methodology
Each cycle consists of 4 phases:
1. **Product Manager** (Opus) - Research similar tools, assess current state, suggest improvements
2. **Architecture Designer** (Opus) - Design software architecture for proposed improvements
3. **SWE** (Sonnet) - Implement the design with unit + integration tests
4. **Code Reviewer** (Opus) - Review implementation, identify blind spots

---

## Cycle 1
**Status**: Complete
**Focus**: TBD (pending PM analysis)

### Phase 1: Product Research
**Status**: Complete
**Findings**: Analyzed lazygit, k9s, aider, Claude Code CLI, gh-dash. Identified 10 improvements.
**Cycle 1 Focus**:
1. Help Overlay (`?` key) - Context-aware keybinding help
2. Toast Notification System - Success/error feedback for operations
3. Welcome/Empty State - Guided onboarding for first-time users

### Phase 2: Architecture Design
**Status**: Complete
**Design**: 3 new files (helpoverlay.go, toast.go, welcome.go) + modifications to model.go, update.go, view.go
**Implementation Order**: Toast → Help Overlay → Welcome State

### Phase 3: Implementation
**Status**: Complete
**Features Implemented**:
1. Toast Notification System (toast.go) - auto-dismiss success/error notifications
2. Help Overlay (helpoverlay.go) - context-aware `?` key help screen
3. Welcome/Empty State (welcome.go) - rich onboarding for new users

### Phase 4: Code Review
**Status**: Complete
**Fixes Applied**:
- Fixed toast panic on small terminal widths
- Fixed help overlay scroll not actually scrolling
- Fixed help overlay footer being scrolled off screen
- Fixed welcome key hint column misalignment
- Extracted inline styles to package-level vars

---

---

## Cycle 2
**Status**: Complete
**Focus**: Polish and correctness - completing Cycle 1 work

### Phase 1: Product Research
**Status**: Complete
**Findings**: Identified silent-failure paths, width calculation bugs, missing scroll memory

### Phase 2: Architecture Design
**Status**: Complete

### Phase 3: Implementation
**Status**: Complete
**Features**:
1. Action Feedback - Toast notifications for all 10 silent-failure key paths
2. Per-Session Scroll Position Memory - Save/restore scroll on session switch
3. Width Calculation Fixes - Replace len(stripAnsi()) with runewidth.StringWidth()

### Phase 4: Code Review
**Status**: Complete
**Fixes Applied**:
- Fixed filetree.go truncation corrupting ANSI escape sequences
- Fixed truncate() using byte-length on multi-byte UTF-8
- Fixed truncatePath() using byte-length for column limits
- Fixed confirmTask() discarding toast expiry command
- Fixed generateDropdownTitle() using byte-length for column fitting

---

## Cycle 3
**Status**: Complete
**Focus**: Code quality refactors and dropdown filtering

### Phase 1-2: PM + Architecture
**Picks**: Scroll rendering helper, TextArea key handler extraction, dropdown search/filtering

### Phase 3: Implementation
**Features**:
1. Shared renderScrollableLines() - eliminated ~130 lines of duplicated scroll logic
2. Shared TextArea.HandleKey() - eliminated ~216 lines of duplicated key handling
3. Dropdown type-to-filter - case-insensitive search in worktree/session dropdowns

### Phase 4: Code Review
**Fixes**: Phantom "0 more lines" indicator, ClearFilter losing selected item

---

## Cycle 4
**Status**: Complete
**Focus**: Making existing data actionable

### Phase 1-2: PM + Architecture
**Picks**: Session progress in dropdown, aggregate cost in status bar, file tree enter-to-open

### Phase 3: Implementation
**Features**:
1. Session progress in dropdown - T:{turns} ${cost} {elapsed} | {prompt}
2. Aggregate cost in status bar - always-visible Cost: $X.XXXX
3. File tree enter-to-open - Enter key opens selected file in editor

### Phase 4: Code Review
**Fixes**: runewidth for subtitle budget, path traversal prevention in AbsSelectedPath,
zero-CreatedAt guard, float64 tolerance comparison, added path traversal test

---

## Cycle 5
**Status**: Complete
**Focus**: UX polish - keyboard ergonomics and safety

### Phase 1-2: PM + Architecture
**Picks**: Unified submit (Enter=send), confirmation before quit, quick session switch 1-9

### Phase 3: Implementation
**Features**:
1. Unified Submit - Enter submits prompt (non-empty), Shift+Enter inserts newline, Ctrl+Enter still submits
2. Confirmation Before Quit - `q` with active sessions shows confirmation toast, second `q`/`y` confirms
3. Quick Session Switch - bare digit keys 1-9 jump to Nth session, numbered labels in dropdown

### Phase 4: Code Review
**Fixes**:
- Fixed `TestQuitConfirm_CompletedSessions_DontCount` not actually testing with completed sessions
- Replaced duplicated session filtering with `currentWorktreeSessions()` in digit handler and tmux list
- Added number prefixes to tmux session list view for discoverability

---

## Summary of All 5 Cycles

| Cycle | Focus | Features | Review Fixes |
|-------|-------|----------|-------------|
| 1 | Discoverability | Help overlay, toast notifications, welcome screen | 5 fixes (panic, scroll, alignment, styles) |
| 2 | Correctness | Action feedback, scroll memory, runewidth | 5 fixes (ANSI corruption, byte-length bugs) |
| 3 | Code quality | Scroll helper, HandleKey extraction, dropdown filter | 2 fixes (phantom indicator, filter selection) |
| 4 | Actionable data | Session progress, cost tracking, file open | 5 fixes (runewidth, path traversal, float comparison) |
| 5 | UX polish | Unified submit, quit confirm, quick switch | 3 fixes (test quality, DRY, tmux numbers) |

**Total**: 15 features implemented, 20 review fixes applied across 5 cycles.
