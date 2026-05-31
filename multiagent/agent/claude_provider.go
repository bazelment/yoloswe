package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
	"github.com/bazelment/yoloswe/wt"
)

const retryPrompt = "retry"

// UnresolvedToolErrorMarkerPrefix is a stable prefix that identifies the
// unresolved-tool-error marker in agent text output. Callers can use it to
// detect whether the marker is already present before re-appending.
const UnresolvedToolErrorMarkerPrefix = "[unresolved tool error ("

const unresolvedToolErrorMarkerTemplate = UnresolvedToolErrorMarkerPrefix + "%s) after %d/%d retries — tool: %s\n excerpt: %s]"

// Abort-reason constants for UnresolvedToolError.Reason.
const (
	RetryStopExhausted      = "exhausted"
	RetryStopNoProgress     = "no_progress"
	RetryStopBudgetExceeded = "budget_exceeded"
	RetryStopCtxCancelled   = "ctx_cancelled"
	RetryStopPermanent      = "permanent"
)

// FormatUnresolvedToolErrorMarker returns the marker string used to surface
// an unresolved tool error in agent text output. Callers that replace
// AgentResult.Text downstream (e.g. plan-file readers) should re-append this
// marker so the failure is not silently dropped.
func FormatUnresolvedToolErrorMarker(e UnresolvedToolError) string {
	reason := e.Reason
	if reason == "" {
		reason = RetryStopExhausted
	}
	return fmt.Sprintf(unresolvedToolErrorMarkerTemplate, reason, e.Attempts, e.Max, e.Tool, e.Excerpt)
}

// AppendUnresolvedToolErrorMarker appends the marker to text, using a blank
// separator when text is non-empty. Exported for callers that re-append the
// marker after downstream text substitution.
func AppendUnresolvedToolErrorMarker(text string, e UnresolvedToolError) string {
	marker := FormatUnresolvedToolErrorMarker(e)
	if text == "" {
		return marker
	}
	return text + "\n\n" + marker
}

const minRetryWallClockBudget = 10 * time.Minute

// streamTurnGracePeriod bounds how long streamTurn waits, after a
// TurnCompleteEvent has been observed, for an outstanding background
// tool_use to produce a terminal event. On expiry the turn is forced done.
// This is the backstop for an agent that backgrounds a tool that never
// terminates (e.g. a `while true` poll loop) without a ScheduleWakeup.
const streamTurnGracePeriod = 3 * time.Minute

