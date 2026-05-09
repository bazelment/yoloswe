package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
	"github.com/bazelment/yoloswe/wt"
)

// GeminiProvider wraps an ACP client (Gemini CLI) behind the Provider interface.
//
//nolint:govet // fieldalignment: keep boundEndpt grouped with clientOpts; extra inline padding from nesting llmendpoint.Endpoint isn't worth obscuring the order.
type GeminiProvider struct {
	boundEndpt llmendpoint.Endpoint
	clientOpts []acp.ClientOption
	client     *acp.Client
	events     chan AgentEvent
	bridgeDone chan struct{} // signals bridge goroutine to exit
	// handlerCh is set during an Execute call. The bridge goroutine sends
	// copies of AgentEvents here for the handler. It is protected by handlerMu.
	handlerCh chan AgentEvent
	handlerMu sync.RWMutex
	bridgeWg  sync.WaitGroup // tracks bridge goroutine
	mu        sync.Mutex
}

// NewGeminiProvider creates a new Gemini provider.
// By default, it launches "gemini --experimental-acp". Use acp.WithBinaryPath
// and acp.WithBinaryArgs to customize.
func NewGeminiProvider(opts ...acp.ClientOption) *GeminiProvider {
	return &GeminiProvider{
		events:     make(chan AgentEvent, 100),
		clientOpts: opts,
	}
}

func (p *GeminiProvider) Name() string { return "gemini" }

func (p *GeminiProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...ExecuteOption) (*AgentResult, error) {
	cfg := applyOptions(opts)
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	// ACP has no reasoning-effort knob — fail fast before spawning the
	// subprocess. EffortAuto is the explicit "use the provider default"
	// sentinel, which a no-knob provider already satisfies, so it passes
	// through.
	if cfg.Effort != "" && cfg.Effort != EffortAuto {
		return nil, EffortUnsupportedError(p.Name(), cfg.Effort)
	}

	// Build full prompt with worktree context
	fullPrompt := prompt
	if wtCtx != nil {
		fullPrompt = wtCtx.FormatForPrompt() + "\n\n" + prompt
	}

	// Ensure client is started (lazy init with mutex protection).
	// LLMEndpoint must be applied at client construction since acp BinaryArgs
	// and Env are passed to the subprocess at boot. Subsequent Execute calls
	// reuse the existing client; reject divergent endpoints loudly rather
	// than silently routing to the originally-bound endpoint.
	p.mu.Lock()
	if p.client == nil {
		// Apply WithLLMEndpoint AFTER caller clientOpts. acp.WithEnv
		// replaces the env map wholesale, so a caller passing
		// WithEnv(...) before our LLMEndpoint creds (GEMINI_API_KEY,
		// GOOGLE_GEMINI_BASE_URL) would otherwise drop them. Putting
		// LLMEndpoint last makes it merge into whatever map the caller
		// supplied — at the cost of letting LLMEndpoint silently
		// overwrite a same-named caller env var, which is the right
		// trade-off (the LLM creds are load-bearing for routing).
		clientOpts := append([]acp.ClientOption{}, p.clientOpts...)
		if !cfg.LLMEndpoint.IsZero() {
			clientOpts = append(clientOpts, acp.WithLLMEndpoint(cfg.LLMEndpoint))
		}
		client := acp.NewClient(clientOpts...)
		// Use context.Background() to decouple the ACP subprocess lifetime
		// from any single request's context. The subprocess should live as long
		// as the provider, not just the first request.
		if err := client.Start(context.Background()); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		p.client = client
		// Clone so a caller mutating cfg.LLMEndpoint.Headers after this Execute
		// returns can't fool the next divergence check by aliasing the same map.
		p.boundEndpt = cfg.LLMEndpoint.Clone()
		p.bridgeDone = make(chan struct{})

		// Start a single persistent bridge goroutine for the client's events.
		// This is the ONLY consumer of client.Events(). It writes to p.events
		// and also copies events to p.handlerCh when set (per Execute call).
		p.bridgeWg.Add(1)
		go func() {
			defer p.bridgeWg.Done()
			p.bridgeEventsWithHandler(client.Events())
		}()
	} else if !endpointsEqual(p.boundEndpt, cfg.LLMEndpoint) {
		p.mu.Unlock()
		// Use Endpoint.String() so divergences confined to headers/auth/wire
		// surface in the message; comparing only BaseURL would mislead callers
		// when both endpoints share a base URL but disagree on other fields.
		return nil, fmt.Errorf("gemini: LLMEndpoint changed across Execute calls (bound=%s, requested=%s); recreate the provider to switch endpoints",
			p.boundEndpt.String(), cfg.LLMEndpoint.String())
	}
	client := p.client
	p.mu.Unlock()

	// Build session options
	var sessionOpts []acp.SessionOption
	if cfg.WorkDir != "" {
		sessionOpts = append(sessionOpts, acp.WithSessionCWD(cfg.WorkDir))
	}

	// Create session and execute
	session, err := client.NewSession(ctx, sessionOpts...)
	if err != nil {
		return nil, err
	}

	// If an EventHandler is provided, set up a buffered channel that the bridge
	// goroutine will copy events to. After Prompt returns we drain this channel
	// to dispatch all events to the handler.
	var hCh chan AgentEvent
	if cfg.EventHandler != nil {
		hCh = make(chan AgentEvent, 100)
		p.handlerMu.Lock()
		p.handlerCh = hCh
		p.handlerMu.Unlock()
	}

	result, promptErr := session.Prompt(ctx, fullPrompt)

	// Drain handler channel: dispatch buffered events to the handler, then
	// detach the channel from the bridge. We wait briefly for any in-flight
	// events (e.g., TurnComplete emitted at the end of Prompt) to be forwarded
	// by the bridge goroutine before draining.
	if hCh != nil {
		p.drainHandlerEvents(hCh, cfg.EventHandler)
	}

	if promptErr != nil {
		return nil, promptErr
	}

	return nonNilAgentResult(acpResultToAgentResult(result)), nil
}

