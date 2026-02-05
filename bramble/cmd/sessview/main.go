// Command sessview renders a session JSONL file for testing the TUI rendering logic.
// It uses the same OutputModel as the TUI to ensure consistent rendering.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/session"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <session-file.jsonl> [width] [height]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nRenders a session JSONL file using the TUI rendering widget.\n")
		fmt.Fprintf(os.Stderr, "Default size: 100x30\n")
		os.Exit(1)
	}

	filePath := os.Args[1]
	width := 100
	height := 30

	// Parse optional width/height
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

// SessionMessage represents a single message from the session JSONL file.
type SessionMessage struct {
	Timestamp string          `json:"timestamp"`
	Direction string          `json:"direction"`
	Message   json.RawMessage `json:"message"`
}

// StreamEvent represents a stream event from Claude.
type StreamEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	Event     json.RawMessage `json:"event"`
	Message   json.RawMessage `json:"message"`
}

// ContentBlockStart represents a content_block_start event.
type ContentBlockStart struct {
	ContentBlock ContentBlock `json:"content_block"`
	Type         string       `json:"type"`
	Index        int          `json:"index"`
}

// ContentBlock represents a content block.
type ContentBlock struct {
	Input any    `json:"input"`
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Text  string `json:"text"`
}

// ContentBlockDelta represents a content_block_delta event.
type ContentBlockDelta struct {
	Delta DeltaBlock `json:"delta"`
	Type  string     `json:"type"`
	Index int        `json:"index"`
}

// DeltaBlock represents the delta in a content_block_delta event.
type DeltaBlock struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
}

// ToolResultMessage represents a tool result message.
type ToolResultMessage struct {
	Role    string       `json:"role"`
	Content []ToolResult `json:"content"`
}

// ToolResult represents a single tool result.
type ToolResult struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

