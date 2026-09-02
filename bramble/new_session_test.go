package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/ipc"
	"github.com/bazelment/yoloswe/bramble/session"
)

// TestParentForSpawnDropsAnUnresolvableInheritedParent pins the difference
// between a parent the caller asked for and one it merely inherited from
// $BRAMBLE_SESSION_ID. Every command run inside a bramble window carries that
// variable, and the registry only sees sessions adopted into an open manager —
// so treating both alike costs an agent whose own repo is closed the ability to
// spawn anything at all.
func TestParentForSpawnDropsAnUnresolvableInheritedParent(t *testing.T) {
	t.Parallel()

	registry := session.NewSessionRegistry()
	params := &ipc.NewSessionParams{ParentSessionID: "gone", ParentInherited: true}

	parent, ok, err := parentForSpawn(registry, params)
	require.NoError(t, err, "an inherited parent that cannot be resolved must not fail the spawn")
	assert.False(t, ok)
	assert.Empty(t, parent.ID)
	assert.Empty(t, params.ParentSessionID,
		"the unresolvable ID must be cleared, or the session is filed under a parent nothing can look up")
}

// TestParentForSpawnRejectsAnUnresolvableExplicitParent is the other half: an
// ID the caller typed is a claim about a session that should exist, and
// silently spawning top-level would hide the typo.
func TestParentForSpawnRejectsAnUnresolvableExplicitParent(t *testing.T) {
	t.Parallel()

	registry := session.NewSessionRegistry()
	params := &ipc.NewSessionParams{ParentSessionID: "gone"}

	_, ok, err := parentForSpawn(registry, params)
	require.Error(t, err)
	assert.False(t, ok)
	assert.Contains(t, err.Error(), "gone")
	assert.Equal(t, "gone", params.ParentSessionID, "an explicit parent is not silently dropped")
}

// TestResolveParentSessionIDReportsWhereTheIDCameFrom guards the input the
// server's decision above rests on.
func TestResolveParentSessionIDReportsWhereTheIDCameFrom(t *testing.T) {
	t.Parallel()

	id, inherited := resolveParentSessionID("explicit", "from-env", false)
	assert.Equal(t, "explicit", id)
	assert.False(t, inherited, "a --parent flag is a claim, not a default")

	id, inherited = resolveParentSessionID("", "from-env", false)
	assert.Equal(t, "from-env", id)
	assert.True(t, inherited)

	id, inherited = resolveParentSessionID("", "from-env", true)
	assert.Empty(t, id, "--no-parent wins over the environment")
	assert.False(t, inherited)

	id, inherited = resolveParentSessionID("", "", false)
	assert.Empty(t, id)
	assert.False(t, inherited, "nothing to inherit is not an inherited parent")
}

// TestNewSessionRefusesToInheritAWorktreeFromAnotherRepo keeps a subagent's
// registration and its files in the same repo. Without the guard an explicit
// --repo selects one manager while the inherited worktree belongs to another,
// so the session's metadata and persisted history land under a repo whose tree
// it is not working in.
func TestNewSessionRefusesToInheritAWorktreeFromAnotherRepo(t *testing.T) {
	t.Parallel()

	parent := session.SessionInfo{
		ID:           "parent-1",
		RepoName:     "repo-a",
		WorktreePath: "/wt/repo-a/feature",
	}
	// nil manager on purpose: the guard must refuse before anything is started.
	_, err := handleNewSession(context.Background(), nil, "/wt", "repo-b",
		&ipc.NewSessionParams{Prompt: "help"}, parent)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo-a")
	assert.Contains(t, err.Error(), "repo-b")
}

// TestNewSessionRejectsANonexistentWorktreePath is the regression test for
// issue #335: a --worktree that does not exist on disk used to sail through
// with only a non-empty check, and tmux would silently fall back to the
// server's own cwd — landing the session somewhere the caller never asked
// for while the IPC response still reported success. It must fail loudly
// and register nothing.
func TestNewSessionRejectsANonexistentWorktreePath(t *testing.T) {
	t.Parallel()

	wtRoot := t.TempDir()
	missing := filepath.Join(wtRoot, "does-not-exist")

	registry := session.NewSessionRegistry()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	t.Cleanup(mgr.Close)
	registry.Register(mgr)

	_, err := handleNewSession(context.Background(), mgr, wtRoot, "test-repo",
		&ipc.NewSessionParams{WorktreePath: missing, Prompt: "help"}, session.SessionInfo{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), missing, "the error must name the resolved path that confused the caller")
	assert.Contains(t, err.Error(), "--create-worktree")
	assert.Empty(t, registry.GetAllSessions(),
		"an error that still leaves a session registered is only half a fix")
}

