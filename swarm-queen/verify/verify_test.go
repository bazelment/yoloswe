package verify

import (
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

func hasBlock(v Verdict, substr string) bool {
	for _, f := range v.Findings {
		if f.Severity == SeverityBlock && strings.Contains(f.Evidence+" "+f.Action, substr) {
			return true
		}
	}
	return false
}

// The most dangerous claim in the system: .done on a branch with no commits.
func TestPhaseCompletionRefusesEmptyBranch(t *testing.T) {
	t.Parallel()
	lane := &state.Lane{ID: "url-ready-backoff", Phase: "swe"}
	wt := reconcile.WorktreeState{Path: "/wt/x", Exists: true, CommitsSinceFork: 0}

	v := PhaseCompletion(lane, wt, true)
	if v.OK || !v.Blocked() {
		t.Fatalf("empty branch must be refused: %+v", v)
	}
	if !hasBlock(v, "do NOT merge") {
		t.Errorf("expected the empty-branch refusal, got %+v", v.Findings)
	}
}

// A read-only phase legitimately commits nothing.
func TestPhaseCompletionAllowsEmptyForReadOnlyPhase(t *testing.T) {
	t.Parallel()
	lane := &state.Lane{ID: "gaps", Phase: "gaps"}
	wt := reconcile.WorktreeState{Path: "/wt/x", Exists: true, CommitsSinceFork: 0}

	if v := PhaseCompletion(lane, wt, false); !v.OK {
		t.Errorf("read-only phase must not be refused for zero commits: %+v", v.Findings)
	}
}

// Untracked work is protected by no branch; a reap destroys it outright.
func TestPhaseCompletionFlagsUntrackedWork(t *testing.T) {
	t.Parallel()
	lane := &state.Lane{ID: "replay", Phase: "swe"}
	wt := reconcile.WorktreeState{
		Path: "/wt/x", Exists: true, CommitsSinceFork: 2,
		DirtyCount: 1, HasUntracked: true,
	}
	v := PhaseCompletion(lane, wt, true)
	if !v.OK {
		t.Errorf("dirty-but-committed must not be refused outright: %+v", v.Findings)
	}
	var warned bool
	for _, f := range v.Findings {
		if strings.Contains(f.Action, "refs/backup") {
			warned = true
		}
	}
	if !warned {
		t.Error("untracked work must demand a snapshot before any reap")
	}
}

func TestPhaseCompletionRefusesMissingWorktree(t *testing.T) {
	t.Parallel()
	lane := &state.Lane{ID: "ghost", Phase: "swe"}
	v := PhaseCompletion(lane, reconcile.WorktreeState{Path: "/gone"}, true)
	if v.OK || !v.Blocked() {
		t.Fatalf("missing worktree must be refused: %+v", v)
	}
}

// A PR-level APPROVED goes stale the moment a new commit lands. One run caught
// this three separate times.
func TestMergeableRefusesStaleApproval(t *testing.T) {
	t.Parallel()
	lane := &state.Lane{
		ID: "base-image-hash-sync", PR: 11968,
		PRHead: "135c17a2", ApprovalSHA: "fa365c16", Checks: "passing",
	}
	v := Mergeable(lane)
	if v.OK || !v.Blocked() {
		t.Fatalf("stale approval must block the merge: %+v", v)
	}
	if !hasBlock(v, "not at head") {
		t.Errorf("expected a stale-approval finding, got %+v", v.Findings)
	}
}

// Absence is never evidence of approval or of green checks.
func TestMergeableTreatsUnknownAsRefusal(t *testing.T) {
	t.Parallel()
	cases := map[string]*state.Lane{
		"no approval recorded": {ID: "a", PR: 1, PRHead: "abc", Checks: "passing"},
		"no head recorded":     {ID: "b", PR: 1, ApprovalSHA: "abc", Checks: "passing"},
		"checks unknown":       {ID: "c", PR: 1, PRHead: "abc", ApprovalSHA: "abc"},
		"no PR":                {ID: "d", PRHead: "abc", ApprovalSHA: "abc", Checks: "passing"},
	}
	for name, lane := range cases {
		if v := Mergeable(lane); v.OK {
			t.Errorf("%s: must not be mergeable", name)
		}
	}
}

func TestMergeableAcceptsFullyGreenLane(t *testing.T) {
	t.Parallel()
	lane := &state.Lane{ID: "ok", PR: 11968, PRHead: "abc", ApprovalSHA: "abc", Checks: "passing"}
	v := Mergeable(lane)
	if !v.OK || v.Blocked() {
		t.Errorf("approval at head with green checks must be mergeable: %+v", v.Findings)
	}
}

func TestMergeableRefusesFailingChecks(t *testing.T) {
	t.Parallel()
	lane := &state.Lane{ID: "red", PR: 1, PRHead: "abc", ApprovalSHA: "abc", Checks: "failing"}
	if v := Mergeable(lane); v.OK {
		t.Error("failing checks must block the merge")
	}
}

// The live 09-08 run: lanes marked done whose worktrees still exist.
func TestLedgerDriftFindsDoneLanesHoldingWorktrees(t *testing.T) {
	t.Parallel()
	st := &state.State{
		Config: state.Config{Phases: []state.Phase{{Name: "swe"}, {Name: "github-review"}}},
		Lanes: []*state.Lane{
			{ID: "transient-split-credential", Status: state.StatusDone, Phase: "github-review"},
			{ID: "clean-lane", Status: state.StatusDone, Phase: "github-review"},
		},
	}
	wts := map[string]reconcile.WorktreeState{
		"transient-split-credential": {Path: "/wt/split-cred", Exists: true},
		"clean-lane":                 {Path: "/wt/clean", Exists: false},
	}
	findings := LedgerDrift(st, wts, nil)
	var got int
	for _, f := range findings {
		if f.Lane == "transient-split-credential" && strings.Contains(f.Evidence, "still exists") {
			got++
		}
		if f.Lane == "clean-lane" {
			t.Errorf("a properly reaped lane must not be flagged: %+v", f)
		}
	}
	if got != 1 {
		t.Errorf("expected one drift finding, got %d from %+v", got, findings)
	}
}

// A running lane whose session has no pane is not running.
func TestLedgerDriftFindsRunningLaneWithDeadSession(t *testing.T) {
	t.Parallel()
	st := &state.State{
		Config: state.Config{Phases: []state.Phase{{Name: "swe"}}},
		Lanes: []*state.Lane{{
			ID: "clustererrors", Status: state.StatusRunning, Phase: "swe",
			Worktree: "/home/ubuntu/worktrees/kernel/swarm/deploy-harden-clustererrors-0905",
		}},
	}
	wts := map[string]reconcile.WorktreeState{
		"clustererrors": {Path: st.Lanes[0].Worktree, Exists: true},
	}
	sessions := []bramble.Session{{
		ID:           "deploy-harden-clustererrors-0905-builder-3782400b",
		Status:       "failed",
		WorktreeName: "deploy-harden-clustererrors-0905",
		// No TmuxTarget: the window is gone.
	}}

	findings := LedgerDrift(st, wts, sessions)
	var blocked bool
	for _, f := range findings {
		if f.Severity == SeverityBlock && strings.Contains(f.Evidence, "no tmux pane") {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("a paneless session under a running lane must block: %+v", findings)
	}
}

// Phases invented at runtime make phase-ordered logic silently wrong.
func TestLedgerDriftFindsUndeclaredPhases(t *testing.T) {
	t.Parallel()
	st := &state.State{
		Config: state.Config{Phases: []state.Phase{{Name: "swe"}, {Name: "clean"}}},
		Lanes:  []*state.Lane{{ID: "elision", Status: state.StatusDone, Phase: "rebase"}},
	}
	findings := LedgerDrift(st, map[string]reconcile.WorktreeState{}, nil)
	var found bool
	for _, f := range findings {
		if strings.Contains(f.Evidence, `"rebase" is not declared`) {
			found = true
		}
	}
	if !found {
		t.Errorf("undeclared phase must be reported: %+v", findings)
	}
}
