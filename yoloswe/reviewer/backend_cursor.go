package reviewer

import (
	"context"
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/cursor"
)

// cursorBackend wraps the Cursor Agent SDK as a Backend.
// Each RunPrompt call is a one-shot execution (no persistent session).
type cursorBackend struct {
	config Config
}

func newCursorBackend(config Config) *cursorBackend {
	return &cursorBackend{config: config}
}

// Start is a no-op for cursor (one-shot per prompt).
func (b *cursorBackend) Start(_ context.Context) error {
	return nil
}

// Stop is a no-op for cursor (one-shot per prompt).
func (b *cursorBackend) Stop() error {
	return nil
}

func (b *cursorBackend) RunPrompt(ctx context.Context, prompt string, handler EventHandler) (*ReviewResult, error) {
	var opts []cursor.SessionOption
	if b.config.Model != "" {
		opts = append(opts, cursor.WithModel(b.config.Model))
	}
	if b.config.WorkDir != "" {
		opts = append(opts, cursor.WithWorkDir(b.config.WorkDir))
	}
	// Cursor requires --trust for non-interactive use (like --dangerously-skip-permissions for Claude)
	opts = append(opts, cursor.WithTrust())

	events, err := cursor.QueryStream(ctx, prompt, opts...)
	if err != nil {
		return nil, fmt.Errorf("cursor query failed: %w", err)
	}

	result := &ReviewResult{}
	var responseText strings.Builder

	for event := range events {
		switch e := event.(type) {
		case cursor.TextEvent:
			if handler != nil {
				handler.OnText(e.Text)
			}
			responseText.WriteString(e.Text)
		case cursor.ToolStartEvent:
			if handler != nil {
				handler.OnToolStart(e.ID, e.Name, "")
			}
		case cursor.ToolCompleteEvent:
			if handler != nil {
				handler.OnToolEnd(e.ID, 0, 0)
			}
		case cursor.TurnCompleteEvent:
			result.ResponseText = responseText.String()
			result.Success = e.Success
			result.DurationMs = e.DurationMs
			return result, nil
		case cursor.ErrorEvent:
			if handler != nil {
				handler.OnError(e.Error, e.Context)
			}
			return nil, fmt.Errorf("cursor error: %v", e.Error)
		}
	}

	// Channel closed without result
	result.ResponseText = responseText.String()
	if result.ResponseText != "" {
		result.Success = true
		return result, nil
	}
	return nil, fmt.Errorf("cursor session ended without result")
}
