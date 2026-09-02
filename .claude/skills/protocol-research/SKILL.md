---
name: protocol-research
description: >
  Investigate and document CLI protocol behavior for Claude, Codex, Gemini (ACP), and Agy agent CLIs.
disable-model-invocation: true
---

# Protocol Research

Investigate agent CLI subprocess protocol behavior using the `agent-cli-wrapper` SDKs, trace capture, and Go test infrastructure. This repo communicates with agent CLIs via subprocess stdio — each with different wire protocols: Claude (NDJSON), Codex (JSON-RPC), the real `gemini` CLI in `--experimental-acp` mode (JSON-RPC/ACP, still wrapped directly by `agent-cli-wrapper/acp/` for standalone consumers like `yoloswe/reviewer/backend_gemini.go` and `bramble/sessionanalysis/summarize.go`), and Agy (a separate print-mode CLI, `agent-cli-wrapper/agy/`, wrapped by `multiagent/agent.AgyProvider` — no persistent JSON-RPC/streaming protocol, just one-shot subprocess invocation with parsed stdout). Note: `multiagent/agent.ProviderGemini` itself has been deleted — model IDs starting with `gemini-` now route to `AgyProvider`, not to the ACP/`gemini` CLI path described in this doc. The ACP protocol content below documents the `acp` package's own still-live wire protocol for its remaining direct consumers, not the deleted provider bridge. Understanding real protocol behavior is essential before building features that depend on message ordering, field presence, or event sequencing.

## Arguments

```
/protocol-research --provider <claude|codex|gemini|agy|all> --scenario <name> [--capture] [--compare]
```

| Flag | Description | Default |
|------|-------------|---------|
| `--provider` | Which CLI protocol to investigate | required |
| `--scenario` | Scenario to investigate (e.g., `mcp-handshake`, `tool-permission`, `multi-turn`) | required |
| `--capture` | Capture real protocol output by running a test session | false |
| `--compare` | Compare behavior across providers for the same scenario | false |

## Provider Protocol Overview

Before investigating, understand what you're looking at:

| Provider | Wire Format | Transport | Key Types Location |
|----------|-------------|-----------|-------------------|
| **Claude** | NDJSON (stream-json) | stdin/stdout | `agent-cli-wrapper/protocol/` and `agent-cli-wrapper/claude/` |
| **Codex** | JSON-RPC 2.0 | stdin/stdout | `agent-cli-wrapper/codex/` |
| **Gemini** (real `gemini --experimental-acp` CLI, standalone consumers only — not `multiagent/agent`) | JSON-RPC 2.0 (ACP) | stdin/stdout | `agent-cli-wrapper/acp/` |
| **Agy** | one-shot print-mode invocation, parsed stdout (no JSON-RPC/streaming protocol) | subprocess exec | `agent-cli-wrapper/agy/` |

Claude, Codex, and Agy are unified through the `agentstream`/`Provider` interfaces at `agent-cli-wrapper/agentstream/` and `multiagent/agent/`. The `acp`/Gemini path is not — it's used directly by standalone consumers outside `multiagent/agent`.

## Workflow

### 1. Check existing documentation

Before capturing anything, check what's already known:

- `agent-cli-wrapper/claude/SDK_PROTOCOL.md` — Claude protocol lifecycle, MCP handshake, gotchas
- `agent-cli-wrapper/README.md` — Agentstream interface mapping across providers
- `agent-cli-wrapper/protocol/` — All Claude wire types (messages, stream events, control requests, MCP)
- `agent-cli-wrapper/codex/jsonrpc.go` — Codex JSON-RPC methods and notification types
- `agent-cli-wrapper/acp/protocol.go` — ACP/Gemini request/response types

### 2. Check existing trace data and tests

Look for test fixtures that already demonstrate the behavior:

```
agent-cli-wrapper/protocol/testdata/traces/    — Real CLI trace files (from_cli.jsonl, to_cli.jsonl)
agent-cli-wrapper/protocol/parse_test.go       — Protocol message parsing validation
agent-cli-wrapper/protocol/trace_test.go       — Trace file parsing and event counting
agent-cli-wrapper/claude/recorder.go           — Session recording (messages.jsonl format)
```

