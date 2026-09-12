package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/decide"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// Spawner creates agent sessions. An interface so the transactional record can
// be tested without a live bramble TUI.
type Spawner interface {
	NewSession(ctx context.Context, req bramble.SpawnRequest) (bramble.SpawnResult, error)
}

// SpawnRecord is <lane>.<phase>.spawn.json in the run directory.
//
// Undocumented in SKILL.md but real in every live run, so it is written here as
// part of the contract rather than as a convention each orchestrator reinvents.
type SpawnRecord struct {
	SessionID    string `json:"session_id"`
	WorktreePath string `json:"worktree_path"`
}

// SpawnBrief is everything a phase needs to start.
type SpawnBrief struct {
	Lane    string
	Phase   string
	Model   string
	Type    string
	Backend string
	// Text is the brief handed to the agent. It must carry LITERAL report paths:
	// a child environment does not point back to the run directory.
	Text  string
	Round int
}

// Spawn creates a session and records it atomically with respect to the ledger.
//
// The recording is deliberately part of THIS call. In real runs the ledger's
// `brief` field decayed 13/13 -> 9/9 -> 1/10 -> 0/12 and `window_id` 6/13 -> 1/12
// across consecutive runs, because recording was a separate step that got
// skipped under load. A field that is only populated when someone remembers is a
// field that will be empty exactly when it matters.
//
// On a spawn failure nothing is recorded. On a record failure the session
// already exists, so the error names it explicitly rather than losing the
// handle -- an unrecorded live session is the worst outcome, since nothing can
// find it to reap it.
func Spawn(
	ctx context.Context,
	sp Spawner,
	store *state.Store,
	runDir string,
	brief SpawnBrief,
	req bramble.SpawnRequest,
) (bramble.SpawnResult, error) {
	req.Prompt = brief.Text
	if req.Type == "" {
		req.Type = brief.Type
	}
	if req.Model == "" {
		req.Model = brief.Model
	}
	if req.Backend == "" {
		req.Backend = brief.Backend
	}

	// A brief without its literal report paths produces a lane that cannot
	// report: a child environment does not point back at the run directory, so
	// a relative path or an env var reaches nothing. Refuse rather than spawn a
	// session that can only ever go silent.
	if !strings.Contains(brief.Text, DonePath(runDir, brief.Lane, brief.Phase, brief.Round)) {
		return bramble.SpawnResult{}, fmt.Errorf(
			"brief for %s.%s omits its literal done path %q; the lane would have no way "+
				"to report completion", brief.Lane, brief.Phase,
			DonePath(runDir, brief.Lane, brief.Phase, brief.Round))
	}

	// Write the brief BEFORE spawning: the agent is told to read it, and a
	// session that starts before its brief exists reads nothing.
	briefPath := filepath.Join(runDir, phaseFile(brief, "brief.txt"))
	if err := os.WriteFile(briefPath, []byte(brief.Text), 0o644); err != nil {
		return bramble.SpawnResult{}, fmt.Errorf("write brief: %w", err)
	}

	res, err := sp.NewSession(ctx, req)
	if err != nil {
		return bramble.SpawnResult{}, fmt.Errorf("spawn %s: %w", brief.Lane, err)
	}

	recErr := writeSpawnRecord(runDir, brief, res)
	ledErr := store.Update(func(st *state.State) error {
		lane, ok := st.Lane(brief.Lane)
		if !ok {
			return fmt.Errorf("lane %q vanished from the ledger during spawn", brief.Lane)
		}
		lane.Status = state.StatusRunning
		lane.Phase = brief.Phase
		lane.Round = brief.Round
		lane.Brief = brief.Text
		lane.RecordSession(brief.Phase, brief.Round, res.SessionID)
		if res.WorktreePath != "" {
			lane.Worktree = res.WorktreePath
		}
		return nil
	})

	if recErr != nil || ledErr != nil {
		return res, fmt.Errorf(
			"session %s IS LIVE at %s but recording failed (spawn.json: %v; ledger: %v) — "+
				"record it by hand or it cannot be found to reap",
			res.SessionID, res.WorktreePath, recErr, ledErr)
	}
	return res, nil
}

