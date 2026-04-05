# Session Analysis & LLM Summarization Learnings

## Haiku vs Gemini Flash for Summarization
- **Haiku is faster**: 5.7s avg vs 9.0s avg (Gemini has ACP subprocess overhead)
- **Quality is comparable**: both produce accurate ~85-word summaries preserving metrics, file names, root causes
- **Gemini has high variance**: 3.6s–16.6s, while Haiku is consistent 4.9s–8.6s
- ACP protocol overhead (subprocess spawn, JSON-RPC init) penalizes Gemini for short queries

## Haiku Optimization: Custom System Prompt + No Tools
- **No benefit**: custom system prompt + `--allowed-tools` empty was actually slower (7.9s vs 5.7s)
- Default Claude Code system prompt doesn't hurt summarization quality
- `WithDisablePlugins()` is the only needed optimization
- Extra CLI args (`--system-prompt`, `--allowed-tools`) add processing overhead

## Haiku Summary Quality Issues
- Haiku sometimes prepends `**Session Summary:**` or `## Summary` headers
- Fix: `cleanSummary()` strips these prefixes; prompt explicitly says "no headers"
- Both fixes together eliminate ~95% of prefix pollution

## JSONL Envelope Metadata for Message Classification
- `isMeta: true` → system meta-messages (local command output, etc.)
- `agentName` present → teammate/subagent messages
- `sourceToolUseID` present → tool result echoes
- **Task notifications lack metadata** — they arrive as plain `type: user` messages
  with XML content, requiring content-based fallback detection
- Best approach: hybrid (envelope metadata first, content fallback for task-notification)

## Gemini CLI ACP Protocol
- v0.32 changed `modes` from `[]SessionModeState` to `{availableModes: [], currentModeId: ""}`
- `protocolVersion` must be int `1`, not string `"2025-03-26"`
