package protocol

import (
	"encoding/json"
	"fmt"
)

// ParseMessage parses a raw JSON line into a typed Message.
func ParseMessage(data []byte) (Message, error) {
	var base struct {
		Type MessageType `json:"type"`
	}
	if err := json.Unmarshal(data, &base); err != nil {
		return nil, fmt.Errorf("failed to parse message type: %w", err)
	}

	switch base.Type {
	case MessageTypeSystem:
		var msg SystemMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse system message: %w", err)
		}
		return msg, nil

	case MessageTypeAssistant:
		var msg AssistantMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse assistant message: %w", err)
		}
		return msg, nil

	case MessageTypeUser:
		var msg UserMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse user message: %w", err)
		}
		return msg, nil

	case MessageTypeResult:
		var msg ResultMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse result message: %w", err)
		}
		return msg, nil

	case MessageTypeStreamEvent:
		var msg StreamEvent
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse stream event: %w", err)
		}
		return msg, nil

	case MessageTypeControlRequest:
		var msg ControlRequest
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse control request: %w", err)
		}
		return msg, nil

	case MessageTypeControlResponse:
		var msg ControlResponse
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse control response: %w", err)
		}
		return msg, nil

	default:
		return nil, fmt.Errorf("unknown message type: %s", base.Type)
	}
}
