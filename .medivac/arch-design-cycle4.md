# Architecture Design - Cycle 4: Batch Triage with Reviewed-Run Tracking

**Date:** 2026-02-10
**Scope:** Redesign the scan/triage flow to batch all unreviewed CI runs into a single LLM triage call, eliminating cross-run dedup variance.
**Addresses:** PM assessment D3 (dedup leaks from LLM triage variance across runs), E2 (dedup instability for non-compiler issues).

---

## 1. Chosen Approach: Two-Phase Extract-Then-Deduplicate

### Options Considered

| Option | Description | Pros | Cons |
|--------|-------------|------|------|
| A. Aggressive truncation | Concatenate all logs, truncate to fit one call | Simple | Loses critical error context; truncation is nondeterministic |
| B. Fallback to per-run | If total > threshold, use current behavior | Safe | Does not solve the core problem (cross-run dedup still fragile) |
| **C. Two-phase** | Phase 1: per-run error extraction (structured, no dedup). Phase 2: single dedup call over structured extractions | Solves the real problem; bounded context; LLM sees full picture | Two LLM calls per scan instead of one; slightly higher cost |

### Decision: Option C (Two-Phase)

**Rationale:**

1. **The core problem is LLM variance, not log volume.** When the LLM triages run A and run B independently, it paraphrases differently. The fix is to make the LLM see all errors at once and deduplicate them itself. Option C achieves this.

2. **Phase 1 output is structured and compact.** A single run's triage output is a JSON array of ~5-20 items, each ~200 bytes. Even 10 runs produce only ~40KB of structured data for Phase 2 -- well within context limits.

3. **Phase 1 reuses existing `TriageRun` almost unchanged.** We keep the per-run extraction prompt that is already well-tuned for enumeration and categorization. We only remove the dedup instruction from Phase 1 (dedup moves to Phase 2).

4. **Phase 2 operates on structured data, not raw logs.** The dedup call sees normalized error records, not 250KB of raw logs. This is cheaper (smaller input), faster, and more reliable (no log noise to confuse the LLM).

5. **Context window safety.** Phase 1: each call stays at current MaxLogSize (50KB). Phase 2: 10 runs x 20 errors x 200 bytes = 40KB of structured input. Both phases are safely within Haiku's 200K context window.

6. **Budget control.** Phase 1 calls are cheap (~$0.005 each with Haiku). Phase 2 is a single call on structured data (~$0.002). Total for 5 runs: ~$0.027 vs current ~$0.025. Marginal cost increase of ~8%.

---

## 2. Data Model Changes

### 2.1 New: `ReviewedRuns` in tracker file

Add a top-level field to the persisted tracker JSON to track which run IDs have been triaged.

**File:** `medivac/issue/tracker.go`

```go
// trackerFile is the persistent JSON structure.
type trackerFile struct {
    Issues       []*Issue `json:"issues"`
    ReviewedRuns []int64  `json:"reviewed_runs,omitempty"`
}
```

`ReviewedRuns` stores the database IDs of all runs that have been successfully triaged. This is a list rather than a single "last ID" because run IDs are not guaranteed to be monotonically increasing across workflows (though in practice they are). Using a list is more robust and allows partial-failure recovery.

**Tracker struct additions:**

```go
type Tracker struct {
    issues       map[string]*Issue
    reviewedRuns map[int64]bool  // set of reviewed run IDs
    filePath     string
    mu           sync.Mutex
}
```

**New methods on Tracker:**

```go
// IsRunReviewed returns true if the run has been triaged.
func (t *Tracker) IsRunReviewed(runID int64) bool {
    t.mu.Lock()
    defer t.mu.Unlock()
    return t.reviewedRuns[runID]
}

// MarkRunsReviewed records run IDs as triaged.
func (t *Tracker) MarkRunsReviewed(runIDs []int64) {
    t.mu.Lock()
    defer t.mu.Unlock()
    for _, id := range runIDs {
        t.reviewedRuns[id] = true
    }
}

// ReviewedRunIDs returns all reviewed run IDs (for persistence).
func (t *Tracker) ReviewedRunIDs() []int64 {
    t.mu.Lock()
    defer t.mu.Unlock()
    ids := make([]int64, 0, len(t.reviewedRuns))
    for id := range t.reviewedRuns {
        ids = append(ids, id)
    }
    return ids
}

// PruneReviewedRuns removes run IDs not present in activeIDs.
// Call this after each scan to prevent unbounded growth.
func (t *Tracker) PruneReviewedRuns(activeIDs map[int64]bool) {
    t.mu.Lock()
    defer t.mu.Unlock()
    for id := range t.reviewedRuns {
        if !activeIDs[id] {
            delete(t.reviewedRuns, id)
        }
    }
}
```

### 2.2 New: `RunExtraction` struct for Phase 1 output

**File:** `medivac/github/triage.go`

```go
// RunExtraction holds the Phase 1 output for a single run:
// structured error records extracted by the LLM, before cross-run dedup.
type RunExtraction struct {
    Run        WorkflowRun
    FailedJobs []JobResult
    Items      []triageResponse // raw LLM extraction (not yet deduped)
    Cost       float64
}
```

### 2.3 New: `BatchTriageResult` struct for Phase 2 output

**File:** `medivac/github/triage.go`

