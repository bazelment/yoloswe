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
	// UsageIsCumulative is set on MappedEventTokenUsage when the source
	// notification only carried TotalTokenUsage (cumulative across the
	// thread), not the per-turn LastTokenUsage. Consumers that render
	// per-turn deltas (e.g. bramble/replay) must label or transform the
	// value rather than show it as a per-turn count.
	UsageIsCumulative bool
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
		errMsg := extractErrorMessage(notif.Turn.Error)
		return MappedEvent{
			Kind:     MappedEventTurnCompleted,
			ThreadID: notif.ThreadID,
			TurnID:   notif.Turn.ID,
			Success:  notif.Turn.Status == "completed",
			Error:    classifyTurnError(notif.ThreadID, notif.Turn.ID, errMsg),
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
		src := msg.Info.PreferredUsage()
		if src == nil {
			return MappedEvent{}, false
		}
		// Cumulative when PreferredUsage fell back to TotalTokenUsage.
		// Use pointer identity rather than `LastTokenUsage == nil` —
		// PreferredUsage also treats a non-nil but all-zero
		// LastTokenUsage (`{}` on the wire) as absent and returns
		// TotalTokenUsage; the two checks must stay in lockstep, or
		// replay would skip baseline subtraction on cumulative data.
		cumulative := msg.Info != nil && src == msg.Info.TotalTokenUsage
		usage := TurnUsage{
			InputTokens:           src.InputTokens,
			CachedInputTokens:     src.CachedInputTokens,
			OutputTokens:          src.OutputTokens,
			ReasoningOutputTokens: src.ReasoningOutputTokens,
			TotalTokens:           src.TotalTokens,
		}
		return MappedEvent{
			Kind:              MappedEventTokenUsage,
			ThreadID:          notif.ConversationID,
			Usage:             usage,
			UsageIsCumulative: cumulative,
		}, true
	}

	return MappedEvent{}, false
}

// TurnNumberFromID derives a 1-based display turn number from a Codex turn
// ID. It is a fallback only — the authoritative number is the client's
// monotonic per-thread counter carried on TurnCompletedEvent.TurnIndex.
//
// Recognised forms:
//   - a pure non-negative integer, interpreted as a 0-based index ("2" → 3);
//   - a "turn-N" / "turn_N" shaped string ("turn-42" → 42).
//
// Any other shape — notably an opaque UUID — returns 1 rather than scraping
// trailing digits, which would otherwise mistake a UUID tail (e.g. the
// "...9926" of "9671fa59a926") for a turn number.
func TurnNumberFromID(turnID string) int {
	normalized := strings.TrimSpace(turnID)
	if normalized == "" {
		return 1
	}

	if n, err := strconv.Atoi(normalized); err == nil && n >= 0 {
		return n + 1
	}

	for _, prefix := range []string{"turn-", "turn_"} {
		if rest, ok := strings.CutPrefix(normalized, prefix); ok {
			if n, err := strconv.Atoi(rest); err == nil && n > 0 {
				return n
			}
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
