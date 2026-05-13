package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bazelment/yoloswe/symphony/config"
	"github.com/bazelment/yoloswe/symphony/model"
)

type startupTracker struct {
	fetchErr       error
	states         []string
	projectSlug    string
	terminalIssues []model.Issue
	fetchCount     int
}

func (t *startupTracker) FetchCandidateIssues(context.Context, []string, string) ([]model.Issue, error) {
	return nil, nil
}

func (t *startupTracker) FetchIssueStatesByIDs(context.Context, []string) ([]model.Issue, error) {
	return nil, nil
}

func (t *startupTracker) FetchIssuesByStates(_ context.Context, states []string, projectSlug string) ([]model.Issue, error) {
	t.fetchCount++
	t.states = slices.Clone(states)
	t.projectSlug = projectSlug
	if t.fetchErr != nil {
		return nil, t.fetchErr
	}
	return slices.Clone(t.terminalIssues), nil
}

func TestStartupCleanupSkipsTrackerWithoutTerminalStates(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.TerminalStates = nil
	tracker := &startupTracker{}
	o := New(func() *config.ServiceConfig { return cfg }, tracker, stateTestClock{}, nil)

	o.startupCleanup(t.Context(), cfg)

	if tracker.fetchCount != 0 {
		t.Fatalf("fetchCount = %d, want 0", tracker.fetchCount)
	}
}

func TestStartupCleanupRemovesTerminalWorkspaces(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.WorkspaceRoot = t.TempDir()
	cfg.TerminalStates = []string{"Done", "Cancelled"}
	cfg.TrackerProjectSlug = "SYM"
	terminalPath := filepath.Join(cfg.WorkspaceRoot, model.SanitizeIdentifier("SYM-1"))
	activePath := filepath.Join(cfg.WorkspaceRoot, model.SanitizeIdentifier("SYM-2"))
	mkdirAll(t, terminalPath)
	mkdirAll(t, activePath)

	tracker := &startupTracker{
		terminalIssues: []model.Issue{{ID: "issue-1", Identifier: "SYM-1", State: "Done"}},
	}
	o := New(func() *config.ServiceConfig { return cfg }, tracker, stateTestClock{}, nil)

	o.startupCleanup(t.Context(), cfg)

	if tracker.fetchCount != 1 {
		t.Fatalf("fetchCount = %d, want 1", tracker.fetchCount)
	}
	if !slices.Equal(tracker.states, cfg.TerminalStates) {
		t.Fatalf("states = %v, want %v", tracker.states, cfg.TerminalStates)
	}
	if tracker.projectSlug != "SYM" {
		t.Fatalf("projectSlug = %q, want SYM", tracker.projectSlug)
	}
	if _, err := os.Stat(terminalPath); !os.IsNotExist(err) {
		t.Fatalf("terminal workspace still exists or stat failed: %v", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active workspace stat failed: %v", err)
	}
}

func TestStartupCleanupKeepsWorkspacesWhenTrackerFetchFails(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.WorkspaceRoot = t.TempDir()
	cfg.TerminalStates = []string{"Done"}
	workspacePath := filepath.Join(cfg.WorkspaceRoot, model.SanitizeIdentifier("SYM-1"))
	mkdirAll(t, workspacePath)

	tracker := &startupTracker{
		fetchErr: errors.New("tracker unavailable"),
	}
	o := New(func() *config.ServiceConfig { return cfg }, tracker, stateTestClock{}, nil)

	o.startupCleanup(t.Context(), cfg)

	if tracker.fetchCount != 1 {
		t.Fatalf("fetchCount = %d, want 1", tracker.fetchCount)
	}
	if _, err := os.Stat(workspacePath); err != nil {
		t.Fatalf("workspace stat failed: %v", err)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}
