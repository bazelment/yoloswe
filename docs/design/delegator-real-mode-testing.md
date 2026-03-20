# Delegator Real Mode: Testing & Iteration Log

## Overview

This document captures the iterative testing and review process for adding `--mode real` to `delegator-test`, from initial implementation through multiple rounds of output readability improvements.

## Files Changed

| File | Change |
|------|--------|
| `bramble/cmd/delegator-test/main.go` | `--mode`, `--log-dir`, `--timeout` flags; `runReal()` with turn-based rendering |
| `bramble/cmd/delegator-test/BUILD.bazel` | No changes needed (gazelle-ignored, deps unchanged) |
| `bramble/session/manager.go` | `RecordingDir` field on `ManagerConfig`; threaded to planner/builder/delegator in `runSession()` |
| `bramble/session/delegator_runner.go` | `recordingDir` field; `claude.WithRecording()` in `Start()` |

## Test Run 1: First Working Real Mode (haiku delegator)

```
echo "What files are in this project?" | delegator-test --mode real --work-dir /tmp/test-project --model haiku
```

**Raw output (ANSI stripped):**
```
Delegator Test Harness (real) | Model: haiku | Work dir: /tmp/test-project

[delegator] Started session test-project-delegator-8002ebc4
[session test-project-delegator-8002ebc4] pending → running
The user is asking whatI'll start
[mcp__delegator-tools__start_session] Starting claude with flags: ...
[session test-project-planner-5c56328a] pending → running
[mcp__delegator-tools__start_session]
[mcp__delegator-tools__start_session] 2026/03/19 WARN skipping unknown protocol message type
TheI
✓ Turn 1 complete (6.0s, $0.0187)
[child test-project-planner-5c56328a] Tool: Bash
[child test-project-planner-5c56328a] Tool: Glob
[child test-project-planner-5c56328a] Tool: Read
[child test-project-planner-5c56328a] This is a minimal project with a single file:
[child test-project-planner-5c56328a] Turn 1 complete (cost: $0.0962)
[session test-project-planner-5c56328a] running → idle
[session test-project-delegator-8002ebc4] idle → running
The pl
[mcp__delegator-tools__get_session_progress]
GreatHere
✓ Turn 2 complete (4.7s, $0.0259)
```

**Problems identified:**
1. Streaming text fragmented: `The user is asking whatI'll start`, `TheI`, `GreatHere`
2. MCP tool names ugly: `mcp__delegator-tools__start_session`
3. Empty tool result lines from re-render signals
4. Every state transition printed: `pending → running` noise
5. Child session output mixed with delegator output
6. SDK WARN and "Starting claude with flags" stderr noise

## Iteration 2: Remove Child Output

**Change:** Skip all `SessionOutputEvent` where `SessionID != delegatorID`.

**Result:** Child `[child ...]` lines gone, but delegator output still fragmented.

## Iteration 3: Switch to Direct Printing

**Change:** Replaced `render.Renderer` calls with direct `fmt.Fprintf` to avoid the Renderer's tool-state tracking (designed for raw streaming, not Manager's pre-accumulated `OutputLine` events).

**Also:**
- Stripped MCP prefix: `mcp__delegator-tools__start_session` → `start_session`
- Filtered tool completion re-renders via `ToolState != ToolStateRunning`
- Only showed child state: `started` / `completed` / `failed` (not `pending → running`)
- Added newline after thinking blocks

**Result:** Cleaned up tool duplication and noise, but text still fragmented.

## Root Cause: Manager Event Model Mismatch

The Manager's `appendOrAddOutput()` has two paths:
1. **First chunk** → `addOutput()` → emits `SessionOutputEvent{Line: populated}` ← we render this
2. **Subsequent chunks** → appends to existing line, emits `SessionOutputEvent{Line: empty}` ← re-render signal

We correctly skip empty-Line events, but that means we only see the *first* chunk of each text line, not the full accumulated content. For example, the model streams "I'll help you build a REST API server" as:
- Chunk 1: `"I'll help"` → new OutputLine → event with content
- Chunk 2: `" you build"` → appended to line → empty event (skipped)
- Chunk 3: `" a REST API server"` → appended → empty event (skipped)

