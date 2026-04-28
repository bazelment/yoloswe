# Plan: Widen `bramble code-review` scope (issue #175)

## Context

A 10-PR audit of recent kernel `/pr-polish` runs found that ~5/10 substantive
bot comments arriving **after** /pr-polish converged would have been catchable
upstream if the review pipeline were broader along two axes:

1. **Test-quality scrutiny.** Co-located test files are not always reviewed,
   and even when they are read the agent rarely flags the specific
   anti-patterns external bots flag (tautological asserts, broad `Exception`
   catches, ineffective mocks, unused imports/locals, dead probes).
2. **Cross-service contract checks.** When a PR touches multiple top-level
   packages (e.g. `services/python/tenant-service/...` + `services/typescript/forge-v2/...`),
   the reviewers do not specifically trace producer/consumer surfaces, so
   stale-data and signature-shape desyncs slip through.

The /pr-polish-side fix (deterministic local lint gate) shipped separately
and covered the CodeQL-style noise (~6/14 of post-converge bot comments).
This plan addresses the remaining ~5/14.

### Empirical refinement of the issue framing

Reading the four kernel evidence directories under `~/.bramble/projects/`
sharpened where the gap actually is:

- **kernel-2978** (sandbox passthrough). Cursor's envelope already cited
  three test files including `tests/test_smoke.py`; Codex flagged
  `test_smoke.py:45`. The bot comments
  (`test_sandbox_passthrough.py`) were on patterns the reviewers *did not*
  surface: a probe workflow that uses `workflow.unsafe.imports_passed_through()`
  (so the negative control is meaningless), and
  `pytest.raises((WorkflowFailureError, Exception))` (degenerate to
  `Exception`). Both agents read the test files; neither graded test
  *effectiveness*.
- **kernel-2799** (app-registration). Most post-push bot comments are
  CodeQL unused-import findings already covered by the lint gate. The
  remaining bramble-relevant gap is a TRIM normalization mismatch between
  Python (`_normalize_app_name`) and SQL (`_find_existing_by_normalized_name`)
  — a same-file producer/consumer desync that fits the cross-surface prompt.
- **kernel-2998** (deployment overview). Cursor caught the
  recent-window/release-map desync at line 215. Bots additionally caught
  FastAPI route ordering (`/overview` shadowed by `/{deployment_id}`) and
  `Release` model inheriting `updated_at` while migration 018 omits it —
  both cross-surface (route-table vs handler; ORM vs DDL). Strongest
  evidence for a contract-sweep prompt.
- **kernel-2755** is included in the test corpus as a regression check
  (must not *worsen* findings).

**Implication for design.** File-selection is already largely correct —
agents do open same-package test files. The dominant fix is **prompt
scaffolding** that tells the agent *what to look for* in tests and across
packages. Path enumeration and multi-package detection are mechanical
support work for the prompt clauses, not the substance.

## Architecture: thin contract, fat skill

Two prior framings were considered and discarded:

- **All-bramble bolt-on.** Compute test-scope and multi-package detection
  inside bramble, append clauses to the prompt. Captures non-/pr-polish
  consumers but bakes still-evolving heuristics into Go.
- **Refactor /pr-polish prep into bramble.** Move ~370 LOC of Python
  (lint gate, base detection, file enumeration) into a `bramble code-review prepare`
  subcommand. Cleanest long-term but freezes detection thresholds and
  severity rules just when they're most likely to change. The audit cycle
  that produced issue #175 will produce more ideas; locking the prep layer
  behind Go's release cadence makes each one harder to ship.

**Picked: thin contract, fat skill.** bramble owns only what it must —
prompt structure and a small, stable consumption point. Computation of
scope hints stays in Python where it can be tuned in flight. Issue #175
ships in roughly the same calendar time as either alternative, with
roughly half the bramble surface change.

### Stability gradient (why the split lands here)

| Component | Stability | Right home |
|---|---|---|
| Prompt structure (base prompt, JSON output rules, severity guidance) | Stable; bramble already owns it | **bramble** |
| Test-quality prompt clause | Must compose with `buildBasePrompt` and the JSON schema | **bramble** |
| Cross-service prompt clause | Same | **bramble** |
| `PromptOptions` struct on the reviewer | Stable shape, evolves with the prompt | **bramble** |
| `topic_of`, envelope parsing (`extract_terminal_envelope`, `parse_envelope`) | Stable | /pr-polish (already there) |
| Lint runners (severity rules, new linters, format changes) | **Actively churning** | /pr-polish |
| `detect_base_branch`, `changed_files` | Stable but git-quirk-sensitive; tuned empirically against real repos | /pr-polish (where the tuning happened) |
| Multi-package detection threshold | **Brand new, unproven** | /pr-polish |
| Test-scope path enumeration | New; rules are language conventions | /pr-polish (lives next to the detection that tunes alongside it) |
| Triage, multi-source consensus, N+1 spiral, state files | PR-loop-specific | /pr-polish |

