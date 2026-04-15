# Jiradozer: Retry on Tool Error

## Problem

A jiradozer round can silently complete with unresolved tool errors. The
concrete failure is Round 3/3 of PLA-212 validate step (session
`1d01f69c-3993-4064-8e91-ee4a785e49c7`): a parallel Bash batch hit a lint
failure, the sibling `pytest` call was cancelled by Claude's dispatcher with
`is_error=true` / `<tool_use_error>Cancelled: parallel tool call … errored</tool_use_error>`,
and the assistant's turn ended immediately. The wrapper saw a clean
`ResultMessage`, returned success to jiradozer, and the workflow advanced to
ship with unresolved lint/tests and no commit.

## Goal

When a Claude turn ends while its final tool_result set contains one or more
errored blocks, keep the session alive and send a short retry user message
asking the model to fix the failure and continue — bounded by a configurable
retry limit and additional runaway guardrails.

Out of scope (explicit): Round 2's "I'll wait for background task" failure
mode (needs a separate bg-bash sentinel fix), the alembic TC003 lint issue
itself, and any change to `workflow.go`'s cross-round loop.

## Design

### Where the loop lives

`ClaudeProvider.Execute` in `multiagent/agent/claude_provider.go:27` already
creates an ephemeral `claude.Session`, calls `session.Ask(ctx, prompt)` once
at line 93, and returns. The retry loop belongs **here**, not inside the
Claude SDK:

- `claude.Session` already exposes a multi-turn API — `Ask` is a convenience
  wrapper; the underlying `SendMessage` (session.go:261) + `WaitForTurn` pair
  lets us inject follow-up user turns on the same session. We don't need
  `--resume` or a new session.
- Keeping the logic in `ClaudeProvider.Execute` avoids pushing
  jiradozer-specific policy into the SDK turn loop. The SDK stays
  agnostic; the provider enforces the retry budget.
- It also sidesteps the `bgSuppressionState` machinery entirely: by the time
  `Ask` returns a `TurnResult`, `finalizeTurn` has already run and all bg
  suppression has been resolved (normal release, timer fire, or continuation).
  There is no double-trigger risk with the Monitor fix.

### Detection

Inspect `TurnResult.ContentBlocks` (already populated via turn.go:355
`AppendContentBlock`, carried into `TurnResult` in the finalize path). A turn
"needs retry" if **any** `ContentBlock` with
`Type == ContentBlockTypeToolResult` has:

- `block.IsError == true`, **or**
- `block.ToolResult` stringifies to content that contains the literal
  substring `<tool_use_error>` (belt-and-braces — in practice `IsError` is
  always set when this substring is present, but the double check costs
  nothing and protects against upstream format drift).

Helper to add in `agent-cli-wrapper/claude/turn.go` (near
`cancelledToolIDs`, ~line 79):

```go
// FinalTurnToolError returns a short description of the first errored
// tool_result in the turn's content blocks, or "" if none. Used by callers
// that want to auto-retry after a turn ends with unresolved tool errors.
func FinalTurnToolError(blocks []ContentBlock) (toolName, excerpt string, ok bool) { … }
```

It walks `blocks` once, finds the first block where `Type ==
ContentBlockTypeToolResult && (IsError || strings.Contains(stringify(ToolResult), "<tool_use_error>"))`,
then looks backward for the matching `ToolUseID`'s `ToolUse` block to recover
the tool name. `excerpt` is the first ~200 chars of the stringified result.

Exporting a free function (not a method on `*turnState`) keeps the internal
turn manager private while giving the provider a clean dependency.

> **Note on the Round 2 "no tool_use in final message" signal.** The plan
> does not use this as a secondary trigger. Round 2 needs Monitor/bg-bash
> sentinel work, not retry: re-prompting "please continue" when the model is
> already waiting on a bg task is counterproductive (the model will just say
> "I'll keep waiting"). Leave that case to the separate fix. The plan does,
> however, structure the retry code so a second detection strategy could be
> plugged into the same loop later.

### Retry action

In `ClaudeProvider.Execute` (claude_provider.go:93), replace the single
`session.Ask` call with a short loop:

```go
result, err := session.Ask(ctx, fullPrompt)
if err != nil { return nil, err }

for attempt := 1; attempt <= cfg.MaxToolErrorRetries; attempt++ {
    name, excerpt, hasErr := claude.FinalTurnToolError(result.ContentBlocks)
    if !hasErr { break }
    if ctx.Err() != nil { break }

    // Runaway guardrails (see below)
    if !retryBudgetOK(attempt, start, prevExcerpt, excerpt) { break }
    prevExcerpt = excerpt

    retryMsg := buildRetryPrompt(name, excerpt)
    // emit a synthetic status event so loggers/renderer see the retry
    emitRetryStatus(cfg.EventHandler, attempt, cfg.MaxToolErrorRetries, name)

    next, askErr := session.Ask(ctx, retryMsg)
    if askErr != nil { return nil, askErr }
    result = next
}
```

