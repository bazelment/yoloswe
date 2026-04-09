package github

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/wt"
)

// mockGHRunner records calls and returns canned responses.
type mockGHRunner struct {
	// responses maps a key derived from args to a CmdResult.
	responses map[string]*wt.CmdResult
	// defaultResponse is returned when no matching response is found.
	defaultResponse *wt.CmdResult
	defaultErr      error
	calls           [][]string
}

func (m *mockGHRunner) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	m.calls = append(m.calls, args)

	// Look for a response matching any arg (to find the API path regardless of position).
	for _, arg := range args {
		if resp, ok := m.responses[arg]; ok {
			return resp, nil
		}
	}

	if m.defaultResponse != nil {
		return m.defaultResponse, m.defaultErr
	}
	return &wt.CmdResult{}, m.defaultErr
}

func jsonResult(v any) *wt.CmdResult {
	data, _ := json.Marshal(v)
	return &wt.CmdResult{Stdout: string(data)}
}

func TestParseIdentifier(t *testing.T) {
	tests := []struct {
		input   string
		owner   string
		repo    string
		number  int
		wantErr bool
	}{
		{"owner/repo#123", "owner", "repo", 123, false},
		{"org/my-repo#1", "org", "my-repo", 1, false},
		{"foo/bar#0", "", "", 0, true},
		{"foo/bar#abc", "", "", 0, true},
		{"no-hash", "", "", 0, true},
		{"#123", "", "", 0, true},
		{"/repo#123", "", "", 0, true},
		{"owner/#123", "", "", 0, true},

		// GitHub issue URLs.
		{"https://github.com/owner/repo/issues/123", "owner", "repo", 123, false},
		{"http://github.com/owner/repo/issues/456", "owner", "repo", 456, false},
		{"https://github.com/owner/repo/issues/123#issuecomment-789", "owner", "repo", 123, false},
		{"https://github.enterprise.com/org/my-repo/issues/7", "org", "my-repo", 7, false},
		{"https://github.com/owner/repo/pulls/123", "", "", 0, true},
		{"https://github.com/owner/repo/issues/abc", "", "", 0, true},
		{"https://github.com/owner/repo/issues/0", "", "", 0, true},
		{"https://github.com/owner/repo", "", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			owner, repo, number, err := ParseIdentifier(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.owner, owner)
			assert.Equal(t, tt.repo, repo)
			assert.Equal(t, tt.number, number)
		})
	}
}

func TestParseOwnerRepo(t *testing.T) {
	tests := []struct {
		input   string
		owner   string
		repo    string
		wantErr bool
	}{
		{"owner/repo", "owner", "repo", false},
		{"org/my-repo", "org", "my-repo", false},
		{"noslash", "", "", true},
		{"/repo", "", "", true},
		{"owner/", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			owner, repo, err := ParseOwnerRepo(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.owner, owner)
			assert.Equal(t, tt.repo, repo)
		})
	}
}

func TestFetchIssue(t *testing.T) {
	body := "Fix the bug description"
	htmlURL := "https://github.com/acme/app/issues/42"

	mock := &mockGHRunner{
		responses: map[string]*wt.CmdResult{
			"repos/acme/app/issues/42": jsonResult(ghIssue{
				Number:  42,
				Title:   "Fix the bug",
				Body:    &body,
				State:   "open",
				HTMLURL: htmlURL,
				Labels:  []ghLabel{{Name: "bug"}, {Name: "urgent"}},
				User:    ghUser{Login: "alice"},
			}),
		},
	}

	client := NewClient(mock, "acme", "app")
	issue, err := client.FetchIssue(context.Background(), "acme/app#42")
	require.NoError(t, err)

	assert.Equal(t, "42", issue.ID)
	assert.Equal(t, "acme/app#42", issue.Identifier)
	assert.Equal(t, "Fix the bug", issue.Title)
	assert.Equal(t, "open", issue.State)
	assert.Equal(t, "acme/app", issue.TeamID)
	assert.Equal(t, &body, issue.Description)
	assert.Equal(t, &htmlURL, issue.URL)
	assert.Equal(t, []string{"bug", "urgent"}, issue.Labels)
}

