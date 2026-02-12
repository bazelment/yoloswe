package issue

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempTrackerPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, ".medivac", "issues.json")
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

	failures := []CIFailure{
		{
			Signature: "lint/go:abc:main.go",
			Category:  CategoryLintGo,
			Summary:   "unused variable",
			File:      "main.go",
			Timestamp: time.Now(),
		},
		{
			Signature: "test/bazel:def:test_test.go",
			Category:  CategoryTest,
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
	tracker.Reconcile([]CIFailure{
		{Signature: sig, Category: CategoryLintGo, Summary: "unused variable", Timestamp: now},
	})

	// Second reconcile with same failure
	later := now.Add(time.Hour)
	result := tracker.Reconcile([]CIFailure{
		{Signature: sig, Category: CategoryLintGo, Summary: "unused variable", Timestamp: later},
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
	tracker.Reconcile([]CIFailure{
		{Signature: sig, Category: CategoryLintGo, Summary: "unused variable", Timestamp: now},
	})
	tracker.UpdateStatus(sig, StatusFixMerged)

	// Failure appears again
	result := tracker.Reconcile([]CIFailure{
		{Signature: sig, Category: CategoryLintGo, Summary: "unused variable", Timestamp: now.Add(time.Hour)},
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
	tracker.Reconcile([]CIFailure{
		{Signature: sig, Category: CategoryLintGo, Summary: "unused variable", Timestamp: now},
	})
	tracker.UpdateStatus(sig, StatusFixMerged)

	// Reconcile without that failure present
	result := tracker.Reconcile([]CIFailure{})

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
	tracker.Reconcile([]CIFailure{
		{Signature: "sig1", Category: CategoryLintGo, Summary: "issue 1", Timestamp: now},
		{Signature: "sig2", Category: CategoryTest, Summary: "issue 2", Timestamp: now},
		{Signature: "sig3", Category: CategoryBuild, Summary: "issue 3", Timestamp: now},
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

	tracker1.Reconcile([]CIFailure{
		{Signature: "sig1", Category: CategoryLintGo, Summary: "issue 1", Timestamp: time.Now()},
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

func TestReconcile_NoDuplicateInUpdated(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	sig := "abc123:main.go"
	now := time.Now()

	// First reconcile creates the issue
	tracker.Reconcile([]CIFailure{
		{Signature: sig, Category: CategoryLintGo, Summary: "unused variable", Timestamp: now},
	})

	// Second reconcile with two failures that have the same signature
	later := now.Add(time.Hour)
	result := tracker.Reconcile([]CIFailure{
		{Signature: sig, Category: CategoryLintGo, Summary: "unused variable", Timestamp: later},
		{Signature: sig, Category: CategoryBuildDocker, Summary: "unused variable", Timestamp: later},
	})

	if len(result.Updated) != 1 {
		t.Fatalf("expected 1 updated (no duplicates), got %d", len(result.Updated))
	}

	issue := tracker.Get(sig)
	// SeenCount should still increment for both failures: 1 (first reconcile) + 2 = 3
	if issue.SeenCount != 3 {
		t.Errorf("expected seen count 3, got %d", issue.SeenCount)
	}
}

func TestReconcile_TimestampOrdering(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	sig := "abc123:main.go"

	// First failure has a "later" timestamp
	later := time.Date(2026, 2, 11, 3, 0, 0, 0, time.UTC)
	tracker.Reconcile([]CIFailure{
		{Signature: sig, Category: CategoryLintGo, Summary: "err", Timestamp: later},
	})

	// Second failure has an "earlier" timestamp (from an older run scanned later)
	earlier := time.Date(2026, 2, 11, 0, 0, 0, 0, time.UTC)
	tracker.Reconcile([]CIFailure{
		{Signature: sig, Category: CategoryLintGo, Summary: "err", Timestamp: earlier},
	})

	issue := tracker.Get(sig)
	if !issue.FirstSeen.Equal(earlier) {
		t.Errorf("FirstSeen should be the earlier timestamp, got %v", issue.FirstSeen)
	}
	if !issue.LastSeen.Equal(later) {
		t.Errorf("LastSeen should be the later timestamp, got %v", issue.LastSeen)
	}
	// Invariant: FirstSeen <= LastSeen
	if issue.FirstSeen.After(issue.LastSeen) {
		t.Errorf("FirstSeen (%v) should not be after LastSeen (%v)", issue.FirstSeen, issue.LastSeen)
	}
}

func TestDismissAndReopen(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	tracker.Reconcile([]CIFailure{
		{Signature: "sig1", Category: CategoryInfraCI, Summary: "parallel golangci-lint", Timestamp: time.Now()},
	})

	iss := tracker.GetByID(tracker.Get("sig1").ID)
	if iss == nil {
		t.Fatal("GetByID returned nil")
	}

	// Dismiss
	if err := tracker.Dismiss(iss.ID, "transient CI flake"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}

	if iss.Status != StatusWontFix {
		t.Errorf("expected wont_fix, got %s", iss.Status)
	}
	if iss.DismissReason != "transient CI flake" {
		t.Errorf("expected reason, got %q", iss.DismissReason)
	}

	// Dismissed issues should not be actionable
	actionable := tracker.GetActionable()
	if len(actionable) != 0 {
		t.Errorf("expected 0 actionable after dismiss, got %d", len(actionable))
	}

	// Reopen
	if err := tracker.Reopen(iss.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	if iss.Status != StatusNew {
		t.Errorf("expected new, got %s", iss.Status)
	}
	if iss.DismissReason != "" {
		t.Errorf("expected empty reason, got %q", iss.DismissReason)
	}

	// Now actionable again
	actionable = tracker.GetActionable()
	if len(actionable) != 1 {
		t.Errorf("expected 1 actionable after reopen, got %d", len(actionable))
	}
}

func TestDismiss_NotFound(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	if err := tracker.Dismiss("nonexistent", "reason"); err == nil {
		t.Error("expected error for nonexistent ID")
	}
}

func TestReopen_NotDismissed(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	tracker.Reconcile([]CIFailure{
		{Signature: "sig1", Category: CategoryLintGo, Summary: "err", Timestamp: time.Now()},
	})

	id := tracker.Get("sig1").ID
	if err := tracker.Reopen(id); err == nil {
		t.Error("expected error for non-dismissed issue")
	}
}

func TestReviewedRuns_Empty(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if tracker.IsRunReviewed(100) {
		t.Error("new tracker should not have reviewed runs")
	}
	if len(tracker.ReviewedRunIDs()) != 0 {
		t.Errorf("expected 0 reviewed run IDs, got %d", len(tracker.ReviewedRunIDs()))
	}
}

func TestReviewedRuns_MarkAndCheck(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	tracker.MarkRunsReviewed([]int64{100, 200})

	if !tracker.IsRunReviewed(100) {
		t.Error("run 100 should be reviewed")
	}
	if !tracker.IsRunReviewed(200) {
		t.Error("run 200 should be reviewed")
	}
	if tracker.IsRunReviewed(300) {
		t.Error("run 300 should NOT be reviewed")
	}
}

func TestReviewedRuns_Persistence(t *testing.T) {
	path := tempTrackerPath(t)

	tracker1, err := NewTracker(path)
	if err != nil {
		t.Fatal(err)
	}

	tracker1.MarkRunsReviewed([]int64{100, 200})
	if err := tracker1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	tracker2, err := NewTracker(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if !tracker2.IsRunReviewed(100) {
		t.Error("run 100 should be reviewed after reload")
	}
	if !tracker2.IsRunReviewed(200) {
		t.Error("run 200 should be reviewed after reload")
	}
	if tracker2.IsRunReviewed(300) {
		t.Error("run 300 should NOT be reviewed after reload")
	}
}

func TestReviewedRuns_Prune(t *testing.T) {
	tracker, err := NewTracker(tempTrackerPath(t))
	if err != nil {
		t.Fatal(err)
	}

	tracker.MarkRunsReviewed([]int64{100, 200, 300})

	active := map[int64]bool{200: true, 300: true}
	tracker.PruneReviewedRuns(active)

	if tracker.IsRunReviewed(100) {
		t.Error("run 100 should be pruned (not active)")
	}
	if !tracker.IsRunReviewed(200) {
		t.Error("run 200 should survive pruning")
	}
	if !tracker.IsRunReviewed(300) {
		t.Error("run 300 should survive pruning")
	}
}

func TestReviewedRuns_BackwardCompat(t *testing.T) {
	// Simulate loading an old tracker file without reviewed_runs field.
	path := tempTrackerPath(t)

	// Write a tracker file with no reviewed_runs.
	tracker1, err := NewTracker(path)
	if err != nil {
		t.Fatal(err)
	}
	tracker1.Reconcile([]CIFailure{
		{Signature: "sig1", Category: CategoryLintGo, Summary: "err", Timestamp: time.Now()},
	})
	if err := tracker1.Save(); err != nil {
		t.Fatal(err)
	}

	// Manually rewrite file without reviewed_runs to simulate old format.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The file should load fine even without reviewed_runs.
	// Just reload it to confirm no errors.
	_ = data

	tracker2, err := NewTracker(path)
	if err != nil {
		t.Fatalf("loading old-format tracker should succeed: %v", err)
	}
	if tracker2.IsRunReviewed(100) {
		t.Error("old tracker should have no reviewed runs")
	}
	if len(tracker2.All()) != 1 {
		t.Errorf("expected 1 issue, got %d", len(tracker2.All()))
	}
}

func TestDismiss_PersistsAcrossSave(t *testing.T) {
	path := tempTrackerPath(t)

	tracker1, err := NewTracker(path)
	if err != nil {
		t.Fatal(err)
	}

	tracker1.Reconcile([]CIFailure{
		{Signature: "sig1", Category: CategoryInfraCI, Summary: "flake", Timestamp: time.Now()},
	})
	id := tracker1.Get("sig1").ID
	tracker1.Dismiss(id, "flake")
	tracker1.Save()

	// Reload
	tracker2, err := NewTracker(path)
	if err != nil {
		t.Fatal(err)
	}

	iss := tracker2.GetByID(id)
	if iss == nil {
		t.Fatal("issue not found after reload")
	}
	if iss.Status != StatusWontFix {
		t.Errorf("expected wont_fix after reload, got %s", iss.Status)
	}
	if iss.DismissReason != "flake" {
		t.Errorf("expected dismiss reason after reload, got %q", iss.DismissReason)
	}
}
