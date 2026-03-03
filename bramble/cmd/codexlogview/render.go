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
		return "", fmt.Errorf("failed to parse log: %w", err)
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
	return model.View().Content, nil
}
