// Package state holds the swarm-queen run store: the durable, on-disk record of
// what a swarm is doing.
//
// The design rule that shapes every type here: a field is either OBSERVED (written
// only by reconcile, from git/gh/tmux/bramble) or DECLARED (written by the contract
// or an operator). Nothing is ever "asserted" by an agent and then trusted.
//
// Real runs left ledgers that described a swarm which no longer existed: lanes
// marked running over merged PRs, 3 of 3 live sessions unregistered, 9 of 12
// worktree paths dangling. Those are not bookkeeping slips; they happen because
// the old ledger had no way to express "this is a claim" separately from "this is
// verified", so a stale write looked exactly like a fresh one. Hence Evidence.
package state

import "strings"

// Status is where a lane is in its life. It is deliberately separate from Phase:
// `running` covers working, wedged and awaiting-input, so the two axes must not
// be collapsed into one enum.
type Status string

const (
	StatusPlanned Status = "planned"
	StatusRunning Status = "running"
	StatusBlocked Status = "blocked"
	StatusFailed  Status = "failed"
	StatusDone    Status = "done"
)

// Terminal reports whether a lane needs no further work. `swarm-queen status`
// refuses to call a run complete while any lane is non-terminal — one real run
// was shut down with 6 of 21 lanes non-terminal, 3 of them p1 lanes never staffed.
func (s Status) Terminal() bool { return s == StatusDone || s == StatusFailed }

// Priority orders the dispatch queue.
type Priority string

const (
	P0 Priority = "p0" // blocks the terminal proof, integration, or several lanes
	P1 Priority = "p1" // removes major uncertainty or builds the proof path
	P2 Priority = "p2" // remaining ready work
)

// Rank orders priorities for dispatch; lower sorts first.
func (p Priority) Rank() int {
	switch p {
	case P0:
		return 0
	case P1:
		return 1
	default:
		return 2
	}
}

// MutatingPhase reports whether a phase is expected to change the branch.
//
// One predicate, because it answers two questions that must not be allowed to
// disagree. A read-only phase legitimately commits nothing, so the empty-branch
// refusal must not apply to it; and a read-only phase is not the work a
// concurrency cap exists to limit, so it must not consume a slot. Held as two
// lists in two packages they drifted immediately: the verifier knew about
// `verify` and a `review` PREFIX, while the slot accounting knew only `report`
// and `gaps`.
//
// The prefix was itself wrong. Every review phase this system actually runs is
// named `local-review` or `github-review` (see reconcile/signal_test.go and
// state/rounds_test.go), and neither has `review` as a prefix -- so every real
// review lane was classified as mutating and its `.done` refused as an empty
// branch for committing nothing, which is exactly what a review is supposed to
// do. Matching a dash-delimited segment is what the naming convention actually
// is.
//
// Lives in state because decide and the CLI both import it and neither imports
// the other.
func MutatingPhase(phase string) bool {
	switch phase {
	case "gaps", "replay", "report", "verify", "":
		return false
	}
	// A segment scan also covers the round suffix convention (local-review-r2)
	// without needing to strip it first.
	for _, part := range strings.Split(phase, "-") {
		if part == "review" {
			return false
		}
	}
	return true
}
