package taskrouter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	r := New(Config{})
	require.NotNil(t, r)
	assert.Equal(t, "gpt-5.2-codex", r.config.Model)
}

func TestNewWithModel(t *testing.T) {
	r := New(Config{Model: "custom-model"})
	require.NotNil(t, r)
	assert.Equal(t, "custom-model", r.config.Model)
}

func TestParseRouteResponse(t *testing.T) {
	tests := []struct {
		want     *RouteProposal
		name     string
		response string
		wantErr  bool
	}{
		{
			name: "valid use_existing",
			response: `{
				"action": "use_existing",
				"worktree": "feature-auth",
				"parent": "",
				"reasoning": "Task relates to existing auth work"
			}`,
			want: &RouteProposal{
				Action:    ActionUseExisting,
				Worktree:  "feature-auth",
				Parent:    "",
				Reasoning: "Task relates to existing auth work",
			},
		},
		{
			name: "valid create_new",
			response: `{
				"action": "create_new",
				"worktree": "feature-oauth",
				"parent": "main",
				"reasoning": "New feature, no existing branch"
			}`,
			want: &RouteProposal{
				Action:    ActionCreateNew,
				Worktree:  "feature-oauth",
				Parent:    "main",
				Reasoning: "New feature, no existing branch",
			},
		},
		{
			name: "create_new with empty parent defaults to main",
			response: `{
				"action": "create_new",
				"worktree": "fix-bug",
				"parent": "",
				"reasoning": "Bug fix"
			}`,
			want: &RouteProposal{
				Action:    ActionCreateNew,
				Worktree:  "fix-bug",
				Parent:    "main",
				Reasoning: "Bug fix",
			},
		},
		{
			name: "json in markdown code block",
			response: "```json\n" + `{
				"action": "use_existing",
				"worktree": "feature-x",
				"parent": "",
				"reasoning": "Matches existing work"
			}` + "\n```",
			want: &RouteProposal{
				Action:    ActionUseExisting,
				Worktree:  "feature-x",
				Parent:    "",
				Reasoning: "Matches existing work",
			},
		},
		{
			name: "json with surrounding text",
			response: `Here is my analysis:
			{
				"action": "create_new",
				"worktree": "new-feature",
				"parent": "develop",
				"reasoning": "Unrelated work"
			}
			This is the best choice.`,
			want: &RouteProposal{
				Action:    ActionCreateNew,
				Worktree:  "new-feature",
				Parent:    "develop",
				Reasoning: "Unrelated work",
			},
		},
		{
			name:     "no json",
			response: "I think you should use the main branch",
			wantErr:  true,
		},
		{
			name:     "invalid action",
			response: `{"action": "invalid", "worktree": "test", "parent": "", "reasoning": ""}`,
			wantErr:  true,
		},
		{
			name:     "empty worktree",
			response: `{"action": "use_existing", "worktree": "", "parent": "", "reasoning": ""}`,
			wantErr:  true,
		},
		{
			name:     "malformed json",
			response: `{"action": "use_existing", "worktree": "test"`,
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRouteResponse(tc.response)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want.Action, got.Action)
			assert.Equal(t, tc.want.Worktree, got.Worktree)
			assert.Equal(t, tc.want.Parent, got.Parent)
			assert.Equal(t, tc.want.Reasoning, got.Reasoning)
		})
	}
}

func TestBuildRoutingPrompt(t *testing.T) {
	t.Run("empty worktrees", func(t *testing.T) {
		req := RouteRequest{
			Prompt:    "Add user authentication",
			Worktrees: nil,
			CurrentWT: "",
			RepoName:  "my-app",
		}

		prompt := buildRoutingPrompt(req)
		assert.Contains(t, prompt, "Repository: my-app")
		assert.Contains(t, prompt, "Add user authentication")
		assert.Contains(t, prompt, "No worktrees exist yet")
	})

	t.Run("with worktrees", func(t *testing.T) {
		req := RouteRequest{
			Prompt: "Fix login bug",
			Worktrees: []WorktreeInfo{
				{
					Name:    "feature-auth",
					Goal:    "Implement OAuth login",
					Parent:  "main",
					PRState: "open",
				},
				{
					Name:    "fix-nav",
					Goal:    "Fix navigation issues",
					IsDirty: true,
				},
			},
			CurrentWT: "feature-auth",
			RepoName:  "my-app",
		}

		prompt := buildRoutingPrompt(req)
		assert.Contains(t, prompt, "feature-auth")
		assert.Contains(t, prompt, "Implement OAuth login")
		assert.Contains(t, prompt, "[PR: open]")
		assert.Contains(t, prompt, "fix-nav")
		assert.Contains(t, prompt, "[dirty]")
		assert.Contains(t, prompt, "Currently selected worktree: feature-auth")
	})

	t.Run("with cascading branch", func(t *testing.T) {
		req := RouteRequest{
			Prompt: "Add feature",
			Worktrees: []WorktreeInfo{
				{
					Name:   "feature-b",
					Goal:   "Part B of feature",
					Parent: "feature-a",
				},
			},
			RepoName: "my-app",
		}

		prompt := buildRoutingPrompt(req)
		assert.Contains(t, prompt, "(based on feature-a)")
	})
}

func TestMockRouter(t *testing.T) {
	m := NewMockRouter()

	t.Run("exact match response", func(t *testing.T) {
		m.SetResponse("add auth", &RouteProposal{
			Action:   ActionUseExisting,
			Worktree: "feature-auth",
		})

		got, err := m.Route(context.Background(), RouteRequest{Prompt: "add auth"})
		require.NoError(t, err)
		assert.Equal(t, ActionUseExisting, got.Action)
		assert.Equal(t, "feature-auth", got.Worktree)
	})

	t.Run("prefix match response", func(t *testing.T) {
		m.SetResponse("fix", &RouteProposal{
			Action:   ActionCreateNew,
			Worktree: "fix-branch",
			Parent:   "main",
		})

		got, err := m.Route(context.Background(), RouteRequest{Prompt: "fix the bug in login"})
		require.NoError(t, err)
		assert.Equal(t, ActionCreateNew, got.Action)
		assert.Equal(t, "fix-branch", got.Worktree)
	})

	t.Run("error response", func(t *testing.T) {
		m.SetError("fail", assert.AnError)

		_, err := m.Route(context.Background(), RouteRequest{Prompt: "fail"})
		assert.Error(t, err)
	})

	t.Run("default response", func(t *testing.T) {
		got, err := m.Route(context.Background(), RouteRequest{Prompt: "something new"})
		require.NoError(t, err)
		assert.Equal(t, ActionCreateNew, got.Action)
		assert.Equal(t, "new-task", got.Worktree)
		assert.Equal(t, "main", got.Parent)
	})
}

func TestProposalAction(t *testing.T) {
	assert.Equal(t, ProposalAction("use_existing"), ActionUseExisting)
	assert.Equal(t, ProposalAction("create_new"), ActionCreateNew)
}

func TestRouteWithoutStart(t *testing.T) {
	r := New(Config{})
	_, err := r.Route(context.Background(), RouteRequest{Prompt: "test"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not started")
}
