package sessionplayer

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	codexrender "github.com/bazelment/yoloswe/agent-cli-wrapper/codex/render"
)

// SessionFormat identifies the session log format.
type SessionFormat string

const (
	FormatClaude SessionFormat = "claude" // messages.jsonl in directory
	FormatCodex  SessionFormat = "codex"  // single JSONL with header
)

// DetectFormat determines the session format from the path.
func DetectFormat(path string) (SessionFormat, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}

	if info.IsDir() {
		// Directory with messages.jsonl = Claude format
		if _, err := os.Stat(filepath.Join(path, "messages.jsonl")); err == nil {
			return FormatClaude, nil
		}
		return "", fmt.Errorf("directory missing messages.jsonl")
	}

	// Single file - check header for format
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		var header struct {
			Format string `json:"format"`
		}
		if json.Unmarshal(scanner.Bytes(), &header) == nil {
			if header.Format == "codex" {
				return FormatCodex, nil
			}
		}
	}

	// Default to Codex for JSONL files without header (legacy format)
	return FormatCodex, nil
}

// CodexPlayer plays back Codex session logs.
type CodexPlayer struct {
	renderer      *codexrender.Renderer
	turnStartTime string
	lastTimestamp string
	inputTokens   int64
	outputTokens  int64
	verbose       bool
}

// NewCodexPlayer creates a new Codex session player.
func NewCodexPlayer(renderer *codexrender.Renderer, verbose bool) *CodexPlayer {
	return &CodexPlayer{
		renderer: renderer,
		verbose:  verbose,
	}
}

// PlayFile plays back a Codex session log file.
func (p *CodexPlayer) PlayFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Increase buffer for large lines (base64 encoded content)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	for scanner.Scan() {
		p.handleLine(scanner.Bytes())
	}
	return scanner.Err()
}

// handleLine processes a single JSONL line.
func (p *CodexPlayer) handleLine(line []byte) {
	// Try to parse as session log entry
	var entry codex.SessionLogEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return
	}

	// Skip header line (has format field but no direction)
	if entry.Direction == "" {
		return
	}

	// Track timestamp for duration calculation
	p.lastTimestamp = entry.Timestamp

	// Only process received messages
	if entry.Direction != "received" {
		return
	}

	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(entry.Message, &msg); err != nil {
		return
	}

	p.handleMethod(msg.Method, msg.Params)
}

// handleMethod dispatches Codex events to the renderer.
func (p *CodexPlayer) handleMethod(method string, params json.RawMessage) {
	switch method {
	case codex.NotifyCodexEventExecBegin:
		p.handleExecBegin(params)
	case codex.NotifyCodexEventExecEnd:
		p.handleExecEnd(params)
	case codex.NotifyCodexEventExecOutput:
		p.handleExecOutput(params)
	case codex.NotifyItemCommandOutputDelta:
		p.handleItemOutputDelta(params)
	case codex.NotifyCodexEventReasoningDelta:
		p.handleReasoning(params)
	case codex.NotifyAgentMessageDelta:
		p.handleTextDelta(params)
	case codex.NotifyTurnStarted:
		p.handleTurnStarted(params)
	case codex.NotifyTurnCompleted:
		p.handleTurnCompleted(params)
	case codex.NotifyCodexEventTokenCount:
		p.handleTokenCount(params)
	case codex.NotifyItemStarted:
		p.handleItemStarted(params)
	}
}

func (p *CodexPlayer) handleExecBegin(params json.RawMessage) {
	var notif codex.CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	var msg codex.ExecCommandBeginMsg
	if err := json.Unmarshal(notif.Msg, &msg); err != nil {
		return
	}

	// Extract simplified command for display
	parsedCmd := ""
	if len(msg.ParsedCmd) > 0 {
		parsedCmd = msg.ParsedCmd[0].Cmd
	} else if len(msg.Command) >= 3 {
		parsedCmd = msg.Command[2]
	}

	p.renderer.CommandStart(msg.CallID, parsedCmd)
}

