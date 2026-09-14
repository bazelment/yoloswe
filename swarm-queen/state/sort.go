package state

import "sort"

// stableSortByPriority orders lanes p0, p1, p2, preserving insertion order
// within a priority so dispatch is deterministic across ticks.
func stableSortByPriority(lanes []*Lane) {
	sort.SliceStable(lanes, func(i, j int) bool {
		return lanes[i].Priority.Rank() < lanes[j].Priority.Rank()
	})
}
