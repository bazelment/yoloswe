package session

import (
	"fmt"
	"strings"
	"time"
)

// GenerateSummary creates a concise text summary of a completed session
// suitable for voice synthesis. The summary includes session type, title,
// status, duration, turn count, cost, and recent output.
func GenerateSummary(info SessionInfo) string {
	var parts []string

	typeLabel := string(info.Type)
	if info.Title != "" {
		parts = append(parts, fmt.Sprintf("%s session %q", typeLabel, info.Title))
	} else {
		parts = append(parts, fmt.Sprintf("%s session", typeLabel))
	}

	switch info.Status {
	case StatusCompleted:
		parts = append(parts, "completed successfully")
	case StatusFailed:
		if info.ErrorMsg != "" {
			parts = append(parts, fmt.Sprintf("failed: %s", info.ErrorMsg))
		} else {
			parts = append(parts, "failed")
		}
	case StatusStopped:
		parts = append(parts, "was stopped")
	default:
		parts = append(parts, fmt.Sprintf("status: %s", string(info.Status)))
	}

	if info.StartedAt != nil {
		end := time.Now()
		if info.CompletedAt != nil {
			end = *info.CompletedAt
		}
		duration := end.Sub(*info.StartedAt).Round(time.Second)
		parts = append(parts, fmt.Sprintf("in %s", formatDuration(duration)))
	}

	if info.Progress.TurnCount > 0 {
		turnWord := "turns"
		if info.Progress.TurnCount == 1 {
			turnWord = "turn"
		}
		if info.Progress.TotalCostUSD > 0 {
			parts = append(parts, fmt.Sprintf("using %d %s at $%.4f",
				info.Progress.TurnCount, turnWord, info.Progress.TotalCostUSD))
		} else {
			parts = append(parts, fmt.Sprintf("using %d %s", info.Progress.TurnCount, turnWord))
		}
	}

	summary := strings.Join(parts, ", ") + "."

	// Append recent output if available.
	recentOutput := info.Progress.RecentOutput
	if len(recentOutput) > 3 {
		recentOutput = recentOutput[len(recentOutput)-3:]
	}
	if len(recentOutput) > 0 {
		summary += " Recent output: " + strings.Join(recentOutput, ". ") + "."
	}

	return summary
}

// formatDuration formats a duration for natural speech.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	}
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	if seconds == 0 {
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	if minutes == 1 {
		return fmt.Sprintf("1 minute %d seconds", seconds)
	}
	return fmt.Sprintf("%d minutes %d seconds", minutes, seconds)
}