// consumeTurnEvents drives one logical turn by feeding events from the
// session channel into a logicalTurnState until the turn is done. It
// returns when LogicalTurnDone() flips, the channel closes, ctx is
// cancelled, or — the structural backstop — gracePeriod elapses after a
// TurnCompleteEvent was observed but the logical turn is still gated on a
// background tool_use that never terminated. dispatch is invoked for every
// event so consumers (EventHandler, AgentEvent channel) see the full stream.
//
// The grace timer is per completion wave: a continuation wave (a fresh
// ResultMessageEvent) or an invalidated pair resets it, so a legitimate
// multi-wave turn is never preempted by a deadline anchored to an earlier
// wave — only a wave that genuinely stalls on a non-terminating bg tool_use
// hits the backstop.
func consumeTurnEvents(
	ctx context.Context,
	events <-chan claude.Event,
	gracePeriod time.Duration,
	logger *slog.Logger,
	dispatch func(claude.Event),
) (*claude.TurnResult, error) {
	state := newLogicalTurnState()
	// The grace timer tracks a single completion wave: it is armed while the
	// current wave has signalled turn completion but is still gated on a
	// background tool_use, and disarmed the moment that wave is invalidated.
	// While graceCh is nil the select blocks only on ctx/events.
	var graceTimer *time.Timer
	var graceCh <-chan time.Time
	disarmGrace := func() {
		if graceTimer == nil {
			return
		}
		if !graceTimer.Stop() {
			// Drain a deadline that fired before Stop so a stale tick can't
			// preempt a later wave through the select.
			select {
			case <-graceTimer.C:
			default:
			}
		}
		graceTimer = nil
		graceCh = nil
	}
	// rearmGrace enforces the single grace invariant after an event is applied:
	// the timer is armed iff the current wave has signalled completion but the
	// logical turn is still gated on a background tool_use. Shared by the main
	// events case and the grace-branch continuation so neither can leave the
	// turn without a backstop (or with a stale one) — re-arming only when none
	// is pinned so an in-flight wave keeps its original deadline.
	rearmGrace := func() {
		if state.SawTurnComplete() {
			if graceTimer == nil {
				graceTimer = time.NewTimer(gracePeriod)
				graceCh = graceTimer.C
			}
		} else {
			disarmGrace()
		}
	}
	defer disarmGrace()
eventLoop:
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-graceCh:
			lg := logger
			if lg == nil {
				lg = slog.Default()
			}
			lg.Warn("forcing turn completion: background tool_use never terminated",
				"grace_period", gracePeriod)
			result := state.ToTurnResult()
			// A grace-forced stop is a structural deadline, not a real turn
			// outcome — reaching this branch means LogicalTurnDone() is false, so
			// the turn is NOT actually complete regardless of result.Success.
			// Resolve in priority order:
			//   1. A genuine error (state.Err() != nil) — return it unwrapped;
			//      never mask a real ResultError that coincides with expiry.
			//   2. ctx cancelled — terminal; propagate ctx.Err() so a cancelled
			//      run never reports a (stale) success or a retryable transient.
			//      (After state.Err() so a real ResultError still wins; before the
			//      classification below so neither path can mask cancellation.)
			//   3. Otherwise the turn is still gated on a background tool_use that
			//      hasn't terminated — whether the last wave's result was success
			//      or not. A pending stream event takes priority (it may be the
			//      continuation that completes the turn); if nothing is queued the
			//      stop is transient so the resume-on-transient path re-drives the
			//      same session and lets the in-flight background work finish.
			//      Critically, a Success==true result is NOT returned as-is here:
			//      a skill (e.g. /pr-polish) that yielded the turn awaiting a long
			//      run_in_background join keeps lastResult successful for the whole
			//      join, so returning it after the 3-minute grace would report the
			//      step as done to a non-interactive caller (jiradozer) while the
			//      reviewers are still running. Treat it as resumable instead.
			if err := state.Err(); err != nil {
				return result, err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			// graceCh can also win a tie with a ready events case. Give a real
			// pending event priority over the deadline, without a destructive
			// peek: a closed stream is terminal; a real pending event is applied
			// (never dropped — it may be the continuation that completes the
			// wave) and the loop re-evaluates. Only when nothing is queued do we
			// classify the stall transient.
			select {
			case ev, ok := <-events:
				if !ok {
					// Stream closed: terminal. Mirror the closed-stream path
					// below — never a retryable TransientError.
					return state.ToTurnResult(), state.Err()
				}
				// The grace timer already fired (its channel is drained but
				// graceTimer is still non-nil). Clear it, then fall through to
				// the SAME re-arm rule the main events case uses below: the turn
				// must end this branch with a grace timer armed whenever it is in
				// a completed-but-gated wave, and disarmed otherwise. (Applying
				// the event here, rather than deferring to the next loop
				// iteration, is necessary because we already had to receive it to
				// distinguish a real event from a closed stream without a
				// destructive peek.)
				disarmGrace()
				state.Apply(ev)
				if dispatch != nil {
					dispatch(ev)
				}
				if state.LogicalTurnDone() {
					return state.ToTurnResult(), state.Err()
				}
				rearmGrace()
				continue eventLoop
			default:
				return result, &claude.TransientError{
					Message: "stream idle: turn forced complete after grace period gated on background tool_use",
				}
			}
		case ev, ok := <-events:
			if !ok {
				// A closed stream is terminal, not a transient stall: unlike the
				// grace path above, don't reclassify a Success=false/Err=nil
				// result as transient — there is no live session left to resume.
				return state.ToTurnResult(), state.Err()
			}
			state.Apply(ev)
			if dispatch != nil {
				dispatch(ev)
			}
			if state.LogicalTurnDone() {
				return state.ToTurnResult(), state.Err()
			}
			// Keep the grace timer pinned to the current completion wave:
			// SawTurnComplete() reports the live wave only — a continuation
			// ResultMessageEvent or invalidateForContinuation clears
			// lastTurnComplete and flips it back to false, so a gone wave
			// disarms and the next gated wave gets a fresh full window.
			rearmGrace()
		}
	}
}

