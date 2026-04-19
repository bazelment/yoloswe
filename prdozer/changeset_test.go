package prdozer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func snapWithRollup(rollup string) *Snapshot {
	return &Snapshot{
		PR: PRDetails{
			Number:         42,
			HeadRefOid:     "head1",
			BaseRefName:    "main",
			State:          "OPEN",
			ReviewDecision: "REVIEW_REQUIRED",
			Mergeable:      "MERGEABLE",
		},
		BaseSHA:      "base1",
		StatusRollup: rollup,
	}
}

func TestComputeChangeset_FirstRun_Idle(t *testing.T) {
	t.Parallel()
	prev := &State{}
	snap := snapWithRollup("SUCCESS")
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.Empty(), "first-run empty PR should be empty changeset")
	assert.False(t, cs.NeedsPolish())
	assert.False(t, cs.Mergeable, "REVIEW_REQUIRED is not mergeable")
}

func TestComputeChangeset_FirstRun_KnownFailure(t *testing.T) {
	t.Parallel()
	prev := &State{}
	snap := snapWithRollup("FAILURE")
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.CIFailed, "first-run FAILURE rollup should be actionable")
	assert.True(t, cs.NeedsPolish())
}

func TestComputeChangeset_BaseMoved(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "old-base",
	}
	snap := snapWithRollup("SUCCESS")
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.BaseMoved)
	assert.True(t, cs.NeedsPolish())
}

func TestComputeChangeset_CIFailureViaNewRun(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:      time.Now(),
		LastSeenHeadSHA:  "head1",
		LastSeenBaseSHA:  "base1",
		LastSeenCIRunIDs: []int64{100},
	}
	snap := snapWithRollup("PENDING")
	snap.FailedRunIDs = []int64{200}
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.CIFailed)
	assert.Equal(t, []int64{200}, cs.NewFailedRuns)
}

func TestComputeChangeset_NewComments_IgnoresSelf(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:        time.Now(),
		LastSeenHeadSHA:    "head1",
		LastSeenBaseSHA:    "base1",
		LastSeenCommentIDs: []string{"c1"},
	}
	snap := snapWithRollup("SUCCESS")
	snap.Comments = []CommentRef{
		{ID: "c1", Author: "alice"},
		{ID: "c2", Author: "bob"},
		{ID: "c3", Author: "me", IsSelf: true},
	}
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.NewComments)
	assert.Equal(t, []string{"c2"}, cs.NewCommentIDs)
}

func TestComputeChangeset_CommentIDsFromDifferentSourcesDoNotCollide(t *testing.T) {
	t.Parallel()
	// Simulate the real fetchComments output: inline + issue endpoints each
	// namespace their IDs. Two distinct comments must both count as new.
	prev := &State{
		LastCheckAt:        time.Now(),
		LastSeenHeadSHA:    "head1",
		LastSeenBaseSHA:    "base1",
		LastSeenCommentIDs: []string{"inline:42"},
	}
	snap := snapWithRollup("SUCCESS")
	snap.Comments = []CommentRef{
		{ID: "inline:42", Source: "inline", Author: "alice"}, // already seen
		{ID: "issue:42", Source: "issue", Author: "bob"},     // same numeric id, different endpoint
	}
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.NewComments)
	assert.Equal(t, []string{"issue:42"}, cs.NewCommentIDs,
		"the issue-sourced #42 must NOT be silently dropped as a duplicate of inline #42")
}

func TestComputeChangeset_PRClosed_ShortCircuits(t *testing.T) {
	t.Parallel()
	snap := snapWithRollup("SUCCESS")
	snap.PR.State = "MERGED"
	prev := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "old", LastSeenBaseSHA: "older"}
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.PRClosed)
	assert.False(t, cs.NeedsPolish())
	assert.False(t, cs.BaseMoved, "closed PR should short-circuit before computing other diffs")
}

func TestComputeChangeset_Mergeable(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup("SUCCESS")
	snap.PR.ReviewDecision = "APPROVED"
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.Mergeable)
	assert.False(t, cs.NeedsPolish(), "mergeable should not trigger polish")
}

func TestComputeChangeset_EmptyRollupNotMergeable(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup("")
	snap.PR.ReviewDecision = "APPROVED"
	cs := ComputeChangeset(prev, snap)
	assert.False(t, cs.Mergeable,
		"empty status rollup (no checks yet / pending / unknown) must NOT be treated as mergeable")
}

func TestComputeChangeset_GhMergeableConflicting(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "base1",
	}
	snap := snapWithRollup("SUCCESS")
	snap.PR.ReviewDecision = "APPROVED"
	snap.PR.Mergeable = "CONFLICTING"
	cs := ComputeChangeset(prev, snap)
	assert.False(t, cs.Mergeable,
		"CONFLICTING PRs must not be flagged mergeable even with APPROVED + SUCCESS")
}

func TestComputeChangeset_BaseMovedSuppressesMergeable(t *testing.T) {
	t.Parallel()
	prev := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "old-base",
	}
	snap := snapWithRollup("SUCCESS")
	snap.PR.ReviewDecision = "APPROVED"
	cs := ComputeChangeset(prev, snap)
	assert.True(t, cs.BaseMoved)
	assert.False(t, cs.Mergeable, "moving base requires a rebase first")
	assert.True(t, cs.NeedsPolish())
}
