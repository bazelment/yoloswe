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
