package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/medivac/issue"
)

// LevelTrace matches engine.LevelTrace for prompt/response logging at -vv.
const LevelTrace slog.Level = slog.LevelDebug - 4

// TriageConfig controls LLM triage behavior.
type TriageConfig struct {
	Query  QueryFn      // injectable for testing; defaults to claude.Query
	Logger *slog.Logger // optional; nil = slog.Default()
	Model  string       // Claude model (default "haiku")
}

// QueryFn is the signature for one-shot LLM queries.
type QueryFn func(ctx context.Context, prompt string, opts ...claude.SessionOption) (*claude.QueryResult, error)

// triageResponse is one element of the JSON array returned by the LLM.
type triageResponse struct {
	Category  string `json:"category"`
	Job       string `json:"job"`
	File      string `json:"file"`
	Summary   string `json:"summary"`
	Details   string `json:"details"`
	ErrorCode string `json:"error_code"`
	RunID     int64  `json:"run_id"`
	Line      int    `json:"line"`
}

// TriageRun sends all metadata for a single workflow run (annotations, failed
// jobs, cleaned log) to the LLM in a single call and returns deduplicated
// issue.CIFailure results plus the cost incurred.
func TriageRun(
	ctx context.Context,
	run WorkflowRun,
	failedJobs []JobResult,
	annotations []Annotation,
	log string,
	config TriageConfig,
) ([]issue.CIFailure, float64, error) {
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

	failures := make([]issue.CIFailure, 0, len(items))
	seen := make(map[string]bool)

	for _, item := range items {
		cat := issue.FailureCategory(item.Category)
		if !issue.ValidCategories[cat] {
			cat = issue.CategoryUnknown
		}
		jobName := item.Job
		if !jobNames[jobName] {
			jobName = defaultJob
		}
		f := issue.CIFailure{
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
		f.Signature = issue.ComputeSignature(f.Category, f.File, f.Summary, f.JobName, f.Details)

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
	b.WriteString("- For 'summary': use the EXACT error message from the log when possible (e.g. the compiler error text). Do NOT paraphrase or reword — deterministic summaries enable deduplication across runs\n")
	b.WriteString("- For 'file': normalize ALL paths to be relative to the repository root. If a log shows a path like 'src/foo.tsx' inside a Docker build context for a subdirectory (e.g. 'services/typescript/forge-v2'), expand it to the full repo-root-relative path (e.g. 'services/typescript/forge-v2/src/foo.tsx'). Use the workflow name and job context to determine the correct prefix.\n")
	b.WriteString("- ENUMERATE: list EVERY individual error with its own file and line number as a separate entry. Do NOT summarize or group multiple errors into one entry. For example, if the log shows 10 TypeScript errors in 8 files, return 10 entries, not 1 summary.\n")
	b.WriteString("- DEDUPLICATE: if the EXACT same error (same message + same file + same line) appears in multiple jobs, include it ONLY ONCE. Attribute it to whichever job is most relevant\n")
	b.WriteString("- For 'job': must be one of the failed job names listed above\n")
	b.WriteString("- For 'details': include the error code if present (e.g. TS7006, TS2307, SA1019). This is critical for downstream grouping.\n")

	return b.String()
}

// RunData holds the collected metadata and trimmed log for a single CI run.
// No LLM call is involved — this is pure data gathering.
type RunData struct {
	FailedJobs  []JobResult
	Annotations []Annotation
	Log         string // cleaned + trimmed log (first/last N lines)
	Run         WorkflowRun
}

// BatchTriageResult holds the output of batch triage.
type BatchTriageResult struct {
	Failures []issue.CIFailure
	Cost     float64
}

// TriageBatch sends all collected run data to the LLM in a single call.
// It extracts and deduplicates failures across all runs at once.
func TriageBatch(
	ctx context.Context,
	runs []RunData,
	config TriageConfig,
) (*BatchTriageResult, error) {
	if len(runs) == 0 {
		return &BatchTriageResult{}, nil
	}

	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	model := config.Model
	if model == "" {
		model = "haiku"
	}
	queryFn := config.Query
	if queryFn == nil {
		queryFn = claude.Query
	}

	prompt := buildBatchTriagePrompt(runs)
	logger.Debug("built triage prompt", "runs", len(runs), "promptChars", len(prompt))
	logger.Log(ctx, LevelTrace, "triage prompt", "content", prompt)

	result, err := queryFn(ctx, prompt, claude.WithModel(model))
	if err != nil {
		return nil, fmt.Errorf("batch triage query: %w", err)
	}

	logger.Debug("triage response received", "responseChars", len(result.Text), "cost", result.Usage.CostUSD)
	logger.Log(ctx, LevelTrace, "triage response", "text", result.Text)

	items, err := parseTriageResponse(result.Text)
	if err != nil {
		return nil, fmt.Errorf("parse batch triage response: %w", err)
	}
	logger.Debug("parsed triage items", "count", len(items))

	// Build valid job name set across all runs.
	jobNames := make(map[string]bool)
	defaultJob := ""
	for i := range runs {
		for _, j := range runs[i].FailedJobs {
			jobNames[j.Name] = true
			if defaultJob == "" {
				defaultJob = j.Name
			}
		}
	}

	// Build run lookup by ID for correct metadata attribution.
	runByID := make(map[int64]WorkflowRun, len(runs))
	for i := range runs {
		runByID[runs[i].Run.ID] = runs[i].Run
	}
	defaultRun := runs[0].Run

	failures := make([]issue.CIFailure, 0, len(items))
	seen := make(map[string]bool)

	for _, item := range items {
		cat := issue.FailureCategory(item.Category)
		if !issue.ValidCategories[cat] {
			cat = issue.CategoryUnknown
		}
		jobName := item.Job
		if !jobNames[jobName] {
			jobName = defaultJob
		}

		// Use the run metadata from the LLM-reported run_id, falling back to the first run.
		run := defaultRun
		if item.RunID != 0 {
			if r, ok := runByID[item.RunID]; ok {
				run = r
			}
		}

		f := issue.CIFailure{
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
		f.Signature = issue.ComputeSignature(f.Category, f.File, f.Summary, f.JobName, f.Details)

		if !seen[f.Signature] {
			seen[f.Signature] = true
			failures = append(failures, f)
		}
	}

	return &BatchTriageResult{
		Failures: failures,
		Cost:     result.Usage.CostUSD,
	}, nil
}

// buildBatchTriagePrompt constructs a single prompt containing all runs' logs
// for combined extraction and deduplication in one LLM call.
func buildBatchTriagePrompt(runs []RunData) string {
	var b strings.Builder

	b.WriteString("You are a CI failure triage system. Analyze the following CI runs and extract structured failure information.\n")
	b.WriteString("Multiple runs may contain the same errors — deduplicate across runs.\n\n")

	for i := range runs {
		rd := &runs[i]
		b.WriteString(fmt.Sprintf("## Run %d: %s (ID: %d, SHA: %s)\n\n", i+1, rd.Run.Name, rd.Run.ID, rd.Run.HeadSHA))
		b.WriteString(fmt.Sprintf("- Branch: %s\n", rd.Run.HeadBranch))

		b.WriteString(fmt.Sprintf("- Failed jobs (%d):\n", len(rd.FailedJobs)))
		for _, job := range rd.FailedJobs {
			b.WriteString(fmt.Sprintf("  - %s\n", job.Name))
		}

		if len(rd.Annotations) > 0 {
			b.WriteString("\nAnnotations:\n")
			for _, ann := range rd.Annotations {
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

		b.WriteString("\nLog:\n```\n")
		b.WriteString(rd.Log)
		b.WriteString("\n```\n\n")
	}

	b.WriteString("## Instructions\n\n")
	b.WriteString("Extract each distinct failure across ALL runs above. Return a single deduplicated JSON array (no markdown fences, just raw JSON).\n")
	b.WriteString("Each element should have these fields:\n\n")
	b.WriteString("```json\n")
	b.WriteString(`[{
  "run_id": 12345,
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
	b.WriteString("- For 'run_id': use the numeric Run ID from the run header above where this error was found\n")
	b.WriteString("- If no clear errors, return an empty array []\n")
	b.WriteString("- Keep summary under 100 characters\n")
	b.WriteString("- Keep details under 500 characters\n")
	b.WriteString("- For 'summary': use the EXACT error message from the log when possible. Do NOT paraphrase\n")
	b.WriteString("- For 'file': normalize ALL paths to be relative to the repository root. Expand Docker build context paths (e.g. 'src/foo.tsx' -> 'services/typescript/forge-v2/src/foo.tsx')\n")
	b.WriteString("- ENUMERATE: list EVERY individual error as a separate entry. Do NOT summarize multiple errors into one\n")
	b.WriteString("- DEDUPLICATE: if the same error appears across multiple runs, include it ONLY ONCE. Pick the version with the most complete file path and details\n")
	b.WriteString("- For 'job': must be one of the failed job names listed above\n")
	b.WriteString("- For 'details': include the error code if present (e.g. TS7006, TS2307). Critical for downstream grouping\n")

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
