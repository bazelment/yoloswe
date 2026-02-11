# Fixer PM Assessment - Cycle 3 (FINAL)

**Date:** 2026-02-10
**Target repo:** sycamore-labs/kernel (Nx monorepo, TypeScript + Python + Go)
**Fixer version:** commit 0eabc0b (feature/fixer branch)
**Previous assessments:** pm-assessment-cycle1.md, pm-assessment-cycle2.md

---

## A. Full Scorecard (Cycle 1 -> 2 -> 3)

| Metric | Cycle 1 | Cycle 2 | Cycle 3 | Change C2->C3 |
|--------|---------|---------|---------|----------------|
| Total tracked issues (2 scans) | 38 | 18 | 63 (59+4 dedup leak) | See notes |
| Phantom duplicates from 2nd scan | 18 new | 1 new | 4 new | Regression (see B6) |
| Agents launched (dry-run) | 38 | 18 | **6** | **-67% (major win)** |
| Agent groups (multi-issue) | 0 | 0 | **2** | New capability |
| Largest group size | N/A | N/A | **42 issues** | New capability |
| Fix prompt accuracy for Nx repo | 0% (Bazel) | ~95% (Nx+uv) | ~95% (unchanged) | Stable |
| Updated list duplicates | Yes (bug) | No | No | Stable |
| Timestamp inversion | Yes (bug) | No | No | Stable |
| Dead --repo-name flag | Present | Removed | Removed | Stable |
| --skip-scan flag | Missing | Working | Working | Stable |
| Dry-run output quality | ID only | ID+summary+file | ID+summary+file+GROUP detail | Improved |
| Issue grouping | Missing | Missing | **Working (6 groups from 58 issues)** | **Added** |
| Dismiss/reopen capability | Missing | Missing | **Working** | **Added** |
| Status output: dismissed section | N/A | N/A | **Shows wont_fix section with reason** | **Added** |
| Triage: error_code field | Not present | Not present | **Present (57/63 issues have it)** | **Added** |
| Triage: ENUMERATE instruction | Not present | Not present | **Present** | **Added** |
| Triage: repo-root paths | Inconsistent | Inconsistent | **Improved (explicit prompt rule)** | **Improved** |
| Unit tests | 3 targets pass | 3 targets pass | 3 targets pass | Stable |

### Why total issues increased from 18 to 63

This is NOT a regression. Cycle 3 scans discovered more issues because:
1. The triage prompt now says "ENUMERATE every individual error" instead of allowing the LLM to summarize. Scan 1 in Cycle 2 found 21 parsed failures; Scan 1 in Cycle 3 found 113. This is correct behavior -- the LLM is now listing every individual TS error instead of summarizing "55 TypeScript errors" as 3 entries.
2. The Docker Publish workflow has 55 failures per run (each TypeScript compilation error is enumerated individually).
3. Dedup across the two Docker Publish runs works: 54 issues were matched as "Updated (seen again)" -- these are the same errors appearing in both runs.

The key metric is not total issues (which should be high for thorough enumeration) but **agents launched**, which dropped from 18 to 6.

---

## B. Grouping Effectiveness

### B1. Agent count reduction

| Before grouping | After grouping | Reduction |
|-----------------|----------------|-----------|
| 58 actionable issues | 6 agents (4 singleton + 2 grouped) | **90% reduction** |

### B2. Group composition

The 6 groups from the dry-run output:

| Group | Key | Issues | Description |
|-------|-----|--------|-------------|
| 1 | `ts:TS2307:services/typescript/forge-v2/` | **42** | All "Cannot find module '@sycamore-labs/ui'" errors |
| 2 | `ts:TS7006:services/typescript/forge-v2/` | **12** | All "Parameter X implicitly has 'any' type" errors |
| 3 | singleton | 1 | `1b1eacaa` infra/dependabot -- cryptography no longer vulnerable |
| 4 | singleton | 1 | `271ceb68` build/docker -- exit code 2 during Docker build |
| 5 | singleton | 1 | `82a43bd8` build/docker -- pnpm build failed |
| 6 | singleton | 1 | `7f61e9df` infra/dependabot -- cryptography version conflict |

