// Command codexlogview renders a Codex protocol session log using Bramble's
// OutputModel, so output can be replayed as it appears in the Bramble UI.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/yoloswe/sessionplayer"
)

type cliConfig struct {
	paths          []string
	width          int
	height         int
	enableMarkdown bool
	compact        bool
}

func main() {
	cfg, err := parseCLIArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Usage: %s <log1.jsonl> [log2.jsonl ...] [width] [height] [markdown] [compact|full]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	hadErrors := false
	for i, path := range cfg.paths {
		rendered, renderErr := renderLog(path, cfg)
		if renderErr != nil {
			hadErrors = true
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, renderErr)
			continue
		}
		if i > 0 {
			fmt.Println()
		}
		fmt.Println(rendered)
	}
	if hadErrors {
		os.Exit(1)
	}
}

func parseCLIArgs(args []string) (cliConfig, error) {
	cfg := cliConfig{
		width:          120,
		height:         30,
		enableMarkdown: true,
		compact:        true,
	}
	if len(args) == 0 {
		return cfg, errors.New("missing protocol log path")
	}

	parts := append([]string(nil), args...)

	if len(parts) > 0 {
		if compact, ok := parseCompactArg(parts[len(parts)-1]); ok {
			cfg.compact = compact
			parts = parts[:len(parts)-1]
		}
	}

	if len(parts) > 0 {
		if md, ok := parseMarkdownArg(parts[len(parts)-1]); ok {
			cfg.enableMarkdown = md
			parts = parts[:len(parts)-1]
		}
	}

	if len(parts) > 0 {
		if v, ok := parsePositiveInt(parts[len(parts)-1]); ok {
			cfg.height = v
			parts = parts[:len(parts)-1]
		}
	}

	if len(parts) > 0 {
		if v, ok := parsePositiveInt(parts[len(parts)-1]); ok {
			cfg.width = v
			parts = parts[:len(parts)-1]
		}
	}

	if len(parts) == 0 {
		return cfg, errors.New("missing protocol log path")
	}
	cfg.paths = parts
	return cfg, nil
}

func renderLog(path string, cfg cliConfig) (string, error) {
	format, err := sessionplayer.DetectFormat(path)
	if err != nil {
		return "", fmt.Errorf("failed to detect log format: %w", err)
	}
	if format != sessionplayer.FormatCodex {
		return "", fmt.Errorf("expected codex log format, got %q", format)
	}

	replay, err := parseCodexProtocolLog(path)
	if err != nil {
		return "", fmt.Errorf("failed to parse log: %w", err)
	}
	if cfg.compact {
		replay.lines = compactReplayLines(replay.lines)
	}

	info := &session.SessionInfo{
		ID:     session.SessionID(filepath.Base(path)),
		Type:   session.SessionTypeBuilder,
		Status: replay.status,
		Prompt: replay.prompt,
	}
	if info.Status == "" {
		info.Status = session.StatusCompleted
	}
	if strings.TrimSpace(info.Prompt) == "" {
		info.Prompt = "(unknown prompt)"
	}

	model := app.NewOutputModel(info, replay.lines)
	if cfg.enableMarkdown {
		model.EnableMarkdown()
	}
	model.SetSize(cfg.width, cfg.height)
	return model.View(), nil
}

func parsePositiveInt(s string) (int, bool) {
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

func parseMarkdownArg(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "no", "plain":
		return false, true
	case "1", "true", "yes", "markdown":
		return true, true
	default:
		return false, false
	}
}

func parseCompactArg(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "compact":
		return true, true
	case "full", "verbose":
		return false, true
	default:
		return false, false
	}
}

type codexReplay struct { //nolint:govet // fieldalignment: readability over packing
	lines  []session.OutputLine
	prompt string
	status session.SessionStatus
}

type codexReplayParser struct { //nolint:govet // fieldalignment: readability over packing
	lines             []session.OutputLine
	itemTextLine      map[string]int
	threadActiveItem  map[string]string
	toolLineIndex     map[string]int
	threadTokenUsage  map[string]codex.TokenUsage
	threadReasoning   map[string]bool
	threadText        map[string]*strings.Builder
	pendingApprovals  map[string]struct{}
	emittedApprovals  map[string]struct{}
	prompt            string
	turnCount         int
	turnStarts        int
	turnCompletions   int
	hadProviderErrors bool
}

