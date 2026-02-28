# Session Data Model: MVC Architecture for bramble/sessionmodel

## Problem

The bramble TUI renders Claude sessions. Data flows from the Claude CLI process
through NDJSON on stdio, gets parsed, and reaches the TUI. User actions flow
back. This pipeline had two structural problems:

1. **Two inconsistent event paths** — live sessions (via `claude.Session` events
   and `sessionEventHandler`) and replay sessions (via `claudeReplayParser`)
   each implemented their own parsing/accumulation logic with different event
   granularity and different output line production.

2. **No comprehensive data model** — the parsing layer didn't cover the full
   Claude message vocabulary visible in raw `.jsonl` session logs. Adding a new
   format (e.g. the native `~/.claude/projects/` JSONL) would require writing
   yet another independent parser.

## Key Insight: Three Formats, One Vocabulary

All three serialization formats encountered in practice share the **same inner
message vocabulary**. The only difference is the envelope:

| Format | Envelope shape | Where used |
|--------|----------------|------------|
| **Live NDJSON** | Bare `{type, message, session_id, uuid, ...}` | `claude.Session.readLoop()` via stdio |
| **SDK recorder** | `{timestamp, direction, message: <bare msg>}` | `.claude-sessions/` replay logs |
| **Raw JSONL** | `{type, parentUuid, isSidechain, gitBranch, ..., message: <inner>}` | `~/.claude/projects/*.jsonl` |

The common vocabulary (defined in `agent-cli-wrapper/protocol/`):

- **Message types**: `system`, `assistant`, `user`, `result`, `stream_event`,
  `control_request`, `control_response`
- **Content block types**: `text`, `thinking`, `tool_use`, `tool_result`
- **Stream event types**: `message_start`, `content_block_start`,
  `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`
- **Raw JSONL extras**: `file-history-snapshot`, `queue-operation`, `progress`
  (envelope-level types with no inner message)

## Architecture

A three-layer pipeline where each layer has a single responsibility:

```
┌────────────────────────────────────────────────────────────┐
│  VIEW  (bramble/app/)                                       │
│  Reads SessionModel.Output() snapshot                       │
│  Renders OutputLine[] → terminal                            │
│  Sends user actions → Controller                            │
└──────────────────────┬─────────────────────────────────────┘
                       │ reads
┌──────────────────────▼─────────────────────────────────────┐
│  MODEL  (bramble/sessionmodel/)                              │
│                                                              │
│  SessionModel: single source of truth per session            │
│  ├─ SessionMeta, OutputBuffer, ProgressSnapshot              │
│  ├─ Observer notifications on mutation                       │
│  │                                                           │
│  MessageParser: vocabulary messages → model mutations         │
│  ├─ Handles system/assistant/user/result/stream_event        │
│  ├─ Stream accumulator for content_block_delta assembly      │
│  │                                                           │
│  Envelope strippers (thin adapters):                         │
│  ├─ FromLiveNDJSON(line) → protocol.Message                  │
│  ├─ FromSDKRecorder(line) → protocol.Message + timestamp     │
│  └─ FromRawJSONL(line) → protocol.Message + envelope meta    │
│                                                              │
│  Event adapters (transition bridges):                        │
│  ├─ FromClaudeEvent(model, event) — claude.Event → model     │
│  └─ FromAgentEvent(model, event) — agent.AgentEvent → model  │
└──────────────────────▲─────────────────────────────────────┘
                       │ writes
┌──────────────────────┴─────────────────────────────────────┐
│  CONTROLLER  (bramble/session/ Manager)                      │
│  Session lifecycle: start, stop, follow-up                   │
│  Creates SessionModel per session                            │
│  Feeds events from claude.Session or replay files            │
│  Routes user responses back to Claude process                │
└────────────────────────────────────────────────────────────┘
```

## Package: bramble/sessionmodel

### File Layout