## What lands in bramble (the thin contract)

### 1. `PromptOptions` struct + `BuildJSONPromptWithScope` in `yoloswe/reviewer/reviewer.go`

```go
type PromptOptions struct {
    SkipTestExecution    bool
    TestScopeHints       []string   // when non-empty, append test-quality clause + paths
    CrossServicePackages []string   // when len >= 2, append cross-service clause
}

// BuildJSONPromptWithScope is the new entry point. The existing
// BuildJSONPromptWithOptions and BuildJSONPrompt remain as 1-line shims that
// pass empty TestScopeHints/CrossServicePackages, preserving byte-for-byte
// legacy output for yoloswe/swe.go:383 and any future caller that hasn't
// opted in.
func BuildJSONPromptWithScope(goal string, opts PromptOptions) string { ... }
```

Composition order in the prompt body:

1. `buildBasePrompt(goal, opts.SkipTestExecution)` — unchanged.
2. Test-quality clause — emitted iff `len(opts.TestScopeHints) > 0`.
   Includes the bullet list of patterns to flag and the path list (capped
   at 50; "(... and N more)" suffix when truncated).
3. Cross-service clause — emitted iff `len(opts.CrossServicePackages) >= 2`.
   Includes the package list verbatim and the 5-item flag list (signature
   shape, async desync, error path, silent fallback, route/schema ordering).
4. JSON output rules — unchanged from today's `BuildJSONPromptWithOptions`.

Each clause is gated by *the data being present*, not by a separate
boolean. This collapses the (boolean, list) cartesian product into a
single source of truth: an empty list means "don't emit." The hints
file controls everything; there's nothing to disable independently.

The exact text for both clauses is in **Appendix A** at the end of this
plan; each bullet maps to a real bot finding from the kernel evidence.

### 2. New flag on `bramble code-review` in `bramble/cmd/codereview/codereview.go`

| Flag | Default | Purpose |
|---|---|---|
| `--scope-hints-file PATH` | `""` | JSON file with `{"schema_version":1, "test_paths":[...], "cross_service_packages":[...]}` |

When `--scope-hints-file` is empty, behavior is identical to today.
When it points to a valid file, the contents populate `PromptOptions`:
the test-quality clause is appended when `test_paths` is non-empty, and
the cross-service clause is appended when `cross_service_packages` has
≥2 entries. To suppress either clause, the caller writes an empty list
to the corresponding field of the hints file — no separate CLI flag.

When the file is missing or malformed, bramble logs an slog warning,
falls back to today's behavior, and continues — never aborts the review
on a malformed scope-hints file.

### 3. `ScopeHints` JSON schema (the public contract)

`yoloswe/reviewer/scope_hints.go` (new file, ~80 LOC):

```go
type ScopeHints struct {
    SchemaVersion        int      `json:"schema_version"`
    TestPaths            []string `json:"test_paths"`
    CrossServicePackages []string `json:"cross_service_packages"`
}

func LoadScopeHints(path string) (*ScopeHints, error) { ... }
func (h *ScopeHints) ToPromptOptions() PromptOptions { ... }
```

`schema_version: 1` is the wire contract. Future fields (e.g. a list of
"surface symbols to trace across packages") add as new optional fields with
no version bump. A version bump only happens on a breaking shape change,
matching how `reviewer.ResultEnvelope` is versioned.

### 4. Test additions in bramble

- `yoloswe/reviewer/reviewer_test.go` — assert clauses appear iff opts
  set; assert legacy `BuildJSONPrompt(goal)` byte-equal to a golden file
  in `yoloswe/reviewer/testdata/legacy_json_prompt.txt`.
- `yoloswe/reviewer/scope_hints_test.go` — load good/bad/missing files,
  schema version mismatch, malformed JSON, empty arrays, oversized lists.
- `bramble/cmd/codereview/codereview_test.go` — flag wiring; verify
  malformed scope-hints file falls back to today's prompt and logs.

