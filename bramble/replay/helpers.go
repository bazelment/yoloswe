package replay

import (
	"fmt"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
)

func parseTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t
	}
	return time.Now()
}

func tokenSummaryContent(usage codex.TokenUsage) string {
	return fmt.Sprintf("Tokens: %d input / %d output", usage.InputTokens, usage.OutputTokens)
}

func approvalKey(callID, command, reason string) string {
	callID = strings.TrimSpace(callID)
	if callID != "" {
		return callID
	}
	command = strings.TrimSpace(command)
	reason = strings.TrimSpace(reason)
	if command == "" && reason == "" {
		return ""
	}
	return command + "\n" + reason
}

func normalizeThreadKey(threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return "_global"
	}
	return threadID
}

func firstTurnText(inputs []codex.UserInput) string {
	for i := range inputs {
		if strings.EqualFold(inputs[i].Type, "text") && strings.TrimSpace(inputs[i].Text) != "" {
			return inputs[i].Text
		}
	}
	return ""
}