We only print `"I'll help"`.

## Iteration 4: Turn-Based Rendering (Final)

**Key insight:** Instead of rendering per-event, wait for turn boundaries and read the full accumulated output via `m.GetSessionOutput(delegatorID)`.

**Approach:**
```go
linesRendered := 0

renderNewOutput := func() {
    lines := m.GetSessionOutput(delegatorID)
    for i := linesRendered; i < len(lines); i++ {
        // render lines[i] based on Type
    }
    linesRendered = len(lines)
}
```

Trigger `renderNewOutput()` on:
- `OutputTypeTurnEnd` event for delegatorID
- Terminal `SessionStateChangeEvent` (completed/failed/stopped)

**Child session status:** On state changes, read `GetSessionInfo()` for the child and print a summary line:
```
  ▶ test-project-planner-xxx (planner)                          — started
  ✓ test-project-planner-xxx (planner) completed (turns: 1, $0.0962) — last output line
  ✗ test-project-builder-xxx (builder) failed: error message
```

**Tool display:** Show tool name + formatted input summary:
```
  start_session (planner) Analyze the current codebase to understand...
  get_session_progress test-project-builder-731a3914
```

**Also fixed:** `hasActiveChildren()` now checks non-terminal states (not just `StatusRunning`) to prevent premature `You>` prompts when children are in `idle` state between turns.

## Test Run 2: Sonnet Delegator, Full Planner→Builder Flow

```
echo "Add a REST API server with /health and /echo endpoints, and also add a CLI tool that calls the API. Use separate packages." \
  | delegator-test --mode real --work-dir /tmp/test-project --model sonnet --log-dir /tmp/delegator-real-logs
```

**Clean output (ANSI stripped, stderr noise filtered):**
```
You> Delegator Test Harness (real) | Model: sonnet | Work dir: /tmp/test-project

  ▶ test-project-planner-feb15ca8 (planner)
The user wants to add a REST API server with /health and /echo endpoints,
and a CLI tool that calls the API, using separate packages. Let me start
a planner session first to analyze the codebase and create a plan.
  start_session Analyze the current codebase to understand:
1. What language/runtime is being...
I've kicked off a planner session to analyze your codebase first. It will
look at the existing project structure, language, conventions, and then
produce a detailed implementation plan. I'll report back once it's done!
✓ Turn 1 (13.7s, $0.0268)
  ▶ test-project-builder-731a3914 (builder)
Let me check the planner session progress.
  get_session_progress test-project-planner-feb15ca8
The planner has analyzed the codebase and produced a plan. Now I'll start
a builder session to implement everything.
The plan is solid. It's a Go project (example.com/testproject, Go 1.21),
and everything will be done with the standard library — no new
dependencies. Let me now kick off the builder session to implement it all.
  start_session Implement the following in a Go 1.21 module ...
Builder session is running! It's creating all four files and will verify
everything compiles and tests pass before finishing.
✓ Turn 2 (22.2s, $0.0581)
  get_session_progress test-project-builder-731a3914
Everything is done and verified! Here's a summary of what was built:

### What was created

**internal/api/handlers.go**
- HealthHandler — GET /health → 200 OK + {"status":"ok"}
- EchoHandler — POST /echo → echoes {"message":"..."} back

**internal/api/handlers_test.go**
- Table-driven tests covering all cases for both handlers

**cmd/server/main.go**
- Registers both handlers, reads PORT env var (default 8080)

**cmd/cli/main.go**
- Global --base-url flag (default http://localhost:8080)
- health subcommand → GET /health
- echo subcommand → --message flag required, POST /echo

### How to use it

  go run ./cmd/server
  go run ./cmd/cli health
  go run ./cmd/cli echo --message "hello world"

Both go build ./... and go test ./internal/api/... passed cleanly.
No third-party dependencies were added.
✓ Turn 3 (8.4s, $0.0750)
```