```go
// BatchTriageResult holds the final output of batch triage.
type BatchTriageResult struct {
    Failures []CIFailure
    Cost     float64 // total cost across all phases
}
```

### 2.4 Existing `CIFailure` -- no changes

The `CIFailure` struct remains unchanged. The `RunID` field will be set to the run ID of the first occurrence (the LLM will pick one representative run when deduplicating).

---

## 3. Flow Diagram

### Current Flow (per-run triage)

```
Scan()
  |
  +--> ListFailedRuns(branch, limit) --> [run1, run2, ..., runN]
  |
  +--> for each run:
  |      |
  |      +--> GetAnnotations(run.ID)
  |      +--> GetJobLog(run.ID) --> CleanLog()
  |      +--> GetJobsForRun(run.ID) --> filter failed
  |      +--> TriageRun(run, jobs, anns, log)  [LLM CALL #i]
  |      |       |
  |      |       +--> buildTriagePrompt() --> LLM --> parseTriageResponse()
  |      |       +--> ComputeSignature() per item
  |      |       +--> dedup within run
  |      |
  |      +--> append failures
  |
  +--> Reconcile(allFailures) --> save tracker
```

### New Flow (batch triage with two phases)

```
Scan()
  |
  +--> ListFailedRuns(branch, limit) --> [run1, run2, ..., runN]
  |
  +--> Filter: remove already-reviewed runs using tracker.IsRunReviewed()
  |    (if ALL runs reviewed, return early with no new failures)
  |
  +--> PHASE 1: Extract errors from each unreviewed run
  |    for each unreviewed run:
  |      |
  |      +--> GetAnnotations(run.ID)
  |      +--> GetJobLog(run.ID) --> CleanLog()
  |      +--> GetJobsForRun(run.ID) --> filter failed
  |      +--> ExtractRun(run, jobs, anns, log)  [LLM CALL - extraction only]
  |      |       |
  |      |       +--> buildExtractionPrompt() --> LLM --> parseTriageResponse()
  |      |       +--> NO signature computation, NO dedup
  |      |
  |      +--> collect RunExtraction
  |
  +--> PHASE 2: Batch deduplicate across all runs
  |    |
  |    +--> DeduplicateBatch(extractions)  [SINGLE LLM CALL]
  |    |       |
  |    |       +--> buildBatchDedupPrompt(extractions) --> LLM --> parseBatchResponse()
  |    |       +--> ComputeSignature() per deduplicated item
  |    |
  |    +--> returns []CIFailure (deduplicated across all runs)
  |
  +--> tracker.MarkRunsReviewed(unreviewedRunIDs)
  +--> tracker.PruneReviewedRuns(activeRunIDs)
  +--> Reconcile(allFailures) --> save tracker
```

### Optimization: Skip Phase 2 when only one unreviewed run

When there is exactly one unreviewed run, Phase 2 adds no value (there is nothing to deduplicate across). In this case, compute signatures directly from Phase 1 output and skip the Phase 2 LLM call. This preserves current cost for the common single-run case.

---

## 4. File-by-File Changes

### 4.1 `medivac/issue/tracker.go`

**Add `reviewedRuns` field to `Tracker` struct and update persistence.**

```go
// trackerFile is the persistent JSON structure.
type trackerFile struct {
    Issues       []*Issue `json:"issues"`
    ReviewedRuns []int64  `json:"reviewed_runs,omitempty"`
}

type Tracker struct {
    issues       map[string]*Issue
    reviewedRuns map[int64]bool
    filePath     string
    mu           sync.Mutex
}
```

**Update `NewTracker` to initialize the map:**

```go
func NewTracker(filePath string) (*Tracker, error) {
    t := &Tracker{
        issues:       make(map[string]*Issue),
        reviewedRuns: make(map[int64]bool),
        filePath:     filePath,
    }
    if err := t.load(); err != nil {
        return nil, err
    }
    return t, nil
}
```

**Update `load()` to populate `reviewedRuns`:**

```go
func (t *Tracker) load() error {
    // ... existing file read ...
    var f trackerFile
    if err := json.Unmarshal(data, &f); err != nil {
        return fmt.Errorf("parse tracker file: %w", err)
    }
    for _, issue := range f.Issues {
        t.issues[issue.Signature] = issue
    }
    for _, id := range f.ReviewedRuns {
        t.reviewedRuns[id] = true
    }
    return nil
}
```

**Update `saveLocked()` to persist `reviewedRuns`:**

```go
func (t *Tracker) saveLocked() error {
    ids := make([]int64, 0, len(t.reviewedRuns))
    for id := range t.reviewedRuns {
        ids = append(ids, id)
    }

    f := trackerFile{
        Issues:       make([]*Issue, 0, len(t.issues)),
        ReviewedRuns: ids,
    }
    for _, issue := range t.issues {
        f.Issues = append(f.Issues, issue)
    }
    // ... rest unchanged ...
}
```

**Add the four new methods** (`IsRunReviewed`, `MarkRunsReviewed`, `ReviewedRunIDs`, `PruneReviewedRuns`) as shown in section 2.1.

### 4.2 `medivac/github/triage.go`

**Add the new structs** (`RunExtraction`, `BatchTriageResult`) as shown in section 2.2-2.3.

