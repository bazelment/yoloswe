package reviewer

import (
	"context"
	"fmt"
	"path/filepath"
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

	// Wrap handler to format cursor-specific tool display names and
	// intercept ReadyEvent (which isn't part of agentstream).
	adapter := &cursorEventAdapter{handler: handler, events: events}
	bridged, err := bridgeStreamEvents(ctx, adapter.filtered(), handler, "")
	if err != nil {
		return nil, fmt.Errorf("cursor: %w", err)
	}

	// Check for turn-level errors (TurnCompleteEvent.Error).
	if tc, ok := bridged.turnEvent.(cursor.TurnCompleteEvent); ok && tc.Error != nil {
		if handler != nil {
			handler.OnError(tc.Error, "turn_complete")
		}
		return nil, fmt.Errorf("cursor turn failed: %w", tc.Error)
	}

	return &ReviewResult{
		ResponseText: bridged.responseText,
		Success:      bridged.success,
		DurationMs:   bridged.durationMs,
	}, nil
}

// cursorEventAdapter filters cursor events, handling ReadyEvent out-of-band
// and formatting tool names before they reach the bridge.
type cursorEventAdapter struct {
	handler EventHandler
	events  <-chan cursor.Event
}

// filtered returns a channel that re-emits cursor events, handling ReadyEvent
// separately and wrapping ToolStartEvent with formatted display names.
func (a *cursorEventAdapter) filtered() <-chan cursor.Event {
	out := make(chan cursor.Event)
	go func() {
		defer close(out)
		for ev := range a.events {
			switch e := ev.(type) {
			case cursor.ReadyEvent:
				// ReadyEvent doesn't implement agentstream.Event; handle directly.
				if a.handler != nil {
					a.handler.OnSessionInfo(e.SessionID, e.Model)
				}
			case cursor.ToolStartEvent:
				// Rewrite tool name for display before the bridge sees it.
				e.Name = formatCursorToolDisplay(e.Name, e.Input)
				out <- e
			default:
				out <- ev
			}
		}
	}()
	return out
}

// cursorToolNames maps Cursor tool call names to short display names.
var cursorToolNames = map[string]string{
	"readToolCall":        "read",
	"shellToolCall":       "shell",
	"globToolCall":        "glob",
	"grepToolCall":        "grep",
	"editToolCall":        "edit",
	"writeToolCall":       "write",
	"updateTodosToolCall": "updateTodos",
}

// cursorToolArgKeys maps Cursor tool call names to the most informative arg key.
var cursorToolArgKeys = map[string]string{
	"readToolCall":  "path",
	"shellToolCall": "command",
	"globToolCall":  "globPattern",
	"grepToolCall":  "pattern",
	"editToolCall":  "path",
	"writeToolCall": "path",
}

// formatCursorToolDisplay formats a cursor tool call into a human-readable display string.
// e.g. "readToolCall" + {path: "/foo/bar/baz.go"} → "read .../bar/baz.go"
func formatCursorToolDisplay(name string, input map[string]interface{}) string {
	displayName, ok := cursorToolNames[name]
	if !ok {
		// Strip "ToolCall" suffix and lowercase for unknown tools
		displayName = name
		if strings.HasSuffix(displayName, "ToolCall") {
			displayName = strings.TrimSuffix(displayName, "ToolCall")
			if len(displayName) > 0 {
				displayName = strings.ToLower(displayName[:1]) + displayName[1:]
			}
		}
	}

	argKey := cursorToolArgKeys[name]
	if argKey == "" || input == nil {
		return displayName
	}

	argVal, ok := input[argKey]
	if !ok {
		return displayName
	}

	argStr, ok := argVal.(string)
	if !ok || argStr == "" {
		return displayName
	}

	// Format the argument value
	switch argKey {
	case "path":
		argStr = shortPath(argStr)
	case "command":
		if len(argStr) > 50 {
			argStr = argStr[:47] + "..."
		}
	}

	if name == "shellToolCall" {
		return displayName + ": " + argStr
	}
	return displayName + " " + argStr
}

// shortPath returns the last 2 path components prefixed with ".../"
// e.g. "/home/user/project/pkg/file.go" → ".../pkg/file.go"
func shortPath(p string) string {
	dir, file := filepath.Split(p)
	if dir == "" {
		return file
	}
	parent := filepath.Base(filepath.Clean(dir))
	return ".../" + parent + "/" + file
}

