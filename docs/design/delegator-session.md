# Delegator Session: Orchestrating Child Sessions

## Problem

Bramble sessions (planner/builder) run autonomously with no orchestration layer.
The user must manually start sessions, monitor progress, interpret failures, and
decide on retries. Common failure modes like transient API errors or lint issues
that could be auto-retried require human attention. This creates friction when
managing multiple sessions on a worktree branch.

## Key Insight: LLM as Orchestrator

Rather than building a rule-based retry engine, we use a Claude session (Sonnet,
plan mode) as the orchestration layer. The LLM reads child session output and
makes judgment calls: is this error retriable? Does the user need to weigh in?
Should I start a planner first or go straight to building?

This keeps retry/escalation logic in the LLM prompt, not hardcoded. The
delegator's only special machinery is three SDK tools and a child-state watcher.

## Architecture

```
User <--> Delegator session (follow-ups) <--> delegatorRunner
                                                |
                                                v
                                          Claude session (Sonnet, plan mode)
                                           with SDK tools via TypedToolRegistry
                                                |
                                          DelegatorToolHandler
                                         /       |         \
                                 start_session  stop_session  get_session_progress
                                         \       |         /
                                          session.Manager
                                                |
                                       child sessions (planner/builder)
```

### How It Fits Into the Existing Stack

The delegator is a new `SessionTypeDelegator` with a `delegatorRunner` that
implements the existing `sessionRunner` interface. It plugs into the same
`Manager.runSession()` turn loop as planner and builder sessions:

```
sessionRunner interface
├── plannerRunner     → wraps PlannerWrapper (claude session in plan mode)
├── builderRunner     → wraps BuilderSession (claude session in bypass mode)
├── providerRunner    → wraps agent.Provider  (codex, gemini, etc.)
├── tmuxRunner        → launches tmux window
└── delegatorRunner   → wraps claude session with SDK tools (NEW)
```

The delegator appears as a regular session in the command center. The user
interacts via follow-ups, same as with planner/builder sessions.

### Constraints

- **TUI mode only.** The delegator runs in-process with SDK tools. Child sessions
  can still be TUI or tmux depending on Manager config.
- **Read-only.** The delegator runs in plan permission mode — it cannot edit files
  itself. It delegates all code changes to builder sessions.
- **Single worktree scope.** One delegator per worktree branch.

## SDK Tools

The delegator's Claude session has three tools registered via `TypedToolRegistry`:

### `start_session`

Starts a new child session on the delegator's worktree.

```go
type StartSessionParams struct {
    Type   string `json:"type"   jsonschema:"required,enum=planner|builder,description=Session type"`
    Prompt string `json:"prompt" jsonschema:"required,description=Task prompt for the session"`
    Model  string `json:"model"  jsonschema:"description=Model ID (optional, defaults to session default)"`
}
```

Calls `Manager.StartSession()` scoped to the delegator's worktree path. Tracks
the returned session ID in a child set for the state watcher. Returns the session
ID as a string.

### `stop_session`

Stops a running child session.

```go
type StopSessionParams struct {
    SessionID string `json:"session_id" jsonschema:"required,description=ID of the session to stop"`
}
```

Calls `Manager.StopSession()`. Validates that the session ID belongs to the
delegator's tracked child set (rejects attempts to stop unrelated sessions).

### `get_session_progress`

Returns a formatted summary of a child session's current state.

```go
type GetSessionProgressParams struct {
    SessionID string `json:"session_id" jsonschema:"required,description=ID of the session to inspect"`
}
```

Calls `Manager.GetSessionInfo()` + `Manager.RecentOutputLines()`. Returns a
formatted string with: status, error message (if any), recent assistant output
(last 20 lines), turn count, and cumulative cost.

## Child-State Watcher

When the delegator's turn completes and it goes idle, the turn loop must also
watch for child session state changes — not just user follow-ups. Without this,
the delegator would sit idle even after a child session completes or fails.

### Mechanism

A goroutine subscribes to `Manager.Events()` and filters for
`SessionStateChangeEvent` where the session ID belongs to the delegator's tracked
child set. Meaningful state changes (idle, completed, failed) are forwarded on a
notification channel.

The turn loop's idle-wait `select` gains an additional case:

```go
select {
case <-session.ctx.Done():
    // cancellation (existing)
case followUp := <-followUpChan:
    // user sent a follow-up (existing)
case notif := <-childNotifyChan:
    // child session state changed — auto-resume
    currentPrompt = fmt.Sprintf(
        "Child session %s status changed to %s. "+
        "Use get_session_progress to check details and decide next steps.",
        notif.SessionID, notif.NewStatus,
    )
    m.updateSessionStatus(session, StatusRunning)
    continue
}
```