**Total bramble change: ~250 LOC across 3 files + 1 testdata fixture.**

## What lands in /pr-polish (the fat skill side)

A new `scope_gate.py` (~150 LOC) under `~/.claude/skills/pr-polish/scripts/`.
Same conventions as `lint_gate.py`: stdlib only, single I/O boundary
through `_common.run`, atomic JSON writes, prints the output path on
stdout for orchestrator pipelines.

```
python3 scope_gate.py --state-dir <dir> [--base BRANCH] \
    [--cross-service-roots services/<lang>/<svc>/,...]
```

**Run cadence: once per round, overwriting a single file.** Kernel-2755
evidence shows the scope set genuinely grows across rounds — by round 5
a fix-introduced test file and a refactor-extracted helper module are in
the diff that weren't there at round 1. Recomputing per round catches
that. Cost is ~100ms (vs 60–400s per backend turn), so it's noise.
Storage stays flat: `<state_dir>/scope-hints.json`, overwritten each
round. Per-round audit trail lives in bramble's run log via
`BRAMBLE_RUN_TAG`, not duplicated to disk.

It does:

1. `changed_files(base)` — reuse existing helper from `lint_gate.py`
   (move to `_common.py` if not already there).
2. **Multi-package detection.** Bucket changed files by the configured
   roots (default: first two segments under known monorepo prefixes).
   Trigger when ≥2 buckets AND ≥3 changed files.
3. **Test path enumeration** for each changed source file:
   - Python: `test_*.py`, `*_test.py`, package-local `tests/` dir at
     any depth under a directory containing changed `.py` files.
   - Go: sibling `*_test.go` in the same package directory.
   - TS/JS: `*.test.{ts,tsx,js,jsx}`, `*.spec.{ts,tsx,js,jsx}`, files
     under `__tests__/` adjacent to the changed source.
4. Sort + dedupe + cap at 50 paths.
5. Emit `<state_dir>/scope-hints.json` (no `r<n>/` subdir; latest wins)
   with the schema bramble expects (`schema_version: 1`, `test_paths`,
   `cross_service_packages`).
6. Print the path so the orchestrator can use it as
   `--scope-hints-file=<path>` on each backend Monitor invocation.

### /pr-polish wire-up changes

Two small edits, no new orchestration shapes:

- `bramble_ops.py:format_monitor_command()` — add an optional
  `scope_hints_file` parameter; when set, append `--scope-hints-file` to
  the assembled command. ~5 LOC change.
- `SKILL.md` — add a step before the per-round backend Monitors:
  "Run `python3 $SKILL_DIR/scripts/scope_gate.py --state-dir … --round …`
  once per round; pass the printed path to each backend invocation via
  `--scope-hints-file`." ~5 lines of prose.

### Tests in /pr-polish

`scripts/tests/test_scope_gate.py` (new) — unit tests over a fake repo
tree built with `tmp_path`. Cover Python/Go/TS conventions, multi-package
threshold, custom roots, empty diff (no trigger, empty file), git failure
(no trigger, empty file). Mirrors the test pattern already used for
`lint_gate.py`.

## Critical files to modify

**bramble (Go):**

- `yoloswe/reviewer/reviewer.go` — add `PromptOptions`,
  `BuildJSONPromptWithScope`, `testQualityClause()`, `crossServiceClause()`.
  Keep `BuildJSONPrompt`/`BuildJSONPromptWithOptions` as shims.
- `yoloswe/reviewer/scope_hints.go` (new) — `ScopeHints`, `LoadScopeHints`,
  `ToPromptOptions`.
- `yoloswe/reviewer/reviewer_test.go` (extend) — clause presence/absence,
  legacy golden.
- `yoloswe/reviewer/scope_hints_test.go` (new) — schema parsing.
- `yoloswe/reviewer/testdata/legacy_json_prompt.txt` (new) — golden.
- `bramble/cmd/codereview/codereview.go` — wire `--scope-hints-file`
  flag into `PromptOptions` at line 180.
- `bramble/cmd/codereview/codereview_test.go` (extend) — flag wiring,
  malformed-file fallback.

**/pr-polish (Python):**

- `scripts/scope_gate.py` (new) — ~150 LOC.
- `scripts/_common.py` — small additions if `changed_files` and language
  bucketing helpers want to be shared between `lint_gate.py` and
  `scope_gate.py`. ~30 LOC of consolidation; otherwise leave alone.
