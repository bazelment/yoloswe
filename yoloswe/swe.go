// Package yoloswe implements a builder-reviewer loop for autonomous software engineering.
//
// # Architecture
//
// The package orchestrates two AI agents in an iterative loop:
//   - Builder: Claude SDK session that implements code changes
//   - Reviewer: Codex SDK session that reviews changes and provides feedback
//
// # Flow
//
//  1. Builder receives initial prompt and implements changes
//  2. Reviewer examines the changes and returns a verdict (accepted/rejected) with feedback
//  3. If rejected, feedback is sent back to builder for another iteration
//  4. Loop continues until accepted or limits reached (budget/time/iterations)
//
// # Safety and Limits
//
// Multiple safety mechanisms prevent runaway execution:
//   - Budget limit: Hard cap on builder API costs
//   - Time limit: Wall-clock timeout for entire session
//   - Iteration limit: Maximum number of builder-reviewer cycles
//   - Context cancellation: Proper Ctrl+C handling with cleanup
//
// # Key Features
//
//   - Auto-approval mode: Builder autonomously executes tools without user prompts
//   - Robust parsing: Handles various JSON response formats from reviewer
//   - Error recovery: Graceful handling of session failures and network issues
//   - Input validation: Comprehensive validation of all configuration and prompts
//   - Session recording: Optional recording of all interactions for debugging
//
// # Example Usage
//
//	config := yoloswe.Config{
//	    BuilderModel:   "sonnet",
//	    ReviewerModel:  "gpt-5.2-codex",
//	    BuilderWorkDir: "/path/to/project",
//	    MaxBudgetUSD:   5.0,
//	    MaxTimeSeconds: 600,
//	    MaxIterations:  10,
//	}
//
//	swe := yoloswe.New(config)
//	ctx := context.Background()
//
//	if err := swe.Run(ctx, "Add unit tests for the authentication module"); err != nil {
//	    log.Fatal(err)
//	}
//
//	swe.PrintSummary()
//	stats := swe.Stats()
//	// Check stats.ExitReason to determine outcome
//
// # Exit Reasons
//
// The loop can exit for several reasons (see ExitReason constants):
//   - ExitReasonAccepted: Reviewer approved the changes (success)
//   - ExitReasonBudgetExceeded: Builder costs exceeded budget limit
//   - ExitReasonTimeExceeded: Wall-clock time exceeded timeout
//   - ExitReasonMaxIterations: Reached maximum iteration count
//   - ExitReasonError: Unrecoverable error occurred
//   - ExitReasonInterrupt: User cancelled with Ctrl+C
package yoloswe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

// ExitReason indicates why the builder-reviewer loop exited.
type ExitReason string

const (
	ExitReasonAccepted       ExitReason = "accepted"       // Reviewer approved the changes
	ExitReasonBudgetExceeded ExitReason = "budget"         // Builder budget limit reached
	ExitReasonTimeExceeded   ExitReason = "timeout"        // Time limit reached
	ExitReasonMaxIterations  ExitReason = "max_iterations" // Safety iteration limit reached
	ExitReasonError          ExitReason = "error"          // Unrecoverable error
	ExitReasonInterrupt      ExitReason = "interrupt"      // User interrupted (Ctrl+C)
)

// Config holds yoloswe configuration.
type Config struct {
	// Builder settings
	BuilderModel    string
	BuilderWorkDir  string
	RecordingDir    string
	SystemPrompt    string
	ResumeSessionID string // Resume from a previous session ID instead of starting fresh

	// Reviewer settings
	ReviewerModel string
	Goal          string // Goal description for reviewer context

	// Limits
	MaxBudgetUSD   float64 // Max USD to spend on builder session
	MaxTimeSeconds int     // Max wall-clock seconds
	MaxIterations  int     // Max builder-reviewer iterations (safety limit)

	// Other settings
	RequireApproval bool // Require user approval for tool executions (default: auto-approve)

	// Output settings
	Verbose bool
}

