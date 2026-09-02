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

// TestNewSessionRejectsANonexistentWorktreePath is the regression test for #335.
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

// TestNewSessionRejectsARelativeWorktreePath pins #335's relative-path
// behavior: the server never guesses a base for a path it did not read. Only
// `bramble new-session` knows the cwd `-w chore/foo` was typed against, and it
// resolves the flag before the request crosses IPC. Anchoring server-side
// instead would accept `-w .` as WT_ROOT — a real directory that is not a
// worktree, which is the mislanding this change exists to stop.
func TestNewSessionRejectsARelativeWorktreePath(t *testing.T) {
	t.Parallel()

	registry := session.NewSessionRegistry()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	t.Cleanup(mgr.Close)
	registry.Register(mgr)

	_, err := handleNewSession(context.Background(), mgr, t.TempDir(), "test-repo",
		&ipc.NewSessionParams{WorktreePath: "chore/does-not-exist", Prompt: "help"}, session.SessionInfo{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")
	assert.Empty(t, registry.GetAllSessions())
}

// TestValidateWorktreePath covers both branches of the guard. Without an accept
// case the whole check could be replaced by an unconditional error and every
// rejection test would still pass.
func TestValidateWorktreePath(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	realWorktree := filepath.Join(base, "repo", "feature/x")
	require.NoError(t, os.MkdirAll(realWorktree, 0o755))
	aFile := filepath.Join(base, "a-file")
	require.NoError(t, os.WriteFile(aFile, []byte("x"), 0o644))

	tests := []struct {
		name    string
		path    string
		wantErr string // empty means the path must be accepted
	}{
		{name: "an existing worktree directory is accepted", path: realWorktree},
		{name: "a relative path is refused, not resolved", path: "feature/x", wantErr: "must be absolute"},
		{name: "a bare dot is refused", path: ".", wantErr: "must be absolute"},
		{name: "a missing path names the remedy", path: filepath.Join(base, "gone"), wantErr: "--create-worktree"},
		{name: "a file is not a worktree", path: aFile, wantErr: "not a directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateWorktreePath(tt.path)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Contains(t, err.Error(), tt.path, "the error must name the path the caller passed")
		})
	}
}

// TestResolveWorktreeFlag covers the client half of the contract the server's
// hard rejection depends on. Without it, deleting the resolution would make
// every relative -w fail with "must be absolute" and no test would notice.
func TestResolveWorktreeFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cwd     string
		flag    string
		want    string
		wantErr string
	}{
		{name: "an empty flag stays empty", cwd: "/wt/repo/feature", flag: "", want: ""},
		{name: "an absolute path passes through unchanged", cwd: "/wt/repo/feature", flag: "/wt/other/x", want: "/wt/other/x"},
		{name: "a bare dot becomes the caller's directory", cwd: "/wt/repo/feature", flag: ".", want: "/wt/repo/feature"},
		{name: "a relative name joins the caller's directory", cwd: "/wt/repo", flag: "feature", want: "/wt/repo/feature"},
		{name: "a parent reference is resolved, not passed on", cwd: "/wt/repo/feature", flag: "../other", want: "/wt/repo/other"},
		{name: "a relative flag with no cwd is an error", cwd: "", flag: "feature", wantErr: "no working directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveWorktreeFlag(tt.cwd, tt.flag)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			if got != "" {
				assert.True(t, filepath.IsAbs(got),
					"whatever the client sends must satisfy the server's absolute-path rule")
			}
		})
	}
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
