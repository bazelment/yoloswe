# Cycle 2 Architecture Design

**Date:** 2026-02-10
**Author:** Architect (Claude Opus 4.6)
**Scope:** Issue Grouping, Dismiss Command, Triage Prompt Improvements

---

## Priority 1: Issue Grouping

### Problem

18 agents for 4 root causes = ~4x cost waste. The 12 TS7006 issues should be fixed by 1 agent with a combined prompt.

### Design Overview

Grouping happens BETWEEN `GetActionable()` and agent launch. A new file `engine/grouping.go` contains all grouping logic. The engine methods `Fix()` and `FixFromTracker()` call `GroupIssues()` on the actionable list, then launch 1 agent per group instead of 1 per issue.

### New File: `medivac/engine/grouping.go`

```go
package engine

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bazelment/yoloswe/medivac/issue"
)

// IssueGroup is a set of issues that should be fixed by a single agent.
type IssueGroup struct {
	// Key is the grouping key (e.g. "TS7006:src/" or "dependabot:cryptography").
	Key    string
	Issues []*issue.Issue
}

// Leader returns the first issue in the group, used as the "representative"
// for branch naming, PR title, etc.
func (g *IssueGroup) Leader() *issue.Issue {
	return g.Issues[0]
}

// tsErrorCode matches TypeScript error codes like TS7006, TS2307.
var tsErrorCode = regexp.MustCompile(`TS\d{4,5}`)

// goErrorPattern matches common Go lint/build error patterns.
var goErrorPattern = regexp.MustCompile(`(?i)(unused|undefined|cannot|missing|incompatible)`)

// GroupIssues groups actionable issues by error pattern to reduce agent count.
// Issues that don't match any grouping heuristic get their own singleton group.
func GroupIssues(issues []*issue.Issue) []IssueGroup {
	groups := make(map[string][]*issue.Issue)
	order := make([]string, 0) // preserve insertion order

	for _, iss := range issues {
		key := groupKey(iss)
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], iss)
	}

	result := make([]IssueGroup, 0, len(groups))
	for _, key := range order {
		result = append(result, IssueGroup{
			Key:    key,
			Issues: groups[key],
		})
	}
	return result
}

// groupKey computes the grouping key for an issue.
// Grouping heuristics (in priority order):
//  1. TypeScript error code + project directory -> "TS7006:src/"
//  2. Dependabot issues -> "dependabot:<package>" (extracted from summary)
//  3. Everything else -> issue signature (singleton group)
func groupKey(iss *issue.Issue) string {
	// 1. TypeScript error code grouping
	if isTypeScriptCategory(iss.Category) {
		code := extractTSErrorCode(iss)
		if code != "" {
			dir := projectDir(iss.File)
			return fmt.Sprintf("ts:%s:%s", code, dir)
		}
	}

	// 2. Dependabot grouping by package name
	if iss.Category == "infra/dependabot" {
		pkg := extractPackageName(iss.Summary)
		if pkg != "" {
			return fmt.Sprintf("dependabot:%s", pkg)
		}
	}

	// 3. Fallback: singleton group by signature
	return iss.Signature
}

// isTypeScriptCategory returns true for categories that contain TypeScript errors.
func isTypeScriptCategory(cat github.FailureCategory) bool {
	switch cat {
	case "lint/ts", "build", "build/docker":
		return true
	}
	return false
}

// extractTSErrorCode pulls TS error codes from the issue's details or summary.
func extractTSErrorCode(iss *issue.Issue) string {
	// Check details first (more likely to have the raw error code)
	if code := tsErrorCode.FindString(iss.Details); code != "" {
		return code
	}
	if code := tsErrorCode.FindString(iss.Summary); code != "" {
		return code
	}
	// Heuristic: detect common TS error patterns without explicit codes
	if strings.Contains(iss.Summary, "implicitly has an 'any' type") ||
		strings.Contains(iss.Summary, "implicitly has 'any' type") {
		return "TS7006"
	}
	if strings.Contains(iss.Summary, "Cannot find module") {
		return "TS2307"
	}
	return ""
}

// extractPackageName pulls the primary package name from a dependabot summary.
// E.g. "No solution found when resolving dependencies for cryptography" -> "cryptography"
// E.g. "Dependency resolution failed for cryptography>=46.0.0" -> "cryptography"
var packageNameRe = regexp.MustCompile(`(?i)(?:for|of)\s+([a-z][a-z0-9_-]*)`)

func extractPackageName(summary string) string {
	m := packageNameRe.FindStringSubmatch(summary)
	if len(m) >= 2 {
		return strings.ToLower(m[1])
	}
	return ""
}

// projectDir returns the top-level project directory from a file path.
// E.g. "src/components/foo.tsx" -> "src/"
// E.g. "services/typescript/forge-v2/src/foo.tsx" -> "services/typescript/forge-v2/"
// E.g. "" -> ""
func projectDir(file string) string {
	if file == "" {
		return ""
	}
	// For files under src/, the project is the root
	if strings.HasPrefix(file, "src/") {
		return "src/"
	}
	// For files under services/<type>/<name>/..., the project is that prefix
	parts := strings.SplitN(file, "/", 4)
	if len(parts) >= 3 && parts[0] == "services" {
		return strings.Join(parts[:3], "/") + "/"
	}
	return ""
}
```