```
bramble/sessionmodel/
├── types.go           Canonical type definitions (OutputLine, SessionStatus, etc.)
├── output_buffer.go   Thread-safe capped ring buffer
├── model.go           SessionModel with read/write API and observer pattern
├── parser.go          MessageParser: protocol.Message → model mutations
├── envelope.go        Three envelope strippers (live, recorder, raw JSONL)
├── event_adapter.go   Bridges from claude.Event and agent.AgentEvent
└── format.go          FormatToolContent and display helpers
```

### types.go — Canonical Type Definitions

This file is the single source of truth for display-layer types. The
`bramble/session` package re-exports these via type aliases.

```go
// Core display unit
type OutputLine struct { ... }
type OutputLineType string   // "text", "thinking", "tool_start", "error", etc.
type ToolState string        // "running", "complete", "error"

// Session lifecycle
type SessionStatus string    // "pending", "running", "idle", "completed", etc.
type SessionID string

// Metadata
type SessionMeta struct { SessionID, Model, CWD, Tools, ... }
type ProgressSnapshot struct { TurnCount, TotalCostUSD, CurrentTool, ... }

// Raw JSONL envelope metadata
type RawEnvelopeMeta struct { ParentUUID, IsSidechain, GitBranch, ... }

// Observer pattern
type Observer interface { OnModelEvent(ModelEvent) }
type ModelEvent interface { modelEvent() }
// Concrete events: OutputAppended, StatusChanged, ProgressUpdated, MetaUpdated
```

**Dependency direction:** `sessionmodel` has no imports from `bramble/session`.
Instead, `bramble/session/types.go` imports `sessionmodel` and creates aliases:

```go
// bramble/session/types.go
type OutputLine = sessionmodel.OutputLine
type SessionStatus = sessionmodel.SessionStatus
const StatusRunning = sessionmodel.StatusRunning
// ...
```

This breaks the dependency cycle that would otherwise occur when `session`
imports `sessionmodel` and `sessionmodel` imports `session`.

### output_buffer.go — Thread-Safe Capped Buffer

Extracted from `Manager.addOutput`, `appendOrAddOutput`, and `updateToolOutput`:

```go
type OutputBuffer struct {
    lines []OutputLine
    max   int           // ring buffer cap (default 1000)
    mu    sync.RWMutex
}

func (b *OutputBuffer) Append(line OutputLine)
func (b *OutputBuffer) AppendStreamingText(delta string)
func (b *OutputBuffer) AppendStreamingThinking(delta string)
func (b *OutputBuffer) UpdateToolByID(toolID string, fn func(*OutputLine)) bool
func (b *OutputBuffer) Snapshot() []OutputLine  // deep-copied
```

Key behaviors:
- **Ring buffer**: evicts oldest line when at capacity
- **Streaming append**: text/thinking deltas accumulate into the last line of
  matching type (plain concatenation, no overlap removal — deltas are
  non-overlapping token chunks in live mode)
- **Tool update**: copy-on-write mutation by tool ID, searching from end
- **Snapshot**: every line deep-copied via `DeepCopyOutputLine` to prevent
  shared mutable state

### model.go — SessionModel

Single source of truth for one session's display state:

```go
type SessionModel struct {
    meta      SessionMeta
    output    *OutputBuffer
    progress  ProgressSnapshot
    mu        sync.RWMutex
    observers []Observer
}
```

**Write API** (called by parser/controller):
- `SetMeta(meta)` — from system{init}
- `UpdateStatus(status)` — lifecycle transitions
- `AppendOutput(line)` — complete output line
- `AppendStreamingText(delta)` / `AppendStreamingThinking(delta)`
- `UpdateTool(toolID, fn)` — in-place tool state change
- `UpdateProgress(fn)` — token counts, cost, current tool

**Read API** (called by view):
- `Meta() SessionMeta`
- `Output() []OutputLine` — deep-copied snapshot
- `Progress() ProgressSnapshot`

