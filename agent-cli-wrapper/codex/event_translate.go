package codex

import (
	"encoding/json"
	"strconv"
	"strings"
)

// MappedEventKind identifies a normalized Codex event shape used by downstream
// consumers (providers, replay tools) to avoid duplicate protocol mapping code.
type MappedEventKind int

const (
	MappedEventUnknown MappedEventKind = iota
	MappedEventTextDelta
	MappedEventReasoningDelta
	MappedEventCommandStart
	MappedEventCommandEnd
	MappedEventTurnCompleted
	MappedEventError
	MappedEventTokenUsage
)

// MappedEvent is a normalized Codex event.
type MappedEvent struct { //nolint:govet // fieldalignment: keep semantic grouping
	Error        error
	Kind         MappedEventKind
	ThreadID     string
	TurnID       string
	ItemID       string
	Delta        string
	CallID       string
	Command      string
	CWD          string
	Stdout       string
	Stderr       string
	ErrorContext string
	Usage        TurnUsage
	ExitCode     int
	DurationMs   int64
	Success      bool
}

// ParseMappedNotification parses a Codex protocol notification into a
// normalized event. Unknown or unsupported methods return (zero, false).
func ParseMappedNotification(method string, params json.RawMessage) (MappedEvent, bool) {
	switch method {
	case NotifyAgentMessageDelta:
		var notif AgentMessageDeltaNotification
		if err := json.Unmarshal(params, &notif); err != nil {
			return MappedEvent{}, false
		}
		return MappedEvent{
			Kind:     MappedEventTextDelta,
			ThreadID: notif.ThreadID,
			TurnID:   notif.TurnID,
			ItemID:   notif.ItemID,
			Delta:    notif.Delta,
		}, true

	case NotifyCodexEventReasoningDelta:
		var notif CodexEventNotification
		if err := json.Unmarshal(params, &notif); err != nil {
			return MappedEvent{}, false
		}
		var msg ReasoningDeltaMsg
		if err := json.Unmarshal(notif.Msg, &msg); err != nil {
			return MappedEvent{}, false
		}
		return MappedEvent{
			Kind:     MappedEventReasoningDelta,
			ThreadID: notif.ConversationID,
			Delta:    msg.Delta,
		}, true

	case NotifyCodexEventExecBegin:
		var notif CodexEventNotification
		if err := json.Unmarshal(params, &notif); err != nil {
			return MappedEvent{}, false
		}
		var msg ExecCommandBeginMsg
		if err := json.Unmarshal(notif.Msg, &msg); err != nil {
			return MappedEvent{}, false
		}
		return MappedEvent{
			Kind:     MappedEventCommandStart,
			ThreadID: notif.ConversationID,
			TurnID:   msg.TurnID,
			CallID:   msg.CallID,
			Command:  parsedCommandText(msg.ParsedCmd, msg.Command),
			CWD:      msg.CWD,
		}, true

	case NotifyCodexEventExecEnd:
		var notif CodexEventNotification
		if err := json.Unmarshal(params, &notif); err != nil {
			return MappedEvent{}, false
		}
		var msg ExecCommandEndMsg
		if err := json.Unmarshal(notif.Msg, &msg); err != nil {
			return MappedEvent{}, false
		}
		return MappedEvent{
			Kind:       MappedEventCommandEnd,
			ThreadID:   notif.ConversationID,
			TurnID:     msg.TurnID,
			CallID:     msg.CallID,
			Command:    parsedCommandText(msg.ParsedCmd, msg.Command),
			CWD:        msg.CWD,
			Stdout:     msg.Stdout,
			Stderr:     msg.Stderr,
			ExitCode:   msg.ExitCode,
			DurationMs: msg.Duration.Secs*1000 + msg.Duration.Nanos/1000000,
			Success:    msg.ExitCode == 0,
		}, true

	case NotifyTurnCompleted:
		var notif TurnCompletedNotification
		if err := json.Unmarshal(params, &notif); err != nil {
			return MappedEvent{}, false
		}
		return MappedEvent{
			Kind:     MappedEventTurnCompleted,
			ThreadID: notif.ThreadID,
			TurnID:   notif.Turn.ID,
			Success:  notif.Turn.Status == "completed",
		}, true

	case NotifyCodexEventError:
		var notif CodexEventNotification
		if err := json.Unmarshal(params, &notif); err != nil {
			return MappedEvent{}, false
		}
		return MappedEvent{
			Kind:         MappedEventError,
			ThreadID:     notif.ConversationID,
			Error:        codexProtocolError(notif.Msg),
			ErrorContext: "codex_event_error",
		}, true

	case NotifyCodexEventTokenCount:
		var notif CodexEventNotification
		if err := json.Unmarshal(params, &notif); err != nil {
			return MappedEvent{}, false
		}
		var msg TokenCountMsg
		if err := json.Unmarshal(notif.Msg, &msg); err != nil {
			return MappedEvent{}, false
		}
		if msg.Info == nil || msg.Info.TotalTokenUsage == nil {
			return MappedEvent{}, false
		}
		var usage TurnUsage
		if msg.Info.TotalTokenUsage != nil {
			usage = TurnUsage{
				InputTokens:           msg.Info.TotalTokenUsage.InputTokens,
				CachedInputTokens:     msg.Info.TotalTokenUsage.CachedInputTokens,
				OutputTokens:          msg.Info.TotalTokenUsage.OutputTokens,
				ReasoningOutputTokens: msg.Info.TotalTokenUsage.ReasoningOutputTokens,
				TotalTokens:           msg.Info.TotalTokenUsage.TotalTokens,
			}
		}
		return MappedEvent{
			Kind:     MappedEventTokenUsage,
			ThreadID: notif.ConversationID,
			Usage:    usage,
		}, true
	}

	return MappedEvent{}, false
}

// TurnNumberFromID converts Codex turn IDs to a 1-based display number.
// Numeric IDs are interpreted as 0-based turn indexes. String IDs like
// "turn-42" map to the trailing number directly.
func TurnNumberFromID(turnID string) int {
	normalized := strings.TrimSpace(turnID)
	if normalized == "" {
		return 1
	}

	if n, err := strconv.Atoi(normalized); err == nil && n >= 0 {
		return n + 1
	}

	last := len(normalized)
	first := last
	for first > 0 {
		ch := normalized[first-1]
		if ch < '0' || ch > '9' {
			break
		}
		first--
	}
	if first < last {
		if n, err := strconv.Atoi(normalized[first:last]); err == nil && n > 0 {
			return n
		}
	}

	return 1
}

func parsedCommandText(parsed []ParsedCmd, command []string) string {
	if len(parsed) > 0 && strings.TrimSpace(parsed[0].Cmd) != "" {
		return strings.TrimSpace(parsed[0].Cmd)
	}
	if len(command) > 0 {
		return strings.TrimSpace(strings.Join(command, " "))
	}
	return ""
}

func codexProtocolError(raw json.RawMessage) error {
	var msg struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(raw, &msg); err == nil {
		if text := strings.TrimSpace(msg.Message); text != "" {
			return &ProtocolError{Message: text}
		}
		if text := strings.TrimSpace(msg.Type); text != "" {
			return &ProtocolError{Message: text}
		}
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		text = "provider error"
	}
	return &ProtocolError{Message: text}
}
