# Product Requirements Document: Bramble

**Version:** 0.1  
**Status:** Draft  
**Last updated:** 2025-02-04

## 1. Overview

### 1.1 Purpose

**Bramble** is a **terminal UI–based multiple worktree editor system** (codebase: `tuimanager`). It lets users manage Git repositories, their worktrees, and AI-powered planning/building sessions from a single TUI. One TUI session operates on a single repo at a time; the user chooses the repo at startup (menu or CLI) and then works within that repo’s worktrees and sessions.

### 1.2 Goals

- **Single-repo focus per TUI session:** One running TUI instance = one selected repo. Repo is chosen at launch.
- **Worktree-centric workflow:** Each worktree is a Git branch (typically one PR). Users list, select, create, and remove worktrees from the TUI.
- **Session management:** Each worktree can have multiple *work sessions*. Sessions start as planner or builder (initial type) but a planning session can later become a builder session or fork to a new builder session. Sessions support an initial prompt and follow-up commands; state is clearly shown (idle vs working).
- **Unified progress and input:** Session progress and user input live in the main center area, with clear ways to switch, create, and delete sessions.
- **Persistent session history and replay:** Session history SHALL persist across app restarts. Users SHALL be able to browse past sessions and replay (view) their activity and output.

### 1.3 Non-Goals (Out of Scope for This PRD)

- Editing multiple repos in one TUI window (future: multi-window or workspace concept).
- Full Git UI (commits, diff view, etc.); use of the wt module and Git is sufficient.

---

## 2. User Personas & Context

- **Developer** using the wt (worktree) module for multi-branch workflows.
- **User** of yoloswe (planner/builder) for AI-assisted planning and coding.
- **Workflow:** One worktree ≈ one branch ≈ one PR; multiple sessions per worktree for different tasks or follow-ups.

---

## 3. Core Concepts

| Concept | Description |
|--------|-------------|
| **Repo** | A Git repository under `WT_ROOT` (e.g. `~/worktrees/repo-name`). Contains a `.bare` clone and multiple worktrees. |
| **Worktree** | A checked-out branch under a repo. One worktree = one branch; conventionally one PR. Can be created or removed from the TUI. |
| **Work session** | A single run that may do planning, building, or both. Starts with one prompt; can support follow-up commands. Has state: **idle** (waiting for input) or **working** (agent running). Session **type** is the **initial mode** (see below). |
| **Session type (initial)** | **Planner** or **Builder** describes how the session *started*, not a fixed lifetime label. A session that started as planner can later become or spawn builder work. |
| **Session history** | Persisted record of past sessions (metadata + activity/output). Survives app restarts; enables browse and replay. |

---

## 4. Functional Requirements

### 4.1 Launch & Repo Selection

- **FR-1.1** The app SHALL support choosing the working repo at startup via:
  - **Initial popup MENU:** List all repos under `WT_ROOT`; user selects one to enter the main TUI.
  - **CLI flag:** e.g. `bramble --repo <name>` to skip the menu and open directly on that repo.
- **FR-1.2** If `--repo` is provided and valid, the TUI SHALL open directly on that repo (no menu).
- **FR-1.3** If current working directory is inside a worktree-managed repo (wt layout), the app MAY pre-select or suggest that repo in the menu (optional convenience).
- **FR-1.4** A single TUI session SHALL work on exactly one repo for its lifetime. Switching repo SHALL require restarting the app (or a future “switch repo” flow that we may add later).

### 4.2 Repo and Worktree Selection

