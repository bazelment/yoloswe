# Bramble

Bramble is a terminal UI for managing AI-assisted software engineering workflows. It orchestrates multiple parallel AI sessions across git worktrees, supporting both an interactive TUI and background tmux execution modes.

![Bramble overview](docs/screenshots/bramble-overview.png)
<!-- TODO: Add screenshot of main TUI view showing worktree list and session output -->

## Key Features

- **Dual execution modes** — run AI sessions in-process (TUI mode) or in background tmux windows
- **Multi-provider support** — Claude, Codex, and Gemini backends with auto-detection
- **Worktree management** — create, switch, sync, and delete git worktrees from the UI
- **Parallel sessions** — run planners and builders side-by-side on the same worktree
- **Multi-repo support** — manage sessions across multiple repositories in a single instance
- **Session persistence** — full JSONL recording with history browsing and replay
- **Cost tracking** — per-session token counts and USD estimates
- **IPC interface** — CLI commands to create sessions, send notifications, and integrate with external tools

## Installation

### Quick install (Linux / macOS)

```bash
curl -fsSL https://raw.githubusercontent.com/bazelment/yoloswe/main/scripts/install.sh | bash
```

This installs both `bramble` and `wt` to `~/.local/bin`.

### Homebrew

```bash
brew install bazelment/tap/bramble
brew install bazelment/tap/wt
```

### Install script options

```bash
# Install only bramble
curl -fsSL https://raw.githubusercontent.com/bazelment/yoloswe/main/scripts/install.sh | bash -s -- --tool bramble

# Install a specific version
curl -fsSL https://raw.githubusercontent.com/bazelment/yoloswe/main/scripts/install.sh | bash -s -- --version v2026.03.29

# Install to a custom directory
curl -fsSL https://raw.githubusercontent.com/bazelment/yoloswe/main/scripts/install.sh | bash -s -- --dir /usr/local/bin
```

## Quick Start

```bash
bramble                              # auto-detect mode (TUI or tmux)
bramble --session-mode tui           # force TUI mode
bramble --session-mode tmux          # force tmux mode
```

## Session Modes

### TUI Mode

TUI mode runs AI sessions in-process and renders output directly in the terminal using a rich BubbleTea-based interface. This is the default when not inside a tmux session.

![TUI mode](docs/screenshots/tui-mode.png)
<!-- TODO: Add screenshot of TUI mode showing session output with markdown rendering -->

**When to use:** Interactive work where you want to see AI output in real time and send follow-up prompts inline.

### Tmux Mode

Tmux mode launches each AI session in its own tmux window. Bramble manages the window lifecycle and monitors session state from the TUI.

![Tmux mode](docs/screenshots/tmux-mode.png)
<!-- TODO: Add screenshot of tmux mode with multiple windows visible -->

**When to use:** Running multiple long-lived sessions in parallel, especially when you want to switch between them or let them run in the background.

Key tmux features:
- Automatic tmux window creation per session
- Window monitoring and idle detection
- Pane capture for inspecting session output from the TUI (`[v]` in command center)
- Notifications via visual bell when sessions need attention
- Windows remain open on error for debugging

## UI Overview

### Main View

The main view shows the selected worktree's session output with a status bar and navigation controls.

![Main view](docs/screenshots/main-view.png)
<!-- TODO: Add screenshot of main view with session output, status bar, and worktree info -->

### Command Center (`Alt-C`)

A full-screen dashboard showing all sessions across all worktrees. Press `[v]` to toggle inline preview of tmux pane content, or `[p/b/c]` to start a new planner/builder/codetalk session on the selected worktree.

![Command center](docs/screenshots/command-center.png)
<!-- TODO: Add screenshot of command center with session list and preview panel -->

### Session Types

- **Planner** (`p`) — AI planning sessions for task decomposition and design
- **Builder** (`b`) — AI implementation sessions that write code

### Keybindings

| Key | Action |
|-----|--------|
| `?` | Help overlay |
| `Alt-R` | Switch repository |
| `Alt-W` | Switch worktree |
| `Alt-S` | Switch session |
| `Alt-C` | Command center |
| `p` | New planner session |
| `b` | New builder session |
| `e` | Open worktree in editor |
| `t` | Stop current session |
| `f` | Fetch from origin |
| `g` | Sync worktree (rebase onto base branch) |
| `d` | Delete worktree |
| `w` | Refresh worktree list |
| `q` | Quit |

![Help overlay](docs/screenshots/help-overlay.png)
<!-- TODO: Add screenshot of the help overlay showing all keybindings -->

## Multi-Repo Support

Bramble can manage sessions across multiple repositories. Use `Alt-R` to switch between repos. Each repo maintains its own worktree list, session state, and configuration.

![Multi-repo dropdown](docs/screenshots/multi-repo.png)
<!-- TODO: Add screenshot showing the repo dropdown with multiple repos -->

## IPC Commands

A running Bramble instance exposes a Unix domain socket for external integration:

```bash
# Check if Bramble is running
bramble ping

# Create a new session
bramble new-session --type builder --branch feature/my-task --prompt "Implement X"

# Create a session on a specific repo
bramble new-session --type planner --repo my-other-repo --prompt "Design Y"

# Create a session with a new worktree
bramble new-session --type builder --create-worktree --branch feature/foo --from main

# List active sessions
bramble list-sessions

# Capture text from a tmux session pane
bramble capture-pane

# Notify Bramble that a session needs attention
bramble notify
```

