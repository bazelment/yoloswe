# Claude CLI SDK Protocol

This document describes the stdin/stdout control protocol used by the Go SDK to communicate with the Claude CLI, with emphasis on the SDK MCP (Model Context Protocol) server support.

## Overview

The Go SDK spawns the Claude CLI as a subprocess, communicating via NDJSON (newline-delimited JSON) over stdin/stdout. The protocol supports:
- **User messages**: Sending prompts to Claude
- **Control requests/responses**: Session management, permissions, MCP tool routing
- **Stream events**: Real-time streaming of Claude's responses

## CLI Invocation

### Required Flags

```
claude --output-format stream-json --verbose --model <model> \
       --input-format stream-json --setting-sources "" \
       [--mcp-config <json>] [--system-prompt <prompt>]
```

Key points:
- **Do NOT use `--print`**: That flag puts the CLI in one-shot mode (no streaming, no control protocol).
- **`--setting-sources ""`**: Disables external settings files that can interfere with SDK mode.
- **`--input-format stream-json` should come last**: Matches Python SDK convention.

### Required Environment Variables

```
CLAUDE_CODE_ENTRYPOINT=sdk-go
```

This tells the CLI it's being invoked by the Go SDK.

### MCP Config Format

The `--mcp-config` flag accepts a JSON object. For SDK (in-process) MCP servers:

```json
{
  "mcpServers": {
    "my-tools": {
      "type": "sdk",
      "name": "my-tools"
    }
  }
}
```

**CRITICAL: The `name` field is required.** Without it, the CLI silently hangs — it starts 16+ threads but never produces any stdout output and never progresses past its internal `configureGlobalAgents` phase. There is no error message. This was the root cause of a multi-hour debugging session.

## Session Lifecycle

### 1. Process Start

The SDK spawns the CLI with the flags above and sets up stdin/stdout NDJSON readers/writers.

### 2. SDK Initialize Handshake

Immediately after starting the CLI, the SDK must send an initialize control request:

```
SDK → CLI: {"type": "control_request", "request_id": "<id>", "request": {"subtype": "initialize"}}
```

The CLI does NOT immediately respond. Instead, it first sets up all MCP servers by sending MCP messages (see step 3). Only after all servers are initialized does the CLI send the initialize response.

### 3. MCP Server Setup (interleaved with initialize)

For each SDK MCP server in the config, the CLI sends a sequence of MCP requests wrapped in control_request messages. The SDK must respond to each before the CLI continues:

```
CLI → SDK: control_request {subtype: "mcp_message", server_name: "my-tools", message: {method: "initialize", ...}}
SDK → CLI: control_response {mcp_response: {result: {protocolVersion: "2024-11-05", ...}}}

CLI → SDK: control_request {subtype: "mcp_message", server_name: "my-tools", message: {method: "notifications/initialized"}}
SDK → CLI: control_response {mcp_response: {result: {}}}

CLI → SDK: control_request {subtype: "mcp_message", server_name: "my-tools", message: {method: "tools/list"}}
SDK → CLI: control_response {mcp_response: {result: {tools: [...]}}}
```

### 4. Initialize Response

After all MCP servers are set up, the CLI sends the initialize response:

```
CLI → SDK: {"type": "control_response", "response": {"request_id": "<id>", "subtype": "success", ...}}
```

### 5. System Init Message

The CLI then sends a system init message with session metadata:

```
CLI → SDK: {"type": "system", "subtype": "init", "session_id": "...", "tools": [...], ...}
```

At this point the session is ready for user messages.

### 6. User Messages and Tool Calls

Normal conversation follows:

```
SDK → CLI: {"type": "user", "message": {"role": "user", "content": "..."}}
CLI → SDK: {"type": "assistant", ...} (streaming events)
CLI → SDK: {"type": "result", ...}
```

When Claude invokes an SDK tool:

```
CLI → SDK: control_request {subtype: "mcp_message", server_name: "my-tools", message: {method: "tools/call", params: {name: "tool_name", arguments: {...}}}}
SDK → CLI: control_response {mcp_response: {result: {content: [{type: "text", text: "..."}]}}}
```

## Concurrency Notes

### Deadlock Prevention

The `sendInitialize` method is called from `Start()` after releasing `s.mu`. This is critical because:

1. `sendInitialize` sends a control_request and blocks waiting for the response.
2. During this wait, the CLI sends MCP setup control_requests.
3. The `readLoop` processes these via `handleControlRequest` → `handleMCPMessage`.
4. If `s.mu` were held by `Start()`, the readLoop's `handleSystem` (which acquires `s.mu`) would deadlock.

### Pending Response Tracking

Control request/response pairs are matched by `request_id`. The `pendingControlResponses` map (protected by its own `pendingMu`, separate from `s.mu`) stores channels that `sendControlRequestLocked` waits on. When `handleControlResponse` receives a response from the readLoop, it routes it to the correct channel.

### Async Tool Calls

`tools/call` handlers run in a separate goroutine because tool execution can take arbitrarily long. This prevents blocking the readLoop, which must continue processing other messages.

## Debugging Tips

### CLI Produces No Output

If the CLI starts but produces zero stdout:
1. Check the MCP config JSON for the `"name"` field in SDK server configs.
2. Check `~/.claude/debug/` for session logs. If only ~4 lines, the CLI never reached STARTUP.
3. Compare with a working Python SDK invocation: `ps -ef | grep claude` to see exact args.

### Protocol Tracing

Enable session recording with `RecordMessages: true` in `SessionConfig` to capture all sent/received messages.

### Comparing with Python SDK

The Python SDK lives at:
```
~/.local/lib/python3.12/site-packages/claude_agent_sdk/
```

Key files:
- `_internal/transport/subprocess_cli.py` — CLI argument construction and process management
- `_internal/query.py` — Protocol handling (initialize handshake, MCP routing)
- `client.py` — High-level client API

## MCP JSON-RPC Responses

All MCP responses are wrapped in control_response messages with the following structure:

```json
{
  "type": "control_response",
  "response": {
    "subtype": "success",
    "request_id": "<matching request_id>",
    "response": {
      "mcp_response": {
        "jsonrpc": "2.0",
        "id": <rpc_id>,
        "result": { ... }
      }
    }
  }
}
```

For errors, use JSON-RPC error format:

```json
{
  "mcp_response": {
    "jsonrpc": "2.0",
    "id": <rpc_id>,
    "error": {"code": -32603, "message": "..."}
  }
}
```
