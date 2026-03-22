# Known Gap Patterns

Gaps found and fixed in past eval iterations. Check for **regressions** every round. When you find and fix a new gap, add it to this table.

| Gap | Root Cause | Fix Location | Fixed In |
|-----|-----------|--------------|----------|
| ToolSearch in delegator | `--allowed-tools` doesn't restrict, only auto-approves | `delegator_runner.go`: use `WithTools("")` | Round 1 |
| Fragmented text (concatenated words) | Per-event rendering of streaming chunks | `delegator.go`: turn-based rendering | Round 1 |
| stderr noise ("Starting claude with flags") | Unconditional debug print | `planner.go`: gate on `Verbose` flag | Round 1 |
| stderr noise ("WARN skipping unknown protocol") | Wrong log level | `protocol/parse.go`: `slog.Debug` not `slog.Warn` | Round 1 |
| Planner cost $0.0000 | Reading from wrong progress field | `delegator.go`: aggregate from `OutputTypeTurnEnd` lines | Round 2 |
| Child model wrong | Not propagating from delegator config | `manager.go`: thread model to all child sessions | Round 2 |
| Premature "You>" prompt | `hasActiveChildren()` only checked `StatusRunning` | `delegator.go`: check all non-terminal states | Round 2 |
| Delegator tool count changed 3→5 | Added `send_followup` SDK tool + `Read` built-in for codetalk support | `delegator_tools.go`, `delegator_runner.go` | Codetalk Round 1 |
| send_followup param naming (message→prompt) | LLM confused `start_session.prompt` with `send_followup.message`, sent empty messages | `delegator_tools.go`: rename to `prompt` + validate | Multi-Turn v2 |
| Context window shows 0% (InputTokens only 4) | Protocol `input_tokens` excludes cached tokens; need `input + cache_creation + cache_read` | `turn.go`: `TotalInputTokens()`, `types.go`: `LastTurnInputTotal`, `manager.go`: propagate | Multi-Turn v3 |
| Verbose delegator responses (doc-style dumps) | No response style guidance in system prompt | `delegator_runner.go`: added Response style section | Multi-Turn v2 |
| Non-Claude child cost/tokens show $0.00 | `agentUsageToTurnUsage` doesn't capture Gemini/Codex billing | `manager.go`: providerRunner cost pipeline | Multi-Provider v1 (unfixed) |
| Codex unusable for multi-turn codetalk | Ephemeral provider (no LongRunningProvider), each turn loses context | Architecture limitation | Multi-Provider v1 (won't fix) |

## Adding New Entries

When you fix a new gap during an eval run:
1. Add a row with the gap description, root cause, fix location, and which round it was fixed in
2. In subsequent rounds, verify the fix holds (no regression)
3. If a previously-fixed gap regresses, update the "Fixed In" column and note the regression