`session.Ask` internally calls `SendMessage` (starts a new turn on the same
live session) then `WaitForTurn`, so the session context, worktree, tool
history, and file state are all preserved. No `--resume` needed.

### Retry prompt wording

Keep it terse and specific. Draft:

```
Your previous turn ended with a tool error that was not addressed:

  tool: {name}
  result: {excerpt}

This is the source of the failure — if a sibling tool in a parallel batch
was cancelled, the real error is in the *other* sibling. Fix the underlying
problem and continue the task. Do not stop until the task is complete or
you have a concrete blocker to report.
```

Rationale for including the excerpt verbatim: the model's history already
contains it, but restating it in the user turn (a) makes the "this is
important, act on it" signal unambiguous, (b) works even if the excerpt was
clipped in the assistant's own view, and (c) the parallel-cancellation
footnote matches the exact PLA-212 failure shape — without it, a naive
model will try to "fix" the cancelled pytest call rather than the ruff
error that actually triggered the cancellation.

### Retry limit and runaway protection

Four guardrails, in order of authority:

1. **Count limit** — `cfg.MaxToolErrorRetries` from `StepConfig` (default
   `0` = feature disabled, matching current behavior). Validate step should
   opt in with `2` in the example config; users can raise it per-step.

2. **Wall-clock budget** — hard cap at `max(10min, stepCfg.MaxTurns * 60s)`
   elapsed since the *original* `Ask` started. Beyond this, stop retrying
   even if the count budget is not exhausted. Prevents a runaway where each
   retry takes 5+ minutes of agent work.

3. **No-progress detector** — if `excerpt` for retry N+1 equals
   `excerpt` for retry N (byte-exact, first 200 chars), abort: the model is
   stuck on the same error and further retries are wasted. Emit a distinct
   log line so this is visible in triage.

4. **Context cancellation** — `ctx.Err()` short-circuits everything.

The jiradozer outer cross-round loop is unchanged. When retries are
exhausted and the round still has a tool error, the round returns
successfully from a jiradozer perspective (we do *not* convert the tool
error into a Go error — that would cause `fail(ctx, …)` to trip the whole
workflow). Instead the round output gets a trailing marker line
(`[unresolved tool error after N retries: {name}: {excerpt}]`) which the
next round's review step and any downstream reader can surface.

> **Alternative considered:** bubble up as an error. Rejected — it would
> short-circuit `runStepRounds` mid-sequence, bypassing the remaining
> rounds and the review step. We want the existing cross-round loop to
> stay in charge of "this step is broken" decisions.

### Observability

