package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// Session represents an active ACP conversation session.
type Session struct {
	client          *Client
	state           *sessionStateManager
	turnDone        chan *TurnResult
	id              string
	text            strings.Builder
	thinking        strings.Builder
	mu              sync.Mutex
	sawToolActivity bool // set when tool_call or tool_call_update is received
}

// TurnResult contains the result of a completed prompt turn.
type TurnResult struct {
	Error      error
	FullText   string
	Thinking   string
	StopReason string // "endTurn", "cancelled", "error", "maxTokens"
	DurationMs int64
	Success    bool
}

func newSession(client *Client, id string) *Session {
	s := &Session{
		client: client,
		id:     id,
		state:  newSessionStateManager(),
	}
	_ = s.state.SetReady()
	return s
}

// ID returns the session ID.
func (s *Session) ID() string {
	return s.id
}

// State returns the current session state.
func (s *Session) State() SessionState {
	return s.state.Current()
}

// Prompt sends a text prompt and waits for the turn to complete.
func (s *Session) Prompt(ctx context.Context, text string) (*TurnResult, error) {
	s.mu.Lock()
	if s.state.IsClosed() {
		s.mu.Unlock()
		return nil, ErrClientClosed
	}
	if !s.state.IsReady() {
		s.mu.Unlock()
		return nil, ErrInvalidState
	}

	if err := s.state.SetProcessing(); err != nil {
		s.mu.Unlock()
		return nil, err
	}

	// Reset accumulators
	s.text.Reset()
	s.thinking.Reset()
	s.sawToolActivity = false
	s.turnDone = make(chan *TurnResult, 1)
	s.mu.Unlock()

	start := time.Now()

	// Send prompt request
	params := PromptRequest{
		SessionID: s.id,
		Prompt:    []ContentBlock{NewTextContent(text)},
	}

	resp, err := s.client.sendRequestAndWait(ctx, MethodSessionPrompt, params)
	durationMs := time.Since(start).Milliseconds()

	if err != nil {
		// Gemini CLI returns RPC error 500 "Model stream ended with empty
		// response text." when the model completes tool calls but produces no
		// final text. The tool calls succeeded and session updates were
		// streamed, so treat this as a successful turn when we have
		// accumulated content from the stream.
		if s.isRecoverablePromptError(err) {
			return s.completeTurn("endTurn", durationMs), nil
		}

		_ = s.state.SetReady()

		result := &TurnResult{
			Error:      err,
			DurationMs: durationMs,
			Success:    false,
		}
		s.signalTurnDone(result)

		return result, err
	}

	// Parse response
	var promptResp PromptResponse
	if resp.Result != nil {
		if jsonErr := json.Unmarshal(resp.Result, &promptResp); jsonErr != nil {
			_ = s.state.SetReady()
			return nil, &ProtocolError{Message: "failed to parse prompt response", Cause: jsonErr}
		}
	}

	return s.completeTurn(promptResp.StopReason, durationMs), nil
}

// Cancel sends a cancel notification for the current prompt.
func (s *Session) Cancel() error {
	return s.client.sendNotification(MethodSessionCancel, CancelNotification{
		SessionID: s.id,
	})
}

// handleUpdate processes a session/update notification from the agent.
// Called by the client's readLoop.
func (s *Session) handleUpdate(update *SessionUpdate) {
	switch update.Type {
	case UpdateTypeAgentMessage:
		if update.Content != nil && update.Content.Type == "text" {
			s.mu.Lock()
			s.text.WriteString(update.Content.Text)
			s.mu.Unlock()
		}
	case UpdateTypeAgentThought:
		if update.Content != nil && update.Content.Type == "text" {
			s.mu.Lock()
			s.thinking.WriteString(update.Content.Text)
			s.mu.Unlock()
		}
	case UpdateTypeToolCall, UpdateTypeToolCallUpdate:
		s.mu.Lock()
		s.sawToolActivity = true
		s.mu.Unlock()
	}
	// Other update types (plan_update, etc.) are handled by
	// the client's event emission in handleSessionUpdate.
}

// completeTurn builds a TurnResult from accumulated session updates, signals
// the turnDone channel, transitions state to ready, and emits a TurnComplete
// event. Used by both the normal success path and the recovery path.
func (s *Session) completeTurn(stopReason string, durationMs int64) *TurnResult {
	s.mu.Lock()
	result := &TurnResult{
		FullText:   s.text.String(),
		Thinking:   s.thinking.String(),
		StopReason: stopReason,
		DurationMs: durationMs,
		Success:    stopReason == "endTurn" || stopReason == "end_turn" || stopReason == "",
	}
	s.signalTurnDoneLocked(result)
	s.mu.Unlock()

	_ = s.state.SetReady()

	s.client.emit(TurnCompleteEvent{
		SessionID:  s.id,
		FullText:   result.FullText,
		Thinking:   result.Thinking,
		StopReason: result.StopReason,
		DurationMs: durationMs,
		Success:    result.Success,
	})

	return result
}

// signalTurnDone acquires mu and sends the result to the turnDone channel.
func (s *Session) signalTurnDone(result *TurnResult) {
	s.mu.Lock()
	s.signalTurnDoneLocked(result)
	s.mu.Unlock()
}

// signalTurnDoneLocked sends the result to the turnDone channel.
// Caller must hold s.mu.
func (s *Session) signalTurnDoneLocked(result *TurnResult) {
	if s.turnDone != nil {
		select {
		case s.turnDone <- result:
		default:
		}
	}
}

// isRecoverablePromptError returns true when the prompt RPC error can be
// treated as a successful turn because tool calls already executed and
// session updates were streamed. The Gemini CLI returns RPC error 500
// "Model stream ended with empty response text." in this scenario.
func (s *Session) isRecoverablePromptError(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	if rpcErr.Code != 500 || !strings.Contains(rpcErr.Message, "empty response text") {
		return false
	}
	// Only recover when session updates confirm activity occurred during
	// this turn (text/thinking streamed or tool calls executed).
	s.mu.Lock()
	hasActivity := s.text.Len() > 0 || s.thinking.Len() > 0 || s.sawToolActivity
	s.mu.Unlock()
	return hasActivity
}

// close marks the session as closed.
func (s *Session) close() {
	s.state.SetClosed()
}
