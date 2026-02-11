package issue

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/fixer/github"
)

// trackerFile is the persistent JSON structure.
type trackerFile struct {
	Issues []*Issue `json:"issues"`
}

// Tracker is the JSON-backed known-issues database.
type Tracker struct {
	issues   map[string]*Issue
	filePath string
	mu       sync.Mutex
}

// NewTracker creates a Tracker backed by the given file path.
// If the file exists, it loads existing issues.
func NewTracker(filePath string) (*Tracker, error) {
	t := &Tracker{
		issues:   make(map[string]*Issue),
		filePath: filePath,
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
	return nil
}

// Save persists the current issue state to disk.
func (t *Tracker) Save() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.saveLocked()
}

func (t *Tracker) saveLocked() error {
	f := trackerFile{
		Issues: make([]*Issue, 0, len(t.issues)),
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
func (t *Tracker) Reconcile(failures []github.CIFailure) *ReconcileResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := &ReconcileResult{}
	seenSignatures := make(map[string]bool)

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
				Status:    StatusNew,
				FirstSeen: f.Timestamp,
				LastSeen:  f.Timestamp,
				SeenCount: 1,
			}
			t.issues[f.Signature] = issue
			result.New = append(result.New, issue)
			continue
		}

		// Existing issue — update
		existing.LastSeen = f.Timestamp
		existing.SeenCount++
		if f.Details != "" {
			existing.Details = f.Details
		}

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

// generateID creates a short ID from a signature.
func generateID(signature string) string {
	h := sha256.Sum256([]byte(signature))
	return fmt.Sprintf("%x", h[:4])
}