func newCodexReplayParser() *codexReplayParser {
	return &codexReplayParser{
		itemTextLine:     make(map[string]int),
		threadActiveItem: make(map[string]string),
		toolLineIndex:    make(map[string]int),
		threadTokenUsage: make(map[string]codex.TokenUsage),
		threadReasoning:  make(map[string]bool),
		threadText:       make(map[string]*strings.Builder),
		pendingApprovals: make(map[string]struct{}),
		emittedApprovals: make(map[string]struct{}),
	}
}

func parseCodexProtocolLog(path string) (*codexReplay, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	p := newCodexReplayParser()
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	for scanner.Scan() {
		var entry codex.SessionLogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Direction == "" {
			continue // header
		}
		ts := parseTimestamp(entry.Timestamp)
		switch entry.Direction {
		case "sent":
			p.handleSent(entry.Message, ts)
		case "received":
			p.handleReceived(entry.Message, ts)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return &codexReplay{
		lines:  p.lines,
		prompt: p.prompt,
		status: p.deriveStatus(),
	}, nil
}

func (p *codexReplayParser) handleSent(raw json.RawMessage, ts time.Time) {
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	if msg.Method != "turn/start" {
		return
	}

	var params codex.TurnStartParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return
	}

	p.turnStarts++

	text := strings.TrimSpace(firstTurnText(params.Input))
	if text == "" {
		return
	}

	if p.prompt == "" {
		p.prompt = text
	} else {
		p.lines = append(p.lines,
			session.OutputLine{
				Timestamp: ts,
				Type:      session.OutputTypeStatus,
				Content:   "Follow-up prompt:",
			},
			session.OutputLine{
				Timestamp: ts,
				Type:      session.OutputTypeText,
				Content:   text,
			},
		)
	}

	p.threadText[params.ThreadID] = &strings.Builder{}
	p.threadReasoning[params.ThreadID] = false
	p.threadActiveItem[params.ThreadID] = ""
}

func (p *codexReplayParser) handleReceived(raw json.RawMessage, ts time.Time) {
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}

	switch msg.Method {
	case codex.NotifyAgentMessageDelta:
		var notif codex.AgentMessageDeltaNotification
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		p.appendTextDelta(ts, notif.ThreadID, notif.ItemID, notif.Delta)
		p.appendThreadText(notif.ThreadID, notif.Delta)

	case codex.NotifyCodexEventReasoningDelta:
		var notif codex.CodexEventNotification
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		var reasoning codex.ReasoningDeltaMsg
		if err := json.Unmarshal(notif.Msg, &reasoning); err != nil {
			return
		}
		p.threadReasoning[notif.ConversationID] = true
		p.appendOrAddThinking(ts, reasoning.Delta)

	case "item/reasoning/summaryTextDelta":
		var notif struct {
			ThreadID string `json:"threadId"`
			Delta    string `json:"delta"`
		}
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		if p.threadReasoning[notif.ThreadID] {
			return
		}
		p.appendOrAddThinking(ts, notif.Delta)

	case "codex/event/reasoning_content_delta":
		var notif codex.CodexEventNotification
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		var reasoning struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(notif.Msg, &reasoning); err != nil {
			return
		}
		if p.threadReasoning[notif.ConversationID] {
			return
		}
		p.appendOrAddThinking(ts, reasoning.Delta)

	case codex.NotifyCodexEventError:
		var notif codex.CodexEventNotification
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		p.lines = append(p.lines, session.OutputLine{
			Timestamp: ts,
			Type:      session.OutputTypeError,
			Content:   extractErrorMessage(notif.Msg),
		})
		p.hadProviderErrors = true

	case codex.NotifyCodexEventExecBegin:
		var notif codex.CodexEventNotification
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		var begin codex.ExecCommandBeginMsg
		if err := json.Unmarshal(notif.Msg, &begin); err != nil {
			return
		}
		cmd := commandDisplay(begin)
		input := map[string]interface{}{}
		if cmd != "" {
			input["command"] = cmd
		}
		if begin.CWD != "" {
			input["cwd"] = begin.CWD
		}
		p.lines = append(p.lines, session.OutputLine{
			Timestamp: ts,
			Type:      session.OutputTypeToolStart,
			Content:   "Bash: " + cmd,
			ToolName:  "Bash",
			ToolID:    begin.CallID,
			ToolInput: input,
			ToolState: session.ToolStateRunning,
			StartTime: ts,
		})
		p.toolLineIndex[begin.CallID] = len(p.lines) - 1
		p.clearPendingApproval(begin.CallID)

	case codex.NotifyCodexEventExecEnd:
		var notif codex.CodexEventNotification
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		var end codex.ExecCommandEndMsg
		if err := json.Unmarshal(notif.Msg, &end); err != nil {
			return
		}
		p.updateToolCompletion(end, ts)

	case codex.NotifyCodexEventTokenCount:
		var notif codex.CodexEventNotification
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		var tokenCount codex.TokenCountMsg
		if err := json.Unmarshal(notif.Msg, &tokenCount); err != nil {
			return
		}
		if tokenCount.Info != nil && tokenCount.Info.TotalTokenUsage != nil {
			p.threadTokenUsage[notif.ConversationID] = *tokenCount.Info.TotalTokenUsage
		}

	case codex.NotifyCodexEventTaskComplete:
		var notif codex.CodexEventNotification
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		var taskComplete codex.TaskCompleteMsg
		if err := json.Unmarshal(notif.Msg, &taskComplete); err != nil {
			return
		}
		last := strings.TrimSpace(taskComplete.LastAgentMessage)
		if last == "" {
			return
		}
		threadText := p.threadText[notif.ConversationID]
		if threadText == nil || threadText.Len() == 0 {
			p.appendOrAddText(ts, last)
		}

	case codex.NotifyItemCompleted:
		var notif struct {
			ThreadID string `json:"threadId"`
			Item     struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		if !strings.EqualFold(notif.Item.Type, "agentMessage") || strings.TrimSpace(notif.Item.Text) == "" {
			return
		}
		p.setFinalItemText(ts, notif.ThreadID, notif.Item.ID, notif.Item.Text)

	case "codex/event/exec_approval_request":
		var notif codex.CodexEventNotification
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		var req struct {
			CallID  string   `json:"call_id"`
			Command []string `json:"command"`
			Reason  string   `json:"reason"`
		}
		if err := json.Unmarshal(notif.Msg, &req); err != nil {
			return
		}
		p.recordApprovalRequest(ts, req.CallID, strings.TrimSpace(strings.Join(req.Command, " ")), req.Reason)

	case "item/commandExecution/requestApproval":
		var req struct {
			ItemID  string `json:"itemId"`
			Reason  string `json:"reason"`
			Command string `json:"command"`
		}
		if err := json.Unmarshal(msg.Params, &req); err != nil {
			return
		}
		p.recordApprovalRequest(ts, req.ItemID, req.Command, req.Reason)

	case codex.NotifyTurnCompleted:
		var notif codex.TurnCompletedNotification
		if err := json.Unmarshal(msg.Params, &notif); err != nil {
			return
		}
		p.turnCount++
		p.turnCompletions++
		p.pendingApprovals = make(map[string]struct{})
		usage := p.threadTokenUsage[notif.ThreadID]
		p.lines = append(p.lines, session.OutputLine{
			Timestamp:  ts,
			Type:       session.OutputTypeTurnEnd,
			Content:    fmt.Sprintf("Turn %d complete", p.turnCount),
			TurnNumber: p.turnCount,
			DurationMs: 0,
			CostUSD:    0,
		})
		if usage.InputTokens > 0 || usage.OutputTokens > 0 {
			p.lines = append(p.lines, session.OutputLine{
				Timestamp: ts,
				Type:      session.OutputTypeStatus,
				Content:   fmt.Sprintf("Tokens: %d input / %d output", usage.InputTokens, usage.OutputTokens),
			})
		}
		p.threadText[notif.ThreadID] = &strings.Builder{}
		p.threadReasoning[notif.ThreadID] = false
		p.threadActiveItem[notif.ThreadID] = ""
	}
}

func (p *codexReplayParser) deriveStatus() session.SessionStatus {
	if p.hadProviderErrors {
		return session.StatusFailed
	}
	if len(p.pendingApprovals) > 0 {
		return session.StatusIdle
	}
	if p.turnStarts > p.turnCompletions {
		return session.StatusRunning
	}
	return session.StatusCompleted
}

func (p *codexReplayParser) recordApprovalRequest(ts time.Time, callID, command, reason string) {
	key := approvalKey(callID, command, reason)
	if key == "" {
		return
	}
	if _, seen := p.emittedApprovals[key]; seen {
		return
	}
	p.emittedApprovals[key] = struct{}{}
	p.pendingApprovals[key] = struct{}{}

	p.lines = append(p.lines, session.OutputLine{
		Timestamp: ts,
		Type:      session.OutputTypeStatus,
		Content:   "Approval required before command execution",
	})

	details := strings.TrimSpace(command)
	if details == "" {
		details = "(command unavailable)"
	}
	reason = strings.TrimSpace(reason)
	if reason != "" {
		details += "\n\nReason: " + reason
	}
	p.lines = append(p.lines, session.OutputLine{
		Timestamp: ts,
		Type:      session.OutputTypeText,
		Content:   details,
	})
}

func (p *codexReplayParser) clearPendingApproval(callID string) {
	key := approvalKey(callID, "", "")
	if key == "" {
		return
	}
	delete(p.pendingApprovals, key)
}

func (p *codexReplayParser) appendTextDelta(ts time.Time, threadID, itemID, delta string) {
	if delta == "" {
		return
	}
	if threadID == "" || itemID == "" {
		p.appendOrAddText(ts, delta)
		return
	}

	if p.threadActiveItem[threadID] != itemID {
		p.lines = append(p.lines, session.OutputLine{
			Timestamp: ts,
			Type:      session.OutputTypeText,
			Content:   delta,
		})
		idx := len(p.lines) - 1
		p.threadActiveItem[threadID] = itemID
		p.itemTextLine[itemID] = idx
		return
	}

	if idx, ok := p.itemTextLine[itemID]; ok && idx >= 0 && idx < len(p.lines) {
		p.lines[idx].Content = appendStreamingDelta(p.lines[idx].Content, delta)
		return
	}

	p.appendOrAddText(ts, delta)
}

func (p *codexReplayParser) setFinalItemText(ts time.Time, threadID, itemID, text string) {
	if idx, ok := p.itemTextLine[itemID]; ok && idx >= 0 && idx < len(p.lines) {
		p.lines[idx].Content = text
	} else {
		p.lines = append(p.lines, session.OutputLine{
			Timestamp: ts,
			Type:      session.OutputTypeText,
			Content:   text,
		})
		p.itemTextLine[itemID] = len(p.lines) - 1
	}
	p.threadActiveItem[threadID] = ""
}

func (p *codexReplayParser) updateToolCompletion(end codex.ExecCommandEndMsg, ts time.Time) {
	idx, ok := p.toolLineIndex[end.CallID]
	if !ok || idx < 0 || idx >= len(p.lines) {
		return
	}
	line := p.lines[idx]
	if line.ToolInput == nil {
		line.ToolInput = map[string]interface{}{}
	}
	if _, ok := line.ToolInput["command"]; !ok {
		cmd := commandFromParsed(end.ParsedCmd, end.Command)
		if cmd != "" {
			line.ToolInput["command"] = cmd
		}
	}
	if _, ok := line.ToolInput["cwd"]; !ok && end.CWD != "" {
		line.ToolInput["cwd"] = end.CWD
	}
	line.ToolResult = map[string]interface{}{
		"stdout":      end.Stdout,
		"stderr":      end.Stderr,
		"exit_code":   end.ExitCode,
		"duration_ms": end.Duration.Secs*1000 + end.Duration.Nanos/1000000,
	}
	line.ToolState = session.ToolStateComplete
	line.IsError = end.ExitCode != 0
	if line.IsError {
		line.ToolState = session.ToolStateError
	}
	line.DurationMs = end.Duration.Secs*1000 + end.Duration.Nanos/1000000
	if line.DurationMs == 0 && !line.StartTime.IsZero() {
		line.DurationMs = ts.Sub(line.StartTime).Milliseconds()
	}
	p.lines[idx] = line
}

func (p *codexReplayParser) appendOrAddText(ts time.Time, text string) {
	if text == "" {
		return
	}
	if len(p.lines) > 0 && p.lines[len(p.lines)-1].Type == session.OutputTypeText {
		p.lines[len(p.lines)-1].Content = appendStreamingDelta(p.lines[len(p.lines)-1].Content, text)
		return
	}
	p.lines = append(p.lines, session.OutputLine{
		Timestamp: ts,
		Type:      session.OutputTypeText,
		Content:   text,
	})
}

func (p *codexReplayParser) appendOrAddThinking(ts time.Time, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if len(p.lines) > 0 && p.lines[len(p.lines)-1].Type == session.OutputTypeThinking {
		p.lines[len(p.lines)-1].Content = appendStreamingDelta(p.lines[len(p.lines)-1].Content, text)
		return
	}
	p.lines = append(p.lines, session.OutputLine{
		Timestamp: ts,
		Type:      session.OutputTypeThinking,
		Content:   text,
	})
}

func (p *codexReplayParser) appendThreadText(threadID, delta string) {
	if threadID == "" || delta == "" {
		return
	}
	b, ok := p.threadText[threadID]
	if !ok {
		b = &strings.Builder{}
		p.threadText[threadID] = b
	}
	b.WriteString(delta)
}

func firstTurnText(inputs []codex.UserInput) string {
	for i := range inputs {
		if strings.EqualFold(inputs[i].Type, "text") && strings.TrimSpace(inputs[i].Text) != "" {
			return inputs[i].Text
		}
	}
	return ""
}

func commandDisplay(begin codex.ExecCommandBeginMsg) string {
	return commandFromParsed(begin.ParsedCmd, begin.Command)
}

func commandFromParsed(parsed []codex.ParsedCmd, command []string) string {
	if len(parsed) > 0 && strings.TrimSpace(parsed[0].Cmd) != "" {
		return strings.TrimSpace(parsed[0].Cmd)
	}
	if len(command) > 0 {
		return strings.TrimSpace(strings.Join(command, " "))
	}
	return ""
}

func approvalKey(callID, command, reason string) string {
	callID = strings.TrimSpace(callID)
	if callID != "" {
		return callID
	}
	command = strings.TrimSpace(command)
	reason = strings.TrimSpace(reason)
	if command == "" && reason == "" {
		return ""
	}
	return command + "\n" + reason
}

func extractErrorMessage(raw json.RawMessage) string {
	var msg struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(raw, &msg); err == nil {
		if text := strings.TrimSpace(msg.Message); text != "" {
			return text
		}
		if text := strings.TrimSpace(msg.Type); text != "" {
			return text
		}
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "provider error"
	}
	return text
}

func parseTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t
	}
	return time.Now()
}

