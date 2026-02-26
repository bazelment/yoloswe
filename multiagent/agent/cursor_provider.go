package agent

import (
	"context"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/cursor"
	"github.com/bazelment/yoloswe/wt"
)

// CursorProvider wraps the Cursor Agent SDK behind the Provider interface.
// Each Execute call creates a one-shot session (no persistent state).
type CursorProvider struct {
	events chan AgentEvent
}

// NewCursorProvider creates a new Cursor provider.
func NewCursorProvider() *CursorProvider {
	return &CursorProvider{
		events: make(chan AgentEvent, 100),
	}
}

func (p *CursorProvider) Name() string { return "cursor" }

func (p *CursorProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...ExecuteOption) (*AgentResult, error) {
	cfg := applyOptions(opts)

	// Build full prompt with worktree context
	fullPrompt := prompt
	if wtCtx != nil {
		fullPrompt = wtCtx.FormatForPrompt() + "\n\n" + prompt
	}

	// Build cursor session options
	var sessionOpts []cursor.SessionOption
	if cfg.Model != "" {
		sessionOpts = append(sessionOpts, cursor.WithModel(cfg.Model))
	}
	if cfg.WorkDir != "" {
		sessionOpts = append(sessionOpts, cursor.WithWorkDir(cfg.WorkDir))
	}

	// Create session
	session := cursor.NewSession(fullPrompt, sessionOpts...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}
	defer session.Stop()

	// Single consumer loop: collect result + dispatch to handler/channel
	var resultText strings.Builder
	var agentResult *AgentResult

	for evt := range session.Events() {
		// Forward event to handler and events channel via agentstream bridge
		p.dispatchEvent(evt, cfg.EventHandler)

		switch e := evt.(type) {
		case cursor.TextEvent:
			resultText.WriteString(e.Text)
		case cursor.TurnCompleteEvent:
			agentResult = &AgentResult{
				Text:       resultText.String(),
				Success:    e.Success,
				DurationMs: e.DurationMs,
			}
			if e.Error != nil {
				agentResult.Error = e.Error
			}
			return agentResult, nil
		case cursor.ErrorEvent:
			return nil, e.Error
		}
	}

	// Channel closed without result
	text := resultText.String()
	if text != "" {
		return &AgentResult{Text: text, Success: true}, nil
	}
	return nil, cursor.ErrSessionClosed
}

// dispatchEvent forwards a cursor event to the EventHandler and AgentEvent channel.
func (p *CursorProvider) dispatchEvent(evt cursor.Event, handler EventHandler) {
	sev, ok := any(evt).(agentstream.Event)
	if !ok {
		return
	}

	kind := sev.StreamEventKind()
	if kind == agentstream.KindUnknown {
		return
	}

	switch kind {
	case agentstream.KindText:
		te := sev.(agentstream.Text)
		delta := te.StreamDelta()
		if handler != nil {
			handler.OnText(delta)
		}
		select {
		case p.events <- TextAgentEvent{Text: delta}:
		default:
		}

	case agentstream.KindToolStart:
		ts := sev.(agentstream.ToolStart)
		name := ts.StreamToolName()
		callID := ts.StreamToolCallID()
		input := ts.StreamToolInput()
		if handler != nil {
			handler.OnToolStart(name, callID, input)
		}
		select {
		case p.events <- ToolStartAgentEvent{Name: name, ID: callID, Input: input}:
		default:
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
		select {
		case p.events <- ToolCompleteAgentEvent{Name: name, ID: callID, Input: input, Result: result, IsError: isError}:
		default:
		}

	case agentstream.KindTurnComplete:
		tc := sev.(agentstream.TurnComplete)
		if handler != nil {
			handler.OnTurnComplete(tc.StreamTurnNum(), tc.StreamIsSuccess(), tc.StreamDuration(), tc.StreamCost())
		}
		select {
		case p.events <- TurnCompleteAgentEvent{
			TurnNumber: tc.StreamTurnNum(),
			Success:    tc.StreamIsSuccess(),
			DurationMs: tc.StreamDuration(),
			CostUSD:    tc.StreamCost(),
		}:
		default:
		}

	case agentstream.KindError:
		ee := sev.(agentstream.Error)
		if handler != nil {
			handler.OnError(ee.StreamErr(), ee.StreamErrorContext())
		}
		select {
		case p.events <- ErrorAgentEvent{Err: ee.StreamErr(), Context: ee.StreamErrorContext()}:
		default:
		}
	}
}

func (p *CursorProvider) Events() <-chan AgentEvent { return p.events }

func (p *CursorProvider) Close() error {
	close(p.events)
	return nil
}
