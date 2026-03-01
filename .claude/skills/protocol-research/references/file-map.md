# Protocol Research File Map

Quick reference for finding protocol-related code across the repo.

## Wire Protocol Types

| File | Provider | Contents |
|------|----------|----------|
| `agent-cli-wrapper/protocol/types.go` | Claude | SystemMessage, AssistantMessage, UserMessage, ResultMessage, StreamEvent, ControlRequest/Response |
| `agent-cli-wrapper/protocol/mcp_types.go` | Claude | JSONRPCRequest/Response, MCPToolDefinition, MCPInitializeResult |
| `agent-cli-wrapper/protocol/parse.go` | Claude | ParseMessage() and ParseStreamEvent() dispatchers |
| `agent-cli-wrapper/codex/jsonrpc.go` | Codex | JSON-RPC methods, request/notification types |
| `agent-cli-wrapper/codex/types.go` | Codex | ThreadInfo, TurnInfo, TokenUsage, event structs |
| `agent-cli-wrapper/acp/protocol.go` | Gemini | ACP request/response types, SessionUpdate discriminated union |
| `agent-cli-wrapper/acp/types.go` | Gemini | Event structs, content blocks |

## SDK Wrappers (Process Management)

| File | Provider | Contents |
|------|----------|----------|
| `agent-cli-wrapper/claude/process.go` | Claude | CLI arg building, process lifecycle |
| `agent-cli-wrapper/claude/sdk.go` | Claude | Session management, control flow, event emission |
| `agent-cli-wrapper/claude/mcp.go` | Claude | MCP server configuration and handshake |
| `agent-cli-wrapper/codex/client.go` | Codex | JSON-RPC client, thread management |
| `agent-cli-wrapper/acp/client.go` | Gemini | ACP client, session management |

## Event Bridging

| File | Contents |
|------|----------|
| `agent-cli-wrapper/agentstream/event.go` | Event kind enum, interface definitions (Text, ToolStart, ToolEnd, TurnComplete, Error, Scoped) |
| `multiagent/agent/bridge.go` | Generic `bridgeEvents[E any]()` — translates SDK events to AgentEvent via type assertions |
| `multiagent/agent/provider.go` | AgentEvent types (TextAgentEvent, ToolStartAgentEvent, etc.) |

## Recording & Traces

| File | Contents |
|------|----------|
| `agent-cli-wrapper/claude/recorder.go` | Session recording (messages.jsonl with timestamps and direction) |
| `agent-cli-wrapper/protocol/trace_test.go` | TraceEntry type, trace file parsing |
| `agent-cli-wrapper/protocol/testdata/traces/` | Real CLI trace files for testing |

## Provider Implementations

| File | Contents |
|------|----------|
| `multiagent/agent/claude_provider.go` | Claude provider — both ephemeral and long-running |
| `multiagent/agent/codex_provider.go` | Codex provider — ephemeral only (thread-per-execution) |
| `multiagent/agent/gemini_provider.go` | Gemini provider — both ephemeral and long-running |
| `multiagent/agent/provider_check.go` | Binary availability probing |
| `multiagent/agent/model_registry.go` | Model-to-provider mapping |

## Conformance & Integration Tests

| File | Contents |
|------|----------|
| `multiagent/agent/integration/provider_conformance_test.go` | Cross-provider test suite |
| `bramble/session/integration/` | Session lifecycle integration tests |
