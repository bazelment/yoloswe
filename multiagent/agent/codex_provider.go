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

			mapped, ok := codex.MapEvent(ev)
			if !ok {
				continue
			}
			if mapped.ThreadID != "" && mapped.ThreadID != threadID {
				continue
			}

			switch mapped.Kind {
			case codex.MappedEventTextDelta:
				if handler != nil {
					handler.OnText(mapped.Delta)
				}
				select {
				case out <- TextAgentEvent{Text: mapped.Delta}:
				default:
				}

			case codex.MappedEventReasoningDelta:
				if handler != nil {
					handler.OnThinking(mapped.Delta)
				}
				select {
				case out <- ThinkingAgentEvent{Thinking: mapped.Delta}:
				default:
				}

			case codex.MappedEventCommandStart:
				input := map[string]interface{}{}
				if mapped.Command != "" {
					input["command"] = mapped.Command
				}
				if mapped.CWD != "" {
					input["cwd"] = mapped.CWD
				}
				toolInputsMu.Lock()
				toolInputs[mapped.CallID] = input
				toolInputsMu.Unlock()

				if handler != nil {
					handler.OnToolStart("Bash", mapped.CallID, input)
				}
				select {
				case out <- ToolStartAgentEvent{Name: "Bash", ID: mapped.CallID, Input: input}:
				default:
				}

			case codex.MappedEventCommandEnd:
				toolInputsMu.Lock()
				input := toolInputs[mapped.CallID]
				delete(toolInputs, mapped.CallID)
				toolInputsMu.Unlock()
				if input == nil {
					input = map[string]interface{}{}
				}
				result := map[string]interface{}{
					"stdout":      mapped.Stdout,
					"stderr":      mapped.Stderr,
					"exit_code":   mapped.ExitCode,
					"duration_ms": mapped.DurationMs,
				}
				isError := mapped.ExitCode != 0
				if handler != nil {
					handler.OnToolComplete("Bash", mapped.CallID, input, result, isError)
				}
				select {
				case out <- ToolCompleteAgentEvent{
					Name:    "Bash",
					ID:      mapped.CallID,
					Input:   input,
					Result:  result,
					IsError: isError,
				}:
				default:
				}

			case codex.MappedEventTurnCompleted:
				turnNumber := parseCodexTurnNumber(mapped.TurnID)
				if handler != nil {
					handler.OnTurnComplete(turnNumber, mapped.Success, mapped.DurationMs, 0)
				}
				select {
				case out <- TurnCompleteAgentEvent{
					TurnNumber: turnNumber,
					Success:    mapped.Success,
					DurationMs: mapped.DurationMs,
				}:
				default:
				}
				markTurnDone()

			case codex.MappedEventError:
				if handler != nil {
					handler.OnError(mapped.Error, mapped.ErrorContext)
				}
				select {
				case out <- ErrorAgentEvent{Err: mapped.Error, Context: mapped.ErrorContext}:
				default:
				}
			}
		}
	}
}

func parseCodexTurnNumber(turnID string) int {
	return codex.TurnNumberFromID(turnID)
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
