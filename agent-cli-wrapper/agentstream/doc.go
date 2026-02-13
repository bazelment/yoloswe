// Package agentstream defines the common streaming event interface shared by
// all agent SDK packages (claude, acp/gemini, codex).
//
// # Background
//
// Each agent SDK wraps a CLI subprocess and emits typed events on a Go channel
// (e.g., claude.TextEvent, codex.CommandStartEvent, acp.ToolCallStartEvent).
// The multiagent/agent provider layer consumes these events and translates them
// into a provider-agnostic AgentEvent type for upstream consumers.
//
// Without a shared interface, each provider needs its own bridge function that
// type-switches on SDK-specific event types and copies fields into AgentEvent.
// These bridge functions are structurally identical — they differ only in field
// names. Adding a new provider means writing another ~40-line bridge function
// that duplicates the same pattern.
//
// # Design
//
// This package defines a narrow set of interfaces that SDK event types can
// optionally implement. The 6 event kinds (Text, Thinking, ToolStart, ToolEnd,
// TurnComplete, Error) capture the common subset that all providers need.
//
// Key design choices:
//
//   - Additive interfaces: SDK event types implement agentstream interfaces via
//     additional methods. The existing SDK Event interface and channel types are
//     unchanged. Direct SDK consumers (builder.go, planner.go, reviewer.go)
//     continue to type-switch on concrete SDK types with full field access.
//
//   - No wrapper goroutine: The provider bridge is a generic function
//     bridgeEvents[E any](events <-chan E, ...) that reads the SDK channel
//     directly and checks each event with any(ev).(agentstream.Event). Events
//     that don't implement the interface are skipped at near-zero cost.
//
//   - Opt-in implementation: SDK-specific events (e.g., codex.CommandOutputEvent,
//     claude.CLIToolResultEvent) do NOT implement agentstream.Event and are
//     naturally excluded from the generic bridge. They remain accessible to
//     direct SDK channel consumers.
//
//   - Scoped filtering: The optional Scoped interface allows per-scope event
//     filtering (e.g., codex thread ID filtering) without provider-specific
//     bridge code.
//
//   - KindUnknown sentinel: Events that conditionally map to a common kind
//     (e.g., ACP ToolCallUpdateEvent with non-terminal status) return
//     KindUnknown, which the bridge skips.
//
// # Limitations
//
//   - Two Event interfaces per SDK: Each SDK type has both its own sdk.Event
//     (for the SDK channel) and agentstream.Event (for the common interface).
//     This is intentional — the SDK interface is the complete vocabulary, the
//     agentstream interface is the common subset — but developers must be
//     aware of both.
//
//   - Method overhead: Each bridged event type gains 2-5 methods. These are
//     trivial one-liners but add lines to SDK events.go files.
//
//   - No streaming tool output: The current 6 kinds do not include streaming
//     tool output (e.g., codex.CommandOutputEvent). This event is only consumed
//     by one direct SDK consumer (reviewer.go) and does not flow through the
//     provider bridge.
//
//   - MappedEvent coexistence: The codex package retains MappedEvent and
//     ParseMappedNotification for session log replay (codexlogview). These are
//     independent of agentstream and can be relocated to codexlogview in a
//     future cleanup.
package agentstream