// Stats tracks cumulative statistics for the SWE loop.
type Stats struct {
	ExitReason        ExitReason
	BuilderCostUSD    float64
	BuilderTokensIn   int
	BuilderTokensOut  int
	ReviewerTokensIn  int64
	ReviewerTokensOut int64
	IterationCount    int
	TotalDurationMs   int64
}

// ReviewIssue represents a single issue found during review.
type ReviewIssue struct {
	Severity   string `json:"severity"`
	File       string `json:"file"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
	Line       int    `json:"line,omitempty"`
}

// ReviewVerdictJSON is the JSON structure returned by the reviewer.
type ReviewVerdictJSON struct {
	Verdict string        `json:"verdict"`
	Summary string        `json:"summary"`
	Issues  []ReviewIssue `json:"issues,omitempty"`
}

// ReviewVerdict represents the reviewer's decision.
type ReviewVerdict struct {
	Summary  string
	Feedback string // Formatted feedback for builder
	Issues   []ReviewIssue
	Accepted bool
}

// SWEWrapper orchestrates the builder-reviewer loop.
type SWEWrapper struct {
	startTime  time.Time
	output     io.Writer
	builder    *BuilderSession
	reviewer   *reviewer.Reviewer
	sessionLog string // Session log file path
	config     Config
	stats      Stats
}

// New creates a new SWEWrapper with the given configuration.
func New(config Config) *SWEWrapper {
	// Sanitize and apply defaults
	SanitizeConfig(&config)

	output := os.Stdout

	// Create builder session
	builderConfig := BuilderConfig{
		Model:           config.BuilderModel,
		WorkDir:         config.BuilderWorkDir,
		RecordingDir:    config.RecordingDir,
		SystemPrompt:    config.SystemPrompt,
		Verbose:         config.Verbose,
		RequireApproval: config.RequireApproval,
		ResumeSessionID: config.ResumeSessionID,
	}
	builder := NewBuilderSession(builderConfig, output)

	// Create reviewer with JSON output enabled for reliable parsing
	reviewerConfig := reviewer.Config{
		Model:          config.ReviewerModel,
		WorkDir:        config.BuilderWorkDir,
		Goal:           config.Goal,
		SessionLogPath: filepath.Join(config.RecordingDir, "reviewer.jsonl"),
		Verbose:        config.Verbose,
		JSONOutput:     true,
	}
	rev := reviewer.New(reviewerConfig)

	return &SWEWrapper{
		builder:    builder,
		reviewer:   rev,
		config:     config,
		output:     output,
		sessionLog: "",
		startTime:  time.Now(),
	}
}

// Run executes the builder-reviewer loop with the given initial prompt.
func (s *SWEWrapper) Run(ctx context.Context, prompt string) error {
	// Validate prompt
	if err := ValidatePrompt(prompt); err != nil {
		s.stats.ExitReason = ExitReasonError
		return fmt.Errorf("invalid prompt: %w", err)
	}

	// Initialize session log
	if err := s.initSessionLog(prompt); err != nil {
		fmt.Fprintf(s.output, "Warning: failed to initialize session log: %v\n", err)
	}

	startTime := time.Now()

	// Start builder session
	fmt.Fprintln(s.output, "\n=== Starting Builder Session ===")
	if err := s.builder.Start(ctx); err != nil {
		s.stats.ExitReason = ExitReasonError
		return fmt.Errorf("failed to start builder: %w", err)
	}
	s.logEvent("builder_started", map[string]interface{}{
		"model": s.config.BuilderModel,
	})
	defer func() {
		if err := s.builder.Stop(); err != nil {
			fmt.Fprintf(s.output, "Warning: error stopping builder: %v\n", err)
		}
	}()

	// Check if context was cancelled during builder start
	if ctx.Err() == context.Canceled {
		s.stats.ExitReason = ExitReasonInterrupt
		return nil
	}

	// Start reviewer
	fmt.Fprintln(s.output, "\n=== Starting Reviewer Session ===")
	if err := s.reviewer.Start(ctx); err != nil {
		s.stats.ExitReason = ExitReasonError
		return fmt.Errorf("failed to start reviewer: %w", err)
	}
	s.logEvent("reviewer_started", map[string]interface{}{
		"model": s.config.ReviewerModel,
	})
	defer func() {
		if err := s.reviewer.Stop(); err != nil {
			fmt.Fprintf(s.output, "Warning: error stopping reviewer: %v\n", err)
		}
	}()

	// Check if context was cancelled during reviewer start
	if ctx.Err() == context.Canceled {
		s.stats.ExitReason = ExitReasonInterrupt
		return nil
	}

	currentMessage := prompt
	isFirstReview := true

	for iteration := 1; ; iteration++ {
		s.stats.IterationCount = iteration

		// Check time limit before iteration
		elapsed := time.Since(startTime)
		if elapsed.Seconds() >= float64(s.config.MaxTimeSeconds) {
			s.stats.ExitReason = ExitReasonTimeExceeded
			fmt.Fprintf(s.output, "\n=== Time limit reached (%.1fs) ===\n", elapsed.Seconds())
			break
		}

		// === Builder Phase ===
		fmt.Fprint(s.output, "\n"+strings.Repeat("=", 60)+"\n")
		fmt.Fprintf(s.output, "=== Iteration %d: BUILDER ===\n", iteration)
		fmt.Fprint(s.output, strings.Repeat("=", 60)+"\n\n")

		builderUsage, err := s.builder.RunTurn(ctx, currentMessage)
		if err != nil {
			if ctx.Err() == context.Canceled {
				s.stats.ExitReason = ExitReasonInterrupt
				fmt.Fprintln(s.output, "\n=== Builder interrupted by user ===")
				return nil
			}
			s.stats.ExitReason = ExitReasonError
			fmt.Fprintf(s.output, "\n=== Builder failed: %v ===\n", err)
			return fmt.Errorf("builder error: %w", err)
		}

		// Update builder stats
		s.stats.BuilderCostUSD += builderUsage.CostUSD
		s.stats.BuilderTokensIn += builderUsage.InputTokens
		s.stats.BuilderTokensOut += builderUsage.OutputTokens

		// Check budget after builder turn
		if s.stats.BuilderCostUSD >= s.config.MaxBudgetUSD {
			s.stats.ExitReason = ExitReasonBudgetExceeded
			fmt.Fprintf(s.output, "\n=== Budget limit reached ($%.4f >= $%.4f) ===\n",
				s.stats.BuilderCostUSD, s.config.MaxBudgetUSD)
			break
		}

		// === Reviewer Phase ===
		fmt.Fprint(s.output, "\n"+strings.Repeat("=", 60)+"\n")
		fmt.Fprintf(s.output, "=== Iteration %d: REVIEWER ===\n", iteration)
		fmt.Fprint(s.output, strings.Repeat("=", 60)+"\n\n")

		var reviewResult *reviewer.ReviewResult
		if isFirstReview {
			reviewPrompt := s.buildInitialReviewPrompt()
			reviewResult, err = s.reviewer.ReviewWithResult(ctx, reviewPrompt)
			isFirstReview = false
		} else {
			reviewPrompt := s.buildFollowUpPrompt()
			reviewResult, err = s.reviewer.FollowUp(ctx, reviewPrompt)
		}

		if err != nil {
			if ctx.Err() == context.Canceled {
				s.stats.ExitReason = ExitReasonInterrupt
				fmt.Fprintln(s.output, "\n=== Reviewer interrupted by user ===")
				return nil
			}
			s.stats.ExitReason = ExitReasonError
			fmt.Fprintf(s.output, "\n=== Reviewer failed: %v ===\n", err)
			return fmt.Errorf("reviewer error: %w", err)
		}

		// Update reviewer stats
		s.stats.ReviewerTokensIn += reviewResult.InputTokens
		s.stats.ReviewerTokensOut += reviewResult.OutputTokens

		// Parse verdict from response
		verdict := s.parseVerdict(reviewResult.ResponseText)

		if verdict.Accepted {
			s.stats.ExitReason = ExitReasonAccepted
			fmt.Fprintln(s.output, "\n=== Reviewer ACCEPTED the changes ===")
			break
		}

		// Check iteration limit
		if iteration >= s.config.MaxIterations {
			s.stats.ExitReason = ExitReasonMaxIterations
			fmt.Fprintf(s.output, "\n=== Max iterations reached (%d) ===\n", s.config.MaxIterations)
			break
		}

		// Format feedback for next iteration
		fmt.Fprintln(s.output, "\n=== Reviewer requested changes, continuing... ===")
		currentMessage = fmt.Sprintf(`The reviewer provided the following feedback on your changes:

%s

Please address this feedback and improve the implementation.`, verdict.Feedback)
	}

	s.stats.TotalDurationMs = time.Since(startTime).Milliseconds()

	// Log session completion
	s.logEvent("session_complete", map[string]interface{}{
		"exit_reason":     s.stats.ExitReason,
		"iterations":      s.stats.IterationCount,
		"duration_ms":     s.stats.TotalDurationMs,
		"builder_cost":    s.stats.BuilderCostUSD,
		"builder_tokens":  s.stats.BuilderTokensIn + s.stats.BuilderTokensOut,
		"reviewer_tokens": s.stats.ReviewerTokensIn + s.stats.ReviewerTokensOut,
	})

	if s.sessionLog != "" {
		fmt.Fprintf(s.output, "\nSession log: %s\n", s.sessionLog)
	}

	return nil
}

// buildInitialReviewPrompt creates the prompt for the first review.
func (s *SWEWrapper) buildInitialReviewPrompt() string {
	return reviewer.BuildJSONPrompt(s.config.Goal)
}

// buildFollowUpPrompt creates the prompt for follow-up reviews.
func (s *SWEWrapper) buildFollowUpPrompt() string {
	return `The builder has made additional changes based on your previous feedback.

Please review the current state of the changes and provide an updated verdict.

Focus on whether the previous issues have been addressed correctly.

## Output Format
You MUST respond with valid JSON in this exact format:
{
  "verdict": "accepted" or "rejected",
  "summary": "Brief overall assessment of the changes",
  "issues": [
    {
      "severity": "critical|high|medium|low",
      "file": "path/to/file.go",
      "line": 42,
      "message": "Description of the issue",
      "suggestion": "How to fix it"
    }
  ]
}

## Rules
- verdict MUST be exactly "accepted" or "rejected"
- If there are any critical or high severity issues, verdict MUST be "rejected"
- issues array can be empty if verdict is "accepted"
- Output ONLY the JSON object, no other text`
}

// parseVerdict extracts the acceptance decision from the reviewer's JSON response.
func (s *SWEWrapper) parseVerdict(text string) *ReviewVerdict {
	if text == "" {
		return &ReviewVerdict{
			Accepted: false,
			Feedback: "Empty response from reviewer",
		}
	}

	jsonStr := extractJSON(text)
	if jsonStr == "" {
		return &ReviewVerdict{
			Accepted: false,
			Feedback: fmt.Sprintf("No JSON found in response: %s", truncateString(text, 200)),
		}
	}

	var result ReviewVerdictJSON
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return &ReviewVerdict{
			Accepted: false,
			Feedback: fmt.Sprintf("Failed to parse reviewer JSON: %v\nRaw response: %s", err, truncateString(text, 200)),
		}
	}

	// Trim whitespace from verdict for robust comparison
	verdict := strings.TrimSpace(result.Verdict)
	accepted := strings.EqualFold(verdict, "accepted")

	return &ReviewVerdict{
		Accepted: accepted,
		Summary:  result.Summary,
		Issues:   result.Issues,
		Feedback: formatFeedback(result),
	}
}

// truncateString truncates a string to maxLen characters, adding "..." if truncated.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// extractJSON attempts to extract JSON from a string that might contain markdown.
// This function handles multiple formats commonly seen in LLM responses:
//
// Supported formats:
//  1. JSON in markdown code blocks: ```json\n{...}\n```
//  2. JSON in generic code blocks: ```\n{...}\n```
//  3. Raw JSON embedded in text: "Here is the result: {...}"
//  4. Plain JSON without surrounding text
//
// The function prioritizes code blocks over inline JSON and returns the first
// complete JSON structure found (either object {...} or array [...]).
//
// For inline JSON, it properly handles:
//   - Nested objects and arrays
//   - Escaped characters within strings
//   - String boundaries (ignores braces inside strings)
//
// Returns the original text unchanged if no valid JSON structure is found.
func extractJSON(text string) string {
	if text == "" {
		return ""
	}

	// Check if the text contains a JSON code block
	if idx := strings.Index(text, "```json"); idx != -1 {
		start := idx + 7
		end := strings.Index(text[start:], "```")
		if end != -1 {
			return strings.TrimSpace(text[start : start+end])
		}
	}

	// Check for generic code block
	if idx := strings.Index(text, "```"); idx != -1 {
		start := idx + 3
		if newline := strings.Index(text[start:], "\n"); newline != -1 {
			start += newline + 1
		}
		end := strings.Index(text[start:], "```")
		if end != -1 {
			return strings.TrimSpace(text[start : start+end])
		}
	}

	// Try to find JSON object directly (handles both objects and arrays)
	startChars := []byte{'{', '['}
	for _, startChar := range startChars {
		if idx := strings.IndexByte(text, startChar); idx != -1 {
			extracted := extractBalancedJSON(text, idx, startChar)
			if extracted != "" {
				return extracted
			}
		}
	}

	return text
}

