// Command sessview renders a session JSONL file for testing the TUI rendering logic.
//
// Deprecated: Use logview instead, which supports both Claude and Codex log formats.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/replay"
	"github.com/bazelment/yoloswe/bramble/session"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <session-file.jsonl> [width] [height]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nRenders a session JSONL file using the TUI rendering widget.\n")
		fmt.Fprintf(os.Stderr, "NOTE: Consider using logview instead, which supports both Claude and Codex formats.\n")
		fmt.Fprintf(os.Stderr, "Default size: 100x30\n")
		os.Exit(1)
	}

	filePath := os.Args[1]
	width := 100
	height := 30

	if len(os.Args) >= 3 {
		fmt.Sscanf(os.Args[2], "%d", &width)
	}
	if len(os.Args) >= 4 {
		fmt.Sscanf(os.Args[3], "%d", &height)
	}

	if err := processSessionFile(filePath, width, height); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func processSessionFile(filePath string, width, height int) error {
	result, err := replay.Parse(filePath)
	if err != nil {
		return fmt.Errorf("failed to parse session: %w", err)
	}

	info := &session.SessionInfo{
		ID:     session.SessionID(filepath.Base(filePath)),
		Type:   session.SessionTypeBuilder,
		Status: result.Status,
		Prompt: result.Prompt,
	}
	if strings.TrimSpace(info.Prompt) == "" {
		info.Prompt = "(prompt not found in session file)"
	}

	fmt.Println(strings.Repeat("=", width))
	fmt.Printf("SESSION RENDERING OUTPUT (%s format, using TUI widget with markdown)\n", result.Format)
	fmt.Printf("Size: %dx%d\n", width, height)
	fmt.Println(strings.Repeat("=", width))

	model := app.NewOutputModelWithMarkdown(info, result.Lines, width)
	model.SetSize(width, height)
	fmt.Println(model.View())

	fmt.Println(strings.Repeat("=", width))

	var toolCount, textCount, errorCount int
	for i := range result.Lines {
		switch result.Lines[i].Type {
		case session.OutputTypeToolStart:
			toolCount++
			if result.Lines[i].ToolState == session.ToolStateError {
				errorCount++
			}
		case session.OutputTypeText:
			textCount++
		case session.OutputTypeError:
			errorCount++
		}
	}
	fmt.Printf("Total: %d output lines | Tools: %d | Text blocks: %d | Errors: %d\n",
		len(result.Lines), toolCount, textCount, errorCount)

	fmt.Println("\n--- RAW OUTPUT LINES (for debugging) ---")
	for i := range result.Lines {
		line := result.Lines[i]
		fmt.Printf("[%3d] %s: %s\n", i, line.Type,
			strings.ReplaceAll(truncateStr(line.Content, 80), "\n", "\\n"))
	}

	return nil
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