**Add `ExtractRun` function (Phase 1):**

This is essentially the current `TriageRun` but without signature computation and without intra-run dedup. It returns raw `triageResponse` items.

```go
// ExtractRun performs Phase 1: extract structured error records from a single
// run's log. No deduplication or signature computation is done here.
func ExtractRun(
    ctx context.Context,
    run WorkflowRun,
    failedJobs []JobResult,
    annotations []Annotation,
    log string,
    config TriageConfig,
) (*RunExtraction, error) {
    model := config.Model
    if model == "" {
        model = "haiku"
    }
    queryFn := config.Query
    if queryFn == nil {
        queryFn = claude.Query
    }

    prompt := buildExtractionPrompt(run, failedJobs, annotations, log)

    result, err := queryFn(ctx, prompt, claude.WithModel(model))
    if err != nil {
        return nil, fmt.Errorf("extraction query: %w", err)
    }

    items, err := parseTriageResponse(result.Text)
    if err != nil {
        return nil, fmt.Errorf("parse extraction response: %w", err)
    }

    return &RunExtraction{
        Run:        run,
        FailedJobs: failedJobs,
        Items:      items,
        Cost:       result.Usage.CostUSD,
    }, nil
}
```

**Add `buildExtractionPrompt` (Phase 1 prompt):**

This is the current `buildTriagePrompt` with one modification: the DEDUPLICATE instruction is removed (dedup happens in Phase 2), and the "deterministic summaries" instruction is strengthened.

