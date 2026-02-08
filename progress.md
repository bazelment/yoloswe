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
**Status**: In Progress
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

## Cycle 3
**Status**: Pending

## Cycle 4
**Status**: Pending

## Cycle 5
**Status**: Pending
