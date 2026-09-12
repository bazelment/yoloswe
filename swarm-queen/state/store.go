package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// LockName is the advisory lock guarding state.json. It is a separate inode from
// state.json itself, created on demand and never deleted — deleting a lock file
// is its own race.
//
// This path is a cross-tool contract with the skill's ledger.py. Two tools each
// inventing their own lock file is no lock at all.
const LockName = "state.json.lock"

// StateName is the ledger file, shared with ledger.py.
const StateName = "state.json"

// DefaultLockTimeout bounds how long a caller waits for the lock.
const DefaultLockTimeout = 10 * time.Second

// ErrLockTimeout means the lock could not be acquired in time.
//
// A tick that cannot acquire the lock MUST abort and report. It must never fall
// back to an unlocked read: proceeding on a possibly-stale read is the same
// class of error as believing a `.done` file, applied to our own state.
var ErrLockTimeout = errors.New("timed out acquiring state lock")

// Store reads and writes a run directory's ledger.
type Store struct {
	Dir         string
	LockTimeout time.Duration
}

// NewStore returns a Store for a run directory.
func NewStore(dir string) *Store {
	return &Store{Dir: dir, LockTimeout: DefaultLockTimeout}
}

func (s *Store) statePath() string { return filepath.Join(s.Dir, StateName) }
func (s *Store) lockPath() string  { return filepath.Join(s.Dir, LockName) }

func (s *Store) timeout() time.Duration {
	if s.LockTimeout <= 0 {
		return DefaultLockTimeout
	}
	return s.LockTimeout
}

// Read loads the ledger under a SHARED lock.
//
// Read paths take LOCK_SH deliberately. watch_lanes.sh polls the ledger every
// INTERVAL seconds; if reads took an exclusive lock they would block the
// reconcile writer on every poll, surfacing as intermittent tick stalls that
// look like a swarm-queen bug rather than lock contention.
func (s *Store) Read() (*State, error) {
	lk, err := acquire(s.lockPath(), false, s.timeout())
	if err != nil {
		return nil, err
	}
	defer lk.release()
	return s.readLocked()
}

func (s *Store) readLocked() (*State, error) {
	b, err := os.ReadFile(s.statePath())
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.statePath(), err)
	}
	// ledger.py defaults a missing priority to p2 on load; match it so the two
	// tools agree on dispatch order.
	for _, l := range st.Lanes {
		if l.Priority == "" {
			l.Priority = P2
		}
		if l.Sessions == nil {
			l.Sessions = map[string]string{}
		}
	}
	return &st, nil
}

// Update runs fn against the ledger under one EXCLUSIVE lock held across the
// whole read-modify-write, then writes atomically.
//
// The lock spans read and write together on purpose. Acquiring it only for the
// write allows a lost update: two callers read the same state, both mutate, and
// whoever writes second silently discards the other's changes.
func (s *Store) Update(fn func(*State) error) error {
	lk, err := acquire(s.lockPath(), true, s.timeout())
	if err != nil {
		return err
	}
	defer lk.release()

	st, err := s.readLocked()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	if err := st.Validate(); err != nil {
		return fmt.Errorf("refusing to write invalid ledger: %w", err)
	}
	return s.writeLocked(st)
}

// Create initialises a new ledger, failing if one already exists.
func (s *Store) Create(cfg Config) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	lk, err := acquire(s.lockPath(), true, s.timeout())
	if err != nil {
		return err
	}
	defer lk.release()

	if _, err := os.Stat(s.statePath()); err == nil {
		return fmt.Errorf("ledger already exists at %s", s.statePath())
	}
	return s.writeLocked(&State{Config: cfg, Lanes: []*Lane{}})
}

// writeLocked serialises state via a temp file in the same directory followed by
// an atomic rename, so a concurrent reader observes either the old file or the
// new one and never a truncated one. Callers must already hold the lock.
func (s *Store) writeLocked(st *State) error {
	if st.Lanes == nil {
		st.Lanes = []*Lane{}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeFileAtomic(s.statePath(), b, 0o644)
}

// writeFileAtomic writes via a same-directory temp file plus rename, fsyncing
// before the rename so the content is durable when it becomes visible.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