Every write method notifies registered observers synchronously. The observer
pattern enables future Bubble Tea integration via a channel adapter:

```go
func observerChannel(model *SessionModel) <-chan ModelEvent {
    ch := make(chan ModelEvent, 100)
    model.AddObserver(channelObserver{ch})
    return ch
}
```

### parser.go — MessageParser

The core component. All three wire formats funnel through `HandleMessage`
after envelope stripping:

```go
type MessageParser struct {
    model  *SessionModel
    blocks map[int]*blockState  // stream accumulator state
}

func (p *MessageParser) HandleMessage(msg protocol.Message)
```

Internal dispatch:

| Message type | Handler | Model mutation |
|-------------|---------|----------------|
| `system` (init) | `handleSystem` | `SetMeta()` |
| `assistant` | `handleAssistant` | Walk content blocks → `AppendOutput()` per block |
| `user` | `handleUser` | Find `tool_result` blocks → `UpdateTool()` |
| `result` | `handleResult` | `UpdateProgress()` + `AppendOutput(turn_end)` |
| `stream_event` | `handleStreamEvent` | Accumulate deltas → `AppendStreamingText/Thinking`, track tool input |

The stream accumulator (ported from `claude/accumulator.go`) maintains per-block
state:

```go
type blockState struct {
    blockType   string  // "text", "thinking", "tool_use"
    text        string
    thinking    string
    toolID      string
    toolName    string
    partialJSON string  // accumulated tool input JSON
    index       int
}
```

Stream event flow:
1. `content_block_start` → create `blockState`, emit `OutputLine{ToolStart}` for tool_use
2. `content_block_delta` → accumulate text/thinking/JSON into block state,
   call `AppendStreamingText/Thinking` for text/thinking deltas
3. `content_block_stop` → for tool_use: parse accumulated JSON, call
   `UpdateTool` to backfill input on the tool_start line

### envelope.go — Format-Specific Envelope Strippers

Three thin functions that normalize different wire formats into
`protocol.Message`:

**`FromLiveNDJSON(line []byte)`** — pass-through to `protocol.ParseMessage()`.
The bare NDJSON line is already a vocabulary message.

**`FromSDKRecorder(line []byte)`** — strips `{timestamp, direction, message}`
envelope. Returns the message, timestamp, and direction.

**`FromRawJSONL(line []byte)`** — strips the `~/.claude/projects/` envelope.
Extracts the inner `message` field, injects the outer `type` field to
reconstruct a vocabulary-compatible message, then calls
`protocol.ParseMessage()`. Also returns `RawEnvelopeMeta` with envelope-level
fields (parentUuid, isSidechain, gitBranch, toolUseResult).

For raw JSONL-only types (`file-history-snapshot`, `queue-operation`,
`progress`): returns nil message with metadata only.

### event_adapter.go — Transition Bridges

During the transition period where `claude.Session` still emits its own event
types, two adapter functions map legacy events to model mutations:

```go
func FromClaudeEvent(model *SessionModel, event claude.Event)
func FromAgentEvent(model *SessionModel, event agent.AgentEvent)
```

These replace `bramble/session/event_handler.go` (`sessionEventHandler`).

| claude.Event | Model mutation |
|-------------|----------------|
| `ReadyEvent` | `SetMeta()` |
| `TextEvent` | `AppendStreamingText(e.Text)` |
| `ThinkingEvent` | `AppendStreamingThinking(e.Thinking)` |
| `ToolStartEvent` | `AppendOutput(tool_start)` + `UpdateProgress(tool)` |
| `ToolCompleteEvent` | `UpdateTool(backfill input)` |
| `CLIToolResultEvent` | `UpdateTool(result, state, duration)` + `UpdateProgress(clear)` |
| `TurnCompleteEvent` | `UpdateProgress(counts)` + `AppendOutput(turn_end)` |
| `ErrorEvent` | `AppendOutput(error)` |