### New File: `medivac/engine/grouping_test.go`

```go
package engine

import (
	"testing"

	"github.com/bazelment/yoloswe/medivac/github"
	"github.com/bazelment/yoloswe/medivac/issue"
)

func TestGroupIssues_TSErrorCodes(t *testing.T) {
	issues := []*issue.Issue{
		{Signature: "sig1", Category: "lint/ts", Summary: "Parameter 'e' implicitly has an 'any' type", File: "src/components/foo.tsx", Details: "error TS7006"},
		{Signature: "sig2", Category: "lint/ts", Summary: "Parameter 'file' implicitly has an 'any' type", File: "src/components/bar.tsx", Details: "error TS7006"},
		{Signature: "sig3", Category: "lint/ts", Summary: "Cannot find module '@sycamore-labs/ui'", File: "src/admin/viewer.tsx", Details: "error TS2307"},
	}

	groups := GroupIssues(issues)

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups (TS7006 + TS2307), got %d", len(groups))
	}

	// First group should have the two TS7006 issues
	if len(groups[0].Issues) != 2 {
		t.Errorf("expected 2 issues in TS7006 group, got %d", len(groups[0].Issues))
	}
	if len(groups[1].Issues) != 1 {
		t.Errorf("expected 1 issue in TS2307 group, got %d", len(groups[1].Issues))
	}
}

func TestGroupIssues_Dependabot(t *testing.T) {
	issues := []*issue.Issue{
		{Signature: "sig1", Category: github.CategoryInfraDepbot, Summary: "No solution found when resolving dependencies for cryptography"},
		{Signature: "sig2", Category: github.CategoryInfraDepbot, Summary: "Dependency resolution failed for cryptography>=46.0.0"},
	}

	groups := GroupIssues(issues)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group (both cryptography), got %d", len(groups))
	}
	if len(groups[0].Issues) != 2 {
		t.Errorf("expected 2 issues in cryptography group, got %d", len(groups[0].Issues))
	}
}

func TestGroupIssues_Singleton(t *testing.T) {
	issues := []*issue.Issue{
		{Signature: "sig1", Category: github.CategoryLintGo, Summary: "unused variable x", File: "main.go"},
		{Signature: "sig2", Category: github.CategoryInfraCI, Summary: "parallel golangci-lint is running"},
	}

	groups := GroupIssues(issues)

	if len(groups) != 2 {
		t.Fatalf("expected 2 singleton groups, got %d", len(groups))
	}
}

func TestGroupIssues_DockerBuildGroupsWithTS(t *testing.T) {
	// Docker build failures with TS error codes should group with lint/ts issues
	issues := []*issue.Issue{
		{Signature: "sig1", Category: "lint/ts", Summary: "Parameter 'e' implicitly has an 'any' type", File: "src/foo.tsx", Details: "TS7006"},
		{Signature: "sig2", Category: "build/docker", Summary: "Parameter 'idx' implicitly has an 'any' type", File: "src/bar.tsx", Details: "TS7006"},
	}

	groups := GroupIssues(issues)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group (both TS7006), got %d", len(groups))
	}
	if len(groups[0].Issues) != 2 {
		t.Errorf("expected 2 issues in TS7006 group, got %d", len(groups[0].Issues))
	}
}

func TestExtractTSErrorCode(t *testing.T) {
	tests := []struct {
		summary string
		details string
		want    string
	}{
		{"Parameter 'e' implicitly has an 'any' type", "error TS7006: ...", "TS7006"},
		{"Cannot find module '@sycamore-labs/ui'", "", "TS2307"},
		{"Parameter 'e' implicitly has an 'any' type", "", "TS7006"},
		{"some random build error", "", ""},
	}

	for _, tt := range tests {
		iss := &issue.Issue{Summary: tt.summary, Details: tt.details}
		got := extractTSErrorCode(iss)
		if got != tt.want {
			t.Errorf("extractTSErrorCode(%q, %q) = %q, want %q", tt.summary, tt.details, got, tt.want)
		}
	}
}

func TestExtractPackageName(t *testing.T) {
	tests := []struct {
		summary string
		want    string
	}{
		{"No solution found when resolving dependencies for cryptography", "cryptography"},
		{"Dependency resolution failed for cryptography>=46.0.0", "cryptography"},
		{"random failure", ""},
	}

	for _, tt := range tests {
		got := extractPackageName(tt.summary)
		if got != tt.want {
			t.Errorf("extractPackageName(%q) = %q, want %q", tt.summary, got, tt.want)
		}
	}
}
```

### Modifications to `medivac/engine/prompts.go`

Add a new function `buildGroupFixPrompt` that produces a combined prompt for a group of issues.

