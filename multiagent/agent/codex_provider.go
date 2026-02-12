package agent

import (
	"context"
	"strconv"
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
	go func() {
		bridgeCodexEvents(
			p.client.Events(),
			thread.ID(),
			cfg.EventHandler,
			p.events,
			bridgeStop,
			turnDone,
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

func bridgeCodexEvents(
	events <-chan codex.Event,
	threadID string,
	handler EventHandler,
	out chan<- AgentEvent,
	stop <-chan struct{},
	turnDone chan struct{},
) {
	if events == nil {
		return
	}

	var (
		toolInputsMu sync.Mutex
		toolInputs   = make(map[string]map[string]interface{})
		turnDoneOnce sync.Once
	)

	markTurnDone := func() {
		turnDoneOnce.Do(func() {
			close(turnDone)
		})
	}

	for {
		select {
		case <-stop:
			return
		case ev, ok := <-events:
			if !ok {
				markTurnDone()
				return
			}

			eventThreadID, hasThread := codexEventThreadID(ev)
			if hasThread && eventThreadID != threadID {
				continue
			}

			switch e := ev.(type) {
			case codex.TextDeltaEvent:
				if handler != nil {
					handler.OnText(e.Delta)
				}
				select {
				case out <- TextAgentEvent{Text: e.Delta}:
				default:
				}

			case codex.ReasoningDeltaEvent:
				if handler != nil {
					handler.OnThinking(e.Delta)
				}
				select {
				case out <- ThinkingAgentEvent{Thinking: e.Delta}:
				default:
				}

			case codex.CommandStartEvent:
				input := codexCommandInput(e)
				toolInputsMu.Lock()
				toolInputs[e.CallID] = input
				toolInputsMu.Unlock()

				if handler != nil {
					handler.OnToolStart("Bash", e.CallID, input)
				}
				select {
				case out <- ToolStartAgentEvent{Name: "Bash", ID: e.CallID, Input: input}:
				default:
				}

			case codex.CommandEndEvent:
				toolInputsMu.Lock()
				input := toolInputs[e.CallID]
				delete(toolInputs, e.CallID)
				toolInputsMu.Unlock()
				if input == nil {
					input = map[string]interface{}{}
				}
				result := map[string]interface{}{
					"stdout":      e.Stdout,
					"stderr":      e.Stderr,
					"exit_code":   e.ExitCode,
					"duration_ms": e.DurationMs,
				}
				isError := e.ExitCode != 0
				if handler != nil {
					handler.OnToolComplete("Bash", e.CallID, input, result, isError)
				}
				select {
				case out <- ToolCompleteAgentEvent{
					Name:    "Bash",
					ID:      e.CallID,
					Input:   input,
					Result:  result,
					IsError: isError,
				}:
				default:
				}

			case codex.TurnCompletedEvent:
				turnNumber := parseCodexTurnNumber(e.TurnID)
				if handler != nil {
					handler.OnTurnComplete(turnNumber, e.Success, e.DurationMs, 0)
				}
				select {
				case out <- TurnCompleteAgentEvent{
					TurnNumber: turnNumber,
					Success:    e.Success,
					DurationMs: e.DurationMs,
				}:
				default:
				}
				markTurnDone()

			case codex.ErrorEvent:
				if handler != nil {
					handler.OnError(e.Error, e.Context)
				}
				select {
				case out <- ErrorAgentEvent{Err: e.Error, Context: e.Context}:
				default:
				}
			}
		}
	}
}

func codexEventThreadID(ev codex.Event) (string, bool) {
	switch e := ev.(type) {
	case codex.ThreadReadyEvent:
		return e.ThreadID, true
	case codex.TurnStartedEvent:
		return e.ThreadID, true
	case codex.TurnCompletedEvent:
		return e.ThreadID, true
	case codex.TextDeltaEvent:
		return e.ThreadID, true
	case codex.ItemStartedEvent:
		return e.ThreadID, true
	case codex.ItemCompletedEvent:
		return e.ThreadID, true
	case codex.TokenUsageEvent:
		return e.ThreadID, true
	case codex.ErrorEvent:
		if e.ThreadID != "" {
			return e.ThreadID, true
		}
		return "", false
	case codex.CommandStartEvent:
		return e.ThreadID, true
	case codex.CommandOutputEvent:
		return e.ThreadID, true
	case codex.CommandEndEvent:
		return e.ThreadID, true
	case codex.ReasoningDeltaEvent:
		return e.ThreadID, true
	default:
		return "", false
	}
}

func codexCommandInput(e codex.CommandStartEvent) map[string]interface{} {
	input := map[string]interface{}{}
	cmd := strings.TrimSpace(e.ParsedCmd)
	if cmd == "" {
		cmd = strings.TrimSpace(strings.Join(e.Command, " "))
	}
	if cmd != "" {
		input["command"] = cmd
	}
	if e.CWD != "" {
		input["cwd"] = e.CWD
	}
	return input
}

func parseCodexTurnNumber(turnID string) int {
	if n, err := strconv.Atoi(turnID); err == nil && n >= 0 {
		return n + 1
	}
	return 1
}

func codexApprovalPolicyForPermissionMode(permissionMode string) (codex.ApprovalPolicy, bool) {
	switch strings.ToLower(strings.TrimSpace(permissionMode)) {
	case "", "default":
		return "", false
	case "bypass":
		return codex.ApprovalPolicyNever, true
	case "plan":
		// Codex provider currently lacks an interactive approval callback in Bramble.
		// Use auto-approval to avoid deadlocked turns waiting for user approval.
		return codex.ApprovalPolicyNever, true
	default:
		return "", false
	}
}
