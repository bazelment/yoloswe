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
	// Base is the branch THIS lane forks from, overriding the run-wide
	// Config.Base. A lane whose work builds on another lane's branch (tests for
	// code that exists only on a feature branch, say) forked from the run's base
	// instead and got a tree without the code under test. Absent means "use the
	// run's base", so existing ledgers are unaffected.
	Base          string   `json:"base,omitempty"`
	MergeSHA      string   `json:"merge_sha"`
	PRHead        string   `json:"pr_head,omitempty"`
	ApprovalSHA   string   `json:"approval_sha,omitempty"`
	Checks        string   `json:"checks,omitempty"`
	ForkSHA       string   `json:"fork_sha,omitempty"`
	PhaseStartSHA string   `json:"phase_start_sha,omitempty"`
	DependsOn     []string `json:"depends_on"`
	Notes         []string `json:"notes"`
	PR            int      `json:"pr,omitempty"`
	Round         int      `json:"round,omitempty"`
	// BackupRetained records that a reap deliberately KEPT this lane's snapshot
	// because the lane was dirty when it closed. Intent is recorded, never
	// inferred: a ref kept on purpose and a ref that leaked are identical in git,
	// and refs leaked repeatedly in real runs, which is what the five-zeros audit
	// exists to catch. Both audits read this field; an unmarked ref is a leak.
	//
	// Last in the struct so the bool packs after the pointer-bearing fields
	// instead of splitting them (govet fieldalignment).
	BackupRetained bool `json:"backup_retained,omitempty"`
}

// ForkBase is the branch a new worktree for this lane must fork from: the
// lane's own Base when set, otherwise the run-wide default passed in.
func (l *Lane) ForkBase(runBase string) string {
	if l.Base != "" {
		return l.Base
	}
	return runBase
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

// Closure is the one answer to "is this lane finished".
//
// The test used to exist in three places that disagreed: the drift rule in
// verify, the skip in `reap`, and the five-zeros audit. A lane reaped while
// dirty was CLOSED to reap, drift to tick, and a LEAK to both audits -- and the
// only way to make the audit pass was to delete the snapshot the reaper kept on
// purpose. Three answers to one question is how a real signal becomes noise.
//
// It lives in state, on plain booleans, because closure is a rule about LEDGER
// FACTS. Putting it in lifecycle would have forced verify to import lifecycle,
// which (via decide -> verify) drags the destructive half of the system into the
// dependency graph of the half that only measures.
type Closure struct {
	// Why names the evidence, so a CLOSED line can be audited, not trusted.
	// First for field alignment: the string header leads, the bools pack after.
	Why string
	// Closed reports that every resource this lane owned is provably released.
	Closed bool
	// BackupRetained reports a snapshot deliberately kept after closure. It is
	// NOT a reason to call the lane unclosed: the work it holds exists nowhere
	// else, which is why the reaper keeps it.
	BackupRetained bool
}

// ClosedLane decides closure from measured facts alone.
//
// Every *Measured flag says whether the corresponding probe actually RAN.
// Unknown is never evidence of closure: an unmeasured worktree, an unlisted
// branch set or an unreachable session fleet each leave the lane not-closed. A
// bramble session can outlive its worktree directory, so an absent directory is
// not evidence that no agent is still running against it.
func (l *Lane) ClosedLane(
	worktreeMeasured, worktreeExists bool,
	branchMeasured, branchPresent bool,
	sessionsMeasured bool, liveSessions int,
	backupRetained bool,
) Closure {
	switch {
	case !l.Status.Terminal():
		return Closure{Why: "lane is not terminal"}
	case !worktreeMeasured:
		return Closure{Why: "worktree could not be measured"}
	case worktreeExists:
		return Closure{Why: "worktree still exists"}
	case !branchMeasured:
		return Closure{Why: "branch state was not measured"}
	case branchPresent:
		return Closure{Why: "branch still exists"}
	case !sessionsMeasured:
		return Closure{Why: "session probe did not run"}
	case liveSessions > 0:
		return Closure{Why: "a live session still holds this lane"}
	}
	why := "terminal, worktree gone, branch gone, no live session"
	if backupRetained {
		why += "; backup retained"
	}
	return Closure{Closed: true, BackupRetained: backupRetained, Why: why}
}