```go
// buildGroupFixPrompt constructs the prompt for a fix agent handling multiple
// related issues. It lists all issues in the group so the agent can fix them
// in a single pass.
func buildGroupFixPrompt(group IssueGroup, branch string, buildInfo BuildInfo) string {
	var b strings.Builder

	b.WriteString("You are a CI failure fixer agent. Your goal is to fix a GROUP of related CI failures in this repository.\n\n")

	b.WriteString("## Failure Group\n\n")
	b.WriteString(fmt.Sprintf("- **Group key:** %s\n", group.Key))
	b.WriteString(fmt.Sprintf("- **Issues in group:** %d\n", len(group.Issues)))
	b.WriteString(fmt.Sprintf("- **Branch:** %s\n\n", branch))

	b.WriteString("### Individual Failures\n\n")
	for i, iss := range group.Issues {
		b.WriteString(fmt.Sprintf("#### Failure %d\n", i+1))
		b.WriteString(fmt.Sprintf("- **Category:** %s\n", iss.Category))
		b.WriteString(fmt.Sprintf("- **Summary:** %s\n", iss.Summary))
		if iss.File != "" {
			b.WriteString(fmt.Sprintf("- **File:** %s", iss.File))
			if iss.Line > 0 {
				b.WriteString(fmt.Sprintf(":%d", iss.Line))
			}
			b.WriteString("\n")
		}
		if iss.Details != "" {
			b.WriteString(fmt.Sprintf("- **Details:** `%s`\n", truncate(iss.Details, 200)))
		}
		b.WriteString("\n")
	}

	// Use the leader issue's category for command selection
	leader := group.Leader()

	// Build system info (same as single-issue prompt)
	b.WriteString("## Build System\n\n")
	b.WriteString(fmt.Sprintf("Detected build system: **%s**\n\n", buildInfo.System))

	b.WriteString("## Instructions\n\n")
	b.WriteString("1. **Read CLAUDE.md** in the repo root first if it exists. Follow its instructions for build/test/lint commands.\n")
	b.WriteString("2. **Investigate** ALL failures listed above. They share a common root cause or error pattern.\n")
	b.WriteString("3. **Fix** all issues with the minimal set of changes needed.\n")
	b.WriteString("4. **Verify** your fix by running the appropriate checks:\n")

	switch {
	case strings.HasPrefix(string(leader.Category), "lint/"):
		b.WriteString(fmt.Sprintf("   - Run: `%s`\n", buildInfo.LintCmd))
	case strings.HasPrefix(string(leader.Category), "build/"):
		b.WriteString(fmt.Sprintf("   - Run: `%s`\n", buildInfo.BuildCmd))
	case strings.HasPrefix(string(leader.Category), "test/"):
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
	b.WriteString("- Fix ALL listed failures, not just the first one\n")
	b.WriteString("- If you cannot fix the issue, explain why clearly\n")
	for _, rule := range buildInfo.ExtraRules {
		b.WriteString(fmt.Sprintf("- %s\n", rule))
	}

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

### Modifications to `medivac/engine/agent.go`

Add a new `RunGroupFixAgent` function and a `GroupFixAgentConfig` struct.

```go
// GroupFixAgentConfig configures a fix agent for a group of related issues.
type GroupFixAgentConfig struct {
	Group      IssueGroup
	WTManager  *wt.Manager
	GHRunner   wt.GHRunner
	Logger     *slog.Logger
	Model      string
	BaseBranch string
	SessionDir string
	RepoDir    string
	BudgetUSD  float64
}

// GroupFixAgentResult reports the outcome of a grouped fix agent.
type GroupFixAgentResult struct {
	Error        error
	Group        IssueGroup
	Branch       string
	WorktreePath string
	PRURL        string
	FilesChanged []string
	AgentCost    float64
	PRNumber     int
	Success      bool
}

