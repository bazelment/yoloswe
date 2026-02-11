package issue

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// trackerFile is the persistent JSON structure.
type trackerFile struct {
	Issues       []*Issue `json:"issues"`
	ReviewedRuns []int64  `json:"reviewed_runs,omitempty"`
}

// Tracker is the JSON-backed known-issues database.
type Tracker struct {
	issues       map[string]*Issue
	reviewedRuns map[int64]bool
	filePath     string
	mu           sync.Mutex
}

// NewTracker creates a Tracker backed by the given file path.
// If the file exists, it loads existing issues.
func NewTracker(filePath string) (*Tracker, error) {
	t := &Tracker{
		issues:       make(map[string]*Issue),
		reviewedRuns: make(map[int64]bool),
		filePath:     filePath,
	}
	if err := t.load(); err != nil {
		return nil, err
	}
	return t, nil
}

// load reads existing issues from disk. No error if file doesn't exist.
func (t *Tracker) load() error {
	data, err := os.ReadFile(t.filePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read tracker file: %w", err)
	}
	if len(data) == 0 {
		return nil
	}

	var f trackerFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse tracker file: %w", err)
	}

	for _, issue := range f.Issues {
		t.issues[issue.Signature] = issue
	}
	for _, id := range f.ReviewedRuns {
		t.reviewedRuns[id] = true
	}
	return nil
}

// Save persists the current issue state to disk.
func (t *Tracker) Save() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.saveLocked()
}

func (t *Tracker) saveLocked() error {
	ids := make([]int64, 0, len(t.reviewedRuns))
	for id := range t.reviewedRuns {
		ids = append(ids, id)
	}

	f := trackerFile{
		Issues:       make([]*Issue, 0, len(t.issues)),
		ReviewedRuns: ids,
	}
	for _, issue := range t.issues {
		f.Issues = append(f.Issues, issue)
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tracker: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(t.filePath), 0755); err != nil {
		return fmt.Errorf("create tracker dir: %w", err)
	}

	return os.WriteFile(t.filePath, data, 0644)
}

// ReconcileResult holds the result of reconciling failures with known issues.
type ReconcileResult struct {
	New      []*Issue
	Updated  []*Issue
	Resolved []*Issue
}

// Reconcile updates the issue database with the latest CI failures.
// It returns new, updated, and resolved issues.
func (t *Tracker) Reconcile(failures []CIFailure) *ReconcileResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := &ReconcileResult{}
	seenSignatures := make(map[string]bool)
	updatedSeen := make(map[string]bool) // prevent duplicate entries in Updated

	for i := range failures {
		f := &failures[i]
		seenSignatures[f.Signature] = true

		existing, ok := t.issues[f.Signature]
		if !ok {
			// New issue
			issue := &Issue{
				ID:        generateID(f.Signature),
				Signature: f.Signature,
				Category:  f.Category,
				Summary:   f.Summary,
				Details:   f.Details,
				File:      f.File,
				Line:      f.Line,
				ErrorCode: f.ErrorCode,
				RunURL:    f.RunURL,
				JobName:   f.JobName,
				Status:    StatusNew,
				FirstSeen: f.Timestamp,
				LastSeen:  f.Timestamp,
				SeenCount: 1,
			}
			t.issues[f.Signature] = issue
			result.New = append(result.New, issue)
			continue
		}

		// Existing issue — update counters and latest metadata
		if f.Timestamp.Before(existing.FirstSeen) {
			existing.FirstSeen = f.Timestamp
		}
		if f.Timestamp.After(existing.LastSeen) {
			existing.LastSeen = f.Timestamp
		}
		existing.SeenCount++
		if f.Details != "" {
			existing.Details = f.Details
		}
		// Update to latest RunURL and JobName
		if f.RunURL != "" {
			existing.RunURL = f.RunURL
		}
		if f.JobName != "" {
			existing.JobName = f.JobName
		}

		// Only add to Updated list once per reconcile
		if updatedSeen[f.Signature] {
			continue
		}
		updatedSeen[f.Signature] = true

		switch existing.Status {
		case StatusFixMerged, StatusVerified:
			// Fix didn't hold — issue recurred
			existing.Status = StatusRecurred
			existing.ResolvedAt = nil
			result.Updated = append(result.Updated, existing)
		default:
			result.Updated = append(result.Updated, existing)
		}
	}

	// Check for resolved issues (fix_merged issues no longer seen)
	for sig, issue := range t.issues {
		if seenSignatures[sig] {
			continue
		}
		if issue.Status == StatusFixMerged {
			issue.Status = StatusVerified
			now := time.Now()
			issue.ResolvedAt = &now
			result.Resolved = append(result.Resolved, issue)
		}
	}

	return result
}

