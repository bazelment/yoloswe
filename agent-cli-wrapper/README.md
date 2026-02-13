# agent-cli-wrapper

Go SDK packages for interacting with agent CLI subprocesses. Each package wraps
a specific agent binary and emits typed events on a Go channel.

| Package | Binary | Protocol | Channel type |
|---------|--------|----------|-------------|
| `claude` | `claude` CLI | JSON streaming | `<-chan claude.Event` |
| `acp` | `gemini --experimental-acp` | JSON-RPC over stdio | `<-chan acp.Event` |
| `codex` | `codex app-server` | JSON-RPC over stdio | `<-chan codex.Event` |

Supporting packages:

- `agentstream` — shared streaming event interface (see below)
- `protocol` — common protocol types
- `internal/` — shared internal helpers (ndjson, procattr)

## Streaming event interface (`agentstream`)

The `agentstream` package defines a narrow set of interfaces that SDK event
types optionally implement. The provider bridge in `multiagent/agent` uses
these interfaces to translate SDK events into provider-agnostic `AgentEvent`
values via a single generic function (`bridgeEvents[E any]`), replacing what
used to be three separate per-provider bridge functions.

### Event kinds

Six event kinds cover the common subset needed by the provider layer:

| Kind | Interface | Description |
|------|-----------|-------------|
| `KindText` | `Text` | Streaming text delta |
| `KindThinking` | `Text` | Chain-of-thought / reasoning delta |
| `KindToolStart` | `ToolStart` | Tool invocation started |
| `KindToolEnd` | `ToolEnd` | Tool invocation completed |
| `KindTurnComplete` | `TurnComplete` | Agent turn finished |
| `KindError` | `Error` | Error occurred |

`KindUnknown` (zero value) is returned by events that conditionally map to a
common kind. For example, ACP's `ToolCallUpdateEvent` returns `KindToolEnd`
only when its status is "completed" or "errored"; otherwise it returns
`KindUnknown` and the bridge skips it.

### How it works

Each SDK event type that participates in the generic bridge implements
`agentstream.Event` (a single method: `StreamEventKind() EventKind`) plus
one of the data interfaces (`Text`, `ToolStart`, `ToolEnd`, `TurnComplete`,
`Error`). SDK-specific events like `codex.CommandOutputEvent` or
`claude.CLIToolResultEvent` do **not** implement these interfaces and are
silently skipped by the bridge.

All interface method names use a `Stream` prefix (e.g., `StreamDelta()`,
`StreamToolName()`) to avoid conflicts with SDK struct field names.

The optional `Scoped` interface enables per-scope event filtering (e.g.,
codex thread ID filtering) without provider-specific code in the bridge.

### Direct SDK consumers

The `agentstream` interfaces are additive. SDK channels still return their
own typed events. Direct consumers (e.g., `builder.go`, `planner.go`,
`reviewer.go`) continue to type-switch on concrete SDK types with full field
access. The generic bridge is only used by the `multiagent/agent` provider
layer.

## Future improvements

- **Collapse `AgentEvent`**: Once all providers use `bridgeEvents`, the
  `AgentEvent` types in `multiagent/agent/provider.go` become redundant.
  Consumers could use `agentstream` interfaces directly, eliminating the
  double-event-type layer.

- **Add `KindToolOutput`**: If streaming tool output becomes a cross-provider
  concept, add a new `EventKind` and interface. SDKs that support it
  implement the interface; others don't.

- **Relocate `MappedEvent`**: The codex package retains `MappedEvent` and
  `ParseMappedNotification` for session log replay (`codexlogview`). These
  are independent of `agentstream` and could be moved into `codexlogview`
  since it is the sole consumer.

- **Unify SDK channel types**: If all SDK event types implement
  `agentstream.Event` (returning `KindUnknown` for SDK-specific ones), the
  SDK channels could return `<-chan agentstream.Event` directly, eliminating
  the `any(ev).(agentstream.Event)` type assertion in the bridge. This would
  require direct consumers to type-assert back to concrete types.