// RunGroupFixAgent runs a single fix agent for a group of related issues.
func RunGroupFixAgent(ctx context.Context, config GroupFixAgentConfig) *GroupFixAgentResult {
	result := &GroupFixAgentResult{Group: config.Group}
	leader := config.Group.Leader()
	log := config.Logger
	if log == nil {
		log = slog.Default()
	}

	branchName := fixBranchName(leader)
	result.Branch = branchName

	log.Info("creating worktree for issue group",
		"branch", branchName,
		"groupKey", config.Group.Key,
		"issueCount", len(config.Group.Issues),
	)

	wtPath, err := config.WTManager.New(ctx, branchName, config.BaseBranch, leader.Summary)
	if err != nil {
		result.Error = fmt.Errorf("create worktree: %w", err)
		return result
	}
	result.WorktreePath = wtPath

	buildInfo := DetectBuildInfo(config.RepoDir)
	prompt := buildGroupFixPrompt(config.Group, config.BaseBranch, buildInfo)

	sess := agent.NewEphemeralSession(agent.AgentConfig{
		Logger:     log,
		Role:       agent.RoleBuilder,
		Model:      config.Model,
		WorkDir:    wtPath,
		SessionDir: config.SessionDir,
		BudgetUSD:  config.BudgetUSD,
	}, fmt.Sprintf("fixer-group-%s", leader.ID))

	log.Info("running group fix agent",
		"groupKey", config.Group.Key,
		"issueCount", len(config.Group.Issues),
		"model", config.Model,
	)

	agentResult, execResult, _, err := sess.ExecuteWithFiles(ctx, prompt)
	result.AgentCost = sess.TotalCost()

	if err != nil {
		result.Error = fmt.Errorf("agent execution: %w", err)
		return result
	}

	if agentResult == nil || !agentResult.Success {
		errMsg := "agent reported failure"
		if agentResult != nil && agentResult.Text != "" {
			errMsg = agentResult.Text
		}
		result.Error = fmt.Errorf("agent failed: %s", errMsg)
		return result
	}

	if execResult != nil {
		result.FilesChanged = append(result.FilesChanged, execResult.FilesCreated...)
		result.FilesChanged = append(result.FilesChanged, execResult.FilesModified...)
	}

	if len(result.FilesChanged) == 0 {
		result.Error = fmt.Errorf("agent made no file changes")
		return result
	}

	log.Info("group agent completed, creating PR",
		"filesChanged", len(result.FilesChanged),
		"cost", fmt.Sprintf("$%.4f", result.AgentCost),
	)

	// PR title/body list all issues in the group
	title := fmt.Sprintf("fix(%s): %s (%d issues)", leader.Category, truncate(leader.Summary, 40), len(config.Group.Issues))
	var bodyBuilder strings.Builder
	bodyBuilder.WriteString("Automated fix for a group of related CI failures.\n\n")
	bodyBuilder.WriteString(fmt.Sprintf("**Group key:** `%s`\n", config.Group.Key))
	bodyBuilder.WriteString(fmt.Sprintf("**Issues fixed:** %d\n\n", len(config.Group.Issues)))
	for _, iss := range config.Group.Issues {
		bodyBuilder.WriteString(fmt.Sprintf("- `%s` %s — %s", iss.ID, iss.Category, truncate(iss.Summary, 80)))
		if iss.File != "" {
			bodyBuilder.WriteString(fmt.Sprintf(" (`%s`)", iss.File))
		}
		bodyBuilder.WriteString("\n")
	}
	body := bodyBuilder.String()
	if len(body) > 4000 {
		body = body[:4000] + "\n... (truncated)"
	}

	prInfo, err := wt.CreatePR(ctx, config.GHRunner, title, body, config.BaseBranch, false, result.WorktreePath)
	if err != nil {
		result.Error = fmt.Errorf("create PR: %w", err)
		return result
	}

	result.PRURL = prInfo.URL
	result.PRNumber = prInfo.Number
	result.Success = true

	log.Info("group fix PR created",
		"pr", prInfo.URL,
		"number", prInfo.Number,
		"issueCount", len(config.Group.Issues),
	)

	return result
}
```

### Modifications to `medivac/engine/engine.go`

Replace the per-issue agent launch loop with per-group agent launch. Both `Fix()` and `FixFromTracker()` need the same change. Extract a shared helper.

**Add new helper `recordGroupFixAttempt`:**

```go
// recordGroupFixAttempt updates the tracker with a group fix agent result,
// recording the attempt on ALL issues in the group.
func recordGroupFixAttempt(tracker *issue.Tracker, result *GroupFixAgentResult) {
	now := time.Now()
	attempt := issue.FixAttempt{
		Branch:    result.Branch,
		PRURL:     result.PRURL,
		PRNumber:  result.PRNumber,
		AgentCost: result.AgentCost / float64(len(result.Group.Issues)), // split cost
		StartedAt: now,
	}

	if result.Success {
		attempt.Outcome = "pr_created"
		attempt.PRState = "OPEN"
	} else {
		attempt.Outcome = "failed"
		if result.Error != nil {
			attempt.Error = result.Error.Error()
		}
	}

	completed := now
	attempt.CompletedAt = &completed

	for _, iss := range result.Group.Issues {
		tracker.AddFixAttempt(iss.Signature, attempt)
		if result.Success {
			tracker.UpdateStatus(iss.Signature, issue.StatusFixPending)
		}
	}
}
```

**Replace the agent launch loop in both `Fix()` and `FixFromTracker()`.**

The current pattern (in both methods) is:

```go
for _, iss := range actionable {
    e.tracker.UpdateStatus(iss.Signature, issue.StatusInProgress)
    wg.Add(1)
    go func(iss *issue.Issue) { ... RunFixAgent(...) ... }(iss)
}
```

Replace with:

```go
groups := GroupIssues(actionable)

e.logger.Info("grouped issues",
    "actionableCount", len(actionable),
    "groupCount", len(groups),
)

