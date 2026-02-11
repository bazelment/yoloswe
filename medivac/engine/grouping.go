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
	if iss.Category == issue.CategoryInfraDepbot {
		pkg := extractPackageName(iss.Summary)
		if pkg != "" {
			return fmt.Sprintf("dependabot:%s", pkg)
		}
	}

	// 3. Fallback: singleton group by signature
	return iss.Signature
}

// isTypeScriptCategory returns true for categories that contain TypeScript errors.
func isTypeScriptCategory(cat issue.FailureCategory) bool {
	switch cat {
	case "lint/ts", "build", "build/docker":
		return true
	}
	return false
}

// extractTSErrorCode pulls TS error codes from the issue's details or summary.
func extractTSErrorCode(iss *issue.Issue) string {
	// Prefer structured error code from triage
	if iss.ErrorCode != "" && strings.HasPrefix(iss.ErrorCode, "TS") {
		return iss.ErrorCode
	}
	// Fallback: regex extraction from details/summary
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
// E.g. "no security update needed as cryptography is no longer vulnerable" -> "cryptography"
var packageNamePrimary = regexp.MustCompile(`(?i)(?:for|of)\s+([a-z][a-z0-9_.-]*)`)
var packageNameFallback = regexp.MustCompile(`(?i)\bas\s+([a-z][a-z0-9_.-]*)\b`)

func extractPackageName(summary string) string {
	if m := packageNamePrimary.FindStringSubmatch(summary); len(m) >= 2 {
		return strings.ToLower(m[1])
	}
	if m := packageNameFallback.FindStringSubmatch(summary); len(m) >= 2 {
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
