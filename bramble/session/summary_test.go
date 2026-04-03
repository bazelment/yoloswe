package session_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/bazelment/yoloswe/bramble/session"
)

func TestGenerateSummary_CompletedBuilder(t *testing.T) {
	t.Parallel()

	started := time.Now().Add(-2 * time.Minute)
	completed := time.Now()
	info := session.SessionInfo{
		Type:        session.SessionTypeBuilder,
		Title:       "Add auth middleware",
		Status:      session.StatusCompleted,
		StartedAt:   &started,
		CompletedAt: &completed,
		Progress: session.SessionProgressSnapshot{
			TurnCount:    5,
			TotalCostUSD: 0.12,
			RecentOutput: []string{"All tests passing.", "Auth implementation complete."},
		},
	}

	summary := session.GenerateSummary(info)

	assert.Contains(t, summary, "builder session")
	assert.Contains(t, summary, "Add auth middleware")
	assert.Contains(t, summary, "completed successfully")
	assert.Contains(t, summary, "2 minutes")
	assert.Contains(t, summary, "5 turns")
	assert.Contains(t, summary, "$0.1200")
	assert.Contains(t, summary, "All tests passing")
}

func TestGenerateSummary_FailedPlanner(t *testing.T) {
	t.Parallel()

	started := time.Now().Add(-30 * time.Second)
	completed := time.Now()
	info := session.SessionInfo{
		Type:        session.SessionTypePlanner,
		Status:      session.StatusFailed,
		ErrorMsg:    "context deadline exceeded",
		StartedAt:   &started,
		CompletedAt: &completed,
		Progress: session.SessionProgressSnapshot{
			TurnCount: 2,
		},
	}

	summary := session.GenerateSummary(info)

	assert.Contains(t, summary, "planner session")
	assert.Contains(t, summary, "failed")
	assert.Contains(t, summary, "context deadline exceeded")
	assert.Contains(t, summary, "30 seconds")
}

func TestGenerateSummary_StoppedSession(t *testing.T) {
	t.Parallel()

	info := session.SessionInfo{
		Type:   session.SessionTypeCodeTalk,
		Title:  "Research logging",
		Status: session.StatusStopped,
	}

	summary := session.GenerateSummary(info)

	assert.Contains(t, summary, "codetalk session")
	assert.Contains(t, summary, "Research logging")
	assert.Contains(t, summary, "was stopped")
}

func TestGenerateSummary_TruncatesRecentOutput(t *testing.T) {
	t.Parallel()

	info := session.SessionInfo{
		Type:   session.SessionTypeBuilder,
		Status: session.StatusCompleted,
		Progress: session.SessionProgressSnapshot{
			RecentOutput: []string{
				"Line 1", "Line 2", "Line 3", "Line 4", "Line 5",
			},
		},
	}

	summary := session.GenerateSummary(info)

	// Should only include last 3 lines.
	assert.NotContains(t, summary, "Line 1")
	assert.NotContains(t, summary, "Line 2")
	assert.Contains(t, summary, "Line 3")
	assert.Contains(t, summary, "Line 4")
	assert.Contains(t, summary, "Line 5")
}

func TestGenerateSummary_NoTitle(t *testing.T) {
	t.Parallel()

	info := session.SessionInfo{
		Type:   session.SessionTypeDelegator,
		Status: session.StatusCompleted,
	}

	summary := session.GenerateSummary(info)

	assert.Contains(t, summary, "delegator session")
	assert.Contains(t, summary, "completed successfully")
	assert.NotContains(t, summary, "\"\"")
}
