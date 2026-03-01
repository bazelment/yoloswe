# Claude Session Architecture: End-to-End Analysis

## Overview

This document describes the end-to-end architecture of Claude session management across
the monorepo, identifies the layers involved, analyzes their current boundaries, and
proposes improvements for cleaner separation, faster evolution, and greater robustness.

---

## 1. Layer Map

The system has **six distinct layers**, from lowest (closest to the CLI process) to
highest (closest to the user):

```
┌───────────────────────────────────────────────────────────┐
│  L6  Presentation        bramble/app, bramble/session     │
│       TUI rendering, tmux integration, output display     │
├───────────────────────────────────────────────────────────┤
│  L5  Session Orchestration  bramble/session/manager.go    │
│       Session lifecycle, runner dispatch, event routing   │
├───────────────────────────────────────────────────────────┤
│  L4  Provider Abstraction   multiagent/agent/provider.go  │
│       Provider/LongRunningProvider interfaces,            │
│       AgentResult, AgentEvent, EventHandler               │
├───────────────────────────────────────────────────────────┤
│  L3  Event Bridge           multiagent/agent/bridge.go    │
│       agentstream interfaces, generic bridgeEvents[E],    │
│       scope filtering, turn-complete signaling            │
├───────────────────────────────────────────────────────────┤
│  L2  SDK Wrappers           agent-cli-wrapper/claude/     │
│                             agent-cli-wrapper/acp/        │
│                             agent-cli-wrapper/codex/      │
│       Per-CLI session, state machine, turn management,    │
│       permission handling, MCP, recording                 │
├───────────────────────────────────────────────────────────┤
│  L1  Protocol / Transport   agent-cli-wrapper/protocol/   │
│                             agent-cli-wrapper/internal/   │
│       NDJSON framing, message parsing, content blocks,    │
│       control request/response, JSON-RPC envelope         │
└───────────────────────────────────────────────────────────┘
```

---

## 2. Layer Details

### L1: Protocol / Transport

**Packages:** `agent-cli-wrapper/protocol/`, `agent-cli-wrapper/internal/ndjson/`

**Responsibility:** Wire format — how bytes on stdin/stdout are framed, parsed, and
discriminated into typed messages.

**Key types:**
| Type | File | Purpose |
|------|------|---------|
| `MessageType` | `messages.go` | Discriminator enum (`system`, `assistant`, `user`, `result`, `stream_event`, `control_request`, `control_response`) |
| `FlexibleContent` | `messages.go` | Union type: string or `[]ContentBlock` |
| `ContentBlocks` / `ContentBlock` | `content_blocks.go` | Polymorphic content: `TextBlock`, `ThinkingBlock`, `ToolUseBlock`, `ToolResultBlock` |
| `ControlRequest` / `ControlResponse` | `control.go` | Two-way control channel for permissions, MCP, interactive tools |
| `StreamEvent` | `stream.go` | Streaming deltas (text, thinking, tool progress) |
| `MCPMessageRequest` | `mcp.go` | MCP JSON-RPC envelope within control messages |

**Protocol flow (Claude CLI):**
```
SDK ──NDJSON──► CLI stdin     (UserMessageToSend, ControlResponse)
CLI ──NDJSON──► SDK stdout    (SystemMessage, StreamEvent, AssistantMessage,
                               UserMessage, ResultMessage, ControlRequest)
```

**ACP protocol flow (Gemini):**
```
Client ──JSON-RPC──► Agent stdin   (InitializeRequest, NewSessionRequest, PromptRequest)
Agent  ──JSON-RPC──► Client stdout (InitializeResponse, SessionNotification, PromptResponse)
```

### L2: SDK Wrappers

**Packages:** `agent-cli-wrapper/claude/`, `agent-cli-wrapper/acp/`, `agent-cli-wrapper/codex/`

**Responsibility:** Manage a single CLI subprocess, expose a typed Go API for sending
messages, receiving events, and handling control flow (permissions, MCP, interrupts).

**Claude SDK (`claude/`) internals:**

