package session

import (
	"os"
	"testing"
)

// TestMain gives the package a private HOME.
//
// Two reasons, both load-bearing. Bazel's test sandbox defines no HOME at all,
// so anything resolving os.UserHomeDir — the delivery queue, the session store,
// a subagent's result file — fails outright. And when HOME *is* defined, tests
// that write under it would write into the developer's real ~/.bramble and
// leave their fixtures in it.
//
// Set for the whole package rather than per test so tests keep running in
// parallel: t.Setenv forbids t.Parallel.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "bramble-session-test-home-")
	if err != nil {
		panic("create test home: " + err.Error())
	}
	if err := os.Setenv("HOME", home); err != nil {
		panic("set test home: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
