// Package local provides a file-backed IssueTracker implementation.
// Issues are stored as individual JSON files in a directory, with no
// external dependencies. This enables jiradozer to run workflows from
// a CLI --description flag without needing Linear or any other tracker.
package local

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// issueFile is the on-disk JSON format for a single issue.
type issueFile struct {
	Issue    issueData `json:"issue"`
	Comments []comment `json:"comments"`
}

type issueData struct {
	ID          string   `json:"id"`
	Identifier  string   `json:"identifier"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	State       string   `json:"state"`
	TeamID      string   `json:"team_id"`
	Labels      []string `json:"labels"`
}

type comment struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	UserName  string    `json:"user_name"`
	IsSelf    bool      `json:"is_self"`
}

const (
	stateIDInProgress = "local-in-progress"
	stateIDInReview   = "local-in-review"
	stateIDDone       = "local-done"

	stateNameTodo       = "Todo"
	stateNameInProgress = "In Progress"
	stateNameInReview   = "In Review"
	stateNameDone       = "Done"
)

// Tracker is a file-backed IssueTracker. Each issue is stored as a JSON
// file under the configured directory, with a counter file for ID generation.
type Tracker struct {
	dir         string
	counterPath string
	mu          sync.Mutex
}

// NewTracker creates a local file-backed tracker. The directory is created
// if it does not exist.
func NewTracker(dir string) (*Tracker, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create tracker dir: %w", err)
	}
	return &Tracker{
		dir:         dir,
		counterPath: filepath.Join(filepath.Dir(dir), "next_id"),
	}, nil
}

// CreateIssue creates a new issue with an auto-incremented LOCAL-N identifier.
// This method is not part of the IssueTracker interface.
//
// NOTE: The mutex provides goroutine safety but not cross-process safety.
// Running multiple jiradozer processes against the same work directory is
// unsupported and may corrupt state.
func (t *Tracker) CreateIssue(title, description string) (*tracker.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	id, err := t.nextID()
	if err != nil {
		return nil, err
	}

	issueID := fmt.Sprintf("local-%d", id)
	identifier := fmt.Sprintf("LOCAL-%d", id)

	f := issueFile{
		Issue: issueData{
			ID:          issueID,
			Identifier:  identifier,
			Title:       title,
			Description: description,
			State:       stateNameTodo,
			TeamID:      "local",
			Labels:      []string{},
		},
	}

	if err := t.writeFile(issueID, &f); err != nil {
		return nil, err
	}

	desc := description
	return &tracker.Issue{
		ID:          issueID,
		Identifier:  identifier,
		Title:       title,
		Description: &desc,
		State:       stateNameTodo,
		TeamID:      "local",
		Labels:      []string{},
	}, nil
}

// --- IssueTracker interface ---

func (t *Tracker) FetchIssue(_ context.Context, identifier string) (*tracker.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Fast path: LOCAL-N identifiers map directly to local-N.json files.
	issueID := strings.ToLower(identifier)
	if _, err := t.filePath(issueID); err != nil {
		return nil, err // path traversal attempt
	}
	if f, err := t.readFile(issueID); err == nil {
		return toTrackerIssue(&f.Issue), nil
	}

	// Slow path: full scan for non-standard identifiers.
	files, err := t.listFiles()
	if err != nil {
		return nil, err
	}
	for i := range files {
		if files[i].Issue.Identifier == identifier {
			return toTrackerIssue(&files[i].Issue), nil
		}
	}
	return nil, fmt.Errorf("issue %q not found", identifier)
}

func (t *Tracker) ListIssues(_ context.Context, filter tracker.IssueFilter) ([]*tracker.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	files, err := t.listFiles()
	if err != nil {
		return nil, err
	}

	var stateSet map[string]bool
	if states := tracker.SplitCSV(filter.Filters[tracker.FilterState]); len(states) > 0 {
		stateSet = make(map[string]bool, len(states))
		for _, s := range states {
			stateSet[s] = true
		}
	}

	var out []*tracker.Issue
	for i := range files {
		if len(stateSet) > 0 && !stateSet[files[i].Issue.State] {
			continue
		}
		out = append(out, toTrackerIssue(&files[i].Issue))
	}
	return out, nil
}

func (t *Tracker) FetchComments(_ context.Context, issueID string, since time.Time) ([]tracker.Comment, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	f, err := t.readFile(issueID)
	if err != nil {
		return nil, err
	}

	var out []tracker.Comment
	for _, c := range f.Comments {
		if !c.CreatedAt.Before(since) {
			out = append(out, tracker.Comment{
				ID:        c.ID,
				Body:      c.Body,
				UserName:  c.UserName,
				IsSelf:    c.IsSelf,
				CreatedAt: c.CreatedAt,
			})
		}
	}
	return out, nil
}

func (t *Tracker) FetchWorkflowStates(_ context.Context, _ string) ([]tracker.WorkflowState, error) {
	return []tracker.WorkflowState{
		{ID: stateIDInProgress, Name: stateNameInProgress, Type: "started"},
		{ID: stateIDInReview, Name: stateNameInReview, Type: "started"},
		{ID: stateIDDone, Name: stateNameDone, Type: "completed"},
	}, nil
}

func (t *Tracker) PostComment(_ context.Context, issueID string, body string) (tracker.Comment, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	f, err := t.readFile(issueID)
	if err != nil {
		return tracker.Comment{}, err
	}

	now := time.Now()
	c := comment{
		ID:        fmt.Sprintf("c-%d", len(f.Comments)+1),
		Body:      body,
		UserName:  "jiradozer",
		IsSelf:    true,
		CreatedAt: now,
	}
	f.Comments = append(f.Comments, c)

	if err := t.writeFile(issueID, f); err != nil {
		return tracker.Comment{}, err
	}
	return tracker.Comment{
		ID:        c.ID,
		Body:      c.Body,
		UserName:  c.UserName,
		IsSelf:    true,
		CreatedAt: now,
	}, nil
}

func (t *Tracker) UpdateIssueState(_ context.Context, issueID string, stateID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	f, err := t.readFile(issueID)
	if err != nil {
		return err
	}

	// Map state IDs back to names for readability in the JSON file.
	switch stateID {
	case stateIDInProgress:
		f.Issue.State = stateNameInProgress
	case stateIDInReview:
		f.Issue.State = stateNameInReview
	case stateIDDone:
		f.Issue.State = stateNameDone
	default:
		f.Issue.State = stateID
	}

	return t.writeFile(issueID, f)
}

// AddLabel adds a label to a local issue. It is idempotent.
func (t *Tracker) AddLabel(_ context.Context, issueID string, label string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	f, err := t.readFile(issueID)
	if err != nil {
		return err
	}

	for _, l := range f.Issue.Labels {
		if l == label {
			return nil // already present — idempotent
		}
	}
	f.Issue.Labels = append(f.Issue.Labels, label)
	return t.writeFile(issueID, f)
}

// RemoveLabel removes a label from a local issue. It is idempotent.
func (t *Tracker) RemoveLabel(_ context.Context, issueID string, label string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	f, err := t.readFile(issueID)
	if err != nil {
		return err
	}

	filtered := f.Issue.Labels[:0]
	removed := false
	for _, l := range f.Issue.Labels {
		if l == label {
			removed = true
			continue
		}
		filtered = append(filtered, l)
	}
	if !removed {
		return nil // not present — idempotent
	}
	f.Issue.Labels = filtered
	return t.writeFile(issueID, f)
}

// --- internal helpers ---

func (t *Tracker) filePath(issueID string) (string, error) {
	p := filepath.Join(t.dir, issueID+".json")
	// Ensure the resolved path stays within the issues directory to prevent
	// path traversal via crafted identifiers (e.g. "../../../etc/passwd").
	clean := filepath.Clean(p)
	if !strings.HasPrefix(clean, filepath.Clean(t.dir)+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid issue ID %q: path escapes tracker directory", issueID)
	}
	return clean, nil
}

func (t *Tracker) readFile(issueID string) (*issueFile, error) {
	p, err := t.filePath(issueID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read issue %q: %w", issueID, err)
	}
	var f issueFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse issue %q: %w", issueID, err)
	}
	return &f, nil
}

func (t *Tracker) writeFile(issueID string, f *issueFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal issue %q: %w", issueID, err)
	}
	p, err := t.filePath(issueID)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return fmt.Errorf("write issue %q: %w", issueID, err)
	}
	return nil
}

func (t *Tracker) listFiles() ([]issueFile, error) {
	entries, err := filepath.Glob(filepath.Join(t.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []issueFile
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var f issueFile
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// nextID reads and increments the counter file. Caller must hold t.mu.
func (t *Tracker) nextID() (int, error) {
	id := 1
	if data, err := os.ReadFile(t.counterPath); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			id = n
		}
	}
	if err := os.WriteFile(t.counterPath, []byte(strconv.Itoa(id+1)+"\n"), 0o600); err != nil {
		return 0, fmt.Errorf("write counter: %w", err)
	}
	return id, nil
}

func toTrackerIssue(d *issueData) *tracker.Issue {
	desc := d.Description
	return &tracker.Issue{
		ID:          d.ID,
		Identifier:  d.Identifier,
		Title:       d.Title,
		Description: &desc,
		State:       d.State,
		TeamID:      d.TeamID,
		Labels:      d.Labels,
	}
}
