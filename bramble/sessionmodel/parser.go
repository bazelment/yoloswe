package sessionmodel

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// MessageParser converts vocabulary-level protocol messages into SessionModel
// mutations. It is THE single entry point — all three wire formats (live
// NDJSON, SDK recorder, raw JSONL) funnel through HandleMessage after their
// envelope is stripped.
//
// Stream accumulation state (blockState) is ported from
// agent-cli-wrapper/claude/accumulator.go.
type MessageParser struct {
	model  *SessionModel
	blocks map[int]*blockState // stream accumulator state, keyed by content block index
}

// blockState tracks a content block being assembled from streaming deltas.
type blockState struct {
	blockType   string // "text", "thinking", "tool_use"
	toolID      string
	toolName    string
	partialJSON string
	index       int
}

// NewMessageParser creates a parser wired to the given model.
func NewMessageParser(model *SessionModel) *MessageParser {
	return &MessageParser{
		model:  model,
		blocks: make(map[int]*blockState),
	}
}

// HandleMessage processes a single vocabulary-level protocol message.
func (p *MessageParser) HandleMessage(msg protocol.Message) {
	switch m := msg.(type) {
	case protocol.SystemMessage:
		p.handleSystem(m)
	case protocol.AssistantMessage:
		p.handleAssistant(m)
	case protocol.UserMessage:
		p.handleUser(m)
	case protocol.ResultMessage:
		p.handleResult(m)
	case protocol.StreamEvent:
		p.handleStreamEvent(m)
	case protocol.ControlRequest:
		// Control requests are handled by the controller, not the model.
	case protocol.ControlResponse:
		// Control responses are handled by the controller, not the model.
	}
}

// --- system -----------------------------------------------------------------

func (p *MessageParser) handleSystem(msg protocol.SystemMessage) {
	if msg.Subtype == "init" {
		p.model.SetMeta(SessionMeta{
			SessionID:         msg.SessionID,
			Model:             msg.Model,
			CWD:               msg.CWD,
			ClaudeCodeVersion: msg.ClaudeCodeVersion,
			PermissionMode:    msg.PermissionMode,
			Tools:             msg.Tools,
			Agents:            msg.Agents,
			Skills:            msg.Skills,
			Status:            StatusRunning,
		})
	}
}

// --- assistant (complete message, e.g. from raw JSONL) ----------------------

func (p *MessageParser) handleAssistant(msg protocol.AssistantMessage) {
	blocks, ok := msg.Message.Content.AsBlocks()
	if !ok {
		// String content — treat as plain text.
		if s, ok := msg.Message.Content.AsString(); ok && s != "" {
			p.model.AppendOutput(OutputLine{
				Timestamp: time.Now(),
				Type:      OutputTypeText,
				Content:   s,
			})
		}
		return
	}

	for _, block := range blocks {
		switch b := block.(type) {
		case protocol.TextBlock:
			if b.Text != "" {
				p.model.AppendOutput(OutputLine{
					Timestamp: time.Now(),
					Type:      OutputTypeText,
					Content:   b.Text,
				})
			}
		case protocol.ThinkingBlock:
			if b.Thinking != "" {
				p.model.AppendOutput(OutputLine{
					Timestamp: time.Now(),
					Type:      OutputTypeThinking,
					Content:   b.Thinking,
				})
			}
		case protocol.ToolUseBlock:
			now := time.Now()
			p.model.AppendOutput(OutputLine{
				Timestamp: now,
				Type:      OutputTypeToolStart,
				Content:   FormatToolContent(b.Name, b.Input),
				ToolName:  b.Name,
				ToolID:    b.ID,
				ToolInput: b.Input,
				ToolState: ToolStateRunning,
				StartTime: now,
			})
		}
	}
}

// --- user (initial prompt or tool results echoed back) ----------------------

func (p *MessageParser) handleUser(msg protocol.UserMessage) {
	// String content is the initial user prompt (common in raw JSONL).
	// Mark it with IsUserPrompt so callers can distinguish user prompts from
	// assistant text — both use OutputTypeText.
	if s, ok := msg.Message.Content.AsString(); ok {
		if s != "" {
			p.model.AppendOutput(OutputLine{
				Timestamp:    time.Now(),
				Type:         OutputTypeText,
				Content:      s,
				IsUserPrompt: true,
			})
		}
		return
	}

	blocks, ok := msg.Message.Content.AsBlocks()
	if !ok {
		return
	}

	for _, block := range blocks {
		if tr, ok := block.(protocol.ToolResultBlock); ok {
			isError := tr.IsError != nil && *tr.IsError
			now := time.Now()
			p.model.UpdateTool(tr.ToolUseID, func(line *OutputLine) {
				line.ToolResult = tr.Content
				line.IsError = isError
				if isError {
					line.ToolState = ToolStateError
				} else {
					line.ToolState = ToolStateComplete
				}
				if !line.StartTime.IsZero() {
					line.DurationMs = now.Sub(line.StartTime).Milliseconds()
				}
			})
			// Clear progress indicator now that the tool result has arrived.
			p.model.UpdateProgress(func(prog *ProgressSnapshot) {
				prog.CurrentTool = ""
				prog.CurrentPhase = ""
			})
		}
	}
}

// --- result (turn completion metrics) ---------------------------------------

