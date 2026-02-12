package agent

import (
	"context"
	"sync"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
	"github.com/bazelment/yoloswe/wt"
)

// GeminiProvider wraps an ACP client (Gemini CLI) behind the Provider interface.
type GeminiProvider struct {
	client     *acp.Client
	events     chan AgentEvent
	bridgeDone chan struct{} // signals bridge goroutine to exit
	clientOpts []acp.ClientOption
	mu         sync.Mutex
	bridgeWg   sync.WaitGroup // tracks bridge goroutine
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

	// Build full prompt with worktree context
	fullPrompt := prompt
	if wtCtx != nil {
		fullPrompt = wtCtx.FormatForPrompt() + "\n\n" + prompt
	}

	// Ensure client is started (lazy init with mutex protection)
	p.mu.Lock()
	if p.client == nil {
		client := acp.NewClient(p.clientOpts...)
		// Use context.Background() to decouple the ACP subprocess lifetime
		// from any single request's context. The subprocess should live as long
		// as the provider, not just the first request.
		if err := client.Start(context.Background()); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		p.client = client
		p.bridgeDone = make(chan struct{})

		// Start a single persistent bridge goroutine for the client's events.
		// This goroutine will forward ALL events from the client to p.events.
		p.bridgeWg.Add(1)
		go bridgeACPEventsToChannel(p.client.Events(), p.events, p.bridgeDone, &p.bridgeWg)
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

	// If an EventHandler is provided, start a bridge goroutine for this specific
	// Execute call to forward events to the handler. This goroutine will exit
	// when the session completes.
	var bridgeDone chan struct{}
	if cfg.EventHandler != nil {
		bridgeDone = make(chan struct{})
		go bridgeACPEventsToHandler(client.Events(), cfg.EventHandler, bridgeDone)
	}

	result, err := session.Prompt(ctx, fullPrompt)

	// Signal the per-Execute bridge to stop
	if bridgeDone != nil {
		close(bridgeDone)
	}

	if err != nil {
		return nil, err
	}

	return acpResultToAgentResult(result), nil
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
	go bridgeACPEventsToChannel(client.Events(), p.events, p.bridgeDone, &p.bridgeWg)
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

	return acpResultToAgentResult(result), nil
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

// bridgeACPEventsToChannel forwards ACP events to the AgentEvent channel.
// This is a persistent goroutine that runs for the lifetime of the client.
// Exits when events channel closes OR done channel is closed.
func bridgeACPEventsToChannel(events <-chan acp.Event, ch chan<- AgentEvent, done <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	if events == nil {
		return
	}
	for {
		select {
		case <-done:
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			switch e := event.(type) {
			case acp.TextDeltaEvent:
				select {
				case ch <- TextAgentEvent{Text: e.Delta}:
				default:
				}

			case acp.ThinkingDeltaEvent:
				select {
				case ch <- ThinkingAgentEvent{Thinking: e.Delta}:
				default:
				}

			case acp.ToolCallStartEvent:
				select {
				case ch <- ToolStartAgentEvent{Name: e.ToolName, ID: e.ToolCallID, Input: e.Input}:
				default:
				}

			case acp.ToolCallUpdateEvent:
				if e.Status == "completed" || e.Status == "errored" {
					select {
					case ch <- ToolCompleteAgentEvent{Name: e.ToolName, ID: e.ToolCallID, Input: e.Input, IsError: e.Status == "errored"}:
					default:
					}
				}

			case acp.TurnCompleteEvent:
				select {
				case ch <- TurnCompleteAgentEvent{TurnNumber: 1, Success: e.Success, DurationMs: e.DurationMs}:
				default:
				}

			case acp.ErrorEvent:
				select {
				case ch <- ErrorAgentEvent{Err: e.Error, Context: e.Context}:
				default:
				}
			}
		}
	}
}

// bridgeACPEventsToHandler forwards ACP events to an EventHandler.
// This is a per-Execute goroutine that runs for the duration of a single request.
// Exits when events channel closes OR done channel is closed.
func bridgeACPEventsToHandler(events <-chan acp.Event, handler EventHandler, done <-chan struct{}) {
	if events == nil || handler == nil {
		return
	}
	for {
		select {
		case <-done:
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			switch e := event.(type) {
			case acp.TextDeltaEvent:
				handler.OnText(e.Delta)

			case acp.ThinkingDeltaEvent:
				handler.OnThinking(e.Delta)

			case acp.ToolCallStartEvent:
				handler.OnToolStart(e.ToolName, e.ToolCallID, e.Input)

			case acp.ToolCallUpdateEvent:
				if e.Status == "completed" || e.Status == "errored" {
					handler.OnToolComplete(e.ToolName, e.ToolCallID, e.Input, nil, e.Status == "errored")
				}

			case acp.TurnCompleteEvent:
				handler.OnTurnComplete(1, e.Success, e.DurationMs, 0)

			case acp.ErrorEvent:
				handler.OnError(e.Error, e.Context)
			}
		}
	}
}