// extractBalancedJSON extracts a balanced JSON structure starting at idx.
// This function correctly handles:
//   - Nested braces/brackets of arbitrary depth
//   - Escaped characters (\", \\, etc.) within strings
//   - Braces and brackets inside string values
//
// Parameters:
//   - text: The text to extract from
//   - idx: Starting index of the JSON structure
//   - startChar: Either '{' for objects or '[' for arrays
//
// Returns:
//   - The complete balanced JSON structure including start and end characters
//   - Empty string if the structure is incomplete (unbalanced)
//
// Algorithm:
//   - Maintains depth counter for nested structures
//   - Tracks string state to ignore structural characters in strings
//   - Handles escape sequences correctly
func extractBalancedJSON(text string, idx int, startChar byte) string {
	var endChar byte
	if startChar == '{' {
		endChar = '}'
	} else {
		endChar = ']'
	}

	depth := 0
	inString := false
	escaped := false

	for i := idx; i < len(text); i++ {
		char := text[i]

		// Handle escape sequences in strings
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' {
			escaped = true
			continue
		}

		// Handle string boundaries
		if char == '"' {
			inString = !inString
			continue
		}

		// Only count braces/brackets outside of strings
		if !inString {
			if char == startChar {
				depth++
			} else if char == endChar {
				depth--
				if depth == 0 {
					return text[idx : i+1]
				}
			}
		}
	}

	return ""
}

