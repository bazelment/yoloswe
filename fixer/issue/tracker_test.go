package issue

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/fixer/github"
)

func tempTrackerPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, ".fixer", "issues.json")
}

func TestNewTracker_EmptyFile(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	if len(tracker.All()) != 0 {
		t.Fatalf("expected 0 issues, got %d", len(tracker.All()))
	}
}

func TestReconcile_NewIssues(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	failures := []github.CIFailure{
		{
			Signature: "lint/go:abc:main.go",
			Category:  github.CategoryLintGo,
			Summary:   "unused variable",
			File:      "main.go",
			Timestamp: time.Now(),
		},
		{
			Signature: "test/bazel:def:test_test.go",
			Category:  github.CategoryTest,
			Summary:   "test failed",
			File:      "test_test.go",
			Timestamp: time.Now(),
		},
	}

	result := tracker.Reconcile(failures)
	if len(result.New) != 2 {
		t.Fatalf("expected 2 new issues, got %d", len(result.New))
	}
	if len(result.Updated) != 0 {
		t.Fatalf("expected 0 updated, got %d", len(result.Updated))
	}
	if len(result.Resolved) != 0 {
		t.Fatalf("expected 0 resolved, got %d", len(result.Resolved))
	}

	// Verify issue fields
	all := tracker.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 tracked issues, got %d", len(all))
	}
}

func TestReconcile_UpdateExisting(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	sig := "lint/go:abc:main.go"
	now := time.Now()

	// First reconcile
	tracker.Reconcile([]github.CIFailure{
		{Signature: sig, Category: github.CategoryLintGo, Summary: "unused variable", Timestamp: now},
	})

	// Second reconcile with same failure
	later := now.Add(time.Hour)
	result := tracker.Reconcile([]github.CIFailure{
		{Signature: sig, Category: github.CategoryLintGo, Summary: "unused variable", Timestamp: later},
	})

	if len(result.New) != 0 {
		t.Fatalf("expected 0 new, got %d", len(result.New))
	}
	if len(result.Updated) != 1 {
		t.Fatalf("expected 1 updated, got %d", len(result.Updated))
	}

	issue := tracker.Get(sig)
	if issue.SeenCount != 2 {
		t.Errorf("expected seen count 2, got %d", issue.SeenCount)
	}
}

func TestReconcile_Recurred(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	sig := "lint/go:abc:main.go"
	now := time.Now()

	// Create and mark as fix_merged
	tracker.Reconcile([]github.CIFailure{
		{Signature: sig, Category: github.CategoryLintGo, Summary: "unused variable", Timestamp: now},
	})
	tracker.UpdateStatus(sig, StatusFixMerged)

	// Failure appears again
	result := tracker.Reconcile([]github.CIFailure{
		{Signature: sig, Category: github.CategoryLintGo, Summary: "unused variable", Timestamp: now.Add(time.Hour)},
	})

	if len(result.Updated) != 1 {
		t.Fatalf("expected 1 updated, got %d", len(result.Updated))
	}

	issue := tracker.Get(sig)
	if issue.Status != StatusRecurred {
		t.Errorf("expected status recurred, got %s", issue.Status)
	}
}

func TestReconcile_Verified(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	sig := "lint/go:abc:main.go"
	now := time.Now()

	// Create and mark as fix_merged
	tracker.Reconcile([]github.CIFailure{
		{Signature: sig, Category: github.CategoryLintGo, Summary: "unused variable", Timestamp: now},
	})
	tracker.UpdateStatus(sig, StatusFixMerged)

	// Reconcile without that failure present
	result := tracker.Reconcile([]github.CIFailure{})

	if len(result.Resolved) != 1 {
		t.Fatalf("expected 1 resolved, got %d", len(result.Resolved))
	}

	issue := tracker.Get(sig)
	if issue.Status != StatusVerified {
		t.Errorf("expected status verified, got %s", issue.Status)
	}
	if issue.ResolvedAt == nil {
		t.Error("expected ResolvedAt to be set")
	}
}

func TestGetActionable(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	tracker.Reconcile([]github.CIFailure{
		{Signature: "sig1", Category: github.CategoryLintGo, Summary: "issue 1", Timestamp: now},
		{Signature: "sig2", Category: github.CategoryTest, Summary: "issue 2", Timestamp: now},
		{Signature: "sig3", Category: github.CategoryBuild, Summary: "issue 3", Timestamp: now},
	})

	// Mark one as in_progress
	tracker.UpdateStatus("sig2", StatusInProgress)

	actionable := tracker.GetActionable()
	if len(actionable) != 2 {
		t.Fatalf("expected 2 actionable, got %d", len(actionable))
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := tempTrackerPath(t)

	tracker1, err := NewTracker(path)
	if err != nil {
		t.Fatal(err)
	}

	tracker1.Reconcile([]github.CIFailure{
		{Signature: "sig1", Category: github.CategoryLintGo, Summary: "issue 1", Timestamp: time.Now()},
	})
	tracker1.AddFixAttempt("sig1", FixAttempt{Branch: "fix/lint-go/abc", PRURL: "https://example.com/pr/1"})

	if err := tracker1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("tracker file not created")
	}

	// Load into new tracker
	tracker2, err := NewTracker(path)
	if err != nil {
		t.Fatalf("NewTracker reload: %v", err)
	}

	all := tracker2.All()
	if len(all) != 1 {
		t.Fatalf("expected 1 issue after reload, got %d", len(all))
	}
	if len(all[0].FixAttempts) != 1 {
		t.Fatalf("expected 1 fix attempt, got %d", len(all[0].FixAttempts))
	}
}
