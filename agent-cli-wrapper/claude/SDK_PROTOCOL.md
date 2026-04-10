# Claude Code SDK Protocol Reference

This document describes the wire protocol between our Go SDK wrapper
(`agent-cli-wrapper/claude/`) and the upstream Claude Code CLI. It is the
canonical reference — the Go types in `agent-cli-wrapper/protocol/` must
match this document, and this document must match the upstream Claude
Code CLI's SDK entrypoints.

## Overview

The Go SDK spawns the Claude Code CLI as a subprocess and communicates over
stdin/stdout using NDJSON (newline-delimited JSON). Every line is a single
JSON object with a `type` field that discriminates the message shape.
Messages flow in both directions:

- **CLI → SDK** (stdout): assistant text, tool use, system/result events,
  stream deltas, control requests, keep-alives.
- **SDK → CLI** (stdin): user messages, control requests/responses,
  environment updates, keep-alives.

Control requests implement request/response RPC on top of the NDJSON stream;
each request carries a `request_id` and is matched to a `control_response`
with the same ID. Unknown message or subtype values are preserved as
`UnknownMessage` / `UnknownControlRequest` rather than dropped so the
wrapper stays forward-compatible with newer CLI releases.

## Invocation

The CLI is spawned with stream-json I/O mode and settings sources disabled:

```
claude --output-format stream-json --verbose --model <model> \
       --setting-sources "" \
       [--mcp-config <json>] [--system-prompt <prompt>] \
       --input-format stream-json
```

Key points:

- **Do NOT pass `--print`**: that flag puts the CLI in one-shot mode with no
  streaming and no control protocol.
- **`--setting-sources ""`**: disables external settings files that can
  interfere with SDK mode.
- **`--input-format stream-json` comes last**: matches Python SDK order.
- **`CLAUDE_CODE_ENTRYPOINT=sdk-go`** must be set in the CLI env so the CLI
  knows it is talking to a Go SDK.

### MCP config

For SDK (in-process) MCP servers, `--mcp-config` takes a JSON object:

```json
{
  "mcpServers": {
    "my-tools": { "type": "sdk", "name": "my-tools" }
  }
}
```

