package state

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Lane is one unit of work moving through the run's phases.
//
// This struct shares state.json with the skill's ledger.py, so field names and
// JSON tags are a cross-tool contract — do not rename them unilaterally.
//
// Extended fields (PR, PRHead, ApprovalSHA, Checks, ForkSHA, PhaseStartSHA,
// Round, LastVerifiedAt) are ABSENT-BY-DEFAULT: ledger.py's `add` writes a fixed
// key set that does not include them, so a lane created by the other tool simply
// will not have them. Never treat a missing field as a signal; backfill it on
// reconcile instead.
type Lane struct {
	Sessions       map[string]string          `json:"sessions"`
	extra          map[string]json.RawMessage `json:"-"`
	Worktree       string                     `json:"worktree"`
	WindowID       string                     `json:"window_id"`
	Brief          string                     `json:"brief"`
	Priority       Priority                   `json:"priority"`
	Status         Status                     `json:"status"`
	Phase          string                     `json:"phase"`
	Title          string                     `json:"title"`
	ID             string                     `json:"id"`
	LastVerifiedAt string                     `json:"last_verified_at,omitempty"`
	Branch         string                     `json:"branch"`
	MergeSHA       string                     `json:"merge_sha"`
	PRHead         string                     `json:"pr_head,omitempty"`
	ApprovalSHA    string                     `json:"approval_sha,omitempty"`
	Checks         string                     `json:"checks,omitempty"`
	ForkSHA        string                     `json:"fork_sha,omitempty"`
	PhaseStartSHA  string                     `json:"phase_start_sha,omitempty"`
	DependsOn      []string                   `json:"depends_on"`
	Notes          []string                   `json:"notes"`
	PR             int                        `json:"pr,omitempty"`
	Round          int                        `json:"round,omitempty"`
}

// ApprovalStale reports whether the recorded approval no longer covers the
// current head. Unknown values are treated as stale: absence is never evidence
// of approval.
func (l *Lane) ApprovalStale() bool {
	if l.ApprovalSHA == "" || l.PRHead == "" {
		return true
	}
	return l.ApprovalSHA != l.PRHead
}

// laneAlias avoids infinite recursion in the custom (Un)MarshalJSON below.
type laneAlias Lane

// UnmarshalJSON decodes a lane and retains any unmodelled keys so they survive
// a write-back. ledger.py rewrites the whole file on every subcommand, so
// dropping a key here would delete another tool's data.
func (l *Lane) UnmarshalJSON(b []byte) error {
	var alias laneAlias
	if err := json.Unmarshal(b, &alias); err != nil {
		return err
	}
	*l = Lane(alias)

	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for _, known := range laneKnownKeys() {
		delete(all, known)
	}
	if len(all) > 0 {
		l.extra = all
	}
	return nil
}

// MarshalJSON re-emits retained unknown keys alongside the modelled ones.
func (l Lane) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(laneAlias(l))
	if err != nil {
		return nil, err
	}
	if len(l.extra) == 0 {
		return b, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(b, &merged); err != nil {
		return nil, err
	}
	for k, v := range l.extra {
		if _, clash := merged[k]; clash {
			continue // a modelled field always wins over a retained copy
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

// laneKnownKeys lists the JSON keys this struct models, derived from the struct
// tags so the two cannot drift apart.
func laneKnownKeys() []string {
	keys := structJSONKeys(Lane{})
	sort.Strings(keys)
	return keys
}

// Validate reports whether a lane is well formed enough to act on.
func (l *Lane) Validate() error {
	if l.ID == "" {
		return fmt.Errorf("lane has no id")
	}
	// watch_lanes.sh parses signal filenames as <lane>.<phase>.<sig> and takes
	// the lane id as everything before the FIRST dot, so a dotted id silently
	// misroutes every signal that lane ever emits.
	for _, r := range l.ID {
		if r == '.' {
			return fmt.Errorf("lane %q: id must not contain '.' (signal filenames split on it)", l.ID)
		}
	}
	switch l.Status {
	case StatusPlanned, StatusRunning, StatusBlocked, StatusFailed, StatusDone:
	default:
		return fmt.Errorf("lane %q: unknown status %q", l.ID, l.Status)
	}
	return nil
}