func computeRetryTimeBudget(cfg ExecuteConfig) time.Duration {
	turnBudget := time.Duration(cfg.MaxTurns) * time.Minute
	if turnBudget > minRetryWallClockBudget {
		return turnBudget
	}
	return minRetryWallClockBudget
}

func buildRetryPrompt() string {
	return retryPrompt
}

func isPermanentToolError(toolResult string) bool {
	if strings.Contains(toolResult, "disable-model-invocation") {
		return true
	}
	return strings.Contains(toolResult, " cannot be used with Skill tool")
}

func permanentToolErrorExcerpt(toolResult, excerpt string) string {
	if isPermanentToolError(excerpt) {
		return excerpt
	}
	if strings.Contains(toolResult, "disable-model-invocation") {
		return "permanent tool error: disable-model-invocation"
	}
	if strings.Contains(toolResult, " cannot be used with Skill tool") {
		return "permanent tool error: cannot be used with Skill tool"
	}
	return excerpt
}

// emitRetry fires the hook before each follow-up Ask so logs see the
// decision even if the retry subsequently hangs.
func emitRetry(h EventHandler, attempt, max int, toolName, excerpt string) {
	if h == nil {
		return
	}
	if rh, ok := h.(RetryHandler); ok {
		rh.OnRetry(attempt, max, toolName, excerpt)
	}
}

func emitRetryAbort(h EventHandler, reason, toolName, excerpt string) {
	if h == nil {
		return
	}
	if rh, ok := h.(RetryHandler); ok {
		rh.OnRetryAbort(reason, toolName, excerpt)
	}
}

// retrySession is the minimal slice that the retry loop depends on. Exists
// so tests can drive runRetryLoop with a fake that scripts a sequence of
// TurnResults without spawning a real CLI. Production callers supply a
// streamTurnSession that issues each retry via the raw streaming loop.
type retrySession interface {
	Ask(ctx context.Context, content string) (*claude.TurnResult, error)
}

// streamTurnSession adapts a streaming-turn closure to retrySession. Used by
// Execute so the retry loop can issue "retry" turns via the same streaming
// path the initial turn used, without runRetryLoop needing to know about
// logicalTurnState.
type streamTurnSession struct {
	fn func(ctx context.Context, prompt string) (*claude.TurnResult, error)
}

func (s streamTurnSession) Ask(ctx context.Context, content string) (*claude.TurnResult, error) {
	return s.fn(ctx, content)
}

// ClaudeProvider wraps the Claude SDK behind the Provider interface.
type ClaudeProvider struct {
	events      chan AgentEvent
	sessionOpts []claude.SessionOption
}

// NewClaudeProvider creates a new Claude provider.
// Optional session options are prepended before per-call options built from ExecuteConfig.
func NewClaudeProvider(sessionOpts ...claude.SessionOption) *ClaudeProvider {
	return &ClaudeProvider{
		events:      make(chan AgentEvent, 100),
		sessionOpts: sessionOpts,
	}
}

func (p *ClaudeProvider) Name() string { return "claude" }

