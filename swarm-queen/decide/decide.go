// Package decide turns verified state into actions.
//
// The rule engine runs FIRST and settles everything it can. An LLM is consulted
// only for genuinely open questions -- which gap to staff, whether a review
// finding is major, how to word a brief -- and its answers are proposals, not
// commands: every one is re-checked against the same invariants before it is
// applied.
//
// Decisions are typed rather than prose so they can be logged, replayed, and
// refused. A decision nobody can audit is how a swarm ends up with a ledger that
// describes work it never did.
package decide

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bazelment/yoloswe/swarm-queen/state"
	"github.com/bazelment/yoloswe/swarm-queen/verify"
)

// Kind is what a decision does.
type Kind string

const (
	// KindSpawn staffs a dependency-ready lane.
	KindSpawn Kind = "spawn"
	// KindAdvance moves a lane to its next phase after its claim verified.
	KindAdvance Kind = "advance"
	// KindRework returns a lane to an earlier phase, incrementing its round.
	KindRework Kind = "rework"
	// KindReap tears a terminal lane down.
	KindReap Kind = "reap"
	// KindEscalate hands a question to a human. Escalation is a first-class
	// outcome, not a failure: improvising past a question nobody answered is
	// how a run acquires claims it cannot support.
	KindEscalate Kind = "escalate"
	// KindHold takes no action and records why.
	KindHold Kind = "hold"
)

// Source records who decided, so a bad decision can be traced to its origin.
type Source string

const (
	// SourceRule is the deterministic engine.
	SourceRule Source = "rule"
	// SourceLLM is a model proposal that passed invariant checks.
	SourceLLM Source = "llm"
	// SourceHuman is an operator nudge.
	SourceHuman Source = "human"
)

// Decision is one action, with the evidence that produced it.
type Decision struct {
	Lane   string `json:"lane"`
	Kind   Kind   `json:"kind"`
	Source Source `json:"source"`
	Phase  string `json:"phase,omitempty"`
	Reason string `json:"reason"`
	// Evidence is what was measured, so `--explain` can show the basis.
	Evidence []string `json:"evidence,omitempty"`
	Round    int      `json:"round,omitempty"`
}

func (d Decision) String() string {
	s := fmt.Sprintf("%-8s %-28s %s", d.Kind, d.Lane, d.Reason)
	if d.Phase != "" {
		s = fmt.Sprintf("%-8s %-28s [%s r%d] %s", d.Kind, d.Lane, d.Phase, d.Round, d.Reason)
	}
	return s
}

// Inputs is everything the engine needs. All of it is already verified: decide
// never probes, so it is pure and fully testable.
type Inputs struct {
	State         *state.State
	Verdicts      map[string]verify.Verdict
	SlotExempt    map[string]bool
	Signals       []LaneSignal
	Drift         []verify.Finding
	MaxConcurrent int
}

// LaneSignal is a claim a lane made.
type LaneSignal struct {
	Lane     string
	Phase    string
	Round    int
	NeedsSWE bool
}

// Plan runs the rule engine and returns the decisions it can justify, plus the
// questions it cannot settle.
//
// Anything ambiguous becomes an escalation rather than a guess. That is the
// deliberate bias: a swarm that stops and asks is recoverable, one that
// improvises is not.
func Plan(in Inputs) []Decision {
	var out []Decision
	if in.State == nil {
		return out
	}

	// Drift first: a lane whose ledger row disagrees with reality must be
	// reconciled before anything is dispatched on top of it.
	out = append(out, driftDecisions(in)...)

	// Signals: advance or rework lanes whose claims verified.
	out = append(out, signalDecisions(in)...)

	// Refill: staff dependency-ready lanes up to the concurrency cap.
	out = append(out, spawnDecisions(in, out)...)

	return out
}

func driftDecisions(in Inputs) []Decision {
	var out []Decision
	for _, f := range in.Drift {
		if f.Severity != verify.SeverityBlock {
			continue
		}
		out = append(out, Decision{
			Lane: f.Lane, Kind: KindEscalate, Source: SourceRule,
			Reason:   f.Action,
			Evidence: []string{f.Claim + ": " + f.Evidence},
		})
	}
	return out
}

