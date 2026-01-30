package yoloswe

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

func TestNew(t *testing.T) {
	// Get expected default recording dir
	homeDir, _ := os.UserHomeDir()
	defaultRecordingDir := filepath.Join(homeDir, ".yoloswe")

	tests := []struct {
		name               string
		expectedBModel     string
		expectedRModel     string
		expectedRecDir     string
		config             Config
		expectedBudget     float64
		expectedTimeout    int
		expectedIterations int
	}{
		{
			name:               "default values",
			config:             Config{},
			expectedBModel:     "sonnet",
			expectedRModel:     "gpt-5.2-codex",
			expectedRecDir:     defaultRecordingDir,
			expectedBudget:     100.0,
			expectedTimeout:    3600,
			expectedIterations: 10,
		},
		{
			name: "custom values",
			config: Config{
				BuilderModel:   "opus",
				ReviewerModel:  "o4-mini",
				RecordingDir:   "/custom",
				MaxBudgetUSD:   10.0,
				MaxTimeSeconds: 1800,
				MaxIterations:  20,
			},
			expectedBModel:     "opus",
			expectedRModel:     "o4-mini",
			expectedRecDir:     "/custom",
			expectedBudget:     10.0,
			expectedTimeout:    1800,
			expectedIterations: 20,
		},
		{
			name: "zero values use defaults",
			config: Config{
				MaxBudgetUSD:   0,
				MaxTimeSeconds: 0,
				MaxIterations:  0,
			},
			expectedBModel:     "sonnet",
			expectedRModel:     "gpt-5.2-codex",
			expectedRecDir:     defaultRecordingDir,
			expectedBudget:     100.0,
			expectedTimeout:    3600,
			expectedIterations: 10,
		},
		{
			name: "negative values use defaults",
			config: Config{
				MaxBudgetUSD:   -1.0,
				MaxTimeSeconds: -100,
				MaxIterations:  -5,
			},
			expectedBModel:     "sonnet",
			expectedRModel:     "gpt-5.2-codex",
			expectedRecDir:     defaultRecordingDir,
			expectedBudget:     100.0,
			expectedTimeout:    3600,
			expectedIterations: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			swe := New(tt.config)

			if swe.config.BuilderModel != tt.expectedBModel {
				t.Errorf("expected builder model %q, got %q", tt.expectedBModel, swe.config.BuilderModel)
			}
			if swe.config.ReviewerModel != tt.expectedRModel {
				t.Errorf("expected reviewer model %q, got %q", tt.expectedRModel, swe.config.ReviewerModel)
			}
			if swe.config.RecordingDir != tt.expectedRecDir {
				t.Errorf("expected recording dir %q, got %q", tt.expectedRecDir, swe.config.RecordingDir)
			}
			if swe.config.MaxBudgetUSD != tt.expectedBudget {
				t.Errorf("expected budget %.2f, got %.2f", tt.expectedBudget, swe.config.MaxBudgetUSD)
			}
			if swe.config.MaxTimeSeconds != tt.expectedTimeout {
				t.Errorf("expected timeout %d, got %d", tt.expectedTimeout, swe.config.MaxTimeSeconds)
			}
			if swe.config.MaxIterations != tt.expectedIterations {
				t.Errorf("expected iterations %d, got %d", tt.expectedIterations, swe.config.MaxIterations)
			}
			if swe.builder == nil {
				t.Error("builder not initialized")
			}
			if swe.reviewer == nil {
				t.Error("reviewer not initialized")
			}
			if swe.output == nil {
				t.Error("output not initialized")
			}
		})
	}
}