// runRetryLoop drives the retry-on-tool-error loop: re-issues the turn up to
// cfg.MaxToolErrorRetries times while a tool_use_error is present. Returns
// the final result, retry count, and the stop reason (exhausted, no_progress,
// budget_exceeded, ctx_cancelled, permanent, or self-recovered).
func runRetryLoop(ctx context.Context, session retrySession, initial *claude.TurnResult, cfg ExecuteConfig) (*claude.TurnResult, int, string, error) {
	result := initial
	start := time.Now()
	budget := computeRetryTimeBudget(cfg)
	stopReason := RetryStopExhausted
	var (
		prevToolResult string
		havePrev       bool
		attempts       int
	)
	for attempts < cfg.MaxToolErrorRetries {
		toolName, toolResult, excerpt, ok := claude.FinalTurnToolErrorDetails(result.ContentBlocks)
		if !ok {
			break
		}
		if ctx.Err() != nil {
			stopReason = RetryStopCtxCancelled
			break
		}
		if time.Since(start) >= budget {
			stopReason = RetryStopBudgetExceeded
			break
		}
		if isPermanentToolError(toolResult) {
			stopReason = RetryStopPermanent
			break
		}
		if havePrev && toolResult == prevToolResult {
			stopReason = RetryStopNoProgress
			break
		}
		prevToolResult = toolResult
		havePrev = true
		attempts++

		emitRetry(cfg.EventHandler, attempts, cfg.MaxToolErrorRetries, toolName, excerpt)

		next, askErr := session.Ask(ctx, buildRetryPrompt())
		if askErr != nil {
			return nil, attempts, stopReason, askErr
		}
		result = next
	}
	return result, attempts, stopReason, nil
}

func (p *ClaudeProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...ExecuteOption) (*AgentResult, error) {
	cfg := applyOptions(opts)
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	// Build full prompt with worktree context
	fullPrompt := prompt
	if wtCtx != nil {
		fullPrompt = wtCtx.FormatForPrompt() + "\n\n" + prompt
	}

	// Map to Claude session options: provider-level opts first, then per-call overrides
	sessionOpts := append([]claude.SessionOption{}, p.sessionOpts...)
	sessionOpts = append(sessionOpts,
		claude.WithModel(cfg.Model),
	)
	if cfg.KeepUserSettings {
		sessionOpts = append(sessionOpts, claude.WithKeepUserSettings())
	}
	if cfg.WorkDir != "" {
		sessionOpts = append(sessionOpts, claude.WithWorkDir(cfg.WorkDir))
	}
	if cfg.SystemPrompt != "" {
		sessionOpts = append(sessionOpts, claude.WithSystemPrompt(cfg.SystemPrompt))
	}
	switch cfg.PermissionMode {
	case "bypass":
		sessionOpts = append(sessionOpts, claude.WithPermissionMode(claude.PermissionModeBypass))
	case "plan":
		sessionOpts = append(sessionOpts, claude.WithPermissionMode(claude.PermissionModePlan))
	}
	if cfg.MaxTurns > 0 {
		sessionOpts = append(sessionOpts, claude.WithMaxTurns(cfg.MaxTurns))
	}
	if cfg.MaxBudgetUSD > 0 {
		sessionOpts = append(sessionOpts, claude.WithMaxBudgetUSD(cfg.MaxBudgetUSD))
	}
	if cfg.Effort != "" {
		sessionOpts = append(sessionOpts, claude.WithEffort(claudeEffortLevel(cfg.Effort)))
	}
	if cfg.ResumeSessionID != "" {
		sessionOpts = append(sessionOpts, claude.WithResume(cfg.ResumeSessionID))
	}
	if !cfg.LLMEndpoint.IsZero() {
		sessionOpts = append(sessionOpts, claude.WithLLMEndpoint(cfg.LLMEndpoint))
	}

	// Create ephemeral session
	session := claude.NewSession(sessionOpts...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}
	defer session.Stop()

	// streamTurn drives one logical turn against the session using the raw
	// event stream. Replaces the old session.Ask path: consume events until
	// logicalTurnState.LogicalTurnDone() flips, feeding EventHandler + the
	// AgentEvent channel along the way.
	streamTurn := func(ctx context.Context, prompt string) (*claude.TurnResult, error) {
		if err := session.Query(ctx, prompt); err != nil {
			return nil, err
		}
		return consumeTurnEvents(ctx, session.Events(), streamTurnGracePeriod, cfg.Logger,
			func(ev claude.Event) {
				dispatchClaudeEvent(ev, cfg.EventHandler, p.events)
			})
	}

	result, err := streamTurn(ctx, fullPrompt)
	if err != nil {
		partial := &AgentResult{}
		if info := session.Info(); info != nil {
			partial.SessionID = info.SessionID
		}
		return partial, err
	}

	result, attempts, stopReason, err := runRetryLoop(ctx, streamTurnSession{fn: streamTurn}, result, cfg)
	if err != nil {
		partial := &AgentResult{}
		if info := session.Info(); info != nil {
			partial.SessionID = info.SessionID
		}
		return partial, err
	}

	agentResult := nonNilAgentResult(claudeResultToAgentResultWithRetryAbort(result, cfg, attempts, stopReason))
	if info := session.Info(); info != nil {
		agentResult.SessionID = info.SessionID
	}
	return agentResult, nil
}

