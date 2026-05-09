package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
	"github.com/bazelment/yoloswe/wt"
)

// CodexProvider wraps the Codex SDK behind the Provider interface.
//
//nolint:govet // fieldalignment: keep boundEndpt grouped with clientOpts; extra inline padding from nesting llmendpoint.Endpoint isn't worth obscuring the order.
type CodexProvider struct {
	boundEndpt llmendpoint.Endpoint
	clientOpts []codex.ClientOption
	client     *codex.Client
	events     chan AgentEvent
	mu         sync.Mutex
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

	// Ensure client is started.
	// LLMEndpoint must be applied at client construction since codex
	// `--config` overrides are passed to `app-server` at boot. Subsequent
	// Execute calls reuse the existing client and cannot change the endpoint;
	// reject divergent endpoints loudly rather than silently routing to the
	// originally-bound endpoint.
	p.mu.Lock()
	if p.client == nil {
		// Apply WithLLMEndpoint first so caller-supplied clientOpts can
		// override its defaults (e.g. WithAppServerArgs to re-enable a
		// feature WithLLMEndpoint disabled). codex applies repeated
		// flags in order, so later args win.
		var clientOpts []codex.ClientOption
		if !cfg.LLMEndpoint.IsZero() {
			clientOpts = append(clientOpts, codex.WithLLMEndpoint(cfg.LLMEndpoint))
		}
		clientOpts = append(clientOpts, p.clientOpts...)
		client := codex.NewClient(clientOpts...)
		if err := client.Start(ctx); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		p.client = client
		p.boundEndpt = cfg.LLMEndpoint
	} else if !endpointsEqual(p.boundEndpt, cfg.LLMEndpoint) {
		p.mu.Unlock()
		return nil, fmt.Errorf("codex: LLMEndpoint changed across Execute calls (bound=%q, requested=%q); recreate the provider to switch endpoints",
			p.boundEndpt.BaseURL, cfg.LLMEndpoint.BaseURL)
	}
	p.mu.Unlock()

	// Build thread options.
	// Only pass an explicit model if the caller overrode the default;
	// Claude-specific aliases (haiku, sonnet, opus) are not valid for codex
	// and should not be forwarded — let codex use its own configured default.
	var threadOpts []codex.ThreadOption
	if cfg.Model != "" && !isClaudeModelAlias(cfg.Model) {
		threadOpts = append(threadOpts, codex.WithModel(cfg.Model))
	}
	if policy, ok := codexApprovalPolicyForPermissionMode(cfg.PermissionMode); ok {
		threadOpts = append(threadOpts, codex.WithApprovalPolicy(policy))
	}
	// When no explicit permission mode is set (empty/"default"), don't override
	// codex's own default approval policy — callers that need auto-approve should
	// set PermissionMode to "bypass" explicitly.
	if cfg.WorkDir != "" {
		threadOpts = append(threadOpts, codex.WithWorkDir(cfg.WorkDir))
	}
	// For bypass mode (builders), disable sandboxing entirely so codex
	// can write files and run commands. The "workspace-write" mode still
	// uses bubblewrap, which may fail in container/VM environments that
	// lack network namespace permissions. Since the delegator runs in a
	// controlled environment, full access is appropriate.
	if strings.ToLower(strings.TrimSpace(cfg.PermissionMode)) == "bypass" {
		threadOpts = append(threadOpts, codex.WithSandbox("danger-full-access"))
	}

	// Create or resume thread and execute.
	var thread *codex.Thread
	var err error
	if cfg.ResumeSessionID != "" {
		thread, err = p.client.ResumeThread(ctx, cfg.ResumeSessionID, threadOpts...)
	} else {
		thread, err = p.client.CreateThread(ctx, threadOpts...)
	}
	if err != nil {
		return nil, err
	}

	bridgeStop := make(chan struct{})
	bridgeDone := make(chan struct{})
	turnDone := make(chan struct{})
	var turnDoneOnce sync.Once
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
		return &AgentResult{SessionID: thread.ID()}, err
	}

	turnOpts := codexTurnOptions(cfg)

	result, err := thread.Ask(ctx, fullPrompt, turnOpts...)
	if err != nil {
		waitForBridgeTurnComplete(ctx, turnDone)
		return &AgentResult{SessionID: thread.ID()}, err
	}
	waitForBridgeTurnComplete(ctx, turnDone)

	agentResult := nonNilAgentResult(codexResultToAgentResult(result))
	agentResult.SessionID = thread.ID()
	return agentResult, nil
}

func (p *CodexProvider) Events() <-chan AgentEvent { return p.events }

func (p *CodexProvider) Close() error {
	close(p.events)
	if p.client != nil {
		return p.client.Stop()
	}
	return nil
}

// codexTurnOptions builds the per-turn codex options derived from the
// provider-neutral ExecuteConfig. Extracted so the effort wiring can be
// unit-tested without spawning the codex subprocess.
//
// Codex accepts the effort string opaquely and forwards it to the model
// (see agent-cli-wrapper/codex/jsonrpc.go field "effort"). The agent.EffortLevel
// vocabulary is already validated upstream by ParseEffort.
func codexTurnOptions(cfg ExecuteConfig) []codex.TurnOption {
	if cfg.Effort == "" {
		return nil
	}
	return []codex.TurnOption{codex.WithEffort(string(cfg.Effort))}
}

func waitForBridgeTurnComplete(ctx context.Context, turnDone <-chan struct{}) {
	select {
	case <-turnDone:
	case <-ctx.Done():
	case <-time.After(500 * time.Millisecond):
	}
}

// codexResultToAgentResult converts a codex.TurnResult to AgentResult.
//
// CostUSD is left at zero: the codex protocol does not currently emit a
// per-turn cost, and we deliberately do not invent one from a hard-coded
// pricing table — wrong numbers in operator dashboards are worse than
// missing ones. Token counts alone are sufficient to detect a runaway
// agent. Revisit if codex starts shipping cost in token_count.
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

// endpointsEqual reports whether two endpoints would produce the same codex
// client configuration. ProviderName and Wire are compared via the canonical
// accessors so an empty Wire ("" → "chat") and an empty ProviderName ("" →
// "custom") compare equal to their explicit-default counterparts; the codex
// args this method gates on are derived from the same accessors, so raw-field
// comparison would over-trigger the divergence error.
func endpointsEqual(a, b llmendpoint.Endpoint) bool {
	if a.BaseURL != b.BaseURL || a.APIKey != b.APIKey || a.APIKeyEnv != b.APIKeyEnv ||
		a.Provider() != b.Provider() || a.WireAPI() != b.WireAPI() {
		return false
	}
	if len(a.Headers) != len(b.Headers) {
		return false
	}
	for k, v := range a.Headers {
		if b.Headers[k] != v {
			return false
		}
	}
	return true
}

// isClaudeModelAlias returns true for model names that are Claude-specific
// shorthand (haiku, sonnet, opus) and not valid for non-Claude providers.
func isClaudeModelAlias(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "haiku", "sonnet", "opus":
		return true
	default:
		return false
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