### Which State Changes Trigger Auto-Resume

All meaningful transitions: **idle** (child finished a turn), **completed**, and
**failed**. This lets the delegator inspect progress after each child turn and
decide whether to send a follow-up, start another session, or escalate to the
user.

## Delegator System Prompt

The system prompt (defined as a constant in `delegator_runner.go`) instructs
the Claude session:

- It orchestrates work on a single git worktree branch
- It can start **planner** sessions (read-only analysis, produces a plan) and
  **builder** sessions (code modification)
- It should monitor child session progress via `get_session_progress`
- When child sessions fail with retriable errors (transient API errors, lint
  failures fixable by retry, context-window exhaustion), auto-retry by starting
  a new session with adjusted instructions
- When it needs genuine human input, it should end its turn with a clear summary
  and question — the user will respond via follow-up
- It should not make code changes itself (it runs in plan mode)

## Runner Implementation

`delegatorRunner` implements the `sessionRunner` interface:

| Method | Implementation |
|--------|---------------|
| `Start(ctx)` | Creates `claude.NewSession()` with plan mode, SDK tools, system prompt, and disables plugins. Starts the session. |
| `RunTurn(ctx, message)` | Calls `claudeSession.SendMessage(ctx, message)`. SDK tool calls are handled in-band by the protocol. Returns when the turn completes. |
| `Stop()` | Calls `claudeSession.Stop()`. |
| `CLISessionID()` | Returns `claudeSession.SessionID()` for resume support. |

The event handler is wired via `claude.WithEventHandler()` to forward streaming
events (text, thinking, tool calls) to the session's `sessionEventHandler`, which
in turn populates the Manager's output buffer — making delegator output visible
in the command center.

## Thread Safety

The `DelegatorToolHandler` calls back into `Manager` methods that are already
mutex-protected. No new locks are needed. The child session ID set is protected
by the tool handler's own mutex (writes from `start_session`, reads from the
state watcher goroutine and `stop_session`).

There is no circular lock risk: the Manager's mutexes are never held when calling
the tool handler, and the tool handler never holds its mutex while calling
Manager methods.

## Prompt Testing Harness

The delegator's behavior is driven by its system prompt (`DelegatorSystemPrompt`
in `delegator_runner.go`). Iterating on the prompt requires observing how the LLM
reacts to child session state changes, errors, ambiguous tasks, and multi-session
orchestration. The test harness provides three layers:

### Mock Tool Handler (`delegator_mock.go`)

`MockDelegatorToolHandler` is a drop-in replacement for `DelegatorToolHandler`
that registers identical tool schemas but returns scripted responses:

- **Scripted state machines**: Each session type (`planner`, `builder`) has a
  queue of `MockSessionBehavior` scripts. Each `start_session` call pops the next
  behavior. States include status, turn count, cost, token counts, recent output,
  and optional `Question` field for `waiting_for_input`.
- **Read-only `get_session_progress`**: Returns current state without advancing.
  State only advances via `AdvanceAll()` between turns, simulating async child
  progress.
- **Notification deduplication**: `DrainNotifications()` tracks which states have
  already been notified via a `notified` map, preventing infinite notification loops.
- **`AdvanceUntilNotification()`**: Steps all sessions forward one at a time until
  a notifiable state is reached (completed, failed, stopped, waiting_for_input) or
  no more progress can be made. Up to 20 steps.
- **Call recording**: All tool invocations are recorded with parameters and
  timestamps for assertion in tests.

### Scenario Runner (`delegator_scenario.go`)

`RunDelegatorScenario` executes a complete delegator conversation with a real
Claude session and mock tools:

```go
type DelegatorScenarioConfig struct {
    InitialPrompt string
    Behaviors     map[string][]*MockSessionBehavior
    MaxTurns      int
    AutoNotify    bool               // auto-send child state notifications
    FollowUps     map[int]string     // turn# → explicit user message
    Model         string             // default: "haiku"
    SystemPrompt  string             // override for experimentation
    SessionOpts   []claude.SessionOption // for future WithAgents() testing
}
```

The `SessionOpts` field allows the same scenario framework to test alternative
delegator implementations (e.g. native `--agents` mode via `claude.WithAgents()`)
without rewriting test infrastructure.

### CLI Harness (`bramble/cmd/delegator/`)