func (p *ClaudeProvider) Events() <-chan AgentEvent { return p.events }

func (p *ClaudeProvider) Close() error {
	close(p.events)
	return nil
}

// ClaudeLongRunningProvider wraps a persistent Claude session.
type ClaudeLongRunningProvider struct {
	eventHandler EventHandler
	*ClaudeProvider
	session     *claude.Session
	sessionOpts []claude.SessionOption
}

// NewClaudeLongRunningProvider creates a Claude provider with a persistent session.
func NewClaudeLongRunningProvider(sessionOpts ...claude.SessionOption) *ClaudeLongRunningProvider {
	return &ClaudeLongRunningProvider{
		ClaudeProvider: NewClaudeProvider(),
		sessionOpts:    sessionOpts,
	}
}

func (p *ClaudeLongRunningProvider) Start(ctx context.Context) error {
	p.session = claude.NewSession(p.sessionOpts...)
	if err := p.session.Start(ctx); err != nil {
		return err
	}
	if p.eventHandler != nil {
		go bridgeEvents(p.session.Events(), p.eventHandler, p.events, nil, "", nil)
	}
	return nil
}

// SendMessage issues one Ask on the persistent session. Unlike Execute, it
// does not run the retry loop — tool-error retries are not applied here.
func (p *ClaudeLongRunningProvider) SendMessage(ctx context.Context, message string) (*AgentResult, error) {
	result, err := p.session.Ask(ctx, message)
	if err != nil {
		return nil, err
	}
	return nonNilAgentResult(ClaudeResultToAgentResult(result)), nil
}

func (p *ClaudeLongRunningProvider) Stop() error {
	if p.session != nil {
		return p.session.Stop()
	}
	return nil
}

// dispatchClaudeEvent fans a single claude.Event out to an EventHandler and/or
// AgentEvent channel. Execute drives this synchronously so it can feed
// logicalTurnState on the same goroutine — long-running providers use
// bridgeEvents instead.
//
// Only events that implement agentstream.Event (Ready, Text, Thinking,
// ToolStart, ToolEnd, TurnComplete, Error) are bridged to the generic
// AgentEvent surface. Claude-specific raw events (ResultMessageEvent,
// AssistantMessageEvent, UserMessageEvent, Task*) are intentionally NOT
// forwarded: they are consumed internally by logicalTurnState to decide when
// the logical turn is done, and the agentstream contract is a deliberately
// smaller, cross-provider subset. Consumers that need the raw Claude event
// stream should read claude.Session.Events() directly instead of relying on
// Execute's AgentEvent channel/EventHandler.
func dispatchClaudeEvent(ev claude.Event, handler EventHandler, out chan<- AgentEvent) {
	sev, ok := any(ev).(agentstream.Event)
	if !ok {
		return
	}
	dispatchStreamEvent(sev, handler, out)
}

