package agent

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/wt"
)

// CodexProvider wraps the Codex SDK behind the Provider interface.
type CodexProvider struct {
	client     *codex.Client
	events     chan AgentEvent
	clientOpts []codex.ClientOption
}

// NewCodexProvider creates a new Codex provider.
func NewCodexProvider(opts ...codex.ClientOption) *CodexProvider {
	return &CodexProvider{
		events:     make(chan AgentEvent, 100),
		clientOpts: opts,
	}
}

func (p *CodexProvider) Name() string { return "codex" }

func (p *CodexProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...ExecuteOption) (*AgentResult, error) {
	cfg := applyOptions(opts)

	// Build full prompt with worktree context
	fullPrompt := prompt
	if wtCtx != nil {
		fullPrompt = wtCtx.FormatForPrompt() + "\n\n" + prompt
	}

	// Ensure client is started
	if p.client == nil {
		client := codex.NewClient(p.clientOpts...)
		if err := client.Start(ctx); err != nil {
			return nil, err
		}
		p.client = client
	}

	// Build thread options
	var threadOpts []codex.ThreadOption
	if cfg.Model != "" {
		threadOpts = append(threadOpts, codex.WithModel(cfg.Model))
	}
	if policy, ok := codexApprovalPolicyForPermissionMode(cfg.PermissionMode); ok {
		threadOpts = append(threadOpts, codex.WithApprovalPolicy(policy))
	}
	if cfg.WorkDir != "" {
		threadOpts = append(threadOpts, codex.WithWorkDir(cfg.WorkDir))
	}

	// Create thread and execute
	thread, err := p.client.CreateThread(ctx, threadOpts...)
	if err != nil {
		return nil, err
	}

	bridgeStop := make(chan struct{})
	bridgeDone := make(chan struct{})
	turnDone := make(chan struct{})
	turnDoneOnce := sync.Once{}
	go func() {
		bridgeEvents(
			p.client.Events(),
			cfg.EventHandler,
			p.events,
			bridgeStop,
			thread.ID(),
			func() { turnDoneOnce.Do(func() { close(turnDone) }) },
		)
		close(bridgeDone)
	}()
	defer func() {
		close(bridgeStop)
		<-bridgeDone
	}()

	if err := thread.WaitReady(ctx); err != nil {
		return nil, err
	}

	result, err := thread.Ask(ctx, fullPrompt)
	if err != nil {
		return nil, err
	}
	select {
	case <-turnDone:
	case <-time.After(150 * time.Millisecond):
	}

	return codexResultToAgentResult(result), nil
}

func (p *CodexProvider) Events() <-chan AgentEvent { return p.events }

func (p *CodexProvider) Close() error {
	close(p.events)
	if p.client != nil {
		return p.client.Stop()
	}
	return nil
}

// codexResultToAgentResult converts a codex.TurnResult to AgentResult.
func codexResultToAgentResult(r *codex.TurnResult) *AgentResult {
	if r == nil {
		return nil
	}
	return &AgentResult{
		Text:       r.FullText,
		Success:    r.Success,
		Error:      r.Error,
		DurationMs: r.DurationMs,
		Usage: AgentUsage{
			InputTokens:     int(r.Usage.InputTokens),
			OutputTokens:    int(r.Usage.OutputTokens),
			CacheReadTokens: int(r.Usage.CachedInputTokens),
		},
	}
}

func codexApprovalPolicyForPermissionMode(permissionMode string) (codex.ApprovalPolicy, bool) {
	switch strings.ToLower(strings.TrimSpace(permissionMode)) {
	case "", "default":
		return "", false
	case "bypass":
		return codex.ApprovalPolicyNever, true
	case "plan":
		// Planner mode should preserve approval gating for potentially mutating tools.
		return codex.ApprovalPolicyOnRequest, true
	default:
		return "", false
	}
}