// BriefInstructions returns what a lane's brief should carry beyond its mission:
// the standing rules, then the run-wide one-shot nudges, then the ones addressed
// to this lane. It returns the LANE-SCOPED nudges separately, because only those
// retire with this spawn.
//
// extra carries nudges the caller is retiring on a different schedule -- the
// run-wide ones a tick delivers to every lane it staffs, and retires once at the
// end. They are rendered here but not returned for consumption, so a lane cannot
// retire an instruction addressed to its siblings.
//
// Shared by both spawn paths. dispatch --apply rendered only the standing rules,
// so `swarm-queen nudge` followed by a dispatch silently dropped the operator's
// instruction -- the same bug the tick path had, surviving in the sibling
// command because the wiring was written twice.
func BriefInstructions(runDir, laneID string, standing []string, extra []decide.Nudge) ([]string, []decide.Nudge, error) {
	own, err := decide.LaneNudges(runDir, laneID)
	if err != nil {
		return nil, nil, fmt.Errorf("read nudges: %w", err)
	}
	out := append([]string(nil), standing...)
	for _, n := range extra {
		out = append(out, n.Text)
	}
	for _, n := range own {
		out = append(out, n.Text)
	}
	return out, own, nil
}

// SpawnBaseline stamps a lane's phase baseline and reports whether it could be
// done BEFORE the session exists.
//
// Pre-stamping is possible whenever the worktree already exists, which is every
// case but a lane's first spawn. Nothing is live yet, so a git failure then costs
// a refused decision rather than a session that can never have its completion
// verified. A lane whose worktree bramble is about to create has no HEAD to read,
// so it must be stamped afterwards and its failure reported as a repair
// obligation naming the live session.
func SpawnBaseline(ctx context.Context, g reconcile.GitRunner, store *state.Store, laneID, worktree string) (pre bool, err error) {
	if worktree == "" {
		return false, nil
	}
	return true, RecordPhaseBaseline(ctx, g, store, laneID, worktree)
}

// PrepareSpawn establishes everything a lane needs BEFORE its session exists,
// and reports whether the baseline was pre-stamped.
//
// One entry point because the sequence has been got wrong once per command per
// round: dispatch missed the nudge delivery, then the pre-stamp, then the
// base-resolved pre-stamp -- each time because the two commands hand-rolled the
// same steps and only one of them was updated. A caller that cannot skip a step
// cannot fall behind.
//
// Every failure here happens with nothing live, which is the point: a lane whose
// baseline cannot be established should be refused while it costs a refused
// decision rather than an unverifiable running session.
func PrepareSpawn(
	ctx context.Context,
	g reconcile.GitRunner,
	store *state.Store,
	runDir, repoDir, laneID, base string,
	standing []string,
	extra []decide.Nudge,
) (instructions []string, nudges []decide.Nudge, preStamped bool, err error) {
	st, err := store.Read()
	if err != nil {
		return nil, nil, false, err
	}
	lane, ok := st.Lane(laneID)
	if !ok {
		return nil, nil, false, fmt.Errorf("lane %q is not in the ledger", laneID)
	}

	instructions, nudges, err = BriefInstructions(runDir, laneID, standing, extra)
	if err != nil {
		return nil, nil, false, err
	}

	preStamped, err = SpawnBaseline(ctx, g, store, laneID, lane.Worktree)
	if err == nil && !preStamped {
		// No worktree yet: resolve the fork point from the base it will be
		// created at, so the baseline is recorded before anything can commit.
		err = SpawnBaselineFromBase(ctx, g, store, repoDir, laneID, base)
		preStamped = err == nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf(
			"refusing to spawn %s: its phase baseline could not be recorded (%w); "+
				"a session without one has every mutating `.done` refused", laneID, err)
	}
	return instructions, nudges, preStamped, nil
}

// SpawnBaselineFromBase stamps the baseline for a lane whose worktree does not
// exist yet, resolving the fork point from the base it will be created at.
//
// This closes the last window in which an agent could commit before its own
// baseline was recorded -- which would make that commit the recorded starting
// HEAD and the phase's real work measure as zero commits. It cannot be read from
// the worktree, because bramble has not created it; but `-f` resolves against
// the REMOTE, so origin/<base> is the HEAD the new worktree will start at and is
// resolvable here, before anything is live.
//
// A base that cannot be resolved returns an error rather than falling back to
// the post-spawn stamp: a caller that cannot establish a baseline should refuse
// while nothing is running, not discover it afterwards.
func SpawnBaselineFromBase(
	ctx context.Context,
	g reconcile.GitRunner,
	store *state.Store,
	repoDir, laneID, base string,
) error {
	if base == "" {
		return fmt.Errorf("no base recorded for %s; its phase baseline cannot be resolved", laneID)
	}
	head, err := g.Run(ctx, repoDir, "rev-parse", "origin/"+base)
	if err != nil {
		// Fall back to a local ref: a run whose base is not pushed is
		// misconfigured for `-f`, but resolving it locally still beats stamping
		// nothing and is the same SHA whenever the two agree.
		head, err = g.Run(ctx, repoDir, "rev-parse", base)
		if err != nil {
			return fmt.Errorf("resolve base %q for %s: %w", base, laneID, err)
		}
	}
	return store.Update(func(st *state.State) error {
		lane, ok := st.Lane(laneID)
		if !ok {
			return fmt.Errorf("lane %q is not in the ledger", laneID)
		}
		lane.PhaseStartSHA = head
		if lane.ForkSHA == "" {
			lane.ForkSHA = head
		}
		return nil
	})
}

