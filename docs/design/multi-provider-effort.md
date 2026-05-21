# Multi-Provider Reasoning Effort

Status: design — ready to implement
Owner: hand-off after this doc lands
Related: PR #172 (`feature/jiradozer-agent-flags`), `multiagent/agent/provider.go:194` (`ExecuteConfig.Effort`)

## Problem

`cfg.Effort` is plumbed through `ExecuteConfig` and consumed only by
`ClaudeProvider` (`multiagent/agent/claude_provider.go:205-211`). The other
providers — Codex, Cursor, and agy — silently ignore it, which means
`jiradozer --thinking-level=high --provider=codex` looks like it works but
applies no effort. Both PR #172 reviewers (codex+cursor agreeing in r2)
flagged this as the main follow-up; the flag help text was hedged with
"Claude provider only" pending this work.

## Per-provider feasibility

| Provider | Verdict | SDK status | Wire point | Constraints |
|---|---|---|---|---|
| **Codex** | Supported — wire it through | `codex.WithEffort(string)` exists at `agent-cli-wrapper/codex/client_options.go:215`; `TurnConfig.Effort` flows to JSON-RPC `effort` field at `agent-cli-wrapper/codex/jsonrpc.go:104` (note: response field is `reasoningEffort`). SDK passes the string through unvalidated. | `codex_provider.go:104` — add `[]codex.TurnOption` arg to `thread.Ask` (signature already accepts `...TurnOption` at `codex/thread.go:172`). | Codex SDK comment at `client_options.go:188` says "for o-series models" — non-o-series silently ignore. SDK accepts any string; validation is ours. |
| **Cursor** | Not supported — reject explicitly | Zero effort/reasoning surface in `agent-cli-wrapper/cursor/`. `session_options.go` has no relevant fields; `--thinking-level` is not a Cursor CLI flag. Cursor *streams* `thinking` deltas (`cursor/protocol.go:154`) but provides no input knob. | None. | Returning success while ignoring `cfg.Effort` is the current bug. |
| **agy / Gemini-family aliases** | Not supported — reject explicitly | `agent-cli-wrapper/agy/` has no reasoning/effort input parameter, and the `agy` CLI exposes no model-selection or effort flag. | None. | `gemini-*` model IDs are compatibility aliases that route to agy's default model. |

## Validation architecture

**Decision:** lift the effort vocabulary out of the `claude` package into a
neutral shim at `multiagent/agent/effort.go`. Each provider then maps the
neutral `EffortLevel` to its own representation, or returns a typed error.

Why not keep `claude.ParseEffort` as the source of truth:

- `jiradozer/cmd/jiradozer/main.go:214` already calls `claude.ParseEffort`
  for CLI-level validation. That's a layering smell — the CLI shouldn't
  reach into a provider SDK to validate a provider-neutral flag.
- Lifting it removes the awkward import and lets non-Claude provider tests
  validate without depending on the `claude` package.
- The valid set (`low/medium/high/max/auto`) happens to match Claude's,
  but that's a coincidence we shouldn't bake in. Future providers can map
  or reject independently.

**Shape** (`multiagent/agent/effort.go`):

```go
package agent

type EffortLevel string

const (
    EffortAuto   EffortLevel = "auto"
    EffortLow    EffortLevel = "low"
    EffortMedium EffortLevel = "medium"
    EffortHigh   EffortLevel = "high"
    EffortMax    EffortLevel = "max"
)

var ErrInvalidEffort = errors.New("invalid effort level")
// ErrEffortUnsupported is returned by providers that have no reasoning-effort
// concept (Cursor, Gemini today) when cfg.Effort is non-empty.
var ErrEffortUnsupported = errors.New("provider does not support reasoning effort")

func ParseEffort(s string) (EffortLevel, error) { /* same as claude.ParseEffort */ }
```

