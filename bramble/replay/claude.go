package replay

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/bramble/session"
)

// Claude JSONL session log types

type claudeSessionMessage struct {
	Timestamp string          `json:"timestamp"`
	Direction string          `json:"direction"`
	Message   json.RawMessage `json:"message"`
}

type claudeStreamEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	Event     json.RawMessage `json:"event"`
	Message   json.RawMessage `json:"message"`
}

type claudeContentBlockStart struct {
	ContentBlock claudeContentBlock `json:"content_block"`
	Type         string             `json:"type"`
	Index        int                `json:"index"`
}

type claudeContentBlock struct {
	Input any    `json:"input"`
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Text  string `json:"text"`
}

type claudeContentBlockDelta struct {
	Delta claudeDeltaBlock `json:"delta"`
	Type  string           `json:"type"`
	Index int              `json:"index"`
}

type claudeDeltaBlock struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
}

type claudeToolResultMessage struct {
	Role    string             `json:"role"`
	Content []claudeToolResult `json:"content"`
}

type claudeToolResult struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

type claudeUserMessage struct {
	Type    string `json:"type"`
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

// claudeReplayParser accumulates state while parsing a Claude session log.
type claudeReplayParser struct { //nolint:govet // fieldalignment: readability over packing
	lines           []session.OutputLine
	toolStartTimes  map[string]time.Time
	toolInputBufs   map[string]*strings.Builder
	currentToolID   string
	currentToolName string
	textBuffer      strings.Builder
	prompt          string
}

func newClaudeReplayParser() *claudeReplayParser {
	return &claudeReplayParser{
		toolStartTimes: make(map[string]time.Time),
		toolInputBufs:  make(map[string]*strings.Builder),
	}
}

func parseClaudeLog(path string) (*Result, error) {
	// If path is a directory, look for messages.jsonl inside it
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	filePath := path
	if info.IsDir() {
		filePath = filepath.Join(path, "messages.jsonl")
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	p := newClaudeReplayParser()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	for scanner.Scan() {
		var msg claudeSessionMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}

		if msg.Direction == "sent" && p.prompt == "" {
			var userMsg claudeUserMessage
			if err := json.Unmarshal(msg.Message, &userMsg); err == nil {
				if userMsg.Type == "user" {
					p.prompt = userMsg.Message.Content
				}
			}
			continue
		}

		if msg.Direction != "received" {
			continue
		}

		var streamEvent claudeStreamEvent
		if err := json.Unmarshal(msg.Message, &streamEvent); err != nil {
			continue
		}

		switch streamEvent.Type {
		case "stream_event":
			p.processStreamEvent(streamEvent.Event)
		case "user":
			var userMsg claudeToolResultMessage
			if err := json.Unmarshal(streamEvent.Message, &userMsg); err == nil {
				for _, result := range userMsg.Content {
					if result.Type == "tool_result" {
						p.updateToolCompletion(result.ToolUseID, result.IsError)
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Flush remaining text
	if p.textBuffer.Len() > 0 {
		p.lines = append(p.lines, session.OutputLine{
			Type:    session.OutputTypeText,
			Content: p.textBuffer.String(),
		})
	}

	return &Result{
		Lines:  p.lines,
		Prompt: p.prompt,
		Status: session.StatusCompleted,
		Format: FormatClaude,
	}, nil
}

func (p *claudeReplayParser) processStreamEvent(eventData json.RawMessage) {
	var baseEvent struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(eventData, &baseEvent); err != nil {
		return
	}

	switch baseEvent.Type {
	case "content_block_start":
		var event claudeContentBlockStart
		if err := json.Unmarshal(eventData, &event); err != nil {
			return
		}
		if event.ContentBlock.Type == "tool_use" {
			// Flush text buffer before tool
			if p.textBuffer.Len() > 0 {
				p.lines = append(p.lines, session.OutputLine{
					Type:    session.OutputTypeText,
					Content: p.textBuffer.String(),
				})
				p.textBuffer.Reset()
			}

			p.currentToolID = event.ContentBlock.ID
			p.currentToolName = event.ContentBlock.Name
			now := time.Now()
			p.toolStartTimes[event.ContentBlock.ID] = now
			p.toolInputBufs[event.ContentBlock.ID] = &strings.Builder{}

			p.lines = append(p.lines, session.OutputLine{
				Type:      session.OutputTypeToolStart,
				ToolName:  event.ContentBlock.Name,
				ToolID:    event.ContentBlock.ID,
				ToolState: session.ToolStateRunning,
				StartTime: now,
			})
		}

	case "content_block_delta":
		var event claudeContentBlockDelta
		if err := json.Unmarshal(eventData, &event); err != nil {
			return
		}
		if event.Delta.Type == "text_delta" {
			p.textBuffer.WriteString(event.Delta.Text)
		} else if event.Delta.Type == "input_json_delta" {
			if p.currentToolID != "" {
				if buf, ok := p.toolInputBufs[p.currentToolID]; ok {
					buf.WriteString(event.Delta.PartialJSON)
				}
			}
		}

	case "content_block_stop":
		if p.currentToolID != "" {
			if buf, ok := p.toolInputBufs[p.currentToolID]; ok {
				inputJSON := buf.String()
				if inputJSON != "" {
					var input map[string]interface{}
					if err := json.Unmarshal([]byte(inputJSON), &input); err == nil {
						for i := len(p.lines) - 1; i >= 0; i-- {
							if p.lines[i].ToolID == p.currentToolID {
								p.lines[i].ToolInput = input
								break
							}
						}
					}
				}
				delete(p.toolInputBufs, p.currentToolID)
			}
			p.currentToolID = ""
			p.currentToolName = ""
		}
	}
}

func (p *claudeReplayParser) updateToolCompletion(toolID string, isError bool) {
	for i := len(p.lines) - 1; i >= 0; i-- {
		line := &p.lines[i]
		if line.ToolID == toolID && line.Type == session.OutputTypeToolStart {
			if startTime, ok := p.toolStartTimes[toolID]; ok {
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
