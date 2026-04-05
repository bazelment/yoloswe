# Tmux Pane Capture Learnings

Collected 2026-03-06 from 10+ minutes of live monitoring across 7 Claude Code windows.

## Claude Code TUI Layout (bottom of pane)

```
<agent output / tool results>
❯ <user input>                              ← idle prompt
✻ Worked for 36m 36s                        ← completion indicator (optional)
─────────────────────────────────────────── ← separator (always, ─{10,})
  ~/path  branch  Model  ctx:XX%  tokens:NNk [Context left until auto-compact: N%]
  ⏵⏵ bypass permissions on (...) [· PR #NNN]
```

## Character Variants

| Type | Characters | Example | Meaning |
|------|-----------|---------|---------|
| Completion | ✻ ✢ ✽ ✹ | `✻ Worked for 36m 36s` | Turn finished, idle |
| Spinner | * · ⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏ | `* Frosting… (2m 30s)` | Actively working |
| Tool exec | ● | `● Bash(git status)` | Content (keep) |
| Idle prompt | ❯ | `❯ ` | Awaiting user input |

## Cursor-Based Parsing (preferred)

`tmux display-message -t <pane_id> -p '#{cursor_y}'` gives the cursor row (0-indexed).
Note: must target a **pane ID** (e.g. `%50`), not a window ID (`@46`). Use `list-panes -t <window> -F '#{pane_id}'` first.

Layout relative to cursor_y:
```
cursor_y - 5: input area separator (──── ▪▪▪ ─)  ← content boundary
cursor_y - 4: ❯ (input prompt)
cursor_y - 3: status bar separator (────────)
cursor_y - 2: info line (path branch model ctx:XX% tokens:NNk)
cursor_y - 1: permissions line (⏵⏵ ...)
cursor_y:     empty (cursor sits here)
```

For unfilled terminals, cursor_y < pane_height, and the empty area below is just blank lines.
This is more reliable than separator scanning because there can be two separator lines.

Implemented in `ParseClaudeStatusBarWithCursor()` with fallback to `ParseClaudeStatusBar()`.

## Parsing Caveats

1. **Token count trailing text** — When context is high, Claude appends warnings:
   `tokens:50k                     Context left until auto-compact: 5%`
   Use `\d+[kKmMbB]?` not `\S+` to avoid consuming the trailing text.

2. **Context compaction** — ctx% drops sharply (73%→0%, 79%→10%). Tokens keep growing.
   Detectable by tracking consecutive ctx% values.

3. **Working state is transient** — At 15s polling, never caught a spinner in 400+ samples.
   Sub-second transitions. Recent output lines are more informative about activity.

4. **⏵⏵ is multi-byte** — The permissions prefix `⏵⏵ ` is 7+ bytes in UTF-8.
   Trailing metadata after permissions: `· PR #930`, `· 4 bashes`, `· Replace gcloud CLI...`

5. **Session disappearance** — Windows can close mid-monitoring. Handle gracefully.

6. **Two separator lines** — Claude TUI has two: input area sep (with ▪▪▪) and status bar sep (plain ─).
   Content boundary is the input area separator, not the status bar one.

7. **Non-breaking space** — The `❯` prompt sometimes uses `\u00a0` instead of regular space.
   Use `strings.HasPrefix(line, "❯")` for robust matching.

8. **Splash banner** — Fresh sessions show `▐▛███▜▌ Claude Code` logo at top.
   Filtered by `isSplashLine()` / `splashRe`.

## Key Files

- `bramble/session/tmux_detect.go` — `CaptureTmuxPane`, `ParseClaudeStatusBar`, `ContentLines`, `StripANSI`
- `bramble/session/manager.go` — 15s capture ticker in `monitorTrackedTmuxWindow`, `CapturePaneText` method
- `bramble/ipc/protocol.go` — `RequestCapturePane` type
- `bramble/cmd/tmuxwatch/` — Live monitoring tool with detailed layout docs in header comment
- `bramble/app/commandcenter.go` — `[p]` preview keybinding, `TogglePreview`, `renderPreviewRow`

## Revalidation

If Claude Code updates its TUI layout, rerun tmuxwatch to check:
```sh
bazel run //bramble/cmd/tmuxwatch 2>/tmp/tmuxwatch.log
# Check stderr for structured logs, stdout for visual rendering
```

Look for:
- New spinner/completion characters
- Changed status bar format
- New fields in info/permissions line
- Different separator characters