**What works well:**
- Complete, unfragmented text for both thinking and assistant output
- Clear delegator flow: think → tool call → explain → wait → resume
- Child session started/completed status with session type
- Tool calls show name + meaningful input summary
- Turn summaries with timing and cost
- Auto-resume: planner completes → delegator checks → starts builder → builder completes → delegator summarizes
- Total cost visible: $0.075 across 3 turns

## Test Run 3: After Fix Round (Sonnet, All Three Fixes)

Same prompt as Test Run 2, after fixing planner cost, ToolSearch display, and model propagation.

**Fixes verified:**
- Planner cost now shows correctly: `planner: 1 session, $0.5623` (was `$0.0000`)
- ToolSearch tool calls filtered from display (model still emits them, but `isDelegatorTool()` filter skips rendering)
- Child model propagated: `--model sonnet` for all children (was defaulting planners to opus)

**New observation:** Planner session cost was $0.56 — much higher than expected. The sonnet planner actually implemented the code (not just planned), despite being a "read-only" session type. This is a model behavior issue: sonnet with `Simple: true` + `BuildModeReturn` may still produce extensive output before calling ExitPlanMode.

**Total:** $0.84 across delegator + planner + builder (3 delegator turns)

## Test Run 4: After --tools "" Fix (Sonnet, All Built-in Tools Disabled)

**Fixes applied:**
- `--tools ""` disables ALL built-in tools in delegator session (replaces `--allowed-tools` approach)
- `WithAllowedTools(DelegatorAllowedTools...)` removed — no longer needed
- System prompt cleaned up: removed ToolSearch-specific language

**Verified across 3 test runs:**
- Delegator session tool count: 3 (only MCP delegator tools)
- Child sessions retain full tool set (26 tools including Bash, Read, Write, etc.)
- Zero ToolSearch invocations in any delegator session
- Simple task (greeting.go): completed in 2 delegator turns, $0.08 total
- Complex task (REST API + CLI): completed in 3 delegator turns, $0.53 total

**New observations:**
- Planner sessions still occasionally implement code despite being "read-only" ($0.28-$0.57 per planner session)
- 5-minute timeout insufficient for complex tasks — builder needs more time
- stderr noise from child processes leaks to terminal (addressed separately)

## Known Limitations

1. **Stderr noise** — ~~Fixed.~~ `planner.go` debug output is now gated on the `Verbose` flag, and `protocol/parse.go` `slog.Warn` was changed to `slog.Debug`. No more stderr leakage during normal operation.

2. **No streaming** — ~~Partially addressed.~~ A periodic progress ticker (30s interval) has been added to `runReal()`, so long-running turns now show periodic status updates. Output is still turn-based rather than truly streaming, but the user no longer sees complete silence during multi-minute builder sessions.

3. **Pipe buffering** — interactive mode doesn't work well with piped stdin (`echo "line1\nline2" |`). The `bufio.Scanner` reads all lines eagerly, so follow-up lines may be consumed before the delegator is ready. Use a real terminal for interactive testing.

4. **`ToolSearch` tool call** — ~~Fixed.~~ The `--tools ""` flag now disables ALL built-in tools in the delegator session. The delegator only has 3 MCP tools available — no built-in tools at all. ToolSearch is completely unavailable, verified across 3 test runs. The previous `--allowed-tools` approach and `WithAllowedTools(DelegatorAllowedTools...)` have been removed.

## Architecture: Why Turn-Based Rendering

The Manager's event model was designed for the TUI (bramble app), which re-renders the full screen on every event. For a CLI, we need sequential line output. The mismatch:

| Manager Event | TUI Behavior | CLI Need |
|---------------|-------------|----------|
| `addOutput(text chunk)` | Re-render screen | Print chunk |
| `appendOrAddOutput(text chunk)` | Re-render screen (same result) | Append to last printed line |
| `updateToolOutput(complete)` | Re-render tool line in-place | Don't reprint tool header |

Turn-based rendering sidesteps all of this by reading the final accumulated state at clean boundaries (turn ends, state changes). The tradeoff is no streaming output, but for a test harness this is acceptable.
