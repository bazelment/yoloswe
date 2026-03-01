# Provider Parity Gap Matrix

Last updated: 2026-02-26 (initial creation)

## Status: Initial — needs first audit pass

Run `/provider-parity-audit` to populate this matrix with real data.

## Known Gaps (from conformance tests)

| Capability | Claude | Codex | Gemini | Conformance Test | Notes |
|-----------|--------|-------|--------|-----------------|-------|
| Basic execution | supported | supported | supported | BasicPrompt | All return text results |
| Event streaming | supported | missing | supported | EventsStreamDuringExecution | Codex `hasEvents: false` |
| Long-running sessions | supported | missing | supported | LongRunningMultiTurn | Codex uses ephemeral threads |
| Permission callbacks | supported | partial | supported | PermissionCallback | Codex uses approval policies only |
| Token usage (input/output) | supported | supported | missing | BasicPrompt | Gemini returns zeros |
| Cost reporting (CostUSD) | supported | missing | missing | BasicPrompt | Only Claude reports cost |
| Thinking/reasoning events | supported | missing | supported | EventsStreamDuringExecution | Codex has no thinking events |
| Tool start/end events | supported | partial | supported | EventsStreamDuringExecution | Codex maps Bash only |
| Context cancellation | supported | supported | supported | ContextCancellation | All handle gracefully |
| Error on invalid workdir | supported | missing | supported | ErrorOnInvalidWorkDir | Codex doesn't error |
| File tool tracking | supported | missing | supported | FileToolTracking | Codex doesn't emit tool events |