// TestNewSessionRejectsAWorktreePathThatIsAFile guards the other half of the
// os.Stat check: a path that exists but is not a directory (e.g. a stray
// file left over from a botched worktree) must not be handed to tmux as a
// start directory either.
func TestNewSessionRejectsAWorktreePathThatIsAFile(t *testing.T) {
	t.Parallel()

	wtRoot := t.TempDir()
	notADir := filepath.Join(wtRoot, "not-a-dir")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o644))

	registry := session.NewSessionRegistry()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	t.Cleanup(mgr.Close)
	registry.Register(mgr)

	_, err := handleNewSession(context.Background(), mgr, wtRoot, "test-repo",
		&ipc.NewSessionParams{WorktreePath: notADir, Prompt: "help"}, session.SessionInfo{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
	assert.Empty(t, registry.GetAllSessions())
}

// TestNewSessionResolvesARelativeWorktreeAgainstWTRoot pins the decision for
// issue #335's second ask: `-w chore/foo` must mean the same thing regardless
// of the caller's cwd, so it is resolved against wtRoot rather than the
// process's working directory. The nonexistent-path branch below proves the
// resolution actually happened (the error names the joined absolute path);
// the sentinel session_type proves resolution runs before anything else that
// would need a real manager.
func TestNewSessionResolvesARelativeWorktreeAgainstWTRoot(t *testing.T) {
	t.Parallel()

	wtRoot := t.TempDir()

	t.Run("nonexistent relative path is resolved and then rejected", func(t *testing.T) {
		t.Parallel()

		registry := session.NewSessionRegistry()
		mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
		t.Cleanup(mgr.Close)
		registry.Register(mgr)

		_, err := handleNewSession(context.Background(), mgr, wtRoot, "test-repo",
			&ipc.NewSessionParams{WorktreePath: "chore/does-not-exist", Prompt: "help"}, session.SessionInfo{})

		require.Error(t, err)
		assert.Contains(t, err.Error(), filepath.Join(wtRoot, "chore/does-not-exist"),
			"a relative path must be resolved against wtRoot before it is reported back")
		assert.Empty(t, registry.GetAllSessions())
	})

	t.Run("existing relative path resolves and clears the stat check", func(t *testing.T) {
		t.Parallel()

		existing := filepath.Join(wtRoot, "chore", "real")
		require.NoError(t, os.MkdirAll(existing, 0o755))

		// mgr is nil on purpose: an invalid session_type errors out before
		// mgr is ever touched, so reaching that error (instead of the stat
		// error) proves the relative path resolved to a real directory.
		_, err := handleNewSession(context.Background(), nil, wtRoot, "test-repo",
			&ipc.NewSessionParams{WorktreePath: "chore/real", SessionType: "bogus", Prompt: "help"}, session.SessionInfo{})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown session_type")
	})
}

// TestRepoForSpawnPrefersTheParentOverAnInferredRepo applies to the repo the
// rule round 6 established for the parent: a value the client guessed is not a
// claim the caller made. `bramble new-session` auto-detects the repo from cwd
// whenever --repo is omitted, so without this the guess is indistinguishable
// from a flag — the parent's repo would effectively never win, and the
// cross-repo refusal below would fire on a spawn nobody asked to redirect.
func TestRepoForSpawnPrefersTheParentOverAnInferredRepo(t *testing.T) {
	t.Parallel()
	parent := session.SessionInfo{ID: "parent-1", RepoName: "repo-a"}

	assert.Equal(t, "repo-a", repoForSpawn(
		&ipc.NewSessionParams{RepoName: "repo-b", RepoInferred: true}, parent, true, "initial"),
		"a cwd guess must lose to the parent that knows its own repo")

	assert.Equal(t, "repo-b", repoForSpawn(
		&ipc.NewSessionParams{RepoName: "repo-b"}, parent, true, "initial"),
		"a --repo the caller typed still wins; handleNewSession decides if that conflicts")

	assert.Equal(t, "repo-a", repoForSpawn(
		&ipc.NewSessionParams{}, parent, true, "initial"),
		"with no repo at all the parent still pins it")

	assert.Equal(t, "repo-b", repoForSpawn(
		&ipc.NewSessionParams{RepoName: "repo-b", RepoInferred: true}, session.SessionInfo{}, false, "initial"),
		"with no parent the inferred repo is the best information there is")

	assert.Equal(t, "initial", repoForSpawn(
		&ipc.NewSessionParams{}, session.SessionInfo{}, false, "initial"),
		"nothing to go on falls back to the repo bramble was launched on")
}
