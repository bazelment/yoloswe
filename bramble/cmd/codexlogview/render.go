package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/yoloswe/sessionplayer"
)

func renderLog(path string, cfg cliConfig) (string, error) {
	format, err := sessionplayer.DetectFormat(path)
	if err != nil {
		return "", fmt.Errorf("failed to detect log format: %w", err)
	}
	if format != sessionplayer.FormatCodex {
		return "", fmt.Errorf("expected codex log format, got %q", format)
	}

	replay, err := parseCodexProtocolLog(path)
	if err != nil {
		return "", fmt.Errorf("failed to parse log: %w", err)
	}
	if cfg.compact {
		replay.lines = compactReplayLines(replay.lines)
	}

	info := &session.SessionInfo{
		ID:     session.SessionID(filepath.Base(path)),
		Type:   session.SessionTypeBuilder,
		Status: replay.status,
		Prompt: replay.prompt,
	}
	if info.Status == "" {
		info.Status = session.StatusCompleted
	}
	if strings.TrimSpace(info.Prompt) == "" {
		info.Prompt = "(unknown prompt)"
	}

	model := app.NewOutputModel(info, replay.lines)
	if cfg.enableMarkdown {
		model.EnableMarkdown()
	}
	model.SetSize(cfg.width, cfg.height)
	return model.View(), nil
}

func compactReplayLines(lines []session.OutputLine) []session.OutputLine {
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
