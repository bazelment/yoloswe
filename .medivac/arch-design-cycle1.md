# Architecture Design - Cycle 1 Fixes

**Date:** 2026-02-10
**Branch:** feature/fixer
**Based on:** PM Assessment Cycle 1

---

## Implementation Order

The changes below are ordered by dependency. Each priority is independent unless noted.

1. **Priority 5: Remove dead `--repo-name` flag** (no dependencies, 2 minutes)
2. **Priority 2: Fix deduplication issues** (no dependencies, foundation for everything else)
3. **Priority 4: Improve dry-run output** (no dependencies)
4. **Priority 3: Add `--skip-scan` flag to fix** (depends on Priority 2 conceptually, not codewise)
5. **Priority 1: Build-system-aware fix prompts** (independent, largest change)

---

## Priority 5: Remove Dead `--repo-name` Flag

### Files to modify

- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/cmd/medivac/main.go`

### Change

Remove the `repoName` variable declaration and the `--repo-name` flag registration.

```go
// BEFORE (main.go lines 13-18):
var (
	repoRoot    string
	repoName    string
	trackerPath string
	sessionDir  string
	dryRun      bool
	verbose     bool
)

// AFTER:
var (
	repoRoot    string
	trackerPath string
	sessionDir  string
	dryRun      bool
	verbose     bool
)
```

```go
// BEFORE (main.go line 31, inside init()):
rootCmd.PersistentFlags().StringVar(&repoName, "repo-name", "", "Repository name for GitHub API (e.g. owner/repo)")

// AFTER: delete this line entirely
```

### Tests

No test changes needed. No existing code references `repoName`.

---

## Priority 2: Fix Deduplication Issues

Three sub-issues:

### 2a. Normalize signatures better

**Files to modify:**
- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/github/parser.go` -- `normalizeMessage()` and `ComputeSignature()`
- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/github/parser_test.go` -- add tests

**What to change in `normalizeMessage()`:**

Add three new normalizations: strip trailing punctuation, collapse whitespace, and lowercase.

```go
// BEFORE (parser.go lines 109-114):
func normalizeMessage(msg string) string {
	msg = normalizeLineCol.ReplaceAllString(msg, "")
	msg = normalizeHex.ReplaceAllString(msg, "")
	msg = normalizeTS.ReplaceAllString(msg, "")
	return strings.TrimSpace(msg)
}

// AFTER:
var normalizeWhitespace = regexp.MustCompile(`\s+`)

func normalizeMessage(msg string) string {
	msg = normalizeLineCol.ReplaceAllString(msg, "")
	msg = normalizeHex.ReplaceAllString(msg, "")
	msg = normalizeTS.ReplaceAllString(msg, "")
	msg = strings.TrimRight(msg, ".!,;:?")       // strip trailing punctuation
	msg = normalizeWhitespace.ReplaceAllString(msg, " ") // collapse whitespace
	msg = strings.ToLower(msg)                     // case-insensitive matching
	return strings.TrimSpace(msg)
}
```

Add a new `normalizeWhitespace` regex alongside the existing regex vars at the top of the file (line ~63 area).

**What to change in `ComputeSignature()`:**

Remove category from the signature format. The same error at the same file should match regardless of whether it was caught by a lint step or a build step.

Also add path canonicalization to strip known build-context prefixes.

```go
// BEFORE (parser.go lines 93-106):
func ComputeSignature(category FailureCategory, file, summary, jobName, details string) string {
	msg := summary
	if msg == "" {
		msg = details
	}
	if msg == "" {
		msg = jobName
	}
	normalized := normalizeMessage(msg)
	h := sha256.Sum256([]byte(normalized))
	shortHash := fmt.Sprintf("%x", h[:8])
	return fmt.Sprintf("%s:%s:%s", category, shortHash, file)
}

// AFTER:
func ComputeSignature(category FailureCategory, file, summary, jobName, details string) string {
	msg := summary
	if msg == "" {
		msg = details
	}
	if msg == "" {
		// Only include job name when there's no other content to hash.
		msg = jobName
	}
	normalized := normalizeMessage(msg)
	h := sha256.Sum256([]byte(normalized))
	shortHash := fmt.Sprintf("%x", h[:8])
	canonFile := canonicalizePath(file)
	return fmt.Sprintf("%s:%s", shortHash, canonFile)
}

// canonicalizePath strips known build-context prefixes from file paths
// so the same file produces the same signature regardless of the build
// context (e.g. Docker build vs lint step).
func canonicalizePath(path string) string {
	// Strip leading "services/<type>/<name>/" prefix that appears in Docker builds.
	// This regex matches patterns like "services/typescript/forge-v2/src/..."
	// and reduces them to just "src/...".
	path = stripBuildContextPrefix(path)
	// Normalize to forward slashes and remove leading ./
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimPrefix(path, "./")
	return path
}

var buildContextPrefixRe = regexp.MustCompile(`^services/[^/]+/[^/]+/`)