The `name` field is required (see [Known Issues](#known-issues-and-gotchas)).

## Handshake

1. The SDK spawns the CLI with the flags above.
2. The SDK immediately sends an `initialize` control request
   (`protocol.InitializeRequest`) carrying hooks, agents, sdkMcpServers,
   and optional system prompt overrides.
3. The CLI does **not** respond immediately. First it walks its MCP server
   list and, for each SDK MCP server, sends nested `mcp_message` control
   requests (`initialize`, `notifications/initialized`, `tools/list`). The
   SDK must answer each with a `control_response` before the CLI proceeds.
4. Once every MCP server is set up, the CLI replies to the original
   `initialize` request with a `control_response` whose body is
   `protocol.InitializeResponse` (commands, agents, models, account, pid).
5. The CLI then emits a `system` message with `subtype: "init"`
   (`protocol.SystemInitPayload`) carrying the resolved session metadata.
6. The session is now ready for user messages.

## CLI → SDK Messages

Every message from the CLI is a single JSON line on stdout with a `type`
field discriminator. The full catalog follows.

### `assistant` — `protocol.AssistantMessage`

A complete assistant message. Carries a nested `message` with a `content`
array of content blocks (text / thinking / tool_use). Fires once an
assistant turn's structured content is finalized.

```json
{"type":"assistant","session_id":"abc","uuid":"m1","parent_tool_use_id":null,
 "message":{"role":"assistant","model":"claude-sonnet-4.5",
   "content":[{"type":"text","text":"Hello"}]}}
```

### `user` — `protocol.UserMessage`

A user message echoed back by the CLI, typically carrying `tool_result`
blocks for tool invocations the model requested.

```json
{"type":"user","session_id":"abc","uuid":"m2","parent_tool_use_id":null,
 "message":{"role":"user",
   "content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
```

### `system` — `protocol.SystemMessage`

Envelope for session-wide events. Further discriminated by `subtype`. Use
`SystemMessage.DecodePayload()` or the typed `As<Subtype>()` helpers in
`protocol/system.go` to get the payload struct.

#### `init` — `protocol.SystemInitPayload`

Emitted once after the handshake completes. Carries the resolved tool,
MCP server, plugin, skill, and agent catalogs plus cwd, model, and
permission mode.

```json
{"type":"system","subtype":"init","session_id":"abc","uuid":"s1",
 "cwd":"/repo","model":"claude-sonnet-4.5","permissionMode":"default",
 "tools":["Read","Bash"],"mcp_servers":[{"name":"my-tools","status":"connected"}],
 "slash_commands":["/compact"],"skills":[],"plugins":[]}
```

Helper: `AsInit()`.

#### `status` — `protocol.SystemStatusPayload`

Transient session status change (e.g. `"compacting"`). `status` is null
when the session returns to idle.

```json
{"type":"system","subtype":"status","session_id":"abc","uuid":"s2","status":"compacting"}
```

#### `compact_boundary` — `protocol.CompactBoundaryPayload`

Emitted when the CLI compacts conversation history, either on demand
(`/compact`) or automatically near the context limit. Carries
`compact_metadata` with trigger, pre-compact token count, and an optional
preserved-segment range.

```json
{"type":"system","subtype":"compact_boundary","session_id":"abc","uuid":"s3",
 "compact_metadata":{"trigger":"auto","pre_tokens":150000,
   "preserved_segment":{"head_uuid":"m1","anchor_uuid":"m20","tail_uuid":"m25"}}}
```

#### `post_turn_summary` — `protocol.PostTurnSummaryPayload`

Internal background summary emitted after each assistant turn. Carries a
title/description, status category, and optional artifact URLs. Used by
bramble-style UIs to describe what just happened.

```json
{"type":"system","subtype":"post_turn_summary","session_id":"abc","uuid":"s4",
 "summarizes_uuid":"m1","title":"Edited render.go","description":"...",
 "status_category":"in_progress","needs_action":"","artifact_urls":[]}
```

#### `api_retry` — `protocol.APIRetryPayload`

Emitted when an API request fails with a retryable error and will be
retried after a delay. `error_status` is null for connection errors with
no HTTP response.

```json
{"type":"system","subtype":"api_retry","session_id":"abc","uuid":"s5",
 "attempt":1,"max_retries":5,"retry_delay_ms":2000,
 "error":"overloaded_error","error_status":529}
```

#### `local_command_output` — `protocol.LocalCommandOutputPayload`

Output from a local slash command such as `/cost` or `/voice`. Displayed
inline as assistant-style text.

```json
{"type":"system","subtype":"local_command_output","session_id":"abc","uuid":"s6",
 "content":"Cost so far: $0.12"}
```

#### `hook_started` — `protocol.HookStartedPayload`

Marks the start of a user hook execution.

```json
{"type":"system","subtype":"hook_started","session_id":"abc","uuid":"s7",
 "hook_id":"h1","hook_name":"pre-tool","hook_event":"PreToolUse"}
```

#### `hook_progress` — `protocol.HookProgressPayload`

Streams partial stdout/stderr from a running hook.

```json
{"type":"system","subtype":"hook_progress","session_id":"abc","uuid":"s8",
 "hook_id":"h1","hook_name":"pre-tool","hook_event":"PreToolUse",
 "stdout":"checking...","stderr":"","output":"checking..."}
```

#### `hook_response` — `protocol.HookResponsePayload`

Marks hook completion with final stdout/stderr, exit code, and outcome
(allow/deny/etc).

```json
{"type":"system","subtype":"hook_response","session_id":"abc","uuid":"s9",
 "hook_id":"h1","hook_name":"pre-tool","hook_event":"PreToolUse",
 "output":"ok","stdout":"ok","stderr":"","exit_code":0,"outcome":"allow"}
```

#### `task_started` — `protocol.TaskStartedPayload`

Marks the start of a background task (subagent, workflow). Carries the
task ID, description, and optional workflow name.

```json
{"type":"system","subtype":"task_started","session_id":"abc","uuid":"s10",
 "task_id":"t1","description":"Run tests","task_type":"agent"}
```

#### `task_progress` — `protocol.TaskProgressPayload`

Streams incremental progress from a running background task. Carries
`usage` (`TaskUsage`) with running token/tool-use counts.

```json
{"type":"system","subtype":"task_progress","session_id":"abc","uuid":"s11",
 "task_id":"t1","description":"Running bazel test","last_tool_name":"Bash",
 "usage":{"total_tokens":4200,"tool_uses":3,"duration_ms":15000}}
```

#### `task_notification` — `protocol.TaskNotificationPayload`

Signals completion, failure, or stop of a background task. Final
authoritative event for the task lifecycle.

```json
{"type":"system","subtype":"task_notification","session_id":"abc","uuid":"s12",
 "task_id":"t1","status":"completed","summary":"All tests passed",
 "output_file":"/tmp/task1.log",
 "usage":{"total_tokens":8100,"tool_uses":7,"duration_ms":45000}}
```

#### `session_state_changed` — `protocol.SessionStateChangedPayload`

Mirrors the CLI's own session state machine. `"idle"` is the authoritative
turn-over signal, firing after the CLI flushes its held-back result and
the background-agent loop exits.

```json
{"type":"system","subtype":"session_state_changed","session_id":"abc","uuid":"s13",
 "state":"idle"}
```

#### `files_persisted` — `protocol.FilesPersistedPayload`

Reports the outcome of a batch file-save operation; lists successes and
failures separately.

```json
{"type":"system","subtype":"files_persisted","session_id":"abc","uuid":"s14",
 "files":[{"filename":"a.go","file_id":"f1"}],"failed":[],
 "processed_at":"2026-04-09T12:00:00Z"}
```

#### `elicitation_complete` — `protocol.ElicitationCompletePayload`

Emitted when an MCP server confirms a URL-mode elicitation has completed.

```json
{"type":"system","subtype":"elicitation_complete","session_id":"abc","uuid":"s15",
 "mcp_server_name":"my-tools","elicitation_id":"e1"}
```

### `result` — `protocol.ResultMessage`

Turn-completion marker. Carries final token/cost usage and a `subtype`
describing how the turn ended. Prefer the sealed `Outcome()` helper over
raw field inspection — it returns `ResultSuccess{Text}` or
`ResultError{Subtype, Errors}` so the error case cannot be ignored.

Subtypes (`protocol.ResultSubtype`):

- `success` — normal completion; `result` holds the final assistant text.
- `error_during_execution` — fatal mid-turn error.
- `error_max_turns` — turn cap exceeded.
- `error_max_budget_usd` — cost budget exceeded.
- `error_max_structured_output_retries` — structured-output retry cap hit.

```json
{"type":"result","subtype":"success","session_id":"abc","uuid":"r1",
 "result":"Done.","is_error":false,"num_turns":3,"duration_ms":12345,
 "duration_api_ms":9000,"total_cost_usd":0.034,
 "usage":{"input_tokens":120,"output_tokens":45,
   "cache_creation_input_tokens":0,"cache_read_input_tokens":0}}
```

### `stream_event` — `protocol.StreamEvent`

Incremental updates during assistant message generation. The inner `event`
field has its own type discriminator — see [Stream Event Deltas](#stream-event-deltas).

```json
{"type":"stream_event","session_id":"abc","uuid":"e1","parent_tool_use_id":null,
 "event":{"type":"content_block_delta","index":0,
   "delta":{"type":"text_delta","text":"Hel"}}}
```

### `control_request` — CLI asking the SDK to do something

The CLI sends control requests to the SDK for permission checks, hook
callbacks, MCP routing, and elicitation. Each has a `subtype` discriminator.
See [Control Requests](#control-requests).

### `control_response` — `protocol.ControlResponse`

Reply to a `control_request` previously sent. Matched to its request by
`request_id`.

```json
{"type":"control_response","response":{"subtype":"success",
  "request_id":"req-1","response":{"commands":[],"agents":[],"models":[]}}}
```

### `control_cancel_request` — `protocol.ControlCancelRequest`

Cancels a pending control request by ID. Flows in either direction.

```json
{"type":"control_cancel_request","request_id":"req-1"}
```

### `keep_alive` — `protocol.KeepAliveMessage`

Heartbeat with no payload. Emitted on an idle timer so the stdio pipe does
not appear dead. The SDK should silently ignore it (not warn as unknown).

```json
{"type":"keep_alive"}
```

### `tool_progress` — `protocol.ToolProgressMessage`

Periodic progress update while a tool is still executing so consumers can
render elapsed time. `parent_tool_use_id` is set for delegated subagent
tool calls; `task_id` is set when the tool runs inside a background task.

```json
{"type":"tool_progress","session_id":"abc","uuid":"tp1",
 "tool_use_id":"t1","tool_name":"Bash","elapsed_time_seconds":3.4}
```

### `tool_use_summary` — `protocol.ToolUseSummaryMessage`

Textual summary covering one or more previously emitted `tool_use` blocks.
`preceding_tool_use_ids` lists the invocations this summary replaces.

```json
{"type":"tool_use_summary","session_id":"abc","uuid":"ts1",
 "preceding_tool_use_ids":["t1","t2"],"summary":"Read 2 files"}
```

### `auth_status` — `protocol.AuthStatusMessage`

Reports progress of an interactive OAuth login flow. `output` accumulates
status lines; `error` is set once the flow fails terminally.

```json
{"type":"auth_status","session_id":"abc","uuid":"a1",
 "isAuthenticating":true,"output":["Opening browser..."],"error":null}
```

### `rate_limit_event` — `protocol.RateLimitEventMessage`

Fires whenever the server-side rate-limit state for the current
subscription changes (entering/leaving warning or rejected). Nested
`rate_limit_info` (`protocol.RateLimitInfo`) carries status, reset
timestamps, utilization, and overage details.

```json
{"type":"rate_limit_event","session_id":"abc","uuid":"rl1",
 "rate_limit_info":{"status":"allowed_warning","rateLimitType":"subscription",
   "utilization":0.85,"resetsAt":1712700000}}
```

### `prompt_suggestion` — `protocol.PromptSuggestionMessage`

Internal feature. Carries a predicted next user prompt, emitted at the end
of a turn when prompt suggestions are enabled.

```json
{"type":"prompt_suggestion","session_id":"abc","uuid":"ps1",
 "suggestion":"Run the tests next"}
```

### `streamlined_text` — `protocol.StreamlinedTextMessage`

Internal. Lightweight text-only assistant message used in streamlined
output mode, with thinking and tool_use blocks stripped.

```json
{"type":"streamlined_text","session_id":"abc","uuid":"st1","text":"Hello."}
```

### `streamlined_tool_use_summary` — `protocol.StreamlinedToolUseSummaryMessage`

Internal. Cumulative human-readable tool-use summary used in streamlined
output mode.

```json
{"type":"streamlined_tool_use_summary","session_id":"abc","uuid":"sts1",
 "tool_summary":"Read 3 files, ran 1 bash command"}
```

### Unknown types

Unrecognized top-level `type` values are returned as `protocol.UnknownMessage`
preserving the raw JSON, so callers (and future versions of this wrapper)
can handle new CLI message types without losing data.

## SDK → CLI Messages

### `user` — `protocol.UserMessageToSend`

A user prompt to deliver to the model. `content` is either a plain string
or an array of content blocks (e.g. image + text).

```json
{"type":"user","message":{"role":"user","content":"Refactor foo.go"}}
```

### `control_request` — SDK asking the CLI to do something

Wrapped with a `request_id`; the CLI replies via `control_response`. See
[Control Requests](#control-requests) for the full subtype catalog.

```json
{"type":"control_request","request_id":"req-2",
 "request":{"subtype":"interrupt"}}
```

### `control_response` — reply to a CLI control_request

Shape matches the CLI→SDK `control_response`. Used to answer `can_use_tool`,
`mcp_message`, `hook_callback`, `elicitation`, etc.

```json
{"type":"control_response",
 "response":{"subtype":"success","request_id":"req-abc",
   "response":{"behavior":"allow","updatedInput":{}}}}
```

### `keep_alive`

Heartbeat the SDK may send to the CLI. Same shape as CLI→SDK.

```json
{"type":"keep_alive"}
```

### `update_environment_variables` — `protocol.UpdateEnvironmentVariablesMessage`

Stdin-only message that updates CLI process environment variables at
runtime (e.g. to refresh credentials between turns).

```json
{"type":"update_environment_variables",
 "variables":{"ANTHROPIC_API_KEY":"sk-..."}}
```

## Stream Event Deltas

`stream_event.event` is an [Anthropic Messages streaming event](https://docs.anthropic.com/en/api/messages-streaming).
The inner `type` values we parse (see `protocol/stream.go`):

- `message_start` — `protocol.MessageStartEvent`
- `content_block_start` — `protocol.ContentBlockStartEvent`
- `content_block_delta` — `protocol.ContentBlockDeltaEvent`
  - `text_delta` — `protocol.TextDelta`
  - `thinking_delta` — `protocol.ThinkingDelta`
  - `input_json_delta` — `protocol.InputJSONDelta` (partial tool input JSON)
- `content_block_stop` — `protocol.ContentBlockStopEvent`
- `message_delta` — `protocol.MessageDeltaEvent` (stop reason, final usage)
- `message_stop` — `protocol.MessageStopEvent`

Chunk boundaries are arbitrary — consumers must accumulate (see
`claude/render/`, `bramble/session/event_handler.go`, or
`bramble/sessionmodel/output_buffer.go` for the three supported layers).

## Content Blocks

Content blocks appear inside `assistant.message.content` and
`user.message.content` arrays (or as a plain string via `FlexibleContent`).
Defined in `protocol/content.go`.

- `text` — `protocol.TextBlock`. Plain text body.
- `thinking` — `protocol.ThinkingBlock`. Extended-thinking trace plus an
  optional verifier signature.
- `tool_use` — `protocol.ToolUseBlock`. Model-requested tool invocation
  with `id`, `name`, and `input` map.
- `tool_result` — `protocol.ToolResultBlock`. Tool invocation result,
  matched by `tool_use_id`. `content` is either a string or nested blocks;
  `is_error` marks failures.

```json
[{"type":"text","text":"I'll read the file."},
 {"type":"tool_use","id":"t1","name":"Read","input":{"path":"a.go"}}]
```

## Control Requests

All control requests have shape:

```json
{"type":"control_request","request_id":"req-1",
 "request":{"subtype":"...", "...fields...":"..."}}
```

The peer responds with:

```json
{"type":"control_response",
 "response":{"subtype":"success","request_id":"req-1","response":{...}}}
```

or `"subtype":"error"` with an `error` string. Subtypes (bidirectional —
some flow SDK→CLI, some CLI→SDK, some both). For each, the Go type lives
in `protocol/control.go`; see there for the exhaustive field list.

### `initialize` — `protocol.InitializeRequest` / `protocol.InitializeResponse`

Direction: SDK→CLI, sent once at startup. Hands over hooks, agents, SDK
MCP servers, and optional system prompt overrides. Response carries
commands, agents, models, available output styles, account info, and CLI
pid. The CLI interleaves MCP setup requests before replying.

### `can_use_tool` — `protocol.CanUseToolRequest`

Direction: CLI→SDK. Permission check before executing a tool. The SDK
replies with `PermissionResultAllow` (requires `updatedInput` as an object)
or `PermissionResultDeny`.

### `interrupt` — `protocol.InterruptRequest`

Direction: SDK→CLI. Interrupts the currently running turn.

### `set_permission_mode` — `protocol.SetPermissionModeRequest`

Direction: SDK→CLI. Switches the active permission mode.
See [Permission Modes](#permission-modes).

### `set_model` — `protocol.SetModelRequest`

Direction: SDK→CLI. Switches the active model mid-session. A null `model`
resets to the default.

### `set_max_thinking_tokens` — `protocol.SetMaxThinkingTokensRequest`

Direction: SDK→CLI. Updates the extended-thinking token cap mid-session.

### `mcp_message` — `protocol.MCPMessageRequest`

Direction: CLI→SDK. Wraps an MCP JSON-RPC request (`initialize`,
`tools/list`, `tools/call`, etc.) to an SDK-hosted MCP server. The SDK
replies with a `mcp_response` object containing the JSON-RPC result or
error. See [MCP JSON-RPC](#mcp-json-rpc-responses).

### `mcp_status` — `protocol.MCPStatusRequest` / `protocol.MCPStatusResponse`

Direction: SDK→CLI. Returns per-server status (connected, error, tools,
capabilities).

### `mcp_set_servers` — `protocol.MCPSetServersRequest` / `protocol.MCPSetServersResponse`

Direction: SDK→CLI. Replaces the dynamically managed MCP server set.
Response lists added, removed, and errored servers.

### `mcp_reconnect` — `protocol.MCPReconnectRequest`

Direction: SDK→CLI. Reconnects a specific server. Field name is camelCase
`serverName` (not `server_name`).

### `mcp_toggle` — `protocol.MCPToggleRequest`

Direction: SDK→CLI. Enables or disables a named MCP server. Field name is
camelCase `serverName`.

### `get_context_usage` — `protocol.GetContextUsageRequest` / `protocol.GetContextUsageResponse`

Direction: SDK→CLI. Returns detailed context-window usage breakdown
(categories, gridRows, memoryFiles, mcpTools, messageBreakdown, apiUsage).
Response is currently a raw map pending a typed schema.

### `reload_plugins` — `protocol.ReloadPluginsRequest` / `protocol.ReloadPluginsResponse`

Direction: SDK→CLI. Reloads plugins from disk. Response lists the new
commands, agents, plugins, MCP servers, and any load-error count.

### `rewind_files` — `protocol.RewindFilesRequest` / `protocol.RewindFilesResponse`

Direction: SDK→CLI. Rewinds file changes made since a specific user
message. `dry_run` reports what would change without applying.

### `cancel_async_message` — `protocol.CancelAsyncMessageRequest` / `protocol.CancelAsyncMessageResponse`

Direction: SDK→CLI. Drops a queued async user message by UUID.

### `seed_read_state` — `protocol.SeedReadStateRequest`

Direction: SDK→CLI. Seeds the CLI's `readFileState` cache for a path with
a known mtime so the CLI will not re-read it.

### `hook_callback` — `protocol.HookCallbackRequest`

Direction: CLI→SDK. Delivers a hook callback for a tool-use or lifecycle
event. The SDK responds with an outcome so the CLI can proceed. Consumers
wire this via `SessionConfig.HookCallbackHandler`; the default handler
returns an empty-success response so the CLI never hangs.

### `get_settings` — `protocol.GetSettingsRequest` / `protocol.GetSettingsResponse`

Direction: SDK→CLI. Returns the effective merged settings plus the layer
sources.

### `apply_flag_settings` — `protocol.ApplyFlagSettingsRequest`

Direction: SDK→CLI. Merges a settings patch into the flag settings layer.

### `stop_task` — `protocol.StopTaskRequest`

Direction: SDK→CLI. Stops a running background task by `task_id`.

### `elicitation` — `protocol.ElicitationRequest` / `protocol.ElicitationResponse`

Direction: CLI→SDK. Asks the SDK consumer to answer an MCP elicitation —
either inline (via `requested_schema`) or by opening a `url`. The SDK
replies with an `action` (`accept`/`decline`/`cancel`) and optional
content. Wired via `SessionConfig.ElicitationHandler`; default is cancel.

### Unknown subtypes

Unknown subtypes return `protocol.UnknownControlRequest` preserving the
raw JSON. The SDK replies with an empty-success payload to keep the CLI
from hanging — this is a deliberate forward-compatibility safety measure.

## Permission Modes

- `default` — standard behavior, prompts via `can_use_tool`.
- `acceptEdits` — auto-accept file edit tools.
- `bypassPermissions` — bypass all checks; requires
  `allowDangerouslySkipPermissions`.
- `plan` — planning only, no tool execution.
- `dontAsk` — deny if not pre-approved.

## MCP JSON-RPC responses

All `mcp_message` responses are wrapped in a `control_response` with this
shape:

```json
{"type":"control_response",
 "response":{"subtype":"success","request_id":"<matching id>",
   "response":{"mcp_response":{"jsonrpc":"2.0","id":1,"result":{...}}}}}
```

For errors, use the JSON-RPC error envelope:

```json
{"mcp_response":{"jsonrpc":"2.0","id":1,
 "error":{"code":-32603,"message":"..."}}}
```

## Known issues and gotchas

### MCP config `name` field is required

Without a `name` on each SDK server config, the CLI silently hangs — it
starts 16+ threads but never produces any stdout output and never
progresses past its internal `configureGlobalAgents` phase. There is no
error message. This was the root cause of a multi-hour debugging session.
Always set `"name"` explicitly.

### `CLAUDECODE` env var stripping

The CLI inspects its parent env for `CLAUDECODE=1` and takes a different
code path if it's set. When spawning the CLI from inside another Claude
Code session (e.g. bramble-in-Claude), this env var must be stripped or
the child CLI misbehaves. The SDK's process launcher handles this.

### `CLI produces no output` debugging checklist

1. Check the MCP config JSON for the `name` field on SDK server configs.
2. Check `~/.claude/debug/` for session logs. If only ~4 lines, the CLI
   never reached STARTUP.
3. Compare with a working Python SDK invocation: `ps -ef | grep claude`
   to see the exact args.

### Deadlock avoidance in the initialize handshake

`sendInitialize` is called from `Start()` **after** releasing `s.mu`. This
matters because:

1. `sendInitialize` sends a control_request and blocks on the response.
2. While it waits, the CLI sends MCP setup control_requests.
3. The `readLoop` processes these via `handleControlRequest` →
   `handleMCPMessage`.
4. If `s.mu` were held by `Start()`, the readLoop's `handleSystem` (which
   also acquires `s.mu`) would deadlock.

### Pending-response tracking

Control request/response pairs are matched by `request_id`. The
`pendingControlResponses` map is protected by its own `pendingMu` lock,
**separate** from `s.mu`, so `handleControlResponse` can route a response
to its waiter without contending with the main session mutex. Mixing the
two locks reintroduces the deadlock above.

### Async tool-call execution

`tools/call` handlers run in a dedicated goroutine because tool execution
can take arbitrarily long. Running them inline on the readLoop would
block all other message processing (including MCP setup and keep-alives)
until the tool finished.

### Unknown subtypes must not hang the CLI

`control_request` subtypes the SDK does not recognize are answered with an
empty-success response (not ignored). If the SDK drops a request on the
floor, the CLI waits forever for its reply. `UnknownControlRequest`
exists so callers can at least see what was received.

### Protocol tracing

Enable `RecordMessages: true` on `SessionConfig` to capture every inbound
and outbound message to a trace file, for after-the-fact debugging.

## Appendix: Go ↔ JSON type mapping

| Go                                | JSON               | Notes |
|-----------------------------------|--------------------|-------|
| `string`, `int`, `bool`, `float64`| primitive          | |
| `*T` (pointer)                    | `T` or `null`      | Used for optional fields that must round-trip null explicitly. |
| `omitempty` tag                   | field omitted      | Used for fields that are absent when zero-valued. |
| `[]T`                             | JSON array         | |
| `map[string]T`                    | JSON object        | |
| `json.RawMessage`                 | verbatim           | Used for payloads we re-decode lazily (control request/response inner, stream event inner, UnknownMessage body). |
| `any` / `interface{}`             | any JSON           | Used for fields whose upstream schema is complex or not yet modeled (e.g. `InitializeResponse.Commands`). Prefer tightening to a concrete type when you need to read the value. |
| Sealed interface (`ResultOutcome`, `ContentBlock`, `ControlRequestData`, `StreamEventData`, `DeltaData`) | tagged union | Returned by helper methods after dispatching on a `type`/`subtype` discriminator. |