// GetActionable returns issues that need fix agents (new or recurred).
func (t *Tracker) GetActionable() []*Issue {
	t.mu.Lock()
	defer t.mu.Unlock()

	var result []*Issue
	for _, issue := range t.issues {
		if issue.Status == StatusNew || issue.Status == StatusRecurred {
			result = append(result, issue)
		}
	}
	return result
}

// GetPendingMerge returns issues with approved PRs ready to merge.
func (t *Tracker) GetPendingMerge() []*Issue {
	t.mu.Lock()
	defer t.mu.Unlock()

	var result []*Issue
	for _, issue := range t.issues {
		if issue.Status == StatusFixApproved {
			result = append(result, issue)
		}
	}
	return result
}

// GetAwaitingVerification returns issues with merged PRs awaiting CI verification.
func (t *Tracker) GetAwaitingVerification() []*Issue {
	t.mu.Lock()
	defer t.mu.Unlock()

	var result []*Issue
	for _, issue := range t.issues {
		if issue.Status == StatusFixMerged {
			result = append(result, issue)
		}
	}
	return result
}

// Get returns an issue by signature, or nil if not found.
func (t *Tracker) Get(signature string) *Issue {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.issues[signature]
}

// UpdateStatus changes the status of an issue by signature.
func (t *Tracker) UpdateStatus(signature string, status Status) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if issue, ok := t.issues[signature]; ok {
		issue.Status = status
	}
}

// AddFixAttempt records a fix attempt for an issue.
func (t *Tracker) AddFixAttempt(signature string, attempt FixAttempt) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if issue, ok := t.issues[signature]; ok {
		issue.FixAttempts = append(issue.FixAttempts, attempt)
	}
}

// All returns all tracked issues.
func (t *Tracker) All() []*Issue {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := make([]*Issue, 0, len(t.issues))
	for _, issue := range t.issues {
		result = append(result, issue)
	}
	return result
}

// GetByID returns an issue by its short ID, or nil if not found.
func (t *Tracker) GetByID(id string) *Issue {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, iss := range t.issues {
		if iss.ID == id {
			return iss
		}
	}
	return nil
}

// Dismiss marks an issue as wont_fix with a reason.
func (t *Tracker) Dismiss(id string, reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, iss := range t.issues {
		if iss.ID == id {
			iss.Status = StatusWontFix
			iss.DismissReason = reason
			return nil
		}
	}
	return fmt.Errorf("issue %s not found", id)
}

// Reopen sets a dismissed issue back to new status.
func (t *Tracker) Reopen(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, iss := range t.issues {
		if iss.ID == id {
			if iss.Status != StatusWontFix {
				return fmt.Errorf("issue %s is not dismissed (status: %s)", id, iss.Status)
			}
			iss.Status = StatusNew
			iss.DismissReason = ""
			return nil
		}
	}
	return fmt.Errorf("issue %s not found", id)
}

// IsRunReviewed returns true if the run has been triaged.
func (t *Tracker) IsRunReviewed(runID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reviewedRuns[runID]
}

// MarkRunsReviewed records run IDs as triaged.
func (t *Tracker) MarkRunsReviewed(runIDs []int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range runIDs {
		t.reviewedRuns[id] = true
	}
}

// ReviewedRunIDs returns all reviewed run IDs (for persistence).
func (t *Tracker) ReviewedRunIDs() []int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]int64, 0, len(t.reviewedRuns))
	for id := range t.reviewedRuns {
		ids = append(ids, id)
	}
	return ids
}

// PruneReviewedRuns removes run IDs not present in activeIDs.
func (t *Tracker) PruneReviewedRuns(activeIDs map[int64]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range t.reviewedRuns {
		if !activeIDs[id] {
			delete(t.reviewedRuns, id)
		}
	}
}

// generateID creates a short ID from a signature.
func generateID(signature string) string {
	h := sha256.Sum256([]byte(signature))
	return fmt.Sprintf("%x", h[:4])
}
