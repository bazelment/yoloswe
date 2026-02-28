# Bramble JSONL Rendering Practices

## Package Layout

| Package | Role | Key files |
|---------|------|-----------|
| `bramble/sessionmodel/` | Model — types, parser, envelope strippers, loader | `types.go`, `parser.go`, `envelope.go`, `loader.go` |
| `bramble/session/` | Controller — session lifecycle, Manager | `manager.go`, `types.go` (aliases) |
| `bramble/app/` | View — TUI rendering | `output.go`, `view.go` |
| `bramble/replay/` | Format detection + parsing | `replay.go`, `raw_jsonl.go`, `claude.go`, `codex.go` |

## Type Ownership

Canonical types (`OutputLine`, `SessionStatus`, `ToolState`, etc.) are defined
in `bramble/sessionmodel/types.go`. The `bramble/session/types.go` re-exports
them as aliases (`type OutputLine = sessionmodel.OutputLine`). This prevents a
circular dependency between `session` and `sessionmodel`.

**Rule:** never import `bramble/session` from `bramble/sessionmodel`.

## Two Render Paths — Keep in Sync

Two functions render `OutputLine` values. Both must handle the same
`OutputLineType` set:

| Function | File | Used by |
|----------|------|---------|
| `formatOutputLineWithStyles` | `bramble/app/output.go` | `OutputModel` (logview, replay, tests) |
| `formatOutputLine` | `bramble/app/view.go` | Live TUI `Model` |

The `TestRenderCoverage_AllOutputTypes` test in `render_coverage_test.go`
catches missing cases in the first path.

## Adding Support for New JSONL Message Types

When Claude Code introduces new message types or fields in
`~/.claude/projects/*.jsonl`:

### 1. Mine the data

Run `/jsonl-mine` to scan real session files and report coverage gaps:

```bash
/jsonl-mine                   # full coverage analysis
/jsonl-mine --fixtures        # generate test fixtures for uncovered types
/jsonl-mine --type progress   # focus on a specific envelope type
```

### 2. Extend the envelope stripper

If the new type is an envelope-only type (no inner vocabulary message):

- **`bramble/sessionmodel/envelope.go`** — add the type to the `switch env.Type`
  block in `FromRawJSONL`. Return `nil` message with populated `RawEnvelopeMeta`.
- **`bramble/sessionmodel/types.go`** — add any new fields to `RawEnvelopeMeta`.

### 3. Extend the loader

- **`bramble/sessionmodel/loader.go`** — add handling in `handleEnvelopeMeta`
  (or `handleSystemMeta` / `handleProgressMeta` for subtypes). Produce an
  `OutputLine` with an appropriate `OutputLineType`.

### 4. Extend the parser (vocabulary messages only)

If the new type is a vocabulary message (system, assistant, user, result,
stream\_event):

- **`bramble/sessionmodel/parser.go`** — add handling in `HandleMessage` or the
  relevant `handle*` method.

### 5. Extend the renderer

If the change introduces a new `OutputLineType`:

- **`bramble/app/output.go`** — add a case in `formatOutputLineWithStyles`
- **`bramble/app/view.go`** — add a case in `formatOutputLine`

### 6. Add test fixtures

- **`bramble/sessionmodel/testdata/full_session.jsonl`** — add representative
  JSONL lines for the new type.
- **`bramble/app/render_coverage_test.go`** — add a test case in
  `TestRenderCoverage_AllOutputTypes`.

### 7. Verify

```bash
# Visual inspection against a real session file
bazel run //bramble/cmd/logview -- ~/.claude/projects/<hash>/<session>.jsonl

# With debug output showing raw line types
bazel run //bramble/cmd/logview -- --debug <file>.jsonl

# Run parser/loader tests
bazel test //bramble/sessionmodel:sessionmodel_test

# Run render coverage tests
bazel test //bramble/app:app_test --test_filter=TestRenderCoverage
```

## Envelope Type → OutputLine Mapping

| Envelope type | Subtype | OutputLine type | Notes |
|---------------|---------|-----------------|-------|
| `assistant` | — | `text`, `thinking`, `tool_start` | Content blocks via `handleAssistant` |
| `user` | — | Updates existing `tool_start` | `tool_result` blocks via `handleUser` |
| `system` | `init` | Sets `SessionMeta` | Via `handleSystem` |
| `system` | `api_error` | `error` | Via `handleSystemMeta` |
| `system` | `turn_duration` | `status` | Via `handleSystemMeta` |
| `system` | `compact_boundary` | `status` | Via `handleSystemMeta` |
| `system` | `local_command` | `status` | Via `handleSystemMeta` |
| `result` | — | `turn_end` + progress update | Via `handleResult` |
| `progress` | `bash_progress` | Skipped | Covered by tool start/result cycle |
| `progress` | `agent_progress` | Skipped | Covered by parent tool tracking |
| `progress` | `mcp_progress` | `status` (completed/failed only) | Via `handleProgressMeta` |
| `progress` | `waiting_for_task` | `status` | Via `handleProgressMeta` |
| `progress` | `hook_progress` | Skipped | Internal |
| `pr-link` | — | `status` | Via `handleEnvelopeMeta` |
| `file-history-snapshot` | — | Skipped | Undo/redo metadata |
| `queue-operation` | — | Skipped | Internal bookkeeping |

## Bazel Test Gotchas

- **Runfiles:** Use `os.Getenv("TEST_SRCDIR")` + `os.Getenv("TEST_WORKSPACE")`
  to locate `testdata/` files. Do not use `runtime.Caller` — it resolves to the
  source tree, not the Bazel sandbox.
- **BUILD.bazel:** Include `data = glob(["testdata/**"])` in `go_test` rules to
  make testdata available at runtime.
