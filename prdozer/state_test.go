package prdozer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestState_LoadMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := LoadState(filepath.Join(dir, "does-not-exist.json"))
	require.NoError(t, err)
	assert.Equal(t, &State{}, s)
}

func TestState_RoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)
	want := &State{
		PRNumber:            42,
		Repo:                "yoloswe",
		LastCheckAt:         now,
		LastSeenHeadSHA:     "abc123",
		LastSeenBaseSHA:     "def456",
		LastSeenCommentIDs:  []string{"c1", "c2"},
		LastSeenCIRunIDs:    []int64{1001, 1002},
		LastAction:          LastActionPolished,
		ConsecutiveFailures: 1,
	}
	require.NoError(t, want.Save(path))
	got, err := LoadState(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestState_MergeSeenComments_DedupAndSort(t *testing.T) {
	t.Parallel()
	s := &State{LastSeenCommentIDs: []string{"b", "a"}}
	s.MergeSeenComments([]string{"a", "c"})
	assert.Equal(t, []string{"a", "b", "c"}, s.LastSeenCommentIDs)
}

func TestState_MergeSeenRuns_DedupAndSort(t *testing.T) {
	t.Parallel()
	s := &State{LastSeenCIRunIDs: []int64{2, 1}}
	s.MergeSeenRuns([]int64{1, 3})
	assert.Equal(t, []int64{1, 2, 3}, s.LastSeenCIRunIDs)
}

func TestStatePath_DeterministicShape(t *testing.T) {
	t.Parallel()
	got := StatePath("yoloswe", 42)
	assert.Contains(t, got, "yoloswe-42")
	assert.True(t, filepath.IsAbs(got))
}