- `scripts/bramble_ops.py:format_monitor_command()` — optional
  `scope_hints_file` parameter.
- `scripts/tests/test_scope_gate.py` (new) — unit tests.
- `SKILL.md` — one new step: "**At the start of each round**, run
  `python3 $SKILL_DIR/scripts/scope_gate.py --state-dir <state_dir>`.
  Pass the printed path to every backend Monitor invocation in that
  round via `--scope-hints-file`. The file is overwritten each round so
  the round-N invocations see the round-N diff, including any test
  files or helpers introduced by round N-1's fixes."

## Backwards compatibility

- `yoloswe/swe.go:383` calls `reviewer.BuildJSONPrompt(goal)` (no
  options). The existing shim is preserved; legacy output is byte-equal
  to today's, locked in by the golden testdata file.
- Today's `bramble code-review` invocations (any caller not passing
  `--scope-hints-file`) get today's prompt. No surprise behavior change.
- The new flag is additive; flag count goes from 11 → 12.
- /pr-polish telemetry already records `goal_len` per run; adding the
  scope-hints file path doesn't change the existing run-log shape.

## Test plan

The acceptance bar in the issue is: re-running on kernel-2978,
kernel-2799, kernel-2998 surfaces the bugs that bots caught post-push.
Three layers:

### Layer 1 — pure-function unit tests (Bazel-runnable + pytest)

`bazel test //yoloswe/reviewer:reviewer_test //bramble/cmd/codereview:codereview_test`

- Prompt builder shape: clauses present/absent by `PromptOptions` flag combos.
- `LoadScopeHints` covers good/bad/missing/version-mismatch.
- Legacy `BuildJSONPrompt(goal)` byte-equal to today's golden output.

`pytest scripts/tests/test_scope_gate.py` (run via `python3 -m pytest`,
not Bazel — same convention as the existing /pr-polish tests):

- Python/Go/TS test-path matchers; dedup; cap at 50.
- Multi-package detection triggers/doesn't at the threshold edges.
- Custom `--cross-service-roots` overrides defaults.
- `git diff` failure produces an empty hints file (trigger=false).

### Layer 2 — replayed-evidence integration tests (manual)

A new directory `yoloswe/reviewer/integration/scope_replay_test.go`
(`# gazelle:ignore`, `tags=["manual","local"]`,
`gotags=["integration"]` per the project's integration-test convention)
loads each kernel evidence directory's diff metadata (from
`actions-r1.json` and, for kernel-2755, the per-round envelopes
`r1..r5/{cursor,codex}-envelope.json`), feeds it through
`scope_gate.py`, and asserts:

- For kernel-2998 only, `cross_service_packages` is non-empty.
- For all four kernels, `test_paths` includes the specific files the
  bots flagged (`tests/test_sandbox_passthrough.py`,
  `tests/integration/test_boundary_*`, etc.).
- The resulting prompt (composed via `BuildJSONPromptWithScope`) is
  byte-stable across runs — snapshot to
  `yoloswe/reviewer/integration/testdata/<kernel-id>.prompt.txt`.
- **Per-round growth check (kernel-2755 only).** Replay round-by-round:
  feed the union of files cited in r1..rN-1 envelopes as a stand-in for
  "files in the diff at round N", invoke `scope_gate.py`, and assert
  the round-5 output contains at least the new files that didn't exist
  at round 1 (`_test_queue.py`, `_test_uv_enforce.py`, `_queue_common.py`,
  `test-hooks.py`, `protect-files.py`). This locks in the "per-round
  recompute is necessary" property so a future optimization can't
  silently drop it.

Driven by `UPDATE_FIXTURES=1` for snapshot regeneration. This layer does
**not** invoke a backend agent — only the prompt bramble would *send*.

### Layer 3 — live backend re-run (manual, evidence-directory comparison)

Documented runbook in the plan file (no automation):

```
# Pre-req: kernel repo cloned at $KERNEL, checked out to each PR's pre-merge SHA.
for pr in 2978 2799 2998 2755; do
  cd $KERNEL
  git checkout $(jq -r .pre_merge_sha ~/.bramble/projects/kernel-$pr/pr-polish-state.json)
  STATE_DIR=$(mktemp -d)
  python3 ~/.claude/skills/pr-polish/scripts/scope_gate.py \
    --state-dir "$STATE_DIR"
  HINTS="$STATE_DIR/scope-hints.json"
  bramble code-review --backend cursor --scope-hints-file "$HINTS" \
    --envelope-file /tmp/$pr-cursor-r2.json
  bramble code-review --backend codex  --scope-hints-file "$HINTS" \
    --envelope-file /tmp/$pr-codex-r2.json
done
```

