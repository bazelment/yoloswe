package github

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizePRReviewThreadsFiltersBotsAndResolved(t *testing.T) {
	line42 := 42
	line1 := 1
	line3 := 3
	raw := []ghPRReviewThread{
		{
			Comments: struct {
				Nodes []ghPRThreadComment `json:"nodes"`
			}{Nodes: []ghPRThreadComment{{
				Author:    ghReviewUser{Login: "alice", Typename: "User"},
				Body:      "fix the nil case",
				Path:      "foo.go",
				Line:      &line42,
				CreatedAt: "2026-05-05T12:00:00Z",
			}}},
		},
		{
			Comments: struct {
				Nodes []ghPRThreadComment `json:"nodes"`
			}{Nodes: []ghPRThreadComment{{
				Author:    ghReviewUser{Login: "dependabot[bot]", Typename: "Bot"},
				Body:      "bot noise",
				Path:      "go.mod",
				Line:      &line1,
				CreatedAt: "2026-05-05T12:01:00Z",
			}}},
		},
		{
			IsResolved: true,
			Comments: struct {
				Nodes []ghPRThreadComment `json:"nodes"`
			}{Nodes: []ghPRThreadComment{{
				Author:    ghReviewUser{Login: "bob", Typename: "User"},
				Body:      "already resolved",
				Path:      "bar.go",
				Line:      &line3,
				CreatedAt: "2026-05-05T12:02:00Z",
			}}},
		},
	}

	got, err := normalizePRReviewThreads(raw)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "foo.go", got[0].Path)
	assert.Equal(t, 42, got[0].Line)
	assert.Equal(t, "fix the nil case", got[0].Body)
	assert.Equal(t, "alice", got[0].Author)
}

func TestNormalizePRReviewsSkipsApprovedWithoutActionableBody(t *testing.T) {
	raw := []ghPRReview{
		{
			User:        ghReviewUser{Login: "alice", Type: "User"},
			State:       "APPROVED",
			Body:        "looks good",
			SubmittedAt: "2026-05-05T12:00:00Z",
		},
		{
			User:        ghReviewUser{Login: "carol", Type: "User"},
			State:       "CHANGES_REQUESTED",
			Body:        "please make timeout configurable",
			SubmittedAt: "2026-05-05T12:01:00Z",
		},
	}

	got, err := normalizePRReviews(raw)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "carol", got[0].Author)
	assert.Equal(t, "please make timeout configurable", got[0].Body)
}

func TestFormatPRReviewFeedback(t *testing.T) {
	got := FormatPRReviewFeedback([]ReviewComment{{
		Path:   "foo.go",
		Line:   7,
		Author: "alice",
		Body:   "tighten this",
	}})

	assert.Contains(t, got, `foo.go:7 by @alice: "tighten this"`)
}