func TestFetchIssueNotFound(t *testing.T) {
	mock := &mockGHRunner{
		defaultErr: fmt.Errorf("HTTP 404"),
		defaultResponse: &wt.CmdResult{
			Stderr:   "Not Found",
			ExitCode: 1,
		},
	}

	client := NewClient(mock, "acme", "app")
	_, err := client.FetchIssue(context.Background(), "acme/app#999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch issue")
}

func TestListIssues(t *testing.T) {
	body := "desc"
	mock := &mockGHRunner{
		responses: map[string]*wt.CmdResult{
			"repos/acme/app/issues?state=open&per_page=10&labels=jiradozer": jsonResult([]ghIssue{
				{Number: 1, Title: "Issue 1", State: "open", Body: &body},
				{Number: 2, Title: "PR 1", State: "open", PullRequest: &ghPullRequest{URL: "x"}},
				{Number: 3, Title: "Issue 2", State: "open"},
			}),
		},
	}

	client := NewClient(mock, "acme", "app")
	issues, err := client.ListIssues(context.Background(), tracker.IssueFilter{
		Filters: map[string]string{
			"team":  "acme/app",
			"state": "Todo",
			"label": "jiradozer",
		},
		Limit: 10,
	})
	require.NoError(t, err)

	// PR should be filtered out.
	require.Len(t, issues, 2)
	assert.Equal(t, "1", issues[0].ID)
	assert.Equal(t, "3", issues[1].ID)
}

func TestListIssues_MultiLabel_ORSemantics(t *testing.T) {
	mock := &mockGHRunner{
		responses: map[string]*wt.CmdResult{
			"repos/acme/app/issues?state=open&per_page=3&labels=bug": jsonResult([]ghIssue{
				{Number: 1, Title: "Bug 1", State: "open"},
				{Number: 2, Title: "Bug 2", State: "open"},
			}),
			"repos/acme/app/issues?state=open&per_page=3&labels=feature": jsonResult([]ghIssue{
				{Number: 2, Title: "Bug 2", State: "open"}, // duplicate
				{Number: 3, Title: "Feature 1", State: "open"},
				{Number: 4, Title: "Feature 2", State: "open"},
			}),
		},
	}

	client := NewClient(mock, "acme", "app")
	issues, err := client.ListIssues(context.Background(), tracker.IssueFilter{
		Filters: map[string]string{
			"team":  "acme/app",
			"state": "Todo",
			"label": "bug,feature",
		},
		Limit: 3,
	})
	require.NoError(t, err)

	// Should dedup issue #2, and respect Limit=3.
	require.Len(t, issues, 3)
	assert.Equal(t, "1", issues[0].ID)
	assert.Equal(t, "2", issues[1].ID)
	assert.Equal(t, "3", issues[2].ID)
}

func TestFetchComments(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	mock := &mockGHRunner{
		responses: map[string]*wt.CmdResult{
			"/user": jsonResult(ghUser{Login: "bot-user"}),
			fmt.Sprintf("repos/acme/app/issues/42/comments?since=%s", now.Add(-time.Hour).Format(time.RFC3339)): jsonResult([]ghComment{
				{ID: 101, Body: "approve", CreatedAt: now.Format(time.RFC3339), User: ghUser{Login: "alice"}},
				{ID: 102, Body: "Plan posted", CreatedAt: now.Format(time.RFC3339), User: ghUser{Login: "bot-user"}},
			}),
		},
	}

	client := NewClient(mock, "acme", "app")
	comments, err := client.FetchComments(context.Background(), "42", now.Add(-time.Hour))
	require.NoError(t, err)

	require.Len(t, comments, 2)
	assert.Equal(t, "approve", comments[0].Body)
	assert.Equal(t, "alice", comments[0].UserName)
	assert.False(t, comments[0].IsSelf)
	assert.Equal(t, "Plan posted", comments[1].Body)
	assert.True(t, comments[1].IsSelf)
}