`claude.ParseEffort` stays where it is (it's used by SDK consumers like the
TUI status bar) but the multiagent layer stops importing it. `ClaudeProvider`
maps the neutral `agent.EffortLevel` to `claude.EffortLevel` with a trivial
switch — the strings happen to match, but the conversion is explicit so a
future divergence doesn't silently break.

## Failure mode for unsupported provider+level

**Decision:** hard-fail at `Execute` entry, before any session/subprocess
work. Returns `fmt.Errorf("%w: provider=%s level=%s", ErrEffortUnsupported, name, level)`.

Considered:

- **Silently ignore** (status quo): the bug we're fixing.
- **Warn-and-continue**: log noise without behavioral change; users still
  think effort is applied. Worst of both worlds.
- **Hard-fail at provider construction**: cleanest in theory, but providers
  are constructed once and reused across many `Execute` calls in
  multi-agent flows; `cfg.Effort` is per-call. So per-call is correct.

This catches the "config has `agent.effort: medium` and Codex got swapped
in for the planner step" case at the first call rather than producing
silently-wrong output for hours.

For Codex specifically: pass `cfg.Effort` straight through to
`codex.WithEffort`. The Codex SDK accepts any string; if Codex
itself rejects an unknown level the JSON-RPC error surfaces normally. We
don't pre-validate that "max" or "auto" are accepted by Codex — let the
backend speak. Rationale: avoids a stale per-provider allow-list that drifts
behind upstream changes, and the worst case (Codex rejects "max") is a
clear runtime error, not silent wrong-effort.

If the Codex error proves opaque in practice (e.g. surfaces as a generic
"invalid request"), add a thin per-provider allow-list in a follow-up. Don't
preempt.

## Help text update

`jiradozer/cmd/jiradozer/main.go:92` — drop "(Claude provider only)" once
this lands:

```diff
-"Agent reasoning effort level: low, medium, high, max, auto (overrides config; Claude provider only)"
+"Agent reasoning effort level: low, medium, high, max, auto (overrides config; rejected by providers that don't support it)"
```

## Backwards compat

Existing configs with `agent.effort: medium` and a Claude model: unchanged.

Existing configs with `agent.effort: medium` and a Codex model: now applies
the effort (was ignored). This is the intended fix.

Existing configs with `agent.effort: medium` and a Cursor/Gemini model:
**now fails at `Execute`** (was ignored). This is a behavior change but
the previous behavior was silently wrong. Users who hit this either:

1. genuinely wanted effort and need to switch providers, or
2. had a stale config field and should remove it.

Either way, surfacing it is correct. The error message names the provider
so the fix is obvious.

We do not add a config-load-time check — the provider isn't always known
until per-step config resolves. Failing at `Execute` is one call later but
covers all code paths uniformly.

## Test strategy

Unit tests, mirroring the pattern in `claude_provider_retry_test.go`:

1. **Codex** (`codex_provider_test.go`): table test asserting that for each
   `agent.EffortLevel` the resulting `codex.TurnConfig.Effort` matches.
   Use a fake `codex.Client` that captures the `TurnOption`s applied
   (build `defaultCodexTurnConfig()` then apply, like `client_options_test.go:250`).

2. **Cursor** (`cursor_provider_test.go`): assert `Execute` with non-empty
   `cfg.Effort` returns `ErrEffortUnsupported` *without* starting a
   subprocess. Empty `cfg.Effort` continues to work.

3. **Gemini** (`gemini_provider_test.go`): same as Cursor.

4. **Cross-provider matrix** (`multiagent/agent/effort_test.go`, new file):
   table test covering every (provider × level) pair, asserting:
   - Claude+{low,medium,high,max,auto}: success
   - Codex+{low,medium,high,max,auto}: success (validation is Codex-side)
   - Cursor+anything: `ErrEffortUnsupported`
   - Gemini+anything: `ErrEffortUnsupported`
   - Any provider + invalid string ("turbo"): `ErrInvalidEffort`
   - Any provider + empty: success (no effort applied)

   This is the regression lock — when someone adds a fifth provider, the
   matrix forces a decision.

