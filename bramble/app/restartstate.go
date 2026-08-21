package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// RestartStateEnvVar names the file holding the state handed from a bramble
// process to the image that replaces it via an in-place exec restart.
const RestartStateEnvVar = "BRAMBLE_RESTART_STATE"

// restartStateVersion guards the on-disk shape. A state file written by a
// different version is ignored rather than half-applied: restart state is
// disposable, and starting fresh beats restoring nonsense.
const restartStateVersion = 1

// staleRestartStateAge is how long an unconsumed handoff lingers before the
// next writer sweeps it. A handoff is normally consumed seconds after it is
// written; anything older belongs to a process image that never came back.
const staleRestartStateAge = time.Hour

// RestartState is the slice of a running TUI that has no other way back.
//
// It is deliberately small. Sessions, their tmux windows, and their output all
// survive on their own — the session store plus Manager.ReconcileTmuxSessions
// and session.ReposNeedingTmuxReconcile re-adopt them at startup. What those
// cannot reconstruct is which repo the user was looking at (repos with no live
// tmux session are invisible to that scan) and where the cursor was.
type RestartState struct {
	// ActiveRepo is the repo the TUI was showing. It outranks --repo and cwd
	// detection on the next start.
	ActiveRepo string `json:"active_repo"`
	// SelectedWorktree is the worktree name selected in the dropdown.
	SelectedWorktree string `json:"selected_worktree,omitempty"`
	// ViewingSessionID is the session whose output was on screen.
	ViewingSessionID string `json:"viewing_session_id,omitempty"`
	// OpenedRepos is every repo open in the TUI, including ActiveRepo.
	OpenedRepos []string `json:"opened_repos,omitempty"`
	// Version is restartStateVersion at write time.
	Version int `json:"version"`
}

// Repos returns the opened-repo list, tolerating a nil state (a cold start).
func (s *RestartState) Repos() []string {
	if s == nil {
		return nil
	}
	return s.OpenedRepos
}

// restartStateDir returns ~/.bramble/restart, alongside the session store.
func restartStateDir() (string, error) {
	dir, err := settingsDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(dir, "restart"), nil
}

// RestartStatePath returns the state file path for this process. It is keyed by
// pid so that concurrent bramble processes restarting at once cannot clobber
// each other's handoff.
func RestartStatePath() (string, error) {
	dir, err := restartStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid())), nil
}

// ownedRestartStateName matches exactly what this package writes into the
// restart directory: the pid-keyed handoff itself, and the "<name>.<random>.tmp"
// scratch file WriteRestartState renames into place. Anything else in there was
// put there by something other than bramble.
var ownedRestartStateName = regexp.MustCompile(`^\d+\.json(\.[^/]*\.tmp)?$`)

// isOwnedRestartStatePath reports whether path is a file this package created:
// it must sit directly in the restart directory and carry a name only
// WriteRestartState produces.
//
// Both deletion sites below are gated on this rather than on any list of paths
// to avoid. The removals are unconditional and silent, so the safe condition
// has to be stated positively — "this is ours" — or the next unowned shape
// anyone can name gets deleted. $BRAMBLE_RESTART_STATE in particular is an
// inherited environment variable: a stale or hand-set value must be ignored,
// never unlinked.
func isOwnedRestartStatePath(path string) bool {
	dir, err := restartStateDir()
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if filepath.Dir(abs) != absDir {
		return false
	}
	return ownedRestartStateName.MatchString(filepath.Base(abs))
}

// WriteRestartState writes state to path via temp file + rename, so a reader
// (the next process image) never observes a partial file.
func WriteRestartState(path string, state RestartState) error {
	state.Version = restartStateVersion
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create restart state dir: %w", err)
	}
	pruneStaleRestartStates(dir)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal restart state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create restart state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write restart state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close restart state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename restart state: %w", err)
	}
	return nil
}

// pruneStaleRestartStates removes handoffs nobody will ever consume: the
// replacement image deletes its own on startup, so anything left behind is from
// an exec that failed or an image that died before reading it. Best-effort —
// the directory holds at most a handful of tiny files.
func pruneStaleRestartStates(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleRestartStateAge)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || entry.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		if !ownedRestartStateName.MatchString(entry.Name()) {
			continue
		}
		os.Remove(filepath.Join(dir, entry.Name()))
	}
}

// LoadRestartState reads a state file written by WriteRestartState.
func LoadRestartState(path string) (*RestartState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read restart state: %w", err)
	}
	var state RestartState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse restart state: %w", err)
	}
	if state.Version != restartStateVersion {
		return nil, fmt.Errorf("unsupported restart state version %d (want %d)", state.Version, restartStateVersion)
	}
	return &state, nil
}

// LoadRestartStateFromEnv consumes the handoff named by $BRAMBLE_RESTART_STATE:
// it unsets the variable, loads the file, and deletes it. Returns nil when
// there is no handoff or it cannot be read — a failed restore degrades to a
// normal cold start, which is always safe.
//
// Unsetting is not optional. The variable is inherited by every child this
// process spawns, and a second restart later in this process's life would
// otherwise re-apply a stale snapshot on top of wherever the user had navigated
// to since.
func LoadRestartStateFromEnv() *RestartState {
	path := os.Getenv(RestartStateEnvVar)
	if path == "" {
		return nil
	}
	os.Unsetenv(RestartStateEnvVar)
	if !isOwnedRestartStatePath(path) {
		// Not a handoff we wrote, so it is not ours to read or unlink. Degrade
		// to a cold start.
		return nil
	}
	// Remove regardless of whether the parse succeeds: a file we cannot use is
	// a file nobody should retry.
	defer os.Remove(path)
	state, err := LoadRestartState(path)
	if err != nil {
		return nil
	}
	return state
}