- **FR-2.1** The TUI SHALL have a **top bar** (no left panel). The top bar SHALL show **REPOSITORY** with the current repo name and **worktree selection** via a **dropdown menu** (e.g. “my-app” then current worktree, or a combined repo+worktree control). **Alt-W** SHALL open the worktree dropdown; the dropdown SHALL list all worktrees for the current repo so the user can select the “current” worktree.
- **FR-2.2** The top bar SHALL display the **current worktree** name (branch) after the repo; it MAY show session count or status summary for that worktree.
- **FR-2.3** User SHALL be able to **select** a worktree from the worktree dropdown (e.g. Alt-W then arrow keys + Enter) to make it the “current” worktree for session creation and session listing.
- **FR-2.4** User SHALL be able to **create** a new worktree in either of two ways:
  - **By branch name:** User invokes “New worktree,” enters a **specific branch name**, and the app creates that worktree via the wt module API. User can then start sessions on it manually.
  - **By prompt (new task):** User invokes “New task” (or equivalent), enters a **natural-language prompt** describing the work. An **AI module** SHALL consider existing worktrees (branches, unmerged work, relationships) and propose either (a) **use an existing worktree** and start a new **planning session** there with the prompt, or (b) **create a new worktree** (which MAY be a **child** of an existing worktree when the task depends on unmerged work) and start a **planning session** there. The user SHALL see the **outcome** (which worktree, new or existing). The user SHALL then be able to **confirm** (proceed with that worktree and start the planning session) or **make adjustments** (e.g. choose a different existing worktree, or change the new branch name / parent) and then proceed. Only after confirm or adjust-and-proceed SHALL the planning session start.
- **FR-2.5** User SHALL be able to **remove** a worktree. Removal SHALL use the wt module API and respect worktree semantics (remove worktree only, or worktree + branch when applicable). The TUI SHALL confirm before destructive remove when appropriate.

### 4.3 Work Sessions

- **FR-3.1** Each worktree SHALL have zero or more **work sessions**. Session **type** (planner vs builder) is the **initial state** only:
  - A session MAY start as **planner** (planning run from a prompt; may complete as planning-only).
  - A session MAY start as **builder** (building/coding run; one prompt + optional follow-up commands).
  - A **planning session MAY later become a builder session** (e.g. user continues with “implement this” in the same session).
  - A **planning session MAY fork to a builder session** (e.g. “Start build from this plan” creates a new builder session, possibly sharing or copying plan context).
- **FR-3.2** A session SHALL have exactly one of the following **states**:
  - **Idle:** Waiting for user input (e.g. initial prompt or follow-up). No agent activity.
  - **Working:** Agent is running (planning or building). No user input expected until completion or error.
- **FR-3.3** The TUI SHALL display session state clearly (e.g. icon or label: “idle” vs “working”) in the top bar (current session) and in the main content area.
- **FR-3.4** Sessions SHALL be **create**able from the current worktree (e.g. “Start planner” / “Start builder” with a prompt). Creating SHALL prompt for the initial prompt when not provided by another flow. The TUI SHALL support **fork to builder** from an existing planner session (new builder session, optionally seeded from the plan).
- **FR-3.5** Sessions SHALL be **delete**able (or “dismiss”able) from the UI. Running sessions SHALL be **stoppable** (cancel agent). Stopped or completed sessions MAY be removed from the list (delete/dismiss).
- **FR-3.6** The TUI SHALL provide a **good way to switch** between sessions. The top bar SHALL show the **current session** and SHALL offer a **session dropdown** (e.g. **Alt-S** to open) listing all sessions for the current worktree (live + history); selecting a session SHALL make it the “viewing” session whose output and input are shown in the center.

### 4.4 Main Center Area: Progress and Input

- **FR-4.1** The **main center area** SHALL show:
  - **Progress of the selected session:** Streaming or buffered output (logs, agent messages, tool calls, errors) for the session currently selected.
  - **Input:** When the selected session is **idle** and supports follow-up, the user SHALL be able to type and send a follow-up command/message in this area (or in a dedicated input line below it).
