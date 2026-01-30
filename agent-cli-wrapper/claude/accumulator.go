package claude

import (
	"encoding/json"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// streamAccumulator accumulates streaming events into complete content blocks.
type streamAccumulator struct {
	session *Session

	// Current block states by index
	blocks map[int]*blockState
}

// blockState tracks the state of a content block being accumulated.
type blockState struct {
	blockType   string
	text        string
	thinking    string
	toolID      string
	toolName    string
	partialJSON string
	index       int
}

// newStreamAccumulator creates a new stream accumulator.
func newStreamAccumulator(session *Session) *streamAccumulator {
	return &streamAccumulator{
		session: session,
		blocks:  make(map[int]*blockState),
	}
}

// HandleEvent processes a stream event.
func (sa *streamAccumulator) HandleEvent(event protocol.StreamEvent) {
	// Parse the inner event
	eventData, err := protocol.ParseStreamEvent(event.Event)
	if err != nil {
		sa.session.emitError(err, "parse_stream_event")
		return
	}

	if eventData == nil {
		// Unknown event type, ignore
		return
	}

	switch e := eventData.(type) {
	case protocol.MessageStartEvent:
		sa.handleMessageStart(e)
	case protocol.ContentBlockStartEvent:
		sa.handleContentBlockStart(e)
	case protocol.ContentBlockDeltaEvent:
		sa.handleContentBlockDelta(e)
	case protocol.ContentBlockStopEvent:
		sa.handleContentBlockStop(e)
	case protocol.MessageDeltaEvent:
		sa.handleMessageDelta(e)
	case protocol.MessageStopEvent:
		sa.handleMessageStop(e)
	}
}

func (sa *streamAccumulator) handleMessageStart(e protocol.MessageStartEvent) {
	// Reset block states for new message
	sa.blocks = make(map[int]*blockState)
}

func (sa *streamAccumulator) handleContentBlockStart(e protocol.ContentBlockStartEvent) {
	// Parse the content block to get type
	var base struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(e.ContentBlock, &base); err != nil {
		sa.session.emitError(err, "parse_content_block_start")
		return
	}

	// Create block state
	state := &blockState{
		index:     e.Index,
		blockType: base.Type,
	}

	if base.Type == "tool_use" {
		state.toolID = base.ID
		state.toolName = base.Name

		// Emit tool start event
		turn := sa.session.turnManager.CurrentTurn()
		if turn != nil {
			// Register tool in turn state
			sa.session.turnManager.GetOrCreateTool(base.ID, base.Name)

			sa.session.emit(ToolStartEvent{
				TurnNumber: sa.session.turnManager.CurrentTurnNumber(),
				ID:         base.ID,
				Name:       base.Name,
				Timestamp:  time.Now(),
			})
		}
	}

	sa.blocks[e.Index] = state
}

func (sa *streamAccumulator) handleContentBlockDelta(e protocol.ContentBlockDeltaEvent) {
	state, exists := sa.blocks[e.Index]
	if !exists {
		return
	}

	// Parse the delta
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
		state.text += delta.Text

		// Emit text event
		fullText := sa.session.turnManager.AppendText(delta.Text)
		sa.session.emit(TextEvent{
			TurnNumber: sa.session.turnManager.CurrentTurnNumber(),
			Text:       delta.Text,
			FullText:   fullText,
		})

	case "thinking_delta":
		var delta protocol.ThinkingDelta
		if err := json.Unmarshal(e.Delta, &delta); err != nil {
			return
		}
		state.thinking += delta.Thinking

		// Emit thinking event
		fullThinking := sa.session.turnManager.AppendThinking(delta.Thinking)
		sa.session.emit(ThinkingEvent{
			TurnNumber:   sa.session.turnManager.CurrentTurnNumber(),
			Thinking:     delta.Thinking,
			FullThinking: fullThinking,
		})

	case "input_json_delta":
		var delta protocol.InputJSONDelta
		if err := json.Unmarshal(e.Delta, &delta); err != nil {
			return
		}
		state.partialJSON += delta.PartialJSON

		// Update tool state
		tool := sa.session.turnManager.GetTool(state.toolID)
		if tool != nil {
			tool.PartialInput = state.partialJSON
		}

		// Emit tool progress event
		sa.session.emit(ToolProgressEvent{
			TurnNumber:   sa.session.turnManager.CurrentTurnNumber(),
			ID:           state.toolID,
			Name:         state.toolName,
			PartialInput: state.partialJSON,
			InputChunk:   delta.PartialJSON,
		})
	}
}

func (sa *streamAccumulator) handleContentBlockStop(e protocol.ContentBlockStopEvent) {
	state, exists := sa.blocks[e.Index]
	if !exists {
		return
	}

	// For tool_use blocks, parse the complete input and emit tool complete
	if state.blockType == "tool_use" {
		var input map[string]interface{}

		// Parse JSON if present, otherwise use empty map
		if state.partialJSON != "" {
			if err := json.Unmarshal([]byte(state.partialJSON), &input); err != nil {
				input = make(map[string]interface{})
			}
		} else {
			input = make(map[string]interface{})
		}

		// Update tool state
		tool := sa.session.turnManager.GetTool(state.toolID)
		if tool != nil {
			tool.Input = input
		}

		// Emit tool complete event
		sa.session.emit(ToolCompleteEvent{
			TurnNumber: sa.session.turnManager.CurrentTurnNumber(),
			ID:         state.toolID,
			Name:       state.toolName,
			Input:      input,
			Timestamp:  time.Now(),
		})
	}

	// Clean up block state
	delete(sa.blocks, e.Index)
}

func (sa *streamAccumulator) handleMessageDelta(e protocol.MessageDeltaEvent) {
	// Message delta contains usage info but we get that from ResultMessage
}

func (sa *streamAccumulator) handleMessageStop(e protocol.MessageStopEvent) {
	// Message complete, clean up
	sa.blocks = make(map[int]*blockState)
}

// Reset clears all accumulated state.
func (sa *streamAccumulator) Reset() {
	sa.blocks = make(map[int]*blockState)
}
