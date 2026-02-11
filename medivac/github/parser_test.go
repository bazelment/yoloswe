package github

import (
	"fmt"
	"strings"
	"testing"
)

func TestCleanLog_StripANSI(t *testing.T) {
	input := "\x1b[31mERROR:\x1b[0m something failed"
	got := CleanLog(input)
	if strings.Contains(got, "\x1b") {
		t.Errorf("ANSI sequences not stripped: %q", got)
	}
	if got != "ERROR: something failed" {
		t.Errorf("unexpected result: %q", got)
	}
}

func TestCleanLog_StripGHActionsMarkers(t *testing.T) {
	input := "##[error]Build failed\n##[group]Run tests\n##[endgroup]"
	got := CleanLog(input)
	if strings.Contains(got, "##[") {
		t.Errorf("GitHub Actions markers not stripped: %q", got)
	}
}

func TestCleanLog_StripTimestamps(t *testing.T) {
	input := "2025-01-15T10:30:45.123Z some log line\n2025-01-15T10:30:46.456Z another line"
	got := CleanLog(input)
	if strings.Contains(got, "2025-01-15T") {
		t.Errorf("timestamps not stripped: %q", got)
	}
	if !strings.Contains(got, "some log line") {
		t.Errorf("log content lost: %q", got)
	}
}

func TestCleanLog_Truncation(t *testing.T) {
	// Build a log bigger than MaxLogSize.
	line := "this is a log line that repeats\n"
	var b strings.Builder
	for b.Len() < MaxLogSize+10000 {
		b.WriteString(line)
	}
	input := b.String()

	got := CleanLog(input)
	if len(got) > MaxLogSize {
		t.Errorf("log not truncated: got %d bytes, want <= %d", len(got), MaxLogSize)
	}
	// After truncation to a newline boundary, the first line should be a
	// complete line (starts with "this").
	if !strings.HasPrefix(got, "this") {
		t.Errorf("expected truncated log to start at a line boundary, got prefix: %q", got[:20])
	}
}

func TestCleanLog_Empty(t *testing.T) {
	got := CleanLog("")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestTrimLog_Short(t *testing.T) {
	// Log shorter than head+tail should be returned as-is.
	log := "line1\nline2\nline3"
	got := TrimLog(log, 100, 100)
	if got != log {
		t.Errorf("short log should be unchanged, got %q", got)
	}
}

func TestTrimLog_ExactBoundary(t *testing.T) {
	// Exactly head+tail lines should be returned as-is.
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i)
	}
	log := strings.Join(lines, "\n")
	got := TrimLog(log, 5, 5)
	if got != log {
		t.Errorf("exact boundary log should be unchanged")
	}
}

func TestTrimLog_Trims(t *testing.T) {
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i)
	}
	log := strings.Join(lines, "\n")
	got := TrimLog(log, 3, 3)

	// Should contain first 3 lines.
	if !strings.Contains(got, "line0") || !strings.Contains(got, "line2") {
		t.Error("trimmed log should contain head lines")
	}
	// Should contain last 3 lines.
	if !strings.Contains(got, "line17") || !strings.Contains(got, "line19") {
		t.Error("trimmed log should contain tail lines")
	}
	// Should contain trimmed marker.
	if !strings.Contains(got, "14 lines trimmed") {
		t.Errorf("trimmed log should contain trim marker, got: %s", got)
	}
	// Should NOT contain middle lines.
	if strings.Contains(got, "line10") {
		t.Error("trimmed log should not contain middle lines")
	}
}

func TestComputeSignature_Stable(t *testing.T) {
	sig1 := ComputeSignature(CategoryLintGo, "main.go", "unused variable x", "lint", "")
	sig2 := ComputeSignature(CategoryLintGo, "main.go", "unused variable x", "lint", "")
	if sig1 != sig2 {
		t.Errorf("signatures should be equal: %s != %s", sig1, sig2)
	}

	// Different message should produce different signature.
	sig3 := ComputeSignature(CategoryLintGo, "main.go", "different error", "lint", "")
	if sig1 == sig3 {
		t.Errorf("signatures should differ for different messages")
	}
}

func TestComputeSignature_IgnoresLineNumbers(t *testing.T) {
	sig1 := ComputeSignature(CategoryBuild, "main.go", "error at :10:5", "build", "")
	sig2 := ComputeSignature(CategoryBuild, "main.go", "error at :20:3", "build", "")
	if sig1 != sig2 {
		t.Errorf("signatures should be equal after line number normalization: %s != %s", sig1, sig2)
	}
}

func TestComputeSignature_SameAcrossJobs(t *testing.T) {
	// Same error in different jobs should produce the same signature
	// so cross-job dedup works correctly.
	sig1 := ComputeSignature(CategoryTest, "foo.go", "test failed", "lint-job", "")
	sig2 := ComputeSignature(CategoryTest, "foo.go", "test failed", "build-job", "")
	if sig1 != sig2 {
		t.Errorf("signatures should be equal across different jobs: %s != %s", sig1, sig2)
	}
}

func TestComputeSignature_JobNameFallback(t *testing.T) {
	// When summary and details are both empty, job name is used as fallback.
	sig1 := ComputeSignature(CategoryUnknown, "", "", "lint-job", "")
	sig2 := ComputeSignature(CategoryUnknown, "", "", "build-job", "")
	if sig1 == sig2 {
		t.Errorf("signatures should differ when job name is the only discriminator")
	}
}

func TestComputeSignature_EmptySummaryFallback(t *testing.T) {
	// With empty summary, should use details.
	sig1 := ComputeSignature(CategoryUnknown, "", "", "job", "some error detail")
	if sig1 == "" {
		t.Error("expected non-empty signature")
	}
	// Signature should be "hash:" (no file)
	if !strings.Contains(sig1, ":") {
		t.Errorf("expected colon separator in signature, got %s", sig1)
	}

	// With empty summary and details, should use job name.
	sig2 := ComputeSignature(CategoryUnknown, "", "", "my-job", "")
	if sig2 == "" {
		t.Error("expected non-empty signature")
	}
}

func TestValidCategories(t *testing.T) {
	expected := []FailureCategory{
		CategoryLintGo, CategoryLintBazel, CategoryLintTS, CategoryLintPython,
		CategoryBuild, CategoryBuildDocker, CategoryTest,
		CategoryInfraDepbot, CategoryInfraCI, CategoryUnknown,
	}
	for _, cat := range expected {
		if !ValidCategories[cat] {
			t.Errorf("category %q should be valid", cat)
		}
	}
	if ValidCategories["bogus"] {
		t.Error("bogus category should not be valid")
	}
}

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
