// Package verify turns claims into verdicts.
//
// The governing rule, learned expensively by every real run: every `.done` file,
// idle notification, lane summary, and review verdict is a CLAIM. Nothing here
// believes one without checking the artifact or the live state behind it.
//
// Each predicate returns a Verdict carrying its evidence, so `verify --explain`
// can show why a lane was advanced, reworked, or refused. A bare boolean would
// reproduce the original problem: an assertion nobody can audit.
package verify

import (
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// Severity ranks a finding.
type Severity string

const (
	// SeverityBlock means the claim is refused: acting on it would be wrong.
	SeverityBlock Severity = "block"
	// SeverityWarn means something is off but does not by itself refuse the claim.
	SeverityWarn Severity = "warn"
	// SeverityInfo records evidence worth surfacing.
	SeverityInfo Severity = "info"
)

// Finding is one piece of evidence about a lane.
type Finding struct {
	Lane     string
	Severity Severity
	// Claim is what was asserted, e.g. "swe.done".
	Claim string
	// Evidence is the measured fact that supports or refutes it.
	Evidence string
	// Action is what a human or the apply step should do about it.
	Action string
}

func (f Finding) String() string {
	s := fmt.Sprintf("%s %s: %s", strings.ToUpper(string(f.Severity)), f.Lane, f.Evidence)
	if f.Action != "" {
		s += " — " + f.Action
	}
	return s
}

// Verdict is the outcome of verifying one claim, with everything that informed it.
type Verdict struct {
	Findings []Finding
	OK       bool
}

// Blocked reports whether any finding refuses the claim.
func (v Verdict) Blocked() bool {
	for _, f := range v.Findings {
		if f.Severity == SeverityBlock {
			return true
		}
	}
	return false
}

func (v *Verdict) add(f Finding) { v.Findings = append(v.Findings, f) }

// PhaseCompletion verifies a lane's claim that a mutating phase finished.
//
// A phase expected to edit must have committed. `.done` with zero commits since
// the phase started is the single most dangerous claim in the system: merging on
// it ships nothing while reporting success. The watcher already prints
// "EMPTY BRANCH: back up and nudge, do NOT merge"; this makes that refusal
// mechanical rather than advisory.
func PhaseCompletion(lane *state.Lane, wt reconcile.WorktreeState, mutating bool) Verdict {
	v := Verdict{OK: true}
	claim := lane.Phase + ".done"

	if !wt.Exists {
		v.OK = false
		v.add(Finding{lane.ID, SeverityBlock, claim,
			"worktree " + wt.Path + " does not exist",
			"the ledger points at nothing; recover the branch before advancing"})
		return v
	}

	if mutating && wt.CommitsSinceFork == 0 {
		v.OK = false
		v.add(Finding{lane.ID, SeverityBlock, claim,
			"branch has 0 commits since the phase started",
			"EMPTY BRANCH: back up and nudge, do NOT merge"})
	}

	if wt.DirtyCount > 0 {
		sev, action := SeverityWarn, "commit or snapshot before advancing"
		if wt.HasUntracked {
			// No branch protects an untracked file, so a worktree removal
			// destroys it with nothing to recover from.
			action = "UNTRACKED work present; snapshot to refs/backup before any reap"
		}
		v.add(Finding{lane.ID, sev, claim,
			fmt.Sprintf("worktree is dirty (%d entries, untracked=%v)",
				wt.DirtyCount, wt.HasUntracked),
			action})
	}
	return v
}

// Mergeable verifies a lane is safe to merge.
//
// The gate is: green checks AND an approval pinned to the current head AND the
// PR actually exists. A review verdict is not proof of a green branch, and
// `reviewDecision: APPROVED` is a PR-level field that goes stale the moment a
// new commit lands — one run caught a stale approval three separate times, and
// merging on it would have shipped unapproved bytes.
func Mergeable(lane *state.Lane) Verdict {
	v := Verdict{OK: true}
	const claim = "mergeable"

	if lane.PR == 0 {
		v.OK = false
		v.add(Finding{lane.ID, SeverityBlock, claim, "no PR recorded",
			"open a PR or record its number before merging"})
		return v
	}
	if lane.ApprovalStale() {
		v.OK = false
		detail := fmt.Sprintf("approval %q is not at head %q",
			orNone(lane.ApprovalSHA), orNone(lane.PRHead))
		if lane.ApprovalSHA == "" || lane.PRHead == "" {
			// Absence is never evidence of approval.
			detail = fmt.Sprintf("approval state unknown (approval=%q head=%q)",
				lane.ApprovalSHA, lane.PRHead)
		}
		v.add(Finding{lane.ID, SeverityBlock, claim, detail,
			"re-request review at the current head; do not merge on a PR-level APPROVED"})
	}
	switch lane.Checks {
	case "passing":
	case "":
		v.OK = false
		v.add(Finding{lane.ID, SeverityBlock, claim, "check status unknown",
			"read the checks before merging; absence is not success"})
	default:
		v.OK = false
		v.add(Finding{lane.ID, SeverityBlock, claim,
			fmt.Sprintf("checks are %q", lane.Checks),
			"wait for green, or rerun confirmed-transient failures"})
	}
	return v
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

// LedgerDrift compares recorded lane state against observed reality.
//
// This is the mechanism against the lying ledger. Real runs shipped ledgers
// where lanes read `running` over merged PRs, `done` lanes still held worktrees,
// and live sessions appeared nowhere at all.
func LedgerDrift(st *state.State, worktrees map[string]reconcile.WorktreeState, sessions []bramble.Session) []Finding {
	return LedgerDriftWithBranches(st, worktrees, sessions, nil)
}

// LedgerDriftWithBranches is LedgerDrift plus the set of branches that still
// exist, so a partial teardown can name what is left to do.
//
// The dangling path says the teardown was incomplete; the surviving branch says
// which resource still needs closing. A nil set skips the branch half rather
// than reporting every branch as absent -- unknown is not evidence of closure.
func LedgerDriftWithBranches(
	st *state.State,
	worktrees map[string]reconcile.WorktreeState,
	sessions []bramble.Session,
	liveBranches map[string]bool,
) []Finding {
	var out []Finding

	// A nil session slice means the bramble probe could not run; it is not proof
	// that a running lane has no session. A non-nil empty slice is measured
	// evidence that no sessions were found.
	sessionsProbed := sessions != nil
	// Index rather than copy: bramble.Session is a large struct and this runs
	// once per session per tick.
	byWorktreeName := map[string][]*bramble.Session{}
	for i := range sessions {
		s := &sessions[i]
		byWorktreeName[s.WorktreeName] = append(byWorktreeName[s.WorktreeName], s)
	}

	for _, lane := range st.Lanes {
		wt, probed := worktrees[lane.ID]

		if lane.Status == state.StatusDone && probed && wt.Exists {
			out = append(out, Finding{lane.ID, SeverityWarn, "status=done",
				"worktree still exists at " + wt.Path,
				"reap it, or the ledger is describing a swarm that no longer matches disk"})
		}
		if lane.Status == state.StatusRunning && probed && !wt.Exists {
			out = append(out, Finding{lane.ID, SeverityBlock, "status=running",
				"worktree " + lane.Worktree + " is gone",
				"the lane died; recover its branch or mark it failed"})
		}

		// A recorded path that no longer exists is drift whatever the status.
		// Asking only whether a done lane still HOLDS a worktree makes a partial
		// teardown read as healthy: once the directory is removed without
		// reconciling the ledger, the old finding disappears and the run reports
		// clean. Observed live -- two lanes had their worktrees removed with
		// state.json untouched and their branches left behind, and the drift
		// check went quiet. Absence of the old finding is not cleanliness.
		//
		// Guarded on a non-empty path: a cleanly reaped lane has no worktree
		// recorded, and flagging those would fire on every properly closed lane,
		// which is how a real signal becomes noise people mute.
		if lane.Status.Terminal() && lane.Worktree != "" && probed && !wt.Exists {
			out = append(out, Finding{lane.ID, SeverityWarn, "worktree",
				"recorded worktree " + lane.Worktree + " does not exist",
				"the teardown was never reconciled; clear the field once the lane is fully closed"})
		}

		if liveBranches != nil && lane.Status.Terminal() && lane.Branch != "" && liveBranches[lane.Branch] {
			out = append(out, Finding{lane.ID, SeverityWarn, "branch",
				"branch " + lane.Branch + " still exists",
				"verify integration by content, then delete it; the five-zeros audit is incomplete"})
		}

		// A lane whose phase the config never declared makes phase-ordered
		// logic silently wrong. Real runs invented merged/rebase/published.
		for _, p := range lane.UnknownPhases(&st.Config) {
			out = append(out, Finding{lane.ID, SeverityWarn, "phase=" + p,
				fmt.Sprintf("phase %q is not declared by the run config %v", p, st.Config.PhaseNames()),
				"declare it in the contract or correct the lane"})
		}

		// Rework rounds that were overwritten rather than recorded.
		for _, phase := range st.Config.PhaseNames() {
			if lost := lane.LostRounds(phase); len(lost) > 0 {
				out = append(out, Finding{lane.ID, SeverityInfo, "sessions",
					fmt.Sprintf("phase %q is missing rounds %v of %d",
						phase, lost, lane.MaxRound(phase)),
					"earlier attempts were overwritten; their sessions are unrecoverable"})
			}
		}

		// A running lane whose sessions have no live pane is not running.
		if lane.Status == state.StatusRunning && sessionsProbed {
			name := worktreeName(lane.Worktree)
			live := byWorktreeName[name]
			if len(live) == 0 {
				out = append(out, Finding{lane.ID, SeverityWarn, "status=running",
					"no live bramble session for worktree " + name,
					"it finished without reporting, or it died; check the branch before deciding"})
			}
			for _, s := range live {
				if !s.HasPane() {
					out = append(out, Finding{lane.ID, SeverityBlock, "status=running",
						fmt.Sprintf("session %s has no tmux pane (status=%s)", s.ID, s.Status),
						"the window is gone; decide now rather than waiting for a stall timeout"})
				}
			}
		}
	}
	return out
}

// worktreeName reduces a worktree path to the name bramble reports, since
// list-sessions carries worktree_name and never a path.
func worktreeName(path string) string {
	path = strings.TrimRight(path, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