// FinishSpawn performs the post-spawn steps a live session is owed, in the order
// their failure modes demand.
//
// The baseline is stamped first (skipped when it was already pre-stamped, which
// is what preStamped records) because a lane without one has every mutating
// `.done` refused as an empty branch. The nudges are retired LAST because
// retiring is irreversible and must not happen for a spawn that is not yet
// usable: reversing these consumed an operator's instruction for a lane that
// then could not report completion, while the error text promised a re-delivery
// the consume had already made impossible.
func FinishSpawn(
	ctx context.Context,
	g reconcile.GitRunner,
	store *state.Store,
	runDir, laneID, worktree string,
	nudges []decide.Nudge,
	preStamped bool,
) error {
	if !preStamped {
		if err := RecordPhaseBaseline(ctx, g, store, laneID, worktree); err != nil {
			return fmt.Errorf("%w; its nudges are NOT consumed and will be re-delivered", err)
		}
	}
	return decide.ConsumeNudges(runDir, nudges)
}

// RecordPhaseBaseline stamps the worktree HEAD a phase starts from.
//
// Without it PhaseStartSHA stays empty, ProbeWorktree skips commit counting
// (counting against a moving branch produces a meaningless number), so
// CommitsSinceFork stays 0 and PhaseCompletion rejects EVERY mutating `.done`
// as an empty branch. The refusal that exists to catch a lane that committed
// nothing instead fires on every lane that worked.
//
// ForkSHA is the lane's original fork point and is stamped once; PhaseStartSHA
// moves to the current head at each phase, so a phase is measured against what
// it inherited rather than against the whole lane.
//
// A failure here is returned, not swallowed: a lane spawned without a baseline
// cannot have its completion verified, and finding that out at `.done` time is
// how an empty branch reaches a merge.
func RecordPhaseBaseline(ctx context.Context, g reconcile.GitRunner, store *state.Store, laneID, worktree string) error {
	if worktree == "" {
		return nil
	}
	head, err := g.Run(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve phase baseline for %s: %w", laneID, err)
	}
	return store.Update(func(st *state.State) error {
		lane, ok := st.Lane(laneID)
		if !ok {
			return fmt.Errorf("lane %q is not in the ledger", laneID)
		}
		lane.PhaseStartSHA = head
		if lane.ForkSHA == "" {
			lane.ForkSHA = head
		}
		return nil
	})
}

func writeSpawnRecord(runDir string, brief SpawnBrief, res bramble.SpawnResult) error {
	b, err := json.MarshalIndent(SpawnRecord{
		SessionID: res.SessionID, WorktreePath: res.WorktreePath,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(runDir, phaseFile(brief, "spawn.json")), append(b, '\n'), 0o644)
}

// phaseFile builds the run-dir filename for a phase artifact. Round 1 keeps the
// bare phase name so filenames match what the shell scripts and ledger.py write.
func phaseFile(b SpawnBrief, suffix string) string {
	return fmt.Sprintf("%s.%s.%s", b.Lane, state.PhaseRoundKey(b.Phase, b.Round), suffix)
}

// ReportPath is where a phase must write its report, and DonePath its completion
// claim. Briefs must contain these literally.
func ReportPath(runDir, lane, phase string, round int) string {
	return filepath.Join(runDir, fmt.Sprintf("%s.%s.md", lane, state.PhaseRoundKey(phase, round)))
}

// DonePath is the completion-claim file a phase touches LAST.
func DonePath(runDir, lane, phase string, round int) string {
	return filepath.Join(runDir, fmt.Sprintf("%s.%s.done", lane, state.PhaseRoundKey(phase, round)))
}

// NeedsSWEPath is the review-rejection signal that returns a lane to swe.
func NeedsSWEPath(runDir, lane, phase string, round int) string {
	return filepath.Join(runDir, fmt.Sprintf("%s.%s.needs-swe", lane, state.PhaseRoundKey(phase, round)))
}