func signalDecisions(in Inputs) []Decision {
	var out []Decision
	for _, sig := range in.Signals {
		lane, ok := in.State.Lane(sig.Lane)
		if !ok {
			out = append(out, Decision{
				Lane: sig.Lane, Kind: KindEscalate, Source: SourceRule,
				Reason:   "signal names a lane that is not in the ledger",
				Evidence: []string{fmt.Sprintf("signal %s.%s", sig.Lane, sig.Phase)},
			})
			continue
		}

		// A review rejection sends the lane back to the first phase with an
		// incremented round, so the previous attempt's session is preserved
		// rather than overwritten.
		if sig.NeedsSWE {
			first := firstPhase(in.State)
			out = append(out, Decision{
				Lane: lane.ID, Kind: KindRework, Source: SourceRule,
				Phase: first, Round: lane.MaxRound(first) + 1,
				Reason:   "review returned the lane for rework",
				Evidence: []string{fmt.Sprintf("%s.%s.needs-swe", sig.Lane, sig.Phase)},
			})
			continue
		}

		// A .done is a CLAIM. It advances only if verification agreed.
		v, verified := in.Verdicts[lane.ID]
		switch {
		case !verified:
			out = append(out, Decision{
				Lane: lane.ID, Kind: KindHold, Source: SourceRule,
				Reason:   "claim not yet verified",
				Evidence: []string{fmt.Sprintf("%s.%s.done", sig.Lane, sig.Phase)},
			})
		case v.Blocked():
			out = append(out, Decision{
				Lane: lane.ID, Kind: KindEscalate, Source: SourceRule,
				Reason:   "completion claim refused by verification",
				Evidence: findingStrings(v.Findings),
			})
		default:
			next, has := in.State.Config.NextPhase(lane.Phase)
			if !has {
				out = append(out, Decision{
					Lane: lane.ID, Kind: KindReap, Source: SourceRule,
					Reason:   "final phase verified complete",
					Evidence: findingStrings(v.Findings),
				})
				continue
			}
			out = append(out, Decision{
				Lane: lane.ID, Kind: KindAdvance, Source: SourceRule,
				Phase: next, Round: 1,
				Reason:   "phase verified complete",
				Evidence: findingStrings(v.Findings),
			})
		}
	}
	return out
}

// spawnDecisions refills free slots from the dependency-ready queue.
//
// A free slot with ready work and no spawn is the stall this harness exists to
// prevent: "idle, available" repeated tick after tick is the stall signal, not a
// status.
func spawnDecisions(in Inputs, already []Decision) []Decision {
	if in.MaxConcurrent <= 0 {
		return nil
	}
	occupied := 0
	for _, l := range in.State.Lanes {
		if l.Status == state.StatusRunning && !in.SlotExempt[l.Phase] {
			occupied++
		}
	}
	// Decisions made earlier this tick change the count.
	for _, d := range already {
		switch d.Kind {
		case KindReap:
			occupied--
		case KindRework, KindAdvance:
			// Still occupying its slot.
		}
	}

	free := in.MaxConcurrent - occupied
	if free <= 0 {
		return nil
	}

	var out []Decision
	for _, lane := range in.State.Ready() {
		if free <= 0 {
			break
		}
		first := firstPhase(in.State)
		out = append(out, Decision{
			Lane: lane.ID, Kind: KindSpawn, Source: SourceRule,
			Phase: first, Round: 1,
			Reason: fmt.Sprintf("dependency-ready %s lane, slot available", lane.Priority),
		})
		free--
	}
	return out
}

func firstPhase(st *state.State) string {
	if names := st.Config.PhaseNames(); len(names) > 0 {
		return names[0]
	}
	return ""
}

func findingStrings(fs []verify.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Evidence)
	}
	sort.Strings(out)
	return out
}

// Summarise counts decisions by kind, for a terse tick report.
func Summarise(ds []Decision) string {
	counts := map[Kind]int{}
	for _, d := range ds {
		counts[d.Kind]++
	}
	var parts []string
	for _, k := range []Kind{KindSpawn, KindAdvance, KindRework, KindReap, KindEscalate, KindHold} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
		}
	}
	if len(parts) == 0 {
		return "no decisions"
	}
	return strings.Join(parts, " · ")
}
