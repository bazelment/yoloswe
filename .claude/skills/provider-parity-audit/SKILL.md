---
name: provider-parity-audit
description: >
  Systematically audit and improve feature parity across Claude, Codex, and Gemini providers.
disable-model-invocation: true
---

# Provider Parity Audit

Systematically find and fix capability gaps across Claude, Codex, and Gemini providers. The goal is to ensure the provider abstraction layer (`multiagent/agent/`) accurately represents each provider's capabilities and that higher-level consumers (Bramble sessions, swarm orchestration, task routing) work correctly regardless of which backend is active.

## Arguments

```
/provider-parity-audit [--iterations N] [--until "condition"] [--focus <area>]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--iterations N` | `5` | Stop after N rounds |
| `--until "cond"` | — | Stop when condition met (e.g., `"no gaps in matrix"`) |
| `--focus <area>` | all | Focus area: `events`, `sessions`, `permissions`, `usage`, `mcp` |

## Before You Start

Read these files to understand the current state — don't research from scratch:

- `multiagent/agent/provider.go` — Core `Provider` and `LongRunningProvider` interfaces
- `multiagent/agent/integration/provider_conformance_test.go` — Current conformance expectations
- `agent-cli-wrapper/agentstream/event.go` — Unified event interface
- `multiagent/agent/bridge.go` — Event bridging logic
- `memory/gap-matrix.md` (in this skill's directory) — Current parity status

Also review the provider implementations:
- `multiagent/agent/claude_provider.go`
- `multiagent/agent/codex_provider.go`
- `multiagent/agent/gemini_provider.go`

## Phase 0 — Build the Gap Matrix

Do this research yourself before dispatching any work.

### Step 1: Inventory provider capabilities

For each provider, document what the SDK wrapper actually supports by reading the implementation code. Don't guess from interface definitions alone — check what each method actually does.

Capability dimensions to check:

| Dimension | What to Verify |
|-----------|---------------|
| **Basic execution** | `Execute()` returns result with text, thinking, usage |
| **Event streaming** | `Events()` channel emits text, thinking, tool, turn, error events |
| **Long-running sessions** | `Start()` / `SendMessage()` / `Stop()` multi-turn flow |
| **Permission handling** | Permission callbacks work, modes (bypass/plan/default) respected |
| **Token usage** | InputTokens, OutputTokens, CacheReadTokens, CostUSD populated |
| **MCP integration** | MCP server configuration and tool routing |
| **Tool tracking** | ToolStart/ToolEnd events with name, ID, input, result |
| **Thinking/reasoning** | Thinking events emitted during execution |
| **Error handling** | Graceful error propagation, context cancellation |
| **Work directory** | Respects configured work directory |

### Step 2: Run the conformance test suite

```bash
bazel test //multiagent/agent/integration/... --test_timeout=120
```

Note which tests are skipped per provider and why. The conformance tests already encode known parity expectations — gaps marked with `hasEvents: false` or `newLongRunning == nil` are acknowledged limitations.

### Step 3: Test through Bramble's session layer

Start Bramble and run sessions with each provider to check the end-to-end flow:

- Does event streaming work through `providerRunner.bridgeProviderEvents()`?
- Does the output render correctly in the TUI for each provider?
- Does session persistence capture all relevant data?
- Does follow-up (multi-turn) work for providers that support it?

### Step 4: Produce the gap matrix

Create a table in `memory/gap-matrix.md`:

- **Rows**: Each capability from Step 1
- **Columns**: Claude | Codex | Gemini | Conformance Test? | Bramble Integration?
- **Cells**: `supported` / `missing` / `partial` / `n/a` with notes

Sort by impact: session-breaking gaps > data loss gaps > degraded UX > nice-to-have.

## Iteration Loop

Print budget each round:
```
[Round N/max] | Focus: <area or all> | Until: <cond or N/A>
```

### 1. Pick gaps (orchestrator)

Select the highest-impact gaps from the matrix. Prioritize:
1. Gaps that cause session failures or data loss
2. Gaps where the interface claims support but the implementation is broken
3. Gaps where adding support is feasible (the CLI supports it, we just don't handle it)
4. Conformance test coverage gaps

### 2. Categorize the fix

Each gap falls into one of these layers:

| Layer | Location | Example Fix |
|-------|----------|-------------|
| **SDK wrapper** | `agent-cli-wrapper/<provider>/` | Parse new event type, handle new protocol message |
| **Agentstream interface** | `agent-cli-wrapper/agentstream/` | Add new event kind, extend interface |
| **Provider implementation** | `multiagent/agent/<provider>_provider.go` | Wire new SDK event to AgentEvent, implement missing method |
| **Event bridge** | `multiagent/agent/bridge.go` | Handle new agentstream kind in generic bridge |
| **Conformance tests** | `multiagent/agent/integration/` | Add test case, update expectations |
| **Consumer layer** | `bramble/session/`, `multiagent/orchestrator/` | Handle new event type, update UI rendering |

### 3. Implement the fix

For each gap:

1. **Verify protocol behavior first** — Use `/protocol-research` to confirm the CLI actually supports the capability. Don't implement handling for events that the CLI doesn't emit.

2. **Write the conformance test first (TDD)** — Add or update `provider_conformance_test.go` to express the expected behavior. The test should fail before your implementation.

3. **Fix bottom-up** — Start at the SDK wrapper layer and work up:
   - SDK wrapper parses the new data
   - Agentstream interface exposes it (if new event kind needed)
   - Provider translates to AgentEvent
   - Bridge handles the new event kind
   - Consumer code uses it

4. **Update the provider's capability flags** — If the provider now supports something new, update the conformance test expectations (`hasEvents`, `newLongRunning`, etc.).

### 4. Verify

After implementation:

```bash
# Lint first
scripts/lint.sh

# Build everything
bazel build //...

# Run all unit tests
bazel test //... --test_timeout=60

# Run conformance tests specifically
bazel test //multiagent/agent/integration/... --test_timeout=120
```

For integration-level changes, also test through Bramble to verify the end-to-end flow works.

### 5. Update state

- Mark rows in `memory/gap-matrix.md`
- Update notes on any protocol behavior discovered during implementation

### 6. Check exit conditions

Stop if: max iterations reached, `--until` condition met, or no actionable gaps remain.

## Key Architecture Notes

### Provider Interface Hierarchy

```
Provider (basic)
  ├── Execute(ctx, prompt, wtCtx, opts...) → AgentResult
  ├── Events() → <-chan AgentEvent
  └── Close()

LongRunningProvider (extends Provider)
  ├── Start(ctx)
  ├── SendMessage(ctx, msg) → AgentResult
  └── Stop()
```

Not all providers implement `LongRunningProvider`. Currently:
- Claude: both interfaces
- Gemini: both interfaces
- Codex: `Provider` only (thread-per-execution model)

### Event Bridge Pattern

The `bridgeEvents[E any]()` generic function in `multiagent/agent/bridge.go` uses type assertions on `agentstream` interfaces. SDK events that don't implement any agentstream interface are silently skipped — this is intentional, not a bug. Provider-specific events (like `claude.CLIToolResultEvent` or `codex.CommandOutputEvent`) are SDK-internal details that don't need to cross the abstraction boundary.

### Known Parity Gaps (at time of writing)

These are documented in conformance tests:
- Codex: `hasEvents: false` — limited streaming event emission
- Codex: No `LongRunningProvider` — uses ephemeral thread model
- Gemini: Token usage returns zeros (`AgentUsage` fields empty)
- Gemini: Cost reporting returns zero
- Codex: No thinking/chain-of-thought events

## Reference Files

| File | Purpose |
|------|---------|
| `multiagent/agent/provider.go` | Core interfaces and AgentEvent types |
| `multiagent/agent/bridge.go` | Generic event bridge implementation |
| `multiagent/agent/integration/provider_conformance_test.go` | Cross-provider test suite |
| `agent-cli-wrapper/agentstream/event.go` | Shared event kind definitions |
| `bramble/session/manager.go` | How Bramble creates and manages provider sessions |
| `bramble/session/provider_runner.go` | How Bramble bridges provider events to TUI output |
