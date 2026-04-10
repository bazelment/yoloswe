package protocol

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// UnknownMessage is returned by ParseMessage when the "type" discriminator
// does not match any known MessageType. Consumers can inspect the raw JSON
// to handle forward-compatible additions to the protocol without dropping
// data silently.
type UnknownMessage struct {
	Type MessageType
	Raw  json.RawMessage
}

// MsgType returns the message type.
func (m UnknownMessage) MsgType() MessageType { return m.Type }

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

	case MessageTypeKeepAlive:
		var msg KeepAliveMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse keep alive message: %w", err)
		}
		return msg, nil

	case MessageTypeToolProgress:
		var msg ToolProgressMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse tool progress message: %w", err)
		}
		return msg, nil

	case MessageTypeToolUseSummary:
		var msg ToolUseSummaryMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse tool use summary message: %w", err)
		}
		return msg, nil

	case MessageTypeAuthStatus:
		var msg AuthStatusMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse auth status message: %w", err)
		}
		return msg, nil

	case MessageTypeRateLimitEvent:
		var msg RateLimitEventMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse rate limit event message: %w", err)
		}
		return msg, nil

	case MessageTypePromptSuggestion:
		var msg PromptSuggestionMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse prompt suggestion message: %w", err)
		}
		return msg, nil

	case MessageTypeStreamlinedText:
		var msg StreamlinedTextMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse streamlined text message: %w", err)
		}
		return msg, nil

	case MessageTypeStreamlinedToolUseSummary:
		var msg StreamlinedToolUseSummaryMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse streamlined tool use summary message: %w", err)
		}
		return msg, nil

	case MessageTypeControlCancelRequest:
		var msg ControlCancelRequest
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse control cancel request: %w", err)
		}
		return msg, nil

	case MessageTypeUpdateEnvironmentVariables:
		var msg UpdateEnvironmentVariablesMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, fmt.Errorf("failed to parse update environment variables message: %w", err)
		}
		return msg, nil

	default:
		slog.Debug("unknown protocol message type — preserving raw", "type", base.Type)
		return UnknownMessage{
			Type: base.Type,
			Raw:  json.RawMessage(append([]byte(nil), data...)),
		}, nil
	}
}