// appendStreamingDelta appends a new streaming delta while removing duplicated
// overlap between the end of the existing text and the start of the delta.
func appendStreamingDelta(existing, delta string) string {
	if existing == "" || delta == "" {
		return existing + delta
	}

	maxOverlap := len(existing)
	if len(delta) < maxOverlap {
		maxOverlap = len(delta)
	}

	for overlap := maxOverlap; overlap > 0; overlap-- {
		if existing[len(existing)-overlap:] == delta[:overlap] {
			return existing + delta[overlap:]
		}
	}

	return existing + delta
}

func compactReplayLines(lines []session.OutputLine) []session.OutputLine {
	out := make([]session.OutputLine, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := lines[i]

		if line.Type == session.OutputTypeTurnEnd {
			summary := fmt.Sprintf("T%d $%.4f", line.TurnNumber, line.CostUSD)
			if i+1 < len(lines) && lines[i+1].Type == session.OutputTypeStatus {
				if in, outTokens, ok := parseTokenSummary(lines[i+1].Content); ok {
					summary = fmt.Sprintf("T%d $%.4f in:%d out:%d", line.TurnNumber, line.CostUSD, in, outTokens)
					i++
				}
			}
			out = append(out, session.OutputLine{
				Timestamp: line.Timestamp,
				Type:      session.OutputTypeStatus,
				Content:   summary,
			})
			continue
		}

		if line.Type == session.OutputTypeStatus {
			if in, outTokens, ok := parseTokenSummary(line.Content); ok {
				line.Content = fmt.Sprintf("tok in:%d out:%d", in, outTokens)
			}
		}
		out = append(out, line)
	}
	return out
}

func parseTokenSummary(content string) (int, int, bool) {
	var in, out int
	n, err := fmt.Sscanf(strings.TrimSpace(content), "Tokens: %d input / %d output", &in, &out)
	if err != nil || n != 2 {
		return 0, 0, false
	}
	return in, out, true
}
