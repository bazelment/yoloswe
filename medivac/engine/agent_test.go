package engine

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/bazelment/yoloswe/medivac/issue"
)

func TestRecordFixAttempt_SuccessWithPR(t *testing.T) {
	dir := t.TempDir()
	trackerPath := filepath.Join(dir, ".medivac", "issues.json")
	tracker, err := issue.NewTracker(trackerPath)
	if err != nil {
		t.Fatal(err)
	}

	// Create a test issue
	iss := &issue.Issue{
		ID:        "test1",
		Signature: "sig1",
		Category:  issue.CategoryLintGo,
		Summary:   "unused variable",
		Status:    issue.StatusInProgress,
	}
	tracker.Reconcile([]issue.CIFailure{{
		Signature: iss.Signature,
		Category:  iss.Category,
		Summary:   iss.Summary,
	}})
	tracker.UpdateStatus(iss.Signature, issue.StatusInProgress)

	// Record a successful fix with PR
	recordFixAttempt(
		tracker,
		[]*issue.Issue{iss},
		"fix/lint-go/test1",
		"https://github.com/user/repo/pull/123",
		123,
		0.50,
		&AgentAnalysis{
			Reasoning:  "Variable was declared but never used",
			RootCause:  "Dead code from refactoring",
			FixApplied: true,
		},
		true,
		nil,
		"agent.log",
	)

	// Check the issue status
	updated := tracker.Get(iss.Signature)
	if updated.Status != issue.StatusFixPending {
		t.Errorf("expected status %s, got %s", issue.StatusFixPending, updated.Status)
	}

	// Check the fix attempt
	if len(updated.FixAttempts) != 1 {
		t.Fatalf("expected 1 fix attempt, got %d", len(updated.FixAttempts))
	}
	attempt := updated.FixAttempts[0]
	if attempt.Outcome != "pr_created" {
		t.Errorf("expected outcome pr_created, got %s", attempt.Outcome)
	}
	if attempt.PRState != "OPEN" {
		t.Errorf("expected PR state OPEN, got %s", attempt.PRState)
	}
	if attempt.PRURL != "https://github.com/user/repo/pull/123" {
		t.Errorf("expected PR URL, got %s", attempt.PRURL)
	}
	if attempt.PRNumber != 123 {
		t.Errorf("expected PR number 123, got %d", attempt.PRNumber)
	}
	if attempt.Reasoning != "Variable was declared but never used" {
		t.Errorf("expected reasoning, got %s", attempt.Reasoning)
	}
}

func TestRecordFixAttempt_AnalysisOnly(t *testing.T) {
	dir := t.TempDir()
	trackerPath := filepath.Join(dir, ".medivac", "issues.json")
	tracker, err := issue.NewTracker(trackerPath)
	if err != nil {
		t.Fatal(err)
	}

	// Create a test issue
	iss := &issue.Issue{
		ID:        "test2",
		Signature: "sig2",
		Category:  issue.CategoryInfraCI,
		Summary:   "service unavailable",
		Status:    issue.StatusInProgress,
	}
	tracker.Reconcile([]issue.CIFailure{{
		Signature: iss.Signature,
		Category:  iss.Category,
		Summary:   iss.Summary,
	}})
	tracker.UpdateStatus(iss.Signature, issue.StatusInProgress)

	// Record a successful analysis-only result (no PR)
	recordFixAttempt(
		tracker,
		[]*issue.Issue{iss},
		"fix/infra-ci/test2",
		"",
		0,
		0.20,
		&AgentAnalysis{
			Reasoning:  "External service dependency failure",
			RootCause:  "Third-party API outage",
			FixApplied: false,
		},
		true,
		nil,
		"agent.log",
	)

	// Check the issue status - should be reset to New
	updated := tracker.Get(iss.Signature)
	if updated.Status != issue.StatusNew {
		t.Errorf("expected status %s, got %s", issue.StatusNew, updated.Status)
	}

	// Check the fix attempt
	if len(updated.FixAttempts) != 1 {
		t.Fatalf("expected 1 fix attempt, got %d", len(updated.FixAttempts))
	}
	attempt := updated.FixAttempts[0]
	if attempt.Outcome != "analysis_only" {
		t.Errorf("expected outcome analysis_only, got %s", attempt.Outcome)
	}
	if attempt.PRURL != "" {
		t.Errorf("expected no PR URL, got %s", attempt.PRURL)
	}
}