| Component | File | Purpose |
|-----------|------|---------|
| `Session` | `session.go` | Top-level orchestrator: Start/SendMessage/Ask/Stop |
| `sessionState` | `state.go` | State machine: Uninitialized→Starting→Ready⇆Processing→Closed |
| `turnManager` | `turn.go` | Per-turn text/thinking/tool accumulation, waiter notification |
| `streamAccumulator` | `accumulator.go` | Converts `StreamEvent` deltas into `Event` emissions |
| `processManager` | `process.go` | Subprocess spawn, stdin/stdout/stderr I/O |
| `permissionManager` | `permission.go` | Routes permission control requests to `PermissionHandler` |
| `sessionRecorder` | `recorder.go` | Records all messages for replay |
| MCP handling | `session.go:handleMCPMessage` | Routes MCP JSON-RPC to `SDKToolHandler` implementations |

**Session lifecycle (Claude):**
```
NewSession(opts)
  │
  ▼
Start(ctx)
  ├─ processManager.Start()          spawn CLI subprocess
  ├─ state.Transition(Started)       Uninitialized → Starting
  ├─ go readLoop()                   background NDJSON reader
  ├─ sendInitialize()                SDK↔CLI handshake + MCP setup
  │    └─ CLI sends system{init}
  │    └─ state.Transition(InitReceived)  Starting → Ready
  │    └─ emit(ReadyEvent)
  ▼
SendMessage(ctx, content)
  ├─ turnManager.StartTurn()
  ├─ process.WriteMessage(UserMessageToSend)
  ├─ state.Transition(UserMessageSent)  Ready → Processing
  │
  │  [readLoop handles StreamEvents, AssistantMessage, UserMessage]
  │
  ├─ handleResult(ResultMessage)
  │    ├─ state.Transition(ResultReceived)  Processing → Ready
  │    ├─ emit(TurnCompleteEvent)
  │    └─ turnManager.CompleteTurn(result)  unblocks WaitForTurn
  ▼
Stop()
  ├─ cancel context
  ├─ close(done)
  ├─ process.Stop()
  ├─ state.Transition(Closed)
  └─ close(events)
```

**Control request dispatch:**
```
readLoop receives control_request
  │
  ├─ MCP message?  ──► handleMCPMessage (initialize / tools/list / tools/call)
  ├─ AskUserQuestion? ──► InteractiveToolHandler.HandleAskUserQuestion
  ├─ ExitPlanMode?  ──► InteractiveToolHandler.HandleExitPlanMode
  └─ Permission?    ──► permissionManager.HandleRequest
```

### L3: Event Bridge

**Packages:** `agent-cli-wrapper/agentstream/`, `multiagent/agent/bridge.go`

**Responsibility:** Normalize SDK-specific event types into a common vocabulary so the
provider layer doesn't need to know which CLI is underneath.

**Design:** The `agentstream` package defines trait interfaces (`Event`, `Text`,
`ToolStart`, `ToolEnd`, `TurnComplete`, `Error`, `Scoped`). Each SDK event type
implements the relevant traits. The generic `bridgeEvents[E]` function reads from any
`<-chan E`, type-asserts to `agentstream.Event`, and dispatches to both an
`EventHandler` callback and an `AgentEvent` channel.

```
claude.Event ──implements──► agentstream.Event ──bridgeEvents──► AgentEvent / EventHandler
acp.Event    ──implements──► agentstream.Event ──bridgeEvents──► AgentEvent / EventHandler
codex.Event  ──implements──► agentstream.Event ──bridgeEvents──► AgentEvent / EventHandler
```

