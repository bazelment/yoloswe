package sessionmodel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// LoadFromRawJSONL loads a ~/.claude/projects/ JSONL session file into a
// SessionModel. It feeds each line through FromRawJSONL → MessageParser and
// handles envelope-only types (system subtypes, progress, pr-link, etc.)
// by emitting synthetic OutputLines.
func LoadFromRawJSONL(path string) (*SessionModel, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	model := NewSessionModel(-1) // uncapped: replay must preserve all history
	parser := NewMessageParser(model)

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024) // 10 MB max line

	var envelopeSessionID string

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		msg, meta, err := FromRawJSONL(line)
		if err != nil {
			continue // skip unparseable lines
		}

		// Vocabulary messages go through the parser.
		if msg != nil {
			parser.HandleMessage(msg)
		}

		// Capture the first envelope sessionId as a fallback (raw JSONL puts
		// the session ID in the outer envelope, not the inner message).
		if meta != nil && meta.SessionID != "" && envelopeSessionID == "" {
			envelopeSessionID = meta.SessionID
		}

		// Envelope-only types produce synthetic output lines.
		if meta != nil && msg == nil {
			handleEnvelopeMeta(model, meta)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan raw JSONL: %w", err)
	}

	// Patch in the envelope session ID if the inner messages didn't carry one.
	parser.PatchSessionID(envelopeSessionID)

	return model, nil
}

// handleEnvelopeMeta converts envelope-only metadata into model mutations.
func handleEnvelopeMeta(model *SessionModel, meta *RawEnvelopeMeta) {
	switch meta.Type {
	case "system":
		handleSystemMeta(model, meta)
	case "progress":
		handleProgressMeta(model, meta)
	case "pr-link":
		if meta.PRURL != "" {
			model.AppendOutput(OutputLine{
				Timestamp: meta.Timestamp,
				Type:      OutputTypeStatus,
				Content:   fmt.Sprintf("PR #%d: %s", meta.PRNumber, meta.PRURL),
			})
		}
	// file-history-snapshot, queue-operation: skip (internal bookkeeping)
	}
}

func handleSystemMeta(model *SessionModel, meta *RawEnvelopeMeta) {
	switch meta.Subtype {
	case "api_error":
		content := "API error"
		if len(meta.ErrorJSON) > 0 {
			var errObj struct {
				Cause struct {
					Code string `json:"code"`
					Path string `json:"path"`
				} `json:"cause"`
			}
			if json.Unmarshal(meta.ErrorJSON, &errObj) == nil && errObj.Cause.Code != "" {
				content = fmt.Sprintf("API error: %s (%s)", errObj.Cause.Code, errObj.Cause.Path)
			}
		}
		model.AppendOutput(OutputLine{
			Timestamp: meta.Timestamp,
			Type:      OutputTypeError,
			Content:   content,
			IsError:   true,
		})

	case "turn_duration":
		if meta.DurationMs > 0 {
			secs := float64(meta.DurationMs) / 1000
			model.AppendOutput(OutputLine{
				Timestamp:  meta.Timestamp,
				Type:       OutputTypeStatus,
				Content:    fmt.Sprintf("Turn duration: %.1fs", secs),
				DurationMs: meta.DurationMs,
			})
		}

	case "compact_boundary":
		model.AppendOutput(OutputLine{
			Timestamp: meta.Timestamp,
			Type:      OutputTypeStatus,
			Content:   "── Context compacted ──",
		})

	case "local_command":
		if meta.Content != "" {
			model.AppendOutput(OutputLine{
				Timestamp: meta.Timestamp,
				Type:      OutputTypeStatus,
				Content:   "/ " + meta.Content,
			})
		}
	}
}

// progressData is the envelope for progress messages.
type progressData struct {
	Type        string `json:"type"`
	Output      string `json:"output,omitempty"`
	Status      string `json:"status,omitempty"`
	ServerName  string `json:"serverName,omitempty"`
	ToolName    string `json:"toolName,omitempty"`
	Message     string `json:"message,omitempty"`
	Description string `json:"taskDescription,omitempty"`
	TaskType    string `json:"taskType,omitempty"`
}

func handleProgressMeta(model *SessionModel, meta *RawEnvelopeMeta) {
	if len(meta.Data) == 0 {
		return
	}

	var data progressData
	if err := json.Unmarshal(meta.Data, &data); err != nil {
		return
	}

	switch data.Type {
	case "bash_progress":
		// Bash progress heartbeats — skip to avoid flooding the output.
		// The tool_start/tool_result cycle already covers this.

	case "agent_progress":
		// Sub-agent progress — skip, handled by the parent's tool tracking.

	case "mcp_progress":
		if data.Status == "completed" || data.Status == "failed" {
			status := "completed"
			if data.Status == "failed" {
				status = "failed"
			}
			model.AppendOutput(OutputLine{
				Timestamp: meta.Timestamp,
				Type:      OutputTypeStatus,
				Content:   fmt.Sprintf("MCP %s/%s: %s", data.ServerName, data.ToolName, status),
			})
		}

	case "hook_progress":
		// Hook progress — skip (internal)

	case "waiting_for_task":
		if data.Description != "" {
			model.AppendOutput(OutputLine{
				Timestamp: meta.Timestamp,
				Type:      OutputTypeStatus,
				Content:   fmt.Sprintf("Waiting: %s", data.Description),
			})
		}
	}
}
