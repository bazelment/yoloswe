# Provider Parity Gap Matrix

Last updated: 2026-02-26 (initial creation)

**SUPERSEDED (2026-09-02):** the table below was written against the deleted
`gemini_provider.go` (ACP-backed Gemini CLI provider, which had both event
streaming and `LongRunningProvider`). That provider is gone; `gemini-*` model
IDs now route to `AgyProvider` (`multiagent/agent/agy_provider.go`), a
print-mode CLI wrapper with materially different — and mostly worse —
capabilities. The old Gemini column is preserved below for history but is
**not a description of the current Agy provider**. Re-run
`/provider-parity-audit` to regenerate this matrix against Agy; until then,
use the "Agy (known, partially audited)" column below rather than the stale "Gemini"
column.

## Status: Initial — needs first audit pass (against Agy)

Run `/provider-parity-audit` to populate this matrix with real data.

## Known Gaps (from conformance tests)

| Capability | Claude | Codex | Gemini (historical, deleted provider) | Agy (known, partially audited) | Conformance Test | Notes |
|-----------|--------|-------|--------|--------|-----------------|-------|
| Basic execution | supported | supported | supported | supported | BasicPrompt | All return text results |
| Event streaming | supported | missing | supported | **partial** | EventsStreamDuringExecution | Codex `hasEvents: false`. Agy streams Text/TurnComplete events but has no tool events. |
| Long-running sessions | supported | missing | supported | **missing** | LongRunningMultiTurn | Codex uses ephemeral threads. Agy implements `Provider` only, no `LongRunningProvider` — but `providerRunner` threads `AgentResult.SessionID` across turns as a resume id, so bramble sessions still keep conversation continuity. |
| Permission callbacks | supported | partial | supported | unaudited | PermissionCallback | Codex uses approval policies only |
| Token usage (input/output) | supported | supported | missing | **supported** | BasicPrompt | Agy's `--output-format json` `usage` field populates `AgentUsage.{InputTokens,OutputTokens,CacheReadTokens}` — a capability GAIN over the old Gemini provider. |
| Cost reporting (CostUSD) | supported | missing | missing | missing | BasicPrompt | Only Claude reports cost |
| Reasoning effort | n/a (own knob) | supported | not supported (rejected) | **supported** | — | `ProviderSupportsEffort(ProviderAgy)==true`, low/medium/high, max clamps to high. This is a capability GAIN over the old Gemini provider, not a gap. |
| Thinking/reasoning events | supported | missing | supported | **missing** | EventsStreamDuringExecution | Codex has no thinking events. Agy has no thinking events either (print-mode has no thinking delta). |
| Tool start/end events | supported | partial | supported | **missing** | EventsStreamDuringExecution | Codex maps Bash only. Agy emits none — use git-diff detection (`detectFileChangesGit`) instead. |
| Context cancellation | supported | supported | supported | supported | ContextCancellation | All handle gracefully |
| Error on invalid workdir | supported | missing | supported | supported | ErrorOnInvalidWorkDir | Codex doesn't error; Agy returns an error. |
| File tool tracking | supported | missing | supported | **missing** | FileToolTracking | Codex doesn't emit tool events. Agy doesn't either (no tool events at all). |
