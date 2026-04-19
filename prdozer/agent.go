package prdozer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// PolishRunner runs the /pr-polish skill for a single PR. The interface lets
// tests substitute a fake without standing up a real Claude session.
type PolishRunner interface {
	Run(ctx context.Context, req PolishRequest) (PolishResult, error)
}

// PolishRequest carries everything the polish runner needs.
type PolishRequest struct {
	WorkDir  string
	Model    string
	Cfg      PolishConfig
	PRNumber int
	Local    bool
}

// PolishResult captures what came out of the polish session.
type PolishResult struct {
	SessionID  string
	Output     string
	DurationMs int64
}

// AgentPolisher invokes /pr-polish through multiagent/agent — the same path
// jiradozer uses to drive Claude sessions.
type AgentPolisher struct {
	renderer *render.Renderer
	logger   *slog.Logger
}

func NewAgentPolisher(renderer *render.Renderer, logger *slog.Logger) *AgentPolisher {
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentPolisher{renderer: renderer, logger: logger}
}

func (p *AgentPolisher) Run(ctx context.Context, req PolishRequest) (PolishResult, error) {
	model, ok := agent.ModelByID(req.Model)
	if !ok {
		return PolishResult{}, fmt.Errorf("unknown model %q", req.Model)
	}
	provider, err := agent.NewProviderForModel(model)
	if err != nil {
		return PolishResult{}, fmt.Errorf("create provider: %w", err)
	}
	defer provider.Close()

	prompt := buildPolishPrompt(req.PRNumber, req.Local)

	logger := p.logger
	logger.Info("invoking pr-polish",
		"pr", req.PRNumber,
		"local", req.Local,
		"model", req.Model,
		"work_dir", req.WorkDir,
	)
	logger.Debug("pr-polish prompt", "prompt", prompt)

	logHandler := newPolishLogHandler(logger, req.PRNumber)
	var handler agent.EventHandler = logHandler
	if p.renderer != nil {
		handler = &compositeHandler{handlers: []agent.EventHandler{logHandler, &rendererHandler{r: p.renderer}}}
	}

	opts := []agent.ExecuteOption{
		agent.WithProviderWorkDir(req.WorkDir),
		agent.WithProviderPermissionMode("bypass"),
		agent.WithProviderModel(req.Model),
		agent.WithProviderKeepUserSettings(),
		agent.WithProviderEventHandler(handler),
	}
	if req.Cfg.MaxTurns > 0 {
		opts = append(opts, agent.WithProviderMaxTurns(req.Cfg.MaxTurns))
	}
	if req.Cfg.MaxBudgetUSD > 0 {
		opts = append(opts, agent.WithProviderMaxBudgetUSD(req.Cfg.MaxBudgetUSD))
	}

	start := time.Now()
	result, err := provider.Execute(ctx, prompt, nil, opts...)
	if err != nil {
		return PolishResult{}, fmt.Errorf("agent execution: %w", err)
	}
	if !result.Success {
		if result.Error != nil {
			return PolishResult{}, result.Error
		}
		return PolishResult{}, fmt.Errorf("pr-polish session failed (no error returned)")
	}
	logger.Info("pr-polish completed",
		"pr", req.PRNumber,
		"session_id", result.SessionID,
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"cost_usd", result.Usage.CostUSD,
		"duration_ms", result.DurationMs,
	)
	return PolishResult{
		SessionID:  result.SessionID,
		Output:     result.Text,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

func buildPolishPrompt(prNumber int, local bool) string {
	var sb strings.Builder
	sb.WriteString("/pr-polish")
	if local {
		sb.WriteString(" --local")
	}
	if prNumber > 0 {
		sb.WriteString(fmt.Sprintf(" %d", prNumber))
	}
	return sb.String()
}

// polishLogHandler mirrors jiradozer's logEventHandler shape.
type polishLogHandler struct {
	logger     *slog.Logger
	toolStarts map[string]time.Time
	textBuf    strings.Builder
	pr         int
}

func newPolishLogHandler(logger *slog.Logger, prNumber int) *polishLogHandler {
	return &polishLogHandler{
		logger:     logger,
		pr:         prNumber,
		toolStarts: make(map[string]time.Time),
	}
}

func (h *polishLogHandler) flushText() {
	if h.textBuf.Len() > 0 {
		h.logger.Debug("agent text", "pr", h.pr, "text", truncate(h.textBuf.String(), 200))
		h.textBuf.Reset()
	}
}

func (h *polishLogHandler) OnSessionInit(sessionID string) {
	h.logger.Info("agent session init", "pr", h.pr, "session_id", sessionID)
}

func (h *polishLogHandler) OnText(text string) {
	h.textBuf.WriteString(text)
	if strings.Contains(text, "\n") || h.textBuf.Len() > 200 {
		h.flushText()
	}
}

func (h *polishLogHandler) OnThinking(thinking string) {
	h.flushText()
	h.logger.Debug("agent thinking", "pr", h.pr, "thinking", truncate(thinking, 200))
}

func (h *polishLogHandler) OnToolStart(name, id string, _ map[string]interface{}) {
	h.flushText()
	h.toolStarts[id] = time.Now()
}

func (h *polishLogHandler) OnToolComplete(name, id string, input map[string]interface{}, _ interface{}, isError bool) {
	attrs := []any{"pr", h.pr, "tool", name}
	if summary := render.FormatToolInput(name, input); summary != "" {
		attrs = append(attrs, "input", summary)
	}
	if start, ok := h.toolStarts[id]; ok {
		attrs = append(attrs, "duration", time.Since(start).Round(100*time.Millisecond))
		delete(h.toolStarts, id)
	}
	if isError {
		attrs = append(attrs, "error", true)
	}
	h.logger.Debug("tool", attrs...)
}

func (h *polishLogHandler) OnTurnComplete(turn int, success bool, durationMs int64, costUSD float64) {
	h.flushText()
	h.logger.Debug("turn complete",
		"pr", h.pr,
		"turn", turn,
		"success", success,
		"duration", fmt.Sprintf("%.1fs", float64(durationMs)/1000),
		"cost", fmt.Sprintf("$%.4f", costUSD),
	)
}

func (h *polishLogHandler) OnError(err error, ctx string) {
	h.flushText()
	clear(h.toolStarts)
	h.logger.Debug("agent error", "pr", h.pr, "error", err, "context", ctx)
}

func (h *polishLogHandler) OnRetry(attempt, max int, tool, _ string) {
	h.flushText()
	h.logger.Info("retry on tool error", "pr", h.pr, "attempt", attempt, "max", max, "tool", tool)
}

func (h *polishLogHandler) OnRetryAbort(reason, tool, _ string) {
	h.flushText()
	h.logger.Info("retry loop aborted", "pr", h.pr, "reason", reason, "tool", tool)
}

type rendererHandler struct {
	r *render.Renderer
}

func (h *rendererHandler) OnText(text string)  { h.r.Text(text) }
func (h *rendererHandler) OnThinking(t string) { h.r.Thinking(t) }
func (h *rendererHandler) OnToolStart(name, id string, _ map[string]interface{}) {
	h.r.ToolStart(name, id)
}
func (h *rendererHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	h.r.ToolComplete(name, input)
	if result != nil || isError {
		h.r.ToolResult(result, isError)
	}
}
func (h *rendererHandler) OnTurnComplete(turn int, success bool, durationMs int64, costUSD float64) {
	h.r.TurnSummary(turn, success, durationMs, costUSD)
}
func (h *rendererHandler) OnError(err error, ctx string) { h.r.Error(err, ctx) }
func (h *rendererHandler) OnRetry(attempt, max int, tool, _ string) {
	h.r.Status(fmt.Sprintf("Retry %d/%d: tool error in %s", attempt, max, tool))
}
func (h *rendererHandler) OnRetryAbort(reason, tool, _ string) {
	h.r.Status(fmt.Sprintf("Retry loop aborted (%s) on tool %s", reason, tool))
}

type compositeHandler struct {
	handlers []agent.EventHandler
}

func (c *compositeHandler) OnText(text string) {
	for _, h := range c.handlers {
		h.OnText(text)
	}
}
func (c *compositeHandler) OnThinking(t string) {
	for _, h := range c.handlers {
		h.OnThinking(t)
	}
}
func (c *compositeHandler) OnToolStart(name, id string, input map[string]interface{}) {
	for _, h := range c.handlers {
		h.OnToolStart(name, id, input)
	}
}
func (c *compositeHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	for _, h := range c.handlers {
		h.OnToolComplete(name, id, input, result, isError)
	}
}
func (c *compositeHandler) OnTurnComplete(turn int, success bool, durationMs int64, costUSD float64) {
	for _, h := range c.handlers {
		h.OnTurnComplete(turn, success, durationMs, costUSD)
	}
}
func (c *compositeHandler) OnError(err error, ctx string) {
	for _, h := range c.handlers {
		h.OnError(err, ctx)
	}
}
func (c *compositeHandler) OnSessionInit(sessionID string) {
	for _, h := range c.handlers {
		if sh, ok := h.(agent.SessionInitHandler); ok {
			sh.OnSessionInit(sessionID)
		}
	}
}
func (c *compositeHandler) OnRetry(attempt, max int, tool, excerpt string) {
	for _, h := range c.handlers {
		if rh, ok := h.(agent.RetryHandler); ok {
			rh.OnRetry(attempt, max, tool, excerpt)
		}
	}
}
func (c *compositeHandler) OnRetryAbort(reason, tool, excerpt string) {
	for _, h := range c.handlers {
		if rh, ok := h.(agent.RetryHandler); ok {
			rh.OnRetryAbort(reason, tool, excerpt)
		}
	}
}

func truncate(s string, maxLen int) string {
	r := []rune(s)
	if len(r) > maxLen {
		return string(r[:maxLen]) + "..."
	}
	return s
}
