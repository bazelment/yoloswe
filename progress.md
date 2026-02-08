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

## Cycle 2
**Status**: Pending

## Cycle 3
**Status**: Pending

## Cycle 4
**Status**: Pending

## Cycle 5
**Status**: Pending