// formatFeedback formats the JSON verdict into a readable feedback string for the builder.
func formatFeedback(result ReviewVerdictJSON) string {
	if len(result.Issues) == 0 {
		return result.Summary
	}

	var sb strings.Builder
	sb.WriteString(result.Summary)
	sb.WriteString("\n\nIssues to address:\n")

	for i, issue := range result.Issues {
		sb.WriteString(fmt.Sprintf("\n%d. [%s] %s", i+1, strings.ToUpper(issue.Severity), issue.Message))
		if issue.File != "" {
			sb.WriteString(fmt.Sprintf("\n   File: %s", issue.File))
			if issue.Line > 0 {
				sb.WriteString(fmt.Sprintf(":%d", issue.Line))
			}
		}
		if issue.Suggestion != "" {
			sb.WriteString(fmt.Sprintf("\n   Suggestion: %s", issue.Suggestion))
		}
	}

	return sb.String()
}

// PrintSummary prints the final statistics.
func (s *SWEWrapper) PrintSummary() {
	fmt.Fprintln(s.output, "\n"+strings.Repeat("=", 60))
	fmt.Fprintln(s.output, "YOLOSWE SESSION SUMMARY")
	fmt.Fprintln(s.output, strings.Repeat("-", 60))
	fmt.Fprintf(s.output, "Exit reason:        %s\n", s.stats.ExitReason)
	fmt.Fprintf(s.output, "Iterations:         %d\n", s.stats.IterationCount)
	fmt.Fprintf(s.output, "Duration:           %.1fs\n", float64(s.stats.TotalDurationMs)/1000)
	fmt.Fprintln(s.output, strings.Repeat("-", 60))
	fmt.Fprintln(s.output, "Builder:")
	fmt.Fprintf(s.output, "  Cost:             $%.4f\n", s.stats.BuilderCostUSD)
	fmt.Fprintf(s.output, "  Input tokens:     %d\n", s.stats.BuilderTokensIn)
	fmt.Fprintf(s.output, "  Output tokens:    %d\n", s.stats.BuilderTokensOut)
	fmt.Fprintln(s.output, strings.Repeat("-", 60))
	fmt.Fprintln(s.output, "Reviewer:")
	fmt.Fprintf(s.output, "  Input tokens:     %d\n", s.stats.ReviewerTokensIn)
	fmt.Fprintf(s.output, "  Output tokens:    %d\n", s.stats.ReviewerTokensOut)
	fmt.Fprintln(s.output, strings.Repeat("=", 60))
}

