# Jiradozer: Tool-Error Retry v2 — Tighten the Gate

## Status

Follow-up to `jiradozer-tool-error-retry.md` (PR #152, commit 4d57278).
The v1 design shipped the retry loop in `ClaudeProvider.Execute`, gated on
`FinalTurnToolError`. In production it fires on turns that should not be
retried, actively destroying legitimate parked work. This doc spells out the
gate changes required to make the loop safe, plus the test coverage the v1
doc waved off as "integration-only, out of scope."

v1 stays the authoritative doc for the *shape* of the loop (where it lives,
how `session.Ask` is reused, retry-prompt wording, runaway guardrails,
config surface, observability). v2 is narrowly about *when* the loop is
allowed to fire.

## Problem

The v1 detector, `FinalTurnToolError` in
`agent-cli-wrapper/claude/turn.go:82`, flags any final tool_result with
`IsError==true` **or** the `<tool_use_error>` substring. The provider loop
in `multiagent/agent/claude_provider.go:182-210` then re-`Ask`s the session
with the literal prompt `"retry"` up to `MaxToolErrorRetries` times.

Two real jiradozer runs show the detector firing on turns where no retry
is warranted and where the retry actively destroys work. Both logs are
kept under `~/.jiradozer/logs/`; the relevant ContentBlock sequences should
be extracted into testdata fixtures (see Test Plan).

### Evidence 1: permanent-error Skill + live bg-Bash parking

`~/.jiradozer/logs/jiradozer-20260415-202027-2762325.log`

- Turn 1 launched two background Bash tools (`sleep 120`, a `gh api`
  poll loop) and a `Skill(sy:pr-polish)` call that the CLI rejected
  permanently with
  `<tool_use_error>Skill sy:pr-polish cannot be used with Skill tool due
  to disable-model-invocation</tool_use_error>`. The agent's final text
  was *"Both background tasks are running. I'll wait for the
  notifications when they complete."*
- `FinalTurnToolError` flagged the Skill error. Retry fired with prompt
  `"retry"`. Turn 2 relaunched a background poll and again said *"I'll
  wait for the notification."*
- Execute then returned, `defer session.Stop()` tore down the ephemeral
  session, and every backgrounded Bash was orphaned.

Two separate bugs compose here:

1. A permanent-class error ("disable-model-invocation") was retried
   despite being unrecoverable.
2. A turn with *live background work* was interrupted even though the
   agent had explicitly parked on that work.

### Wire-level evidence from session JSONL

Both bugs were cross-checked against the Claude CLI session JSONL files
(`~/.claude/projects/<cwd-slug>/<session-id>.jsonl`) corresponding to
each run. The wire shape is conclusive:

| Case | `is_error` | Content shape | `<tool_use_error>` wrapper |
|---|---|---|---|
| Successful Bash | `false` | structured object `{stdout, stderr, interrupted, isImage, noOutputExpected}` | no |
| `gh pr checks` exit 8 (informational) | `true` | string `"Error: Exit code 8\n<stdout>"` | **no** |
| `git diff` exit 128 (real but self-inspectable) | `true` | string `"Error: Exit code 128\n<stderr>"` | **no** |
| `sleep 120` CLI-blocked | `true` | string `"<tool_use_error>Blocked: sleep 120...</tool_use_error>"` | **yes** |
| `Skill(sy:pr-polish)` disabled | `true` | string `"<tool_use_error>Skill ... cannot be used ... due to disable-model-invocation</tool_use_error>"` | **yes** |
| Parallel-cancelled sibling | `true` | string `"<tool_use_error>Cancelled: parallel tool call ... errored</tool_use_error>"` | **yes** |

**The `<tool_use_error>` marker is a stable sentinel the CLI uses
exclusively for tool invocations it refused, blocked, or cancelled.**
Nonzero-exit Bash — whether informational (`gh pr checks`) or a real
command failure (`git diff` against a missing ref) — is indistinguishable
on the wire from a polling check: raw `"Error: Exit code N\n<output>"`
with `is_error: true` and no wrapper.

This is actually the right boundary for the retry gate. The agent
itself runs nonzero-exit commands to *inspect* their output — whether
a failure is "real" or "informational" is a decision the agent makes
in its next turn, not something the wrapper can determine from the
wire. If the agent's session had not been torn down by the retry
orphan, it would have handled the git-128 case in-turn and either
fixed the command or moved on. The retry loop's job is narrower:
recover from the case where the CLI itself aborted a tool the agent
was counting on, after which the agent stops with nothing to act on.
That's exactly what the `<tool_use_error>` marker signals.

**Additional observations from the JSONL analysis:**

- `stop_reason` did not differentiate parked-on-bg from
  tool-error-unresolved cases — both show `"tool_use"` as the last
  assistant stop before the terminal ResultMessage. So `stop_reason`
  is not a usable secondary signal.
- No `task_*` frame types appeared in the files for backgrounded
  Bash (run_in_background:true). The bg state is conveyed purely
  through the tool_use block's `run_in_background` input flag and
  the presence of a `backgroundTaskId` field in the tool_result.
  Confirms that `turnState.shouldSuppressForBgTasks()` (which keys
  off `run_in_background` input) is the right detection point;
  there's no separate frame type to watch.
- No frame type carries a permanent-vs-transient error category.
  G3 (the deny-list) remains substring-based — the CLI does not
  emit a structured error kind field.
- The `toolUseResult` field on user frames carries the same content
  twice (once inside `message.content[].content`, once in
  `toolUseResult` with an `Error: ` prefix). The wrapper already reads
  from the first; no change needed.

### Evidence 2: nonzero-exit Bash treated as tool error

`~/.jiradozer/logs/jiradozer-20260415-203130-2938633.log`

- Turn 1 ended with a `Write` tool_use_error ("File has not been read
  yet") after legitimate commits had already landed; the agent was
  waiting on CI. Retry attempt 1 fired.
- Turn 2 ran `gh pr checks 2286`, which exits with code **8** while
  checks are pending. The CLI surfaced this as a `tool_result` with
  `is_error: true` even though the output was informational, not a
  `<tool_use_error>` block. The agent launched a bg poll loop and said
  *"Polling CI in the background. I'll evaluate exit criteria once all
  checks complete."*
- Retry attempt 2 fired on the `gh pr checks` nonzero exit. Turn 3 ran
  another `gh pr checks`, then used `ScheduleWakeup(270s, prompt="retry")`
  as a workaround to defend against the retry loop itself. The
  no-progress abort finally caught it on attempt 3.
- Net: 3 wasted turns and the agent's intentional park-and-wait pattern
  was actively fought.

### Root causes

Three distinct bugs in the gate.

**G1 — `IsError` is not a tool_use_error.** The Claude CLI sets
`tool_result.is_error=true` on any nonzero-exit Bash, regardless of
whether the agent ran it specifically to inspect the exit code.
`gh pr checks` exits 8, `grep -q` exits 1, `test -f` exits 1 — all
informational. Only the `<tool_use_error>` marker (already a constant at
`turn.go:14`) means the CLI itself is reporting a tool invocation
failure. The v1 detector treats the two as equivalent.

Confirmed empirically against the four session JSONL files — see the
"Wire-level evidence" table below. The `<tool_use_error>` wrapper is
used *exclusively* by the CLI for tool invocations it refused, blocked,
or cancelled (disable-model-invocation, sleep-block, parallel-cancel).
Nonzero-exit Bash — whether a real command failure (`git diff` against
a missing ref, exit 128) or an informational poll (`gh pr checks` exit
8) — carries raw `"Error: Exit code N\n<output>"` without the wrapper.
The wrapper cannot tell these apart from the wire, and neither can it:
whether a nonzero exit is "real" or "informational" is a decision only
the agent itself can make in its next turn.

**G2 — Retry fires while the agent is parked on background work.** When
a turn ends with `shouldSuppressForBgTasks()==true` OR with registered
live tasks, the agent has explicitly deferred to background progress.
Re-`Ask`ing "retry" interrupts the park; the follow-up turn re-launches
bg work from scratch, then `Execute` returns and the new bg work is
orphaned by `defer session.Stop()`. The gate currently has no visibility
into turnState at all.

**G3 — Permanent-class errors are indistinguishable from transient
ones.** Tools rejected via `disable-model-invocation`, permissions
denials, or "tool X cannot be used with tool Y" messages will never
succeed on retry. These are lower priority than G1/G2 because G2 alone
covers most occurrences in practice (permanent errors usually coexist
with parked bg work), but worth a deny-list belt.

## Design

### G1 — Distinguish real tool_use_error from nonzero-exit Bash

`FinalTurnToolError` should require **both** signals:

1. `block.IsError == true`, AND
2. `stringifyToolResult(block.ToolResult)` contains
   `toolUseErrorMarker` (`<tool_use_error>`).

Currently it ORs the two. The v1 doc called the substring check
"belt-and-braces"; in the wire data we actually see, it's load-bearing
— the substring is what distinguishes a *CLI-reported tool invocation
failure* from a *tool that ran to completion with a nonzero exit*.

Rationale for the AND (not OR, not substring-only):

- `IsError=true` without the marker covers every nonzero-exit Bash the
  agent ran on purpose. These must not retry.
- The marker without `IsError=true` would be ambiguous content — e.g. a
  `grep` that literally finds the string `<tool_use_error>` in a log
  file. Requiring `IsError=true` rules that out.
- Real CLI tool_use_errors set both. Confirmed by the
  `SubstringOnly` case in `turn_retry_test.go:26-47` where the test
  stubs `IsError:false` but content has the marker — that case should
  now be **not** detected. The test was written to defend against
  "upstream format drift" that does not exist in the wire data; remove
  it and invert the assertion.

Concrete spec (no code yet):

```
FinalTurnToolError(blocks) walks blocks once.
For each block where Type == ContentBlockTypeToolResult:
  content := stringifyToolResult(block.ToolResult)
  if block.IsError && strings.Contains(content, toolUseErrorMarker):
    return (toolName, excerpt, true)
return ("", "", false)
```

Parallel-cancellation semantics preserved: on a parallel batch where a
real error and a cancelled sibling both set `IsError=true`, the
cancelled sibling's content carries the marker too ("Cancelled: parallel
tool call ... errored"), but the real error wins because the function
walks in content-block order — same as today. The existing
`ParallelCancelled` test still passes (both blocks have `IsError=true`
and both *would* match under the new rule, but the first-in-order rule
picks the real error).

### G2 — Skip retry when the turn has live bg work

`turnState.shouldSuppressForBgTasks()` already encodes the "this turn
is a deliberate park" signal. It's battle-tested via bg/Monitor tests.
Reuse it — don't re-derive.

At `finalizeTurn` time in `session.go:1149`, the code computes
`shouldSuppress := !wasSuppressed && turn.shouldSuppressForBgTasks()`
and, in the `msg.IsError` branch (line 1161), **falls through** to
finalize anyway. That fall-through is exactly when the retry-loop bug
bites: the turn finalizes with bg work still live, and the provider
has no way to know.

The fix is to carry that signal into the `TurnResult` so the provider
can inspect it.

**Chosen approach: add `HasLiveBackgroundWork bool` to `TurnResult`.**

Set it in `finalizeTurn` immediately before `CompleteTurn`, computed as
`turn != nil && turn.shouldSuppressForBgTasks()`. The provider gates
retry on `!result.HasLiveBackgroundWork`.

Also-ran approaches and why they lose:

- *Pass `turnState` into `FinalTurnToolError`* — couples a pure content
  helper to turn internals. `turnState` is private to the claude
  package; exposing it would leak more than we need.
- *New `turnState.ShouldRetryOnToolError()`* — conflates the "is there
  an error" question with the "should we retry" question. The first is
  a property of the content blocks; the second is policy that belongs
  one layer up. Mixing them makes the helper harder to test.
- *Session-level snapshot accessor
  (`session.CurrentTurnHasLiveBackgroundWork()`)* — races. The provider
  calls it after `Ask` returns; by then a new turn might be starting.
  A field on the immutable `TurnResult` is race-free.
- *`turnState` field in `TurnResult`* — too broad; exposes internal
  machinery the provider doesn't need and makes the struct mutable.

The field is a minimal, immutable, point-in-time snapshot.

### G3 — Permanent-class error deny list (optional, lower priority)

Add a package-private list of error substrings that should never
trigger retry:

```
permanentErrorMarkers = []string{
    "disable-model-invocation",
    "cannot be used with",  // covers "Skill X cannot be used with Skill tool"
}
```

`FinalTurnToolError` (or a new sibling `FinalTurnRetryableToolError`)
returns `ok=false` when the excerpt matches any marker. Keep the list
*tight* — each entry must correspond to a real CLI error class that
is definitionally unrecoverable within the same session.

G3 is optional. G2 alone would have caught both evidence logs, because
in both cases live bg work was present when the retry fired. Ship G1+G2
first; add G3 only if a repro surfaces a permanent error *without*
concurrent bg work.

### Gate combination

Final retry decision in `ClaudeProvider.Execute`:

```
fire retry iff:
  result.HasLiveBackgroundWork == false AND
  FinalTurnToolError(blocks) returns ok==true AND
  (G3 optional) excerpt does not match permanentErrorMarkers AND
  // v1 guardrails unchanged:
  attempts < cfg.MaxToolErrorRetries AND
  time.Since(start) < budget AND
  excerpt != prevExcerpt AND
  ctx.Err() == nil
```

Order matters: check `HasLiveBackgroundWork` *first*, before the content
walk. It's the cheapest check and it's the most consequential — it
blocks the destructive case where the retry orphans bg work.

When the gate blocks for bg reasons, emit a distinct
`RetryStopReason = "bg_work_live"` via `OnRetryAbort` so triage can
tell "we saw a tool error but chose not to retry" apart from "no tool
error was seen at all." Without this signal the non-retry case is
invisible in logs.

## File/line summary

| File | Change |
|---|---|
| `agent-cli-wrapper/claude/turn.go:82-104` | `FinalTurnToolError` tightens to require `IsError && marker` |
| `agent-cli-wrapper/claude/turn.go` (TurnResult struct ~L124) | New field `HasLiveBackgroundWork bool` |
| `agent-cli-wrapper/claude/session.go:1116-1120` | Set `result.HasLiveBackgroundWork = turn.shouldSuppressForBgTasks()` before `CompleteTurn` |
| `multiagent/agent/claude_provider.go:182-210` | Gate retry on `!result.HasLiveBackgroundWork` first; new abort reason `bg_work_live` |
| `multiagent/agent/claude_provider.go` const block ~L22 | `RetryStopBgWorkLive = "bg_work_live"` |
| `agent-cli-wrapper/claude/turn_retry_test.go` | Update `SubstringOnly` (now negative); add new cases (see Test Plan) |
| `agent-cli-wrapper/claude/testdata/` (new) | Wire-shape fixtures extracted from the two evidence logs |

## Test plan

The v1 doc punted provider-layer tests to "integration only." That
position has allowed five successive PR polish rounds to ship with zero
provider retry coverage. v2 specifies unit tests at both layers.

### Unit — detection (turn_retry_test.go)

Rename existing cases where the assertion inverts:

- `TestFinalTurnToolError_IsError` — `IsError:true` + no marker →
  **ok=false** (was: ok=true). This is the core G1 correction. Cover
  both the `gh pr checks` (nonzero exit with informational output) and
  `Write` ("File has not been read yet") shapes explicitly.
- `TestFinalTurnToolError_SubstringOnly` — `IsError:false` + marker →
  **ok=false** (was: ok=true). Removes the "format drift" belt.
- `TestFinalTurnToolError_IsErrorPlusMarker` — *new*. Both set →
  ok=true. Explicit positive case.
- `TestFinalTurnToolError_ParallelCancelled` — unchanged behavior; real
  error wins over cancelled sibling. Both blocks now have `IsError:true`
  and marker in the cancelled sibling. Assert excerpt contains the real
  error, not "Cancelled".
- `TestFinalTurnToolError_Clean` / `_NoToolUse` / `_BlockShape` /
  `_ExcerptLength` / `_UnknownTool` — unchanged.
- `TestFinalTurnToolError_NonzeroExitBash_Fixture` — *new*. Loads
  `testdata/nonzero_exit_bash.json` (extracted from evidence log 2)
  and asserts `ok=false`. This is the regression test for G1.
- (G3 only) `TestFinalTurnToolError_PermanentSkillError_Fixture` —
  loads `testdata/permanent_skill_error.json` (evidence log 1) and
  asserts `ok=false` when G3 deny list is enabled.

### Unit — provider retry loop (new: claude_provider_retry_test.go)

The v1 doc said a fake `claude.Session` was "too invasive." It isn't —
`claude.NewSession` already returns a struct, and the provider uses
exactly four entry points (`Start`, `Stop`, `Ask`, `Info`, `Events`).
Pattern: introduce a package-private `sessionFactory` field on
`ClaudeProvider` (default: real `claude.NewSession`). Tests set it to
a factory that returns a fake implementing a minimal `claudeSession`
interface. `Ask` is script-driven: the fake holds a slice of canned
`TurnResult`s and returns them in order. `Events()` returns a closed
channel so the bridge goroutine exits immediately.

Cases:

- `retries_until_clean` — fake returns [error, error, clean]; assert 3
  Ask calls, final result is the clean one, no `UnresolvedToolError`
  on `AgentResult`.
- `respects_count_limit` — `MaxToolErrorRetries=1`, fake always returns
  error; assert exactly 2 Asks, `UnresolvedToolError.Reason=exhausted`.
- `no_progress_aborts` — identical excerpts twice; assert abort with
  `Reason=no_progress` before count budget is hit.
- `ctx_cancelled_between_retries` — cancel ctx after first retry
  returns; assert no extra Ask, `Reason=ctx_cancelled`.
- `disabled_by_default` — `MaxToolErrorRetries=0`, fake returns error;
  single Ask, raw result returned, no marker, no
  `UnresolvedToolError`. Preserves today's behavior for jiradozer
  configs that did not opt in.
- **`skips_when_bg_work_live`** — *G2 core test*. Fake returns one
  `TurnResult` with `HasLiveBackgroundWork:true` AND the content
  blocks of a real tool_use_error. Assert exactly 1 Ask, no retry,
  `UnresolvedToolError.Reason=bg_work_live`, marker appended. This is
  the regression test for evidence log 1.
- **`skips_on_nonzero_exit_bash`** — *G1 core test*. Fake returns a
  `TurnResult` whose blocks have `IsError:true` but no
  `<tool_use_error>` marker. Assert exactly 1 Ask, no retry, no
  `UnresolvedToolError` (because `FinalTurnToolError` returned
  ok=false — nothing to mark unresolved). Regression test for
  evidence log 2.
- `retries_cleanly_on_real_tool_use_error` — fake returns [real
  tool_use_error with `HasLiveBackgroundWork:false`, clean]; assert 2
  Asks, clean final. Ensures G1+G2 tightening did not break the
  original PLA-212 fix path.
- `skips_when_bg_work_live_and_cancelled_sibling` — evidence-shaped:
  one tool_use_error, one cancelled sibling, `HasLiveBackgroundWork:
  true`. Assert no retry (G2 dominates even when G1 would allow).

### Fixture-driven testing

Extract from both evidence logs into
`agent-cli-wrapper/claude/testdata/`:

- `testdata/retry/nonzero_exit_bash.json` — the `gh pr checks` exit-8
  `tool_result` from evidence log 2.
- `testdata/retry/parked_with_skill_error.json` — the Skill
  permanent-error + two bg-Bash tool_use blocks from evidence log 1.
- `testdata/retry/real_tool_use_error.json` — the original PLA-212
  parallel-cancellation fixture (already in tests inline; externalize
  for consistency).

Fixtures are JSON snapshots of `[]ContentBlock`. A shared loader in
`turn_retry_test.go` unmarshals them. Storing the full log is
unnecessary — only the `ContentBlock` slice that `FinalTurnToolError`
sees matters for the detection tests, and the provider tests drive the
fake via these same fixtures.

Fixture extraction can be a one-off script; the log files have
unambiguous JSONL framing and the ContentBlock shape matches wire
format.

### Integration — wrapper level

Keep the existing v1 integration test (fake CLI + real
`ClaudeProvider.Execute`) that covers the PLA-212 recover-on-retry
path. Add:

- `TestExecute_SkipsRetryWhenBgWorkLive_Integration` — fake CLI
  script: turn 1 emits a Bash tool_use_error AND a bg-Bash tool_use
  (uncancelled), then a `ResultMessage` with `is_error:true` that
  falls through to finalize. Assert 1 turn issued, no `OnRetry`
  callback, `UnresolvedToolError.Reason=bg_work_live`.

### Integration — jiradozer validate step

No change from v1's position. If validate is not yet covered in
jiradozer integration, skip. The wrapper-level integration test above
is the load-bearing regression coverage.

## Migration notes

- `MaxToolErrorRetries` default remains `0` (disabled). No existing
  config changes.
- Existing jiradozer configs that opted in (e.g.
  `validate.max_tool_error_retries: 2` in the example config) keep
  working. The fix **tightens** when retry fires — any case the old
  code retried is a subset of what v2 retries plus strictly more cases
  it correctly skips. No config currently in the tree relies on the
  buggy retry behavior.
- `HasLiveBackgroundWork` is a new field on the public `TurnResult`
  struct. Monorepo style permits API changes; all in-tree callers are
  exhaustively enumerated (provider, render, bramble via adapter).
  Gazelle-regenerated BUILDs pick up the new field with no changes.
- `RetryStopBgWorkLive` is a new abort reason string. Any log parser
  that pattern-matches on reasons should be updated. (In tree:
  jiradozer log consumers only; no external consumers.)
- `OnRetryAbort` fires for `bg_work_live` even though no retry was
  attempted. This is a semantic widening — "abort" now covers "gate
  blocked from the start" as well as "we tried and ran out of budget."
  `logEventHandler.OnRetryAbort` (jiradozer/agent.go) should log at
  INFO, not WARN, when `reason == bg_work_live` since it's the
  expected case.

## G4 — Error must be the final tool_result in the turn

Added after a production failure on 2026-04-18 (commit on
`fix/retry-detector-recovered-errors`).

**Symptom.** Jiradozer validate ran a 16-Edit turn. One early Edit returned
`<tool_use_error>File has not been read yet</tool_use_error>`. The agent
recovered: Read the file, re-issued the Edit, completed 20+ more tool calls,
and ended with `stop_reason=end_turn` and a 1426-char summary text.
Jiradozer's retry loop still fired ("Retry 1/3: tool error in Edit") and
re-ran "simplify review from scratch," wasting ~5 min and clobbering the
successful output.

**Evidence.**
- Session JSONL:
  `~/.claude/projects/-home-ubuntu-worktrees-yoloswe-faeture-step-tracking/fc29ffb6-7b4d-40f6-80bc-4be1b0a2df33.jsonl`
  line 132 (errored tool_result), line 156 (clean `end_turn` with text summary).
- Jiradozer log:
  `~/.jiradozer/logs/jiradozer-20260418-042630-3768733.log`
  lines 203–208: `turn complete step=validate success=true` immediately
  followed by `retry on tool error ... tool=Edit`.

**Root cause.** The pre-G4 `FinalTurnToolError` walked `ContentBlocks` in
forward order and returned the *first* qualifying error block. It never
checked whether the error was the *last* tool_result. A turn with a transient
early error and later recovery looked identical to a terminal error.

**Fix.** Walk `ContentBlocks` in reverse. Take the first `tool_result`
encountered (i.e. the last one in forward order). Return `ok=true` only if
that last tool_result has both `IsError==true` and the `toolUseErrorMarker`.
If the agent recovered (last tool_result is a success), `ok=false`.

**Compatibility.** Existing G1/G2/G3 gates in `claude_provider.go` are
unchanged. The PLA-212 fixture (`real_tool_use_error.json`) still passes
because in that fixture the cancelled parallel sibling IS the last
tool_result. The `parked_with_skill_error.json` fixture still returns
`ok=true` at the detector level (Skill error is the last tool_result);
G2 (`HasLiveBackgroundWork`) continues to suppress the retry.

**Test regression.** `TestFinalTurnToolError_Fixture_EditErrorThenRecovered`
in `turn_retry_test.go` + `TestRunRetryLoop_SkipsWhenAgentRecovered` in
`claude_provider_retry_test.go`.

## Open questions for human review

1. **Do we need G3 at all?** If G1+G2 land and no log shows a
   permanent error without concurrent bg work within one week of
   running, drop G3. Ask: should the plan land G3 as a separate PR or
   not at all?

2. **Fixture format.** JSON snapshots of `[]ContentBlock` are simple
   but tied to struct field names. Alternative: store raw CLI wire
   frames and parse them through the real accumulator. The wire-frame
   route is more faithful but requires running more of the session
   machinery in test. Recommend JSON snapshots for unit-level; defer
   wire frames to a dedicated recorder replay test if a future bug
   escapes the JSON path.

3. **Fake session interface extraction.** The v1 doc flagged interface
   extraction as "too invasive." It isn't — the provider uses five
   session methods. Confirm: is there a reason the real `Session`
   type cannot be made to satisfy a local interface
   `claudeSession` in `multiagent/agent/`? (I believe there is none.
   The interface lives provider-side, the real type satisfies it
   structurally, tests inject the fake via a factory function.)

4. **`HasLiveBackgroundWork` naming.** Alternatives:
   `HadLiveBackgroundWork` (past tense — snapshot at finalize),
   `BackgroundWorkParked`, `DeliberateFinalPark`. Pick one. I lean
   `HasLiveBackgroundWork` as a point-in-time read at `finalizeTurn`
   — semantically the same as "the turn that just finished was
   parked on bg work."

5. **Do we also need to gate on `result.Error != nil`?** A turn that
   failed with `msg.IsError=true` from the CLI (not a tool_use_error,
   a session-level error) falls through finalize. Retrying an
   already-errored session turn is suspicious — the error is
   already surfaced up the stack via `result.Error`. Consider:
   if `result.Error != nil`, do not retry, return immediately. This
   is a fourth gate (G4) not in the bug list but worth a small sweep
   of the finalize paths (`session.go:1122-1134`, `1248`, `1290`,
   `1336`, `1390`) before deciding.

6. **Should the provider emit a synthetic event when the gate blocks
   a retry?** Currently `OnRetryAbort(bg_work_live, ...)` covers it,
   but event handlers that don't implement `RetryHandler` will lose
   the signal entirely. Consider logging at provider level even when
   no event handler is installed — at least once per Execute call.
