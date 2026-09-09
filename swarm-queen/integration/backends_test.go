//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/decide"
	"github.com/bazelment/yoloswe/swarm-queen/lifecycle"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
	"github.com/bazelment/yoloswe/swarm-queen/verify"
)

// backend is one CLI a lane can run under. The swarm mixes them by design: swe
// on one model, clean on another, review on a third.
type backend struct {
	name  string
	model string
	// backend is bramble's --backend value; empty infers from the model.
	bramble string
}

// backends covers all three the mined runs actually used. agy is included
// deliberately: bramble's --backend help text listed only claude and codex for
// weeks while the code routed gemini-* to agy, and a human had to correct the
// orchestrator about it twice, two days apart.
func backends() []backend {
	return []backend{
		{name: "claude", model: "sonnet"},
		{name: "codex", model: "gpt-5.4-mini", bramble: "codex"},
		{name: "agy", model: "gemini-3.8-flash-low", bramble: "agy"},
	}
}

// TestSpawnLifecycleAcrossBackends drives the real thing: for each backend,
// spawn a bramble session on a fresh worktree, confirm swarm-queen recorded it
// transactionally, confirm the session is live in tmux, then reap all three
// layers and assert the five-zeros audit is clean.
//
// This is the test the unit suite cannot be: every assertion here is about
// agreement between swarm-queen's record and what bramble and git actually hold.
func TestSpawnLifecycleAcrossBackends(t *testing.T) {
	c := client(t)
	repo := repoRoot(t)
	git := reconcile.ExecGit{}

	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			r := newReaper(t, c, repo)
			suffix := uniqueSuffix()
			laneID := "sq-" + b.name + "-" + suffix
			branch := "swarm-queen-test/" + laneID

			runDir, store := seedRun(t,
				[]state.Phase{{Name: "swe", Model: b.model}, {Name: "clean", Model: b.model}},
				&state.Lane{
					ID: laneID, Title: "swarm-queen lifecycle probe", Branch: branch,
					Status: state.StatusPlanned, Priority: state.P0,
					Sessions: map[string]string{},
				})

			// A trivial, self-terminating brief: this test is about the harness,
			// not about the model's work. Asking for real work would make the
			// test's runtime depend on a model's judgement.
			brief := lifecycle.SpawnBrief{
				Lane: laneID, Phase: "swe", Round: 1,
				Model: b.model, Type: "builder", Backend: b.bramble,
				Text: fmt.Sprintf(
					"You are a harness probe. Do exactly this and nothing else:\n"+
						"1. Run: echo swarm-queen-probe > probe.txt && git add probe.txt && "+
						"git -c user.email=t@e.com -c user.name=t commit -m 'probe'\n"+
						"2. Run: touch %s\n3. Stop.\n",
					lifecycle.DonePath(runDir, laneID, "swe", 1)),
			}

			res, err := lifecycle.Spawn(ctx(t), c, store, runDir, brief, bramble.SpawnRequest{
				Repo: testRepo, Branch: branch, From: "main", CreateWorktree: true,
				Backend: b.bramble, Model: b.model, Goal: "swarm-queen probe",
			})
			if err != nil {
				t.Fatalf("spawn under %s: %v", b.name, err)
			}
			r.session(res.SessionID)
			r.branch(branch)

			// --- the transactional record, which is the whole point of Spawn ---
			st, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			lane, _ := st.Lane(laneID)
			if lane.Status != state.StatusRunning || lane.Phase != "swe" {
				t.Errorf("ledger not updated: status=%s phase=%s", lane.Status, lane.Phase)
			}
			if got, _ := lane.SessionFor("swe", 1); got != res.SessionID {
				t.Errorf("session id not recorded: %v", lane.Sessions)
			}
			if lane.Brief == "" {
				t.Error("brief not recorded — this is the field that decayed to 0/12 in real runs")
			}
			if lane.Worktree == "" {
				t.Error("worktree not recorded — a lane with no worktree is invisible to every probe")
			}
			r.tree(lane.Worktree)

			// spawn.json: real in every live run, and now part of the contract.
			if _, err := os.Stat(filepath.Join(runDir, laneID+".swe.spawn.json")); err != nil {
				t.Errorf("spawn.json missing: %v", err)
			}

			// --- the session is genuinely live, in a real tmux window ---
			sess := waitForSession(t, c, res.SessionID, spawnSettle)
			if !sess.HasPane() {
				t.Fatalf("session %s has no tmux pane; it did not really start", sess.ID)
			}
			if sess.WorktreeName != filepath.Base(lane.Worktree) {
				t.Errorf("session worktree_name %q does not match lane worktree %q",
					sess.WorktreeName, lane.Worktree)
			}
			if _, err := (lifecycle.ExecTmux{}).Run(ctx(t), "list-panes", "-t", sess.TmuxTarget); err != nil {
				t.Errorf("tmux does not know pane %s: %v", sess.TmuxTarget, err)
			}

			// --- reap all three layers, and prove nothing is left ---
			if err := store.Update(func(s *state.State) error {
				l, _ := s.Lane(laneID)
				l.Status = state.StatusDone
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Re-read: `lane` was captured before the update and is now stale.
			st, err = store.Read()
			if err != nil {
				t.Fatal(err)
			}
			lane, _ = st.Lane(laneID)

			wt, _ := reconcile.ProbeWorktree(ctx(t), git, lane.Worktree, "")
			plan := lifecycle.PlanReap(ctx(t), git, repo, lane, wt, "main",
				[]lifecycle.LiveSession{{ID: sess.ID, Status: sess.Status, TmuxTarget: sess.TmuxTarget}})
			if !plan.Safe {
				t.Fatalf("reap refused: %v", plan.Blockers)
			}
			// The kill must precede the removal, or the agent keeps running
			// against a path that no longer exists.
			killAt := -1
			rmAt := -1
			for i, s := range plan.Steps {
				if strings.Contains(s, "kill tmux window") {
					killAt = i
				}
				if strings.HasPrefix(s, "remove worktree") {
					rmAt = i
				}
			}
			if killAt < 0 {
				t.Fatalf("plan omits the session kill: %v", plan.Steps)
			}
			if rmAt >= 0 && killAt > rmAt {
				t.Errorf("plan removes the worktree before killing the session: %v", plan.Steps)
			}

			tmux := lifecycle.ExecTmux{}
			self, err := lifecycle.ResolveSelf(ctx(t), tmux)
			if err != nil {
				t.Skipf("cannot resolve own tmux window, so a kill cannot be proven safe: %v", err)
			}
			if err := lifecycle.SafeKillWindow(ctx(t), tmux, sess.TmuxTarget, self, laneID); err != nil {
				t.Fatalf("kill window: %v", err)
			}
			if _, err := git.Run(ctx(t), repo, "worktree", "remove", "--force", lane.Worktree); err != nil {
				t.Fatalf("remove worktree: %v", err)
			}
			if _, err := git.Run(ctx(t), repo, "branch", "-D", branch); err != nil {
				t.Fatalf("delete branch: %v", err)
			}

			// Five zeros: session, worktree, branch, backup ref, tmux pane.
			lane.WindowID = sess.TmuxTarget
			lane.Worktree = ""
			audit := lifecycle.AuditLane(ctx(t), git, tmux, repo, lane, false)
			if !audit.Clean() {
				t.Errorf("teardown left something behind: %s", audit)
			}
		})
	}
}

