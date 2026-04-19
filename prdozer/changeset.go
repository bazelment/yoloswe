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
// State may be a zero-value (first run): in that case all observed comments
// and failed runs are recorded into State on save, but only BaseMoved /
// CIFailed via rollup signal a real action — we don't want to polish the PR
// just because we've never seen it before.
func ComputeChangeset(prev *State, snap *Snapshot) Changeset {
	cs := Changeset{}

	// PR closed/merged: short-circuit.
	if snap.PR.State == "CLOSED" || snap.PR.State == "MERGED" {
		cs.PRClosed = true
		return cs
	}

	firstRun := prev == nil || prev.LastCheckAt.IsZero()

	// Head/base SHA tracking.
	if !firstRun && prev.LastSeenHeadSHA != "" && prev.LastSeenHeadSHA != snap.PR.HeadRefOid {
		cs.HeadMoved = true
	}
	if !firstRun && prev.LastSeenBaseSHA != "" && snap.BaseSHA != "" && prev.LastSeenBaseSHA != snap.BaseSHA {
		cs.BaseMoved = true
	}

	// Comments: anything not in the previously-seen set, ignoring self-authored.
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

	// CI: new failed runs since last tick.
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
	if !firstRun && (len(cs.NewFailedRuns) > 0 || snap.StatusRollup == "FAILURE") {
		cs.CIFailed = true
	}
	// On the very first run, treat a current FAILURE rollup as actionable so
	// prdozer doesn't silently swallow a known-broken PR.
	if firstRun && snap.StatusRollup == "FAILURE" {
		cs.CIFailed = true
	}

	// Mergeable: APPROVED + green checks. Base unchanged is implied since
	// BaseMoved is its own flag.
	if snap.PR.ReviewDecision == "APPROVED" && (snap.StatusRollup == "SUCCESS" || snap.StatusRollup == "") && !cs.BaseMoved && !cs.CIFailed {
		cs.Mergeable = true
	}

	return cs
}