1. In the existing `logEventHandler` (jiradozer/agent.go:187), add a new
   method `OnRetry(attempt, max int, tool, excerpt string)` to
   `agent.EventHandler` (check whether the interface already has an
   extension shape like `SessionInitHandler` at agent.go:354 — use the same
   optional-interface pattern so renderer/other handlers don't break).
2. `logEventHandler.OnRetry` logs at INFO with fields `{step, attempt,
   max, tool, excerpt}`.
3. `rendererEventHandler.OnRetry` calls `h.r.Status(fmt.Sprintf("Retry
   %d/%d: tool error in %s", attempt, max, tool))` so it appears above the
   streaming output.
4. `compositeEventHandler` fans out via the same optional-interface check
   as `OnSessionInit`.
5. In the provider loop, emit the retry status *before* the follow-up
   `Ask` so the log/renderer shows the decision and the tool name, even if
   the retry subsequently panics or hangs.

The jiradozer log output will then include lines like:

```
[INFO ] retry on tool error  step=validate attempt=1 max=2 tool=Bash excerpt="<tool_use_error>Cancelled: parallel tool call Bash(uv run ruff check services/python/api-ga…) errored</tool_use_error>"
```

### Config surface

Changes in `jiradozer/config.go`:

- Add `MaxToolErrorRetries int \`yaml:"max_tool_error_retries"\`` to
  `StepConfig` at line 64 (after `MaxTurns`).
- `RoundConfig` at line 78 also gets `MaxToolErrorRetries` for per-round
  override. `ResolveRound` (wherever it lives — see `jiradozer/config.go`
  for the resolver; grep for it) treats `0` as inherit-from-step.
- `defaultConfig()` at line 136 sets `Validate.MaxToolErrorRetries = 0`
  (default disabled). The example config `jiradozer.example.yaml` gets an
  annotated `max_tool_error_retries: 2` on the validate step with a comment
  explaining what it does.
- No top-level default field — step-level only. Keeps the blast radius
  narrow: enabling on `validate` is a single-line change, enabling globally
  requires touching every step.

Changes in `multiagent/agent` (`ExecuteConfig` + options file):

- Add `MaxToolErrorRetries int` to `ExecuteConfig`.
- Add `func WithProviderMaxToolErrorRetries(n int) ExecuteOption`.

Changes in `jiradozer/agent.go` (`runAgent`, line 363):

- After the existing `if cfg.MaxTurns > 0` block (~line 403), add an
  analogous `if cfg.MaxToolErrorRetries > 0 { opts = append(opts,
  agent.WithProviderMaxToolErrorRetries(cfg.MaxToolErrorRetries)) }`.

### File/line-number summary

| File | Change |
|---|---|
| `agent-cli-wrapper/claude/turn.go` ~L79 | New `FinalTurnToolError(blocks)` helper |
| `agent-cli-wrapper/claude/turn.go` | Small `stringifyToolResult(interface{})` helper (private, handles string / `[]map[string]interface{}` shapes produced by `handleUser` at session.go:1005) |
| `multiagent/agent/provider.go` `ExecuteConfig` | New field `MaxToolErrorRetries int` |
| `multiagent/agent/provider.go` options | New `WithProviderMaxToolErrorRetries` |
| `multiagent/agent/events.go` (or wherever `EventHandler` lives) | New optional interface `RetryHandler { OnRetry(attempt, max int, tool, excerpt string) }` |
| `multiagent/agent/claude_provider.go:93` | Replace single `Ask` call with retry loop |
| `jiradozer/config.go:64` | New `StepConfig.MaxToolErrorRetries` field |
| `jiradozer/config.go:78` | New `RoundConfig.MaxToolErrorRetries` field |
| `jiradozer/config.go` `ResolveRound` | Inherit rule for new field |
| `jiradozer/config.go:136-158` `defaultConfig` | Leave all defaults at `0` (opt-in) |
| `jiradozer/jiradozer.example.yaml` validate step | Annotated `max_tool_error_retries: 2` |
| `jiradozer/agent.go:403` `runAgent` | Pass new field into `ExecuteOption` chain |
| `jiradozer/agent.go:187` `logEventHandler` | New `OnRetry` method, INFO log |
| `jiradozer/agent.go:280` `rendererEventHandler` | New `OnRetry` method → `r.Status(…)` |
| `jiradozer/agent.go:312` `compositeEventHandler` | Optional-interface fan-out |

## Testing

### Unit — detection helper

New file `agent-cli-wrapper/claude/turn_retry_test.go`:

- `TestFinalTurnToolError_IsError` — single tool_result with
  `IsError: true`, no `<tool_use_error>` substring → detected, tool name
  recovered.
- `TestFinalTurnToolError_SubstringOnly` — synthetic block with
  `IsError: false` but content containing `<tool_use_error>` → detected
  (defends against upstream format drift).
- `TestFinalTurnToolError_ParallelCancelled` — two tool_use blocks, one
  errored; returns the errored one's name, not the cancelled sibling. This
  is the exact PLA-212 shape.
- `TestFinalTurnToolError_Clean` — all tool_results succeed → `ok=false`.
- `TestFinalTurnToolError_NoToolUse` — empty or text-only turn → `ok=false`.
- `TestFinalTurnToolError_NonStringResult` — content is
  `[]map[string]interface{}{{"type":"text","text":"<tool_use_error>…"}}`
  (the shape that actually comes off the wire in `handleUser`).

### Unit — provider retry loop

New file `multiagent/agent/claude_provider_retry_test.go`. Pattern: use a
fake `claude.Session` — the simplest form is a thin interface extraction
around the two methods the provider needs (`Ask`, `Info`, `Stop`,
`Events`) wired behind a package-private constructor so tests can inject a
stub. If that's too invasive, fall back to the integration test (next
section) and drop this unit level.

Cases:

- `retries_until_clean` — stub returns errored result twice then clean;
  verify 3 total calls and final result is the clean one.
- `respects_count_limit` — MaxToolErrorRetries=1, stub always errors;
  verify exactly 2 calls (initial + 1 retry) and the final errored result
  is returned with the unresolved marker appended.