Interactive tool for prompt engineering, available as a subcommand of the
bramble CLI. Uses `render.Renderer` for terminal output, which handles
streaming text buffering and ANSI-colored formatting (see "Streaming Text
Event Pipeline" in CLAUDE.md).

```
bazel run //bramble -- delegator --prompt "Create a hello world" --model haiku
```

Flags: `--model`, `--auto-advance`, `--behavior`, `--system-prompt`, `--prompt`,
`--work-dir`, `--verbose`.

### Key Design Decisions

- **`get_session_progress` is read-only**: The mock does not advance state on
  reads. This prevents the delegator from polling the same session repeatedly
  within a turn and seeing artificial progress.
- **Tool isolation defense-in-depth**: The delegator session is restricted to
  only the three SDK tools via `WithTools("")` (disables all built-in CLI tools)
  and `WithDisablePlugins()` (prevents MCP plugins). This prevents the model from
  directly using Read, Write, Bash even if the prompt instructions are insufficient.
- **Async notification model**: The prompt explicitly tells the delegator that
  child sessions are async and it should yield after starting them. The test
  harness simulates this by calling `AdvanceUntilNotification()` between turns.

## Files

| File | Change |
|------|--------|
| `bramble/session/types.go` | Add `SessionTypeDelegator` constant |
| `bramble/session/manager.go` | Wire `delegatorRunner` into `runSession()`, add child-state watcher to turn loop, default model to sonnet for delegator |
| `bramble/session/delegator_tools.go` | **NEW** — `DelegatorToolHandler` with start/stop/get_session_progress |
| `bramble/session/delegator_runner.go` | **NEW** — `delegatorRunner` implementing `sessionRunner`, exported `DelegatorSystemPrompt` |
| `bramble/session/delegator_mock.go` | **NEW** — `MockDelegatorToolHandler` with scripted state machines |
| `bramble/session/delegator_scenario.go` | **NEW** — `RunDelegatorScenario` helper + config/result types |
| `bramble/session/delegator_tools_test.go` | **NEW** — Unit tests for tool handler |
| `bramble/session/delegator_runner_test.go` | **NEW** — Unit tests for runner and child watcher |
| `bramble/session/integration/delegator_test.go` | **NEW** — Integration test with real Manager |
| `bramble/session/integration/delegator_scenario_test.go` | **NEW** — 5 scenario integration tests |
| `bramble/cmd/delegator/delegator.go` | **NEW** — Interactive CLI harness (bramble subcommand) |
| `bramble/cmd/delegator/BUILD.bazel` | **NEW** — Bazel library target |

## Testing Strategy

### Unit Tests (no Claude session needed)

Test the tool handler against a real `Manager` instance with mock providers:

- **`TestStartSessionTool`**: Verify `start_session` creates a child session via
  Manager and returns its ID.
- **`TestStopSessionTool`**: Verify `stop_session` calls `Manager.StopSession()`
  and rejects IDs not in the child set.
- **`TestGetSessionProgressTool`**: Verify formatted output includes status,
  recent output lines, error messages, and cost.
- **`TestChildNotificationChannel`**: Verify child state changes
  (`SessionStateChangeEvent`) are forwarded to the notification channel, and
  non-child events are filtered out.

### Scenario Integration Tests (`//go:build integration`)

Using a real Claude session (haiku) with mock tools:

| Scenario | Prompt | Key Assertion |
|----------|--------|---------------|
| Happy path | "Create a hello world Go program" | Calls start_session, reports completion |
| Retriable error | "Refactor auth module" | Detects failure, retries with new session |
| Non-retriable error | "Rewrite entire codebase" | Does NOT retry, asks user |
| Ambiguous task | "Fix the bug" | No start_session, asks for clarification |
| Multi-session | "Add auth and update docs" | Multiple builder sessions created |

Assertions are flexible (e.g. "at least one start_session") due to LLM
nondeterminism.

### Manager Integration Tests (`//go:build integration`)

Using a real Claude session with the delegator tools and real Manager:

- Start a delegator session with a simple task prompt
- Verify it creates child sessions (planner/builder) via the tools
- Verify it produces output visible through `GetSessionOutput()`
- Verify child state changes auto-resume the delegator
- Use `require.Eventually` for async state assertions

## Existing Code Reused

- `claude.TypedToolRegistry` / `AddTool[T]()` — type-safe SDK tool registration
- `claude.NewSession()` with `WithSDKTools()`, `WithPermissionMode()`, `WithModel()`
- `render.Renderer` — streaming text buffering and ANSI terminal output (used by CLI harness)
- `sessionEventHandler` — event forwarding to Manager output buffer
- `Manager.StartSession()`, `StopSession()`, `GetSessionInfo()`, `RecentOutputLines()`
- `SessionStateChangeEvent` in `Manager.Events()` — for child-state watching
- `PlannerToolHandler` pattern — reference for tool handler design
