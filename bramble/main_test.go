package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
)

func newEndpointFlagsTestCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	registerLLMEndpointFlags(cmd.Flags())
	require.NoError(t, cmd.Flags().Parse(args))
	return cmd
}

// Not parallel: newSessionEndpoint reads the named env var, so these tests pin
// it rather than inherit whatever the machine happens to export.
func TestNewSessionEndpointOpenRouterPreset(t *testing.T) {
	t.Setenv(llmendpoint.OpenRouterAPIKeyEnv, "")

	endpoint, err := newSessionEndpoint(newEndpointFlagsTestCommand(t, "--llm-preset", "openrouter"))
	require.NoError(t, err)
	assert.Equal(t, llmendpoint.OpenRouter(), endpoint)
}

func TestNewSessionEndpointCustomFlags(t *testing.T) {
	t.Setenv("GATEWAY_KEY", "")

	endpoint, err := newSessionEndpoint(newEndpointFlagsTestCommand(t,
		"--llm-base-url", "https://gateway.example/v1",
		"--llm-api-key-env", "GATEWAY_KEY",
		"--llm-provider-name", "gateway",
		"--llm-wire-api", "responses",
	))
	require.NoError(t, err)
	assert.Equal(t, llmendpoint.Endpoint{
		BaseURL:      "https://gateway.example/v1",
		APIKeyEnv:    "GATEWAY_KEY",
		ProviderName: "gateway",
		Wire:         llmendpoint.WireAPIResponses,
	}, endpoint)
}

// --llm-api-key-env is typed in the user's shell, but the session is launched
// by the long-running bramble TUI in another process. Resolving the name here,
// on the side that has the value, is what makes the documented flow independent
// of whether the server was started before the key was exported.
func TestNewSessionEndpointResolvesKeyInTheClient(t *testing.T) {
	t.Setenv("GATEWAY_KEY", "gateway-secret")

	endpoint, err := newSessionEndpoint(newEndpointFlagsTestCommand(t,
		"--llm-base-url", "https://gateway.example/v1",
		"--llm-api-key-env", "GATEWAY_KEY",
	))
	require.NoError(t, err)
	assert.Equal(t, "gateway-secret", endpoint.APIKey)
	assert.Equal(t, "GATEWAY_KEY", endpoint.APIKeyEnv,
		"the name must still cross so the server can fall back to its own environment")
}

// A key the client cannot see is not an error here: the server may have it, and
// startSessionWithID reports by name if neither side does. Sending an endpoint
// with an empty APIKey and a live APIKeyEnv is what preserves that fallback.
func TestNewSessionEndpointKeepsEnvNameWhenClientCannotResolve(t *testing.T) {
	t.Setenv("GATEWAY_KEY", "")

	endpoint, err := newSessionEndpoint(newEndpointFlagsTestCommand(t,
		"--llm-base-url", "https://gateway.example/v1",
		"--llm-api-key-env", "GATEWAY_KEY",
	))
	require.NoError(t, err)
	assert.Empty(t, endpoint.APIKey)
	assert.Equal(t, "GATEWAY_KEY", endpoint.APIKeyEnv)
}

// mkBareRepo creates a fake wt-managed repo under wtRoot:
//
//	<wtRoot>/<repoName>/.bare/
//	<wtRoot>/<repoName>/<branchName>/  (a worktree directory)
func mkBareRepo(t *testing.T, wtRoot, repoName, branchName string) string {
	t.Helper()
	repoDir := filepath.Join(wtRoot, repoName)
	bareDir := filepath.Join(repoDir, ".bare")
	require.NoError(t, os.MkdirAll(bareDir, 0o755))
	worktreeDir := filepath.Join(repoDir, branchName)
	require.NoError(t, os.MkdirAll(worktreeDir, 0o755))
	return worktreeDir
}