// UserMessage represents an initial user message.
type UserMessage struct {
	Type    string `json:"type"`
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

func processSessionFile(filePath string, width, height int) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	var outputLines []session.OutputLine
	toolStartTimes := make(map[string]time.Time)
	currentToolID := ""
	currentToolName := ""
	var textBuffer strings.Builder
	var userPrompt string

	scanner := bufio.NewScanner(file)
	// Increase buffer size for large lines
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		var msg SessionMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		// Check for initial user message (sent direction)
		if msg.Direction == "sent" && userPrompt == "" {
			var userMsg UserMessage
			if err := json.Unmarshal(msg.Message, &userMsg); err == nil {
				if userMsg.Type == "user" {
					userPrompt = userMsg.Message.Content
				}
			}
			continue
		}

		// Only process received messages
		if msg.Direction != "received" {
			continue
		}

		var streamEvent StreamEvent
		if err := json.Unmarshal(msg.Message, &streamEvent); err != nil {
			continue
		}

		switch streamEvent.Type {
		case "stream_event":
			processStreamEvent(streamEvent.Event, &outputLines, toolStartTimes,
				&currentToolID, &currentToolName, &textBuffer)

		case "user":
			// Check for tool results
			var userMsg ToolResultMessage
			if err := json.Unmarshal(streamEvent.Message, &userMsg); err == nil {
				for _, result := range userMsg.Content {
					if result.Type == "tool_result" {
						// Update the tool line with completion status
						updateToolCompletion(&outputLines, result.ToolUseID,
							toolStartTimes, result.IsError)
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}

	// Flush any remaining text
	if textBuffer.Len() > 0 {
		outputLines = append(outputLines, session.OutputLine{
			Type:    session.OutputTypeText,
			Content: textBuffer.String(),
		})
	}

	// Create session info for rendering
	info := &session.SessionInfo{
		ID:     "session-replay",
		Type:   session.SessionTypeBuilder, // Assume builder for now
		Status: session.StatusCompleted,
		Prompt: userPrompt,
	}
	if userPrompt == "" {
		info.Prompt = "(prompt not found in session file)"
	}

	// Render using the same OutputModel as the TUI
	fmt.Println(strings.Repeat("=", width))
	fmt.Println("SESSION RENDERING OUTPUT (using TUI widget with markdown)")
	fmt.Printf("Size: %dx%d\n", width, height)
	fmt.Println(strings.Repeat("=", width))

	model := app.NewOutputModelWithMarkdown(info, outputLines, width)
	model.SetSize(width, height)
	fmt.Println(model.View())

	fmt.Println(strings.Repeat("=", width))

	// Summary
	var toolCount, textCount, errorCount int
	for i := range outputLines {
		switch outputLines[i].Type {
		case session.OutputTypeToolStart:
			toolCount++
			if outputLines[i].ToolState == session.ToolStateError {
				errorCount++
			}
		case session.OutputTypeText:
			textCount++
		case session.OutputTypeError:
			errorCount++
		}
	}
	fmt.Printf("Total: %d output lines | Tools: %d | Text blocks: %d | Errors: %d\n",
		len(outputLines), toolCount, textCount, errorCount)

	// Show raw output lines for debugging
	fmt.Println("\n--- RAW OUTPUT LINES (for debugging) ---")
	for i := range outputLines {
		renderDebugLine(i, outputLines[i])
	}

	return nil
}

// toolInputBuffers accumulates streamed JSON input for tools.
var toolInputBuffers = make(map[string]*strings.Builder)

func processStreamEvent(eventData json.RawMessage, outputLines *[]session.OutputLine,
	toolStartTimes map[string]time.Time, currentToolID, currentToolName *string,
	textBuffer *strings.Builder) {

	var baseEvent struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(eventData, &baseEvent); err != nil {
		return
	}

	switch baseEvent.Type {
	case "content_block_start":
		var event ContentBlockStart
		if err := json.Unmarshal(eventData, &event); err != nil {
			return
		}

		if event.ContentBlock.Type == "tool_use" {
			// Flush text buffer before tool
			if textBuffer.Len() > 0 {
				*outputLines = append(*outputLines, session.OutputLine{
					Type:    session.OutputTypeText,
					Content: textBuffer.String(),
				})
				textBuffer.Reset()
			}

			// Start new tool
			*currentToolID = event.ContentBlock.ID
			*currentToolName = event.ContentBlock.Name
			now := time.Now()
			toolStartTimes[event.ContentBlock.ID] = now

			// Initialize input buffer for this tool
			toolInputBuffers[event.ContentBlock.ID] = &strings.Builder{}

			*outputLines = append(*outputLines, session.OutputLine{
				Type:      session.OutputTypeToolStart,
				ToolName:  event.ContentBlock.Name,
				ToolID:    event.ContentBlock.ID,
				ToolState: session.ToolStateRunning,
				StartTime: now,
			})
		}

	case "content_block_delta":
		var event ContentBlockDelta
		if err := json.Unmarshal(eventData, &event); err != nil {
			return
		}

		if event.Delta.Type == "text_delta" {
			textBuffer.WriteString(event.Delta.Text)
		} else if event.Delta.Type == "input_json_delta" {
			// Accumulate tool input JSON
			if *currentToolID != "" {
				if buf, ok := toolInputBuffers[*currentToolID]; ok {
					buf.WriteString(event.Delta.PartialJSON)
				}
			}
		}

	case "content_block_stop":
		// Tool input is complete, parse and store the input
		if *currentToolID != "" {
			if buf, ok := toolInputBuffers[*currentToolID]; ok {
				inputJSON := buf.String()
				if inputJSON != "" {
					var input map[string]interface{}
					if err := json.Unmarshal([]byte(inputJSON), &input); err == nil {
						// Update the tool line with parsed input
						for i := len(*outputLines) - 1; i >= 0; i-- {
							if (*outputLines)[i].ToolID == *currentToolID {
								(*outputLines)[i].ToolInput = input
								break
							}
						}
					}
				}
				delete(toolInputBuffers, *currentToolID)
			}
			*currentToolID = ""
			*currentToolName = ""
		}
	}
}

func updateToolCompletion(outputLines *[]session.OutputLine, toolID string,
	toolStartTimes map[string]time.Time, isError bool) {

	for i := len(*outputLines) - 1; i >= 0; i-- {
		line := &(*outputLines)[i]
		if line.ToolID == toolID && line.Type == session.OutputTypeToolStart {
			// Calculate duration
			if startTime, ok := toolStartTimes[toolID]; ok {
				line.DurationMs = time.Since(startTime).Milliseconds()
			}

			if isError {
				line.ToolState = session.ToolStateError
				line.IsError = true
			} else {
				line.ToolState = session.ToolStateComplete
			}
			return
		}
	}
}

// renderDebugLine renders a single line for debugging purposes.
func renderDebugLine(idx int, line session.OutputLine) {
	prefix := fmt.Sprintf("[%3d] ", idx)

	switch line.Type {
	case session.OutputTypeText:
		content := line.Content
		if len(content) > 80 {
			content = content[:80] + "..."
		}
		content = strings.ReplaceAll(content, "\n", "\\n")
		fmt.Printf("%sTEXT: %s\n", prefix, content)

	case session.OutputTypeToolStart:
		var stateIcon string
		switch line.ToolState {
		case session.ToolStateRunning:
			stateIcon = "⏳"
		case session.ToolStateComplete:
			stateIcon = "✓"
		case session.ToolStateError:
			stateIcon = "✗"
		default:
			stateIcon = "?"
		}
		inputStr := ""
		if line.ToolInput != nil {
			if path, ok := line.ToolInput["file_path"].(string); ok {
				inputStr = path
			} else if cmd, ok := line.ToolInput["command"].(string); ok {
				inputStr = cmd
			} else if pattern, ok := line.ToolInput["pattern"].(string); ok {
				inputStr = pattern
			}
			if len(inputStr) > 40 {
				inputStr = inputStr[:40] + "..."
			}
		}
		fmt.Printf("%s%s [%s] %s\n", prefix, stateIcon, line.ToolName, inputStr)

	case session.OutputTypeStatus:
		fmt.Printf("%s→ %s\n", prefix, line.Content)

	case session.OutputTypeError:
		fmt.Printf("%s✗ %s\n", prefix, line.Content)

	default:
		fmt.Printf("%s%s: %s\n", prefix, line.Type, truncate(line.Content, 60))
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
