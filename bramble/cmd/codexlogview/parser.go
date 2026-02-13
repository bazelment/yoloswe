package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/bramble/session"
)

type codexReplay struct { //nolint:govet // fieldalignment: readability over packing
	lines  []session.OutputLine
	prompt string
	status session.SessionStatus
}

type codexReplayParser struct { //nolint:govet // fieldalignment: readability over packing
	lines            []session.OutputLine
	itemTextLine     map[string]int
	threadActiveItem map[string]string
	toolLineIndex    map[string]int
	threadTokenUsage map[string]codex.TokenUsage
	threadReasoning  map[string]bool
	threadText       map[string]*strings.Builder
	pendingApprovals map[string]map[string]struct{}
	emittedApprovals map[string]map[string]struct{}
	prompt           string
	turnCount        int
	turnStarts       int
	turnCompletions  int
	// hadProviderErrors marks explicit provider error events from the log.
	hadProviderErrors bool
}

type codexExecApprovalRequest struct { //nolint:govet // fieldalignment: readability over packing
	Command []string `json:"command"`
	CallID  string   `json:"call_id"`
	Reason  string   `json:"reason"`
}

type codexItemApprovalRequest struct {
	ThreadID string `json:"threadId"`
	ItemID   string `json:"itemId"`
	Reason   string `json:"reason"`
	Command  string `json:"command"`
}

