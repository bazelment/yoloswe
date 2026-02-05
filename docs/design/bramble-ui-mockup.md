# Bramble — UI Mockup

This document describes the target UI layout and key screens for **Bramble** (TUI worktree + session manager). It aligns with [../prd/tuimanager.md](../prd/tuimanager.md).

---

## 1. Screen flow

1. **Launch** → Repo picker (menu or skip via `bramble --repo <name>`).
2. **Main TUI** → Top bar (REPOSITORY + worktree dropdown, current session + session dropdown); full-width center: session output + input; bottom: status bar. No left panel.
3. **Replay** → Same layout; user picks a past session from the session dropdown (Alt-S); center shows recorded output (read-only).

---

## 2. Repo picker (initial screen)

Shown when no `--repo` flag is provided. User selects one repo to enter the main TUI.

```
┌─────────────────────────────────────────────────────────┐
│  Bramble — Choose repository                             │
├─────────────────────────────────────────────────────────┤
│                                                           │
│  Select a repo (↑/↓ then Enter):                         │
│                                                           │
│    > my-app                                               │
│      backend-services                                     │
│      frontend-monore                                      │
│      yoloswe                                               │
│                                                           │
│  [ Tab: next  Enter: open  q: quit ]                      │
└─────────────────────────────────────────────────────────┘
```

- List: all repos under `WT_ROOT` (from wt module).
- One repo per line; current selection highlighted.
- Enter opens main TUI for that repo.

---

## 3. Main TUI layout