- `no_progress_aborts` — same excerpt twice in a row; verify exits after
  detecting the duplicate even though count budget remains.
- `ctx_cancelled` — cancel ctx between retries; verify no extra `Ask`.
- `disabled_by_default` — MaxToolErrorRetries=0, errored result → single
  call, returns errored result as-is (no marker, no retries — preserves
  today's behavior).

### Integration — full round

New test in `agent-cli-wrapper/claude/integration/` (check existing tests
for harness conventions — there's a fake CLI fixture pattern used by the
Monitor tests). Structure:

- Start a session against a fake CLI that, on turn 1, emits an assistant
  message with a Bash tool_use, a user message with
  `tool_result { is_error: true, content: "<tool_use_error>Cancelled…" }`,
  and a clean `ResultMessage`. On turn 2 it emits a clean text turn with
  no tool use and a clean `ResultMessage`.
- Drive via `ClaudeProvider.Execute` with
  `WithProviderMaxToolErrorRetries(2)`.
- Assert: 2 turns issued, final `AgentResult.Text` matches turn 2,
  `EventHandler.OnRetry` was called exactly once with attempt=1.

This fixture is also the regression test for PLA-212 specifically.

### Integration — jiradozer validate step

Extend (don't duplicate) an existing `jiradozer/integration` test if one
covers the validate step — same pattern as the existing session-resumption
test described in `memory/jiradozer-testing-approach.md`. If validate is
not yet covered in integration, skip this and rely on the wrapper-level
integration test above.

## Rollout

1. **Disabled by default.** `MaxToolErrorRetries: 0` everywhere. No
   behavior change for existing users unless they opt in.
2. **Example config opts validate in** with `max_tool_error_retries: 2`
   and a comment explaining what it does and what wall-clock cost looks
   like (each retry is a full agent turn — budget accordingly).
3. **Monitor via existing jiradozer logs.** The new INFO line is grepable;
   after a week of running, review how often it fires and whether the
   no-progress abort is hitting. If retries are consistently succeeding,
   consider bumping the example default to 3. If the no-progress abort is
   firing often, consider adding the errored tool's *history* to the
   prompt (not just the latest excerpt).
4. **Kill switch**: setting the step's `max_tool_error_retries` back to
   `0` disables the feature per step without a code change.

## Open questions

1. **`session.Ask` vs `SendMessage` + `WaitForTurn`.** The plan assumes
   `Ask` can be called a second time on the same live session after the
   first call returns. Need to confirm (a) `Ask` doesn't internally call
   `Stop` on the first turn, and (b) the session's `done` channel isn't
   closed by the first `ResultMessage`. If either assumption is wrong, the
   retry loop must drop down to `SendMessage` + `WaitForTurn` directly,
   and `ClaudeProvider.Execute`'s `defer session.Stop()` stays as the only
   shutdown path. Spot-check required before implementation.

2. **`ContentBlock.ToolResult` shape.** `handleUser` in session.go:1005
   stores whatever the CLI sent — usually `[]map[string]interface{}`
   blocks with `{type: "text", text: "…"}`. `stringifyToolResult` must
   handle this shape plus plain strings. Confirm by inspecting a real
   recorded session jsonl (the PLA-212 recording under
   `~/.claude/projects/-home-ubuntu-worktrees-kernel-feature-PLA-212/` has
   the exact fixture we need — copy a trimmed version into testdata).

3. **Multi-round feedback injection.** `runStepRounds` starts a *fresh*
   session per round. The retry loop lives inside one round's session,
   so retries are scoped to that round — good, matches the design. But
   when round N fails retries and emits the unresolved marker, should
   round N+1's prompt include that marker automatically? Currently
   `runStepRounds` concatenates round outputs (`allOutputs`) but does not
   forward them into the next round's prompt. Out of scope for this fix;
   worth a follow-up note.

4. **Cost accounting.** Retry turns bill against the session's cumulative
   cost, which is already enforced by `SessionConfig.MaxBudgetUSD`
   (session.go:1280). No new budget field needed — a runaway retry will
   hit the existing `ErrBudgetExceeded` first. Confirm this in the
   integration test by setting a tiny budget and verifying the retry loop
   exits cleanly on budget error.

5. **The synthetic status event / `OnRetry` callback**. The
   `agent.EventHandler` interface lives in `multiagent/agent`; renderer
   already tolerates optional-interface extensions (see `SessionInitHandler`
   at jiradozer/agent.go:354). Confirm the same pattern applies to the
   renderer and external consumers before adding the method.
