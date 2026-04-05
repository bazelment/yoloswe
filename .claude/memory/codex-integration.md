---
name: Codex integration requirements
description: Codex CLI uses OAuth (not API key), bubblewrap warning is harmless, and sandbox defaults to danger-full-access
type: reference
---

## Codex CLI authentication
Codex uses **OAuth login** (not `OPENAI_API_KEY`). Integration tests work without an API key as long as `codex` is installed and the user has logged in via `codex auth`.

## Integration test environment
- Pass `--test_env=HOME="${HOME}" --test_env=PATH="${PATH}"` so Bazel sandbox can find codex and its OAuth tokens
- Run via `scripts/test-manual.sh` or `bazel test` with manual tag filter
- Tests are tagged `manual` + `local` and excluded from `bazel test //...`

## Bubblewrap (bwrap) warning
The stderr message "Codex could not find system bubblewrap on PATH" is **harmless noise** — codex falls back to vendored bwrap. On systems with AppArmor unprivileged userns restrictions (like our hosts), bwrap-based sandbox modes (`read-only`, `workspace-write`) fail anyway. The code defaults to `danger-full-access` sandbox which bypasses bwrap entirely.

## Silent failure pattern (fixed in PR #103)
Before the fix, when codex returned a failed turn (e.g., usage limit hit), `TurnCompletedEvent.Error` was always nil and `DurationMs` was always 0 — making failures completely opaque ("0.0s, 0 input / 0 output tokens" with no explanation). The fix propagates both fields from the thread to the emitted event.