The `FromAgentEvent` function handles the same vocabulary via
`agent.AgentEvent` types (used by Codex/Gemini providers).

## Integration with Manager

`bramble/session/manager.go` creates a `SessionModel` per session alongside the
existing `outputs` map:

```go
type Manager struct {
    sessions map[SessionID]*Session
    outputs  map[SessionID][]OutputLine             // legacy path
    models   map[SessionID]*sessionmodel.SessionModel  // new model
    // ...
}
```

Models are created in `StartSession` and `TrackTmuxWindow`, and cleaned up in
all lifecycle paths (delete, tmux window disappearance, etc.).

`GetSessionModel(id)` provides direct access to the model for callers that want
the new API.

## Loading a Session File

`LoadFromRawJSONL` in `loader.go` loads `~/.claude/projects/` JSONL files
through the full pipeline:

```go
func LoadFromRawJSONL(path string) (*SessionModel, error) {
    model := NewSessionModel(0)
    parser := NewMessageParser(model)
    for line := range scanLines(path) {
        msg, meta, _ := FromRawJSONL(line)
        if msg != nil {
            parser.HandleMessage(msg)
        }
        if meta != nil && msg == nil {
            handleEnvelopeMeta(model, meta)
        }
    }
    return model, nil
}
```

`handleEnvelopeMeta` converts envelope-only types into synthetic OutputLines:

| Envelope type | Subtype/variant | OutputLine produced |
|---------------|-----------------|---------------------|
| `system` | `api_error` | `OutputTypeError` with error code |
| `system` | `turn_duration` | `OutputTypeStatus` with duration |
| `system` | `compact_boundary` | `OutputTypeStatus` "── Context compacted ──" |
| `system` | `local_command` | `OutputTypeStatus` with command content |
| `progress` | `mcp_progress` (completed/failed) | `OutputTypeStatus` with server/tool name |
| `progress` | `waiting_for_task` | `OutputTypeStatus` with description |
| `progress` | `bash_progress` | Skipped (covered by tool cycle) |
| `progress` | `agent_progress` | Skipped (covered by parent tool) |
| `pr-link` | — | `OutputTypeStatus` with PR URL |
| `file-history-snapshot` | — | Skipped (undo/redo metadata) |
| `queue-operation` | — | Skipped (internal bookkeeping) |

### CLI: `logview`

The `bramble/cmd/logview` CLI renders any session format using the OutputModel
widget. It auto-detects raw JSONL via `replay.DetectFormat`:

```
bazel run //bramble/cmd/logview -- ~/.claude/projects/<hash>/<session>.jsonl
```

Flags: `--debug` (show raw line types), `--width`, `--height`, `--plain`,
`--full` (disable compaction).

### Data Mining: `/jsonl-mine` Skill

The `jsonl-mine` skill (`~/.claude/skills/jsonl-mine/`) scans real session
files and reports coverage gaps between the JSONL vocabulary and the renderer.
Use it whenever new message types appear:

```
/jsonl-mine                   # full coverage analysis
/jsonl-mine --fixtures        # generate test fixtures for uncovered types
/jsonl-mine --type progress   # focus on progress subtypes
```

## Future Work

1. **Wire View directly to SessionModel** — replace
   `m.sessionManager.GetSessionOutput(id)` with `model.Output()` in
   `bramble/app/view.go`. Subscribe to model events via the observer channel
   adapter.

2. **Eliminate legacy `outputs` map** — once all paths go through SessionModel,
   remove the parallel `outputs` map from Manager.

3. **Direct protocol parsing** — have `claude.Session` expose raw NDJSON lines
   (before they become `claude.Event`) and feed them through
   `FromLiveNDJSON → MessageParser.HandleMessage`. This eliminates the
   `claude.Event` intermediate entirely for bramble's purposes.

4. **Remove `event_handler.go`** — once the event adapter path is the sole
   consumer, delete `bramble/session/event_handler.go`.