for _, group := range groups {
    for _, iss := range group.Issues {
        e.tracker.UpdateStatus(iss.Signature, issue.StatusInProgress)
    }

    wg.Add(1)
    go func(g IssueGroup) {
        defer wg.Done()

        sem <- struct{}{}
        defer func() { <-sem }()

        if len(g.Issues) == 1 {
            // Singleton group: use existing single-issue path
            result := RunFixAgent(ctx, FixAgentConfig{
                Issue:      g.Issues[0],
                WTManager:  e.config.WTManager,
                GHRunner:   e.config.GHRunner,
                Model:      e.config.AgentModel,
                BudgetUSD:  e.config.AgentBudget,
                BaseBranch: e.config.Branch,
                SessionDir: e.config.SessionDir,
                RepoDir:    e.config.RepoDir,
                Logger:     e.logger.With("issue", g.Issues[0].ID),
            })
            recordFixAttempt(e.tracker, result)
            mu.Lock()
            fixResult.Results = append(fixResult.Results, result)
            fixResult.TotalCost += result.AgentCost
            mu.Unlock()
        } else {
            // Multi-issue group: use grouped agent
            result := RunGroupFixAgent(ctx, GroupFixAgentConfig{
                Group:      g,
                WTManager:  e.config.WTManager,
                GHRunner:   e.config.GHRunner,
                Model:      e.config.AgentModel,
                BudgetUSD:  e.config.AgentBudget,
                BaseBranch: e.config.Branch,
                SessionDir: e.config.SessionDir,
                RepoDir:    e.config.RepoDir,
                Logger:     e.logger.With("group", g.Key),
            })
            recordGroupFixAttempt(e.tracker, result)
            mu.Lock()
            fixResult.GroupResults = append(fixResult.GroupResults, result)
            fixResult.TotalCost += result.AgentCost
            mu.Unlock()
        }
    }(group)
}
```

**Add `GroupResults` to `FixResult`:**

```go
type FixResult struct {
	ScanResult   *ScanResult
	Results      []*FixAgentResult
	GroupResults []*GroupFixAgentResult // multi-issue group results
	TotalCost    float64
}
```

**Dry-run path also needs updating** to show groups:

```go
if e.config.DryRun {
    for _, group := range groups {
        if len(group.Issues) == 1 {
            fixResult.Results = append(fixResult.Results, &FixAgentResult{
                Issue: group.Issues[0],
            })
        } else {
            fixResult.GroupResults = append(fixResult.GroupResults, &GroupFixAgentResult{
                Group: group,
            })
        }
    }
    return fixResult, nil
}
```

### Modifications to `medivac/cmd/medivac/fix.go` (printFixResult)

Update `printFixResult` to display group results:

```go
func printFixResult(r *engine.FixResult) {
	// ... existing scan result printing ...

	totalAgents := len(r.Results) + len(r.GroupResults)
	if totalAgents == 0 {
		fmt.Println("\nNo fix agents were launched.")
		return
	}

	fmt.Printf("\n=== Fix Results ===\n")
	fmt.Printf("Agents launched: %d (%d single, %d grouped)\n",
		totalAgents, len(r.Results), len(r.GroupResults))
	fmt.Printf("Total cost:      $%.4f\n", r.TotalCost)

	// Print single-issue results (same as before)
	for _, res := range r.Results {
		// ... existing code ...
	}

	// Print group results
	for _, res := range r.GroupResults {
		groupDesc := fmt.Sprintf("GROUP %s (%d issues)", res.Group.Key, len(res.Group.Issues))
		if res.Success {
			fmt.Printf("  [OK]   %s\n", groupDesc)
			fmt.Printf("         PR: %s\n", res.PRURL)
		} else if res.Error != nil {
			fmt.Printf("  [FAIL] %s\n", groupDesc)
			fmt.Printf("         %s\n", res.Error)
		} else {
			fmt.Printf("  [SKIP] %s (dry-run)\n", groupDesc)
			for _, iss := range res.Group.Issues {
				fmt.Printf("         - %s %s -- %s\n", iss.ID, iss.Category,
					truncateSummary(iss.Summary, 60))
			}
		}
	}
}
```

### Tests Needed for Grouping

1. **`engine/grouping_test.go`** (shown above): Unit tests for `GroupIssues`, `extractTSErrorCode`, `extractPackageName`, `projectDir`.
2. **`engine/engine_test.go`** additions:
   - `TestFix_DryRun_Grouped`: Verify that dry-run with multiple TS7006 issues produces fewer results than issues.
   - `TestFixFromTracker_DryRun_Grouped`: Same for skip-scan path.

```go
func TestFix_DryRun_Grouped(t *testing.T) {
	mock := newMockGHRunner()
	setupMockForScan(mock)

	dir := t.TempDir()

	// 3 issues with the same TS error code should group into 1
	triageQuery := mockTriageQuery([]triageItem{
		{Category: "lint/ts", File: "src/foo.tsx", Summary: "Parameter 'e' implicitly has an 'any' type", Details: "TS7006"},
		{Category: "lint/ts", File: "src/bar.tsx", Summary: "Parameter 'idx' implicitly has an 'any' type", Details: "TS7006"},
		{Category: "lint/ts", File: "src/baz.tsx", Summary: "Parameter 'x' implicitly has an 'any' type", Details: "TS7006"},
	}, 0.001)

	eng, err := New(Config{
		GHRunner:    mock,
		RepoDir:     dir,
		TrackerPath: filepath.Join(dir, ".fixer", "issues.json"),
		Branch:      "main",
		RunLimit:    5,
		DryRun:      true,
		TriageQuery: triageQuery,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := eng.Fix(context.Background())
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}

	// Should have 0 single results and 1 group result
	if len(result.Results) != 0 {
		t.Errorf("expected 0 single results, got %d", len(result.Results))
	}
	if len(result.GroupResults) != 1 {
		t.Errorf("expected 1 group result, got %d", len(result.GroupResults))
	}
	if len(result.GroupResults) > 0 && len(result.GroupResults[0].Group.Issues) != 3 {
		t.Errorf("expected 3 issues in group, got %d", len(result.GroupResults[0].Group.Issues))
	}
}
```

---

## Priority 2: Issue Dismiss/Reopen Commands

### Problem

Transient flakes (golangci-lint parallel execution) and infra issues cannot be dismissed, wasting agent budget on every fix run.

### Design Overview

`StatusWontFix` already exists in `types.go` but is never used. We reuse it as the dismiss status. Add `DismissReason` field to `Issue`. Add two CLI commands: `dismiss` and `reopen`.

### Modifications to `medivac/issue/types.go`

Add `DismissReason` to `Issue`:

```go
type Issue struct {
	// ... existing fields ...
	DismissReason string `json:"dismiss_reason,omitempty"`
}
```

Specifically, add `DismissReason` after the `FixAttempts` field:

```go
	FixAttempts   []FixAttempt           `json:"fix_attempts,omitempty"`
	DismissReason string                 `json:"dismiss_reason,omitempty"`
	Line          int                    `json:"line,omitempty"`
```

### Modifications to `medivac/issue/tracker.go`

Add `GetByID` and `Dismiss` / `Reopen` methods:

```go
// GetByID returns an issue by its short ID, or nil if not found.
func (t *Tracker) GetByID(id string) *Issue {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, iss := range t.issues {
		if iss.ID == id {
			return iss
		}
	}
	return nil
}

// Dismiss marks an issue as wont_fix with a reason.
func (t *Tracker) Dismiss(id string, reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, iss := range t.issues {
		if iss.ID == id {
			iss.Status = StatusWontFix
			iss.DismissReason = reason
			return nil
		}
	}
	return fmt.Errorf("issue %s not found", id)
}

// Reopen sets a dismissed issue back to new status.
func (t *Tracker) Reopen(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, iss := range t.issues {
		if iss.ID == id {
			if iss.Status != StatusWontFix {
				return fmt.Errorf("issue %s is not dismissed (status: %s)", id, iss.Status)
			}
			iss.Status = StatusNew
			iss.DismissReason = ""
			return nil
		}
	}
	return fmt.Errorf("issue %s not found", id)
}
```

### New File: `medivac/cmd/medivac/dismiss.go`

```go
package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/medivac/issue"
)