func newCodexReplayParser() *codexReplayParser {
	return &codexReplayParser{
		itemTextLine:     make(map[string]int),
		threadActiveItem: make(map[string]string),
		toolLineIndex:    make(map[string]int),
		threadTokenUsage: make(map[string]codex.TokenUsage),
		threadReasoning:  make(map[string]bool),
		threadText:       make(map[string]*strings.Builder),
		pendingApprovals: make(map[string]map[string]struct{}),
		emittedApprovals: make(map[string]map[string]struct{}),
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

	if mapped, ok := codex.ParseMappedNotification(msg.Method, msg.Params); ok {
		p.handleMappedEvent(mapped, ts)
		return
	}

	switch msg.Method {
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
		var req codexExecApprovalRequest
		if err := json.Unmarshal(notif.Msg, &req); err != nil {
			return
		}
		p.recordApprovalRequest(
			ts,
			notif.ConversationID,
			req.CallID,
			strings.TrimSpace(strings.Join(req.Command, " ")),
			req.Reason,
		)

	case "item/commandExecution/requestApproval":
		var req codexItemApprovalRequest
		if err := json.Unmarshal(msg.Params, &req); err != nil {
			return
		}
		p.recordApprovalRequest(ts, req.ThreadID, req.ItemID, req.Command, req.Reason)
	}
}

func (p *codexReplayParser) handleMappedEvent(ev codex.MappedEvent, ts time.Time) {
	switch ev.Kind {
	case codex.MappedEventTextDelta:
		p.appendTextDelta(ts, ev.ThreadID, ev.ItemID, ev.Delta)
		p.appendThreadText(ev.ThreadID, ev.Delta)

	case codex.MappedEventReasoningDelta:
		p.threadReasoning[ev.ThreadID] = true
		p.appendOrAddThinking(ts, ev.Delta)

	case codex.MappedEventCommandStart:
		input := map[string]interface{}{}
		if ev.Command != "" {
			input["command"] = ev.Command
		}
		if ev.CWD != "" {
			input["cwd"] = ev.CWD
		}
		p.lines = append(p.lines, session.OutputLine{
			Timestamp: ts,
			Type:      session.OutputTypeToolStart,
			Content:   "Bash: " + ev.Command,
			ToolName:  "Bash",
			ToolID:    ev.CallID,
			ToolInput: input,
			ToolState: session.ToolStateRunning,
			StartTime: ts,
		})
		p.toolLineIndex[ev.CallID] = len(p.lines) - 1
		p.clearPendingApproval(ev.ThreadID, ev.CallID)

	case codex.MappedEventCommandEnd:
		p.updateToolCompletion(ev, ts)

	case codex.MappedEventTokenUsage:
		p.threadTokenUsage[ev.ThreadID] = codex.TokenUsage{
			InputTokens:           ev.Usage.InputTokens,
			CachedInputTokens:     ev.Usage.CachedInputTokens,
			OutputTokens:          ev.Usage.OutputTokens,
			ReasoningOutputTokens: ev.Usage.ReasoningOutputTokens,
			TotalTokens:           ev.Usage.TotalTokens,
		}

	case codex.MappedEventTurnCompleted:
		p.turnCount++
		p.turnCompletions++
		p.clearThreadApprovals(ev.ThreadID)
		usage := p.threadTokenUsage[ev.ThreadID]
		p.lines = append(p.lines, session.OutputLine{
			Timestamp:  ts,
			Type:       session.OutputTypeTurnEnd,
			Content:    "Turn complete",
			TurnNumber: p.turnCount,
			DurationMs: ev.DurationMs,
			CostUSD:    0,
		})
		if usage.InputTokens > 0 || usage.OutputTokens > 0 {
			p.lines = append(p.lines, session.OutputLine{
				Timestamp: ts,
				Type:      session.OutputTypeStatus,
				Content:   tokenSummaryContent(usage),
			})
		}
		p.threadText[ev.ThreadID] = &strings.Builder{}
		p.threadReasoning[ev.ThreadID] = false
		p.threadActiveItem[ev.ThreadID] = ""

	case codex.MappedEventError:
		content := "provider error"
		if ev.Error != nil {
			content = strings.TrimSpace(ev.Error.Error())
		}
		if content == "" {
			content = "provider error"
		}
		p.lines = append(p.lines, session.OutputLine{
			Timestamp: ts,
			Type:      session.OutputTypeError,
			Content:   content,
		})
		p.hadProviderErrors = true
	}
}

func (p *codexReplayParser) deriveStatus() session.SessionStatus {
	if p.hadProviderErrors {
		return session.StatusFailed
	}
	for _, byKey := range p.pendingApprovals {
		if len(byKey) > 0 {
			return session.StatusIdle
		}
	}
	if p.turnStarts > p.turnCompletions {
		return session.StatusRunning
	}
	return session.StatusCompleted
}

func (p *codexReplayParser) recordApprovalRequest(ts time.Time, threadID, callID, command, reason string) {
	key := approvalKey(callID, command, reason)
	if key == "" {
		return
	}
	threadID = normalizeThreadKey(threadID)
	if _, ok := p.emittedApprovals[threadID]; !ok {
		p.emittedApprovals[threadID] = make(map[string]struct{})
	}
	if _, ok := p.pendingApprovals[threadID]; !ok {
		p.pendingApprovals[threadID] = make(map[string]struct{})
	}
	if _, seen := p.emittedApprovals[threadID][key]; seen {
		return
	}
	p.emittedApprovals[threadID][key] = struct{}{}
	p.pendingApprovals[threadID][key] = struct{}{}

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

func (p *codexReplayParser) clearPendingApproval(threadID, callID string) {
	key := approvalKey(callID, "", "")
	if key == "" {
		return
	}
	threadID = normalizeThreadKey(threadID)
	if threadPending, ok := p.pendingApprovals[threadID]; ok {
		delete(threadPending, key)
		if len(threadPending) == 0 {
			delete(p.pendingApprovals, threadID)
		}
	}
}

func (p *codexReplayParser) clearThreadApprovals(threadID string) {
	threadID = normalizeThreadKey(threadID)
	delete(p.pendingApprovals, threadID)
	delete(p.emittedApprovals, threadID)
}

func normalizeThreadKey(threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return "_global"
	}
	return threadID
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

func (p *codexReplayParser) updateToolCompletion(ev codex.MappedEvent, ts time.Time) {
	idx, ok := p.toolLineIndex[ev.CallID]
	if !ok || idx < 0 || idx >= len(p.lines) {
		return
	}
	line := p.lines[idx]
	if line.ToolInput == nil {
		line.ToolInput = map[string]interface{}{}
	}
	if _, ok := line.ToolInput["command"]; !ok && ev.Command != "" {
		line.ToolInput["command"] = ev.Command
	}
	if _, ok := line.ToolInput["cwd"]; !ok && ev.CWD != "" {
		line.ToolInput["cwd"] = ev.CWD
	}
	line.ToolResult = map[string]interface{}{
		"stdout":      ev.Stdout,
		"stderr":      ev.Stderr,
		"exit_code":   ev.ExitCode,
		"duration_ms": ev.DurationMs,
	}
	line.ToolState = session.ToolStateComplete
	line.IsError = ev.ExitCode != 0
	if line.IsError {
		line.ToolState = session.ToolStateError
	}
	line.DurationMs = ev.DurationMs
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

func tokenSummaryContent(usage codex.TokenUsage) string {
	return fmt.Sprintf("Tokens: %d input / %d output", usage.InputTokens, usage.OutputTokens)
}