func (p *MessageParser) handleResult(msg protocol.ResultMessage) {
	p.model.UpdateProgress(func(prog *ProgressSnapshot) {
		prog.TurnCount = msg.NumTurns
		prog.TotalCostUSD = msg.TotalCostUSD
		prog.InputTokens = msg.Usage.InputTokens
		prog.OutputTokens = msg.Usage.OutputTokens
		prog.LastActivity = time.Now()
	})

	p.model.AppendOutput(OutputLine{
		Timestamp:  time.Now(),
		Type:       OutputTypeTurnEnd,
		Content:    fmt.Sprintf("Turn %d complete", msg.NumTurns),
		TurnNumber: msg.NumTurns,
		CostUSD:    msg.TotalCostUSD,
		DurationMs: msg.DurationMs,
		IsError:    msg.IsError,
	})

	// Transition the session to a terminal status now that the result is known.
	// These transitions are from StatusRunning so the guard should never fire.
	if msg.IsError {
		_ = p.model.UpdateStatus(StatusFailed)
	} else {
		_ = p.model.UpdateStatus(StatusCompleted)
	}
}

// --- stream_event (live streaming deltas) -----------------------------------

func (p *MessageParser) handleStreamEvent(se protocol.StreamEvent) {
	eventData, err := protocol.ParseStreamEvent(se.Event)
	if err != nil || eventData == nil {
		return
	}

	switch e := eventData.(type) {
	case protocol.MessageStartEvent:
		p.blocks = make(map[int]*blockState)

	case protocol.ContentBlockStartEvent:
		p.handleContentBlockStart(e)

	case protocol.ContentBlockDeltaEvent:
		p.handleContentBlockDelta(e)

	case protocol.ContentBlockStopEvent:
		p.handleContentBlockStop(e)

	case protocol.MessageDeltaEvent:
		// Usage comes from ResultMessage; nothing to do here.

	case protocol.MessageStopEvent:
		p.blocks = make(map[int]*blockState)
	}
}

func (p *MessageParser) handleContentBlockStart(e protocol.ContentBlockStartEvent) {
	var base struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(e.ContentBlock, &base); err != nil {
		return
	}

	state := &blockState{
		index:     e.Index,
		blockType: base.Type,
	}

	if base.Type == "tool_use" {
		state.toolID = base.ID
		state.toolName = base.Name

		now := time.Now()
		p.model.AppendOutput(OutputLine{
			Timestamp: now,
			Type:      OutputTypeToolStart,
			Content:   base.Name, // input not yet available
			ToolName:  base.Name,
			ToolID:    base.ID,
			ToolState: ToolStateRunning,
			StartTime: now,
		})

		p.model.UpdateProgress(func(prog *ProgressSnapshot) {
			prog.CurrentTool = base.Name
			prog.CurrentPhase = "tool_execution"
			prog.LastActivity = time.Now()
		})
	}

	p.blocks[e.Index] = state
}

func (p *MessageParser) handleContentBlockDelta(e protocol.ContentBlockDeltaEvent) {
	state, exists := p.blocks[e.Index]
	if !exists {
		return
	}

	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(e.Delta, &base); err != nil {
		return
	}

	switch base.Type {
	case "text_delta":
		var delta protocol.TextDelta
		if err := json.Unmarshal(e.Delta, &delta); err != nil {
			return
		}
		p.model.AppendStreamingText(delta.Text)

	case "thinking_delta":
		var delta protocol.ThinkingDelta
		if err := json.Unmarshal(e.Delta, &delta); err != nil {
			return
		}
		p.model.AppendStreamingThinking(delta.Thinking)

	case "input_json_delta":
		var delta protocol.InputJSONDelta
		if err := json.Unmarshal(e.Delta, &delta); err != nil {
			return
		}
		state.partialJSON += delta.PartialJSON
	}
}

func (p *MessageParser) handleContentBlockStop(e protocol.ContentBlockStopEvent) {
	state, exists := p.blocks[e.Index]
	if !exists {
		return
	}

	if state.blockType == "tool_use" {
		var input map[string]interface{}
		if state.partialJSON != "" {
			if err := json.Unmarshal([]byte(state.partialJSON), &input); err != nil {
				input = make(map[string]interface{})
			}
		} else {
			input = make(map[string]interface{})
		}

		// Update the tool_start line with the parsed input.
		// Note: CurrentTool/CurrentPhase are cleared when the tool result
		// arrives (handleUser ToolResultBlock), not here, so the progress
		// indicator stays visible until execution actually completes.
		p.model.UpdateTool(state.toolID, func(line *OutputLine) {
			line.ToolInput = input
			line.Content = FormatToolContent(state.toolName, input)
		})
	}

	delete(p.blocks, e.Index)
}

// PatchSessionID fills in the session ID from the outer envelope if the model
// metadata currently has an empty session ID. Raw JSONL stores the session ID
// in the outer envelope rather than the inner system{init} message.
// Delegates to SessionModel.PatchSessionID for an atomic check-and-update.
func (p *MessageParser) PatchSessionID(id string) {
	p.model.PatchSessionID(id)
}

// Reset clears all accumulated streaming state.
func (p *MessageParser) Reset() {
	p.blocks = make(map[int]*blockState)
}
