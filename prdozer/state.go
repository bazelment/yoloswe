// Package prdozer watches GitHub pull requests and keeps them merge-ready by
// invoking the /pr-polish skill whenever the PR's base moves, CI fails, or new
// review comments arrive. The Go side is orchestration only — the actual code
// fixing is delegated to the agent.
package prdozer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// LastAction is the prdozer-side action recorded after a tick.
type LastAction string

const (
	LastActionInit     LastAction = ""
	LastActionIdle     LastAction = "idle"
	LastActionPolished LastAction = "polished"
	LastActionMerged   LastAction = "merged"
	LastActionFailed   LastAction = "failed"
	LastActionDryRun   LastAction = "dry_run"
)

// State is the per-PR persisted state, used to detect change between ticks
// and to back off after repeated failures.
type State struct {
	LastCheckAt         time.Time  `json:"last_check_at,omitempty"`
	CooldownUntil       time.Time  `json:"cooldown_until,omitempty"`
	LastSeenHeadSHA     string     `json:"last_seen_head_sha,omitempty"`
	LastSeenBaseSHA     string     `json:"last_seen_base_sha,omitempty"`
	LastAction          LastAction `json:"last_action,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	Repo                string     `json:"repo,omitempty"`
	LastSeenCommentIDs  []string   `json:"last_seen_comment_ids,omitempty"`
	LastSeenCIRunIDs    []int64    `json:"last_seen_ci_run_ids,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures,omitempty"`
	PRNumber            int        `json:"pr_number"`
}

// LoadState reads the state file at path. Returns a zero State (no error) when
// the file does not exist.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	return &s, nil
}

// Save writes the state to path, creating parent directories as needed.
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write state %s: %w", path, err)
	}
	return nil
}

// MergeSeenComments adds new IDs to LastSeenCommentIDs, deduplicating and
// sorting for stable output.
func (s *State) MergeSeenComments(ids []string) {
	seen := make(map[string]bool, len(s.LastSeenCommentIDs)+len(ids))
	for _, id := range s.LastSeenCommentIDs {
		seen[id] = true
	}
	for _, id := range ids {
		seen[id] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	s.LastSeenCommentIDs = out
}

// MergeSeenRuns adds new run IDs to LastSeenCIRunIDs, deduplicating and
// sorting for stable output.
func (s *State) MergeSeenRuns(ids []int64) {
	seen := make(map[int64]bool, len(s.LastSeenCIRunIDs)+len(ids))
	for _, id := range s.LastSeenCIRunIDs {
		seen[id] = true
	}
	for _, id := range ids {
		seen[id] = true
	}
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	s.LastSeenCIRunIDs = out
}

// StatePath returns the canonical state-file path for a given repo and PR.
// Mirrors the layout used by the /pr-polish skill so both files coexist under
// the same project directory.
func StatePath(repo string, prNumber int) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	dir := filepath.Join(home, ".bramble", "projects", fmt.Sprintf("%s-%d", repo, prNumber))
	return filepath.Join(dir, "prdozer-state.json")
}
