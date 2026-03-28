package prompt

import (
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/symphony/model"
)

func strPtr(s string) *string        { return &s }
func intPtr(i int) *int              { return &i }
func timePtr(t time.Time) *time.Time { return &t }

func testIssue() model.Issue {
	return model.Issue{
		ID:          "issue-123",
		Identifier:  "PROJ-42",
		Title:       "Fix login bug",
		Description: strPtr("Users cannot log in after password reset"),
		Priority:    intPtr(2),
		State:       "In Progress",
		BranchName:  strPtr("fix/login-bug"),
		URL:         strPtr("https://linear.app/proj/issue/PROJ-42"),
		Labels:      []string{"bug", "auth", "critical"},
		BlockedBy: []model.BlockerRef{
			{Identifier: strPtr("PROJ-40"), State: strPtr("Done")},
			{ID: strPtr("blocker-id-99")},
		},
		CreatedAt: timePtr(time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)),
		UpdatedAt: timePtr(time.Date(2026, 3, 20, 14, 0, 0, 0, time.UTC)),
	}
}

func TestRenderInitialPrompt_EmptyTemplate(t *testing.T) {
	t.Parallel()
	got, err := RenderInitialPrompt("", testIssue(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != fallbackPrompt {
		t.Fatalf("expected fallback prompt, got %q", got)
	}
}

func TestRenderInitialPrompt_NoVariables(t *testing.T) {
	t.Parallel()
	tmpl := "Just a plain string with no variables."
	got, err := RenderInitialPrompt(tmpl, testIssue(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != tmpl {
		t.Fatalf("expected template back verbatim, got %q", got)
	}
}

func TestRenderInitialPrompt_AllIssueFields(t *testing.T) {
	t.Parallel()
	tmpl := `ID={{ issue.id }} IDENT={{ issue.identifier }} TITLE={{ issue.title }} DESC={{ issue.description }} PRI={{ issue.priority }} STATE={{ issue.state }} BRANCH={{ issue.branch_name }} URL={{ issue.url }} LABELS={{ issue.labels }} BLOCKED={{ issue.blocked_by }} CREATED={{ issue.created_at }} UPDATED={{ issue.updated_at }}`

	issue := testIssue()
	got, err := RenderInitialPrompt(tmpl, issue, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := map[string]string{
		"ID=issue-123":        "",
		"IDENT=PROJ-42":       "",
		"TITLE=Fix login bug": "",
		"DESC=Users cannot log in after password": "",
		"PRI=2":                "",
		"STATE=In Progress":    "",
		"BRANCH=fix/login-bug": "",
		"URL=https://linear.app/proj/issue/PROJ-42": "",
		"LABELS=bug, auth, critical":                "",
		"BLOCKED=PROJ-40, blocker-id-99":            "",
		"CREATED=2026-01-15T10:30:00Z":              "",
		"UPDATED=2026-03-20T14:00:00Z":              "",
	}
	for expect := range checks {
		if !strings.Contains(got, expect) {
			t.Errorf("output missing %q\nfull output: %s", expect, got)
		}
	}
}

func TestRenderInitialPrompt_NilOptionalFields(t *testing.T) {
	t.Parallel()
	issue := model.Issue{
		ID:         "id-1",
		Identifier: "X-1",
		Title:      "Minimal",
		State:      "Todo",
	}
	tmpl := "desc={{ issue.description }} pri={{ issue.priority }} branch={{ issue.branch_name }}"
	got, err := RenderInitialPrompt(tmpl, issue, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "desc= pri= branch=" {
		t.Fatalf("unexpected output for nil fields: %q", got)
	}
}

func TestRenderInitialPrompt_AttemptNil(t *testing.T) {
	t.Parallel()
	tmpl := "attempt={{ attempt }}"
	got, err := RenderInitialPrompt(tmpl, testIssue(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "attempt=" {
		t.Fatalf("expected empty attempt, got %q", got)
	}
}

func TestRenderInitialPrompt_AttemptSet(t *testing.T) {
	t.Parallel()
	tmpl := "attempt={{ attempt }}"
	attempt := 3
	got, err := RenderInitialPrompt(tmpl, testIssue(), &attempt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "attempt=3" {
		t.Fatalf("expected attempt=3, got %q", got)
	}
}

func TestRenderInitialPrompt_ConditionalTrue(t *testing.T) {
	t.Parallel()
	tmpl := "before{% if attempt %} retry={{ attempt }}{% endif %} after"
	attempt := 2
	got, err := RenderInitialPrompt(tmpl, testIssue(), &attempt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "before retry=2 after" {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestRenderInitialPrompt_ConditionalFalse(t *testing.T) {
	t.Parallel()
	tmpl := "before{% if attempt %} retry={{ attempt }}{% endif %} after"
	got, err := RenderInitialPrompt(tmpl, testIssue(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "before after" {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestRenderInitialPrompt_ConditionalWithIssueField(t *testing.T) {
	t.Parallel()
	tmpl := "{% if issue.labels %}Labels: {{ issue.labels }}{% endif %}"
	got, err := RenderInitialPrompt(tmpl, testIssue(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Labels: bug, auth, critical" {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestRenderInitialPrompt_ConditionalEmptyField(t *testing.T) {
	t.Parallel()
	issue := model.Issue{ID: "1", Identifier: "X-1", Title: "t", State: "s"}
	tmpl := "{% if issue.labels %}has labels{% endif %}done"
	got, err := RenderInitialPrompt(tmpl, issue, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "done" {
		t.Fatalf("expected empty labels conditional to be skipped, got %q", got)
	}
}

func TestRenderInitialPrompt_StrictUnknownVariable(t *testing.T) {
	t.Parallel()
	tmpl := "{{ issue.nonexistent }}"
	_, err := RenderInitialPrompt(tmpl, testIssue(), nil)
	if err == nil {
		t.Fatal("expected error for unknown variable")
	}
	if !strings.Contains(err.Error(), "template_render_error") {
		t.Fatalf("error should mention template_render_error: %v", err)
	}
	if !strings.Contains(err.Error(), "issue.nonexistent") {
		t.Fatalf("error should mention the variable name: %v", err)
	}
}

func TestRenderInitialPrompt_StrictUnknownFilter(t *testing.T) {
	t.Parallel()
	tmpl := "{{ issue.title | upcase }}"
	_, err := RenderInitialPrompt(tmpl, testIssue(), nil)
	if err == nil {
		t.Fatal("expected error for unknown filter")
	}
	if !strings.Contains(err.Error(), "template_render_error") {
		t.Fatalf("error should mention template_render_error: %v", err)
	}
	if !strings.Contains(err.Error(), "filter") {
		t.Fatalf("error should mention filter: %v", err)
	}
}

func TestRenderInitialPrompt_StrictUnknownConditionalVar(t *testing.T) {
	t.Parallel()
	tmpl := "{% if bogus %}stuff{% endif %}"
	_, err := RenderInitialPrompt(tmpl, testIssue(), nil)
	if err == nil {
		t.Fatal("expected error for unknown conditional variable")
	}
	if !strings.Contains(err.Error(), "template_render_error") {
		t.Fatalf("error should mention template_render_error: %v", err)
	}
}

func TestRenderInitialPrompt_UnclosedVariable(t *testing.T) {
	t.Parallel()
	tmpl := "{{ issue.title"
	_, err := RenderInitialPrompt(tmpl, testIssue(), nil)
	if err == nil {
		t.Fatal("expected error for unclosed variable tag")
	}
}

func TestRenderInitialPrompt_MissingEndif(t *testing.T) {
	t.Parallel()
	tmpl := "{% if attempt %}no end"
	_, err := RenderInitialPrompt(tmpl, testIssue(), nil)
	if err == nil {
		t.Fatal("expected error for missing endif")
	}
}

func TestRenderContinuationPrompt_DoesNotContainTemplate(t *testing.T) {
	t.Parallel()
	issue := testIssue()
	got := RenderContinuationPrompt(issue, 3)
	if strings.Contains(got, "{{") {
		t.Fatal("continuation prompt should not contain template syntax")
	}
	if !strings.Contains(got, "Continue working") {
		t.Fatal("continuation prompt should contain continuation guidance")
	}
	if !strings.Contains(got, "PROJ-42") {
		t.Fatal("continuation prompt should reference the issue identifier")
	}
	if !strings.Contains(got, "turn 3") {
		t.Fatal("continuation prompt should mention the turn number")
	}
}

func TestBuildContinuationGuidance_WithMaxTurns(t *testing.T) {
	t.Parallel()
	issue := testIssue()
	got := BuildContinuationGuidance(issue, 2, 5)
	if !strings.Contains(got, "turn 2 of 5") {
		t.Fatalf("expected turn count, got %q", got)
	}
	if !strings.Contains(got, "Fix login bug") {
		t.Fatalf("expected issue title in guidance, got %q", got)
	}
	if !strings.Contains(got, `"In Progress"`) {
		t.Fatalf("expected state in guidance, got %q", got)
	}
}

func TestBuildContinuationGuidance_ZeroMaxTurns(t *testing.T) {
	t.Parallel()
	issue := testIssue()
	got := BuildContinuationGuidance(issue, 4, 0)
	if !strings.Contains(got, "turn 4.") {
		t.Fatalf("expected turn count without max, got %q", got)
	}
	if strings.Contains(got, " of ") {
		t.Fatal("should not mention max turns when it is zero")
	}
}
