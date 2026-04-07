# Jiradozer: Fix verbose output and plan content display

## Context
Running `jiradozer --verbose --run-step plan` has two gaps:
1. `--verbose` shows no extra detail — log level is set to Debug but no Debug-level logs exist
2. Plan step output is empty — Claude writes the plan to `.claude/plans/{uuid}.md` (plan mode behavior) but jiradozer only reads `result.Text` (conversational summary)

## Approach: EventHandler-based plan file tracking

Track the plan file write via the existing `EventHandler.OnToolComplete` callback, then read the file after execution. No changes to `multiagent/agent` or `agent-cli-wrapper/claude` packages.

### Changes

**`jiradozer/agent.go`** — already partially done (verbose logging)

1. Add `planFilePath string` field to `logEventHandler`
2. In `OnToolComplete`: detect `Write` tool calls to `.claude/plans/*.md`, store path (same pattern as `yoloswe/planner/planner.go:926-938`)
3. In `runAgent`: after `provider.Execute()`, if `handler.planFilePath != ""`, read plan file from disk and use as output instead of `result.Text`. Fall back to `result.Text` if read fails or no plan file detected.
4. Need to add `"os"` and `"path/filepath"` imports

**`jiradozer/agent_test.go`** — new test

- Test `logEventHandler.OnToolComplete` correctly detects plan file writes
- Negative cases: Write to non-plan paths, non-Write tools

**`jiradozer/cmd/jiradozer/main.go`** — already done (output formatting with `=== plan output ===` header and empty-output warning)

### Verification
1. `scripts/lint.sh` passes
2. `bazel test //jiradozer/... --test_timeout=60` passes
3. Manual: `bazel run //jiradozer/cmd/jiradozer -- --config ~/jiradozer.yaml --run-step plan --verbose --issue INF-211` should show:
   - Debug logs with the rendered prompt
   - Debug logs for each tool call (Write, Read, etc.)
   - Plan file content in the output (structured markdown, not just "I've created a plan...")