Also check integration tests that exercise real provider behavior:
```
multiagent/agent/integration/provider_conformance_test.go  — Cross-provider conformance suite
bramble/session/integration/                               — Session lifecycle tests
```

### 3. Write a targeted test (if needed)

If existing traces and tests don't cover the scenario, write a Go test that captures the specific behavior. This serves dual purpose — it validates your understanding AND becomes permanent test coverage.

Place the test in the appropriate package:
- Protocol parsing behavior → `agent-cli-wrapper/protocol/`
- SDK session lifecycle → `agent-cli-wrapper/claude/` (or `codex/`, `acp/`)
- Event bridging behavior → `multiagent/agent/`
- Cross-provider behavior → `multiagent/agent/integration/`

Follow the repo's test conventions:
- Use `require.Eventually()` for async conditions, never `time.Sleep()`
- Use random ports, no static ports
- Integration tests go in `integration/` directories with `# gazelle:ignore` BUILD.bazel
- Include `//go:build integration` tag for tests that need real CLI binaries

### 4. Capture real protocol output (if --capture)

For Claude, use the recording infrastructure:

```go
// The claude.SDK already records sessions when configured
// See agent-cli-wrapper/claude/recorder.go
// Output: .claude-sessions/session-<id>-<ts>/messages.jsonl
```

Each recorded message includes:
```json
{"timestamp": 1234567890123, "direction": "sent|received", "message": {...}}
```

For Codex, enable protocol logging:
```
bramble --protocol-log-dir /tmp/protocol-logs
```

This captures raw JSON-RPC exchanges to files for analysis.

### 5. Analyze the protocol exchange

For the specific scenario, document:

- **Message sequence**: What messages appear and in what order?
- **Required fields**: Which fields are always present vs. optional?
- **Timing**: Are there synchronization points (e.g., must wait for response before proceeding)?
- **Error paths**: What happens when things go wrong?
- **Provider differences**: How does the same logical operation differ across providers?

### 6. Cross-validate with agentstream bridge (if --compare)

Verify that the `agentstream` event interfaces correctly translate provider-specific events:

```
agent-cli-wrapper/agentstream/event.go  — Event kind definitions and interfaces
multiagent/agent/bridge.go              — Generic bridgeEvents[E any]() function
```

For each event in the scenario, trace:
1. Provider SDK emits typed event (e.g., `claude.ToolStartEvent`)
2. Event implements agentstream interface (e.g., `agentstream.ToolStart`)
3. Bridge translates to `AgentEvent` (e.g., `ToolStartAgentEvent`)
4. Consumer receives provider-agnostic event

Check for events that are provider-specific and NOT bridged (intentionally skipped by the generic bridge).

### 7. Document findings

Update or create documentation:

- For protocol-level findings: update the relevant SDK package docs (e.g., `SDK_PROTOCOL.md`)
- For cross-provider findings: update `agent-cli-wrapper/README.md`
- For integration-level findings: add to test comments or create a new reference doc

## Key Protocol Concepts

### Claude NDJSON Protocol
- **Session lifecycle**: Process start → `sendInitialize` control request → MCP handshake (interleaved) → system init message → ready
- **Stream events**: `message_start` → `content_block_start` → `content_block_delta`(s) → `content_block_stop` → `message_delta` → `message_stop`
- **Control flow**: `control_request` (stdout) / `control_response` (stdin) for permissions, MCP, interactive tools
- **Critical gotcha**: MCP config MUST include `name` field or CLI silently hangs

### Codex JSON-RPC Protocol
- **Thread model**: Each execution creates a thread; events scoped by thread ID
- **Methods**: `Initialize`, `ThreadStart`, `TurnStart`, `TurnInterrupt`
- **Notifications**: `thread/started`, `turn/started`, `turn/completed`, `item/started`, `item/completed`, `codex/event/*`
- **Scoped filtering**: Use `ScopeID()` on events to filter by thread