- **FR-4.2** Progress SHALL update in near real time when the session is **working** (e.g. via event stream from session manager).
- **FR-4.3** If no session is selected, the center area SHALL show an empty state or instructions (e.g. “Select a session” or “Start a session”).
- **FR-4.4** While the agent is **working**, the TUI SHALL **stream all agent activity** in an **intuitive** way:
  - Activity SHALL be delivered as a **live stream** (minimal delay; no need to wait for a phase to finish before showing output).
  - Activity types (e.g. thinking, tool invocations, file edits, command output, errors) SHALL be **visually distinguishable** (e.g. labels, icons, or styling) so the user can scan and understand what the agent is doing.
  - Output SHALL be **chronologically ordered** and easy to follow (e.g. scrolling log; nested or indented structure for tool-in-tool is acceptable).
  - The presentation SHALL support **at-a-glance** awareness of current step or phase (e.g. “Running tool X”, “Editing file Y”) without overwhelming the main content.

### 4.5 Session Switching, Create, Delete

- **FR-5.1** **Switch session:** User SHALL be able to change the “viewing” session via the **session dropdown** (Alt-S; select from list). The center area SHALL then show that session’s progress and allow input if that session is idle and supports follow-up.
- **FR-5.2** **Create session:** From the current worktree (chosen in the worktree dropdown), user SHALL be able to start a new planner or builder session (with prompt). New session SHALL appear in the session dropdown and MAY be auto-selected for viewing.
- **FR-5.3** **Delete / dismiss session:** User SHALL be able to remove a session from the list. If the session is working, the TUI SHALL first stop it (cancel), then allow removal. Completed/failed/stopped sessions MAY be removable without extra confirmation (configurable or by design).

### 4.6 Persistent Session History and Replay

- **FR-6.1** Session **history** SHALL **persist across app restarts**. Completed, failed, or stopped sessions SHALL be stored (e.g. under the worktree or repo) so that after restart the user can see past sessions for each worktree.
- **FR-6.2** The TUI SHALL allow **browsing** session history (e.g. list of past sessions for the current worktree, with prompt, type, status, timestamps). Past sessions MAY be grouped or filterable (e.g. by date, status).
- **FR-6.3** The user SHALL be able to **replay** (view) a past session: open its recorded activity and output in the main center area in the same intuitive form as live streaming (chronological, distinguishable activity types). Replay is read-only; no agent is running.
- **FR-6.4** Session persistence SHALL store at least: session id, worktree, type, prompt, status, timestamps, and the **full activity/output stream** needed for replay. Storage format and retention (e.g. how many sessions to keep) MAY be implementation-defined.

### 4.7 New Worktree / New Task (User Journey Summary)

- **Branch-first:** “New worktree” → enter branch name → create worktree → user starts sessions as needed.
- **Prompt-first (new task):** “New task” → enter prompt → AI proposes worktree (existing or new, optionally child). User sees outcome → **confirm** or **adjust** (e.g. different worktree, different branch/parent) → then proceed; planning session starts only after confirm or adjust-and-proceed.

### 4.8 Worktree–PR Mapping (Informational)

- **FR-8.1** The product SHALL treat **one worktree = one Git branch = one PR** as the intended workflow. The TUI need not implement PR creation/merge; it SHALL support the workflow (branch → worktree → sessions) and MAY display or link to PR info (e.g. branch name; future: PR number/URL) where available.

---

## 5. UI Layout (Target)

- **No left panel.** All selection is via the top bar and dropdowns.
- **Top bar:**
  - **REPOSITORY** with current repo name (e.g. “my-app”). **Worktree** selection via **dropdown** (Alt-W): lists all worktrees for the repo; selection defines “current worktree.” The bar SHALL show current worktree name after the repo.
  - **Current session** shown after worktree; **session dropdown** (Alt-S): lists all sessions for the current worktree (live + history); selection defines “viewing” session. Past sessions available for replay.
- **Center (main) area:** Full width below the top bar.
  - **Session output:** Scrolling, streamed view of the selected session’s progress. While the agent is working, all activity (thinking, tool calls, edits, output, errors) streams in live and is presented intuitively (clear activity types, chronological order, current-step awareness).
  - **Input line/area:** For sending initial prompt or follow-up when the selected session is idle.
