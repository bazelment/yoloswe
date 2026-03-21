# Delegator Real Mode: Testing & Iteration Log

## Overview

This document captures the iterative testing and review process for adding `--mode real` to `bramble delegator`, from initial implementation through multiple rounds of output readability improvements.

## Files Changed

| File | Change |
|------|--------|
| `bramble/cmd/delegator/delegator.go` | `--mode`, `--log-dir`, `--timeout` flags; `runReal()` with turn-based rendering |
| `bramble/cmd/delegator/BUILD.bazel` | Bazel library target for the delegator subcommand |
| `bramble/session/manager.go` | `RecordingDir` field on `ManagerConfig`; threaded to planner/builder/delegator in `runSession()` |
| `bramble/session/delegator_runner.go` | `recordingDir` field; `claude.WithRecording()` in `Start()` |

## Test Run 1: First Working Real Mode (haiku delegator)

```
echo "What files are in this project?" | bramble delegator --mode real --work-dir /tmp/test-project --model haiku
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
  | bramble delegator --mode real --work-dir /tmp/test-project --model sonnet --log-dir /tmp/delegator-real-logs
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

## Test Run 5: Non-Claude Child Sessions (codex + gemini)

**Purpose:** Verify that the delegator can launch child sessions on non-Claude providers (codex, gemini) after adding `ModelRegistry` support.

**Changes applied:**
- `DelegatorToolHandler` now accepts a `*agent.ModelRegistry` and validates model names
- `AvailableModelsDescription()` method formats available models grouped by provider for system prompt injection
- `delegatorSystemPromptWithModels()` appends an "Available models" section to the delegator system prompt
- `bramble delegator` harness probes installed providers via `NewProviderAvailability()` and passes the registry to `ManagerConfig`

**Test 1: Codex child (gpt-5.3-codex)**
```
echo 'Create greeting.go with Greet function. Use gpt-5.3-codex model.' | bramble delegator --mode real --model sonnet
```
- Delegator correctly selected `gpt-5.3-codex` for the builder session
- Codex builder ran but hit a read-only workspace restriction (codex CLI limitation, not our code)
- Delegator self-recovered: started a second builder using Claude sonnet as fallback
- File was successfully created by the fallback session
- 3 delegator turns, $0.14 total

**Test 2: Gemini child (gemini-2.5-flash)**
```
echo 'Create greeting.go with Greet function. Use gemini-2.5-flash model.' | bramble delegator --mode real --model sonnet
```
- Delegator correctly selected `gemini-2.5-flash` for the builder session
- Gemini builder completed successfully on first attempt
- File was successfully created
- 2 delegator turns, $0.04 total
- Clean stderr (no noise)

**Observations:**
- Non-Claude child sessions report `$0.0000` cost — these providers don't report cost through the same mechanism
- Codex workspace read-only issue is external (codex CLI needs different permission config)
- Model validation prevents the delegator from requesting unavailable models
- The delegator's self-recovery behavior (falling back to Claude when codex failed) was emergent and useful

## Test Run 6: Protocol Logging & Token Count Display for Non-Claude Children

**Purpose:** Verify that `ProtocolLogDir` propagation produces protocol logs for codex/gemini children, and that token counts appear in status/summary lines.

**Changes applied:**
- `ManagerConfig.ProtocolLogDir` now set alongside `RecordingDir` when `--log-dir` is provided
- `renderChildStatus` uses `formatProgressDetail()` which shows `Xin/Yout tokens` when cost is zero but tokens are available
- Progress ticker and final summary both include token counts

**Test 1: Codex builder (gpt-5.3-codex) — REST API server**
```
echo 'Add a REST API with /health and /echo endpoints using separate packages. Use gpt-5.3-codex model.' | bramble delegator --mode real --model sonnet --log-dir /tmp/dt-logs-codex
```
- Protocol log created: `test-project-codex-builder-cd0f263e-codex.protocol.jsonl` ✅
- Planner (Claude sonnet) completed with cost: `$0.3491` and tokens: `28in/5605out`
- Summary line shows aggregated tokens: `planner: 1 session, $0.3491, 28in/5605out tokens`
- Codex builder shows `$0.0000` (codex doesn't report cost or tokens through TurnUsage)
- 3 delegator turns, $0.48 total
- Clean stderr

**Test 2: Gemini builder (gemini-2.5-flash) — REST API server**
```
echo 'Add a REST API with /health and /echo endpoints using separate packages. Use gemini-2.5-flash model.' | bramble delegator --mode real --model sonnet --log-dir /tmp/dt-logs-gemini
```
- Protocol log created: `test-project-gemini-builder-273c915b-gemini.protocol.jsonl` ✅
- Gemini stderr log created: `test-project-gemini-builder-273c915b-gemini.stderr.log` ✅
- Builder shows `$0.0000` (gemini ACP doesn't report tokens)
- 2 delegator turns, $0.06 total
- Clean stderr

**Observations:**
- Protocol logging now works for all non-Claude providers (was completely absent before)
- Token count display works correctly in both `renderChildStatus` and the summary line
- The `formatProgressDetail()` function correctly falls through: cost > tokens > zero-cost
- Non-Claude providers still don't report tokens through the session progress mechanism — this is a provider limitation, not a harness issue

## Test Run 7: Model Name Format Fix + Multi-Scenario Validation

**Purpose:** Fix the "gemini: gemini-2.5-flash" model name parsing issue and validate both codex and gemini across simple and complex real-world scenarios.

**Fix applied:**
- `AvailableModelsDescription()` in `delegator_tools.go` changed from grouped format (`- gemini: model1, model2`) to flat list with parenthetical provider (`- model1 (gemini)`). This eliminates ambiguity where the LLM would include the provider prefix as part of the model name.

**4 test scenarios run (before fix):**

| Test | Provider | Prompt | Result | Turns | Cost |
|------|----------|--------|--------|-------|------|
| Simple | codex | Greeting function + tests | Pass | 2 | $0.05 |
| Simple | gemini | Greeting function + tests | Pass | 2 | $0.05 |
| Complex | codex | Task manager CLI (add/list/done, JSON storage, separate packages) | Pass | 2 | $0.10 |
| Complex | gemini | Calculator package (4 ops, table-driven tests, CLI) | Pass, but **duplicate `start_session`** | 2 | $0.09 |

**Finding: Duplicate `start_session` in gemini complex test**
- Delegator first called `start_session` with `model="gemini: gemini-2.5-flash"` (provider prefix included)
- Tool handler returned error: `unknown model "gemini: gemini-2.5-flash"`
- Delegator self-recovered, retried with correct `model="gemini-2.5-flash"`
- Root cause: `AvailableModelsDescription()` format `- gemini: gemini-2.5-flash, ...` was ambiguous

**2 retest scenarios run (after fix):**

| Test | Provider | Result | start_session calls |
|------|----------|--------|---------------------|
| Complex | gemini | Pass, 2 turns, $0.07 | **1** (correct model name) |
| Complex | codex | Pass, 2 turns, $0.08 | 1 |

**Verified:**
- Gemini retest: exactly 1 `start_session` call with `model=gemini-2.5-flash` (no prefix)
- Codex retest: 1 `start_session` call with correct model name
- All 6 runs: clean stderr, correct file creation, proper child lifecycle
- All 6 runs: protocol logs created for non-Claude children

## Test Run 8: Mid-Session User Interaction

**Purpose:** Allow the user to interact with the delegator while child sessions are running, asking questions about progress that the delegator answers via `get_session_progress`.

**Problem:** In interactive mode, when the delegator went idle after starting a child session, `hasActiveChildren()` returned true and the code skipped the `You>` prompt. The user couldn't type until all children finished.

**Fix:** Removed the `hasActiveChildren` gate on the idle prompt. Now whenever the delegator goes idle (including while children are running), the user sees a prompt. The prompt text differentiates: `You (children active)>` vs `You>`.

**Why this is safe:** `SendFollowUp` and child notifications (`watchChildSessionChanges`) both feed into the same `select` in `runSession` (manager.go lines 1787-1825). They are processed sequentially — no race. If a child notification transitions the delegator from idle to running between the prompt display and the user's send, `SendFollowUp` returns an error (status check at manager.go:2200), which is displayed to the user.

**Verified:** Simple ($0.075) and complex ($0.74) non-interactive tests pass all 8 checklist items with no regressions. Interactive feature verified by code inspection (piped stdin cannot test follow-ups).

## Known Limitations

1. **Stderr noise** — ~~Fixed.~~ `planner.go` debug output is now gated on the `Verbose` flag, and `protocol/parse.go` `slog.Warn` was changed to `slog.Debug`. No more stderr leakage during normal operation.

2. **No streaming** — ~~Partially addressed.~~ A periodic progress ticker (30s interval) has been added to `runReal()`, so long-running turns now show periodic status updates. Output is still turn-based rather than truly streaming, but the user no longer sees complete silence during multi-minute builder sessions.

3. **Pipe buffering** — interactive mode doesn't work well with piped stdin (`echo "line1\nline2" |`). The `bufio.Scanner` reads all lines eagerly, so follow-up lines may be consumed before the delegator is ready. Use a real terminal for interactive testing.

4. **`ToolSearch` tool call** — ~~Fixed.~~ The `--tools ""` flag now disables ALL built-in tools in the delegator session. The delegator only has 3 MCP tools available — no built-in tools at all. ToolSearch is completely unavailable, verified across 3 test runs. The previous `--allowed-tools` approach and `WithAllowedTools(DelegatorAllowedTools...)` have been removed.

5. **Non-Claude child session cost reporting** — ~~Partially addressed.~~ The harness now shows token counts when cost is zero (`Xin/Yout tokens` format), and protocol logs are captured for debugging. However, codex and gemini providers still don't report usage through the `TurnUsage` mechanism, so both cost and token counts remain zero for these providers. The total cost summary underreports when non-Claude children are involved.

6. **Codex read-only workspace** — The codex CLI doesn't write files in the workspace by default. This is a codex CLI configuration issue, not a delegator bug. The delegator correctly self-recovers by spawning a Claude fallback session.

## Architecture: Why Turn-Based Rendering

The Manager's event model was designed for the TUI (bramble app), which re-renders the full screen on every event. For a CLI, we need sequential line output. The mismatch:

| Manager Event | TUI Behavior | CLI Need |
|---------------|-------------|----------|
| `addOutput(text chunk)` | Re-render screen | Print chunk |
| `appendOrAddOutput(text chunk)` | Re-render screen (same result) | Append to last printed line |
| `updateToolOutput(complete)` | Re-render tool line in-place | Don't reprint tool header |

Turn-based rendering sidesteps all of this by reading the final accumulated state at clean boundaries (turn ends, state changes). The tradeoff is no streaming output, but for a test harness this is acceptable.