func claudeResultToAgentResultWithRetryAbort(result *claude.TurnResult, cfg ExecuteConfig, attempts int, stopReason string) *AgentResult {
	agentResult := ClaudeResultToAgentResult(result)
	if agentResult == nil || cfg.MaxToolErrorRetries <= 0 {
		return agentResult
	}
	toolName, toolResult, excerpt, ok := claude.FinalTurnToolErrorDetails(result.ContentBlocks)
	if !ok {
		return agentResult
	}
	if stopReason == RetryStopPermanent {
		excerpt = permanentToolErrorExcerpt(toolResult, excerpt)
	}
	unresolved := &UnresolvedToolError{
		Tool:     toolName,
		Excerpt:  excerpt,
		Reason:   stopReason,
		Attempts: attempts,
		Max:      cfg.MaxToolErrorRetries,
	}
	agentResult.UnresolvedToolError = unresolved
	agentResult.Text = AppendUnresolvedToolErrorMarker(agentResult.Text, *unresolved)
	// Fire the abort callback once per loop execution that stopped with a
	// tool error still present, including permanent errors skipped pre-retry.
	emitRetryAbort(cfg.EventHandler, stopReason, toolName, excerpt)
	return agentResult
}

// claudeEffortLevel maps the neutral agent.EffortLevel to claude.EffortLevel.
// The strings happen to match today, but the conversion is explicit so a
// future divergence does not silently break the wiring.
func claudeEffortLevel(level EffortLevel) claude.EffortLevel {
	switch level {
	case EffortAuto:
		return claude.EffortAuto
	case EffortLow:
		return claude.EffortLow
	case EffortMedium:
		return claude.EffortMed
	case EffortHigh:
		return claude.EffortHigh
	case EffortMax:
		return claude.EffortMax
	}
	panic(fmt.Sprintf("BUG: unhandled EffortLevel %q in claudeEffortLevel", level))
}

// ClaudeResultToAgentResult converts a claude.TurnResult to AgentResult.
func ClaudeResultToAgentResult(r *claude.TurnResult) *AgentResult {
	if r == nil {
		return nil
	}
	return &AgentResult{
		Text:          r.Text,
		Thinking:      r.Thinking,
		Success:       r.Success,
		Error:         r.Error,
		DurationMs:    r.DurationMs,
		ContentBlocks: claudeBlocksToAgentBlocks(r.ContentBlocks),
		Usage: AgentUsage{
			InputTokens:     r.Usage.InputTokens,
			OutputTokens:    r.Usage.OutputTokens,
			CacheReadTokens: r.Usage.CacheReadTokens,
			CostUSD:         r.Usage.CostUSD,
		},
	}
}

// claudeBlocksToAgentBlocks maps protocol content blocks to the
// provider-agnostic AgentContentBlock representation. Unknown block types are
// preserved with their declared type and a textual rendering so downstream
// consumers don't silently drop content the protocol layer kept.
func claudeBlocksToAgentBlocks(blocks protocol.ContentBlocks) []AgentContentBlock {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]AgentContentBlock, 0, len(blocks))
	for _, block := range blocks {
		switch b := block.(type) {
		case protocol.TextBlock:
			out = append(out, AgentContentBlock{
				Type: string(protocol.ContentBlockTypeText),
				Text: b.Text,
			})
		case protocol.ThinkingBlock:
			out = append(out, AgentContentBlock{
				Type: string(protocol.ContentBlockTypeThinking),
				Text: b.Thinking,
			})
		case protocol.ToolUseBlock:
			out = append(out, AgentContentBlock{
				Type:      string(protocol.ContentBlockTypeToolUse),
				ToolName:  b.Name,
				ToolInput: b.Input,
			})
		case protocol.ToolResultBlock:
			isError := b.IsError != nil && *b.IsError
			out = append(out, AgentContentBlock{
				Type:       string(protocol.ContentBlockTypeToolResult),
				ToolResult: b.Content,
				IsError:    isError,
			})
		case protocol.UnknownContentBlock:
			out = append(out, AgentContentBlock{
				Type: string(b.Type),
				Text: b.DisplayString(),
			})
		default:
			// A future protocol.ContentBlock implementation that this switch
			// hasn't been updated for. Preserve at least the declared type
			// so downstream consumers can see something landed rather than
			// silently dropping the block.
			out = append(out, AgentContentBlock{
				Type: string(block.BlockType()),
			})
		}
	}
	return out
}
