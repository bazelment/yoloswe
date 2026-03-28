package prompt

import (
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/symphony/model"
)

// BuildContinuationGuidance generates a focused continuation message for an
// existing coding-agent thread. Per Spec Section 7.1, continuation turns do
// not resend the original task prompt; they provide only turn-count context
// and any issue metadata the agent needs to keep working.
//
// When maxTurns is 0, the turn limit line is omitted.
func BuildContinuationGuidance(issue model.Issue, turnNumber, maxTurns int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Continue working on %s: %s.", issue.Identifier, issue.Title)
	b.WriteString("\n\n")

	if maxTurns > 0 {
		fmt.Fprintf(&b, "This is turn %d of %d.", turnNumber, maxTurns)
	} else {
		fmt.Fprintf(&b, "This is turn %d.", turnNumber)
	}

	if issue.State != "" {
		fmt.Fprintf(&b, " The issue is currently in state %q.", issue.State)
	}

	return b.String()
}