```go
func buildExtractionPrompt(run WorkflowRun, failedJobs []JobResult, annotations []Annotation, log string) string {
    var b strings.Builder

    b.WriteString("You are a CI failure extraction system. Extract structured failure records from the following CI run.\n\n")

    b.WriteString("## Run Context\n\n")
    b.WriteString(fmt.Sprintf("- Workflow: %s\n", run.Name))
    b.WriteString(fmt.Sprintf("- Branch: %s\n", run.HeadBranch))
    b.WriteString(fmt.Sprintf("- SHA: %s\n", run.HeadSHA))
    b.WriteString(fmt.Sprintf("- Run ID: %d\n", run.ID))

    b.WriteString(fmt.Sprintf("- Failed jobs (%d):\n", len(failedJobs)))
    for _, job := range failedJobs {
        b.WriteString(fmt.Sprintf("  - %s\n", job.Name))
    }

    if len(annotations) > 0 {
        b.WriteString("\n## Annotations (from CI system)\n\n")
        for _, ann := range annotations {
            b.WriteString(fmt.Sprintf("- [%s] %s", ann.Level, ann.Message))
            if ann.Path != "" {
                b.WriteString(fmt.Sprintf(" (file: %s", ann.Path))
                if ann.StartLine > 0 {
                    b.WriteString(fmt.Sprintf(":%d", ann.StartLine))
                }
                b.WriteString(")")
            }
            b.WriteString("\n")
        }
    }

    b.WriteString("\n## Combined Log Output (all failed jobs)\n\n```\n")
    b.WriteString(log)
    b.WriteString("\n```\n\n")

    b.WriteString("## Instructions\n\n")
    b.WriteString("Extract each distinct failure from the log. Return a JSON array (no markdown fences, just raw JSON).\n")
    b.WriteString("Each element should have these fields:\n\n")
    b.WriteString("```json\n")
    b.WriteString(`[{
  "category": "one of: lint/go, lint/bazel, lint/ts, lint/python, build, build/docker, test, infra/dependabot, infra/ci, unknown",
  "job": "name of the failed job this error belongs to",
  "file": "path/to/file relative to repository root (empty string if not identifiable)",
  "line": 0,
  "error_code": "compiler/linter error code if present (e.g. TS7006, TS2307, SA1019, empty string otherwise)",
  "summary": "brief one-line description of the failure",
  "details": "relevant error context copied from the log (a few lines)"
}]`)
    b.WriteString("\n```\n\n")
    b.WriteString("Rules:\n")
    b.WriteString("- Return ONLY the JSON array, no other text\n")
    b.WriteString("- Use the most specific category that fits\n")
    b.WriteString("- If the log shows no clear errors, return an empty array []\n")
    b.WriteString("- Keep summary under 100 characters\n")
    b.WriteString("- Keep details under 500 characters\n")
    b.WriteString("- For 'summary': use the EXACT error message from the log when possible (e.g. the compiler error text). Do NOT paraphrase or reword.\n")
    b.WriteString("- For 'file': normalize ALL paths to be relative to the repository root. If a log shows a path like 'src/foo.tsx' inside a Docker build context for a subdirectory (e.g. 'services/typescript/forge-v2'), expand it to the full repo-root-relative path (e.g. 'services/typescript/forge-v2/src/foo.tsx'). Use the workflow name and job context to determine the correct prefix.\n")
    b.WriteString("- ENUMERATE: list EVERY individual error with its own file and line number as a separate entry. Do NOT summarize or group multiple errors into one entry.\n")
    b.WriteString("- For 'job': must be one of the failed job names listed above\n")
    b.WriteString("- For 'details': include the error code if present (e.g. TS7006, TS2307, SA1019). This is critical for downstream grouping.\n")

    return b.String()
}
```

Key difference from `buildTriagePrompt`: no DEDUPLICATE instruction. Phase 1 should enumerate everything. Phase 2 handles dedup.

**Add `DeduplicateBatch` function (Phase 2):**

```go
// DeduplicateBatch performs Phase 2: takes structured extractions from multiple
// runs and produces a deduplicated set of CIFailure records via a single LLM call.
// If only one extraction is provided, skips the LLM call and computes
// signatures directly (no cross-run dedup needed).
func DeduplicateBatch(
    ctx context.Context,
    extractions []*RunExtraction,
    config TriageConfig,
) (*BatchTriageResult, error) {
    if len(extractions) == 0 {
        return &BatchTriageResult{}, nil
    }

    // Accumulate Phase 1 cost.
    var totalCost float64
    for _, ext := range extractions {
        totalCost += ext.Cost
    }

    // Single-run optimization: skip Phase 2 LLM call.
    if len(extractions) == 1 {
        failures := buildFailuresFromExtraction(extractions[0])
        return &BatchTriageResult{
            Failures: failures,
            Cost:     totalCost,
        }, nil
    }

    // Multi-run: make a single LLM call to deduplicate.
    model := config.Model
    if model == "" {
        model = "haiku"
    }
    queryFn := config.Query
    if queryFn == nil {
        queryFn = claude.Query
    }

    prompt := buildBatchDedupPrompt(extractions)

    result, err := queryFn(ctx, prompt, claude.WithModel(model))
    if err != nil {
        return nil, fmt.Errorf("batch dedup query: %w", err)
    }
    totalCost += result.Usage.CostUSD

    items, err := parseTriageResponse(result.Text)
    if err != nil {
        return nil, fmt.Errorf("parse batch dedup response: %w", err)
    }

    // Build valid job name set across all runs.
    jobNames := make(map[string]bool)
    defaultJob := ""
    for _, ext := range extractions {
        for _, j := range ext.FailedJobs {
            jobNames[j.Name] = true
            if defaultJob == "" {
                defaultJob = j.Name
            }
        }
    }

    // Build run ID lookup for validation.
    runsByID := make(map[int64]WorkflowRun)
    for _, ext := range extractions {
        runsByID[ext.Run.ID] = ext.Run
    }
    // Default to first extraction's run.
    defaultRun := extractions[0].Run

    failures := make([]CIFailure, 0, len(items))
    seen := make(map[string]bool)

    for _, item := range items {
        cat := FailureCategory(item.Category)
        if !ValidCategories[cat] {
            cat = CategoryUnknown
        }
        jobName := item.Job
        if !jobNames[jobName] {
            jobName = defaultJob
        }

        // Use the default run's metadata (the LLM dedup merges across runs).
        run := defaultRun

        f := CIFailure{
            RunID:     run.ID,
            RunURL:    run.URL,
            HeadSHA:   run.HeadSHA,
            Branch:    run.HeadBranch,
            JobName:   jobName,
            Category:  cat,
            Summary:   item.Summary,
            Details:   item.Details,
            File:      item.File,
            Line:      item.Line,
            ErrorCode: item.ErrorCode,
            Timestamp: run.CreatedAt,
        }
        f.Signature = ComputeSignature(f.Category, f.File, f.Summary, f.JobName, f.Details)

        if !seen[f.Signature] {
            seen[f.Signature] = true
            failures = append(failures, f)
        }
    }

    return &BatchTriageResult{
        Failures: failures,
        Cost:     totalCost,
    }, nil
}
```

**Add `buildFailuresFromExtraction` helper (single-run shortcut):**

```go
// buildFailuresFromExtraction converts a single RunExtraction into CIFailures
// with signature computation. Used when there is only one run (no cross-run dedup needed).
func buildFailuresFromExtraction(ext *RunExtraction) []CIFailure {
    jobNames := make(map[string]bool, len(ext.FailedJobs))
    for _, j := range ext.FailedJobs {
        jobNames[j.Name] = true
    }
    defaultJob := ""
    if len(ext.FailedJobs) > 0 {
        defaultJob = ext.FailedJobs[0].Name
    }

    failures := make([]CIFailure, 0, len(ext.Items))
    seen := make(map[string]bool)

    for _, item := range ext.Items {
        cat := FailureCategory(item.Category)
        if !ValidCategories[cat] {
            cat = CategoryUnknown
        }
        jobName := item.Job
        if !jobNames[jobName] {
            jobName = defaultJob
        }
        f := CIFailure{
            RunID:     ext.Run.ID,
            RunURL:    ext.Run.URL,
            HeadSHA:   ext.Run.HeadSHA,
            Branch:    ext.Run.HeadBranch,
            JobName:   jobName,
            Category:  cat,
            Summary:   item.Summary,
            Details:   item.Details,
            File:      item.File,
            Line:      item.Line,
            ErrorCode: item.ErrorCode,
            Timestamp: ext.Run.CreatedAt,
        }
        f.Signature = ComputeSignature(f.Category, f.File, f.Summary, f.JobName, f.Details)

        if !seen[f.Signature] {
            seen[f.Signature] = true
            failures = append(failures, f)
        }
    }
    return failures
}
```

**Add `buildBatchDedupPrompt` (Phase 2 prompt):**

```go
func buildBatchDedupPrompt(extractions []*RunExtraction) string {
    var b strings.Builder

    b.WriteString("You are a CI failure deduplication system. Below are structured error records extracted from ")
    b.WriteString(fmt.Sprintf("%d different CI runs. ", len(extractions)))
    b.WriteString("Your task is to deduplicate: if the same error appears across multiple runs, include it ONLY ONCE.\n\n")

    for i, ext := range extractions {
        b.WriteString(fmt.Sprintf("## Run %d: %s (ID: %d, SHA: %s)\n\n", i+1, ext.Run.Name, ext.Run.ID, ext.Run.HeadSHA))
        b.WriteString("Errors:\n```json\n")
        data, _ := json.Marshal(ext.Items)
        b.WriteString(string(data))
        b.WriteString("\n```\n\n")
    }

    b.WriteString("## Instructions\n\n")
    b.WriteString("Merge the above error lists into a single deduplicated JSON array.\n\n")
    b.WriteString("Deduplication rules:\n")
    b.WriteString("- Two errors are the SAME if they have the same file, the same error_code (when present), and the same essential meaning in their summary\n")
    b.WriteString("- When merging duplicates, keep the version with the most complete information (longer details, more specific file path)\n")
    b.WriteString("- For summaries: use the EXACT compiler/linter error message. Do NOT paraphrase or reword\n")
    b.WriteString("- For file paths: choose the most complete path (prefer 'services/typescript/forge-v2/src/foo.tsx' over 'src/foo.tsx')\n")
    b.WriteString("- Preserve ALL unique errors. Only merge errors that are truly the same failure appearing in different runs\n\n")
    b.WriteString("Return the deduplicated JSON array with the same schema as the input (no markdown fences, just raw JSON):\n")
    b.WriteString("```json\n")
    b.WriteString(`[{
  "category": "...",
  "job": "...",
  "file": "...",
  "line": 0,
  "error_code": "...",
  "summary": "...",
  "details": "..."
}]`)
    b.WriteString("\n```\n\n")
    b.WriteString("Rules:\n")
    b.WriteString("- Return ONLY the JSON array, no other text\n")
    b.WriteString("- Keep every field from the original records\n")
    b.WriteString("- Do NOT add new errors that were not in the input\n")
    b.WriteString("- Do NOT modify summaries (use them exactly as they appear in the input)\n")
    b.WriteString("- When two runs have the same error with slightly different summaries, pick the one that is closest to the raw compiler/linter output\n")

    return b.String()
}
```

**Keep `TriageRun` unchanged** for backward compatibility (it is still useful for one-off debugging or if callers need per-run triage). But the engine will no longer call it.

### 4.3 `medivac/engine/engine.go`

**Rewrite `Scan()` to use the two-phase flow:**

```go
func (e *Engine) Scan(ctx context.Context) (*ScanResult, error) {
    e.logger.Info("scanning CI failures",
        "branch", e.config.Branch,
        "limit", e.config.RunLimit,
    )

    // Fetch failed runs.
    runs, err := e.ghClient.ListFailedRuns(ctx, e.config.Branch, e.config.RunLimit)
    if err != nil {
        return nil, fmt.Errorf("list failed runs: %w", err)
    }

    e.logger.Info("found failed runs", "count", len(runs))

    // Build active run ID set (for pruning stale reviewed markers).
    activeRunIDs := make(map[int64]bool, len(runs))
    for _, r := range runs {
        activeRunIDs[r.ID] = true
    }

    // Filter to unreviewed runs.
    var unreviewedRuns []github.WorkflowRun
    for _, r := range runs {
        if !e.tracker.IsRunReviewed(r.ID) {
            unreviewedRuns = append(unreviewedRuns, r)
        }
    }

    e.logger.Info("unreviewed runs", "count", len(unreviewedRuns), "reviewed", len(runs)-len(unreviewedRuns))

    if len(unreviewedRuns) == 0 {
        e.logger.Info("all runs already reviewed, nothing to triage")
        // Still reconcile (to detect resolved issues) but with empty failures.
        reconciled := e.tracker.Reconcile(nil)
        e.tracker.PruneReviewedRuns(activeRunIDs)
        if err := e.tracker.Save(); err != nil {
            return nil, fmt.Errorf("save tracker: %w", err)
        }
        return &ScanResult{
            Runs:          runs,
            Reconciled:    reconciled,
            TotalIssues:   len(e.tracker.All()),
            ActionableLen: len(e.tracker.GetActionable()),
        }, nil
    }

    triageCfg := github.TriageConfig{
        Model: e.config.TriageModel,
        Query: e.config.TriageQuery,
    }

    // PHASE 1: Extract errors from each unreviewed run.
    var (
        extractions     []*github.RunExtraction
        totalTriageCost float64
    )

    for i := range unreviewedRuns {
        run := unreviewedRuns[i]

        // Check budget.
        if totalTriageCost >= e.config.TriageBudget {
            e.logger.Warn("triage budget exhausted",
                "spent", fmt.Sprintf("$%.4f", totalTriageCost),
                "budget", fmt.Sprintf("$%.4f", e.config.TriageBudget),
            )
            break
        }

        // Fetch annotations (best-effort).
        annotations, _ := e.ghClient.GetAnnotations(ctx, run.ID)

        // Fetch raw log.
        rawLog, err := e.ghClient.GetJobLog(ctx, run.ID)
        if err != nil {
            e.logger.Warn("failed to get job log", "runID", run.ID, "error", err)
            continue
        }
        cleanedLog := github.CleanLog(rawLog)

        // Get failed jobs.
        jobs, err := e.ghClient.GetJobsForRun(ctx, run.ID)
        if err != nil {
            e.logger.Warn("failed to get jobs", "runID", run.ID, "error", err)
            continue
        }

        var failedJobs []github.JobResult
        for _, job := range jobs {
            if job.Conclusion == "failure" {
                failedJobs = append(failedJobs, job)
            }
        }
        if len(failedJobs) == 0 {
            continue
        }

        e.logger.Debug("extracting errors from run",
            "runID", run.ID,
            "workflow", run.Name,
            "failedJobs", len(failedJobs),
        )

        extraction, extractErr := github.ExtractRun(ctx, run, failedJobs, annotations, cleanedLog, triageCfg)
        if extractErr != nil {
            e.logger.Warn("extraction failed", "runID", run.ID, "error", extractErr)
            continue
        }
        totalTriageCost += extraction.Cost

        e.logger.Debug("extraction results",
            "runID", run.ID,
            "items", len(extraction.Items),
            "cost", fmt.Sprintf("$%.4f", extraction.Cost),
        )

        extractions = append(extractions, extraction)
    }

    // PHASE 2: Batch deduplicate across all runs.
    batchResult, err := github.DeduplicateBatch(ctx, extractions, triageCfg)
    if err != nil {
        return nil, fmt.Errorf("batch dedup: %w", err)
    }
    totalTriageCost += batchResult.Cost - sumExtractionCosts(extractions) // batchResult.Cost includes Phase 1 costs; adjust

    allFailures := batchResult.Failures

    e.logger.Info("triaged CI failures",
        "count", len(allFailures),
        "triageCost", fmt.Sprintf("$%.4f", batchResult.Cost),
    )

    // Mark successfully extracted runs as reviewed.
    reviewedIDs := make([]int64, 0, len(extractions))
    for _, ext := range extractions {
        reviewedIDs = append(reviewedIDs, ext.Run.ID)
    }
    e.tracker.MarkRunsReviewed(reviewedIDs)
    e.tracker.PruneReviewedRuns(activeRunIDs)

    // Reconcile with known issues.
    reconciled := e.tracker.Reconcile(allFailures)

    if err := e.tracker.Save(); err != nil {
        return nil, fmt.Errorf("save tracker: %w", err)
    }

    actionable := e.tracker.GetActionable()

    return &ScanResult{
        Runs:          runs,
        Failures:      allFailures,
        Reconciled:    reconciled,
        TotalIssues:   len(e.tracker.All()),
        ActionableLen: len(actionable),
        TriageCost:    batchResult.Cost,
    }, nil
}
```

Note: The cost accounting in `DeduplicateBatch` already includes Phase 1 costs, so `ScanResult.TriageCost` is set to `batchResult.Cost` directly. The `totalTriageCost` variable in the loop is used only for budget checking during Phase 1.

**Add helper:**

```go
func sumExtractionCosts(extractions []*github.RunExtraction) float64 {
    var total float64
    for _, ext := range extractions {
        total += ext.Cost
    }
    return total
}
```

### 4.4 No changes needed

- `medivac/github/actions.go` -- no changes (data fetching is unchanged)
- `medivac/github/parser.go` -- no changes (signature computation is unchanged)
- `medivac/engine/grouping.go` -- no changes (grouping is downstream of triage)

---

## 5. Triage Prompt Changes Summary

### Phase 1 prompt (extraction): `buildExtractionPrompt`

Differences from current `buildTriagePrompt`:
1. **System role changed:** "CI failure extraction system" (not "triage system") to prime the LLM for raw extraction without editorial dedup.
2. **Adds Run ID** to context (useful for Phase 2 provenance).
3. **DEDUPLICATE instruction removed.** Phase 1 should extract all errors faithfully, even if two jobs in the same run report the same error. Dedup happens in Phase 2.
4. **Wording "deterministic summaries enable deduplication across runs" removed.** The motivation for exact summaries is still present ("use the EXACT error message") but the cross-run rationale is no longer in Phase 1.

### Phase 2 prompt (dedup): `buildBatchDedupPrompt`

This is entirely new. Key design decisions:
1. **Input is structured JSON, not raw logs.** The LLM sees clean `triageResponse` records, not noisy log text. This makes dedup much more reliable.
2. **Explicit same-error criteria:** "same file + same error_code + same essential meaning." This gives the LLM clear rules for when to merge.
3. **Preserve-don't-modify instruction:** "Do NOT modify summaries" prevents the LLM from paraphrasing during dedup, which would cause downstream signature mismatches.
4. **Pick-best-path instruction:** "choose the most complete path" resolves the `go-common` vs `services/go/common` problem documented in D3.
5. **No-invention instruction:** "Do NOT add new errors" prevents hallucination.

---

## 6. Edge Cases

### 6.1 First scan (no marker, empty tracker)

`reviewedRuns` map is empty. All fetched runs pass the `IsRunReviewed` filter. All runs are triaged. Behavior is identical to current system.

### 6.2 All runs already reviewed

`unreviewedRuns` is empty. Scan returns early with no new failures but still calls `Reconcile(nil)` to detect resolved issues (StatusFixMerged -> StatusVerified transitions). This is correct: if all runs are reviewed and still failing, the existing issues are already tracked. If a fix was merged, the reconcile-with-empty-failures path will mark it verified.

### 6.3 Stale marker (old run IDs no longer in GitHub results)

`PruneReviewedRuns(activeRunIDs)` removes reviewed markers for runs that no longer appear in `ListFailedRuns`. This happens naturally as GitHub pages old runs out of the recent results. The pruning prevents `reviewedRuns` from growing unboundedly.

### 6.4 Context window overflow in Phase 2

With 10 runs x 20 errors x 200 bytes per error = 40KB of structured data. Even with 50 errors per run (the worst case observed: 55 TS errors from Docker Publish), 10 runs x 55 errors x 200 bytes = 110KB. Haiku's context window is 200K tokens (~800KB of text), so this fits comfortably.

If future usage produces truly enormous batches (e.g., 20 runs with 100 errors each = 400KB), add a safety check:

```go
const maxBatchPromptSize = 150 * 1024 // 150KB

