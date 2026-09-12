package decide

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// Advisor answers questions the rule engine cannot settle.
//
// It is deliberately narrow: it receives a compact digest of ALREADY VERIFIED
// state and returns typed proposals. It never probes, never acts, and its output
// is re-checked before anything happens. A model's answer is evidence about what
// to do, not authority to do it.
type Advisor interface {
	// Propose answers one question. The implementation is expected to return
	// strict JSON matching Proposal.
	Propose(ctx context.Context, digest string, question string) (Proposal, error)
}

// Proposal is what an Advisor may suggest.
type Proposal struct {
	// Unresolved is a question the model itself could not answer. Saying so is a
	// valid, useful outcome; inventing an answer is not.
	Unresolved string     `json:"unresolved,omitempty"`
	Decisions  []Decision `json:"decisions"`
}

// Invariant is a rule an LLM proposal must not violate.
//
// These exist because a proposal is generated text: plausible, fluent, and
// entirely capable of suggesting a merge on a stale approval or a spawn onto an
// occupied worktree.
//
// WHAT THEY CANNOT CHECK. Check sees only the ledger, so every invariant here is
// a property of RECORDED state: lane exists, status is terminal, round is not
// reused, phase is declared. The deterministic path additionally refuses on
// OBSERVATIONS the ledger does not hold -- a `.done` without a passing
// verify.Verdict, a branch whose content is not on the target, a slot policy
// computed from a live count. A proposal cannot be accepted on the strength of
// these invariants alone; it must also be routed through the same verified
// inputs Plan uses, which is why the advisor is not wired into any command yet.
//
// Vet is therefore a NECESSARY, not a sufficient, condition. Keep it that way or
// state the stronger claim only once Check receives observations too.
type Invariant struct {
	Check func(*state.State, Decision) error
	Name  string
}

// DefaultInvariants are the checks every proposal passes before it is applied.
func DefaultInvariants() []Invariant {
	return []Invariant{
		{
			Name: "lane must exist",
			Check: func(st *state.State, d Decision) error {
				if _, ok := st.Lane(d.Lane); !ok {
					return fmt.Errorf("lane %q is not in the ledger", d.Lane)
				}
				return nil
			},
		},
		{
			Name: "final-phase reap must be rule-generated",
			Check: func(_ *state.State, d Decision) error {
				if d.FinalPhaseComplete && d.Source != SourceRule {
					return fmt.Errorf("only the rule engine may mark a final phase complete")
				}
				return nil
			},
		},
		{
			Name: "phase must be declared",
			Check: func(st *state.State, d Decision) error {
				if d.Phase == "" {
					return nil
				}
				for _, n := range st.Config.PhaseNames() {
					if n == d.Phase {
						return nil
					}
				}
				return fmt.Errorf("phase %q is not declared by the run config %v",
					d.Phase, st.Config.PhaseNames())
			},
		},
		{
			Name: "no reap of a non-terminal lane",
			Check: func(st *state.State, d Decision) error {
				if d.Kind != KindReap {
					return nil
				}
				lane, ok := st.Lane(d.Lane)
				if ok && !lane.Status.Terminal() {
					return fmt.Errorf("lane is %s, not terminal", lane.Status)
				}
				return nil
			},
		},
		{
			Name: "no spawn onto an occupied worktree",
			Check: func(st *state.State, d Decision) error {
				if d.Kind != KindSpawn {
					return nil
				}
				lane, ok := st.Lane(d.Lane)
				if ok && lane.Status == state.StatusRunning {
					return fmt.Errorf("lane is already running; a second agent on one "+
						"worktree is a concurrent-ownership collision (%s)", lane.Worktree)
				}
				return nil
			},
		},
		{
			Name: "rework must not reuse a round",
			Check: func(st *state.State, d Decision) error {
				if d.Kind != KindRework || d.Phase == "" {
					return nil
				}
				lane, ok := st.Lane(d.Lane)
				if !ok {
					return nil
				}
				if d.Round <= lane.MaxRound(d.Phase) {
					return fmt.Errorf("round %d would overwrite an existing attempt "+
						"(max recorded is %d); its session id would be lost",
						d.Round, lane.MaxRound(d.Phase))
				}
				return nil
			},
		},
	}
}

// Vet filters a proposal to the decisions that satisfy every invariant,
// returning the accepted decisions and the reasons for each rejection.
//
// Rejections are returned rather than silently dropped: a model repeatedly
// proposing the same refused action is itself a finding.
//
// "Accepted" means "not refused by a ledger-level invariant". It does NOT mean
// verified -- see the Invariant doc. A caller that applies these without also
// checking the observations Plan checks is trusting the model for the half Vet
// cannot see.
func Vet(st *state.State, p Proposal, invs []Invariant) (accepted []Decision, rejected []string) {
	for _, d := range p.Decisions {
		d.Source = SourceLLM
		var bad string
		for _, inv := range invs {
			if err := inv.Check(st, d); err != nil {
				bad = fmt.Sprintf("%s [%s]: %s — %v", d.Kind, d.Lane, inv.Name, err)
				break
			}
		}
		if bad != "" {
			rejected = append(rejected, bad)
			continue
		}
		accepted = append(accepted, d)
	}
	return accepted, rejected
}

// Digest renders the compact, already-verified view an Advisor is given.
//
// It is deliberately small. The failure it exists to avoid is a 29-hour
// conversation that compacts away its own contract: a fresh digest each tick
// means there is no history to lose.
func Digest(st *state.State, open []Escalation, standing []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "goal: %s\n", st.Config.Goal)
	fmt.Fprintf(&b, "target: %s (base %s)\n", st.Config.Target, st.Config.Base)
	fmt.Fprintf(&b, "phases: %s\n", strings.Join(st.Config.PhaseNames(), " -> "))

	if len(standing) > 0 {
		b.WriteString("\nstanding rules (apply every tick):\n")
		for _, r := range standing {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}

	b.WriteString("\nlanes:\n")
	for _, l := range st.Lanes {
		fmt.Fprintf(&b, "  %-30s %-8s %-14s p=%s", l.ID, l.Status, orDashLLM(l.Phase), l.Priority)
		if l.PR != 0 {
			fmt.Fprintf(&b, " pr=%d checks=%s", l.PR, orDashLLM(l.Checks))
			if l.ApprovalStale() {
				b.WriteString(" approval=STALE")
			}
		}
		if len(l.DependsOn) > 0 {
			fmt.Fprintf(&b, " deps=%v", l.DependsOn)
		}
		b.WriteString("\n")
	}

	if len(open) > 0 {
		b.WriteString("\nopen questions:\n")
		for _, e := range open {
			fmt.Fprintf(&b, "  [%s] %s: %s\n", e.ID, e.Lane, e.Question)
		}
	}
	return b.String()
}

func orDashLLM(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ParseProposal decodes an Advisor's JSON reply.
//
// Models fence JSON in markdown often enough that tolerating it is worth more
// than being strict, but anything that is not decodable JSON is an error rather
// than an empty proposal: silently reading zero decisions would look exactly
// like a model that correctly found nothing to do.
func ParseProposal(raw string) (Proposal, error) {
	s := strings.TrimSpace(raw)
	if fenced := strings.Index(s, "```"); fenced >= 0 {
		rest := s[fenced+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			s = strings.TrimSpace(rest[:end])
		}
	}
	var p Proposal
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return Proposal{}, fmt.Errorf("advisor reply is not valid JSON: %w", err)
	}
	return p, nil
}