func TestParseVerdict(t *testing.T) {
	swe := New(Config{})

	tests := []struct {
		name             string
		text             string
		expectedSummary  string
		expectedIssues   int
		expectedAccepted bool
		expectFeedback   bool
	}{
		{
			name:             "JSON accepted - no issues",
			text:             `{"verdict": "accepted", "summary": "All changes look good", "issues": []}`,
			expectedAccepted: true,
			expectedSummary:  "All changes look good",
			expectedIssues:   0,
			expectFeedback:   true,
		},
		{
			name:             "JSON accepted - uppercase verdict",
			text:             `{"verdict": "ACCEPTED", "summary": "Changes approved", "issues": []}`,
			expectedAccepted: true,
			expectedSummary:  "Changes approved",
			expectedIssues:   0,
			expectFeedback:   true,
		},
		{
			name:             "JSON rejected - with issues",
			text:             `{"verdict": "rejected", "summary": "Needs fixes", "issues": [{"severity": "high", "file": "main.go", "line": 10, "message": "Missing error handling"}]}`,
			expectedAccepted: false,
			expectedSummary:  "Needs fixes",
			expectedIssues:   1,
			expectFeedback:   true,
		},
		{
			name:             "JSON in markdown code block",
			text:             "```json\n{\"verdict\": \"accepted\", \"summary\": \"LGTM\", \"issues\": []}\n```",
			expectedAccepted: true,
			expectedSummary:  "LGTM",
			expectedIssues:   0,
			expectFeedback:   true,
		},
		{
			name:             "JSON with surrounding text",
			text:             "Here is my review:\n{\"verdict\": \"rejected\", \"summary\": \"Issues found\", \"issues\": [{\"severity\": \"critical\", \"file\": \"api.go\", \"message\": \"SQL injection\"}]}\nThanks!",
			expectedAccepted: false,
			expectedSummary:  "Issues found",
			expectedIssues:   1,
			expectFeedback:   true,
		},
		{
			name:             "empty text",
			text:             "",
			expectedAccepted: false,
			expectedSummary:  "",
			expectedIssues:   0,
			expectFeedback:   true,
		},
		{
			name:             "invalid JSON - parse error",
			text:             "This is not valid JSON",
			expectedAccepted: false,
			expectedSummary:  "",
			expectedIssues:   0,
			expectFeedback:   true,
		},
		{
			name:             "malformed JSON",
			text:             `{"verdict": "accepted", "summary": }`,
			expectedAccepted: false,
			expectedSummary:  "",
			expectedIssues:   0,
			expectFeedback:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verdict := swe.parseVerdict(tt.text)

			if verdict.Accepted != tt.expectedAccepted {
				t.Errorf("expected accepted=%v, got %v", tt.expectedAccepted, verdict.Accepted)
			}

			if verdict.Summary != tt.expectedSummary {
				t.Errorf("expected summary=%q, got %q", tt.expectedSummary, verdict.Summary)
			}

			if len(verdict.Issues) != tt.expectedIssues {
				t.Errorf("expected %d issues, got %d", tt.expectedIssues, len(verdict.Issues))
			}

			if tt.expectFeedback && verdict.Feedback == "" {
				t.Error("expected feedback but got empty string")
			}
		})
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain JSON",
			input:    `{"verdict": "accepted"}`,
			expected: `{"verdict": "accepted"}`,
		},
		{
			name:     "JSON in markdown code block",
			input:    "```json\n{\"verdict\": \"accepted\"}\n```",
			expected: `{"verdict": "accepted"}`,
		},
		{
			name:     "JSON in generic code block",
			input:    "```\n{\"verdict\": \"accepted\"}\n```",
			expected: `{"verdict": "accepted"}`,
		},
		{
			name:     "JSON with surrounding text",
			input:    "Here is the result:\n{\"verdict\": \"accepted\"}\nDone!",
			expected: `{"verdict": "accepted"}`,
		},
		{
			name:     "no JSON",
			input:    "Just plain text",
			expected: "Just plain text",
		},
		{
			name:     "nested JSON objects",
			input:    `{"outer": {"inner": {"verdict": "accepted"}}}`,
			expected: `{"outer": {"inner": {"verdict": "accepted"}}}`,
		},
		{
			name:     "JSON with escaped characters",
			input:    `{"verdict": "accepted", "message": "All \"tests\" passed"}`,
			expected: `{"verdict": "accepted", "message": "All \"tests\" passed"}`,
		},
		{
			name:  "JSON array - extracts first object within",
			input: `[{"verdict": "accepted"}, {"verdict": "rejected"}]`,
			// When we have an array, we find the first object inside it
			expected: `{"verdict": "accepted"}`,
		},
		{
			name:     "incomplete JSON",
			input:    `{"verdict": "accepted"`,
			expected: `{"verdict": "accepted"`,
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "JSON with only opening brace",
			input:    "{",
			expected: "{",
		},
		{
			name:     "multiple JSON objects, returns first",
			input:    `{"first": 1} {"second": 2}`,
			expected: `{"first": 1}`,
		},
		{
			name:     "JSON in code block with language",
			input:    "```typescript\n{\"verdict\": \"accepted\"}\n```",
			expected: `{"verdict": "accepted"}`,
		},
		{
			name:     "whitespace before and after JSON",
			input:    "  \n\n  {\"verdict\": \"accepted\"}  \n\n  ",
			expected: `{"verdict": "accepted"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractJSON(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestFormatFeedback(t *testing.T) {
	tests := []struct {
		name        string
		verdict     ReviewVerdictJSON
		shouldMatch []string
	}{
		{
			name: "no issues",
			verdict: ReviewVerdictJSON{
				Summary: "All good",
				Issues:  nil,
			},
			shouldMatch: []string{"All good"},
		},
		{
			name: "with issues",
			verdict: ReviewVerdictJSON{
				Summary: "Found problems",
				Issues: []ReviewIssue{
					{Severity: "high", File: "main.go", Line: 10, Message: "Missing error check", Suggestion: "Add error handling"},
				},
			},
			shouldMatch: []string{"Found problems", "[HIGH]", "Missing error check", "main.go:10", "Add error handling"},
		},
		{
			name: "multiple issues",
			verdict: ReviewVerdictJSON{
				Summary: "Multiple issues",
				Issues: []ReviewIssue{
					{Severity: "critical", File: "api.go", Message: "SQL injection"},
					{Severity: "medium", File: "util.go", Line: 5, Message: "Unused variable"},
				},
			},
			shouldMatch: []string{"Multiple issues", "[CRITICAL]", "SQL injection", "[MEDIUM]", "Unused variable"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatFeedback(tt.verdict)
			for _, match := range tt.shouldMatch {
				if !strings.Contains(result, match) {
					t.Errorf("expected feedback to contain %q, got: %s", match, result)
				}
			}
		})
	}
}

func TestBuildInitialReviewPrompt(t *testing.T) {
	tests := []struct {
		name        string
		goal        string
		shouldMatch string
	}{
		{
			name:        "with goal",
			goal:        "implement user authentication",
			shouldMatch: "implement user authentication",
		},
		{
			name:        "empty goal",
			goal:        "",
			shouldMatch: "(not specified)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			swe := New(Config{Goal: tt.goal})
			prompt := swe.buildInitialReviewPrompt()

			if !strings.Contains(prompt, tt.shouldMatch) {
				t.Errorf("expected prompt to contain %q, got: %q", tt.shouldMatch, prompt)
			}

			// Check for key review areas
			expectedSections := []string{
				"correct",
				"test coverage",
				"maintainability",
				"performance",
				"security",
			}
			for _, section := range expectedSections {
				if !strings.Contains(strings.ToLower(prompt), section) {
					t.Errorf("expected prompt to contain %q", section)
				}
			}

			// Check for JSON output format specification
			jsonSections := []string{
				"verdict",
				"summary",
				"issues",
				"JSON",
			}
			for _, section := range jsonSections {
				if !strings.Contains(prompt, section) {
					t.Errorf("expected JSON prompt to contain %q", section)
				}
			}
		})
	}
}

func TestBuildFollowUpPrompt(t *testing.T) {
	swe := New(Config{})
	prompt := swe.buildFollowUpPrompt()

	expectedPhrases := []string{
		"additional changes",
		"previous feedback",
		"verdict",
		"JSON",
		"accepted",
		"rejected",
	}

	for _, phrase := range expectedPhrases {
		if !strings.Contains(strings.ToLower(prompt), strings.ToLower(phrase)) {
			t.Errorf("expected follow-up prompt to contain %q, got: %q", phrase, prompt)
		}
	}
}

func TestStats(t *testing.T) {
	swe := New(Config{})

	// Initial stats should be zero
	stats := swe.Stats()
	if stats.BuilderCostUSD != 0 {
		t.Errorf("expected initial cost 0, got %.4f", stats.BuilderCostUSD)
	}
	if stats.IterationCount != 0 {
		t.Errorf("expected initial iterations 0, got %d", stats.IterationCount)
	}
	if stats.ExitReason != "" {
		t.Errorf("expected initial exit reason empty, got %q", stats.ExitReason)
	}

	// Modify stats and check
	swe.stats.BuilderCostUSD = 1.5
	swe.stats.IterationCount = 3
	swe.stats.ExitReason = ExitReasonAccepted

	stats = swe.Stats()
	if stats.BuilderCostUSD != 1.5 {
		t.Errorf("expected cost 1.5, got %.4f", stats.BuilderCostUSD)
	}
	if stats.IterationCount != 3 {
		t.Errorf("expected iterations 3, got %d", stats.IterationCount)
	}
	if stats.ExitReason != ExitReasonAccepted {
		t.Errorf("expected exit reason %q, got %q", ExitReasonAccepted, stats.ExitReason)
	}
}

func TestPrintSummary(t *testing.T) {
	tests := []struct {
		name       string
		shouldFind []string
		stats      Stats
	}{
		{
			name: "accepted with costs",
			stats: Stats{
				ExitReason:        ExitReasonAccepted,
				IterationCount:    3,
				TotalDurationMs:   45000,
				BuilderCostUSD:    2.50,
				BuilderTokensIn:   1000,
				BuilderTokensOut:  2000,
				ReviewerTokensIn:  500,
				ReviewerTokensOut: 1500,
			},
			shouldFind: []string{
				"accepted",
				"3",
				"45.0s",
				"$2.5000",
				"1000",
				"2000",
				"500",
				"1500",
			},
		},
		{
			name: "budget exceeded",
			stats: Stats{
				ExitReason:      ExitReasonBudgetExceeded,
				IterationCount:  5,
				TotalDurationMs: 30000,
				BuilderCostUSD:  5.0,
			},
			shouldFind: []string{
				"budget",
				"5",
				"30.0s",
				"$5.0000",
			},
		},
		{
			name: "timeout",
			stats: Stats{
				ExitReason:      ExitReasonTimeExceeded,
				IterationCount:  2,
				TotalDurationMs: 600000,
			},
			shouldFind: []string{
				"timeout",
				"2",
				"600.0s",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			swe := New(Config{})
			swe.output = &buf
			swe.stats = tt.stats

			swe.PrintSummary()

			output := buf.String()
			for _, text := range tt.shouldFind {
				if !strings.Contains(output, text) {
					t.Errorf("expected summary to contain %q, got:\n%s", text, output)
				}
			}

			// Check for summary structure
			if !strings.Contains(output, "YOLOSWE SESSION SUMMARY") {
				t.Error("expected summary header")
			}
			if !strings.Contains(output, "Builder:") {
				t.Error("expected builder section")
			}
			if !strings.Contains(output, "Reviewer:") {
				t.Error("expected reviewer section")
			}
		})
	}
}

func TestExitReasonConstants(t *testing.T) {
	// Ensure all exit reasons are defined and unique
	reasons := []ExitReason{
		ExitReasonAccepted,
		ExitReasonBudgetExceeded,
		ExitReasonTimeExceeded,
		ExitReasonMaxIterations,
		ExitReasonError,
		ExitReasonInterrupt,
	}

	seen := make(map[ExitReason]bool)
	for _, reason := range reasons {
		if reason == "" {
			t.Error("exit reason should not be empty")
		}
		if seen[reason] {
			t.Errorf("duplicate exit reason: %q", reason)
		}
		seen[reason] = true
	}
}

// TestRunEdgeCases tests edge cases in the Run method
// Note: These are unit tests for logic, not integration tests
func TestRunEdgeCases(t *testing.T) {
	t.Run("immediate timeout", func(t *testing.T) {
		// Test that a zero timeout is handled correctly
		swe := New(Config{
			MaxTimeSeconds: 0, // Will be defaulted to 3600
		})
		if swe.config.MaxTimeSeconds != 3600 {
			t.Errorf("expected default timeout 3600, got %d", swe.config.MaxTimeSeconds)
		}
	})

	t.Run("immediate budget exceeded", func(t *testing.T) {
		// Test that a zero budget is handled correctly
		swe := New(Config{
			MaxBudgetUSD: 0, // Will be defaulted to 100.0
		})
		if swe.config.MaxBudgetUSD != 100.0 {
			t.Errorf("expected default budget 100.0, got %.2f", swe.config.MaxBudgetUSD)
		}
	})
}

func TestTruncateString(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
		maxLen   int
	}{
		{
			name:     "short string",
			input:    "hello",
			maxLen:   10,
			expected: "hello",
		},
		{
			name:     "exact length",
			input:    "hello",
			maxLen:   5,
			expected: "hello",
		},
		{
			name:     "needs truncation",
			input:    "hello world",
			maxLen:   8,
			expected: "hello...",
		},
		{
			name:     "very short maxLen",
			input:    "hello",
			maxLen:   3,
			expected: "hel",
		},
		{
			name:     "maxLen of 1",
			input:    "hello",
			maxLen:   1,
			expected: "h",
		},
		{
			name:     "empty string",
			input:    "",
			maxLen:   10,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateString(tt.input, tt.maxLen)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
			if len(result) > tt.maxLen {
				t.Errorf("result length %d exceeds maxLen %d", len(result), tt.maxLen)
			}
		})
	}
}

func TestExtractBalancedJSON(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		expected  string
		idx       int
		startChar byte
	}{
		{
			name:      "simple object",
			text:      `{"key": "value"}`,
			idx:       0,
			startChar: '{',
			expected:  `{"key": "value"}`,
		},
		{
			name:      "nested objects",
			text:      `{"outer": {"inner": "value"}}`,
			idx:       0,
			startChar: '{',
			expected:  `{"outer": {"inner": "value"}}`,
		},
		{
			name:      "array",
			text:      `[1, 2, 3]`,
			idx:       0,
			startChar: '[',
			expected:  `[1, 2, 3]`,
		},
		{
			name:      "with escaped quotes",
			text:      `{"msg": "say \"hello\""}`,
			idx:       0,
			startChar: '{',
			expected:  `{"msg": "say \"hello\""}`,
		},
		{
			name:      "braces in string",
			text:      `{"msg": "use {} brackets"}`,
			idx:       0,
			startChar: '{',
			expected:  `{"msg": "use {} brackets"}`,
		},
		{
			name:      "incomplete JSON",
			text:      `{"key": "value"`,
			idx:       0,
			startChar: '{',
			expected:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractBalancedJSON(tt.text, tt.idx, tt.startChar)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// TestConfigValidation tests configuration validation
func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		valid  bool
	}{
		{
			name: "valid minimal config",
			config: Config{
				BuilderWorkDir: "/tmp",
			},
			valid: true,
		},
		{
			name: "valid full config",
			config: Config{
				BuilderModel:    "opus",
				BuilderWorkDir:  "/tmp",
				ReviewerModel:   "o4-mini",
				Goal:            "test task",
				MaxBudgetUSD:    10.0,
				MaxTimeSeconds:  1800,
				MaxIterations:   20,
				RecordingDir:    "/tmp/recordings",
				SystemPrompt:    "custom prompt",
				RequireApproval: true,
				Verbose:         true,
			},
			valid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			swe := New(tt.config)
			if swe == nil {
				t.Error("New() returned nil")
			}
		})
	}
}

// TestReviewResultHandling tests handling of review results
func TestReviewResultHandling(t *testing.T) {
	// Test various review result scenarios with JSON format
	tests := []struct {
		name            string
		responseText    string
		expectedVerdict bool
	}{
		{
			name:            "clear acceptance",
			responseText:    `{"verdict": "accepted", "summary": "All changes look good", "issues": []}`,
			expectedVerdict: true,
		},
		{
			name:            "clear rejection",
			responseText:    `{"verdict": "rejected", "summary": "Needs fixes", "issues": [{"severity": "high", "message": "Bug found"}]}`,
			expectedVerdict: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			swe := New(Config{})
			result := &reviewer.ReviewResult{
				ResponseText: tt.responseText,
				Success:      true,
			}

			verdict := swe.parseVerdict(result.ResponseText)
			if verdict.Accepted != tt.expectedVerdict {
				t.Errorf("expected verdict %v, got %v", tt.expectedVerdict, verdict.Accepted)
			}
		})
	}
}

func TestParseVerdictEdgeCases(t *testing.T) {
	swe := New(Config{})

	tests := []struct {
		checkFeedback    func(string) bool
		name             string
		text             string
		expectedAccepted bool
	}{
		{
			name:             "verdict with extra whitespace",
			text:             `{"verdict": "  accepted  ", "summary": "OK", "issues": []}`,
			expectedAccepted: true,
			checkFeedback:    nil,
		},
		{
			name:             "verdict in mixed case",
			text:             `{"verdict": "AcCePtEd", "summary": "OK", "issues": []}`,
			expectedAccepted: true,
			checkFeedback:    nil,
		},
		{
			name:             "JSON with null issues",
			text:             `{"verdict": "accepted", "summary": "OK", "issues": null}`,
			expectedAccepted: true,
			checkFeedback:    nil,
		},
		{
			name:             "JSON with unicode characters",
			text:             `{"verdict": "accepted", "summary": "看起来不错 ✓", "issues": []}`,
			expectedAccepted: true,
			checkFeedback: func(f string) bool {
				return strings.Contains(f, "✓")
			},
		},
		{
			name:             "verdict with additional fields",
			text:             `{"verdict": "accepted", "summary": "OK", "issues": [], "extra": "ignored", "timestamp": 123456}`,
			expectedAccepted: true,
			checkFeedback:    nil,
		},
		{
			name:             "code block with extra markdown",
			text:             "Here's my review:\n```json\n{\"verdict\": \"accepted\", \"summary\": \"OK\", \"issues\": []}\n```\nThanks!",
			expectedAccepted: true,
			checkFeedback:    nil,
		},
		{
			name:             "JSON with line breaks in strings",
			text:             "{\"verdict\": \"rejected\", \"summary\": \"Line1\\nLine2\", \"issues\": []}",
			expectedAccepted: false,
			checkFeedback: func(f string) bool {
				return strings.Contains(f, "Line1")
			},
		},
		{
			name:             "empty verdict field",
			text:             `{"verdict": "", "summary": "test", "issues": []}`,
			expectedAccepted: false,
			checkFeedback:    nil,
		},
		{
			name:             "missing verdict field",
			text:             `{"summary": "test", "issues": []}`,
			expectedAccepted: false,
			checkFeedback:    nil,
		},
		{
			name:             "verdict with typo variations",
			text:             `{"verdict": "accept", "summary": "OK", "issues": []}`,
			expectedAccepted: false, // "accept" not "accepted"
			checkFeedback:    nil,
		},
		{
			name:             "very large JSON response",
			text:             `{"verdict": "accepted", "summary": "` + strings.Repeat("A", 10000) + `", "issues": []}`,
			expectedAccepted: true,
			checkFeedback: func(f string) bool {
				return len(f) > 1000
			},
		},
		{
			name:             "JSON with special characters escaped",
			text:             `{"verdict": "accepted", "summary": "File: \"test.go\" is OK", "issues": []}`,
			expectedAccepted: true,
			checkFeedback:    nil,
		},
		{
			name:             "multiple issues with varying severity",
			text:             `{"verdict": "rejected", "summary": "Multiple issues found", "issues": [{"severity": "low", "message": "minor"}, {"severity": "critical", "message": "major"}]}`,
			expectedAccepted: false,
			checkFeedback: func(f string) bool {
				return strings.Contains(f, "CRITICAL") && strings.Contains(f, "LOW")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verdict := swe.parseVerdict(tt.text)

			if verdict.Accepted != tt.expectedAccepted {
				t.Errorf("expected accepted=%v, got %v", tt.expectedAccepted, verdict.Accepted)
			}

			if tt.checkFeedback != nil && !tt.checkFeedback(verdict.Feedback) {
				t.Errorf("feedback check failed for: %s", verdict.Feedback)
			}

			// Ensure feedback is never empty
			if verdict.Feedback == "" {
				t.Error("verdict feedback should never be empty")
			}
		})
	}
}