func DeduplicateBatch(...) (*BatchTriageResult, error) {
    // ... build prompt ...
    if len(prompt) > maxBatchPromptSize {
        // Fall back: compute signatures directly without LLM dedup.
        // This loses cross-run dedup but is safe.
        var failures []CIFailure
        for _, ext := range extractions {
            failures = append(failures, buildFailuresFromExtraction(ext)...)
        }
        return &BatchTriageResult{Failures: dedupBySignature(failures), Cost: totalCost}, nil
    }
    // ... normal LLM call ...
}
```

This fallback degrades gracefully to the current behavior (signature-based dedup only).

### 6.5 Partial extraction failure

If Phase 1 fails for some runs (e.g., log fetch error, LLM timeout), those runs are skipped and NOT marked as reviewed. The next scan will retry them. Runs that were successfully extracted are marked as reviewed even if Phase 2 fails, because the extraction data can be regenerated.

Actually, correction: if Phase 2 fails, we should NOT mark any runs as reviewed, because the failures were not reconciled. The `MarkRunsReviewed` call happens AFTER successful `DeduplicateBatch`. If `DeduplicateBatch` returns an error, `Scan` returns that error before reaching `MarkRunsReviewed`.

### 6.6 Budget exhaustion during Phase 1

The budget check already breaks out of the Phase 1 loop. Runs that were not extracted are not included in `extractions`, so Phase 2 only deduplicates across the runs that were successfully processed. The unprocessed runs are not marked as reviewed and will be picked up in the next scan.

### 6.7 Single unreviewed run

Phase 2 LLM call is skipped. Signatures are computed directly from Phase 1 output. This preserves current cost for the common case where only one new run appeared since the last scan.

### 6.8 Same run appears as both reviewed and still failing

This is the normal steady-state. A run was triaged in a previous scan and its issues are tracked. On the next scan, the run still appears in `ListFailedRuns` (because it is still a failed run on GitHub), but `IsRunReviewed` returns true, so it is skipped. The tracked issues remain in the tracker. This is correct: we do not need to re-triage a run whose errors are already captured.

### 6.9 Run disappears from GitHub (re-run succeeds, or run is too old)

When a run that was previously tracked disappears from `ListFailedRuns`, `PruneReviewedRuns` removes its marker. If the run reappears later (unlikely), it will be treated as new and re-triaged. The issues from the original triage remain in the tracker (they are keyed by signature, not run ID).

---

## 7. Tests Needed

### 7.1 `medivac/issue/tracker_test.go` -- new tests

| Test | Description |
|------|-------------|
| `TestReviewedRuns_Empty` | New tracker has no reviewed runs. `IsRunReviewed(any)` returns false. |
| `TestReviewedRuns_MarkAndCheck` | `MarkRunsReviewed([100, 200])`, then `IsRunReviewed(100)` = true, `IsRunReviewed(300)` = false. |
| `TestReviewedRuns_Persistence` | Mark runs, save, reload. Reviewed runs survive the round-trip. |
| `TestReviewedRuns_Prune` | Mark runs [100, 200, 300]. Prune with active = {200, 300}. Verify 100 is removed, 200/300 remain. |
| `TestReviewedRuns_BackwardCompat` | Load a tracker file that has no `reviewed_runs` field. Verify it loads without error and `reviewedRuns` is empty. |

### 7.2 `medivac/github/triage_test.go` -- new tests

| Test | Description |
|------|-------------|
| `TestExtractRun_Basic` | Same as `TestTriageRun_BasicJSON` but calls `ExtractRun`. Verifies `RunExtraction` has correct items and cost. |
| `TestDeduplicateBatch_SingleRun` | One extraction. Verifies no Phase 2 LLM call, signatures computed directly. |
| `TestDeduplicateBatch_MultiRun_Dedup` | Two extractions with overlapping errors. Mock LLM returns deduplicated list. Verify fewer failures than sum of inputs. |
| `TestDeduplicateBatch_Empty` | Zero extractions. Returns empty result. |
| `TestBuildBatchDedupPrompt` | Verify prompt contains all run context, JSON data, and dedup instructions. |
| `TestBuildExtractionPrompt` | Verify extraction prompt contains run context, log, and does NOT contain "DEDUPLICATE". |

### 7.3 `medivac/engine/engine_test.go` -- update existing tests

| Test | Change needed |
|------|---------------|
| `TestScan` | The mock `TriageQuery` now receives an extraction prompt (Phase 1) and possibly a dedup prompt (Phase 2). With one run, only Phase 1 fires. Update mock to handle `ExtractRun` call pattern. Verify run is marked as reviewed after scan. |
| `TestScan_NoFailures` | No change needed (no unreviewed runs = no extraction calls). |
| `TestScan_MultipleFailures` | Same as `TestScan` -- single run, three failures. Verify all three pass through. |
| New: `TestScan_SkipsReviewedRuns` | Pre-populate tracker with reviewed run ID 100. Mock returns run 100. Verify no triage calls are made. |
| New: `TestScan_MultipleRuns_BatchDedup` | Mock returns 2 runs. Mock query returns overlapping errors for each. Verify Phase 2 fires and produces deduplicated results. Verify both runs marked as reviewed. |
| New: `TestScan_SecondScan_OnlyNewRuns` | First scan: 2 runs, both triaged. Second scan: same 2 runs + 1 new run. Verify only 1 extraction call (for the new run). |

### 7.4 Test approach for Phase 2 LLM mock

The `mockTriageQuery` function currently returns a single fixed response regardless of prompt content. For tests involving both Phase 1 and Phase 2 calls, we need a prompt-aware mock:

```go
// promptAwareQuery returns different responses based on prompt content.
func promptAwareQuery(extractionResp string, dedupResp string, cost float64) github.QueryFn {
    return func(_ context.Context, prompt string, _ ...claude.SessionOption) (*claude.QueryResult, error) {
        text := extractionResp
        if strings.Contains(prompt, "deduplication system") {
            text = dedupResp
        }
        return &claude.QueryResult{
            TurnResult: claude.TurnResult{
                Text:    text,
                Success: true,
                Usage:   claude.TurnUsage{CostUSD: cost},
            },
        }, nil
    }
}
```

---

## 8. Migration

### What happens to existing tracker files without `reviewed_runs`

The JSON `omitempty` tag on `ReviewedRuns` means:
- **Old file -> new code:** `json.Unmarshal` sees no `reviewed_runs` field. The slice is nil. The `load()` method iterates over a nil slice (no-op). `reviewedRuns` map stays empty. **All existing runs will be treated as unreviewed on the first scan.** This is correct: the first scan with the new code re-triages all fetched runs, which is the same behavior as before. After that scan, the runs are marked as reviewed and subsequent scans benefit from the new behavior.
- **New file -> old code:** If someone rolls back, the old code's `trackerFile` struct does not have `ReviewedRuns`. `json.Unmarshal` ignores unknown fields by default in Go. The tracker file loads fine. The `reviewed_runs` field is lost (not re-persisted), which means the next scan with old code triages all runs. This is safe.

**No migration script needed.** The change is fully backward- and forward-compatible.

### Cost impact of first scan after migration

The first scan with the new code re-triages all N runs that `ListFailedRuns` returns. This is the same number of LLM calls as the old code. If N > 1, it also makes one Phase 2 dedup call. The marginal cost is one Haiku call on structured data (~$0.002). This is negligible and happens only once.

---

## 9. Implementation Order

1. **Tracker changes** (`medivac/issue/tracker.go`) -- add `reviewedRuns` field, methods, persistence. Write tests.
2. **Triage changes** (`medivac/github/triage.go`) -- add `ExtractRun`, `DeduplicateBatch`, prompt builders, helper functions. Write tests.
3. **Engine changes** (`medivac/engine/engine.go`) -- rewrite `Scan()` to use the new flow. Update existing tests, add new tests.
4. **Lint and full test pass** -- `scripts/lint.sh && bazel test //medivac/... --test_timeout=60`

Steps 1 and 2 are independent and can be done in parallel. Step 3 depends on both.

---

## 10. Summary of Benefits

| Metric | Current (per-run triage) | New (batch triage) |
|--------|------------------------|--------------------|
| LLM calls per scan (5 runs) | 5 | 5 extraction + 1 dedup = 6 |
| LLM calls per scan (1 new run) | 1 | 1 extraction (dedup skipped) |
| Cross-run dedup | Signature-only (fragile) | LLM-assisted (sees all errors) |
| Dedup leak rate (from D3) | 4/63 = 6.3% | Expected ~0% (LLM merges variants) |
| Cost per scan (5 runs) | ~$0.025 | ~$0.027 (+8%) |
| Repeated scan cost (0 new runs) | ~$0.025 (re-triages all) | $0 (all reviewed, skip) |
| File path normalization | Per-run (LLM may vary) | Cross-run (LLM picks best) |
| Summary consistency | Per-run (LLM may paraphrase differently) | Cross-run (LLM picks one) |

The biggest win is not the 8% cost increase on first scan, but the **$0 cost on subsequent scans when no new runs appear.** Currently, every `medivac scan` invocation re-triages all N runs even if nothing changed. With the reviewed-run marker, repeat scans are free.