// TestEmptyBranchClaimIsRefused is the single most dangerous claim in the
// system, exercised against a REAL session and a REAL branch: a lane touches
// .done without committing anything. Merging on that ships nothing while
// reporting success.
func TestEmptyBranchClaimIsRefused(t *testing.T) {
	c := client(t)
	repo := repoRoot(t)
	git := reconcile.ExecGit{}
	r := newReaper(t, c, repo)

	suffix := uniqueSuffix()
	laneID := "sq-empty-" + suffix
	branch := "swarm-queen-test/" + laneID

	runDir, store := seedRun(t,
		[]state.Phase{{Name: "swe", Model: "sonnet"}, {Name: "clean", Model: "sonnet"}},
		&state.Lane{
			ID: laneID, Title: "empty branch probe", Branch: branch,
			Status: state.StatusPlanned, Priority: state.P0, Sessions: map[string]string{},
		})

	// Instructed to claim completion WITHOUT committing: exactly the lie the
	// verification layer exists to catch.
	brief := lifecycle.SpawnBrief{
		Lane: laneID, Phase: "swe", Round: 1, Model: "sonnet", Type: "builder",
		Text: fmt.Sprintf("You are a harness probe. Do exactly this: run `touch %s` and stop. "+
			"Do not commit anything.\n", lifecycle.DonePath(runDir, laneID, "swe", 1)),
	}
	res, err := lifecycle.Spawn(ctx(t), c, store, runDir, brief, bramble.SpawnRequest{
		Repo: testRepo, Branch: branch, From: "main", CreateWorktree: true,
		Model: "sonnet", Goal: "empty branch probe",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	r.session(res.SessionID)
	r.branch(branch)

	st, _ := store.Read()
	lane, _ := st.Lane(laneID)
	r.tree(lane.Worktree)
	waitForSession(t, c, res.SessionID, spawnSettle)

	// Wait for the .done claim, then check it against the branch.
	donePath := lifecycle.DonePath(runDir, laneID, "swe", 1)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(donePath); err == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if _, err := os.Stat(donePath); err != nil {
		t.Skipf("the probe never claimed completion within the timeout; "+
			"nothing to verify (%v)", err)
	}

	fork, err := git.Run(ctx(t), repo, "rev-parse", "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	wt, err := reconcile.ProbeWorktree(ctx(t), git, lane.Worktree, fork)
	if err != nil {
		t.Fatalf("probe worktree: %v", err)
	}
	if wt.CommitsSinceFork != 0 {
		t.Skipf("the probe committed after all (%d commits); "+
			"this test needs an empty branch", wt.CommitsSinceFork)
	}

	v := verify.PhaseCompletion(lane, wt, true)
	if v.OK || !v.Blocked() {
		t.Fatalf("an empty branch behind a .done MUST be refused: %+v", v)
	}
	var sawRefusal bool
	for _, f := range v.Findings {
		if strings.Contains(f.Action, "do NOT merge") {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Errorf("expected the empty-branch refusal, got %+v", v.Findings)
	}

	// And the decision layer must escalate rather than advance.
	ds := decide.Plan(decide.Inputs{
		State:    st,
		Signals:  []decide.LaneSignal{{Lane: laneID, Phase: "swe", Round: 1}},
		Verdicts: map[string]verify.Verdict{laneID: v},
	})
	for _, d := range ds {
		if d.Kind == decide.KindAdvance {
			t.Errorf("a refused claim must never advance: %+v", d)
		}
	}
}

// TestLiveSessionBlocksWorktreeRemoval reproduces the failure mode found on a
// live run: the ledger's window_id is empty while a real session holds the
// worktree. Planning from the ledger alone removes the worktree out from under
// a running agent.
func TestLiveSessionBlocksWorktreeRemoval(t *testing.T) {
	c := client(t)
	repo := repoRoot(t)
	git := reconcile.ExecGit{}
	r := newReaper(t, c, repo)

	suffix := uniqueSuffix()
	laneID := "sq-squat-" + suffix
	branch := "swarm-queen-test/" + laneID

	runDir, store := seedRun(t,
		[]state.Phase{{Name: "swe", Model: "sonnet"}},
		&state.Lane{
			ID: laneID, Title: "squatter probe", Branch: branch,
			Status: state.StatusPlanned, Priority: state.P0, Sessions: map[string]string{},
		})

	st0, _ := store.Read()
	lane0, _ := st0.Lane(laneID)
	res, err := lifecycle.Spawn(ctx(t), c, store, runDir, lifecycle.SpawnBrief{
		Lane: laneID, Phase: "swe", Round: 1, Model: "sonnet", Type: "builder",
		Text: lifecycle.RenderBrief(lifecycle.BriefContext{
			RunDir: runDir, Lane: lane0, Phase: "swe", Round: 1,
			Mission: "You are a harness probe. Wait quietly; do nothing.",
		}),
	}, bramble.SpawnRequest{
		Repo: testRepo, Branch: branch, From: "main", CreateWorktree: true,
		Model: "sonnet", Goal: "squatter probe",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	r.session(res.SessionID)
	r.branch(branch)

	sess := waitForSession(t, c, res.SessionID, spawnSettle)
	st, _ := store.Read()
	lane, _ := st.Lane(laneID)
	r.tree(lane.Worktree)

	// Simulate the live ledger: terminal status, and window_id NEVER recorded.
	if err := store.Update(func(s *state.State) error {
		l, _ := s.Lane(laneID)
		l.Status = state.StatusDone
		l.WindowID = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _ = store.Read()
	lane, _ = st.Lane(laneID)

	wt, _ := reconcile.ProbeWorktree(ctx(t), git, lane.Worktree, "")

	// Planning from the ledger alone omits the kill entirely.
	blind := lifecycle.PlanReap(ctx(t), git, repo, lane, wt, "main", nil)
	for _, s := range blind.Steps {
		if strings.Contains(s, "kill tmux window") {
			t.Errorf("premise broken: a ledger-only plan should not know about the session: %v", blind.Steps)
		}
	}

	// Resolving from bramble finds the squatter and kills it first.
	live := []lifecycle.LiveSession{{ID: sess.ID, Status: sess.Status, TmuxTarget: sess.TmuxTarget}}
	seeing := lifecycle.PlanReap(ctx(t), git, repo, lane, wt, "main", live)
	killAt := -1
	rmAt := -1
	for i, s := range seeing.Steps {
		if strings.Contains(s, "kill tmux window "+sess.TmuxTarget) {
			killAt = i
		}
		if strings.HasPrefix(s, "remove worktree") {
			rmAt = i
		}
	}
	if killAt < 0 {
		t.Fatalf("the live session was not detected: %v", seeing.Steps)
	}
	if rmAt >= 0 && killAt > rmAt {
		t.Errorf("worktree removed before the live session was killed: %v", seeing.Steps)
	}
}

// TestDoctorAgreesWithLedgerPy runs both implementations against the same run
// directory. They share no code, so agreement is real evidence rather than a
// tautology -- and a disagreement is a bug in one of them.
func TestDoctorAgreesWithLedgerPy(t *testing.T) {
	repo := repoRoot(t)
	runDir, store := seedRun(t,
		[]state.Phase{{Name: "swe", Model: "sonnet"}},
		&state.Lane{
			ID: "sq-drift", Title: "drift probe", Branch: "swarm-queen-test/drift",
			Status: state.StatusDone, Phase: "swe",
			Worktree: repo, // a done lane still holding a worktree: the drift case
			Sessions: map[string]string{"swe": "fake-session"},
		})

	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	wts := map[string]reconcile.WorktreeState{
		"sq-drift": {Path: repo, Exists: true},
	}
	findings := verify.LedgerDrift(st, wts, nil)
	var goSawIt bool
	for _, f := range findings {
		if f.Lane == "sq-drift" && strings.Contains(f.Evidence, "still exists") {
			goSawIt = true
		}
	}
	if !goSawIt {
		t.Fatalf("swarm-queen missed the drift: %+v", findings)
	}

	// Compare against the ledger.py the SWARM actually uses. ~/.claude/skills is
	// a symlink into a worktree, so this can lag the merged skill -- and when it
	// does, the swarm is running older code than the repo suggests. Say so
	// rather than skipping quietly.
	ledger := ledgerPyPath(t)
	// A non-zero exit means findings, not breakage: doctor reports drift through
	// its exit code so a tick can gate on it. Only an unusable invocation is a
	// real failure, so distinguish the two rather than treating any error as one.
	out, err := runCmd(ctx(t), "/usr/bin/python3", ledger, "doctor", runDir)
	switch {
	case strings.Contains(out, "invalid choice: 'doctor'"):
		t.Skipf("the live ledger.py at %s predates `doctor`; the skill in use is "+
			"older than the merged one, which is worth fixing but is not a "+
			"swarm-queen defect\n%s", ledger, out)
	case err != nil && !strings.Contains(out, "finding(s)"):
		t.Fatalf("ledger.py doctor: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sq-drift") {
		t.Errorf("ledger.py doctor missed the same drift swarm-queen found:\n%s", out)
	}
}

// ledgerPyPath resolves the ledger.py the swarm would actually invoke.
func ledgerPyPath(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		os.ExpandEnv("$HOME/.claude/skills/subagent-swarm/scripts/ledger.py"),
		filepath.Join(repoRoot(t), ".claude/skills/subagent-swarm/scripts/ledger.py"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no ledger.py found")
	return ""
}

// A check that cannot run must SAY so, on stdout, in the summary.
//
// Reporting only on stderr is not enough: the moment stderr is redirected, a
// count that silently omits a whole category reads as a complete answer. That is
// the clean-bill-of-health-from-a-broken-probe failure that audit_cleanup.sh
// documents for BRAMBLE_SOCK, reproduced in the check whose job is finding leaks.
func TestDoctorSaysWhenABranchProbeCouldNotRun(t *testing.T) {
	bin := doctorBinary(t)
	repo := repoRoot(t)

	runDir, _ := seedRun(t,
		[]state.Phase{{Name: "swe", Model: "sonnet"}},
		&state.Lane{ID: "sq-skip", Title: "probe", Branch: "swarm-queen-test/absent",
			Status: state.StatusDone, Worktree: "/definitely/not/here",
			Sessions: map[string]string{}})

	// stderr discarded on purpose: the caller must still be able to tell.
	fromNonRepo, err := runCmdStdout(ctx(t), bin, "doctor", runDir, "--repo", t.TempDir())
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, fromNonRepo)
	}
	if !strings.Contains(fromNonRepo, "SKIPPED") {
		t.Errorf("an unrunnable branch probe must be reported on stdout:\n%s", fromNonRepo)
	}
	if !strings.Contains(fromNonRepo, "branch checks skipped") {
		t.Errorf("the summary line must carry the caveat:\n%s", fromNonRepo)
	}

	// From a real repo the caveat must be absent, or it becomes noise nobody reads.
	fromRepo, err := runCmdStdout(ctx(t), bin, "doctor", runDir, "--repo", repo)
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, fromRepo)
	}
	if strings.Contains(fromRepo, "SKIPPED") {
		t.Errorf("a working probe must not claim it was skipped:\n%s", fromRepo)
	}
}