Compare to `~/.bramble/projects/kernel-$pr/r1/{cursor,codex}-envelope.json`.
Acceptance: r2 envelopes include findings whose path/line match the bot
comments in `pp-comments.json` that were absent from r1.

A small comparison helper at `scripts/compare-r1-r2.py` (in
/pr-polish/scripts/, optional) walks `pp-comments.json`, filters out
CodeQL "unused import" noise (already covered by lint gate), and emits
a report of which substantive bot findings were caught in r1 vs r2.

## Implementation order

1. **Phase 1 — bramble side, behind no-op default.**
   `PromptOptions`, `BuildJSONPromptWithScope`, `LoadScopeHints`, the new
   flags, golden test, malformed-file fallback test. Mergeable as a pure
   refactor: every today-caller still gets today's output.
2. **Phase 2 — /pr-polish side.** `scope_gate.py` + tests +
   `format_monitor_command` change + SKILL.md edit. Wire the orchestrator
   to call `scope_gate.py` once per round and pass the result through
   `--scope-hints-file`.
3. **Phase 3 — validation.** Layer-3 manual rerun on the four kernel
   evidence directories. Update the issue with a comment listing which
   bot findings were and weren't caught in r2.
4. **Phase 4 — soak.** Watch one week of /pr-polish runs for noise
   regressions in the test-quality clause (false-positive nit floods).
   Tune the prompt text in /pr-polish first if bramble's clause is too
   aggressive (the contract supports this: bramble's clause is fixed
   text, but the *trigger* and the *path list* are skill-controlled).

## Verification checklist

- [ ] `scripts/lint.sh && bazel build //... && bazel test //...` green.
- [ ] `bazel test //yoloswe/reviewer:reviewer_test //bramble/cmd/codereview:codereview_test`
      passes including new clause cases and the legacy golden.
- [ ] `pytest ~/.claude/skills/pr-polish/scripts/tests/test_scope_gate.py`
      passes including the language-convention table cases.
- [ ] Layer-2 fixtures regenerate cleanly with `UPDATE_FIXTURES=1`.
- [ ] Layer-3 manual rerun on the four kernel evidence dirs produces
      r2 envelopes whose findings include (at minimum):
  - kernel-2978: a finding citing `imports_passed_through` weakening the
    negative control, OR the broad-`Exception` catch in
    `test_sandbox_passthrough.py`.
  - kernel-2799: a finding citing the Python/SQL TRIM normalization
    mismatch in `_find_existing_by_normalized_name`.
  - kernel-2998: at least one of (a) FastAPI route ordering between
    `/overview` and `/{deployment_id}`, (b) `Release.updated_at` vs
    migration 018, (c) `deployment-history-panel.tsx` dropping succeeded
    rows.
- [ ] kernel-2755 r2 envelope no worse than r1 (no severity regressions,
      no findings dropped).
- [ ] `yoloswe/swe.go:383` callers still get the legacy prompt
      byte-for-byte.
- [ ] `bramble code-review` invocations without `--scope-hints-file`
      produce identical output to today's runs (verified by Layer-2 with
      empty `PromptOptions{}`).

## Out of scope (explicit non-goals)

- No diff-aware file pre-loading into the prompt. Agents still read
  files via their own tools.
