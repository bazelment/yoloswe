# Architecture Design - Cycle 3 Final Improvements

**Date:** 2026-02-10
**Scope:** Four focused changes addressing MEDIUM/LOW items from the Cycle 3 PM assessment (E3, E4, E6, and dry-run summary).

---

## Change 1: Extract shared agent launch helper (E6)

### Problem

`Fix()` (lines 249-327) and `FixFromTracker()` (lines 355-433) in `engine.go` contain near-identical logic for:
1. Dry-run group enumeration (~13 lines each)
2. Bounded-parallelism goroutine launch with semaphore, mutex, WaitGroup (~50 lines each)
3. Dispatching singleton vs group agents with identical config construction (~20 lines each)

Total: ~80 duplicated lines.

### Design

Extract a single private method on `*Engine`:

```go
// launchAgents runs fix agents for the given groups, populating fixResult.
// It handles dry-run short-circuit, bounded parallelism, and tracker updates.
func (e *Engine) launchAgents(ctx context.Context, groups []IssueGroup, fixResult *FixResult)
```

Both `Fix()` and `FixFromTracker()` call it after obtaining their group list. The method owns everything from the dry-run check through `wg.Wait()`, including tracker status updates (`StatusInProgress`) and result accumulation.

### Scope of change

- **File:** `medivac/engine/engine.go`
- `Fix()` reduces to: scan, get actionable, group, log, call `launchAgents`, save tracker.
- `FixFromTracker()` reduces to: get actionable, group, log, call `launchAgents`, save tracker.
- No signature changes to public API. No new files.

### Config construction detail

The method builds `FixAgentConfig` / `GroupFixAgentConfig` internally from `e.config`, which already holds all required fields (`WTManager`, `GHRunner`, `AgentModel`, `AgentBudget`, `Branch`, `SessionDir`, `RepoDir`). The only per-group field is the logger context (`issue` or `group`), which is derived from the group itself.

### Test impact

Existing `engine_test.go` tests exercise `Fix()` and `FixFromTracker()` through the public API. No new tests needed; existing tests cover the refactored path. Verify with `bazel test //medivac/engine:engine_test`.

---

## Change 2: Improve dependabot grouping (E3)

### Problem

`extractPackageName` uses regex `(?i)(?:for|of)\s+([a-z][a-z0-9_-]*)` which only matches packages preceded by "for" or "of". The summary "no security update needed as cryptography is no longer vulnerable" does not match because "cryptography" follows "as", not "for"/"of".

### Design

Add a second regex pattern as a fallback. The approach uses a broader set of English prepositions that commonly precede package names in dependabot messages:

```go
// Primary: matches "for <pkg>" or "of <pkg>"
var packageNamePrimary = regexp.MustCompile(`(?i)(?:for|of)\s+([a-z][a-z0-9_.-]*)`)

// Fallback: matches "as <pkg>" or bare "<pkg> is" / "<pkg> version" / "<pkg>>=..."
var packageNameFallback = regexp.MustCompile(`(?i)\bas\s+([a-z][a-z0-9_.-]*)\b`)
```

The function tries the primary regex first. On miss, it tries the fallback. This is conservative -- it only adds "as" as a new preposition rather than trying to guess arbitrary words, keeping false-positive risk low.

Additionally, the primary regex character class should include `.` to handle scoped/dotted package names (e.g., `clerk-backend-api`).

### Updated function

```go
func extractPackageName(summary string) string {
    if m := packageNamePrimary.FindStringSubmatch(summary); len(m) >= 2 {
        return strings.ToLower(m[1])
    }
    if m := packageNameFallback.FindStringSubmatch(summary); len(m) >= 2 {
        return strings.ToLower(m[1])
    }
    return ""
}
```

### Scope of change

- **File:** `medivac/engine/grouping.go` -- modify `extractPackageName` and its regexes
- **File:** `medivac/engine/grouping_test.go` -- add test cases:
  - `"no security update needed as cryptography is no longer vulnerable"` -> `"cryptography"`
  - `"Dependency resolution failed for clerk-backend-api"` -> `"clerk-backend-api"` (existing should still pass with `.` in char class)
  - Existing test cases must continue to pass

### Risk

Low. The "as" preposition in English rarely appears before a non-package word in dependabot summaries. The change is additive (fallback only fires when primary misses).

---

## Change 3: Sort status output (E4)

### Problem

Issues within each status group in `printStatus` appear in arbitrary map-iteration order. The PM assessment requests sorting by `seen_count` descending, then by `category`.

### Design

After grouping issues by status, sort each group slice before printing:

```go
sort.Slice(group, func(i, j int) bool {
    if group[i].SeenCount != group[j].SeenCount {
        return group[i].SeenCount > group[j].SeenCount // descending
    }
    return group[i].Category < group[j].Category // ascending alpha
})
```

### Scope of change

- **File:** `medivac/cmd/medivac/status.go` -- add `sort` import, add `sort.Slice` call inside the status-group print loop, before the `for _, iss := range group` line.
- No test file changes (status output is not unit-tested; this is CLI formatting).

---

## Change 4: Dry-run group summary header

### Problem

When `--dry-run` shows fix results, the output jumps straight to individual `[SKIP]` lines. For large issue sets (58 issues, 6 groups), a summary line at the top provides immediate context.

### Design

Add a summary header in `printFixResult` right after the `=== Fix Results ===` banner, before the per-agent detail:

```
=== Fix Results ===
Agents launched: 6 (4 single, 2 grouped)
Total issues:    58 (in 6 groups)
Total cost:      $0.0000
```

The "Total issues" line is the new addition. It requires computing the total issue count across all results:

```go
totalIssues := len(r.Results) // each singleton result = 1 issue
for _, gr := range r.GroupResults {
    totalIssues += len(gr.Group.Issues)
}
fmt.Printf("Total issues:    %d (in %d groups)\n", totalIssues, totalAgents)
```

### Scope of change

- **File:** `medivac/cmd/medivac/fix.go` -- modify `printFixResult`, add 4 lines after the "Agents launched" line.

---

## Implementation Order

1. **Change 1** (extract `launchAgents`) -- do first since it touches the most code and all other changes are independent of it.
2. **Change 2** (dependabot regex) -- independent, includes test updates.
3. **Change 3** (status sort) -- trivial, independent.
4. **Change 4** (dry-run summary) -- trivial, independent.

Changes 2-4 can be implemented in parallel after Change 1 is complete, or in any order since they touch different files.

## Verification

After all changes:
```
scripts/lint.sh
bazel test //medivac/... --test_timeout=60
```

All three existing test targets (`engine_test`, `grouping_test`, `buildinfo_test`) must pass, plus the new test cases in `grouping_test.go`.
