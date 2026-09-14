package state

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// fileLock is an advisory flock(2) on a lock file.
//
// flock is advisory and cooperative: it only works because every writer agrees
// to take it. The skill's ledger.py must take the SAME lock on the SAME path for
// this to protect anything at all.
type fileLock struct{ f *os.File }

// acquire takes a shared or exclusive flock, retrying until timeout.
//
// It polls with LOCK_NB rather than blocking in the syscall so the wait is
// bounded. An unbounded blocking acquire turns a deadlock into a hang, which in
// a background tick is indistinguishable from the swarm simply being idle — the
// exact failure mode this harness exists to eliminate.
func acquire(path string, exclusive bool, timeout time.Duration) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}

	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	how |= syscall.LOCK_NB

	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if err == nil {
			return &fileLock{f: f}, nil
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("flock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("%w after %s (%s)", ErrLockTimeout, timeout, path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// release drops the lock. The lock FILE is intentionally left on disk: removing
// it would let a later caller create a fresh inode and lock that instead, so two
// processes could hold "the" lock simultaneously.
func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
	l.f = nil
}