- No envelope schema change. Same `ResultEnvelope`, richer contents.
- No second review turn / second envelope. Single-turn fatter prompt.
- No new external dependency in either bramble or /pr-polish.
- No language-aware AST parsing. Test-file matching is purely path-based.
- No bramble-side prep subcommand (`bramble code-review prepare`). The
  size-of-bramble-surface tradeoff favors keeping computation in
  /pr-polish until the heuristics earn stability rights.

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| Prompt becomes too long, agent latency or cost balloons. | 50-path cap on test scope hints; cross-service clause only when multi-package detected. Layer-2 test asserts prompt size under e.g. 8 KB for the four kernels. |
| Test-quality clause turns into a nit factory. | Existing prompt's "avoid nit-level comments unless they block understanding" rule (`buildBasePrompt` line 129) is re-emphasized at end of the new clause. Phase 4 soak catches regressions before wider rollout. |
| `git diff --name-only` fails in CI/worktree edge cases. | `scope_gate.py` returns an empty hints file on failure; bramble's malformed-file fallback runs the legacy prompt. Reviewer never crashes on a bad hints file. |
| /pr-polish and `yoloswe/swe.go` (or future Go callers) drift on scope-hint computation. | The `ScopeHints` JSON schema is versioned (`schema_version: 1`). Phase 4+ may consolidate by porting `scope_gate.py` to Go inside bramble; the contract is designed to make that transition mechanical when the heuristics earn stability rights. |
| Custom monorepo layouts don't bucket cleanly under default cross-service roots. | `--cross-service-roots` flag on `scope_gate.py` plus `BRAMBLE_CROSS_SERVICE_ROOTS` env override. |
| Backwards-compat regression for the legacy `BuildJSONPrompt` consumer. | Golden snapshot test pinning legacy output byte-for-byte. |

## Appendix A — Prompt clause text

### Test-quality clause (gated by non-empty `TestScopeHints`)

```
## Test quality
For each test file in scope (whether in the diff or co-located, see paths
listed below), assess whether the tests would actually catch a regression
of the change under review. Flag patterns that weaken regression signal:
- Tautological assertions (e.g. `type(x) == type(x)`, `assert x == x`,
  `assert isinstance(x, type(x))`).
- Mock setups that bypass the system under test (e.g. patching the function
  itself, asserting only the mock was called, never the new behavior).
- Negative controls that catch too broad an exception class
  (`pytest.raises((Specific, Exception))` is `Exception`).
- Tests that "pass" only because of incidental side-effects in the harness
  (logger sinks that aren't actually written by the workflow under test;
  context managers that force-pass a check the test claims to verify).
- Unused imports, unused locals, unused fixtures (cite by line).
- Missing kwargs/args on construction that the production code now requires
  for behavior under test (e.g. `Worker(...)` called without
  `workflow_runner=` when production code passes it).

Continue to avoid nit-level comments unless they block understanding of
the diff or weaken a stated regression signal.

Co-located test files to read (in addition to anything in the diff):
<one path per line, capped at 50; truncation suffix when applicable>
```

Each bullet maps to a real bot finding from the kernel evidence:

| Bullet | Evidence |
|---|---|
| Tautological asserts | issue body cites kernel-2978 type==type |
| Mock-bypass | codex envelope kernel-2978 test_smoke.py:45 |
| Broad `Exception` catch | cursor[bot] kernel-2978 negative-control low-sev |
| Incidental harness side-effects | cursor[bot] kernel-2978 loguru sink test |
| Unused imports | github-code-quality kernel-2799 |
| Missing constructor kwargs | cursor[bot] kernel-2978 several workers |

### Cross-service clause (gated by multi-package detection)

```
## Cross-service contract sweep
This PR touches multiple top-level packages: <package list>.
Trace every public API/handler/exported symbol modified in one package to its
consumers in the others. Read both sides of each surface and flag:
1. Signature or shape changes that don't match consumer expectations
   (request/response field names, types, optionality, enum values).
2. Async state updates that desync between producer and consumer
   (optimistic UI updates that diverge from refetched server state,
   stale-while-revalidate paths returning prior values).
3. Error or loading paths whose handling differs across packages
   (one side throws, the other silently falls back; one side surfaces a
   typed error, the other treats it as success).
4. Silent fallbacks that swallow values from another service (default
   values masking missing fields, empty arrays masking failed lookups).
5. Route-table or schema ordering issues where one definition shadows or
   conflicts with another (FastAPI path-parameter ordering, ORM-mixin
   columns vs explicit migrations, OpenAPI tag collisions).

When citing an issue, name both sides (file:line) and explain the desync
explicitly. If both sides agree, do not flag the surface.
```

Item 5 is added beyond the issue body's draft because kernel-2998
evidence shows route-ordering and ORM-vs-migration are the same shape of
bug and bots reliably catch them.

---

## Note on plan-file location

The harness pinned this plan to
`/home/ubuntu/.claude/plans/valiant-waddling-harp.md`, overriding the
user's request to put it under `plans/` in the worktree. After plan
approval, the file should be copied (or symlinked) to
`/home/ubuntu/worktrees/yoloswe/plan/issue-175-widen-review-scope/plans/issue-175-widen-review-scope.md`
so the implementer can find it next to the worktree's checked-out code.
