package protocol

import "encoding/json"

// StreamEvent wraps streaming updates.
type StreamEvent struct {
	Type            MessageType     `json:"type"`
	Event           json.RawMessage `json:"event"`
	SessionID       string          `json:"session_id"`
	UUID            string          `json:"uuid"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
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
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
}

// EventType returns the stream event type.
func (e ContentBlockStartEvent) EventType() StreamEventType { return StreamEventTypeContentBlockStart }

// ContentBlockDeltaEvent contains incremental content.
type ContentBlockDeltaEvent struct {
	Type  StreamEventType `json:"type"`
	Index int             `json:"index"`
	Delta json.RawMessage `json:"delta"`
}

// EventType returns the stream event type.
func (e ContentBlockDeltaEvent) EventType() StreamEventType { return StreamEventTypeContentBlockDelta }

// TextDelta is a delta containing text.
type TextDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ThinkingDelta is a delta containing thinking.
type ThinkingDelta struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

// InputJSONDelta is a delta containing partial JSON for tool input.
type InputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

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
		// Unknown event type, return nil without error for forward compatibility
		return nil, nil
	}
}
