package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
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
