package jiradozer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

func TestRuntimeStateRoundTrip(t *testing.T) {
	started := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "state", "123.json")
	url := "https://example.com/issue/ENG-1"
	state := RuntimeState{
		ActiveWorkflow: []ManagedWorkflowSnapshot{
			{
				Issue: &tracker.Issue{
					ID:         "issue-id",
					Identifier: "ENG-1",
					Title:      "Fix it",
					TeamID:     "team-id",
					URL:        &url,
				},
				PID:          12345,
				Branch:       "jiradozer/ENG-1",
				WorktreePath: "/worktrees/repo/jiradozer/ENG-1",
				StartedAt:    started,
			},
		},
	}

	require.NoError(t, WriteRuntimeStateAtomically(path, state))
	got, err := LoadRuntimeState(path)
	require.NoError(t, err)

	require.Len(t, got.ActiveWorkflow, 1)
	require.Equal(t, 12345, got.ActiveWorkflow[0].PID)
	require.Equal(t, "ENG-1", got.ActiveWorkflow[0].Issue.Identifier)
	require.Equal(t, "jiradozer/ENG-1", got.ActiveWorkflow[0].Branch)
	require.Equal(t, "/worktrees/repo/jiradozer/ENG-1", got.ActiveWorkflow[0].WorktreePath)
}
