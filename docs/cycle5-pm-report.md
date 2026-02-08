# Cycle 5 PM Report -- Bramble TUI Final Cycle

## Summary of All Cycles

Over four cycles, Bramble's TUI has evolved from a functional but rough tool into a polished, discoverable interface:

- **Cycle 1**: Foundation UX -- Help overlay (`?` key), toast notifications (auto-dismiss success/error feedback), and welcome/empty-state screen (guided onboarding). These gave users a self-service way to learn the tool and see immediate feedback from every action.

- **Cycle 2**: Polish and correctness -- Added toast feedback to all 10 silent-failure key paths (pressing `f` with no idle session, pressing `b` with no worktree, etc.), per-session scroll position memory (switching sessions preserves scroll state), and width calculation fixes (replaced byte-length calculations with `runewidth.StringWidth` for correct CJK/emoji handling).

- **Cycle 3**: Code quality and filtering -- Extracted shared `renderScrollableLines()` (eliminated ~130 lines of duplicated scroll logic), shared `TextArea.HandleKey()` (eliminated ~216 lines of duplicated key handling), and dropdown type-to-filter (case-insensitive search in worktree/session dropdowns).

- **Cycle 4**: Making data actionable -- Session progress in dropdown (turns, cost, elapsed time), aggregate cost in status bar (always-visible `Cost: $X.XXXX`), and file tree enter-to-open (Enter key opens selected file in the configured editor).

The remaining item from the original PM report is **3.9 Unified Submit Behavior** (Enter=submit, Shift+Enter=newline).

---

## Cycle 5 Recommended Picks

After reading the current codebase, I identified three high-impact improvements for this final cycle. These are ordered by impact and address the most meaningful UX friction points remaining.

---

### 5.1 Unified Submit Behavior (Enter=submit, Shift+Enter=newline)

**Description**

Today, the TextArea component uses `Ctrl+Enter` to submit and `Enter` to insert a newline. This is counterintuitive: the vast majority of chat/prompt interfaces (Slack, Discord, ChatGPT, Claude.ai, GitHub Copilot Chat) use `Enter` to submit and `Shift+Enter` for newlines. Since the overwhelming majority of prompts in Bramble are single-line (plan/build prompts, follow-ups, task descriptions), the current mapping forces an extra modifier keystroke on every submission.

The change: swap `Enter` to submit (when focus is on text input and content is non-empty) and `Shift+Enter` to insert a newline. When the text area is empty, `Enter` should be a no-op (prevent accidental empty submissions). The `Ctrl+Enter` binding should remain as an alternative submit for users who expect it.

**User story**

As a developer entering prompts, I want Enter to submit my prompt immediately (matching the convention from ChatGPT, Slack, and other tools I use daily), so I do not have to remember a non-standard Ctrl+Enter binding.

**Priority**: P0 -- This was the last remaining item from the original PM report, and it addresses the single most frequent interaction in the entire TUI (typing and submitting prompts).

**Complexity**: Low -- Only `textarea.go` HandleKey method changes; the button-focus behavior already works correctly.

**Files to modify**:
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/textarea.go` -- Swap `enter` (text input focus) from InsertNewline to Submit; add `shift+enter` as InsertNewline.
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/textarea_test.go` -- Update tests for new key mapping.
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/view.go` -- Update status bar hint from `[Ctrl+Enter] Send` to `[Enter] Send, [Shift+Enter] Newline`.
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/helpoverlay.go` -- Update help text in `buildHelpSections` for Input Mode section.

---

### 5.2 Confirmation Before Quit

**Description**

Pressing `q` in normal mode immediately exits Bramble with no confirmation, even when sessions are actively running. This is dangerous: a single accidental keystroke can kill running planner/builder sessions mid-work, potentially losing in-progress output. Every comparable TUI tool (lazygit, k9s, htop) either confirms before quitting when there is active work, or at least warns.

The change: when `q` is pressed and there are running or idle sessions, show a confirmation prompt ("N sessions still active. Quit? [y/n]"). When no sessions are active, quit immediately as before. `Ctrl+C` should always force-quit without confirmation (escape hatch).

**User story**

As a developer running multiple builder sessions, I want Bramble to confirm before quitting when sessions are active, so that I do not accidentally lose in-progress work by hitting `q` instead of another key.

**Priority**: P0 -- Data loss prevention. This is the kind of improvement that prevents a catastrophic-feeling moment (losing 10 minutes of AI work to a mispress).

**Complexity**: Low -- Add a conditional check before `tea.Quit` in the `q` key handler; reuse existing `promptInput` for confirmation.

**Files to modify**:
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/update.go` -- In `handleKeyPress`, wrap the `q` case with a session-count check and confirmation prompt.
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/update_feedback_test.go` or a new test file -- Test that `q` with active sessions prompts, and `q` without sessions quits immediately.

---

### 5.3 Keyboard Shortcut for Quick Session Switch (Ctrl+1..9)

**Description**

Currently, switching between sessions requires opening the session dropdown (Alt+S), navigating with arrows, and pressing Enter -- a 3-step flow. When a user is actively juggling 2-3 sessions (a planner and a builder, or multiple builders on different worktrees), this friction adds up. Power users of tools like tmux (`prefix+0..9`), browsers (`Ctrl+1..9`), and terminal multiplexers expect numeric shortcuts for quick tab/session switching.

The change: bind `Ctrl+1` through `Ctrl+9` (or `1`..`9` since single digits are not currently mapped) to directly switch to the Nth session in the current worktree's session list. The number should correspond to the session's position in the dropdown. Show the index numbers in the session dropdown items (e.g., `1. Plan: fix auth bug`, `2. Build: implement feature`).

Since single-character keys `1`-`9` are unused in normal mode and there is no text-input context to conflict with, using bare digits is the simpler and more ergonomic option.

**User story**

As a developer running multiple sessions, I want to press a single digit key to jump directly to a specific session, so that I can monitor and switch between sessions without opening the dropdown.

**Priority**: P1 -- Workflow speed. Less critical than the P0 picks but high-value for the multi-session power-user workflow that Bramble is built for.

**Complexity**: Medium -- Requires mapping digit keys to sessions, showing index numbers in the dropdown, and handling the edge case where the digit exceeds the session count.

**Files to modify**:
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/update.go` -- Add `1`..`9` cases in `handleKeyPress` to switch to the Nth session.
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/model.go` -- In `updateSessionDropdown`, prefix dropdown labels with index numbers.
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/helpoverlay.go` -- Add `1..9` binding to the Navigation or Sessions help section.
- `/home/ming/worktrees/yoloswe/feat/ue-improve-loop/bramble/app/view.go` -- Optionally show session numbers in the status bar.

---

## Recommendation

Implement all three picks in this order:

1. **5.1 Unified Submit Behavior** -- Low complexity, highest daily-use impact. Every prompt submission becomes one keystroke faster.
2. **5.2 Confirmation Before Quit** -- Low complexity, prevents data loss. Simple safety net.
3. **5.3 Quick Session Switch** -- Medium complexity, power-user delight. Makes the multi-session workflow feel native.

Together, these three changes complete the original PM report backlog (5.1), add essential safety (5.2), and round out the power-user workflow (5.3) -- a fitting finale for the improvement cycles.