### Gemini ACP Protocol (still-live `acp` package; not what Agy speaks)
- **Session model**: Initialize → NewSession → Prompt cycles
- **Session updates**: Discriminated union with subtypes: `agent_message_chunk`, `agent_thought_chunk`, `tool_call`, `tool_call_result`, `plan_update`
- **Permission handling**: `RequestPermissionRequest` for tool approval
- **Tool status lifecycle**: `running` → `completed` | `errored`
- This is the real `gemini --experimental-acp` CLI's wire protocol, wrapped by
  `agent-cli-wrapper/acp/`. `multiagent/agent.ProviderGemini` (which used to bridge
  this into the `Provider` interface) has been deleted; the package itself is
  still used directly by `yoloswe/reviewer/backend_gemini.go` and
  `bramble/sessionanalysis/summarize.go`, unrelated to the agy migration.

### Agy Protocol
- **Session model**: no persistent session — one subprocess invocation (`agy -p
  "<prompt>" [--model ...] [--effort ...] [--print-timeout ...] ...`) per `Execute`
  call, always requesting `--output-format json`; the JSON envelope is parsed for
  the response, `conversation_id`, and `usage`. A caller may supply `--conversation`
  to resume an existing agy conversation, and `AgyProvider.Execute` now returns the
  turn's `conversation_id` as `AgentResult.SessionID`, which `providerRunner` feeds
  back in as the next turn's `ResumeSessionID` — so higher-level bramble sessions
  resume automatically without a long-running provider.
- **Events**: `TextEvent`, `TurnCompleteEvent`, `ErrorEvent` only
  (`agent-cli-wrapper/agy/events.go`) — no tool-call or thinking events, no
  JSON-RPC/streaming wire format to trace the way Claude/Codex/ACP have.
- **Effort**: `--effort low|medium|high` is a plain pass-through CLI flag
  (`agy.WithEffort`), no validation/normalization in the wrapper itself.

### Agentstream Event Mapping
| agentstream Kind | Claude Event | Codex Event | Agy Event |
|------------------|-------------|-------------|--------------|
| `KindText` | `TextEvent` | `TextDeltaEvent` | `TextEvent` |
| `KindThinking` | `ThinkingEvent` | `ReasoningDeltaEvent` | not emitted |
| `KindToolStart` | `ToolStartEvent` | `CommandStartEvent` | not emitted |
| `KindToolEnd` | `ToolCompleteEvent` | `CommandEndEvent` | not emitted |
| `KindTurnComplete` | `TurnCompleteEvent` | `TurnCompletedEvent` | `TurnCompleteEvent` |
| `KindError` | `ErrorEvent` | `ErrorEvent` | `ErrorEvent` |

Agy's wrapper (`agent-cli-wrapper/agy/events.go`) only defines `TextEvent`,
`TurnCompleteEvent`, and `ErrorEvent` — print mode exposes no thinking or
tool-call events at all, unlike Claude/Codex and the historical ACP/Gemini
path (kept here for reference, not agentstream-bridged):

| agentstream Kind | Gemini (ACP) Event, historical reference only |
|------------------|--------------|
| `KindText` | `TextDeltaEvent` |
| `KindThinking` | `ThinkingDeltaEvent` |
| `KindToolStart` | `ToolCallStartEvent` |
| `KindToolEnd` | `ToolCallUpdateEvent` (completed/errored) |
| `KindTurnComplete` | `TurnCompleteEvent` |
| `KindError` | `ErrorEvent` |

## Reference Files

| File | What It Contains |
|------|-----------------|
| `agent-cli-wrapper/claude/SDK_PROTOCOL.md` | Claude protocol lifecycle, MCP handshake, known gotchas |
| `agent-cli-wrapper/README.md` | Agentstream interface design and cross-provider event mapping |
| `agent-cli-wrapper/protocol/types.go` | All Claude wire types (messages, events, control, MCP) |
| `agent-cli-wrapper/codex/jsonrpc.go` | Codex JSON-RPC methods and event types |
| `agent-cli-wrapper/acp/protocol.go` | ACP/Gemini protocol types |
| `multiagent/agent/integration/provider_conformance_test.go` | Cross-provider test expectations |