Single full-screen layout: **top bar** (repo + worktree dropdown, current session + session dropdown), **full-width center** (output + input), **status bar**. No left panel.

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│ REPOSITORY  my-app  ▼   feature-auth   │   📋 def-456  ● working  ▼   [Alt-W] [Alt-S] │
├──────────────────────────────────────────────────────────────────────────────────┤
│ SESSION OUTPUT                                                                     │
│ ─────────────────────────────────────────────────────────────────────────────────  │
│   📋 planner  def-456  ● working                                                   │
│   "implement the OAuth callback"                                                   │
│                                                                                    │
│   💭 Thinking...                                                                   │
│   🔧 run_terminal_cmd  git status                                                  │
│   ✓ Command completed                                                              │
│   🔧 read_file  src/auth/login.go                                                  │
│   ...                                                                               │
│   [ scrollable stream; live updates when working ]                                  │
│                                                                                    │
├──────────────────────────────────────────────────────────────────────────────────┤
│ ┌────────────────────────────────────────────────────────────────────────────┐   │
│ │ Plan prompt:                                                                │   │
│ │ Implement OAuth authentication with Google and GitHub providers._          │   │
│ │                                                                             │   │
│ │                                              [ Send ]    [ Cancel ]         │   │
│ └────────────────────────────────────────────────────────────────────────────┘   │
├──────────────────────────────────────────────────────────────────────────────────┤
│ [Tab] Switch  [Enter] Select                                       Running: 1   │
└──────────────────────────────────────────────────────────────────────────────────┘
```

### 3.1 Top bar

- **REPOSITORY** + current repo name (e.g. `my-app`) with a **dropdown** (▼). **Alt-W** opens the **worktree dropdown**: list of all worktrees for the repo (main, feature-auth, fix-nav-bug, …). Selection sets "current worktree"; the bar shows that worktree name (e.g. `feature-auth`) after the repo.
- After the worktree, the **current session** is shown (e.g. type icon + short id + status: `📋 def-456  ● working`). A **dropdown** (▼) is available; **Alt-S** opens the **session dropdown**: list of all sessions for the current worktree (live + history). Selection sets "viewing" session; its output and input are shown in the center.
- Optional: inline hint `[Alt-W]` / `[Alt-S]` in the bar or in status line.

### 3.2 Center (main) area

- **Session header (when one selected):** Type icon, short id, status; truncated prompt.
- **Output region:** Scrollable, streamed log. When agent is working:
  - Lines clearly typed (💭 thinking, 🔧 tool, ✓/✗ result, error styling).
  - Chronological order; optional indentation for nested tool calls.
  - Live updates (no batching delay).
  - **Scrolling:** Use **↑/↓** or **j/k** to scroll line by line, **PgUp/PgDn** to scroll by page, **Home** to jump to start, **End** to jump to latest output.
  - When scrolled up, a hint shows "↑ N more lines (press End to jump to latest)".

### 3.2.1 Input area (modal dialog style)

When the user types a prompt or follow-up, an **input area** appears with focusable buttons like a modal dialog.

```
├──────────────────────────────────────────────────────────────────────────────────┤
│ ┌────────────────────────────────────────────────────────────────────────────┐   │
│ │ Plan prompt:                                                                │   │
│ │ Implement OAuth authentication with Google and GitHub providers._          │   │
│ │                                                                             │   │
│ │                                           [ Send ]    [ Cancel ]            │   │
│ └────────────────────────────────────────────────────────────────────────────┘   │
├──────────────────────────────────────────────────────────────────────────────────┤
```

**Features:**
- **Tab-focusable buttons:** Press **Tab** to cycle focus between the text input, Send button, and Cancel button. The focused button is highlighted.
- **Word wrapping:** Long text wraps automatically to fit the box width.
- **Dynamic height:** The box grows as content is added.
- **Visual border:** Distinguished from the output area with a rounded border.

**Keyboard navigation:**

| Key            | Action                                      |
|----------------|---------------------------------------------|
| **Tab**        | Cycle focus: text → Send → Cancel → text    |
| **Shift+Tab**  | Cycle focus backward                        |
| **Enter**      | Activate focused element (send or cancel)   |
| **Esc**        | Cancel / close input                        |
| **Backspace**  | Delete character (when text is focused)     |
| **←/→**        | Move cursor (when text is focused)          |

**Long prompt handling:**
- Very long prompts are displayed with word wrap.
- The input box shows a truncated preview in the session header after submission (e.g., first 100 chars + "...").

### 3.3 Status bar (bottom)

- Left: Contextual key hints (e.g. `[p]lan  [b]uild  [n]ew wt  [t]ask  [s]top  [q]uit`).
- Right: Session counts (e.g. `Running: 1  Idle: 2`).

---

## 4. Focus and navigation

| Focus area         | Keys (example)     | Action / note                                    |
|--------------------|--------------------|--------------------------------------------------|
| Worktree dropdown  | **Alt-W**, ↑/↓, Enter | Open worktree list; select current worktree    |
| Session dropdown   | **Alt-S**, ↑/↓, Enter | Open session list; select viewing session       |
| Center (output)    | —                  | Scroll / view; focus moves to input when typing  |
| Input area         | Type, Tab, Enter | Compose prompt, Tab to buttons, Enter to activate |

- **Alt-W:** Open worktree dropdown (list of worktrees for current repo).
- **Alt-S:** Open session dropdown (list of sessions for current worktree, live + history).
- **Global:** `q` / Ctrl+C quit; `r` refresh repos/worktrees where applicable.

### 4.1 No conflict when typing (input area)

When focus is in the **input area** (user is typing a prompt or follow-up), **single-letter shortcuts do not fire**. Only the following have special meaning:

| Key           | In input area                                  |
|---------------|------------------------------------------------|
| **Tab**       | Cycle focus: text input → Send → Cancel        |
| **Shift+Tab** | Cycle focus backward                           |
| **Enter**     | Activate focused element (submit or cancel)    |
| **Esc**       | Cancel / close input                           |
| **Alt-W**     | Open worktree dropdown (optional: may blur input and open dropdown) |
| **Alt-S**     | Open session dropdown (optional) |
| **Ctrl+C**    | Quit app (or cancel input, implementation choice) |

All other keys—including **p**, **b**, **n**, **t**, **s**, **q**—are **inserted as normal characters**. So typing "please build the auth module" does not trigger plan or build; only when focus is **not** in the input area do those keys act as commands. This avoids conflict between composing feedback and invoking actions.

---

## 5. New worktree / new task — user journey

Two ways to create work or a worktree: **branch-first** (explicit branch name) or **prompt-first** (AI decides existing vs new worktree and starts planning).

### 5.1 Branch-first: "New worktree" (explicit branch)

User knows the branch name. Flow:

1. **n** (new worktree) from anywhere.
2. Prompt: **"Branch name: "** → user types e.g. `feature-oauth`.
3. App creates worktree via wt module API; worktree dropdown refreshes; new worktree becomes current (shown in top bar).
4. User starts a session with **p** or **b** as usual.

```
┌─────────────────────────────────────────────────────────┐
│ Branch name: feature-oauth_                              │
│ [ Enter: create  Esc: cancel ]                           │
└─────────────────────────────────────────────────────────┘
```

### 5.2 Prompt-first: "New task" (AI routing)

User describes intent in natural language. AI (with context of existing worktrees) proposes using an existing worktree or creating a new one (optionally a child branch). User sees the outcome, then **confirms** or **adjusts** (e.g. pick different worktree, or edit branch/parent) and proceeds; the planning session starts only after that.

**Step 1 — Invoke and describe**

- **t** (new task) from anywhere; uses **current worktree** (from Alt-W dropdown) as context.
- Opens multi-line input area: **"Describe what you want to work on:"**

```
┌─────────────────────────────────────────────────────────────────┐
│ New task — Describe what you want to work on:                    │
│ ┌───────────────────────────────────────────────────────────┐   │
│ │ Add OAuth login with Google and GitHub providers.          │   │
│ │ Need to support both web and mobile clients with proper    │   │
│ │ token refresh handling and session management._            │   │
│ │                                                             │   │
│ │                                        [ Continue ]    [ Cancel ] │
│ └───────────────────────────────────────────────────────────┘   │
│ [Tab] Switch focus    [Enter] Select                             │
└─────────────────────────────────────────────────────────────────┘
```

**Step 2 — AI routing (brief feedback)**

- App invokes **AI module** with: user prompt + repo context + existing worktrees (names, branches, unmerged state, relationships).
- AI decides:
  - **Option A:** Use **existing worktree** (e.g. `feature-auth`) and start a **new planning session** there with the prompt.
  - **Option B:** **Create new worktree** (e.g. child of `feature-auth` if task depends on unmerged work), then start a **planning session** there.
- While deciding, TUI shows a short "Deciding where to run this…" or "Checking worktrees…" state (spinner or one-line message).

**Step 3 — Show outcome**

- AI proposes one of:
  - **Option A:** Use **existing worktree** (e.g. `feature-auth`) for a new planning session with the prompt.
  - **Option B:** **Create new worktree** (e.g. `feature-oauth-refactor` from `feature-auth`) and start a planning session there.
- TUI shows the outcome clearly (which worktree, and parent if new). **No session has started yet.**

```
┌─────────────────────────────────────────────────────────┐
│ Proposed: Use existing worktree feature-auth              │
│   → Start planning session with your prompt there.       │
│                                                          │
│   [ Enter: confirm  a: adjust  Esc: cancel ]              │
└─────────────────────────────────────────────────────────┘
```

or

```
┌─────────────────────────────────────────────────────────┐
│ Proposed: Create worktree feature-oauth-refactor         │
│   from feature-auth → start planning session there.      │
│                                                          │
│   [ Enter: confirm  a: adjust  Esc: cancel ]              │
└─────────────────────────────────────────────────────────┘
```

**Step 4 — Confirm or adjust, then proceed**

- **Confirm (Enter):** Use the proposed worktree (no new worktree created if proposal was "use existing"; create worktree if proposal was "create new"). Start the planning session. Selection and center update to the session.
- **Adjust (a):** Let the user change the plan before proceeding.
  - If proposal was **existing worktree:** Show list of existing worktrees; user picks one (or keeps current). Then confirm → start planning session on chosen worktree.
  - If proposal was **new worktree:** Let user edit **branch name** and/or **parent worktree** (e.g. pick another base). Then confirm → create worktree with those values and start planning session there.
- **Cancel (Esc):** Abandon the flow; no worktree created, no session started.

Only after **confirm** or **adjust then confirm** does the planning session start.

**Design notes**

- User always sees **which** worktree is proposed (existing or new, and parent if new is a child).
- User can **confirm** to accept the proposal or **adjust** (different worktree, or different branch/parent for new) and then proceed.
- Planning session starts **only after** the user confirms or completes an adjustment.
- If the AI module is unavailable, the app MAY fall back to "New worktree (branch name)" or show an error and suggest creating a branch manually.

### 5.3 Key actions (summary)

*All single-letter keys below apply only when focus is **not** in the input area; when typing feedback, they are normal characters (see §4.1).*

| Key / action   | Context        | Effect                                                  |
|----------------|----------------|---------------------------------------------------------|
| **Alt-W**      | Global         | Open **worktree dropdown** (select current worktree)    |
| **Alt-S**      | Global         | Open **session dropdown** (select viewing session)      |
| **n**          | Global         | New worktree by **branch name** (prompt for name)       |
| **t**          | Global         | **New task** (prompt) → AI proposes → confirm or adjust → planning session |
| **p**          | Global         | Start planner session (prompt) on **current worktree**  |
| **b**          | Global         | Start builder session (prompt) on **current worktree**   |
| **s**          | Global / session | Stop running session (e.g. current or selected session) |
| **Delete/d**   | Session dropdown | Dismiss/delete session from list                        |
| **Fork**       | Planner session| "Fork to builder" (new builder session)                  |

---

## 6. Replay mode (past session)

- User opens the **session dropdown** (Alt-S) and selects a **past** session (from history / persisted sessions).
- Center shows the **same output layout** as live, but:
  - Content is from stored history (no live stream).
  - No input area (read-only); status shows "Replay" or similar.
- Same visual treatment of activity types (thinking, tool, result, error) for consistency.

---

## 7. Empty / no selection states

- **No worktree selected:** Use Alt-W to open worktree dropdown and pick one; center can show "Choose worktree (Alt-W)."
- **No session selected:** Center shows "Choose a session (Alt-S)" or "Start a session with [p] or [b]."
- **No repos:** Repo picker shows "No repos found in WT_ROOT" and hint to add repos via wt.

---

## 8. Not in scope for this mockup

- Exact pixel dimensions or font sizes.
- Theming (colors) beyond "clear distinction of activity types."
- Mouse support (keyboard-first).

For full requirements and session model, see [../prd/tuimanager.md](../prd/tuimanager.md).
