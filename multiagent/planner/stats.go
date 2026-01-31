package planner

import (
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// SessionStats tracks cumulative token usage and cost for a session phase.
// This pattern is adopted from yoloswe/planner/planner.go for phase-aware tracking.
type SessionStats struct {
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	CostUSD         float64
	TurnCount       int
}

// Add accumulates stats from a turn result's usage.
func (s *SessionStats) Add(usage claude.TurnUsage) {
	s.InputTokens += usage.InputTokens
	s.OutputTokens += usage.OutputTokens
	s.CacheReadTokens += usage.CacheReadTokens
	s.CostUSD += usage.CostUSD
	s.TurnCount++
}

// AddStats merges another SessionStats into this one.
func (s *SessionStats) AddStats(other SessionStats) {
	s.InputTokens += other.InputTokens
	s.OutputTokens += other.OutputTokens
	s.CacheReadTokens += other.CacheReadTokens
	s.CostUSD += other.CostUSD
	s.TurnCount += other.TurnCount
}

// TotalTokens returns the total number of tokens (input + output).
func (s *SessionStats) TotalTokens() int {
	return s.InputTokens + s.OutputTokens
}

// Reset clears all stats to zero.
func (s *SessionStats) Reset() {
	s.InputTokens = 0
	s.OutputTokens = 0
	s.CacheReadTokens = 0
	s.CostUSD = 0
	s.TurnCount = 0
}

// PhaseStats tracks stats per execution phase for detailed reporting.
type PhaseStats struct {
	Planning  SessionStats
	Designing SessionStats
	Building  SessionStats
	Reviewing SessionStats
}

// Total returns aggregated stats across all phases.
func (ps *PhaseStats) Total() SessionStats {
	var total SessionStats
	total.AddStats(ps.Planning)
	total.AddStats(ps.Designing)
	total.AddStats(ps.Building)
	total.AddStats(ps.Reviewing)
	return total
}

// TotalCostUSD returns the total cost across all phases.
func (ps *PhaseStats) TotalCostUSD() float64 {
	return ps.Planning.CostUSD + ps.Designing.CostUSD +
		ps.Building.CostUSD + ps.Reviewing.CostUSD
}

// TotalTurns returns the total number of turns across all phases.
func (ps *PhaseStats) TotalTurns() int {
	return ps.Planning.TurnCount + ps.Designing.TurnCount +
		ps.Building.TurnCount + ps.Reviewing.TurnCount
}

// Reset clears all phase stats.
func (ps *PhaseStats) Reset() {
	ps.Planning.Reset()
	ps.Designing.Reset()
	ps.Building.Reset()
	ps.Reviewing.Reset()
}

// AddForPhase adds usage stats to the appropriate phase.
func (ps *PhaseStats) AddForPhase(state PlannerState, usage claude.TurnUsage) {
	switch state {
	case StatePlanning, StateWaitingForInput:
		ps.Planning.Add(usage)
	case StateDesigning:
		ps.Designing.Add(usage)
	case StateBuilding:
		ps.Building.Add(usage)
	case StateReviewing:
		ps.Reviewing.Add(usage)
	default:
		// For idle/completed/failed states, attribute to planning
		ps.Planning.Add(usage)
	}
}
