package engine

import (
	"testing"

	"github.com/bazelment/yoloswe/medivac/issue"
)

func TestParseAnalysis_Valid(t *testing.T) {
	text := `I investigated the issue and found the root cause.

<ANALYSIS>
reasoning: The Dockerfile was missing a COPY directive for the shared package.
root_cause: Missing COPY directive in Dockerfile for @kernel/ui package
fix_applied: yes
fix_options:
- Add COPY: Add the missing COPY directive to the Dockerfile
- Restructure imports: Move shared code into the service directory
</ANALYSIS>`

	a := ParseAnalysis(text)
	if a == nil {
		t.Fatal("expected non-nil analysis")
	}
	if a.Reasoning != "The Dockerfile was missing a COPY directive for the shared package." {
		t.Errorf("reasoning = %q", a.Reasoning)
	}
	if a.RootCause != "Missing COPY directive in Dockerfile for @kernel/ui package" {
		t.Errorf("root_cause = %q", a.RootCause)
	}
	if !a.FixApplied {
		t.Error("expected fix_applied = true")
	}
	if len(a.FixOptions) != 2 {
		t.Fatalf("expected 2 fix options, got %d", len(a.FixOptions))
	}
	if a.FixOptions[0].Label != "Add COPY" {
		t.Errorf("option[0].Label = %q", a.FixOptions[0].Label)
	}
	if a.FixOptions[1].Label != "Restructure imports" {
		t.Errorf("option[1].Label = %q", a.FixOptions[1].Label)
	}
}

func TestParseAnalysis_NoFixApplied(t *testing.T) {
	text := `<ANALYSIS>
reasoning: The CI failure is caused by a missing GitHub repo secret.
root_cause: Missing GitHub repo secret WIF_PROVIDER
fix_applied: no
fix_options:
- Configure secret: Add WIF_PROVIDER to GitHub repo settings
- Use service account key: Switch to JSON key auth instead of workload identity
</ANALYSIS>`

	a := ParseAnalysis(text)
	if a == nil {
		t.Fatal("expected non-nil analysis")
	}
	if a.FixApplied {
		t.Error("expected fix_applied = false")
	}
	if a.RootCause != "Missing GitHub repo secret WIF_PROVIDER" {
		t.Errorf("root_cause = %q", a.RootCause)
	}
	if len(a.FixOptions) != 2 {
		t.Fatalf("expected 2 fix options, got %d", len(a.FixOptions))
	}
}

func TestParseAnalysis_MissingBlock(t *testing.T) {
	text := "I fixed the issue by updating the import path."
	a := ParseAnalysis(text)
	if a != nil {
		t.Error("expected nil for missing block")
	}
}

func TestParseAnalysis_PartialBlock_NoClose(t *testing.T) {
	text := `<ANALYSIS>
reasoning: something
root_cause: something else`

	a := ParseAnalysis(text)
	if a != nil {
		t.Error("expected nil for unclosed block")
	}
}

func TestParseAnalysis_EmptyBlock(t *testing.T) {
	text := `<ANALYSIS>
</ANALYSIS>`

	a := ParseAnalysis(text)
	if a == nil {
		t.Fatal("expected non-nil analysis for empty block")
	}
	if a.Reasoning != "" || a.RootCause != "" || a.FixApplied || len(a.FixOptions) != 0 {
		t.Error("expected all fields empty/zero")
	}
}

func TestParseAnalysis_SingleOption(t *testing.T) {
	text := `<ANALYSIS>
reasoning: Unused import
root_cause: Leftover import from refactor
fix_applied: yes
fix_options:
- Remove import: Delete the unused import statement
</ANALYSIS>`

	a := ParseAnalysis(text)
	if a == nil {
		t.Fatal("expected non-nil analysis")
	}
	if len(a.FixOptions) != 1 {
		t.Fatalf("expected 1 fix option, got %d", len(a.FixOptions))
	}
	if a.FixOptions[0].Label != "Remove import" {
		t.Errorf("option label = %q", a.FixOptions[0].Label)
	}
	if a.FixOptions[0].Description != "Delete the unused import statement" {
		t.Errorf("option description = %q", a.FixOptions[0].Description)
	}
}

func TestParseAnalysis_FixAppliedCaseInsensitive(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"yes", true},
		{"Yes", true},
		{"YES", true},
		{"no", false},
		{"No", false},
		{"maybe", false},
	}
	for _, tt := range tests {
		text := "<ANALYSIS>\nfix_applied: " + tt.value + "\n</ANALYSIS>"
		a := ParseAnalysis(text)
		if a == nil {
			t.Fatalf("nil for fix_applied=%s", tt.value)
		}
		if a.FixApplied != tt.want {
			t.Errorf("fix_applied=%s: got %v, want %v", tt.value, a.FixApplied, tt.want)
		}
	}
}

func TestParseAnalysis_FixOptionWithoutDescription(t *testing.T) {
	text := `<ANALYSIS>
reasoning: test
root_cause: test
fix_applied: no
fix_options:
- Manual intervention required
</ANALYSIS>`

	a := ParseAnalysis(text)
	if a == nil {
		t.Fatal("expected non-nil")
	}
	if len(a.FixOptions) != 1 {
		t.Fatalf("expected 1 option, got %d", len(a.FixOptions))
	}
	if a.FixOptions[0].Label != "Manual intervention required" {
		t.Errorf("label = %q", a.FixOptions[0].Label)
	}
	if a.FixOptions[0].Description != "" {
		t.Errorf("description should be empty, got %q", a.FixOptions[0].Description)
	}
}

func TestParseFixOption(t *testing.T) {
	tests := []struct {
		input string
		want  issue.FixOption
	}{
		{"Add COPY: Add the missing COPY directive", issue.FixOption{Label: "Add COPY", Description: "Add the missing COPY directive"}},
		{"Simple label", issue.FixOption{Label: "Simple label"}},
		{"  Trimmed : spaces  ", issue.FixOption{Label: "Trimmed", Description: "spaces"}},
	}
	for _, tt := range tests {
		got := parseFixOption(tt.input)
		if got.Label != tt.want.Label || got.Description != tt.want.Description {
			t.Errorf("parseFixOption(%q) = {%q, %q}, want {%q, %q}",
				tt.input, got.Label, got.Description, tt.want.Label, tt.want.Description)
		}
	}
}
