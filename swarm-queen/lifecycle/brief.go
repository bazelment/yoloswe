package lifecycle

import (
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// BriefContext is everything the envelope needs.
type BriefContext struct {
	Lane    *state.Lane
	RunDir  string
	Phase   string
	Goal    string
	Target  string
	Mission string
	// Standing are run-level rules re-applied every tick.
	Standing []string
	// Findings are review findings returning the lane to this phase.
	Findings []string
	// Reserved names actions only the orchestrator may take.
	Reserved []string
	Round    int
}

// RenderBrief builds the brief handed to a phase.
//
// The MISSION is authored (by an operator or an advisor); the ENVELOPE is
// templated, because the parts that get forgotten are always the same parts:
// literal report paths, ownership boundaries, and the exit gate. Real runs lost
// instructions by leaving them implicit, so they are emitted mechanically here.
//
// Deliberately terse. Generic coding, testing and review advice is NOT included:
// the agents have good judgement, and padding the brief buries the constraints
// that actually bind.
func RenderBrief(c BriefContext) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are the %q lane of a swarm, phase %s (round %d).\n\n",
		c.Lane.ID, c.Phase, c.Round)

	if c.Goal != "" {
		fmt.Fprintf(&b, "Run goal: %s\n", c.Goal)
	}
	if c.Target != "" {
		fmt.Fprintf(&b, "Integrating into: %s\n", c.Target)
	}
	b.WriteString("\n## Mission\n\n")
	b.WriteString(strings.TrimSpace(c.Mission))
	b.WriteString("\n")

	if len(c.Findings) > 0 {
		b.WriteString("\n## Why this returned to you\n\n")
		for _, f := range c.Findings {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}

	b.WriteString("\n## Ownership\n\n")
	fmt.Fprintf(&b, "- Your branch is `%s`; your worktree is `%s`.\n",
		c.Lane.Branch, orNoneBrief(c.Lane.Worktree))
	b.WriteString("- Do not edit files owned by another lane, and do not mutate shared live state.\n")
	if len(c.Reserved) > 0 {
		b.WriteString("- Reserved for the orchestrator, do not do these yourself:\n")
		for _, r := range c.Reserved {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}

	if len(c.Standing) > 0 {
		b.WriteString("\n## Standing rules\n\n")
		for _, r := range c.Standing {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}

	// Literal paths: a child environment does not point back at the run dir, so
	// a relative path or an env var here reaches nothing.
	b.WriteString("\n## Reporting\n\n")
	fmt.Fprintf(&b, "- Write your report to `%s`.\n",
		ReportPath(c.RunDir, c.Lane.ID, c.Phase, c.Round))
	fmt.Fprintf(&b, "- Commit your work, then touch `%s` LAST.\n",
		DonePath(c.RunDir, c.Lane.ID, c.Phase, c.Round))
	b.WriteString("- A `.done` file with no commits is treated as an incomplete phase, not a finished one.\n")
	if strings.Contains(c.Phase, "review") {
		fmt.Fprintf(&b, "- If this must return for rework, touch `%s` instead and record the findings in your report.\n",
			NeedsSWEPath(c.RunDir, c.Lane.ID, c.Phase, c.Round))
	}
	b.WriteString("- Never weaken an assertion or delete a test to make a gate green. " +
		"A supported finding that the fix belongs elsewhere is a valid outcome.\n")
	b.WriteString("- If you are blocked on a credential or permission, stop and report it. Do not work around it.\n")

	return b.String()
}

func orNoneBrief(s string) string {
	if s == "" {
		return "(to be created)"
	}
	return s
}
