package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// Split names a deterministic half of a dataset's moments. Tuning iterates on
// SplitTune only; SplitHoldout is run once at the end so it cannot be fit to.
type Split string

const (
	SplitTune    Split = "tune"
	SplitHoldout Split = "holdout"
)

func momentHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// SplitMomentIDs partitions moment IDs by SHA-256 of the ID: the lower half
// (rounded up) of the hash order is tune, the rest holdout. The result depends
// only on the set of IDs, not on dataset order, so adding or reordering
// unrelated fields never moves a moment between halves.
func SplitMomentIDs(ids []string) (tune, holdout []string) {
	sorted := slices.Clone(ids)
	slices.SortFunc(sorted, func(a, b string) int { return strings.Compare(momentHash(a), momentHash(b)) })
	cut := (len(sorted) + 1) / 2
	tune = sorted[:cut]
	holdout = sorted[cut:]
	slices.Sort(tune)
	slices.Sort(holdout)
	return tune, holdout
}

// Subset returns a copy of ds holding only the moments of the given split and
// no Concierge questions (they are not part of the Dispatcher fix surface and
// are reported on the full set). SplitAll-style use is just ds itself.
func (ds *Dataset) Subset(split Split) (*Dataset, error) {
	ids := make([]string, len(ds.Moments))
	for i := range ds.Moments {
		ids[i] = ds.Moments[i].ID
	}
	tune, holdout := SplitMomentIDs(ids)
	var keep []string
	switch split {
	case SplitTune:
		keep = tune
	case SplitHoldout:
		keep = holdout
	default:
		return nil, fmt.Errorf("orch/replay: unknown split %q (want %q or %q)", split, SplitTune, SplitHoldout)
	}
	return ds.SelectMoments(keep)
}

// SelectMoments returns a copy of ds restricted to the named moment IDs (in
// dataset order) and without Concierge questions.
func (ds *Dataset) SelectMoments(ids []string) (*Dataset, error) {
	out := *ds
	out.Moments = nil
	out.Questions = nil
	for i := range ds.Moments {
		if slices.Contains(ids, ds.Moments[i].ID) {
			out.Moments = append(out.Moments, ds.Moments[i])
		}
	}
	for _, id := range ids {
		if !slices.ContainsFunc(out.Moments, func(m JudgmentMoment) bool { return m.ID == id }) {
			return nil, fmt.Errorf("orch/replay: unknown moment id %q", id)
		}
	}
	return &out, nil
}