func TestFetchWorkflowStates(t *testing.T) {
	client := NewClient(&mockGHRunner{}, "acme", "app")
	states, err := client.FetchWorkflowStates(context.Background(), "acme/app")
	require.NoError(t, err)

	require.Len(t, states, 3)
	assert.Equal(t, "open", states[0].ID)
	assert.Equal(t, "In Progress", states[0].Name)
	assert.Equal(t, "started", states[0].Type)
	assert.Equal(t, "label:in-review", states[1].ID)
	assert.Equal(t, "In Review", states[1].Name)
	assert.Equal(t, "closed", states[2].ID)
	assert.Equal(t, "Done", states[2].Name)
	assert.Equal(t, "completed", states[2].Type)
}

func TestPostComment(t *testing.T) {
	mock := &mockGHRunner{
		responses: map[string]*wt.CmdResult{
			"repos/acme/app/issues/42/comments": jsonResult(ghComment{
				ID:        201,
				Body:      "test comment",
				CreatedAt: "2025-01-15T10:00:00Z",
				User:      ghUser{Login: "bot-user"},
			}),
		},
	}

	client := NewClient(mock, "acme", "app")
	comment, err := client.PostComment(context.Background(), "42", "test comment")
	require.NoError(t, err)

	assert.Equal(t, "201", comment.ID)
	assert.Equal(t, "test comment", comment.Body)
	assert.True(t, comment.IsSelf)
	assert.Equal(t, time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC), comment.CreatedAt)

	// Verify the call was made with correct args.
	require.Len(t, mock.calls, 1)
	assert.Contains(t, mock.calls[0], "-X")
	assert.Contains(t, mock.calls[0], "POST")
}

func TestUpdateIssueState_Close(t *testing.T) {
	mock := &mockGHRunner{
		defaultResponse: &wt.CmdResult{Stdout: "{}"},
	}

	client := NewClient(mock, "acme", "app")
	err := client.UpdateIssueState(context.Background(), "42", "closed")
	require.NoError(t, err)

	// Should have called PATCH to close + DELETE to remove label.
	require.Len(t, mock.calls, 2)
	assert.Contains(t, mock.calls[0], "repos/acme/app/issues/42")
	assert.Contains(t, mock.calls[0], "state=closed")
	assert.Contains(t, mock.calls[1], "DELETE")
}

func TestUpdateIssueState_Label(t *testing.T) {
	mock := &mockGHRunner{
		defaultResponse: &wt.CmdResult{Stdout: "{}"},
	}

	client := NewClient(mock, "acme", "app")
	err := client.UpdateIssueState(context.Background(), "42", "label:in-review")
	require.NoError(t, err)

	require.Len(t, mock.calls, 1)
	assert.Contains(t, mock.calls[0], "POST")
	assert.Contains(t, mock.calls[0], "repos/acme/app/issues/42/labels")
}

func TestUpdateIssueState_Open(t *testing.T) {
	mock := &mockGHRunner{
		defaultResponse: &wt.CmdResult{Stdout: "{}"},
	}

	client := NewClient(mock, "acme", "app")
	err := client.UpdateIssueState(context.Background(), "42", "open")
	require.NoError(t, err)

	require.Len(t, mock.calls, 2)
	assert.Contains(t, mock.calls[0], "state=open")
	assert.Contains(t, mock.calls[1], "DELETE")
}

func TestUpdateIssueState_Unknown(t *testing.T) {
	client := NewClient(&mockGHRunner{}, "acme", "app")
	err := client.UpdateIssueState(context.Background(), "42", "unknown")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown state ID")
}
