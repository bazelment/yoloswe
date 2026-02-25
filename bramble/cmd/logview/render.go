package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/replay"
	"github.com/bazelment/yoloswe/bramble/session"
)

func renderLog(path string, cfg cliConfig) (string, error) {
	result, err := replay.Parse(path)
	if err != nil {
		return "", err
	}
	if cfg.compact {
		result.Lines = replay.CompactLines(result.Lines)
	}

	info := &session.SessionInfo{
		ID:     session.SessionID(filepath.Base(path)),
		Type:   session.SessionTypeBuilder,
		Status: result.Status,
		Prompt: result.Prompt,
	}
	if info.Status == "" {
		info.Status = session.StatusCompleted
	}
	if strings.TrimSpace(info.Prompt) == "" {
		info.Prompt = "(unknown prompt)"
	}

	model := app.NewOutputModel(info, result.Lines)
	if cfg.enableMarkdown {
		model.EnableMarkdown()
	}
	model.SetSize(cfg.width, cfg.height)

	var b strings.Builder
	b.WriteString(model.View())

	if cfg.debug {
		b.WriteString("\n--- RAW OUTPUT LINES ---\n")
		for i := range result.Lines {
			b.WriteString(fmt.Sprintf("[%3d] %s: %s\n", i, result.Lines[i].Type, truncateDebug(result.Lines[i].Content, 80)))
		}
	}

	return b.String(), nil
}

func truncateDebug(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
