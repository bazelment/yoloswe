package main

import (
	"context"
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