### B3. Are the right issues grouped together?

**YES, with one caveat.**

The TS2307 group (42 issues) correctly contains all "Cannot find module" errors from forge-v2. These all share the same root cause: the `@sycamore-labs/ui` package is missing or not built.

The TS7006 group (12 issues) correctly contains all "implicitly has 'any' type" errors. These all need explicit type annotations.

**Caveat:** The two Docker build failures (271ceb68 and 82a43bd8) are SYMPTOMS of the TS errors. They represent "pnpm build failed" which fails because of the TypeScript compilation errors. Ideally, these would be linked to the TS groups as "caused_by" and excluded from agent assignment, saving 2 agents. This root-cause linking was identified in Cycle 2 (B4) and is still not implemented.

### B4. Are singleton groups handled correctly?

**YES.** The 4 singletons are all issues that genuinely have no grouping partner:
- 2 Docker build failures (different summaries from different runs, not grouped because they don't have TS error codes)
- 2 dependabot issues (different packages/messages, not grouped because `extractPackageName` returns "cryptography" for the version conflict but the "no longer vulnerable" message doesn't match the same pattern)

**Minor issue:** The two dependabot issues (`1b1eacaa` and `7f61e9df`) both relate to the cryptography package. `7f61e9df` extracts "cryptography" correctly, but `1b1eacaa` summary is "no security update needed as cryptography is no longer vulnerable" -- `extractPackageName` uses the regex `(?i)(?:for|of)\s+([a-z][a-z0-9_-]*)` which matches "for" or "of" followed by a word. This summary doesn't contain "for" or "of" before "cryptography", so the package name is not extracted and it falls to a singleton.

### B5. Group fix prompt quality

The `buildGroupFixPrompt` function in `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/engine/prompts.go` correctly:
- Lists ALL individual failures with file, line, and details
- Uses the group key for context
- Instructs the agent to "fix ALL listed failures, not just the first one"
- Uses the leader issue's category for command selection (lint -> lint command)
- Includes CLAUDE.md content and build system detection

The prompt is well-structured for an agent to fix 42 TS2307 errors in one pass.

---

## C. Dismiss/Reopen Workflow

### C1. Dismiss command

**PASS.** Command:
```
fixer dismiss 74a3a396 --repo-root ... --reason "transient CI flake"
```

Output:
```
Dismissed [74a3a396] lint/go -- parallel golangci-lint is running
  Reason: transient CI flake
```

The issue was immediately excluded from `GetActionable()`. The dry-run showed 58 actionable issues (59 total minus 1 dismissed).

### C2. Persistence across re-scans

**PASS.** After running a full re-scan:
- The dismissed issue `74a3a396` remained in `wont_fix` status
- Status output showed it under a separate "wont_fix (1):" section with reason
- It was NOT included in the actionable count

**However:** The re-scan created a NEW golangci-lint issue `0c93309a` because the LLM returned a different file path (`go-common` vs `services/go/common`). The dismissed issue is protected, but a new near-duplicate escapes the dismiss. This is a dedup issue, not a dismiss issue.

### C3. Reopen command

**PASS.** Command:
```
fixer reopen 74a3a396 --repo-root ...
```

Output:
```
Reopened [74a3a396] lint/go -- parallel golangci-lint is running
```

After reopen:
- Issue returned to "new" status in the status output
- Issue appeared in the regular "new" section, not the "wont_fix" section
- Would be included in future `GetActionable()` calls

### C4. Error handling

Tested reopen on a non-dismissed issue (it correctly requires the issue to be in wont_fix status):
```go
if iss.Status != StatusWontFix {
    return fmt.Errorf("issue %s is not dismissed (status: %s)", id, iss.Status)
}
```

Dismiss on non-existent ID returns an error. Both are tested in unit tests.

---

## D. Data Quality Assessment

### D1. issues.json structure

The issues.json contains 63 issues with clean structure:
- **error_code field**: 57 of 63 issues have it populated (all lint/ts issues). This is new in Cycle 3 and critical for grouping.
- **Signature format**: `{hash}:{canonical_path}` -- e.g., `95d7677c111ea9a7:src/components/foo.tsx`
- **Seen count**: Properly incremented -- most lint/ts issues show `seen_count: 4` (seen in 2 scans x 2 runs per scan)
- **Timestamps**: `first_seen < last_seen` for all issues (no inversions)

### D2. Category distribution

| Category | Count |
|----------|-------|
| lint/ts | 54 |
| build/docker | 4 |
| infra/dependabot | 3 |
| lint/go | 2 |

### D3. Dedup issues found in Cycle 3

The second scan created 4 new issues that should have matched existing ones:

| New ID | Existing ID | Why no match |
|--------|-------------|-------------|
| `0c93309a` | `74a3a396` | File path: `go-common` vs `services/go/common` |
| `ac4b9936` | `271ceb68` | Summary: "failed to solve: process..." vs "Process completed with exit code 2" |
| `9708d511` | `82a43bd8` | Summary: "Docker build failed: pnpm build exited..." vs "pnpm build failed with exit code 2" |
| `31586e89` | `7f61e9df` | Summary: shorter version vs longer version with "clerk-backend-api" detail |

**Root cause:** The LLM triage produces slightly different summaries and file paths across invocations for non-TypeScript issues. The TypeScript issues have stable dedup because the error messages are exact compiler output. The Docker/Go/dependabot issues have LLM-paraphrased summaries that vary between calls.

**Severity:** Medium. The 54 TypeScript issues (86% of total) dedup correctly. The 4 leaks are in edge categories where the LLM paraphrases rather than quoting compiler output.

---

## E. Remaining Issues

### E1. [MEDIUM] Docker build failures not linked to root cause

The two Docker build failure singletons (`271ceb68`, `82a43bd8`) are symptoms of the TS errors. An agent launched for these cannot fix them independently. They should be excluded from agent assignment or linked to the TS groups.

**Impact:** 2 wasted agent invocations per fix run.

### E2. [MEDIUM] Dedup instability for non-compiler issues

As documented in D3, 4 of 63 issues leaked as duplicates on rescan. The signature computation works well for compiler errors (exact messages) but poorly for LLM-paraphrased summaries.

**Possible fix:** For categories like `build/docker` and `infra/dependabot`, use a broader matching strategy: same category + same file = same issue (ignore summary hash). Or use fuzzy matching with an edit distance threshold.

### E3. [MEDIUM] Dependabot grouping incomplete

The regex `(?:for|of)\s+([a-z][a-z0-9_-]*)` misses the pattern "as cryptography is no longer vulnerable" because "cryptography" is not preceded by "for" or "of". A broader regex like `\b(cryptography|numpy|requests|...)\b` or a more general heuristic would catch this.

### E4. [LOW] Status output still not sorted

Issues within the "new" status group appear in arbitrary order. Sorting by seen_count descending or category would improve readability.

### E5. [LOW] No color or terminal formatting

All output remains monochrome. This was identified in Cycle 1 and has not been addressed.

### E6. [LOW] Code duplication between Fix() and FixFromTracker()

In `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/engine/engine.go`, the agent launch logic (lines 249-327 and 355-433) is nearly identical between `Fix()` and `FixFromTracker()`. This should be extracted to a shared helper function to reduce maintenance burden.

---

## F. Deployment Readiness Assessment

### F1. Is the tool ready for real use against the scanner repo?

**Verdict: CONDITIONALLY READY for a supervised first run.**

The tool can now:
1. Scan CI failures and enumerate individual errors (113 raw failures from 5 runs)
2. Dedup them to 59 unique issues (86% dedup rate on TypeScript errors)
3. Group 58 actionable issues into 6 agent groups (90% reduction)
4. Dismiss known flakes (golangci-lint parallel run)
5. Generate correct build/test/lint commands for the Nx repo
6. Include CLAUDE.md instructions in fix prompts

### F2. What would need to happen before running `medivac fix` for real?

**Pre-flight checklist:**

1. **Dismiss the golangci-lint flake.** This is a transient CI issue, not a code bug. Launching an agent for it is a waste.
   ```
   fixer dismiss 74a3a396 --reason "transient CI flake"
   fixer dismiss 0c93309a --reason "duplicate of 74a3a396, transient CI flake"
   ```

2. **Dismiss the Docker build symptom issues.** These will resolve when the TS errors are fixed.
   ```
   fixer dismiss 271ceb68 --reason "symptom of TS errors, will resolve when TS2307/TS7006 fixed"
   fixer dismiss 82a43bd8 --reason "symptom of TS errors"
   fixer dismiss ac4b9936 --reason "duplicate of 271ceb68"
   fixer dismiss 9708d511 --reason "duplicate of 82a43bd8"
   ```

3. **Dismiss the dependabot issues.** These require human decision-making about dependency versions, not automated code fixes.
   ```
   fixer dismiss 1b1eacaa --reason "dependabot issue, needs human review"
   fixer dismiss 7f61e9df --reason "dependabot issue, needs human review"
   fixer dismiss 31586e89 --reason "duplicate of 7f61e9df"
   ```

4. **After dismissals, run dry-run to confirm.** Should show 2 groups:
   - `ts:TS2307:services/typescript/forge-v2/` (42 issues) -- fix: add/configure `@sycamore-labs/ui` package
   - `ts:TS7006:services/typescript/forge-v2/` (12 issues) -- fix: add explicit type annotations

5. **Set appropriate budget.** The TS2307 group (42 files with missing module) likely has a single root cause: the `@sycamore-labs/ui` package is not in the project's dependencies or not built. A smart agent should fix the package.json/tsconfig, not individually annotate 42 files. Budget of $2-5 is appropriate per agent.

6. **Consider running TS2307 group first.** This is the higher-impact fix (42 issues). If the agent correctly identifies that the fix is to add `@sycamore-labs/ui` to dependencies, all 42 issues resolve at once. The TS7006 group can run after.

### F3. Risk assessment for a real fix run

| Risk | Likelihood | Mitigation |
|------|-----------|------------|
| Agent creates wrong fix (e.g., suppresses errors instead of fixing) | Medium | PR review before merge; `--dry-run` first |
| Agent cannot reproduce error locally (no Docker, no pnpm) | Medium | Worktree has full repo; CLAUDE.md guides the agent |
| Agent budget exceeded for 42-file group | Low | Set `--budget 5` per agent |
| Merge conflicts between TS2307 and TS7006 PRs | Low | Run sequentially, not in parallel |
| Agent modifies files outside forge-v2 | Low | Fix prompt says "minimal, focused changes" |

### F4. Overall tool maturity

| Aspect | Grade | Notes |
|--------|-------|-------|
| Scan accuracy | B+ | Enumerate works, dedup good for TS, weak for Docker/dependabot |
| Issue tracking | B+ | Dismiss/reopen/persistence all work; minor dedup leaks |
| Grouping | A- | 90% agent reduction; Docker symptom linking missing |
| Fix prompts | B+ | Correct for Nx repo; group prompt enumerates all issues |
| CLI UX | B | Clear commands; no color; status unsorted |
| Test coverage | B | Unit tests for grouping, tracker, triage; no integration tests |
| Architecture | B+ | Clean separation; code duplication in Fix/FixFromTracker |

**Final verdict: The tool has progressed from "proof of concept" (Cycle 1) through "functional but impractical" (Cycle 2) to "ready for supervised deployment" (Cycle 3).** The grouping feature is the single biggest improvement -- reducing 58 agents to 6 makes the tool economically viable. The dismiss/reopen workflow gives operators the control needed to manage false positives and flakes. The remaining issues (dedup leaks, Docker symptom linking, status sorting) are polish items, not blockers.
