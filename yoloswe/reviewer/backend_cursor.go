package reviewer

import (
	"context"
	"encoding/json"
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

	result := &ReviewResult{}
	var responseText strings.Builder

	for event := range events {
		switch e := event.(type) {
		case cursor.ReadyEvent:
			if handler != nil {
				handler.OnSessionInfo(e.SessionID, e.Model)
			}
		case cursor.TextEvent:
			if handler != nil {
				handler.OnText(e.Text)
			}
			responseText.WriteString(e.Text)
		case cursor.ToolStartEvent:
			if handler != nil {
				displayName := formatCursorToolDisplay(e.Name, e.Input)
				inputStr := serializeToolInput(e.Input)
				handler.OnToolStart(e.ID, displayName, inputStr)
			}
		case cursor.ToolCompleteEvent:
			if handler != nil {
				exitCode := 0
				if e.IsError {
					exitCode = 1
				}
				handler.OnToolEnd(e.ID, exitCode, 0)
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

	// Channel closed without a TurnCompleteEvent — this is abnormal.
	result.ResponseText = responseText.String()
	result.Success = false
	if result.ResponseText != "" {
		return result, fmt.Errorf("cursor session ended unexpectedly (partial response: %d chars)", len(result.ResponseText))
	}
	return nil, fmt.Errorf("cursor session ended without result")
}

// cursorToolNames maps Cursor tool call names to short display names.
var cursorToolNames = map[string]string{
	"readToolCall":       "read",
	"shellToolCall":      "shell",
	"globToolCall":       "glob",
	"grepToolCall":       "grep",
	"editToolCall":       "edit",
	"writeToolCall":      "write",
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

// serializeToolInput serializes tool input to a JSON string for display.
func serializeToolInput(input map[string]interface{}) string {
	if len(input) == 0 {
		return ""
	}
	data, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return string(data)
}
