package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// TriageConfig controls LLM triage behavior.
type TriageConfig struct {
	Query QueryFn // injectable for testing; defaults to claude.Query
	Model string  // Claude model (default "haiku")
}

// QueryFn is the signature for one-shot LLM queries.
type QueryFn func(ctx context.Context, prompt string, opts ...claude.SessionOption) (*claude.QueryResult, error)

// triageResponse is one element of the JSON array returned by the LLM.
type triageResponse struct {
	Category string `json:"category"`
	Job      string `json:"job"`
	File     string `json:"file"`
	Summary  string `json:"summary"`
	Details  string `json:"details"`
	Line     int    `json:"line"`
}

// TriageRun sends all metadata for a single workflow run (annotations, failed
// jobs, cleaned log) to the LLM in a single call and returns deduplicated
// CIFailure results plus the cost incurred.
func TriageRun(
	ctx context.Context,
	run WorkflowRun,
	failedJobs []JobResult,
	annotations []Annotation,
	log string,
	config TriageConfig,
) ([]CIFailure, float64, error) {
	model := config.Model
	if model == "" {
		model = "haiku"
	}
	queryFn := config.Query
	if queryFn == nil {
		queryFn = claude.Query
	}

	prompt := buildTriagePrompt(run, failedJobs, annotations, log)

	result, err := queryFn(ctx, prompt, claude.WithModel(model))
	if err != nil {
		return nil, 0, fmt.Errorf("triage query: %w", err)
	}

	cost := result.Usage.CostUSD
	text := result.Text

	items, err := parseTriageResponse(text)
	if err != nil {
		return nil, cost, fmt.Errorf("parse triage response: %w", err)
	}

	// Build job name lookup for validation.
	jobNames := make(map[string]bool, len(failedJobs))
	for _, j := range failedJobs {
		jobNames[j.Name] = true
	}
	// Default job name when LLM omits or returns invalid one.
	defaultJob := ""
	if len(failedJobs) > 0 {
		defaultJob = failedJobs[0].Name
	}

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
			Timestamp: run.CreatedAt,
		}
		f.Signature = ComputeSignature(f.Category, f.File, f.Summary, f.JobName, f.Details)

		if !seen[f.Signature] {
			seen[f.Signature] = true
			failures = append(failures, f)
		}
	}

	return failures, cost, nil
}

// buildTriagePrompt constructs the prompt sent to the LLM for log triage.
// It includes all failed jobs and the combined log so the LLM can deduplicate
// across jobs and attribute failures accurately.
func buildTriagePrompt(run WorkflowRun, failedJobs []JobResult, annotations []Annotation, log string) string {
	var b strings.Builder

	b.WriteString("You are a CI failure triage system. Analyze the following CI run and extract structured failure information.\n\n")

	b.WriteString("## Run Context\n\n")
	b.WriteString(fmt.Sprintf("- Workflow: %s\n", run.Name))
	b.WriteString(fmt.Sprintf("- Branch: %s\n", run.HeadBranch))
	b.WriteString(fmt.Sprintf("- SHA: %s\n", run.HeadSHA))

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
  "file": "path/to/file (if identifiable from the log, empty string otherwise)",
  "line": 0,
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
	b.WriteString("- For 'summary': use the EXACT error message from the log when possible (e.g. the compiler error text). Do NOT paraphrase or reword — deterministic summaries enable deduplication across runs\n")
	b.WriteString("- For 'file': use the full path as shown in the log. If the log shows a path relative to the repo root, use that. Do NOT add or remove path prefixes\n")
	b.WriteString("- DEDUPLICATE: if the same error (same message + same file) appears in multiple jobs, include it ONLY ONCE. Attribute it to whichever job is most relevant\n")
	b.WriteString("- For 'job': must be one of the failed job names listed above\n")

	return b.String()
}

// parseTriageResponse extracts the JSON array from the LLM response text.
func parseTriageResponse(text string) ([]triageResponse, error) {
	text = strings.TrimSpace(text)

	// Strip markdown code fences if present.
	if strings.HasPrefix(text, "```") {
		if idx := strings.Index(text, "\n"); idx >= 0 {
			text = text[idx+1:]
		}
		if idx := strings.LastIndex(text, "```"); idx >= 0 {
			text = text[:idx]
		}
		text = strings.TrimSpace(text)
	}

	// Find the JSON array boundaries.
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array found in response")
	}
	jsonStr := text[start : end+1]

	var items []triageResponse
	if err := json.Unmarshal([]byte(jsonStr), &items); err != nil {
		return nil, fmt.Errorf("unmarshal triage JSON: %w", err)
	}
	return items, nil
}
