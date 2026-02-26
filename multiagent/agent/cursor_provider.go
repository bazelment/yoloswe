package agent

import (
	"context"
	"strings"

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
	// Cursor requires --trust for non-interactive use
	sessionOpts = append(sessionOpts, cursor.WithTrust())

	// Create session
	session := cursor.NewSession(fullPrompt, sessionOpts...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}
	defer session.Stop()

	// Tee session events: one copy for bridgeEvents (handler + AgentEvent channel),
	// one copy for local result collection. This avoids duplicating bridgeEvents logic.
	bridgeCh := make(chan cursor.Event, 100)
	bridgeStop := make(chan struct{})
	bridgeDone := make(chan struct{})
	go func() {
		bridgeEvents(bridgeCh, cfg.EventHandler, p.events, bridgeStop, "", nil)
		close(bridgeDone)
	}()
	defer func() {
		close(bridgeStop)
		<-bridgeDone
	}()
	defer close(bridgeCh)

	var resultText strings.Builder

	for evt := range session.Events() {
		// Forward to bridge goroutine
		select {
		case bridgeCh <- evt:
		default:
		}

		// Collect result locally
		switch e := evt.(type) {
		case cursor.TextEvent:
			resultText.WriteString(e.Text)
		case cursor.TurnCompleteEvent:
			agentResult := &AgentResult{
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

func (p *CursorProvider) Events() <-chan AgentEvent { return p.events }

func (p *CursorProvider) Close() error {
	close(p.events)
	return nil
}
