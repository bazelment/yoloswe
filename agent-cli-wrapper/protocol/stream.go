package protocol

import (
	"encoding/json"
	"log/slog"
)

// StreamEvent wraps streaming updates.
type StreamEvent struct {
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	Type            MessageType     `json:"type"`
	SessionID       string          `json:"session_id"`
	UUID            string          `json:"uuid"`
	Event           json.RawMessage `json:"event"`
}

// MsgType returns the message type.
func (m StreamEvent) MsgType() MessageType { return MessageTypeStreamEvent }

// StreamEventType discriminates between stream event kinds.
type StreamEventType string

const (
	StreamEventTypeMessageStart      StreamEventType = "message_start"
	StreamEventTypeContentBlockStart StreamEventType = "content_block_start"
	StreamEventTypeContentBlockDelta StreamEventType = "content_block_delta"
	StreamEventTypeContentBlockStop  StreamEventType = "content_block_stop"
	StreamEventTypeMessageDelta      StreamEventType = "message_delta"
	StreamEventTypeMessageStop       StreamEventType = "message_stop"
)

// StreamEventData is the interface for stream event discrimination.
type StreamEventData interface {
	EventType() StreamEventType
}

// MessageStartEvent starts a new message.
type MessageStartEvent struct {
	Type    StreamEventType `json:"type"`
	Message MessageContent  `json:"message"`
}

// EventType returns the stream event type.
func (e MessageStartEvent) EventType() StreamEventType { return StreamEventTypeMessageStart }

// ContentBlockStartEvent starts a content block.
type ContentBlockStartEvent struct {
	Type         StreamEventType `json:"type"`
	ContentBlock json.RawMessage `json:"content_block"`
	Index        int             `json:"index"`
}

// EventType returns the stream event type.
func (e ContentBlockStartEvent) EventType() StreamEventType { return StreamEventTypeContentBlockStart }

// ContentBlockDeltaEvent contains incremental content.
type ContentBlockDeltaEvent struct {
	Type  StreamEventType `json:"type"`
	Delta json.RawMessage `json:"delta"`
	Index int             `json:"index"`
}

// EventType returns the stream event type.
func (e ContentBlockDeltaEvent) EventType() StreamEventType { return StreamEventTypeContentBlockDelta }

// DeltaData is the interface for content block delta discrimination.
type DeltaData interface {
	DeltaType() string
}

// TextDelta is a delta containing text.
type TextDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// DeltaType returns the delta type.
func (d TextDelta) DeltaType() string { return d.Type }

// ThinkingDelta is a delta containing thinking.
type ThinkingDelta struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

// DeltaType returns the delta type.
func (d ThinkingDelta) DeltaType() string { return d.Type }

// InputJSONDelta is a delta containing partial JSON for tool input.
type InputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

// DeltaType returns the delta type.
func (d InputJSONDelta) DeltaType() string { return d.Type }

// ContentBlockStopEvent marks block completion.
type ContentBlockStopEvent struct {
	Type  StreamEventType `json:"type"`
	Index int             `json:"index"`
}

// EventType returns the stream event type.
func (e ContentBlockStopEvent) EventType() StreamEventType { return StreamEventTypeContentBlockStop }

// MessageDelta contains message metadata updates.
type MessageDelta struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

// MessageDeltaEvent updates message metadata.
type MessageDeltaEvent struct {
	Type  StreamEventType `json:"type"`
	Delta MessageDelta    `json:"delta"`
	Usage Usage           `json:"usage"`
}

// EventType returns the stream event type.
func (e MessageDeltaEvent) EventType() StreamEventType { return StreamEventTypeMessageDelta }

// MessageStopEvent marks message completion.
type MessageStopEvent struct {
	Type StreamEventType `json:"type"`
}

// EventType returns the stream event type.
func (e MessageStopEvent) EventType() StreamEventType { return StreamEventTypeMessageStop }

// ParseContentBlockDelta parses the inner delta from a ContentBlockDeltaEvent.
func ParseContentBlockDelta(data json.RawMessage) (DeltaData, error) {
	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &base); err != nil {
		return nil, err
	}

	switch base.Type {
	case "text_delta":
		var d TextDelta
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, err
		}
		return d, nil
	case "thinking_delta":
		var d ThinkingDelta
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, err
		}
		return d, nil
	case "input_json_delta":
		var d InputJSONDelta
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, err
		}
		return d, nil
	default:
		slog.Warn("skipping unknown content block delta type", "type", base.Type)
		return nil, nil
	}
}

// ParsedBlock parses the content_block field of a ContentBlockStartEvent.
func (e ContentBlockStartEvent) ParsedBlock() (ContentBlock, error) {
	return UnmarshalContentBlock(e.ContentBlock)
}

// ParsedDelta parses the delta field of a ContentBlockDeltaEvent.
func (e ContentBlockDeltaEvent) ParsedDelta() (DeltaData, error) {
	return ParseContentBlockDelta(e.Delta)
}

// ParseStreamEvent parses the inner event from a StreamEvent.
func ParseStreamEvent(data json.RawMessage) (StreamEventData, error) {
	var base struct {
		Type StreamEventType `json:"type"`
	}
	if err := json.Unmarshal(data, &base); err != nil {
		return nil, err
	}

	switch base.Type {
	case StreamEventTypeMessageStart:
		var e MessageStartEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return e, nil
	case StreamEventTypeContentBlockStart:
		var e ContentBlockStartEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return e, nil
	case StreamEventTypeContentBlockDelta:
		var e ContentBlockDeltaEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return e, nil
	case StreamEventTypeContentBlockStop:
		var e ContentBlockStopEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return e, nil
	case StreamEventTypeMessageDelta:
		var e MessageDeltaEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return e, nil
	case StreamEventTypeMessageStop:
		var e MessageStopEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		return e, nil
	default:
		slog.Warn("skipping unknown stream event type", "type", base.Type)
		return nil, nil
	}
}
