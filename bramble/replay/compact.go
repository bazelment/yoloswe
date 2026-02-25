package replay

import (
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/bramble/session"
)

// CompactLines merges turn-end and token-summary status lines into compact
// single-line summaries. This is provider-agnostic and operates on OutputLines.
func CompactLines(lines []session.OutputLine) []session.OutputLine {
	out := make([]session.OutputLine, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := lines[i]

		if line.Type == session.OutputTypeTurnEnd {
			summary := fmt.Sprintf("T%d $%.4f", line.TurnNumber, line.CostUSD)
			if i+1 < len(lines) && lines[i+1].Type == session.OutputTypeStatus {
				if in, outTokens, ok := parseTokenSummary(lines[i+1].Content); ok {
					summary = fmt.Sprintf("T%d $%.4f in:%d out:%d", line.TurnNumber, line.CostUSD, in, outTokens)
					i++
				}
			}
			out = append(out, session.OutputLine{
				Timestamp: line.Timestamp,
				Type:      session.OutputTypeStatus,
				Content:   summary,
			})
			continue
		}

		if line.Type == session.OutputTypeStatus {
			if in, outTokens, ok := parseTokenSummary(line.Content); ok {
				line.Content = fmt.Sprintf("tok in:%d out:%d", in, outTokens)
			}
		}
		out = append(out, line)
	}
	return out
}

func parseTokenSummary(content string) (int, int, bool) {
	var in, out int
	n, err := fmt.Sscanf(strings.TrimSpace(content), "Tokens: %d input / %d output", &in, &out)
	if err != nil || n != 2 {
		return 0, 0, false
	}
	return in, out, true
}