func stripBuildContextPrefix(path string) string {
	// If path matches "services/<type>/<project>/src/...", strip the prefix.
	// Only strip if the remainder looks like a source path (starts with src/, lib/, etc.)
	loc := buildContextPrefixRe.FindStringIndex(path)
	if loc == nil {
		return path
	}
	remainder := path[loc[1]:]
	// Only strip if the remainder starts with a common source directory.
	// This prevents stripping legitimate paths like "services/api/handler.go".
	srcPrefixes := []string{"src/", "lib/", "test/", "tests/", "pkg/", "cmd/", "internal/"}
	for _, p := range srcPrefixes {
		if strings.HasPrefix(remainder, p) {
			return remainder
		}
	}
	return path
}
```

Add `buildContextPrefixRe` to the regex var block.

**Key design decision:** We remove `category` from the signature. This is the highest-impact dedup change. The same TS7006 error caught by `lint/ts` and `build/docker` will now match. The category is still stored on the Issue -- it is just not part of the dedup key. When an existing issue is updated with a new failure, the category is NOT overwritten (first writer wins), preserving the most specific categorization.

**Impact on existing data:** Existing `issues.json` files will have old-format signatures like `lint/go:hash:file`. New signatures will be `hash:file`. Since signatures are just strings used as map keys, old issues will remain in the tracker and simply not match new failures (treated as stale). On next reconcile, new-format issues will be created for current failures, and old-format issues will age out naturally. This is acceptable for a tool in early development. If needed, a one-time migration could be added, but it is NOT in scope for this cycle.

**Tests to add in `parser_test.go`:**

```go
func TestNormalizeMessage_TrailingPunctuation(t *testing.T) {
	a := normalizeMessage("Parameter 'file' implicitly has an 'any' type.")
	b := normalizeMessage("Parameter 'file' implicitly has an 'any' type")
	if a != b {
		t.Errorf("trailing period should be stripped: %q != %q", a, b)
	}
}

func TestNormalizeMessage_Whitespace(t *testing.T) {
	a := normalizeMessage("error:  too   many spaces")
	b := normalizeMessage("error: too many spaces")
	if a != b {
		t.Errorf("whitespace should be collapsed: %q != %q", a, b)
	}
}

func TestNormalizeMessage_CaseInsensitive(t *testing.T) {
	a := normalizeMessage("Cannot find module '@sycamore-labs/ui'")
	b := normalizeMessage("cannot find module '@sycamore-labs/ui'")
	if a != b {
		t.Errorf("case should be ignored: %q != %q", a, b)
	}
}

func TestComputeSignature_IgnoresCategory(t *testing.T) {
	sig1 := ComputeSignature(CategoryLintTS, "src/app.tsx", "Parameter 'e' implicitly has an 'any' type", "lint", "")
	sig2 := ComputeSignature(CategoryBuildDocker, "src/app.tsx", "Parameter 'e' implicitly has an 'any' type", "build", "")
	if sig1 != sig2 {
		t.Errorf("signatures should be equal regardless of category: %s != %s", sig1, sig2)
	}
}

func TestCanonicalizePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"src/components/app.tsx", "src/components/app.tsx"},
		{"services/typescript/forge-v2/src/components/app.tsx", "src/components/app.tsx"},
		{"./src/app.tsx", "src/app.tsx"},
		{"services/api/handler.go", "services/api/handler.go"}, // no src/ after prefix, keep as-is
		{"", ""},
	}
	for _, tt := range tests {
		got := canonicalizePath(tt.input)
		if got != tt.want {
			t.Errorf("canonicalizePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
```

**Update existing tests:** The `TestComputeSignature_Stable` test asserts the signature format contains the category prefix. This test must be updated to expect the new format (no category prefix). Same for `TestComputeSignature_EmptySummaryFallback` which checks for `"unknown:"` prefix.

```go
// BEFORE:
func TestComputeSignature_Stable(t *testing.T) {
	sig1 := ComputeSignature(CategoryLintGo, "main.go", "unused variable x", "lint", "")
	sig2 := ComputeSignature(CategoryLintGo, "main.go", "unused variable x", "lint", "")
	if sig1 != sig2 {
		t.Errorf("signatures should be equal: %s != %s", sig1, sig2)
	}
	sig3 := ComputeSignature(CategoryLintGo, "main.go", "different error", "lint", "")
	if sig1 == sig3 {
		t.Errorf("signatures should differ for different messages")
	}
}

// AFTER: same logic, no change needed -- still tests stability.

// BEFORE:
func TestComputeSignature_EmptySummaryFallback(t *testing.T) {
	sig1 := ComputeSignature(CategoryUnknown, "", "", "job", "some error detail")
	if sig1 == "" {
		t.Error("expected non-empty signature")
	}
	if !strings.HasPrefix(sig1, "unknown:") {
		t.Errorf("expected 'unknown:' prefix, got %s", sig1)
	}
	...
}

// AFTER: remove the prefix check since category is no longer in the signature
func TestComputeSignature_EmptySummaryFallback(t *testing.T) {
	sig1 := ComputeSignature(CategoryUnknown, "", "", "job", "some error detail")
	if sig1 == "" {
		t.Error("expected non-empty signature")
	}
	// Signature should be "hash:" (no file)
	if !strings.Contains(sig1, ":") {
		t.Errorf("expected colon separator in signature, got %s", sig1)
	}
	sig2 := ComputeSignature(CategoryUnknown, "", "", "my-job", "")
	if sig2 == "" {
		t.Error("expected non-empty signature")
	}
}
```

### 2b. Fix the Reconcile() duplicate in Updated list

**File to modify:**
- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/issue/tracker.go` -- `Reconcile()`

**What to change:**

The bug: when two failures in the same scan have the same signature, the existing issue is appended to `result.Updated` twice. Add a `seenUpdated` set to deduplicate.

```go
// BEFORE (tracker.go Reconcile(), lines 100-164):
func (t *Tracker) Reconcile(failures []github.CIFailure) *ReconcileResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := &ReconcileResult{}
	seenSignatures := make(map[string]bool)

	for i := range failures {
		f := &failures[i]
		seenSignatures[f.Signature] = true

		existing, ok := t.issues[f.Signature]
		if !ok {
			// New issue
			issue := &Issue{ ... }
			t.issues[f.Signature] = issue
			result.New = append(result.New, issue)
			continue
		}

		// Existing issue -- update
		existing.LastSeen = f.Timestamp
		existing.SeenCount++
		if f.Details != "" {
			existing.Details = f.Details
		}

		switch existing.Status {
		case StatusFixMerged, StatusVerified:
			existing.Status = StatusRecurred
			existing.ResolvedAt = nil
			result.Updated = append(result.Updated, existing)
		default:
			result.Updated = append(result.Updated, existing)
		}
	}
	// ... resolved check ...
}

// AFTER:
func (t *Tracker) Reconcile(failures []github.CIFailure) *ReconcileResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := &ReconcileResult{}
	seenSignatures := make(map[string]bool)
	updatedSeen := make(map[string]bool) // <-- NEW: prevent duplicate entries in Updated

	for i := range failures {
		f := &failures[i]
		seenSignatures[f.Signature] = true

		existing, ok := t.issues[f.Signature]
		if !ok {
			// New issue -- same as before
			issue := &Issue{
				ID:        generateID(f.Signature),
				Signature: f.Signature,
				Category:  f.Category,
				Summary:   f.Summary,
				Details:   f.Details,
				File:      f.File,
				Line:      f.Line,
				Status:    StatusNew,
				FirstSeen: f.Timestamp,
				LastSeen:  f.Timestamp,
				SeenCount: 1,
			}
			t.issues[f.Signature] = issue
			result.New = append(result.New, issue)
			continue
		}

		// Existing issue -- update counters regardless
		existing.LastSeen = f.Timestamp
		existing.SeenCount++
		if f.Details != "" {
			existing.Details = f.Details
		}

		// Only add to Updated list once per reconcile
		if updatedSeen[f.Signature] {
			continue
		}
		updatedSeen[f.Signature] = true

		switch existing.Status {
		case StatusFixMerged, StatusVerified:
			existing.Status = StatusRecurred
			existing.ResolvedAt = nil
			result.Updated = append(result.Updated, existing)
		default:
			result.Updated = append(result.Updated, existing)
		}
	}

	// Check for resolved issues (unchanged)
	for sig, issue := range t.issues {
		if seenSignatures[sig] {
			continue
		}
		if issue.Status == StatusFixMerged {
			issue.Status = StatusVerified
			now := time.Now()
			issue.ResolvedAt = &now
			result.Resolved = append(result.Resolved, issue)
		}
	}

	return result
}
```

**Tests to add in `tracker_test.go`:**

```go
func TestReconcile_NoDuplicateInUpdated(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	sig := "abc123:main.go"
	now := time.Now()

	// First reconcile creates the issue
	tracker.Reconcile([]github.CIFailure{
		{Signature: sig, Category: github.CategoryLintGo, Summary: "unused variable", Timestamp: now},
	})

	// Second reconcile with two failures that have the same signature
	later := now.Add(time.Hour)
	result := tracker.Reconcile([]github.CIFailure{
		{Signature: sig, Category: github.CategoryLintGo, Summary: "unused variable", Timestamp: later},
		{Signature: sig, Category: github.CategoryBuildDocker, Summary: "unused variable", Timestamp: later},
	})

	if len(result.Updated) != 1 {
		t.Fatalf("expected 1 updated (no duplicates), got %d", len(result.Updated))
	}

	issue := tracker.Get(sig)
	// SeenCount should still increment for both failures: 1 (first reconcile) + 2 = 3
	if issue.SeenCount != 3 {
		t.Errorf("expected seen count 3, got %d", issue.SeenCount)
	}
}
```

### 2c. Fix first_seen/last_seen timestamp inversion

**File to modify:**
- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/issue/tracker.go` -- `Reconcile()`, the new-issue and update-existing blocks

**What to change:**

For new issues, use `min(now, f.Timestamp)` for FirstSeen. For existing issues, use `min/max` for FirstSeen/LastSeen.

The root cause: `f.Timestamp` comes from the GitHub run's `CreatedAt`, which can be earlier than `time.Now()` (when the scan happens). For new issues, `FirstSeen` is set to `f.Timestamp` which could be older than "now", and `LastSeen` is also set to `f.Timestamp`. So both fields come from the same source and should be consistent for new issues. The real inversion happens when an existing issue is updated: `LastSeen` is set to `f.Timestamp` (the run's creation time), but `FirstSeen` was set from a different run's `f.Timestamp` that happened to have a later `CreatedAt`. This means multiple runs with different `CreatedAt` values can create inversions.

Fix: always enforce `FirstSeen <= LastSeen`.

```go
// In the "Existing issue -- update" block, CHANGE:
existing.LastSeen = f.Timestamp

// TO:
if f.Timestamp.Before(existing.FirstSeen) {
	existing.FirstSeen = f.Timestamp
}
if f.Timestamp.After(existing.LastSeen) {
	existing.LastSeen = f.Timestamp
}
```

This ensures `FirstSeen` only moves backward in time and `LastSeen` only moves forward.

**Tests to add in `tracker_test.go`:**

```go
func TestReconcile_TimestampOrdering(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	sig := "abc123:main.go"

	// First failure has a "later" timestamp
	later := time.Date(2026, 2, 11, 3, 0, 0, 0, time.UTC)
	tracker.Reconcile([]github.CIFailure{
		{Signature: sig, Category: github.CategoryLintGo, Summary: "err", Timestamp: later},
	})

	// Second failure has an "earlier" timestamp (from an older run scanned later)
	earlier := time.Date(2026, 2, 11, 0, 0, 0, 0, time.UTC)
	tracker.Reconcile([]github.CIFailure{
		{Signature: sig, Category: github.CategoryLintGo, Summary: "err", Timestamp: earlier},
	})

	issue := tracker.Get(sig)
	if !issue.FirstSeen.Equal(earlier) {
		t.Errorf("FirstSeen should be the earlier timestamp, got %v", issue.FirstSeen)
	}
	if !issue.LastSeen.Equal(later) {
		t.Errorf("LastSeen should be the later timestamp, got %v", issue.LastSeen)
	}
	// Invariant: FirstSeen <= LastSeen
	if issue.FirstSeen.After(issue.LastSeen) {
		t.Errorf("FirstSeen (%v) should not be after LastSeen (%v)", issue.FirstSeen, issue.LastSeen)
	}
}
```

---

## Priority 4: Improve Dry-Run Output

### Files to modify

- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/cmd/medivac/fix.go` -- `printFixResult()`

### What to change

Show issue details in dry-run output instead of just the ID.

```go
// BEFORE (fix.go lines 98-112):
	var succeeded, failed int
	for _, res := range r.Results {
		if res.Success {
			succeeded++
			fmt.Printf("  [OK]   %s — PR: %s\n", res.Issue.ID, res.PRURL)
		} else if res.Error != nil {
			failed++
			fmt.Printf("  [FAIL] %s — %s\n", res.Issue.ID, res.Error)
		} else {
			fmt.Printf("  [SKIP] %s (dry-run)\n", res.Issue.ID)
		}
	}

// AFTER:
	var succeeded, failed int
	for _, res := range r.Results {
		iss := res.Issue
		issueDesc := fmt.Sprintf("%s %s -- %s", iss.ID, iss.Category, truncateSummary(iss.Summary, 60))
		if iss.File != "" {
			issueDesc += fmt.Sprintf(" (%s)", iss.File)
		}

		if res.Success {
			succeeded++
			fmt.Printf("  [OK]   %s\n", issueDesc)
			fmt.Printf("         PR: %s\n", res.PRURL)
		} else if res.Error != nil {
			failed++
			fmt.Printf("  [FAIL] %s\n", issueDesc)
			fmt.Printf("         %s\n", res.Error)
		} else {
			fmt.Printf("  [SKIP] %s (dry-run)\n", issueDesc)
		}
	}
```

Note: the `truncate()` function already exists in `agent.go`. We need to either:
- Import it from `agent.go` (it's in the same package `engine`) -- but `printFixResult` is in package `main`. So we need a local helper.
- Add a local `truncateSummary` helper in `fix.go`.

Add this to `fix.go`:

```go
func truncateSummary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
```

### Tests

This is CLI output formatting. No unit test needed, but manual verification is expected.

---

## Priority 3: Add `--skip-scan` Flag to Fix Command

### Files to modify

- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/cmd/medivac/fix.go` -- add flag, change RunE
- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/engine/engine.go` -- add `FixFromTracker()` method

### Changes in `fix.go`

Add a new flag variable and register it:

```go
var (
	fixBranch      string
	fixMaxParallel int
	fixModel       string
	fixBudget      float64
	fixSkipScan    bool // <-- NEW
)

func init() {
	rootCmd.AddCommand(fixCmd)
	fixCmd.Flags().StringVar(&fixBranch, "branch", "main", "Branch to scan for failures")
	fixCmd.Flags().IntVar(&fixMaxParallel, "max-parallel", 3, "Maximum parallel fix agents")
	fixCmd.Flags().StringVar(&fixModel, "model", "sonnet", "Claude model for fix agents")
	fixCmd.Flags().Float64Var(&fixBudget, "budget", 1.0, "Cost budget per agent in USD")
	fixCmd.Flags().BoolVar(&fixSkipScan, "skip-scan", false, "Skip scanning; fix issues from existing tracker state")
}
```

In the `RunE` function, branch on `fixSkipScan`:

```go
RunE: func(cmd *cobra.Command, args []string) error {
	// ... (same setup as before through engine creation) ...

	var result *engine.FixResult
	var err error
	if fixSkipScan {
		result, err = eng.FixFromTracker(cmd.Context())
	} else {
		result, err = eng.Fix(cmd.Context())
	}
	if err != nil {
		return err
	}

	printFixResult(result)
	return nil
},
```

### Changes in `engine.go`

Add a new `FixFromTracker()` method that skips scanning:

```go
// FixFromTracker launches fix agents for currently actionable issues in the
// tracker without re-scanning CI. Use this when you have already run a scan
// and want to fix the issues it found.
func (e *Engine) FixFromTracker(ctx context.Context) (*FixResult, error) {
	fixResult := &FixResult{}

	actionable := e.tracker.GetActionable()
	if len(actionable) == 0 {
		e.logger.Info("no actionable issues found in tracker")
		return fixResult, nil
	}

	e.logger.Info("launching fix agents from tracker",
		"count", len(actionable),
		"maxParallel", e.config.MaxParallel,
		"dryRun", e.config.DryRun,
	)

	if e.config.DryRun {
		for _, iss := range actionable {
			fixResult.Results = append(fixResult.Results, &FixAgentResult{
				Issue: iss,
			})
		}
		return fixResult, nil
	}

	// Launch fix agents with bounded parallelism (same logic as Fix())
	sem := make(chan struct{}, e.config.MaxParallel)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, iss := range actionable {
		e.tracker.UpdateStatus(iss.Signature, issue.StatusInProgress)

		wg.Add(1)
		go func(iss *issue.Issue) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			result := RunFixAgent(ctx, FixAgentConfig{
				Issue:      iss,
				WTManager:  e.config.WTManager,
				GHRunner:   e.config.GHRunner,
				Model:      e.config.AgentModel,
				BudgetUSD:  e.config.AgentBudget,
				BaseBranch: e.config.Branch,
				SessionDir: e.config.SessionDir,
				Logger:     e.logger.With("issue", iss.ID),
			})

			recordFixAttempt(e.tracker, result)

			mu.Lock()
			fixResult.Results = append(fixResult.Results, result)
			fixResult.TotalCost += result.AgentCost
			mu.Unlock()
		}(iss)
	}

	wg.Wait()

	if err := e.tracker.Save(); err != nil {
		return fixResult, fmt.Errorf("save tracker: %w", err)
	}

	return fixResult, nil
}
```

**Design decision:** We create a separate `FixFromTracker()` method rather than adding a `SkipScan bool` to `Config`. This keeps the API explicit -- callers know whether they are scanning or not. The `FixResult` struct's `ScanResult` field will be `nil` when called via `FixFromTracker()`, which `printFixResult()` must handle.

**Update `printFixResult()` to handle nil ScanResult:**

```go
func printFixResult(r *engine.FixResult) {
	if r.ScanResult != nil {
		printScanResult(r.ScanResult)
	}
	// ... rest of function unchanged ...
}
```

### Tests to add in `engine_test.go`

```go
func TestFixFromTracker_DryRun(t *testing.T) {
	mock := newMockGHRunner()

	dir := t.TempDir()
	trackerPath := filepath.Join(dir, ".fixer", "issues.json")

	// Create engine and pre-populate tracker via a scan first
	setupMockForScan(mock)
	triageQuery := mockTriageQuery([]triageItem{
		{Category: "lint/go", File: "main.go", Line: 10, Summary: "unused variable x", Details: "detail"},
	}, 0.001)

	eng, err := New(Config{
		GHRunner:    mock,
		RepoDir:     dir,
		TrackerPath: trackerPath,
		Branch:      "main",
		RunLimit:    5,
		DryRun:      true,
		TriageQuery: triageQuery,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Scan to populate tracker
	_, err = eng.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Now fix from tracker (no re-scan)
	result, err := eng.FixFromTracker(context.Background())
	if err != nil {
		t.Fatalf("FixFromTracker: %v", err)
	}

	if result.ScanResult != nil {
		t.Error("expected ScanResult to be nil for FixFromTracker")
	}
	if len(result.Results) != 1 {
		t.Errorf("expected 1 result, got %d", len(result.Results))
	}
	if result.TotalCost != 0 {
		t.Errorf("expected 0 cost for dry-run, got %f", result.TotalCost)
	}
}
```

---

## Priority 1: Make Fix Prompts Build-System-Aware

### Files to modify

- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/engine/prompts.go` -- major rewrite of `buildFixPrompt()`
- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/engine/buildinfo.go` -- NEW file for build system detection
- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/engine/agent.go` -- pass RepoDir to buildFixPrompt
- `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/engine/engine.go` -- pass RepoDir to agent config

### New file: `buildinfo.go`

This file handles build system detection from repo files.

```go
package engine

import (
	"os"
	"path/filepath"
	"strings"
)

// BuildSystem identifies the project's build tooling.
type BuildSystem string

const (
	BuildSystemBazel   BuildSystem = "bazel"
	BuildSystemNx      BuildSystem = "nx"
	BuildSystemMake    BuildSystem = "make"
	BuildSystemNPM     BuildSystem = "npm"
	BuildSystemUnknown BuildSystem = "unknown"
)

// BuildInfo holds detected build system metadata for a repository.
type BuildInfo struct {
	System       BuildSystem
	LintCmd      string
	BuildCmd     string
	TestCmd      string
	ClaudeMD     string // contents of CLAUDE.md if present
	ExtraRules   []string
}

// DetectBuildInfo probes the repo root for build system markers and returns
// structured build metadata. Detection order matters: more specific systems
// (Bazel, Nx) are checked before generic ones (Make, npm).
func DetectBuildInfo(repoDir string) BuildInfo {
	info := BuildInfo{System: BuildSystemUnknown}

	// Read CLAUDE.md if present (always, regardless of build system)
	claudeMDPath := filepath.Join(repoDir, "CLAUDE.md")
	if data, err := os.ReadFile(claudeMDPath); err == nil {
		info.ClaudeMD = string(data)
	}

	// Check for Bazel
	if fileExists(filepath.Join(repoDir, "BUILD.bazel")) ||
		fileExists(filepath.Join(repoDir, "WORKSPACE")) ||
		fileExists(filepath.Join(repoDir, "WORKSPACE.bazel")) ||
		fileExists(filepath.Join(repoDir, "MODULE.bazel")) {
		info.System = BuildSystemBazel
		info.LintCmd = "scripts/lint.sh (if it exists) or bazel test //..."
		info.BuildCmd = "bazel build //..."
		info.TestCmd = "bazel test //..."
		info.ExtraRules = []string{
			"Never use `go build` or `go test` directly -- use Bazel",
			"Never manually edit BUILD.bazel or go.mod -- use the proper Bazel commands",
		}
		return info
	}

	// Check for Nx monorepo
	if fileExists(filepath.Join(repoDir, "nx.json")) {
		info.System = BuildSystemNx
		info.LintCmd = "pnpm dlx nx affected -t lint"
		info.BuildCmd = "pnpm dlx nx affected -t build"
		info.TestCmd = "pnpm dlx nx affected -t test"
		info.ExtraRules = []string{
			"Use pnpm for package management, not npm or yarn",
			"Run Nx targets through `pnpm dlx nx` or `npx nx`",
		}
		// Check for Python tooling in the same repo
		if fileExists(filepath.Join(repoDir, "pyproject.toml")) {
			info.ExtraRules = append(info.ExtraRules,
				"For Python code, use `uv run ruff check .` for linting and `uv run pytest` for tests",
			)
		}
		return info
	}

	// Check for Makefile
	if fileExists(filepath.Join(repoDir, "Makefile")) {
		info.System = BuildSystemMake
		info.LintCmd = "make lint (if target exists)"
		info.BuildCmd = "make build (if target exists)"
		info.TestCmd = "make test (if target exists)"
		return info
	}

	// Check for npm/pnpm/yarn
	if fileExists(filepath.Join(repoDir, "package.json")) {
		info.System = BuildSystemNPM
		pkgManager := "npm"
		if fileExists(filepath.Join(repoDir, "pnpm-lock.yaml")) {
			pkgManager = "pnpm"
		} else if fileExists(filepath.Join(repoDir, "yarn.lock")) {
			pkgManager = "yarn"
		}
		info.LintCmd = pkgManager + " run lint"
		info.BuildCmd = pkgManager + " run build"
		info.TestCmd = pkgManager + " run test"
		return info
	}

	// Fallback
	info.LintCmd = "check CLAUDE.md for lint instructions"
	info.BuildCmd = "check CLAUDE.md for build instructions"
	info.TestCmd = "check CLAUDE.md for test instructions"
	return info
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TruncateClaudeMD returns the CLAUDE.md contents, truncated to maxLen
// characters with an ellipsis if needed. This prevents the fix prompt from
// becoming excessively large.
func TruncateClaudeMD(content string, maxLen int) string {
	if len(content) <= maxLen {
		return content
	}
	return content[:maxLen] + "\n... (truncated, read the full CLAUDE.md for complete instructions)"
}
```

### Rewrite `prompts.go`

Replace the hardcoded Bazel prompt with a build-info-aware prompt:

```go
package engine

import (
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/medivac/issue"
)

// maxClaudeMDLen is the maximum number of characters of CLAUDE.md to include
// in the fix prompt. This prevents the prompt from becoming excessively large.
const maxClaudeMDLen = 4000

// buildFixPrompt constructs the prompt for a fix agent given an issue.
// It uses detected build system information to generate correct commands.
func buildFixPrompt(iss *issue.Issue, branch string, buildInfo BuildInfo) string {
	var b strings.Builder

	b.WriteString("You are a CI failure fixer agent. Your goal is to fix a specific CI failure in this repository.\n\n")

	b.WriteString("## Failure Details\n\n")
	b.WriteString(fmt.Sprintf("- **Category:** %s\n", iss.Category))
	b.WriteString(fmt.Sprintf("- **Summary:** %s\n", iss.Summary))
	if iss.File != "" {
		b.WriteString(fmt.Sprintf("- **File:** %s", iss.File))
		if iss.Line > 0 {
			b.WriteString(fmt.Sprintf(":%d", iss.Line))
		}
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("- **Branch:** %s\n", branch))
	b.WriteString(fmt.Sprintf("- **Times seen:** %d\n", iss.SeenCount))

	if iss.Details != "" {
		b.WriteString(fmt.Sprintf("\n### Error Details\n\n```\n%s\n```\n\n", iss.Details))
	}

	// Build system info
	b.WriteString("## Build System\n\n")
	b.WriteString(fmt.Sprintf("Detected build system: **%s**\n\n", buildInfo.System))

	b.WriteString("## Instructions\n\n")
	b.WriteString("1. **Read CLAUDE.md** in the repo root first if it exists. Follow its instructions for build/test/lint commands.\n")
	b.WriteString("2. **Investigate** the failure by reading the relevant files and understanding the root cause.\n")
	b.WriteString("3. **Fix** the issue with the minimal change needed.\n")
	b.WriteString("4. **Verify** your fix by running the appropriate checks:\n")

	switch {
	case strings.HasPrefix(string(iss.Category), "lint/"):
		b.WriteString(fmt.Sprintf("   - Run: `%s`\n", buildInfo.LintCmd))
	case strings.HasPrefix(string(iss.Category), "build/"):
		b.WriteString(fmt.Sprintf("   - Run: `%s`\n", buildInfo.BuildCmd))
	case strings.HasPrefix(string(iss.Category), "test/"):
		b.WriteString(fmt.Sprintf("   - Run: `%s`\n", buildInfo.TestCmd))
	default:
		b.WriteString(fmt.Sprintf("   - Lint: `%s`\n", buildInfo.LintCmd))
		b.WriteString(fmt.Sprintf("   - Build: `%s`\n", buildInfo.BuildCmd))
	}

	b.WriteString("5. **Commit** your changes with a clear commit message explaining the fix.\n")
	b.WriteString("6. **Push** to the remote branch.\n\n")

	b.WriteString("## Important Rules\n\n")
	b.WriteString("- Read and follow the CLAUDE.md instructions in the repo root (if present)\n")
	b.WriteString("- Make minimal, focused changes -- do not refactor unrelated code\n")
	b.WriteString("- If you cannot fix the issue, explain why clearly\n")
	for _, rule := range buildInfo.ExtraRules {
		b.WriteString(fmt.Sprintf("- %s\n", rule))
	}

	// Include CLAUDE.md content if available
	if buildInfo.ClaudeMD != "" {
		b.WriteString("\n## Repository Instructions (from CLAUDE.md)\n\n")
		b.WriteString("The following are the project-specific instructions from CLAUDE.md. **Follow these instructions as your primary guide for build, test, and lint commands.**\n\n")
		b.WriteString("```\n")
		b.WriteString(TruncateClaudeMD(buildInfo.ClaudeMD, maxClaudeMDLen))
		b.WriteString("\n```\n")
	}

	return b.String()
}
```

### Update call sites

**`agent.go`:** The `RunFixAgent` function calls `buildFixPrompt`. It needs access to `repoDir` to detect the build system. We pass it through the config.

```go
// BEFORE (agent.go line 71):
prompt := buildFixPrompt(config.Issue, config.BaseBranch)

// AFTER:
buildInfo := DetectBuildInfo(config.RepoDir)
prompt := buildFixPrompt(config.Issue, config.BaseBranch, buildInfo)
```

Add `RepoDir` to `FixAgentConfig`:

```go
// BEFORE (agent.go lines 16-25):
type FixAgentConfig struct {
	Issue      *issue.Issue
	WTManager  *wt.Manager
	GHRunner   wt.GHRunner
	Logger     *slog.Logger
	Model      string
	BaseBranch string
	SessionDir string
	BudgetUSD  float64
}

// AFTER:
type FixAgentConfig struct {
	Issue      *issue.Issue
	WTManager  *wt.Manager
	GHRunner   wt.GHRunner
	Logger     *slog.Logger
	Model      string
	BaseBranch string
	SessionDir string
	RepoDir    string  // <-- NEW: for build system detection
	BudgetUSD  float64
}
```

**`engine.go`:** Pass `RepoDir` when constructing `FixAgentConfig`:

```go
// BEFORE (engine.go line 266, inside Fix()):
result := RunFixAgent(ctx, FixAgentConfig{
	Issue:      iss,
	WTManager:  e.config.WTManager,
	GHRunner:   e.config.GHRunner,
	Model:      e.config.AgentModel,
	BudgetUSD:  e.config.AgentBudget,
	BaseBranch: e.config.Branch,
	SessionDir: e.config.SessionDir,
	Logger:     e.logger.With("issue", iss.ID),
})

// AFTER:
result := RunFixAgent(ctx, FixAgentConfig{
	Issue:      iss,
	WTManager:  e.config.WTManager,
	GHRunner:   e.config.GHRunner,
	Model:      e.config.AgentModel,
	BudgetUSD:  e.config.AgentBudget,
	BaseBranch: e.config.Branch,
	SessionDir: e.config.SessionDir,
	RepoDir:    e.config.RepoDir,
	Logger:     e.logger.With("issue", iss.ID),
})
```

Apply the same change in `FixFromTracker()` (the new method from Priority 3).

### Tests

**New file: `/Users/ming/worktrees/yoloswe/feature/fixer/medivac/engine/buildinfo_test.go`**

```go
package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectBuildInfo_Bazel(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "WORKSPACE"), []byte(""), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemBazel {
		t.Errorf("expected bazel, got %s", info.System)
	}
	if info.BuildCmd == "" {
		t.Error("expected non-empty build command")
	}
}

func TestDetectBuildInfo_Nx(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "nx.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemNx {
		t.Errorf("expected nx, got %s", info.System)
	}
}

func TestDetectBuildInfo_NxWithPython(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "nx.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(""), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemNx {
		t.Errorf("expected nx, got %s", info.System)
	}
	// Should have Python rule
	found := false
	for _, rule := range info.ExtraRules {
		if rule == "For Python code, use `uv run ruff check .` for linting and `uv run pytest` for tests" {
			found = true
		}
	}
	if !found {
		t.Error("expected Python-specific rule in ExtraRules")
	}
}

func TestDetectBuildInfo_Make(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte(""), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemMake {
		t.Errorf("expected make, got %s", info.System)
	}
}

func TestDetectBuildInfo_NPM(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemNPM {
		t.Errorf("expected npm, got %s", info.System)
	}
}

func TestDetectBuildInfo_Pnpm(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte(""), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemNPM {
		t.Errorf("expected npm (pnpm variant), got %s", info.System)
	}
	if info.LintCmd != "pnpm run lint" {
		t.Errorf("expected pnpm lint cmd, got %s", info.LintCmd)
	}
}

func TestDetectBuildInfo_Unknown(t *testing.T) {
	dir := t.TempDir()
	info := DetectBuildInfo(dir)
	if info.System != BuildSystemUnknown {
		t.Errorf("expected unknown, got %s", info.System)
	}
}

func TestDetectBuildInfo_ClaudeMD(t *testing.T) {
	dir := t.TempDir()
	content := "# Build Instructions\n\nRun `make all`\n"
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(content), 0644)

	info := DetectBuildInfo(dir)
	if info.ClaudeMD != content {
		t.Errorf("CLAUDE.md content not loaded")
	}
}

func TestTruncateClaudeMD(t *testing.T) {
	short := "short content"
	if TruncateClaudeMD(short, 100) != short {
		t.Error("short content should not be truncated")
	}

	long := "a very long string that should be truncated"
	result := TruncateClaudeMD(long, 10)
	if len(result) <= 10 {
		// Result should be 10 chars + the truncation message
		t.Error("truncation did not work")
	}
	if result[:10] != long[:10] {
		t.Errorf("truncated prefix mismatch")
	}
}
```

**Update existing prompt test:** The `buildFixPrompt` function signature changes from 2 to 3 arguments. Update any existing tests that call it. Looking at the test files, `buildFixPrompt` is not directly tested in the existing test suite (it is called indirectly through `RunFixAgent`). However, it would be prudent to add a direct test:

```go
// In engine_test.go or a new prompts_test.go:
func TestBuildFixPrompt_BazelRepo(t *testing.T) {
	iss := &issue.Issue{
		Category: github.CategoryLintGo,
		Summary:  "unused variable",
		File:     "main.go",
		Line:     10,
	}
	info := BuildInfo{
		System:   BuildSystemBazel,
		LintCmd:  "scripts/lint.sh",
		BuildCmd: "bazel build //...",
		TestCmd:  "bazel test //...",
	}
	prompt := buildFixPrompt(iss, "main", info)
	if !strings.Contains(prompt, "scripts/lint.sh") {
		t.Error("expected lint command in prompt")
	}
	if strings.Contains(prompt, "pnpm") {
		t.Error("should not contain pnpm for bazel repo")
	}
}

func TestBuildFixPrompt_NxRepo(t *testing.T) {
	iss := &issue.Issue{
		Category: github.CategoryLintTS,
		Summary:  "unused variable",
		File:     "src/app.ts",
	}
	info := BuildInfo{
		System:  BuildSystemNx,
		LintCmd: "pnpm dlx nx affected -t lint",
	}
	prompt := buildFixPrompt(iss, "main", info)
	if !strings.Contains(prompt, "pnpm dlx nx") {
		t.Error("expected nx lint command in prompt")
	}
	if strings.Contains(prompt, "bazel") {
		t.Error("should not contain bazel for nx repo")
	}
}

func TestBuildFixPrompt_IncludesClaudeMD(t *testing.T) {
	iss := &issue.Issue{
		Category: github.CategoryBuild,
		Summary:  "build failed",
	}
	info := BuildInfo{
		System:   BuildSystemUnknown,
		BuildCmd: "check CLAUDE.md",
		ClaudeMD: "## Build\n\nRun `make all`\n",
	}
	prompt := buildFixPrompt(iss, "main", info)
	if !strings.Contains(prompt, "Run `make all`") {
		t.Error("expected CLAUDE.md content in prompt")
	}
	if !strings.Contains(prompt, "Repository Instructions") {
		t.Error("expected CLAUDE.md section header")
	}
}
```

---

## Key Design Decisions

### 1. Category removed from signature (Priority 2a)

**Decision:** The signature format changes from `{category}:{hash}:{file}` to `{hash}:{file}`.

**Rationale:** The PM assessment showed that the same error (e.g., TS7006 at `chat-panel.tsx:252`) produces 4 separate tracked issues because it appears in different categories (`lint/ts` vs `build/docker`). Category is a presentation concern, not a dedup concern. The same underlying error should be one issue regardless of which CI job caught it.

**Trade-off:** We lose the ability to distinguish genuinely different errors that happen to have the same message in the same file but different categories. In practice, this is extremely rare -- if the message and file are the same, the root cause is the same.

### 2. Build info detection via file probing, not config file (Priority 1)

**Decision:** We auto-detect the build system by probing for marker files (`nx.json`, `WORKSPACE`, `Makefile`, `package.json`) rather than requiring a `.medivac/config.yaml`.

**Rationale:** Zero-config is essential for adoption. A config file creates friction and a chicken-and-egg problem (you need to configure fixer before you can use it). File probing works for the vast majority of repos. The CLAUDE.md inclusion handles edge cases where the auto-detection is insufficient.

**Trade-off:** The detection is heuristic. A repo with both `Makefile` and `package.json` would be detected as `make` (higher priority). If this becomes a problem, a config file can be added later as an override, not the primary mechanism.

### 3. Separate `FixFromTracker()` method rather than a Config flag (Priority 3)

**Decision:** New method `FixFromTracker()` instead of `Config.SkipScan bool`.

**Rationale:** The caller's intent is explicit in the code. `Fix()` always scans; `FixFromTracker()` never scans. There is no ambiguity. Config flags that conditionally skip steps create harder-to-reason-about behavior.

### 4. Old signature format not migrated (Priority 2a)

**Decision:** Existing `issues.json` files with old-format signatures (`category:hash:file`) are NOT migrated. They will remain in the tracker as stale issues that no longer match incoming failures.

**Rationale:** The fixer is in early development with no production deployments. The cost of a migration tool exceeds the cost of users re-scanning (which they would do anyway). If needed, a user can delete `issues.json` to start fresh.

---

## Out of Scope (NOT doing in this cycle)

1. **Issue grouping / batch fixes (C4/D2).** This requires a new "group" concept, changes to the agent prompt, and coordination logic. It is a significant feature, not a bug fix. Deferring to cycle 2.

2. **Root-cause linking (C6/D7).** Linking Docker build failures to their constituent TS errors requires cross-issue analysis during triage. This is architecturally complex. Deferring to cycle 2.

3. **Issue filtering flags (D5).** `--category`, `--issue`, `--file`, `--min-seen` flags for the fix command. Useful but not critical. Deferring to cycle 2.

4. **Interactive confirmation before fix (workflow friction item).** Requires stdin interaction. Deferring.

5. **Color output (D9).** Nice-to-have cosmetic improvement. Deferring.

6. **Expanded category taxonomy (D8).** Adding `build/nx`, `test/vitest`, etc. This is a triage prompt change and can be done independently. Deferring to cycle 2 to pair with issue grouping.

7. **`dismiss`/`wontfix` command (C8/D6).** New command. Deferring to cycle 2.

8. **Cost tracking in `status` output (D10).** Deferring.

9. **Signature migration tool.** Not needed for early-stage tool.

---

## File Change Summary

| File | Change Type | Priority |
|------|------------|----------|
| `medivac/cmd/medivac/main.go` | Remove `repoName` var and flag | P5 |
| `medivac/github/parser.go` | Improve `normalizeMessage()`, update `ComputeSignature()`, add `canonicalizePath()` | P2a |
| `medivac/github/parser_test.go` | Add normalization tests, update signature format tests | P2a |
| `medivac/issue/tracker.go` | Fix duplicate in Updated list, fix timestamp ordering | P2b, P2c |
| `medivac/issue/tracker_test.go` | Add dedup and timestamp tests | P2b, P2c |
| `medivac/cmd/medivac/fix.go` | Add `--skip-scan` flag, improve dry-run output, add `truncateSummary` | P3, P4 |
| `medivac/engine/engine.go` | Add `FixFromTracker()` method, pass RepoDir in agent config | P3, P1 |
| `medivac/engine/engine_test.go` | Add `TestFixFromTracker_DryRun` | P3 |
| `medivac/engine/buildinfo.go` | NEW: build system detection | P1 |
| `medivac/engine/buildinfo_test.go` | NEW: build info detection tests | P1 |
| `medivac/engine/prompts.go` | Rewrite `buildFixPrompt()` to use BuildInfo | P1 |
| `medivac/engine/agent.go` | Add RepoDir to FixAgentConfig, pass BuildInfo to prompt | P1 |

**Total: 12 files (10 modified, 2 new), ~350 lines of new/changed code.**
