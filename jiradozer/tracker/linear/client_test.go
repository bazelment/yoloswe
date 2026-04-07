package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchIssue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "test-key", r.Header.Get("Authorization"))

		resp := graphqlResponse{
			Data: &graphqlData{
				Issues: graphqlIssuesConnection{
					Nodes: []graphqlIssue{
						{
							ID:         "issue-1",
							Identifier: "ENG-123",
							Title:      "Fix the bug",
							State:      graphqlStateRef{ID: "state-1", Name: "Todo", Type: "unstarted"},
							Team:       &graphqlTeamRef{ID: "team-1"},
							Labels:     graphqlLabels{Nodes: []graphqlLabel{{Name: "bug"}}},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL, "test-key")
	issue, err := client.FetchIssue(context.Background(), "ENG-123")
	require.NoError(t, err)
	assert.Equal(t, "issue-1", issue.ID)
	assert.Equal(t, "ENG-123", issue.Identifier)
	assert.Equal(t, "Fix the bug", issue.Title)
	assert.Equal(t, "Todo", issue.State)
	assert.Equal(t, "team-1", issue.TeamID)
	assert.Equal(t, []string{"bug"}, issue.Labels)
}

func TestFetchIssueNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := graphqlResponse{
			Data: &graphqlData{
				Issues: graphqlIssuesConnection{Nodes: nil},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL, "test-key")
	_, err := client.FetchIssue(context.Background(), "ENG-999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issue not found")
}

func TestFetchComments(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := graphqlResponse{
			Data: &graphqlData{
				Comments: graphqlCommentsConnection{
					PageInfo: graphqlPageInfo{HasNextPage: false},
					Nodes: []graphqlComment{
						{
							ID:        "comment-1",
							Body:      "approve",
							CreatedAt: now.Add(-time.Hour).Format(time.RFC3339),
							User:      &graphqlUser{ID: "user-1", Name: "Alice", IsMe: false},
						},
						{
							ID:        "comment-2",
							Body:      "Plan posted",
							CreatedAt: now.Format(time.RFC3339),
							User:      &graphqlUser{ID: "bot-1", Name: "Bot", IsMe: true},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL, "test-key")
	comments, err := client.FetchComments(context.Background(), "issue-1", time.Time{})
	require.NoError(t, err)
	require.Len(t, comments, 2)
	assert.Equal(t, "approve", comments[0].Body)
	assert.Equal(t, "Alice", comments[0].UserName)
	assert.False(t, comments[0].IsSelf)
	assert.True(t, comments[1].IsSelf)
}

func TestFetchCommentsSinceFilter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := graphqlResponse{
			Data: &graphqlData{
				Comments: graphqlCommentsConnection{
					PageInfo: graphqlPageInfo{HasNextPage: false},
					Nodes: []graphqlComment{
						{
							ID:        "comment-old",
							Body:      "old comment",
							CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
						},
						{
							ID:        "comment-new",
							Body:      "new comment",
							CreatedAt: now.Format(time.RFC3339),
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL, "test-key")
	comments, err := client.FetchComments(context.Background(), "issue-1", now.Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "new comment", comments[0].Body)
}

func TestFetchWorkflowStates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := graphqlResponse{
			Data: &graphqlData{
				WorkflowStates: graphqlWorkflowStatesConnection{
					Nodes: []graphqlWorkflowState{
						{ID: "s1", Name: "Todo", Type: "unstarted"},
						{ID: "s2", Name: "In Progress", Type: "started"},
						{ID: "s3", Name: "Done", Type: "completed"},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL, "test-key")
	states, err := client.FetchWorkflowStates(context.Background(), "team-1")
	require.NoError(t, err)
	require.Len(t, states, 3)
	assert.Equal(t, "In Progress", states[1].Name)
	assert.Equal(t, "started", states[1].Type)
}

func TestPostComment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		json.NewDecoder(r.Body).Decode(&req)
		assert.Contains(t, req.Query, "commentCreate")

		resp := graphqlResponse{
			Data: &graphqlData{
				CommentCreate: graphqlMutationResult{
					Success: true,
					Comment: &graphqlCommentResult{
						ID:        "comment-1",
						Body:      "test comment",
						CreatedAt: "2025-01-15T10:00:00Z",
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL, "test-key")
	comment, err := client.PostComment(context.Background(), "issue-1", "test comment")
	require.NoError(t, err)
	assert.Equal(t, "comment-1", comment.ID)
	assert.True(t, comment.IsSelf)
}

func TestUpdateIssueState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		json.NewDecoder(r.Body).Decode(&req)
		assert.Contains(t, req.Query, "issueUpdate")

		resp := graphqlResponse{
			Data: &graphqlData{
				IssueUpdate: graphqlMutationResult{Success: true},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL, "test-key")
	err := client.UpdateIssueState(context.Background(), "issue-1", "state-2")
	require.NoError(t, err)
}

func TestGraphQLError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := graphqlResponse{
			Errors: []graphqlError{{Message: "something went wrong"}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL, "test-key")
	_, err := client.FetchIssue(context.Background(), "ENG-123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "something went wrong")
}

func TestHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL, "test-key")
	_, err := client.FetchIssue(context.Background(), "ENG-123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "linear API returned HTTP 500")
}