- **Status bar (bottom):** Global hints (e.g. keybindings, repo name, session counts: idle vs working).

Navigation (keyboard-first): **Alt-W** worktree dropdown, **Alt-S** session dropdown; dedicated keys for “new worktree,” “new task” (prompt → AI routing), “new session” (plan/build), “stop,” “delete/dismiss” as needed.

**Input vs commands (no conflict):** When the user is **typing in the input line** (prompt or follow-up for a session), single-letter shortcuts (e.g. `p`, `b`, `n`, `t`, `s`) SHALL **not** be treated as commands; they SHALL be inserted as normal text. Only **Enter** (submit), **Esc** (cancel), and **modifier keys** (e.g. **Alt-W**, **Alt-S**, **Ctrl+C** for quit) SHALL have special meaning in the input line. Thus typing text like “please build the auth module” does not trigger plan/build actions.

---

## 6. Session State Model (Reference)

### 6.1 Runtime state (idle / working)

| State   | Meaning              | User can send input? | UI emphasis        |
|--------|----------------------|----------------------|--------------------|
| Idle   | Waiting for prompt or follow-up | Yes                  | Show input, “Ready” |
| Working| Agent is running     | No                   | Show progress, “Working” |

Optional terminal states (completed / failed / stopped) can be shown in the session dropdown and optionally allow “dismiss” or “delete” to remove from list.

### 6.2 Session type (initial mode and transitions)

- **Session type** reflects how the session *started* (planner or builder), not a permanent tag. The UI MAY show current “mode” (e.g. now in building phase) separately from initial type.
- **Transitions:** A planning session MAY transition to builder behavior in the same session (e.g. after plan is done, user sends “implement” and the session continues as builder).
- **Fork:** From a planner session (e.g. when idle or completed), the user MAY create a **new** builder session forked from that plan (new session, optionally with plan context). The original planner session remains; the forked session is a separate builder session.

---

## 7. Out of Scope / Future

- Changing repo without restart (e.g. “Switch repo” in-app).
- Full PR lifecycle (open/merge/close) inside TUI.
- Multi-repo tabs or split view in one window.

---

## 8. Acceptance Criteria (Summary)

- [ ] User can launch with repo chosen via **initial popup menu** or **CLI flag** `--repo`.
- [ ] **Top bar** shows repo and **worktree dropdown** (Alt-W); user can **select** worktree, **create**, and **remove** worktrees. Creation: **(a)** by branch name (new worktree), or **(b)** by prompt (“new task”) with AI proposal → user **confirms** or **adjusts** worktree → then planning session starts.
- [ ] **Session dropdown** (Alt-S) shows sessions for current worktree; user can **create** (planner/builder with prompt), **switch** (select to view), **stop** (if working), and **delete/dismiss** sessions.
- [ ] Session state is clearly **idle** vs **working** in list and center.
- [ ] **Main center** shows selected session’s **progress** and supports **input** (prompt/follow-up) when session is idle.
- [ ] While the agent is working, **all activity streams** into the center area in an **intuitive** way (live, distinguishable activity types, chronological, current-step visible).
- [ ] **Session history persists** across restarts; user can **browse** past sessions and **replay** (view) their activity/output in the center area.
- [ ] One worktree = one branch; product supports one-PR-per-worktree workflow.

---

## 9. References

- **wt:** Git worktree management (wt module) — repo/worktree layout under `WT_ROOT`.
- **yoloswe:** Planner and builder sessions used by Bramble for “plan” and “build” session types.
- **Current implementation:** Bramble is implemented in `tuimanager/` (BubbleTea TUI, session manager). This PRD aligns and extends the existing behavior (repo selection at startup, top bar with worktree and session dropdowns, center area for progress and input).
