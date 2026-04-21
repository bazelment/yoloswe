package claude

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// drainEvents non-blockingly drains the session's event channel into a slice.
// Used by replay tests that synchronously push CLI protocol frames into
// handleLine and immediately inspect the resulting events.
func drainEvents(t *testing.T, s *Session) []Event {
	t.Helper()
	var out []Event
	for {
		select {
		case ev := <-s.events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// countEvents returns how many events of the given concrete type exist in
// the slice. Used as a coarse assertion that the raw-stream API emits one
// envelope per CLI message without coalescing or dropping.
func countEvents[T Event](events []Event) int {
	n := 0
	for _, e := range events {
		if _, ok := e.(T); ok {
			n++
		}
	}
	return n
}

// TestStream_Replay_RawEventsAlwaysEmitted is the PR-1 smoke test for the new
// raw-streaming API (Query + AssistantMessageEvent + ResultMessageEvent +
// UserMessageEvent). Feeds a minimal three-line CLI protocol sequence into
// handleLine — assistant text, user tool_result (simulating a CLI-executed
// tool), and ResultMessage — and asserts the raw events are present
// regardless of any wrapper-level coalescing.
func TestStream_Replay_RawEventsAlwaysEmitted(t *testing.T) {
	s := newTestSession(t)
	// Start a turn so handleAssistant/handleUser see a current turn number.
	s.turnManager.StartTurn("hello")

	// Assistant message with a single text block.
	assistantLine := []byte(`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","content":[{"type":"text","text":"hi"}]},"session_id":"s1","uuid":"u1"}`)
	s.handleLine(assistantLine)

	// User message containing a tool_result (CLI auto-executed a tool).
	userLine := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]},"session_id":"s1","uuid":"u2"}`)
	s.handleLine(userLine)

	// ResultMessage — CLI's turn boundary.
	resultLine := []byte(`{"type":"result","subtype":"success","session_id":"s1","uuid":"u3","result":"hi","num_turns":1,"duration_ms":10,"duration_api_ms":5,"total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	s.handleLine(resultLine)

	events := drainEvents(t, s)

	// Each top-level CLI message produces exactly one raw envelope event,
	// independent of any existing TurnComplete coalescing path.
	require.Equal(t, 1, countEvents[AssistantMessageEvent](events),
		"exactly one AssistantMessageEvent per assistant CLI frame")
	require.Equal(t, 1, countEvents[UserMessageEvent](events),
		"exactly one UserMessageEvent per user CLI frame")
	require.Equal(t, 1, countEvents[ResultMessageEvent](events),
		"exactly one ResultMessageEvent per result CLI frame")

	// Order check: raw envelopes fire in protocol order (assistant → user →
	// result). Coalesced events (TextEvent, CLIToolResultEvent,
	// TurnCompleteEvent) can still interleave — we only assert the raw
	// envelopes' relative order here.
	var seen []EventType
	for _, e := range events {
		switch e.(type) {
		case AssistantMessageEvent, UserMessageEvent, ResultMessageEvent:
			seen = append(seen, e.Type())
		}
	}
	require.Equal(t, []EventType{
		EventTypeAssistantMessage,
		EventTypeUserMessage,
		EventTypeResultMessage,
	}, seen, "raw envelopes preserve protocol order")
}

// TestStream_Replay_ResultMessageEvent_Subtype asserts ResultMessageEvent
// carries subtype and IsError faithfully. Consumers use these to
// distinguish success vs error vs max-turns-exceeded vs bg-auto-continued
// turns without running wrapper-level suppression.
func TestStream_Replay_ResultMessageEvent_Subtype(t *testing.T) {
	s := newTestSession(t)
	s.turnManager.StartTurn("prompt")

	errorLine := []byte(`{"type":"result","subtype":"error_max_turns","session_id":"s1","uuid":"u_err","is_error":true,"errors":["max turns exceeded"],"num_turns":3,"duration_ms":1000,"duration_api_ms":800,"total_cost_usd":0.05,"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	s.handleLine(errorLine)

	events := drainEvents(t, s)
	var rm *ResultMessageEvent
	for _, e := range events {
		if got, ok := e.(ResultMessageEvent); ok {
			rm = &got
			break
		}
	}
	require.NotNil(t, rm, "ResultMessageEvent must fire for error turns")
	require.Equal(t, "error_max_turns", rm.Subtype)
	require.True(t, rm.IsError)
	require.Error(t, rm.Error, "Error field populated for error turns")
	require.Equal(t, 3, rm.NumTurns)
	require.Equal(t, int64(1000), rm.DurationMs)
	require.InDelta(t, 0.05, rm.TotalCostUSD, 1e-9)
}
