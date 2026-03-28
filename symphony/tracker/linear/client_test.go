package linear

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// newTestServer creates an httptest.Server that responds with the given handler.
// The returned client is configured to use the test server endpoint.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "test-api-key")
	return client, srv
}

func TestFetchCandidateIssues_QueryConstruction(t *testing.T) {
	t.Parallel()

	var captured graphqlRequest
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)

		// Verify Authorization header.
		if got := r.Header.Get("Authorization"); got != "test-api-key" {
			t.Errorf("Authorization header = %q, want %q", got, "test-api-key")
		}

		resp := graphqlResponse{
			Data: &graphqlData{
				Issues: graphqlIssuesConnection{
					PageInfo: graphqlPageInfo{HasNextPage: false},
					Nodes:    nil,
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	_, err := client.FetchCandidateIssues(context.Background(), []string{"In Progress", "Todo"}, "my-project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify query variables.
	vars := captured.Variables
	if vars["projectSlug"] != "my-project" {
		t.Errorf("projectSlug = %v, want %q", vars["projectSlug"], "my-project")
	}
	states, ok := vars["states"].([]any)
	if !ok || len(states) != 2 {
		t.Fatalf("states = %v, want 2-element list", vars["states"])
	}
	if states[0] != "In Progress" || states[1] != "Todo" {
		t.Errorf("states = %v, want [In Progress, Todo]", states)
	}
	first, ok := vars["first"].(float64)
	if !ok || int(first) != pageSize {
		t.Errorf("first = %v, want %d", vars["first"], pageSize)
	}
}

func TestFetchCandidateIssues_Pagination(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	cursor := "cursor-page-1"

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		_ = json.Unmarshal(body, &req)

		var resp graphqlResponse
		switch n {
		case 1:
			// First page: has next page.
			if req.Variables["after"] != nil {
				t.Errorf("first request should not have after cursor")
			}
			resp = graphqlResponse{
				Data: &graphqlData{
					Issues: graphqlIssuesConnection{
						PageInfo: graphqlPageInfo{
							HasNextPage: true,
							EndCursor:   &cursor,
						},
						Nodes: []graphqlIssue{
							{ID: "id-1", Identifier: "PROJ-1", Title: "Issue 1", State: graphqlState{Name: "Todo"}},
						},
					},
				},
			}
		case 2:
			// Second page: verify cursor, no more pages.
			if req.Variables["after"] != cursor {
				t.Errorf("second request after = %v, want %q", req.Variables["after"], cursor)
			}
			resp = graphqlResponse{
				Data: &graphqlData{
					Issues: graphqlIssuesConnection{
						PageInfo: graphqlPageInfo{HasNextPage: false},
						Nodes: []graphqlIssue{
							{ID: "id-2", Identifier: "PROJ-2", Title: "Issue 2", State: graphqlState{Name: "In Progress"}},
						},
					},
				},
			}
		default:
			t.Errorf("unexpected request #%d", n)
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	issues, err := client.FetchCandidateIssues(context.Background(), []string{"Todo", "In Progress"}, "proj")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	if issues[0].Identifier != "PROJ-1" || issues[1].Identifier != "PROJ-2" {
		t.Errorf("issues = %v, want PROJ-1 and PROJ-2", issues)
	}
	if callCount.Load() != 2 {
		t.Errorf("API called %d times, want 2", callCount.Load())
	}
}

func TestFetchCandidateIssues_MissingEndCursor(t *testing.T) {
	t.Parallel()

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := graphqlResponse{
			Data: &graphqlData{
				Issues: graphqlIssuesConnection{
					PageInfo: graphqlPageInfo{
						HasNextPage: true,
						EndCursor:   nil, // Missing cursor.
					},
					Nodes: []graphqlIssue{
						{ID: "id-1", Identifier: "PROJ-1", Title: "Issue 1", State: graphqlState{Name: "Todo"}},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	_, err := client.FetchCandidateIssues(context.Background(), []string{"Todo"}, "proj")
	if err == nil {
		t.Fatal("expected error for missing end cursor")
	}
	trackerErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if trackerErr.Category != ErrLinearMissingEndCursor {
		t.Errorf("error category = %q, want %q", trackerErr.Category, ErrLinearMissingEndCursor)
	}
}

func TestFetchCandidateIssues_MissingProjectSlug(t *testing.T) {
	t.Parallel()

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not make API call with missing project slug")
	})

	_, err := client.FetchCandidateIssues(context.Background(), []string{"Todo"}, "")
	if err == nil {
		t.Fatal("expected error for missing project slug")
	}
	trackerErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if trackerErr.Category != ErrMissingTrackerProjSlug {
		t.Errorf("error category = %q, want %q", trackerErr.Category, ErrMissingTrackerProjSlug)
	}
}

func TestNormalization_Labels(t *testing.T) {
	t.Parallel()

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := graphqlResponse{
			Data: &graphqlData{
				Issues: graphqlIssuesConnection{
					PageInfo: graphqlPageInfo{HasNextPage: false},
					Nodes: []graphqlIssue{
						{
							ID:         "id-1",
							Identifier: "PROJ-1",
							Title:      "Test",
							State:      graphqlState{Name: "Todo"},
							Labels: graphqlLabelConnection{
								Nodes: []graphqlLabel{
									{Name: "Bug"},
									{Name: "HIGH-PRIORITY"},
									{Name: "feature"},
								},
							},
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	issues, err := client.FetchCandidateIssues(context.Background(), []string{"Todo"}, "proj")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}

	labels := issues[0].Labels
	want := []string{"bug", "high-priority", "feature"}
	if len(labels) != len(want) {
		t.Fatalf("got %d labels, want %d", len(labels), len(want))
	}
	for i, w := range want {
		if labels[i] != w {
			t.Errorf("label[%d] = %q, want %q", i, labels[i], w)
		}
	}
}

func TestNormalization_BlockedBy(t *testing.T) {
	t.Parallel()

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := graphqlResponse{
			Data: &graphqlData{
				Issues: graphqlIssuesConnection{
					PageInfo: graphqlPageInfo{HasNextPage: false},
					Nodes: []graphqlIssue{
						{
							ID:         "id-1",
							Identifier: "PROJ-1",
							Title:      "Blocked issue",
							State:      graphqlState{Name: "Todo"},
							InverseRelations: graphqlRelationConnection{
								Nodes: []graphqlRelation{
									{
										Type: "blocks",
										Issue: &graphqlRelRef{
											ID:         "blocker-id",
											Identifier: "PROJ-99",
											State:      graphqlState{Name: "In Progress"},
										},
									},
									{
										Type: "related", // Should be ignored.
										Issue: &graphqlRelRef{
											ID:         "related-id",
											Identifier: "PROJ-50",
											State:      graphqlState{Name: "Done"},
										},
									},
								},
							},
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	issues, err := client.FetchCandidateIssues(context.Background(), []string{"Todo"}, "proj")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}

	blockers := issues[0].BlockedBy
	if len(blockers) != 1 {
		t.Fatalf("got %d blockers, want 1", len(blockers))
	}
	if *blockers[0].ID != "blocker-id" {
		t.Errorf("blocker ID = %q, want %q", *blockers[0].ID, "blocker-id")
	}
	if *blockers[0].Identifier != "PROJ-99" {
		t.Errorf("blocker identifier = %q, want %q", *blockers[0].Identifier, "PROJ-99")
	}
	if *blockers[0].State != "In Progress" {
		t.Errorf("blocker state = %q, want %q", *blockers[0].State, "In Progress")
	}
}

func TestNormalization_Priority(t *testing.T) {
	t.Parallel()

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		intPriority := 2.0
		floatPriority := 2.5
		resp := graphqlResponse{
			Data: &graphqlData{
				Issues: graphqlIssuesConnection{
					PageInfo: graphqlPageInfo{HasNextPage: false},
					Nodes: []graphqlIssue{
						{
							ID: "id-1", Identifier: "PROJ-1", Title: "Integer priority",
							State: graphqlState{Name: "Todo"}, Priority: &intPriority,
						},
						{
							ID: "id-2", Identifier: "PROJ-2", Title: "Float priority",
							State: graphqlState{Name: "Todo"}, Priority: &floatPriority,
						},
						{
							ID: "id-3", Identifier: "PROJ-3", Title: "Nil priority",
							State: graphqlState{Name: "Todo"}, Priority: nil,
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	issues, err := client.FetchCandidateIssues(context.Background(), []string{"Todo"}, "proj")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 3 {
		t.Fatalf("got %d issues, want 3", len(issues))
	}

	// Integer priority should be preserved.
	if issues[0].Priority == nil || *issues[0].Priority != 2 {
		t.Errorf("issue 1 priority = %v, want 2", issues[0].Priority)
	}
	// Non-integer priority should become nil.
	if issues[1].Priority != nil {
		t.Errorf("issue 2 priority = %v, want nil", *issues[1].Priority)
	}
	// Nil priority stays nil.
	if issues[2].Priority != nil {
		t.Errorf("issue 3 priority = %v, want nil", *issues[2].Priority)
	}
}

func TestNormalization_Timestamps(t *testing.T) {
	t.Parallel()

	created := "2024-06-15T10:30:00Z"
	updated := "2024-06-15T14:45:30.123Z"
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := graphqlResponse{
			Data: &graphqlData{
				Issues: graphqlIssuesConnection{
					PageInfo: graphqlPageInfo{HasNextPage: false},
					Nodes: []graphqlIssue{
						{
							ID: "id-1", Identifier: "PROJ-1", Title: "Test",
							State:     graphqlState{Name: "Todo"},
							CreatedAt: &created,
							UpdatedAt: &updated,
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	issues, err := client.FetchCandidateIssues(context.Background(), []string{"Todo"}, "proj")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	if issues[0].CreatedAt == nil {
		t.Fatal("CreatedAt should not be nil")
	}
	if issues[0].UpdatedAt == nil {
		t.Fatal("UpdatedAt should not be nil")
	}
	if issues[0].CreatedAt.Year() != 2024 || issues[0].CreatedAt.Month() != 6 || issues[0].CreatedAt.Day() != 15 {
		t.Errorf("CreatedAt = %v, want 2024-06-15", issues[0].CreatedAt)
	}
}

func TestError_TransportFailure(t *testing.T) {
	t.Parallel()

	// Create a server and immediately close it to cause a transport error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := srv.URL
	srv.Close()

	client := NewClient(endpoint, "test-key")
	_, err := client.FetchCandidateIssues(context.Background(), []string{"Todo"}, "proj")
	if err == nil {
		t.Fatal("expected error for transport failure")
	}
	trackerErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if trackerErr.Category != ErrLinearAPIRequest {
		t.Errorf("error category = %q, want %q", trackerErr.Category, ErrLinearAPIRequest)
	}
}

func TestError_NonOKStatus(t *testing.T) {
	t.Parallel()

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	})

	_, err := client.FetchCandidateIssues(context.Background(), []string{"Todo"}, "proj")
	if err == nil {
		t.Fatal("expected error for non-200 status")
	}
	trackerErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if trackerErr.Category != ErrLinearAPIStatus {
		t.Errorf("error category = %q, want %q", trackerErr.Category, ErrLinearAPIStatus)
	}
}

func TestError_GraphQLErrors(t *testing.T) {
	t.Parallel()

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := graphqlResponse{
			Errors: []graphqlError{
				{Message: "field not found"},
				{Message: "unauthorized"},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	_, err := client.FetchCandidateIssues(context.Background(), []string{"Todo"}, "proj")
	if err == nil {
		t.Fatal("expected error for GraphQL errors")
	}
	trackerErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if trackerErr.Category != ErrLinearGraphQLErrors {
		t.Errorf("error category = %q, want %q", trackerErr.Category, ErrLinearGraphQLErrors)
	}
}

func TestFetchIssuesByStates_EmptyReturnsWithoutAPICall(t *testing.T) {
	t.Parallel()

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not make API call with empty states")
	})

	issues, err := client.FetchIssuesByStates(context.Background(), []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != nil {
		t.Errorf("got %v, want nil", issues)
	}
}

func TestFetchIssueStatesByIDs_EmptyReturnsWithoutAPICall(t *testing.T) {
	t.Parallel()

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not make API call with empty IDs")
	})

	issues, err := client.FetchIssueStatesByIDs(context.Background(), []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != nil {
		t.Errorf("got %v, want nil", issues)
	}
}

func TestFetchIssuesByStates_ReturnsIssues(t *testing.T) {
	t.Parallel()

	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := graphqlResponse{
			Data: &graphqlData{
				Issues: graphqlIssuesConnection{
					Nodes: []graphqlIssue{
						{ID: "id-1", Identifier: "PROJ-1", Title: "Done issue", State: graphqlState{Name: "Done"}},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	issues, err := client.FetchIssuesByStates(context.Background(), []string{"Done"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	if issues[0].State != "Done" {
		t.Errorf("state = %q, want %q", issues[0].State, "Done")
	}
}

func TestFetchIssueStatesByIDs_ReturnsIssues(t *testing.T) {
	t.Parallel()

	var captured graphqlRequest
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)

		resp := graphqlResponse{
			Data: &graphqlData{
				Issues: graphqlIssuesConnection{
					Nodes: []graphqlIssue{
						{ID: "id-1", Identifier: "PROJ-1", Title: "Test", State: graphqlState{Name: "In Progress"}},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	issues, err := client.FetchIssueStatesByIDs(context.Background(), []string{"id-1", "id-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}

	// Verify the IDs variable was passed correctly.
	ids, ok := captured.Variables["ids"].([]any)
	if !ok {
		t.Fatalf("ids variable = %T, want []any", captured.Variables["ids"])
	}
	if len(ids) != 2 {
		t.Fatalf("got %d ids, want 2", len(ids))
	}
}