func TestRecordFixAttempt_Failure(t *testing.T) {
	dir := t.TempDir()
	trackerPath := filepath.Join(dir, ".medivac", "issues.json")
	tracker, err := issue.NewTracker(trackerPath)
	if err != nil {
		t.Fatal(err)
	}

	// Create a test issue
	iss := &issue.Issue{
		ID:        "test3",
		Signature: "sig3",
		Category:  issue.CategoryTest,
		Summary:   "test failed",
		Status:    issue.StatusInProgress,
	}
	tracker.Reconcile([]issue.CIFailure{{
		Signature: iss.Signature,
		Category:  iss.Category,
		Summary:   iss.Summary,
	}})
	tracker.UpdateStatus(iss.Signature, issue.StatusInProgress)

	// Record a failed fix attempt
	testErr := errors.New("agent made no file changes")
	recordFixAttempt(
		tracker,
		[]*issue.Issue{iss},
		"fix/test/test3",
		"",
		0,
		0.30,
		&AgentAnalysis{
			Reasoning:  "Test environment issue",
			RootCause:  "Missing test fixture",
			FixApplied: false,
		},
		false,
		testErr,
		"agent.log",
	)

	// Check the issue status - should be reset to New
	updated := tracker.Get(iss.Signature)
	if updated.Status != issue.StatusNew {
		t.Errorf("expected status %s, got %s", issue.StatusNew, updated.Status)
	}

	// Check the fix attempt
	if len(updated.FixAttempts) != 1 {
		t.Fatalf("expected 1 fix attempt, got %d", len(updated.FixAttempts))
	}
	attempt := updated.FixAttempts[0]
	if attempt.Outcome != "failed" {
		t.Errorf("expected outcome failed, got %s", attempt.Outcome)
	}
	if attempt.Error != testErr.Error() {
		t.Errorf("expected error %q, got %q", testErr.Error(), attempt.Error)
	}
}

func TestRecordFixAttempt_GroupFix(t *testing.T) {
	dir := t.TempDir()
	trackerPath := filepath.Join(dir, ".medivac", "issues.json")
	tracker, err := issue.NewTracker(trackerPath)
	if err != nil {
		t.Fatal(err)
	}

	// Create multiple test issues
	issues := []*issue.Issue{
		{
			ID:        "test4a",
			Signature: "sig4a",
			Category:  issue.CategoryLintTS,
			Summary:   "implicit any type",
			Status:    issue.StatusInProgress,
		},
		{
			ID:        "test4b",
			Signature: "sig4b",
			Category:  issue.CategoryLintTS,
			Summary:   "implicit any type",
			Status:    issue.StatusInProgress,
		},
	}

	for _, iss := range issues {
		tracker.Reconcile([]issue.CIFailure{{
			Signature: iss.Signature,
			Category:  iss.Category,
			Summary:   iss.Summary,
		}})
		tracker.UpdateStatus(iss.Signature, issue.StatusInProgress)
	}

	// Record a successful group fix with PR
	totalCost := 0.80
	recordFixAttempt(
		tracker,
		issues,
		"fix/lint-ts/test4a",
		"https://github.com/user/repo/pull/456",
		456,
		totalCost,
		&AgentAnalysis{
			Reasoning:  "Missing type annotations",
			RootCause:  "Implicit any types",
			FixApplied: true,
		},
		true,
		nil,
		"agent.log",
	)

	// Check that both issues were updated
	for _, iss := range issues {
		updated := tracker.Get(iss.Signature)
		if updated.Status != issue.StatusFixPending {
			t.Errorf("issue %s: expected status %s, got %s", iss.ID, issue.StatusFixPending, updated.Status)
		}

		if len(updated.FixAttempts) != 1 {
			t.Fatalf("issue %s: expected 1 fix attempt, got %d", iss.ID, len(updated.FixAttempts))
		}
		attempt := updated.FixAttempts[0]
		if attempt.Outcome != "pr_created" {
			t.Errorf("issue %s: expected outcome pr_created, got %s", iss.ID, attempt.Outcome)
		}

		// Check that cost was split evenly
		expectedCost := totalCost / float64(len(issues))
		if attempt.AgentCost != expectedCost {
			t.Errorf("issue %s: expected cost %.2f, got %.2f", iss.ID, expectedCost, attempt.AgentCost)
		}
	}
}