// drainHandlerEvents waits for the TurnComplete event (or a short timeout),
// dispatches all buffered events to the handler, and detaches the handler channel.
func (p *GeminiProvider) drainHandlerEvents(hCh chan AgentEvent, handler EventHandler) {
	// Wait up to 100ms for TurnComplete to arrive. This covers the race where
	// session.Prompt returns before the bridge goroutine has forwarded the
	// TurnComplete event from the ACP client's event channel to hCh.
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()

	sawTurnComplete := false
	for !sawTurnComplete {
		select {
		case ev := <-hCh:
			dispatchAgentEvent(ev, handler)
			if _, ok := ev.(TurnCompleteAgentEvent); ok {
				sawTurnComplete = true
			}
		case <-timer.C:
			// Timed out waiting for TurnComplete; proceed with what we have.
			sawTurnComplete = true
		}
	}

	// Detach the handler channel from the bridge.
	p.handlerMu.Lock()
	p.handlerCh = nil
	p.handlerMu.Unlock()

	// Drain any remaining buffered events.
	for {
		select {
		case ev := <-hCh:
			dispatchAgentEvent(ev, handler)
		default:
			return
		}
	}
}

// bridgeEventsWithHandler is the single consumer of client.Events(). It
// forwards events to p.events and also copies to p.handlerCh when set.
func (p *GeminiProvider) bridgeEventsWithHandler(events <-chan acp.Event) {
	if events == nil {
		return
	}
	for {
		select {
		case <-p.bridgeDone:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			agentEv := acpEventToAgentEvent(ev)
			if agentEv == nil {
				continue
			}

			// Forward to the channel for Events() consumers.
			select {
			case p.events <- agentEv:
			default:
			}

			// Copy to the per-Execute handler channel if set.
			p.handlerMu.RLock()
			hCh := p.handlerCh
			p.handlerMu.RUnlock()
			if hCh != nil {
				select {
				case hCh <- agentEv:
				default:
				}
			}
		}
	}
}

func (p *GeminiProvider) Events() <-chan AgentEvent { return p.events }

func (p *GeminiProvider) Close() error {
	p.mu.Lock()

	// Signal bridge goroutine to exit
	if p.bridgeDone != nil {
		close(p.bridgeDone)
		p.bridgeDone = nil
	}

	// Stop client, which closes its events channel and stops the subprocess
	if p.client != nil {
		p.client.Stop()
		p.client = nil
	}

	p.mu.Unlock()

	// Wait for bridge goroutine to fully exit before closing events channel
	p.bridgeWg.Wait()

	// Now safe to close our events channel since bridge goroutine has exited
	close(p.events)
	return nil
}

// GeminiLongRunningProvider wraps a persistent ACP session for multi-turn use.
type GeminiLongRunningProvider struct {
	*GeminiProvider
	longRunningClient *acp.Client
	session           *acp.Session
	clientOpts        []acp.ClientOption
	sessionOpts       []acp.SessionOption
}

// NewGeminiLongRunningProvider creates a Gemini provider with a persistent session.
func NewGeminiLongRunningProvider(clientOpts []acp.ClientOption, sessionOpts ...acp.SessionOption) *GeminiLongRunningProvider {
	return &GeminiLongRunningProvider{
		GeminiProvider: NewGeminiProvider(clientOpts...),
		clientOpts:     clientOpts,
		sessionOpts:    sessionOpts,
	}
}