func (p *CodexPlayer) handleExecEnd(params json.RawMessage) {
	var notif codex.CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	var msg codex.ExecCommandEndMsg
	if err := json.Unmarshal(notif.Msg, &msg); err != nil {
		return
	}

	// Only provide output if we didn't receive it via streaming (outputDelta events)
	// The renderer tracks if output was already accumulated for this callID
	// We use FormattedOutput as fallback only if no streaming output was received
	if !p.renderer.HasOutput(msg.CallID) {
		if msg.FormattedOutput != "" {
			p.renderer.CommandOutput(msg.CallID, msg.FormattedOutput)
		} else if msg.Stdout != "" {
			p.renderer.CommandOutput(msg.CallID, msg.Stdout)
		}
	}

	durationMs := msg.Duration.Secs*1000 + msg.Duration.Nanos/1000000
	p.renderer.CommandEnd(msg.CallID, msg.ExitCode, durationMs)
}

func (p *CodexPlayer) handleExecOutput(params json.RawMessage) {
	var notif codex.CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	var msg codex.ExecCommandOutputMsg
	if err := json.Unmarshal(notif.Msg, &msg); err != nil {
		return
	}

	// The Codex protocol always base64-encodes the chunk field
	// (see codex-rs/protocol/src/protocol.rs ExecCommandOutputDeltaEvent)
	chunk := msg.Chunk
	if decoded, err := base64.StdEncoding.DecodeString(msg.Chunk); err == nil {
		chunk = string(decoded)
	}
	p.renderer.CommandOutput(msg.CallID, chunk)
}

func (p *CodexPlayer) handleItemOutputDelta(params json.RawMessage) {
	// item/commandExecution/outputDelta has a different structure
	var notif struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		ItemID   string `json:"itemId"`
		Delta    string `json:"delta"`
	}
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	// Use itemId as callId since they correspond
	p.renderer.CommandOutput(notif.ItemID, notif.Delta)
}

func (p *CodexPlayer) handleReasoning(params json.RawMessage) {
	var notif codex.CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	var msg codex.ReasoningDeltaMsg
	if err := json.Unmarshal(notif.Msg, &msg); err != nil {
		return
	}

	p.renderer.Reasoning(msg.Delta)
}

func (p *CodexPlayer) handleTextDelta(params json.RawMessage) {
	var notif codex.AgentMessageDeltaNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	p.renderer.Text(notif.Delta)
}

func (p *CodexPlayer) handleTurnStarted(params json.RawMessage) {
	// Record turn start time from the entry timestamp
	p.turnStartTime = p.lastTimestamp
}

func (p *CodexPlayer) handleTurnCompleted(params json.RawMessage) {
	var notif codex.TurnCompletedNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	success := notif.Turn.Status == "completed"

	// Calculate duration from turn start to completion
	var durationMs int64
	if p.turnStartTime != "" && p.lastTimestamp != "" {
		startTime, err1 := time.Parse(time.RFC3339Nano, p.turnStartTime)
		endTime, err2 := time.Parse(time.RFC3339Nano, p.lastTimestamp)
		if err1 == nil && err2 == nil {
			durationMs = endTime.Sub(startTime).Milliseconds()
		}
	}

	p.renderer.TurnComplete(success, durationMs, p.inputTokens, p.outputTokens)
}

func (p *CodexPlayer) handleTokenCount(params json.RawMessage) {
	var notif codex.CodexEventNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	var msg struct {
		Info *struct {
			TotalTokenUsage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"total_token_usage"`
		} `json:"info"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(notif.Msg, &msg); err != nil {
		return
	}

	if msg.Info != nil {
		p.inputTokens = msg.Info.TotalTokenUsage.InputTokens
		p.outputTokens = msg.Info.TotalTokenUsage.OutputTokens
	}
}

func (p *CodexPlayer) handleItemStarted(params json.RawMessage) {
	// Could add verbose output for item started events
	if !p.verbose {
		return
	}

	var notif codex.ItemNotification
	if err := json.Unmarshal(params, &notif); err != nil {
		return
	}

	var item struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(notif.Item, &item); err != nil {
		return
	}

	if item.Type == "Reasoning" {
		// Reasoning items are rendered via reasoning delta events
		return
	}

	p.renderer.Status(fmt.Sprintf("Item started: %s", item.Type))
}