5. **Integration**: the existing `multiagent/agent/integration/` tests
   already exercise real subprocesses. Add a minimal Codex+effort case
   under an env-gated path so it doesn't run in `bazel test //...`.
   Skip Cursor/Gemini integration — there's nothing to verify beyond the
   unit-test rejection.

No fixture regen needed.

## Rollout

**Decision:** single PR.

For:
- The neutral `agent/effort.go` shim, the matrix test, and the help-text
  update have to land together — they cross-reference each other. Splitting
  forces a temporary state where the shim exists but providers still call
  `claude.ParseEffort`, or the matrix test exists but skips two providers.
- Total surface area is small: ~5 files, ~150 lines. One reviewer, one
  context-load.
- The risk profile is symmetric across the three providers — Codex wires
  in, Cursor/Gemini reject. No provider-specific deep dive needed that
  would benefit from isolated review.

Against:
- Codex changes are the only behavior change for users with existing Codex
  configs; Cursor/Gemini are pure error-path additions. A split would let
  Codex bake separately. But the Cursor/Gemini changes are trivial enough
  that this isn't worth two review cycles.

Land as one PR. Title: `feat(multiagent): wire reasoning effort across all providers`.

## 5-minute handoff

Files to touch, in order:

1. **Create `multiagent/agent/effort.go`** — `EffortLevel` type, constants
   (`EffortAuto/Low/Medium/High/Max`), `ParseEffort(string) (EffortLevel, error)`,
   `ErrInvalidEffort`, `ErrEffortUnsupported`. Body lifted from
   `agent-cli-wrapper/claude/special_commands.go:419-437`.

2. **Edit `multiagent/agent/claude_provider.go:205-211`** — replace
   `claude.ParseEffort(cfg.Effort)` with `agent.ParseEffort(cfg.Effort)`,
   then map to `claude.EffortLevel` with a trivial switch (or by string
   round-trip — they happen to match, but be explicit).

3. **Edit `multiagent/agent/codex_provider.go:104`** — change
   `thread.Ask(ctx, fullPrompt)` to build `[]codex.TurnOption` with
   `codex.WithEffort(string(level))` when `cfg.Effort != ""`. Use
   `agent.ParseEffort` for validation. Pass the variadic to `Ask`.

4. **Edit `multiagent/agent/cursor_provider.go:27`** — early-return
   `ErrEffortUnsupported` if `cfg.Effort != ""`. Add the check before
   `cursor.NewSession`.

5. **Edit `multiagent/agent/gemini_provider.go:39`** — same shape as
   Cursor: early-return `ErrEffortUnsupported` if `cfg.Effort != ""`.
   Place before the `client.Start` block so we don't spawn the ACP
   subprocess just to fail.

6. **Edit `jiradozer/cmd/jiradozer/main.go:92`** — update flag help text
   per the diff above. Also at `:214` switch from `claude.ParseEffort`
   to `agent.ParseEffort` (and drop the `claude` import if it becomes
   unused — likely not, jiradozer uses other claude bits).

7. **Tests**:
   - `multiagent/agent/codex_provider_test.go` — add `TestExecute_PassesEffort`
     using a captured-options fake.
   - `multiagent/agent/cursor_provider_test.go` — add
     `TestExecute_RejectsEffort`.
   - `multiagent/agent/gemini_provider_test.go` — add
     `TestExecute_RejectsEffort`.
   - `multiagent/agent/effort_test.go` (new) — the cross-provider matrix.

8. **Run quality gates** before pushing:
   - `scripts/lint.sh`
   - `bazel run //:gazelle` (new `effort.go` and `effort_test.go` need
     BUILD entries)
   - `bazel test //multiagent/agent/...` with a 1-minute timeout

Out of scope (do not touch): `EffortLevel` vocabulary, jiradozer Effort
plumbing, new providers, config-load-time validation.