func (p *GeminiLongRunningProvider) Start(ctx context.Context) error {
	client := acp.NewClient(p.clientOpts...)
	if err := client.Start(ctx); err != nil {
		return err
	}
	p.longRunningClient = client

	// Start the persistent event bridge for this long-running client
	p.mu.Lock()
	p.bridgeDone = make(chan struct{})
	p.bridgeWg.Add(1)
	go func() {
		defer p.bridgeWg.Done()
		bridgeEvents(client.Events(), nil, p.events, p.bridgeDone, "", nil)
	}()
	p.mu.Unlock()

	session, err := client.NewSession(ctx, p.sessionOpts...)
	if err != nil {
		client.Stop()
		return err
	}
	p.session = session

	return nil
}

func (p *GeminiLongRunningProvider) SendMessage(ctx context.Context, message string) (*AgentResult, error) {
	if p.session == nil {
		return nil, acp.ErrNotStarted
	}

	result, err := p.session.Prompt(ctx, message)
	if err != nil {
		return nil, err
	}

	return nonNilAgentResult(acpResultToAgentResult(result)), nil
}

func (p *GeminiLongRunningProvider) Stop() error {
	if p.longRunningClient != nil {
		return p.longRunningClient.Stop()
	}
	return nil
}

// Close stops the long-running provider's ACP client and closes the event channel.
func (p *GeminiLongRunningProvider) Close() error {
	p.mu.Lock()

	// Signal bridge goroutine to exit
	if p.bridgeDone != nil {
		close(p.bridgeDone)
		p.bridgeDone = nil
	}

	// Stop the long-running client (distinct from the embedded GeminiProvider.client).
	if p.longRunningClient != nil {
		p.longRunningClient.Stop()
		p.longRunningClient = nil
	}

	// Also stop the embedded GeminiProvider's client in case Execute() was called.
	// We can't call GeminiProvider.Close() because it would try to close bridgeDone again,
	// so we just stop the client directly.
	if p.GeminiProvider.client != nil {
		p.GeminiProvider.client.Stop()
		p.GeminiProvider.client = nil
	}

	p.mu.Unlock()

	// Wait for bridge goroutine to fully exit before closing events channel
	p.bridgeWg.Wait()

	// Now safe to close our events channel since bridge goroutine has exited
	close(p.events)
	return nil
}

// acpEventToAgentEvent converts an ACP Event to a provider-agnostic AgentEvent.
// Returns nil for events that have no AgentEvent equivalent.
func acpEventToAgentEvent(ev acp.Event) AgentEvent {
	switch e := ev.(type) {
	case acp.TextDeltaEvent:
		return TextAgentEvent{Text: e.Delta}
	case acp.ThinkingDeltaEvent:
		return ThinkingAgentEvent{Thinking: e.Delta}
	case acp.ToolCallStartEvent:
		return ToolStartAgentEvent{Name: e.ToolName, ID: e.ToolCallID, Input: e.Input}
	case acp.ToolCallUpdateEvent:
		if e.Status == "completed" || e.Status == "errored" {
			return ToolCompleteAgentEvent{
				Name:    e.ToolName,
				ID:      e.ToolCallID,
				Input:   e.Input,
				IsError: e.Status == "errored",
			}
		}
		return nil
	case acp.TurnCompleteEvent:
		return TurnCompleteAgentEvent{
			TurnNumber: e.TurnNumber,
			Success:    e.Success,
			DurationMs: e.DurationMs,
		}
	case acp.ErrorEvent:
		return ErrorAgentEvent{Err: e.Error, Context: e.Context}
	default:
		return nil
	}
}

// dispatchAgentEvent sends a single AgentEvent to an EventHandler.
func dispatchAgentEvent(ev AgentEvent, handler EventHandler) {
	switch e := ev.(type) {
	case TextAgentEvent:
		handler.OnText(e.Text)
	case ThinkingAgentEvent:
		handler.OnThinking(e.Thinking)
	case ToolStartAgentEvent:
		handler.OnToolStart(e.Name, e.ID, e.Input)
	case ToolCompleteAgentEvent:
		handler.OnToolComplete(e.Name, e.ID, e.Input, e.Result, e.IsError)
	case TurnCompleteAgentEvent:
		handler.OnTurnComplete(e.TurnNumber, e.Success, e.DurationMs, e.CostUSD)
	case ErrorAgentEvent:
		handler.OnError(e.Err, e.Context)
	}
}

// acpResultToAgentResult converts an ACP TurnResult to the provider-agnostic AgentResult.
func acpResultToAgentResult(r *acp.TurnResult) *AgentResult {
	if r == nil {
		return nil
	}
	return &AgentResult{
		Text:       r.FullText,
		Thinking:   r.Thinking,
		Success:    r.Success,
		Error:      r.Error,
		DurationMs: r.DurationMs,
		// ACP does not define token usage; fields default to zero.
	}
}