// Stats returns the current statistics.
func (s *SWEWrapper) Stats() Stats {
	return s.stats
}

// initSessionLog initializes the session log file.
func (s *SWEWrapper) initSessionLog(prompt string) error {
	// Ensure recording directory exists
	if err := os.MkdirAll(s.config.RecordingDir, 0755); err != nil {
		return fmt.Errorf("failed to create recording directory: %w", err)
	}

	// Create session log file with timestamp
	timestamp := time.Now().Format("20060102-150405")
	logFilename := fmt.Sprintf("session-%s.log", timestamp)
	logPath := filepath.Join(s.config.RecordingDir, logFilename)

	s.sessionLog = logPath

	// Write initial log entry
	entry := map[string]interface{}{
		"timestamp":  time.Now().Format(time.RFC3339),
		"event":      "session_started",
		"goal":       s.config.Goal,
		"builder":    s.config.BuilderModel,
		"reviewer":   s.config.ReviewerModel,
		"budget":     s.config.MaxBudgetUSD,
		"timeout":    s.config.MaxTimeSeconds,
		"iterations": s.config.MaxIterations,
	}
	return s.appendLogEntry(entry)
}

// logEvent appends an event to the session log.
func (s *SWEWrapper) logEvent(event string, data map[string]interface{}) {
	if s.sessionLog == "" {
		return
	}

	entry := map[string]interface{}{
		"timestamp": time.Now().Format(time.RFC3339),
		"event":     event,
	}

	// Merge data into entry
	for k, v := range data {
		entry[k] = v
	}

	if err := s.appendLogEntry(entry); err != nil {
		fmt.Fprintf(s.output, "Warning: failed to log event: %v\n", err)
	}
}

// appendLogEntry appends a JSON log entry to the session log file.
func (s *SWEWrapper) appendLogEntry(entry map[string]interface{}) error {
	jsonData, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(s.sessionLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(string(jsonData) + "\n")
	return err
}