**Key features:**
- **Scope filtering:** Events implementing `Scoped` can be filtered by ID (used by
  Codex's multiplexed thread model).
- **Turn-complete callback:** Optional `onTurnComplete` fires once on the first
  `KindTurnComplete` event, used for synchronization (e.g., Gemini's drain pattern).

### L4: Provider Abstraction

**Package:** `multiagent/agent/`

**Responsibility:** Define the pluggable backend contract. Any new LLM backend
implements `Provider` (one-shot) or `LongRunningProvider` (persistent session).

**Core interfaces:**
```go
Provider {
    Name() string
    Execute(ctx, prompt, wtCtx, ...ExecuteOption) (*AgentResult, error)
    Events() <-chan AgentEvent
    Close() error
}

LongRunningProvider extends Provider {
    Start(ctx) error
    SendMessage(ctx, message) (*AgentResult, error)
    Stop() error
}
```

**Implementations:**
| Provider | File | Backend |
|----------|------|---------|
| `ClaudeProvider` | `claude_provider.go` | Ephemeral `claude.Session` per Execute |
| `ClaudeLongRunningProvider` | `claude_provider.go` | Persistent `claude.Session` |
| `GeminiProvider` | `gemini_provider.go` | `acp.Client` with persistent bridge |
| `CodexProvider` | `codex_provider.go` | `codex.Client` with thread-based execution |

**Session wrappers (higher-level convenience):**
| Type | File | Purpose |
|------|------|---------|
| `LongRunningSession` | `session.go` | Wraps `claude.Session` for orchestrator/planner agents; lazy init, cost tracking |
| `EphemeralSession` | `session.go` | Creates fresh session per task; file tracking via events or git diff |

### L5: Session Orchestration

**Package:** `bramble/session/`

**Responsibility:** Manage the lifecycle of user-visible sessions in the TUI. Creates
the appropriate runner, routes events to display output, handles follow-up messages.

**Key types:**
| Type | File | Purpose |
|------|------|---------|
| `Manager` | `manager.go` | Creates sessions, dispatches to runners, manages output lines |
| `sessionRunner` | `manager.go` | Interface: `Start`, `RunTurn`, `Stop` |
| `plannerRunner` | `manager.go` | Adapts `PlannerWrapper` |
| `builderRunner` | `manager.go` | Adapts `BuilderSession` |
| `providerRunner` | `manager.go` | Adapts `agent.Provider` via event bridge |
| `sessionEventHandler` | `event_handler.go` | Converts `EventHandler` callbacks → `OutputLine` entries |
| `Session` / `SessionInfo` | `types.go` | Session metadata, status, progress tracking |

**Runner selection flow:**
```
Manager.createRunner(session)
  │
  ├─ Provider configured?  ──► providerRunner (Claude/Codex/Gemini via agent.Provider)
  ├─ SessionTypePlanner?   ──► plannerRunner (wraps planner.PlannerWrapper)
  └─ SessionTypeBuilder?   ──► builderRunner (wraps yoloswe.BuilderSession)
```

### L6: Presentation

**Packages:** `bramble/app/`, `bramble/replay/`, `bramble/cmd/`

**Responsibility:** Render session output in the terminal (Bubble Tea TUI), manage
tmux windows, support session replay.

---

## 3. Data Flow: End-to-End Turn

```
User types prompt in TUI
  │
  ▼
bramble/app ──► Manager.SendFollowUp(sessionID, message)
  │
  ▼
Manager ──► sessionRunner.RunTurn(ctx, message)
  │
  ├─[builderRunner] ──► yoloswe.BuilderSession.RunTurn()
  │                        └─► claude.Session.Ask()
  │
  ├─[plannerRunner] ──► planner.PlannerWrapper.RunTurn()
  │                        └─► claude.Session.Ask()
  │
  └─[providerRunner] ──► agent.Provider.Execute()
                            └─► claude.Session.Ask()  (or acp/codex equivalent)
                                  │
                                  ▼
                            processManager.WriteMessage(UserMessageToSend)
                                  │
                              ┌───┴───┐
                              │CLI    │  Claude CLI subprocess
                              └───┬───┘
                                  │
                                  ▼ (NDJSON on stdout)
                            readLoop() ──► handleLine()
                              │
                              ├─ stream_event ──► accumulator ──► emit(TextEvent/ThinkingEvent)
                              ├─ assistant ──► handleAssistant ──► emit(ToolStartEvent/ToolCompleteEvent)
                              ├─ user ──► handleUser ──► emit(CLIToolResultEvent)
                              ├─ result ──► handleResult ──► emit(TurnCompleteEvent)
                              │                               └─► turnManager.CompleteTurn()
                              └─ control_request ──► permission/MCP/interactive handler
                                                       │
                                                       ▼
                                                  processManager.WriteMessage(ControlResponse)
```

**Event propagation path:**
```
claude.Session.events ──► bridgeEvents[claude.Event]
  │                          │
  │                          ├──► AgentEvent channel
  │                          └──► EventHandler callbacks
  │                                 │
  │                                 ▼
  │                          sessionEventHandler
  │                                 │
  │                                 ▼
  │                          Manager.addOutput(sessionID, OutputLine)
  │                                 │
  │                                 ▼
  │                          TUI renders OutputLine
  ▼
  (also consumed by: LongRunningSession.LoggingEvents,
   EphemeralSession.runSessionWithFileTracking)
```

---

## 4. Current Issues and Improvement Opportunities

### Issue 1: Dual Session Abstraction at L4/L5

**Problem:** There are two parallel session abstractions that don't share a common
interface:

1. `multiagent/agent/LongRunningSession` — wraps `claude.Session`, adds cost tracking,
   lazy init, logging. Used by multiagent orchestrator.
2. `bramble/session/Manager` + `sessionRunner` — manages TUI sessions, routes to
   plannerRunner/builderRunner/providerRunner.

The `LongRunningSession` is tightly coupled to `claude.Session` (not behind the
`Provider` interface), while `providerRunner` in bramble wraps `agent.Provider`.
This means the multiagent orchestrator and the TUI follow different code paths for
the same underlying operation.

**Recommendation:** Unify into a single `SessionManager` interface at L4 that both the
multiagent orchestrator and the TUI consume. The interface should expose:

```go
type SessionManager interface {
    Start(ctx context.Context) error
    SendMessage(ctx context.Context, message string) error
    Events() <-chan AgentEvent
    WaitForTurn(ctx context.Context) (*AgentResult, error)
    Stop() error
    Metrics() SessionMetrics  // cost, turn count, duration
}
```

Both `LongRunningSession` and `EphemeralSession` should implement this, backed by
any `Provider`. This eliminates the direct `claude.Session` dependency from L4.

---

### Issue 2: Event Type Proliferation

**Problem:** Events are defined at three separate layers with manual conversion between
them:

1. **L2 SDK events:** `claude.Event` (TextEvent, ToolStartEvent, etc.)
2. **L3 bridge traits:** `agentstream.Event` (KindText, KindToolStart, etc.)
3. **L4 agent events:** `AgentEvent` (TextAgentEvent, ToolStartAgentEvent, etc.)

Plus the display-layer `OutputLine` types at L5/L6. Each SDK event type must implement
the `agentstream` trait interfaces, and the bridge then constructs yet another set of
concrete types (`AgentEvent`). This creates a 3-deep conversion chain:

```
claude.TextEvent ──(trait methods)──► agentstream.Text ──(bridge)──► TextAgentEvent
                                                                       │
                                                                       ▼
                                                                   OutputLine
```

**Recommendation:** Collapse L3 and L4 into a single event vocabulary:

- Make `AgentEvent` the canonical event type at the bridge boundary.
- SDK events implement `agentstream.Event` traits (this is already clean).
- The bridge directly constructs `AgentEvent` values (it already does this).
- Remove the separate `EventHandler` callback interface; instead have consumers
  range over `<-chan AgentEvent` and switch on type. This is simpler, avoids the
  `trackingEventHandler` wrapper pattern, and is easier to test.

If callback-style consumption is still needed, provide a single adapter:

```go
func ForwardEvents(events <-chan AgentEvent, handler EventHandler) { ... }
```

---

### Issue 3: Recording and Metrics Scattered Across Layers

**Problem:** Session recording (`sessionRecorder` at L2) only captures protocol-level
messages. Cost tracking is duplicated:

- `claude.Session.cumulativeCostUSD` (L2)
- `LongRunningSession.totalCost` (L4)
- `EphemeralSession.totalCost` (L4)
- `SessionProgress.TotalCostUSD` (L5)

The `LongRunningSession.RecordUsage` comment explicitly warns about double-counting
between `LoggingEvents` and `WaitForTurn`.

**Recommendation:** Centralize metrics into a `SessionMetrics` struct owned by the
unified `SessionManager` (from Issue 1). Recording should be a decorator/middleware
at the provider boundary, not embedded inside the SDK:

```go
type RecordingProvider struct {
    inner    Provider
    recorder *Recorder
}
```

This makes recording composable and testable independently of any specific SDK.

---

### Issue 4: Permission/Interactive Tool Handling Mixed into Session Core

**Problem:** `claude/session.go` directly handles `AskUserQuestion` and `ExitPlanMode`
control requests with inline parsing and response building (lines 903-989). This mixes
protocol concerns (parsing tool input) with business logic (question/plan handling)
inside the session core.

**Recommendation:** Extract a `ControlDispatcher` that maps control request subtypes
to handler functions, similar to how HTTP routers work:

```go
type ControlDispatcher struct {
    handlers map[string]ControlHandler
}

type ControlHandler interface {
    Handle(ctx context.Context, requestID string, req *ToolUseRequest) (*ControlResponse, error)
}
```

Register handlers for `AskUserQuestion`, `ExitPlanMode`, and permissions as separate
implementations. The session's `handleControlRequest` becomes a one-liner delegation.

---

### Issue 5: `bramble/session` Has Direct SDK Dependencies

**Problem:** `bramble/session/manager.go` imports `claude`, `acp`, `codex`, and
`agent` packages directly. It constructs SDK sessions inline and has provider-specific
branching (`isClaudeModel`, `isCodexModel`, `isGeminiModel`). This means adding a new
provider requires touching the session manager.

**Recommendation:** The session manager should depend only on the `Provider` interface
(L4). Provider construction should be handled by a factory:

```go
type ProviderFactory interface {
    Create(model string, opts ProviderOptions) (Provider, error)
}
```

The factory encapsulates model→provider mapping (currently in `multiagent/agent/models.go`).
The session manager calls `factory.Create()` and works with the resulting `Provider`
without knowing whether it's Claude, Gemini, or Codex.

---

### Issue 6: No Clear Error Contract Across Providers

**Problem:** Each SDK defines its own error types (`claude.ProcessError`,
`acp.RPCError`, `codex.ProcessError`) with different recoverability semantics.
The provider layer has no common error taxonomy.

**Recommendation:** Define provider-level error categories:

```go
type ProviderError struct {
    Kind    ErrorKind   // Fatal, Transient, RateLimited, BudgetExceeded, Cancelled
    Cause   error
    Message string
}
```

Each provider maps its SDK-specific errors to these categories. Upper layers
(orchestrator, TUI) switch on `Kind` without knowing the underlying provider.

---

### Issue 7: State Machine Duplication

**Problem:** Three near-identical state machines exist:

- `claude/state.go`: `Uninitialized→Starting→Ready⇆Processing→Closed`
- `acp/state.go`: Client (`Uninitialized→Starting→Ready→Closed`) + Session (`Created→Ready⇆Processing→Closed`)
- `codex/state.go`: Client + Thread state machines

Each has its own `sync.RWMutex`, transition table, and error handling.

**Recommendation:** Extract a generic state machine:

```go
type StateMachine[S comparable] struct {
    transitions map[S]map[S]bool  // from → set of valid targets
    current     S
    mu          sync.RWMutex
}

func (sm *StateMachine[S]) Transition(target S) error { ... }
func (sm *StateMachine[S]) Current() S { ... }
```

Each SDK instantiates it with its own state enum. This eliminates ~150 lines of
duplicated code and ensures consistent behavior.

---

### Issue 8: `LongRunningSession` Exposes `claude.Session` Details

**Problem:** `LongRunningSession` exposes `Events() <-chan claude.Event` and
`Recording() *claude.SessionRecording` — these are Claude-specific types leaking
through what should be a provider-agnostic wrapper.

**Recommendation:** `LongRunningSession` should expose `Events() <-chan AgentEvent`
(post-bridge) and a provider-agnostic `Recording` type. The bridge should run inside
the session wrapper, not at the call site.

---

## 5. Proposed Target Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  L6  Presentation                                                │
│       Only depends on: SessionManager, AgentEvent, OutputLine    │
├──────────────────────────────────────────────────────────────────┤
│  L5  Session Orchestration                                       │
│       SessionManager interface                                   │
│       Depends on: Provider, AgentEvent, SessionMetrics           │
├──────────────────────────────────────────────────────────────────┤
│  L4  Provider Abstraction                                        │
│       Provider / LongRunningProvider interfaces                  │
│       ProviderFactory for model→provider dispatch                │
│       ProviderError for unified error taxonomy                   │
│       AgentEvent as the single event vocabulary                  │
│       RecordingProvider decorator                                │
├──────────────────────────────────────────────────────────────────┤
│  L3  Event Bridge (unchanged, internal to each provider impl)    │
│       agentstream traits + bridgeEvents[E]                       │
│       Runs inside provider, not exposed to upper layers          │
├──────────────────────────────────────────────────────────────────┤
│  L2  SDK Wrappers (unchanged, internal to each provider impl)    │
│       claude.Session, acp.Client, codex.Client                   │
│       ControlDispatcher for permission/MCP/interactive tools     │
│       Generic StateMachine[S]                                    │
├──────────────────────────────────────────────────────────────────┤
│  L1  Protocol / Transport (unchanged)                            │
│       NDJSON, JSON-RPC, content blocks                           │
└──────────────────────────────────────────────────────────────────┘
```

**Key changes:**
1. L3 becomes internal to provider implementations (not a visible layer boundary).
2. L4 gains `ProviderFactory`, `ProviderError`, and `RecordingProvider`.
3. L5 gains `SessionManager` interface consumed by both multiagent and TUI.
4. L6 depends only on L5 abstractions, never on SDK types.

---

## 6. Migration Path

The improvements can be implemented incrementally without breaking existing code:

| Phase | Change | Risk | Effort |
|-------|--------|------|--------|
| 1 | Extract generic `StateMachine[S]` | Low | Small — mechanical refactor |
| 2 | Extract `ControlDispatcher` from session.go | Low | Small — moves code without changing behavior |
| 3 | Define `ProviderError` taxonomy | Low | Medium — each provider maps errors |
| 4 | Create `ProviderFactory` | Low | Small — extracts existing model→provider logic |
| 5 | Create `RecordingProvider` decorator | Medium | Medium — must verify recording fidelity |
| 6 | Remove `EventHandler` interface, use channel-only | Medium | Medium — all consumers switch to channel pattern |
| 7 | Unify `SessionManager` interface | Medium | Large — aligns multiagent + bramble |
| 8 | Remove direct SDK imports from `bramble/session` | Medium | Large — depends on phases 4+7 |

---

## 7. Appendix: File Reference

### L1 Protocol
- `agent-cli-wrapper/protocol/messages.go` — Message types, content structures
- `agent-cli-wrapper/protocol/content_blocks.go` — Polymorphic content block parsing
- `agent-cli-wrapper/protocol/control.go` — Control request/response types
- `agent-cli-wrapper/protocol/stream.go` — Stream event structures
- `agent-cli-wrapper/protocol/mcp.go` — MCP tool definitions and JSON-RPC types
- `agent-cli-wrapper/protocol/parse.go` — Message discrimination and parsing
- `agent-cli-wrapper/internal/ndjson/` — NDJSON reader/writer

### L2 SDK Wrappers
- `agent-cli-wrapper/claude/session.go` — Claude session (1160 lines)
- `agent-cli-wrapper/claude/state.go` — State machine
- `agent-cli-wrapper/claude/turn.go` — Turn management
- `agent-cli-wrapper/claude/accumulator.go` — Stream event accumulation
- `agent-cli-wrapper/claude/process.go` — CLI subprocess management
- `agent-cli-wrapper/claude/permission.go` — Permission handling
- `agent-cli-wrapper/claude/recorder.go` — Session recording
- `agent-cli-wrapper/claude/events.go` — Event type definitions
- `agent-cli-wrapper/claude/errors.go` — Error types and recoverability
- `agent-cli-wrapper/claude/session_options.go` — Functional options
- `agent-cli-wrapper/claude/mcp.go` — MCP configuration
- `agent-cli-wrapper/claude/sdk_mcp.go` — SDK tool handler interface
- `agent-cli-wrapper/acp/client.go` — ACP/Gemini client
- `agent-cli-wrapper/acp/session.go` — ACP session
- `agent-cli-wrapper/acp/protocol.go` — ACP JSON-RPC protocol
- `agent-cli-wrapper/acp/state.go` — ACP state machines
- `agent-cli-wrapper/acp/handlers.go` — FS/Terminal/Permission handlers
- `agent-cli-wrapper/codex/client.go` — Codex client
- `agent-cli-wrapper/codex/state.go` — Codex state machines

### L3 Event Bridge
- `agent-cli-wrapper/agentstream/events.go` — Trait interfaces (Event, Text, ToolStart, etc.)
- `multiagent/agent/bridge.go` — Generic `bridgeEvents[E]` function

### L4 Provider Abstraction
- `multiagent/agent/provider.go` — Provider interface, AgentEvent types, EventHandler
- `multiagent/agent/claude_provider.go` — Claude provider implementations
- `multiagent/agent/gemini_provider.go` — Gemini provider
- `multiagent/agent/codex_provider.go` — Codex provider
- `multiagent/agent/session.go` — LongRunningSession, EphemeralSession

### L5 Session Orchestration
- `bramble/session/manager.go` — Session manager (1300+ lines)
- `bramble/session/types.go` — Session, SessionProgress, OutputLine types
- `bramble/session/event_handler.go` — Event→OutputLine conversion
- `bramble/session/store.go` — Session persistence

### L6 Presentation
- `bramble/app/` — Bubble Tea TUI application
- `bramble/replay/` — Session replay (claude.go, codex.go, compact.go)
- `bramble/session/tmux_runner.go` — Tmux integration
