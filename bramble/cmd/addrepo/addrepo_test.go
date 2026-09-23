package addrepo

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGitHubRepo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{in: "bazelment/yoloswe", want: "https://github.com/bazelment/yoloswe.git"},
		{in: "bazelment/yoloswe.git", want: "https://github.com/bazelment/yoloswe.git"},
		{in: "  bazelment/yoloswe  ", want: "https://github.com/bazelment/yoloswe.git"},
		{in: "https://github.com/bazelment/yoloswe", want: "https://github.com/bazelment/yoloswe.git"},
		{in: "https://github.com/bazelment/yoloswe.git", want: "https://github.com/bazelment/yoloswe.git"},
		{in: "https://github.com/bazelment/yoloswe/", want: "https://github.com/bazelment/yoloswe.git"},
		{in: "http://github.com/bazelment/yoloswe", want: "https://github.com/bazelment/yoloswe.git"},
		{in: "https://www.github.com/bazelment/yoloswe", want: "https://github.com/bazelment/yoloswe.git"},
		{in: "https://GitHub.com/bazelment/yoloswe", want: "https://github.com/bazelment/yoloswe.git"},
		{in: "github.com/bazelment/yoloswe", want: "https://github.com/bazelment/yoloswe.git"},
		{in: "git://github.com/bazelment/yoloswe.git", want: "https://github.com/bazelment/yoloswe.git"},
		{in: "git@github.com:bazelment/yoloswe.git", want: "git@github.com:bazelment/yoloswe.git"},
		{in: "git@github.com:bazelment/yoloswe", want: "git@github.com:bazelment/yoloswe.git"},
		{in: "ssh://git@github.com/bazelment/yoloswe.git", want: "git@github.com:bazelment/yoloswe.git"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseGitHubRepo(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseGitHubRepoRejects(t *testing.T) {
	t.Parallel()

	rejected := []string{
		"",
		"   ",
		"not-a-repo",
		"https://gitlab.com/bazelment/yoloswe",
		"git@gitlab.com:bazelment/yoloswe.git",
		"https://github.com/bazelment",
		"https://github.com/bazelment/yoloswe/tree/main",
		"https://github.com/bazelment/yoloswe?tab=readme",
		"https://example.com/bazelment/yoloswe",
		"owner/../repo",
		"../repo",
	}
	for _, in := range rejected {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := parseGitHubRepo(in)
			assert.Error(t, err)
		})
	}
}

func TestAddRepoClonesAndPrintsWorktree(t *testing.T) {
	t.Parallel()

	var gotRoot, gotURL string
	var buf bytes.Buffer
	err := addRepo(context.Background(), "https://github.com/bazelment/yoloswe", "/worktrees", &buf,
		func(root string) repoInitializer {
			gotRoot = root
			return initFunc(func(_ context.Context, cloneURL string) (string, error) {
				gotURL = cloneURL
				return filepath.Join(root, "yoloswe", "main"), nil
			})
		})
	require.NoError(t, err)
	assert.Equal(t, "/worktrees", gotRoot)
	assert.Equal(t, "https://github.com/bazelment/yoloswe.git", gotURL)
	assert.Equal(t, "added yoloswe at /worktrees/yoloswe/main\n", buf.String())
}

func TestAddRepoKeepsSSHURL(t *testing.T) {
	t.Parallel()

	var gotURL string
	err := addRepo(context.Background(), "git@github.com:bazelment/yoloswe.git", "/worktrees", io.Discard,
		func(string) repoInitializer {
			return initFunc(func(_ context.Context, cloneURL string) (string, error) {
				gotURL = cloneURL
				return "/worktrees/yoloswe/main", nil
			})
		})
	require.NoError(t, err)
	assert.Equal(t, "git@github.com:bazelment/yoloswe.git", gotURL)
}

type initFunc func(context.Context, string) (string, error)

func (f initFunc) Init(ctx context.Context, cloneURL string) (string, error) {
	return f(ctx, cloneURL)
}

func TestAddRepoDoesNotCloneRejectedURL(t *testing.T) {
	t.Parallel()

	called := false
	err := addRepo(context.Background(), "https://gitlab.com/bazelment/yoloswe", "/worktrees", io.Discard,
		func(string) repoInitializer {
			called = true
			return initFunc(func(context.Context, string) (string, error) {
				return "", nil
			})
		})
	require.Error(t, err)
	assert.False(t, called)
}

func TestAddRepoReturnsCloneError(t *testing.T) {
	t.Parallel()

	err := addRepo(context.Background(), "bazelment/yoloswe", "/worktrees", io.Discard,
		func(string) repoInitializer {
			return initFunc(func(context.Context, string) (string, error) {
				return "", assert.AnError
			})
		})
	require.ErrorIs(t, err, assert.AnError)
}

func TestAddRepoRequiresWTRoot(t *testing.T) {
	t.Parallel()

	err := addRepo(context.Background(), "bazelment/yoloswe", "  ", io.Discard, func(string) repoInitializer {
		t.Fatal("initializer should not be called")
		return nil
	})
	require.Error(t, err)
}

func TestCommandUsesWTRoot(t *testing.T) {
	wtRoot := t.TempDir()
	t.Setenv("WT_ROOT", wtRoot)

	var gotRoot, gotURL string
	orig := newInitializer
	t.Cleanup(func() {
		newInitializer = orig
		Cmd.SetOut(nil)
		Cmd.SetErr(nil)
		Cmd.SetArgs(nil)
	})
	newInitializer = func(root string) repoInitializer {
		gotRoot = root
		return initFunc(func(_ context.Context, cloneURL string) (string, error) {
			gotURL = cloneURL
			return filepath.Join(root, "yoloswe", "main"), nil
		})
	}

	var buf bytes.Buffer
	Cmd.SetOut(&buf)
	Cmd.SetErr(io.Discard)
	Cmd.SetArgs([]string{"bazelment/yoloswe"})
	require.NoError(t, Cmd.Execute())

	assert.Equal(t, wtRoot, gotRoot)
	assert.Equal(t, "https://github.com/bazelment/yoloswe.git", gotURL)
	assert.Contains(t, buf.String(), "added yoloswe at ")
}
