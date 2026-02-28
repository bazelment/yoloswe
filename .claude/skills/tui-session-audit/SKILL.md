---
name: tui-session-audit
description: >
  Audit and improve Bramble TUI session rendering, interaction flows, and replay fidelity.
---

# TUI Session Audit

Systematically test and improve Bramble's terminal UI to ensure sessions render correctly, interactive flows work end-to-end, and replay faithfully reproduces live sessions — across all providers.

## Arguments

```
/tui-session-audit [--iterations N] [--until "condition"] [--focus <area>]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--iterations N` | `5` | Stop after N rounds |
| `--until "cond"` | — | Stop when condition met (e.g., `"no gaps in rendering"`) |
| `--focus <area>` | all | Focus: `rendering`, `replay`, `interaction`, `layout`, `persistence` |

## Before You Start

Read these files to understand Bramble's architecture:

- `bramble/session/types.go` — Session, OutputLine, SessionProgress types
- `bramble/session/event_handler.go` — How provider events become OutputLines
- `bramble/app/output.go` — How OutputLines render in the TUI
- `bramble/app/text_render.go` — Text and markdown rendering
- `bramble/replay/` — Session replay parsing (Claude and Codex formats)
- `bramble/session/store.go` — Session persistence format

Also check the current audit state in `memory/gap-matrix.md` (this skill's directory).

## Architecture Overview

Bramble's rendering pipeline:

```
Provider SDK Event
  → agentstream interface (type assertion)
    → bridgeProviderEvents() goroutine
      → trackingEventHandler (turn filtering, dedup)
        → sessionEventHandler (convert to OutputLine)
          → Manager.addOutput(sessionID, line)
            → TUI Model listens on Manager.Events()
              → OutputModel.View() renders lines
```

For replay:
```
messages.jsonl or codex log file
  → replay.Parse() (auto-detect format)
    → []OutputLine (unified)
      → OutputModel.View() renders lines
```

The key insight: both live sessions and replay share the same rendering path through `OutputLine` → `OutputModel.View()`. Fixes to rendering benefit both.

### OutputLine Types

Every piece of session output is an `OutputLine` with a `Type`:

| Type | Renders As | Source |
|------|-----------|--------|
| `Text` | Prose, optionally markdown-rendered | Text streaming from provider |
| `Thinking` | Dim/italic text | Thinking/reasoning events |
| `ToolStart` | `[Tool] ToolName: summary` (running state) | Tool invocation begin |
| `ToolResult` | Updated tool line with result, duration, cost | Tool completion |
| `Error` | Error-styled text | Provider errors |
| `Status` | Dim status text | Status messages, token summaries |
| `TurnEnd` | Turn summary line | Turn completion |
| `PlanReady` | Plan notification | Planner sessions only |

### Tool Content Formatting

Tool summaries are truncated for display (`event_handler.go`):
- Read: `"Read /path/to/file"`
- Write/Edit: `"Write → /path"` / `"Edit → /path"`
- Bash: `"Bash: command"` (50 char truncation)
- Glob: `"Glob pattern"`
- Grep: `"Grep pattern"` (40 char truncation)

## Phase 0 — Build the Gap Matrix

### Step 1: Inventory TUI capabilities

For each OutputLine type and each provider, verify:

1. **Does the event reach the TUI?** — Is `bridgeProviderEvents()` translating the SDK event correctly?
2. **Does `sessionEventHandler` produce the right OutputLine?** — Correct type, content, tool metadata?
3. **Does `OutputModel.View()` render it well?** — Readable, properly styled, not truncated badly?
4. **Does replay reproduce it?** — Does `replay.Parse()` produce equivalent OutputLines from recorded sessions?
5. **Does persistence round-trip?** — Does `Store.SaveSession()` → `Store.LoadSession()` preserve all data?

### Step 2: Test with real sessions

Run Bramble and exercise each provider. For each, try:

- Simple text response (does streaming work?)
- Tool use (Read, Write, Bash — do tool start/complete render?)
- Multi-tool response (parallel tool calls — do they interleave correctly?)
- Thinking/reasoning (does it render in dim/italic?)
- Errors (does error styling work?)
- Turn completion (does the summary appear with cost/tokens?)
- Follow-up messages (does multi-turn work for providers that support it?)

### Step 3: Test replay fidelity

For sessions recorded by each provider:

1. Record a session (live Bramble or use existing recordings in `~/.bramble/sessions/`)
2. Replay with `bramble logview <path>` (or `bramble logview --compact`)
3. Compare live output vs. replay output — same lines? Same order? Same formatting?
4. Check edge cases: sessions with many tool calls, sessions with errors, sessions that were cancelled

### Step 4: Produce the gap matrix

Create a table in `memory/gap-matrix.md`:

- **Rows**: Each capability (text rendering, tool display, thinking, errors, turn summary, replay, persistence, layout, etc.)
- **Columns**: Claude | Codex | Gemini | Replay? | Persistence?
- **Cells**: `works` / `broken` / `partial` / `untested` with notes

Sort by severity: broken rendering > missing data > degraded display > cosmetic.

## Iteration Loop

### 1. Pick gaps

Select highest-impact rendering or interaction issues.

### 2. Locate the fix

Each gap maps to a specific layer:

| Symptom | Likely Fix Location |
|---------|-------------------|
| Event never reaches TUI | `multiagent/agent/<provider>_provider.go` or `bridge.go` |
| Wrong OutputLine type/content | `bramble/session/event_handler.go` |
| Bad rendering | `bramble/app/output.go` or `bramble/app/text_render.go` |
| Replay doesn't match live | `bramble/replay/claude.go` or `bramble/replay/codex.go` |
| Data lost after restart | `bramble/session/store.go` |
| Layout broken at certain widths | `bramble/app/view.go` or `bramble/app/model.go` |
| Compact mode wrong | `bramble/replay/compact.go` |

### 3. Implement the fix

Follow TDD:

1. **Write a test first** — Unit test for the specific rendering/parsing behavior
   - Event handler tests: verify OutputLine produced from provider events
   - Replay tests: verify parsed output matches expected lines
   - Store tests: verify round-trip fidelity
   - For integration tests, use the `integration/` directory pattern with `# gazelle:ignore` BUILD.bazel

2. **Fix the implementation** — Work at the right layer (don't paper over event handler bugs in the renderer)

3. **Check the compact mode path too** — `compact.go` merges verbose status lines; make sure your fix works in both normal and compact modes

### 4. Verify

```bash
scripts/lint.sh
bazel build //...
bazel test //bramble/... --test_timeout=60
```

For replay changes, also test with real session files:
```bash
bazel run //bramble -- logview <path-to-session>
bazel run //bramble -- logview --compact <path-to-session>
```

### 5. Update state

Mark rows in `memory/gap-matrix.md`. Note any provider-specific quirks discovered.

### 6. Check exit conditions

Stop if: max iterations reached, `--until` condition met, or no actionable gaps remain.

## Testing Patterns

### Unit Tests

For event handler changes:
```go
// bramble/session/event_handler_test.go
func TestEventHandler_ToolStartProducesCorrectOutputLine(t *testing.T) {
    handler := newSessionEventHandler(...)
    handler.OnToolStart("Read", "tool-123", map[string]interface{}{"file_path": "/tmp/test"})
    // Assert the OutputLine has correct Type, ToolName, Content
}
```

For replay changes:
```go
// bramble/replay/claude_test.go
func TestClaudeReplay_ToolCallSequence(t *testing.T) {
    result, err := Parse("testdata/session-with-tools/")
    require.NoError(t, err)
    // Assert OutputLine sequence matches expected
}
```

For store round-trip:
```go
// bramble/session/integration/store_test.go
func TestStorePersistenceRoundtrip(t *testing.T) {
    // Create session with varied OutputLines
    // Save, Load, compare
}
```

### Integration Tests

Place in `integration/` directories with:
- `//go:build integration` build tag
- `# gazelle:ignore` in BUILD.bazel
- `tags = ["manual"]` to exclude from `bazel test //...`

### Replay Test Data

Keep sample session recordings in `bramble/replay/testdata/` for regression testing. Include sessions from each provider format.

## Reference Files

| File | Purpose |
|------|---------|
| `bramble/session/types.go` | OutputLine, Session, SessionProgress types |
| `bramble/session/event_handler.go` | Provider event → OutputLine translation |
| `bramble/session/provider_runner.go` | Provider event bridging to session handler |
| `bramble/app/output.go` | OutputModel and rendering logic |
| `bramble/app/text_render.go` | Text and markdown rendering |
| `bramble/replay/claude.go` | Claude session replay parser |
| `bramble/replay/codex.go` | Codex session replay parser |
| `bramble/replay/compact.go` | Compact mode line merging |
| `bramble/session/store.go` | Session persistence (JSON) |