var dismissReason string

var dismissCmd = &cobra.Command{
	Use:   "dismiss <id>",
	Short: "Dismiss an issue (mark as wont_fix)",
	Long:  `Mark an issue as dismissed so it will not be picked up by fix agents. Use "fixer reopen <id>" to undo.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := resolveRepoRoot()
		if err != nil {
			return err
		}

		tracker, err := issue.NewTracker(resolveTrackerPath(root))
		if err != nil {
			return fmt.Errorf("load tracker: %w", err)
		}

		id := args[0]
		if err := tracker.Dismiss(id, dismissReason); err != nil {
			return err
		}

		if err := tracker.Save(); err != nil {
			return fmt.Errorf("save tracker: %w", err)
		}

		iss := tracker.GetByID(id)
		fmt.Printf("Dismissed [%s] %s -- %s\n", iss.ID, iss.Category, iss.Summary)
		if dismissReason != "" {
			fmt.Printf("  Reason: %s\n", dismissReason)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(dismissCmd)
	dismissCmd.Flags().StringVar(&dismissReason, "reason", "", "Reason for dismissing the issue")
}
```

### New File: `medivac/cmd/medivac/reopen.go`

```go
package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/medivac/issue"
)

var reopenCmd = &cobra.Command{
	Use:   "reopen <id>",
	Short: "Reopen a dismissed issue",
	Long:  `Set a previously dismissed issue back to "new" status so fix agents will pick it up again.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := resolveRepoRoot()
		if err != nil {
			return err
		}

		tracker, err := issue.NewTracker(resolveTrackerPath(root))
		if err != nil {
			return fmt.Errorf("load tracker: %w", err)
		}

		id := args[0]
		if err := tracker.Reopen(id); err != nil {
			return err
		}

		if err := tracker.Save(); err != nil {
			return fmt.Errorf("save tracker: %w", err)
		}

		iss := tracker.GetByID(id)
		fmt.Printf("Reopened [%s] %s -- %s\n", iss.ID, iss.Category, iss.Summary)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(reopenCmd)
}
```

### Modifications to `medivac/cmd/medivac/status.go`

Add `StatusDismissed` display in the status order. The `printStatus` function already iterates `statusOrder` and `StatusWontFix` is already in the list (line 69). Add dismiss reason display:

```go
// In printStatus, after printing the issue line:
for _, iss := range group {
    fmt.Printf("  [%s] %s — %s", iss.ID, iss.Category, iss.Summary)
    if iss.DismissReason != "" {
        fmt.Printf(" (reason: %s)", iss.DismissReason)
    }
    // ... rest of existing printing ...
}
```

### Tests Needed for Dismiss/Reopen

Add to `medivac/issue/tracker_test.go`:

```go
func TestDismissAndReopen(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	tracker.Reconcile([]github.CIFailure{
		{Signature: "sig1", Category: github.CategoryInfraCI, Summary: "parallel golangci-lint", Timestamp: time.Now()},
	})

	iss := tracker.GetByID(tracker.Get("sig1").ID)
	if iss == nil {
		t.Fatal("GetByID returned nil")
	}

	// Dismiss
	if err := tracker.Dismiss(iss.ID, "transient CI flake"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}

	if iss.Status != StatusWontFix {
		t.Errorf("expected wont_fix, got %s", iss.Status)
	}
	if iss.DismissReason != "transient CI flake" {
		t.Errorf("expected reason, got %q", iss.DismissReason)
	}

	// Dismissed issues should not be actionable
	actionable := tracker.GetActionable()
	if len(actionable) != 0 {
		t.Errorf("expected 0 actionable after dismiss, got %d", len(actionable))
	}

	// Reopen
	if err := tracker.Reopen(iss.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	if iss.Status != StatusNew {
		t.Errorf("expected new, got %s", iss.Status)
	}
	if iss.DismissReason != "" {
		t.Errorf("expected empty reason, got %q", iss.DismissReason)
	}

	// Now actionable again
	actionable = tracker.GetActionable()
	if len(actionable) != 1 {
		t.Errorf("expected 1 actionable after reopen, got %d", len(actionable))
	}
}

func TestDismiss_NotFound(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	if err := tracker.Dismiss("nonexistent", "reason"); err == nil {
		t.Error("expected error for nonexistent ID")
	}
}

func TestReopen_NotDismissed(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	tracker.Reconcile([]github.CIFailure{
		{Signature: "sig1", Category: github.CategoryLintGo, Summary: "err", Timestamp: time.Now()},
	})

	id := tracker.Get("sig1").ID
	if err := tracker.Reopen(id); err == nil {
		t.Error("expected error for non-dismissed issue")
	}
}

func TestDismiss_PersistsAcrossSave(t *testing.T) {
	path := tempTrackerPath(t)

	tracker1, err := NewTracker(path)
	if err != nil {
		t.Fatal(err)
	}

	tracker1.Reconcile([]github.CIFailure{
		{Signature: "sig1", Category: github.CategoryInfraCI, Summary: "flake", Timestamp: time.Now()},
	})
	id := tracker1.Get("sig1").ID
	tracker1.Dismiss(id, "flake")
	tracker1.Save()

	// Reload
	tracker2, err := NewTracker(path)
	if err != nil {
		t.Fatal(err)
	}

	iss := tracker2.GetByID(id)
	if iss == nil {
		t.Fatal("issue not found after reload")
	}
	if iss.Status != StatusWontFix {
		t.Errorf("expected wont_fix after reload, got %s", iss.Status)
	}
	if iss.DismissReason != "flake" {
		t.Errorf("expected dismiss reason after reload, got %q", iss.DismissReason)
	}
}
```

---

## Priority 3: Triage Prompt Improvements

### Problem

1. LLM triage output variance creates inconsistent issue counts across scans (B2).
2. Triage prompt says "use the full path as shown in the log" causing inconsistent file paths (B5).
3. No structured identifier extraction (B6).

### 3a. Explicit Enumeration Instruction

**File:** `medivac/github/triage.go`, function `buildTriagePrompt`

Replace the existing rules section (lines 163-172) with:

```go
	b.WriteString("Rules:\n")
	b.WriteString("- Return ONLY the JSON array, no other text\n")
	b.WriteString("- Use the most specific category that fits\n")
	b.WriteString("- If the log shows no clear errors, return an empty array []\n")
	b.WriteString("- Keep summary under 100 characters\n")
	b.WriteString("- Keep details under 500 characters\n")
	b.WriteString("- For 'summary': use the EXACT error message from the log when possible (e.g. the compiler error text). Do NOT paraphrase or reword — deterministic summaries enable deduplication across runs\n")
	b.WriteString("- For 'file': normalize ALL paths to be relative to the repository root. If a log shows a path like 'src/foo.tsx' inside a Docker build context for a subdirectory (e.g. 'services/typescript/forge-v2'), expand it to the full repo-root-relative path (e.g. 'services/typescript/forge-v2/src/foo.tsx'). Use the workflow name and job context to determine the correct prefix.\n")
	b.WriteString("- ENUMERATE: list EVERY individual error with its own file and line number as a separate entry. Do NOT summarize or group multiple errors into one entry. For example, if the log shows 10 TypeScript errors in 8 files, return 10 entries, not 1 summary.\n")
	b.WriteString("- DEDUPLICATE: if the EXACT same error (same message + same file + same line) appears in multiple jobs, include it ONLY ONCE. Attribute it to whichever job is most relevant\n")
	b.WriteString("- For 'job': must be one of the failed job names listed above\n")
	b.WriteString("- For 'details': include the error code if present (e.g. TS7006, TS2307, SA1019). This is critical for downstream grouping.\n")
```

Key changes from current:
1. **"ENUMERATE"** rule explicitly requires listing every individual error separately. This addresses B2 (LLM summarizing 13 errors as 3).
2. **File path normalization** now instructs the LLM to expand paths to repo-root-relative. This addresses B5.
3. **Error code in details** explicitly requested. This improves grouping reliability.

### 3b. Add `error_code` Field to Triage Response

**File:** `medivac/github/triage.go`

Update `triageResponse` struct:

```go
type triageResponse struct {
	Category  string `json:"category"`
	Job       string `json:"job"`
	File      string `json:"file"`
	Summary   string `json:"summary"`
	Details   string `json:"details"`
	ErrorCode string `json:"error_code"`
	Line      int    `json:"line"`
}
```

Update the JSON example in `buildTriagePrompt`:

```go
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
```

### 3c. Pass Error Code Through to Issue

**File:** `medivac/github/parser.go`

Add `ErrorCode` field to `CIFailure`:

```go
type CIFailure struct {
	// ... existing fields ...
	ErrorCode string
}
```

**File:** `medivac/github/triage.go`, in `TriageRun`:

```go
	f := CIFailure{
		// ... existing fields ...
		ErrorCode: item.ErrorCode,
	}
```

**File:** `medivac/issue/types.go`, add to `Issue`:

```go
type Issue struct {
	// ... existing fields ...
	ErrorCode string `json:"error_code,omitempty"`
}
```

**File:** `medivac/issue/tracker.go`, in `Reconcile` new-issue creation:

```go
	issue := &Issue{
		// ... existing fields ...
		ErrorCode: f.ErrorCode,
	}
```

Then `engine/grouping.go` can use `iss.ErrorCode` directly instead of regex extraction from details/summary, with a fallback to the heuristic extraction:

```go
func extractTSErrorCode(iss *issue.Issue) string {
	// Prefer structured error code from triage
	if iss.ErrorCode != "" && strings.HasPrefix(iss.ErrorCode, "TS") {
		return iss.ErrorCode
	}
	// Fallback: regex extraction from details/summary
	if code := tsErrorCode.FindString(iss.Details); code != "" {
		return code
	}
	// ... heuristic detection ...
}
```

### Tests Needed for Triage Improvements

Add to `medivac/github/triage_test.go`:

```go
func TestTriageResponse_WithErrorCode(t *testing.T) {
	response := `[{
		"category": "lint/ts",
		"job": "lint",
		"file": "services/typescript/forge-v2/src/foo.tsx",
		"line": 42,
		"error_code": "TS7006",
		"summary": "Parameter 'e' implicitly has an 'any' type",
		"details": "error TS7006: Parameter 'e' implicitly has an 'any' type."
	}]`

	items, err := parseTriageResponse(response)
	if err != nil {
		t.Fatalf("parseTriageResponse: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ErrorCode != "TS7006" {
		t.Errorf("expected error_code TS7006, got %q", items[0].ErrorCode)
	}
}
```

---

## Summary of All Changes

### New Files

| File | Purpose |
|------|---------|
| `medivac/engine/grouping.go` | Issue grouping logic: `GroupIssues()`, `IssueGroup`, extraction helpers |
| `medivac/engine/grouping_test.go` | Unit tests for grouping |
| `medivac/cmd/medivac/dismiss.go` | `dismiss` CLI subcommand |
| `medivac/cmd/medivac/reopen.go` | `reopen` CLI subcommand |

### Modified Files

| File | Changes |
|------|---------|
| `medivac/issue/types.go` | Add `DismissReason` and `ErrorCode` fields to `Issue` |
| `medivac/issue/tracker.go` | Add `GetByID()`, `Dismiss()`, `Reopen()` methods |
| `medivac/issue/tracker_test.go` | Add tests for dismiss/reopen lifecycle |
| `medivac/engine/engine.go` | Replace per-issue agent launch with per-group launch; add `GroupResults` to `FixResult` |
| `medivac/engine/agent.go` | Add `GroupFixAgentConfig`, `GroupFixAgentResult`, `RunGroupFixAgent()`, `recordGroupFixAttempt()` |
| `medivac/engine/prompts.go` | Add `buildGroupFixPrompt()` |
| `medivac/engine/engine_test.go` | Add `TestFix_DryRun_Grouped` |
| `medivac/github/parser.go` | Add `ErrorCode` field to `CIFailure` |
| `medivac/github/triage.go` | Update triage prompt (enumerate, repo-root paths, error codes), add `error_code` to response schema |
| `medivac/github/triage_test.go` | Add test for error_code parsing |
| `medivac/cmd/medivac/fix.go` | Update `printFixResult` to show group results |
| `medivac/cmd/medivac/status.go` | Show `DismissReason` in status output |

### Implementation Order

1. **Priority 2 first** (dismiss/reopen) -- simplest, no dependencies, immediately useful for manual workflow.
2. **Priority 3 next** (triage prompt + error_code field) -- adds structured data that Priority 1 consumes.
3. **Priority 1 last** (grouping) -- depends on error_code field from Priority 3 for best results, but has regex fallback.

### What This Does NOT Do (explicit scope limits)

- No root-cause linking (B4) -- deferred to Cycle 3. Grouping already handles most of the waste; the Docker build symptom issue is just 1 extra agent.
- No status output sorting (B8) -- low impact, can be done anytime.
- No color/terminal formatting (B10) -- low impact.
- No staleness mechanism for aging out old issues -- consider for Cycle 3.
- No `canonicalizePath()` applied to stored file field -- the triage prompt fix (repo-root-relative paths) is the cleaner approach.
