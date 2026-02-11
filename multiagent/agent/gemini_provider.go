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

		// Start a single persistent bridge goroutine for this client
		if cfg.EventHandler != nil {
			go bridgeACPEvents(p.client.Events(), cfg.EventHandler, p.events, p.bridgeDone)
		}
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

	result, err := session.Prompt(ctx, fullPrompt)
	if err != nil {
		return nil, err
	}

	return acpResultToAgentResult(result), nil
}

func (p *GeminiProvider) Events() <-chan AgentEvent { return p.events }

func (p *GeminiProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Signal bridge goroutine to exit before closing events channel
	if p.bridgeDone != nil {
		close(p.bridgeDone)
		p.bridgeDone = nil
	}

	// Stop client, which closes its events channel and stops the subprocess
	if p.client != nil {
		p.client.Stop()
		p.client = nil
	}

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
	// Stop the long-running client (distinct from the embedded GeminiProvider.client).
	if p.longRunningClient != nil {
		p.longRunningClient.Stop()
	}
	// Also stop the embedded provider's client if it was lazily initialized.
	if p.GeminiProvider.client != nil {
		p.GeminiProvider.client.Stop()
	}
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

// bridgeACPEvents converts ACP events to AgentEvents and forwards to handler.
// Exits when events channel closes OR done channel is closed.
func bridgeACPEvents(events <-chan acp.Event, handler EventHandler, ch chan<- AgentEvent, done <-chan struct{}) {
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
				handler.OnText(e.Delta)
				select {
				case ch <- TextAgentEvent{Text: e.Delta}:
				default:
				}

			case acp.ThinkingDeltaEvent:
				handler.OnThinking(e.Delta)
				select {
				case ch <- ThinkingAgentEvent{Thinking: e.Delta}:
				default:
				}

			case acp.ToolCallStartEvent:
				handler.OnToolStart(e.ToolName, e.ToolCallID, e.Input)
				select {
				case ch <- ToolStartAgentEvent{Name: e.ToolName, ID: e.ToolCallID, Input: e.Input}:
				default:
				}

			case acp.ToolCallUpdateEvent:
				if e.Status == "completed" || e.Status == "errored" {
					handler.OnToolComplete(e.ToolName, e.ToolCallID, e.Input, nil, e.Status == "errored")
					select {
					case ch <- ToolCompleteAgentEvent{Name: e.ToolName, ID: e.ToolCallID, Input: e.Input, IsError: e.Status == "errored"}:
					default:
					}
				}

			case acp.TurnCompleteEvent:
				handler.OnTurnComplete(1, e.Success, e.DurationMs, 0)
				select {
				case ch <- TurnCompleteAgentEvent{TurnNumber: 1, Success: e.Success, DurationMs: e.DurationMs}:
				default:
				}

			case acp.ErrorEvent:
				handler.OnError(e.Error, e.Context)
				select {
				case ch <- ErrorAgentEvent{Err: e.Error, Context: e.Context}:
				default:
				}
			}
		}
	}
}
