package prdozer

// Changeset describes what's different between a stored State and the latest
// Snapshot. The watcher uses these flags to decide whether to invoke the
// /pr-polish agent.
type Changeset struct {
	NewCommentIDs []string // human/bot comment IDs not in State.LastSeenCommentIDs
	NewFailedRuns []int64  // failed CI run IDs not in State.LastSeenCIRunIDs
	BaseMoved     bool     // base SHA differs from State.LastSeenBaseSHA
	HeadMoved     bool     // head SHA differs from State.LastSeenHeadSHA
	CIFailed      bool     // status rollup says FAILURE OR there are new failed runs
	NewComments   bool     // at least one new non-self comment
	Mergeable     bool     // PR is APPROVED + checks SUCCESS + base unchanged
	PRClosed      bool     // PR state is CLOSED or MERGED
}

// Empty reports whether nothing actionable changed (no new failed CI, no new
// comments, base hasn't moved). The watcher uses this to decide "nothing to
// do, sleep until next tick".
func (c Changeset) Empty() bool {
	return !c.BaseMoved && !c.CIFailed && !c.NewComments && !c.PRClosed
}

// NeedsPolish reports whether prdozer should invoke the polish agent.
// Mergeable PRs do NOT need polish even if other flags are true (e.g. a fresh
// commit moved HEAD but checks are green).
func (c Changeset) NeedsPolish() bool {
	if c.PRClosed || c.Mergeable {
		return false
	}
	return c.BaseMoved || c.CIFailed || c.NewComments
}

// ComputeChangeset diffs the snapshot against the previously persisted State.
// On first run (prev is zero), observed comments/runs are recorded but don't
// trigger polish — we only react to a current FAILURE rollup, so prdozer
// doesn't silently swallow a known-broken PR.
func ComputeChangeset(prev *State, snap *Snapshot) Changeset {
	cs := Changeset{}

	if snap.PR.State == "CLOSED" || snap.PR.State == "MERGED" {
		cs.PRClosed = true
		return cs
	}

	firstRun := prev == nil || prev.LastCheckAt.IsZero()

	if !firstRun && prev.LastSeenHeadSHA != "" && prev.LastSeenHeadSHA != snap.PR.HeadRefOid {
		cs.HeadMoved = true
	}
	if !firstRun && prev.LastSeenBaseSHA != "" && snap.BaseSHA != "" && prev.LastSeenBaseSHA != snap.BaseSHA {
		cs.BaseMoved = true
	}

	seenComments := make(map[string]bool)
	if prev != nil {
		for _, id := range prev.LastSeenCommentIDs {
			seenComments[id] = true
		}
	}
	for _, c := range snap.Comments {
		if c.IsSelf {
			continue
		}
		if !seenComments[c.ID] {
			cs.NewCommentIDs = append(cs.NewCommentIDs, c.ID)
		}
	}
	if !firstRun && len(cs.NewCommentIDs) > 0 {
		cs.NewComments = true
	}

	seenRuns := make(map[int64]bool)
	if prev != nil {
		for _, id := range prev.LastSeenCIRunIDs {
			seenRuns[id] = true
		}
	}
	for _, id := range snap.FailedRunIDs {
		if !seenRuns[id] {
			cs.NewFailedRuns = append(cs.NewFailedRuns, id)
		}
	}
	if snap.StatusRollup == StatusFailure || (!firstRun && len(cs.NewFailedRuns) > 0) {
		cs.CIFailed = true
	}

	// Require an explicit SUCCESS rollup AND an explicit MERGEABLE verdict from
	// gh. An empty rollup (no checks yet, pending, or unknown) must NOT count as
	// mergeable — otherwise auto-merge can fire before CI has even started.
	if snap.PR.ReviewDecision == "APPROVED" &&
		snap.StatusRollup == StatusSuccess &&
		snap.PR.Mergeable == "MERGEABLE" &&
		!cs.BaseMoved && !cs.CIFailed {
		cs.Mergeable = true
	}

	return cs
}
