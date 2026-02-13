package agent

import "github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"

// bridgeEvents reads SDK events from a typed channel and forwards them to an
// EventHandler and/or AgentEvent channel. It replaces the provider-specific
// bridge functions (bridgeClaudeEvents, bridgeCodexEvents, bridgeACPEventsToChannel,
// bridgeACPEventsToHandler).
//
// Parameters:
//   - events: the SDK event channel (e.g., <-chan claude.Event).
//   - handler: optional EventHandler callback receiver (nil to skip).
//   - out: optional AgentEvent output channel (nil to skip channel sends).
//   - stop: optional stop signal channel (nil means no stop signal; exits on events close).
//   - scopeID: if non-empty, events implementing agentstream.Scoped are filtered
//     to match this scope (e.g., codex thread ID).
//   - onTurnComplete: optional callback invoked once on the first TurnComplete event
//     (e.g., codex's turnDone channel pattern). Nil to skip.
func bridgeEvents[E any](
	events <-chan E,
	handler EventHandler,
	out chan<- AgentEvent,
	stop <-chan struct{},
	scopeID string,
	onTurnComplete func(),
) {
	if events == nil {
		return
	}

	turnCompleted := false

	for {
		select {
		case <-stop:
			if !turnCompleted && onTurnComplete != nil {
				onTurnComplete()
			}
			return
		case ev, ok := <-events:
			if !ok {
				if !turnCompleted && onTurnComplete != nil {
					onTurnComplete()
				}
				return
			}

			sev, ok := any(ev).(agentstream.Event)
			if !ok {
				continue
			}

			kind := sev.StreamEventKind()
			if kind == agentstream.KindUnknown {
				continue
			}

			// Scope filtering for multiplexed channels (e.g., codex thread ID).
			if scopeID != "" {
				if scoped, ok := any(ev).(agentstream.Scoped); ok {
					if id := scoped.ScopeID(); id != "" && id != scopeID {
						continue
					}
				}
			}

			switch kind {
			case agentstream.KindText:
				te := sev.(agentstream.Text)
				delta := te.StreamDelta()
				if handler != nil {
					handler.OnText(delta)
				}
				if out != nil {
					select {
					case out <- TextAgentEvent{Text: delta}:
					default:
					}
				}

			case agentstream.KindThinking:
				te := sev.(agentstream.Text)
				delta := te.StreamDelta()
				if handler != nil {
					handler.OnThinking(delta)
				}
				if out != nil {
					select {
					case out <- ThinkingAgentEvent{Thinking: delta}:
					default:
					}
				}

			case agentstream.KindToolStart:
				ts := sev.(agentstream.ToolStart)
				name := ts.StreamToolName()
				callID := ts.StreamToolCallID()
				input := ts.StreamToolInput()
				if handler != nil {
					handler.OnToolStart(name, callID, input)
				}
				if out != nil {
					select {
					case out <- ToolStartAgentEvent{Name: name, ID: callID, Input: input}:
					default:
					}
				}

			case agentstream.KindToolEnd:
				te := sev.(agentstream.ToolEnd)
				name := te.StreamToolName()
				callID := te.StreamToolCallID()
				input := te.StreamToolInput()
				result := te.StreamToolResult()
				isError := te.StreamToolIsError()
				if handler != nil {
					handler.OnToolComplete(name, callID, input, result, isError)
				}
				if out != nil {
					select {
					case out <- ToolCompleteAgentEvent{
						Name:    name,
						ID:      callID,
						Input:   input,
						Result:  result,
						IsError: isError,
					}:
					default:
					}
				}

			case agentstream.KindTurnComplete:
				tc := sev.(agentstream.TurnComplete)
				turnNum := tc.StreamTurnNum()
				success := tc.StreamIsSuccess()
				duration := tc.StreamDuration()
				cost := tc.StreamCost()
				if handler != nil {
					handler.OnTurnComplete(turnNum, success, duration, cost)
				}
				if out != nil {
					select {
					case out <- TurnCompleteAgentEvent{
						TurnNumber: turnNum,
						Success:    success,
						DurationMs: duration,
						CostUSD:    cost,
					}:
					default:
					}
				}
				if !turnCompleted && onTurnComplete != nil {
					onTurnComplete()
					turnCompleted = true
				}

			case agentstream.KindError:
				ee := sev.(agentstream.Error)
				err := ee.StreamErr()
				ctx := ee.StreamErrorContext()
				if handler != nil {
					handler.OnError(err, ctx)
				}
				if out != nil {
					select {
					case out <- ErrorAgentEvent{Err: err, Context: ctx}:
					default:
					}
				}
			}
		}
	}
}
