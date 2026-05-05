package jiradozer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// RuntimeState is the team-mode supervisor state persisted immediately before
// an exec restart.
//
//nolint:govet // fieldalignment: JSON shape is grouped by supervisor concern.
type RuntimeState struct {
	ActiveWorkflow    []ManagedWorkflowSnapshot `json:"active_workflows"`
	PreservedWorktree []PreservedWorktree       `json:"preserved_worktrees,omitempty"`
}

// ManagedWorkflowSnapshot is a serializable view of one active child workflow.
type ManagedWorkflowSnapshot struct {
	Issue        *tracker.Issue `json:"issue"`
	StartedAt    time.Time      `json:"started_at"`
	WorktreePath string         `json:"worktree_path"`
	Branch       string         `json:"branch"`
	PID          int            `json:"pid"`
}

// WriteRuntimeStateAtomically writes state to path using temp-file + rename.
func WriteRuntimeStateAtomically(path string, state RuntimeState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create runtime state dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal runtime state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create runtime state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write runtime state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close runtime state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename runtime state: %w", err)
	}
	return nil
}

// LoadRuntimeState reads state written by WriteRuntimeStateAtomically.
func LoadRuntimeState(path string) (*RuntimeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read runtime state: %w", err)
	}
	var state RuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse runtime state: %w", err)
	}
	return &state, nil
}

func cloneConfig(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	cp := *cfg
	cp.Source.Filters = cloneStringMap(cfg.Source.Filters)
	cp.SkipPhases = append([]string(nil), cfg.SkipPhases...)
	cp.Plan.Rounds = append([]RoundConfig(nil), cfg.Plan.Rounds...)
	cp.Build.Rounds = append([]RoundConfig(nil), cfg.Build.Rounds...)
	cp.CreatePR.Rounds = append([]RoundConfig(nil), cfg.CreatePR.Rounds...)
	cp.Validate.Rounds = append([]RoundConfig(nil), cfg.Validate.Rounds...)
	cp.Ship.Rounds = append([]RoundConfig(nil), cfg.Ship.Rounds...)
	return &cp
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneIssue(issue *tracker.Issue) *tracker.Issue {
	if issue == nil {
		return nil
	}
	cp := *issue
	cp.Labels = append([]string(nil), issue.Labels...)
	cp.LabelIDs = append([]string(nil), issue.LabelIDs...)
	cp.Description = cloneStringPtr(issue.Description)
	cp.BranchName = cloneStringPtr(issue.BranchName)
	cp.URL = cloneStringPtr(issue.URL)
	return &cp
}

func cloneStringPtr(s *string) *string {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}