## Subagents

A session can spawn another session as its **subagent**. Pass `--parent`; inside
a Bramble-spawned session it defaults to `$BRAMBLE_SESSION_ID`, so the flag is
usually implicit.

```bash
# Spawn a Codex subagent on the parent's own worktree
bramble new-session --type codetalk --model gpt-5.5 --prompt "Explain the session lifecycle"

# Give it a worktree of its own instead
bramble new-session --type builder --create-worktree --branch fix/foo --prompt "..."

# Spawn a top-level session even from inside one
bramble new-session --no-parent --type planner --prompt "..."

# See just your own subagents
bramble list-sessions --parent=
```

With no `--branch` and no `--worktree`, a subagent works on its parent's
worktree — the usual "helper on the same branch" case.

When a subagent finishes a turn, Bramble delivers a one-line report to its
parent naming a file with the subagent's full output:

```
[bramble] subagent <id> (codetalk, gpt-5.5) is idle
result: /tmp/bramble-research/<id>.md
```

Backends differ in how Bramble learns a turn ended: Claude and Codex are given a
completion hook, and Cursor — which has neither a notify flag nor a working CLI
hook — has its idleness read off its pane. Gemini and Agy have neither, so a
subagent on those reports only when its window closes.

The report is generated from Bramble's own view of the session, so it arrives
whatever backend the subagent ran and whether or not the agent inside
cooperated — which is what makes Codex, Gemini, Cursor and Agy usable as
subagents, none of which can be given a reporting instruction as reliably as
Claude. A subagent that messages its parent itself replaces the generated
report rather than being duplicated by it.

### Messaging a session

`send-input` writes into a session's tmux pane immediately. That is right for a
deliberate interrupt, but if the recipient is mid-turn the text lands in its
*next* prompt, stripped of the context that made it make sense — and a TUI-mode
session has no pane at all.

`--queue` holds the message until the recipient is idle, then delivers it by
whichever path its runner supports:

```bash
bramble send-input --session-id <id> --queue --submit --text "also check the tests"
```

Queued messages are persisted under `~/.bramble/deliveries/`, so they survive a
Bramble restart, and are delivered one per idle transition — a delivery starts
the recipient's next turn, so the rest wait for the one after it.

## Configuration

Settings are stored in `~/.bramble/settings.json`:

```json
{
  "theme_name": "dark",
  "enabled_providers": ["claude", "codex", "gemini"],
  "repos": {
    "my-repo": {
      "on_worktree_create": ["./scripts/setup-worktree.sh"],
      "on_worktree_delete": ["./scripts/cleanup-worktree.sh"]
    }
  }
}
```

### Themes

Switch between available themes with a live preview from the theme picker.

![Theme picker](docs/screenshots/theme-picker.png)
<!-- TODO: Add screenshot of the theme picker with preview -->

### Per-Repo Hooks

Configure shell commands that run automatically on worktree lifecycle events:
- `on_worktree_create` — runs after a new worktree is created
- `on_worktree_delete` — runs before a worktree is deleted

## Session Persistence

Sessions are recorded in JSONL format and stored in `~/.bramble/sessions/<repo>/<worktree>/`. You can replay session logs with the built-in log viewer:

```bash
bazel run //bramble/cmd/logview -- path/to/session.jsonl
```

## CLI Flags

| Flag | Description |
|------|-------------|
| `--repo <name>` | Open a specific repo directly |
| `--editor <cmd>` | Set editor for `[e]dit` action (default: `$EDITOR` or `code`) |
| `--session-mode auto\|tui\|tmux` | Execution mode (default: auto-detect) |
| `--tmux-exit-on-quit` | Kill tmux windows when quitting Bramble |
| `--protocol-log-dir <dir>` | Directory for provider protocol/stderr logs |
| `--yolo` | Skip all permission prompts (use with caution) |

## Utility Tools

| Tool | Description |
|------|-------------|
| `bramble/cmd/logview` | Render JSONL session logs in the terminal |
| `bramble/cmd/tmuxwatch` | Live monitoring dashboard for tmux-mode sessions |
| `bramble/cmd/sessanalyze` | Analyze session recordings |

## Development

To build from source (requires [Bazel](https://bazel.build)):

```bash
# Build
bazel build //bramble

# Run
bazel run //bramble

# Run with flags
bazel run //bramble -- --session-mode tui

# Run tests
bazel test //...
```

## Architecture

Bramble follows an MVC architecture:

```
bramble/app/          VIEW        BubbleTea TUI components
bramble/session/      CONTROLLER  Session lifecycle, runners, persistence
bramble/sessionmodel/ MODEL       Canonical types, output parsing, observers
bramble/ipc/          IPC         Unix socket protocol for CLI integration
bramble/replay/       REPLAY      Multi-format log parsing (Claude, Codex, raw JSONL)
```

See [`docs/design/sessionmodel-architecture.md`](docs/design/sessionmodel-architecture.md) for a deep dive into the data pipeline.

## Environment Variables

| Variable | Description |
|----------|-------------|
| `WT_ROOT` | Base directory for worktrees (default: `~/worktrees`) |
| `EDITOR` | Editor for the `[e]dit` action (default: `code`) |
| `BRAMBLE_PROTOCOL_LOG_DIR` | Directory for provider protocol/stderr logs |
| `BRAMBLE_SOCK` | IPC socket path (set automatically by Bramble) |