func TestDetectRepoFromPath_FromWorktreeDir(t *testing.T) {
	t.Parallel()
	wtRoot := t.TempDir()

	worktreeDir := mkBareRepo(t, wtRoot, "myrepo", "feature-branch")

	repo, err := detectRepoFromPath(worktreeDir, wtRoot)
	require.NoError(t, err)
	assert.Equal(t, "myrepo", repo)
}

func TestDetectRepoFromPath_FromSubdir(t *testing.T) {
	t.Parallel()
	wtRoot := t.TempDir()

	worktreeDir := mkBareRepo(t, wtRoot, "myrepo", "feature-branch")
	subDir := filepath.Join(worktreeDir, "pkg", "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	repo, err := detectRepoFromPath(subDir, wtRoot)
	require.NoError(t, err)
	assert.Equal(t, "myrepo", repo)
}

func TestDetectRepoFromPath_FromRepoRoot(t *testing.T) {
	t.Parallel()
	wtRoot := t.TempDir()

	mkBareRepo(t, wtRoot, "myrepo", "main")
	repoRoot := filepath.Join(wtRoot, "myrepo")

	repo, err := detectRepoFromPath(repoRoot, wtRoot)
	require.NoError(t, err)
	assert.Equal(t, "myrepo", repo)
}

func TestDetectRepoFromPath_OutsideWtRoot(t *testing.T) {
	t.Parallel()
	wtRoot := t.TempDir()
	otherDir := t.TempDir()

	// Create a repo but look from a completely different directory.
	mkBareRepo(t, wtRoot, "myrepo", "main")

	_, err := detectRepoFromPath(otherDir, wtRoot)
	assert.Error(t, err)
}

func TestDetectRepoFromPath_NoBareDir(t *testing.T) {
	t.Parallel()
	wtRoot := t.TempDir()
	cwd := filepath.Join(wtRoot, "somerepo", "worktree")
	require.NoError(t, os.MkdirAll(cwd, 0o755))

	_, err := detectRepoFromPath(cwd, wtRoot)
	assert.Error(t, err)
}

func TestCheckCodetalkModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		modelID   string
		errSubstr string
		wantErr   bool
	}{
		{
			name:    "opus is Claude — no error",
			modelID: "opus",
			wantErr: false,
		},
		{
			name:    "sonnet is Claude — no error",
			modelID: "sonnet",
			wantErr: false,
		},
		{
			name:    "unknown model ID — no error (not routed to non-Claude)",
			modelID: "some-future-model",
			wantErr: false,
		},
		{
			name:      "gpt-5.5 is Codex — error with TUI hint",
			modelID:   "gpt-5.5",
			wantErr:   true,
			errSubstr: "bramble TUI",
		},
		{
			name:      "gemini-3-pro-preview routes to agy — error with TUI hint",
			modelID:   "gemini-3-pro-preview",
			wantErr:   true,
			errSubstr: "bramble TUI",
		},
		{
			name:      "cursor-default is Cursor — error with TUI hint",
			modelID:   "cursor-default",
			wantErr:   true,
			errSubstr: "bramble TUI",
		},
		{
			name:      "agy-default is agy — error with TUI hint",
			modelID:   "agy-default",
			wantErr:   true,
			errSubstr: "bramble TUI",
		},
		{
			name:      "gpt- prefix triggers Codex routing — error",
			modelID:   "gpt-9-future",
			wantErr:   true,
			errSubstr: "codex",
		},
		{
			name:      "gemini- prefix triggers agy routing — error",
			modelID:   "gemini-99",
			wantErr:   true,
			errSubstr: "agy",
		},
		{
			name:      "agy- prefix triggers agy routing — error",
			modelID:   "agy-future",
			wantErr:   true,
			errSubstr: "agy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkCodetalkModel(tt.modelID)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errSubstr != "" {
					assert.True(t, strings.Contains(err.Error(), tt.errSubstr),
						"expected error to contain %q, got: %v", tt.errSubstr, err)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