func TestFormatFeedbackEdgeCases(t *testing.T) {
	tests := []struct {
		validate func(string) bool
		name     string
		verdict  ReviewVerdictJSON
	}{
		{
			name: "issue without file",
			verdict: ReviewVerdictJSON{
				Summary: "Found issues",
				Issues: []ReviewIssue{
					{Severity: "high", Message: "Generic problem"},
				},
			},
			validate: func(f string) bool {
				return strings.Contains(f, "[HIGH]") && strings.Contains(f, "Generic problem")
			},
		},
		{
			name: "issue without line number",
			verdict: ReviewVerdictJSON{
				Summary: "Found issues",
				Issues: []ReviewIssue{
					{Severity: "medium", File: "test.go", Message: "Problem"},
				},
			},
			validate: func(f string) bool {
				return strings.Contains(f, "test.go") && !strings.Contains(f, "test.go:")
			},
		},
		{
			name: "empty severity defaults to display",
			verdict: ReviewVerdictJSON{
				Summary: "Issues",
				Issues: []ReviewIssue{
					{Severity: "", Message: "Unknown severity"},
				},
			},
			validate: func(f string) bool {
				return strings.Contains(f, "Unknown severity")
			},
		},
		{
			name: "very long suggestion text",
			verdict: ReviewVerdictJSON{
				Summary: "Issues",
				Issues: []ReviewIssue{
					{
						Severity:   "low",
						Message:    "Test",
						Suggestion: strings.Repeat("Very long suggestion text. ", 100),
					},
				},
			},
			validate: func(f string) bool {
				return strings.Contains(f, "Suggestion:") && len(f) > 1000
			},
		},
		{
			name: "special characters in messages",
			verdict: ReviewVerdictJSON{
				Summary: "Test <>&\"'",
				Issues: []ReviewIssue{
					{Severity: "high", Message: "Error: <>&\"'", File: "path/with spaces/file.go"},
				},
			},
			validate: func(f string) bool {
				return strings.Contains(f, "<>&\"'")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatFeedback(tt.verdict)

			if !tt.validate(result) {
				t.Errorf("validation failed for formatted feedback: %s", result)
			}

			// Ensure summary is always included
			if !strings.Contains(result, tt.verdict.Summary) {
				t.Error("formatted feedback should contain summary")
			}
		})
	}
}
