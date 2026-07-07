package jiradozer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// PromptData is the template context for rendering step prompts.
type PromptData struct {
	Identifier  string // e.g. "ENG-123"
	Title       string
	Description string // empty string if issue.Description is nil
	URL         string // empty string if issue.URL is nil
	Labels      string // comma-separated
	BaseBranch  string // e.g. "main"
	Plan        string // plan output from the planning step
	BuildOutput string // build output from the build step
}

// Truncate shortens s to at most maxLen runes (rune-safe, never splits a
// multibyte character), appending "..." when truncation occurs.
func Truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen]) + "..."
	}
	return s
}

type StepAgentResult struct {
	Output    string
	SessionID string
}

// sessionLimitSkipPct is the plan-utilization threshold (0–100) at or above
// which a Claude model is treated as exhausted during pre-flight and skipped in
// favor of the next fallback. Set just below 100 so a model that is effectively
// out — but not yet erroring — is bypassed before launching a long run.
const sessionLimitSkipPct = 98.0

type agentRunner struct {
	newProviderForModel func(agent.AgentModel) (agent.Provider, error)
	// claudeUtilization reads the max active plan-limit utilization (0–100) for
	// a Claude model, returning ok=false for non-Claude models or when usage is
	// unavailable. Injectable so tests can drive the pre-flight without network.
	claudeUtilization func(context.Context, agent.AgentModel) (float64, bool)
	retryBackoffs     []time.Duration
}

var defaultAgentRunner = agentRunner{
	newProviderForModel: agent.NewProviderForModel,
	claudeUtilization: func(ctx context.Context, m agent.AgentModel) (float64, bool) {
		return agent.ClaudeSessionUtilization(ctx, m)
	},
	// A single shared schedule. Overload/5xx and other transients reuse the
	// last entry once attempts exceed the slice (see retryBackoff), so a longer
	// tail gives genuine cool-down for sustained provider overload (#273).
	retryBackoffs: []time.Duration{30 * time.Second, 90 * time.Second, 3 * time.Minute, 5 * time.Minute},
}

// RunStepAgent runs an agent session for the given workflow step and returns
// the StepAgentResult.
// On first execution (resumeSessionID == ""), the prompt template is rendered with issue data.
// On follow-up (resumeSessionID != ""), feedback is sent directly to the resumed session.
// If renderer is non-nil, agent events are streamed to the terminal.
func RunStepAgent(ctx context.Context, stepName string, data PromptData, cfg StepConfig, workDir string, feedback string, resumeSessionID string, renderer *render.Renderer, logger *slog.Logger) (StepAgentResult, error) {
	prompt, err := resolvePromptForExecution(stepName, cfg.Prompt, data, feedback, resumeSessionID)
	if err != nil {
		return StepAgentResult{}, fmt.Errorf("render %s prompt: %w", stepName, err)
	}
	return defaultAgentRunner.runAgent(ctx, stepName, prompt, cfg, workDir, resumeSessionID, renderer, logger)
}

// RunCommand runs a shell command template for the given workflow step.
// The commandTmpl is rendered with data, then executed via sh -c in workDir.
// Returns the combined stdout+stderr output and any error.
//
// Security note: command templates are rendered with PromptData fields (Title,
// Description, etc.) which originate from the issue tracker and are
// user-controlled. Interpolating these fields directly into shell commands can
// allow shell injection. Only include tracker fields in command templates when
// the issue source is fully trusted (e.g. an internal tracker with restricted
// write access). Avoid interpolating free-text fields such as Title or
// Description unless you control all issue authors.
func RunCommand(ctx context.Context, stepName string, data PromptData, commandTmpl string, workDir string, logger *slog.Logger) (string, error) {
	rendered, err := renderPrompt(commandTmpl, data)
	if err != nil {
		return "", fmt.Errorf("render %s command: %w", stepName, err)
	}

	logger.Info("running command", "step", stepName, "command", Truncate(rendered, 200), "work_dir", workDir)

	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", rendered)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		logger.Info("command failed", "step", stepName, "duration", time.Since(start), "error", err)
		return output, fmt.Errorf("command failed: %w", err)
	}

	logger.Info("command completed", "step", stepName, "duration", time.Since(start), "output", Truncate(output, 200))
	return output, nil
}

// resolvePromptForExecution determines the prompt to send to the agent.
// On first execution (no resume session), configPrompt is required; users
// supply prompts in their YAML (run `jiradozer bootstrap` to scaffold one).
func resolvePromptForExecution(stepName, configPrompt string, data PromptData, feedback, resumeSessionID string) (string, error) {
	// Resume: send feedback directly as the prompt.
	if resumeSessionID != "" && feedback != "" {
		return feedback, nil
	}

	if configPrompt == "" {
		return "", fmt.Errorf("%s: prompt is required (run `jiradozer bootstrap` to generate a starter config)", stepName)
	}

	// First execution: render template.
	prompt, err := renderPrompt(configPrompt, data)
	if err != nil {
		return "", err
	}

	// Fallback: no session to resume but have feedback — append it.
	if feedback != "" {
		prompt += "\n\nPrevious feedback to incorporate:\n" + feedback
	}
	return prompt, nil
}

// logEventHandler logs agent events to the log file with high signal-to-noise ratio.
// Text is accumulated and flushed at semantic boundaries. Tool start/complete are
// merged into a single log line with input summary and duration. Tool IDs are omitted.
type logEventHandler struct {
	logger       *slog.Logger
	toolStarts   map[string]time.Time
	step         string
	provider     string
	planFilePath string
	lastWriteMD  string
	textBuf      strings.Builder
}

func newLogEventHandler(logger *slog.Logger, step, provider string) *logEventHandler {
	return &logEventHandler{
		logger:     logger,
		step:       step,
		provider:   provider,
		toolStarts: make(map[string]time.Time),
	}
}

// providerReportsCost reports whether a provider's turn/result events carry a
// real cost measurement. Only the Claude provider does today; codex, cursor,
// gemini and agy emit a structural zero. Logging that zero as "$0.0000" reads
// like a measurement, so callers log "n/a" instead.
func providerReportsCost(provider string) bool {
	return provider == agent.ProviderClaude
}

// providerReportsTokens reports whether a provider's result carries real
// input/output token counts. Claude and codex both do (codex populates
// AgentResult.Usage from its token_count events); cursor, gemini and agy
// leave Usage zero. This is intentionally distinct from providerReportsCost:
// codex reports tokens but not cost, so the two metrics need separate gates.
func providerReportsTokens(provider string) bool {
	return provider == agent.ProviderClaude || provider == agent.ProviderCodex
}

// usageLogAttr returns a single slog key/value pair: the measured value when
// reported is true, the literal "n/a" otherwise. Callers pick `reported` from
// providerReportsCost or providerReportsTokens depending on the metric, so a
// real measurement is never mislabelled "n/a" and a structural zero never
// reads like a measurement.
func usageLogAttr(reported bool, key string, value any) []any {
	if reported {
		return []any{key, value}
	}
	return []any{key, "n/a"}
}

// resetPerAttempt clears state that must not leak between retry attempts.
// Plan-file detection (planFilePath/lastWriteMD) and pending tool-start
// timestamps belong to a single agent run; carrying them across attempts can
// surface a stale plan file via resolveOutput when a retry doesn't write one.
func (h *logEventHandler) resetPerAttempt() {
	h.planFilePath = ""
	h.lastWriteMD = ""
	h.toolStarts = make(map[string]time.Time)
	h.textBuf.Reset()
}

func (h *logEventHandler) OnSessionInit(sessionID string) {
	h.logger.Info("agent session init", "step", h.step, "session_id", sessionID)
}

// flushText logs accumulated text and resets the buffer.
func (h *logEventHandler) flushText() {
	if h.textBuf.Len() > 0 {
		h.logger.Debug("agent text", "step", h.step, "text", Truncate(h.textBuf.String(), 200))
		h.textBuf.Reset()
	}
}

func (h *logEventHandler) OnText(text string) {
	h.textBuf.WriteString(text)
	if strings.Contains(text, "\n") || h.textBuf.Len() > 200 {
		h.flushText()
	}
}

func (h *logEventHandler) OnThinking(thinking string) {
	h.flushText()
	h.logger.Debug("agent thinking", "step", h.step, "thinking", Truncate(thinking, 200))
}

func (h *logEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	h.flushText()
	h.toolStarts[id] = time.Now()
}

func (h *logEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	attrs := []any{"step", h.step, "tool", name}
	if inputSummary := render.FormatToolInput(name, input); inputSummary != "" {
		attrs = append(attrs, "input", inputSummary)
	}
	if start, ok := h.toolStarts[id]; ok {
		attrs = append(attrs, "duration", time.Since(start).Round(100*time.Millisecond))
		delete(h.toolStarts, id)
	}
	if isError {
		attrs = append(attrs, "error", true)
	}
	h.logger.Debug("tool", attrs...)

	// Track .md file writes — the last one before ExitPlanMode is the plan file.
	if name == "Write" && !isError {
		if filePath, ok := input["file_path"].(string); ok {
			if strings.HasSuffix(filePath, ".md") {
				h.lastWriteMD = filePath
			}
		}
	}
	if name == "ExitPlanMode" && h.lastWriteMD != "" {
		h.planFilePath = h.lastWriteMD
		h.logger.Debug("plan file confirmed", "step", h.step, "path", h.planFilePath)
	}
}

func (h *logEventHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
	h.flushText()
	attrs := []any{
		"step", h.step,
		"provider", h.provider,
		"turn", turnNumber,
		"success", success,
		"duration", fmt.Sprintf("%.1fs", float64(durationMs)/1000),
	}
	attrs = append(attrs, usageLogAttr(providerReportsCost(h.provider), "cost", fmt.Sprintf("$%.4f", costUSD))...)
	h.logger.Debug("turn complete", attrs...)
}

func (h *logEventHandler) OnError(err error, context string) {
	h.flushText()
	clear(h.toolStarts)
	h.logger.Debug("agent error", "step", h.step, "error", err, "context", context)
}

func (h *logEventHandler) OnRetry(attempt, max int, tool, excerpt string) {
	h.flushText()
	h.logger.Info("retry on tool error",
		"step", h.step,
		"attempt", attempt,
		"max", max,
		"tool", tool,
	)
	// Excerpt is derived from raw tool output and may contain paths,
	// command lines, or other sensitive material, so keep it at Debug.
	h.logger.Debug("retry on tool error excerpt",
		"step", h.step,
		"attempt", attempt,
		"tool", tool,
		"excerpt", excerpt,
	)
}

func (h *logEventHandler) OnRetryAbort(reason, tool, excerpt string) {
	h.flushText()
	h.logger.Info("retry loop aborted",
		"step", h.step,
		"reason", reason,
		"tool", tool,
	)
	h.logger.Debug("retry loop aborted excerpt",
		"step", h.step,
		"reason", reason,
		"tool", tool,
		"excerpt", excerpt,
	)
}

// rendererEventHandler adapts agent.EventHandler to a render.Renderer for
// terminal display.
type rendererEventHandler struct {
	r *render.Renderer
}

func (h *rendererEventHandler) OnText(text string) {
	h.r.Text(text)
}

func (h *rendererEventHandler) OnThinking(thinking string) {
	h.r.Thinking(thinking)
}

func (h *rendererEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	h.r.ToolStart(name, id)
}

func (h *rendererEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	h.r.ToolComplete(name, input)
	if result != nil || isError {
		h.r.ToolResultForTool(name, id, result, isError)
	}
}

func (h *rendererEventHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
	h.r.TurnSummary(turnNumber, success, durationMs, costUSD)
}

func (h *rendererEventHandler) OnError(err error, ctx string) {
	h.r.Error(err, ctx)
}

func (h *rendererEventHandler) OnRetry(attempt, max int, tool, _ string) {
	h.r.Status(fmt.Sprintf("Retry %d/%d: tool error in %s", attempt, max, tool))
}

func (h *rendererEventHandler) OnRetryAbort(reason, tool, _ string) {
	h.r.Status(fmt.Sprintf("Retry loop aborted (%s) on tool %s", reason, tool))
}

// compositeEventHandler fans out events to multiple handlers.
type compositeEventHandler struct {
	handlers []agent.EventHandler
}

func (c *compositeEventHandler) OnText(text string) {
	for _, h := range c.handlers {
		h.OnText(text)
	}
}

func (c *compositeEventHandler) OnThinking(thinking string) {
	for _, h := range c.handlers {
		h.OnThinking(thinking)
	}
}

func (c *compositeEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	for _, h := range c.handlers {
		h.OnToolStart(name, id, input)
	}
}

func (c *compositeEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	for _, h := range c.handlers {
		h.OnToolComplete(name, id, input, result, isError)
	}
}

func (c *compositeEventHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
	for _, h := range c.handlers {
		h.OnTurnComplete(turnNumber, success, durationMs, costUSD)
	}
}

func (c *compositeEventHandler) OnError(err error, ctx string) {
	for _, h := range c.handlers {
		h.OnError(err, ctx)
	}
}

func (c *compositeEventHandler) OnSessionInit(sessionID string) {
	for _, h := range c.handlers {
		if sh, ok := h.(agent.SessionInitHandler); ok {
			sh.OnSessionInit(sessionID)
		}
	}
}

func (c *compositeEventHandler) OnRetry(attempt, max int, tool, excerpt string) {
	for _, h := range c.handlers {
		if rh, ok := h.(agent.RetryHandler); ok {
			rh.OnRetry(attempt, max, tool, excerpt)
		}
	}
}

func (c *compositeEventHandler) OnRetryAbort(reason, tool, excerpt string) {
	for _, h := range c.handlers {
		if rh, ok := h.(agent.RetryHandler); ok {
			rh.OnRetryAbort(reason, tool, excerpt)
		}
	}
}

// runAgent runs an agent with the given prompt and step configuration.
// If renderer is non-nil, agent events are rendered to the terminal in addition
// to being logged to the log file.
func (r agentRunner) runAgent(ctx context.Context, stepName, prompt string, cfg StepConfig, workDir string, resumeSessionID string, renderer *render.Renderer, logger *slog.Logger) (StepAgentResult, error) {
	if renderer != nil {
		defer renderer.Reset()
	}

	// Outer loop: the primary model followed by any configured fallbacks. A
	// model is advanced only on a workspace-wide out-of-credits failure (a
	// same-model retry can't refill it). Failover starts a fresh session —
	// cross-provider resume is not reliable — so currentResume is reset.
	models := append([]string{cfg.Model}, cfg.FallbackModels...)
	currentResume := resumeSessionID
	var lastErr error
	var lastResult StepAgentResult
	for mi, modelID := range models {
		activeCfg := cfg
		activeCfg.Model = modelID

		// Pre-flight: if this is a Claude model whose plan is already at/over the
		// limit, don't burn a 40–50 min run that dies at the last step — skip
		// straight to the next fallback with a fresh session. Fails open: any
		// error or non-Claude model leaves claudeNearLimit false. The last model
		// is never skipped (nothing to fall back to).
		if mi < len(models)-1 && r.claudeNearLimit(ctx, activeCfg, stepName, logger) {
			next := models[mi+1]
			logger.Warn("claude plan near limit; skipping to fallback",
				"step", stepName,
				"from", modelID,
				"to", next,
				"threshold", sessionLimitSkipPct,
			)
			if renderer != nil {
				renderer.Status(fmt.Sprintf("%s near plan limit; skipping to %s", modelID, next))
			}
			currentResume = ""
			continue
		}

		res, outOfCredits, err := r.runAgentForModel(ctx, stepName, prompt, activeCfg, workDir, currentResume, renderer, logger)
		if err == nil {
			return res, nil
		}
		// A bare context cancellation is terminal regardless of model — don't
		// burn the fallback budget on a shutdown.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return res, err
		}
		lastErr, lastResult = err, res
		if outOfCredits && mi < len(models)-1 {
			next := models[mi+1]
			logger.Warn("model out of credits; falling back",
				"step", stepName,
				"from", modelID,
				"to", next,
			)
			if renderer != nil {
				renderer.Status(fmt.Sprintf("%s out of credits; falling back to %s", modelID, next))
			}
			// Fresh session on the next provider.
			currentResume = ""
			continue
		}
		return res, err
	}
	return lastResult, lastErr
}

// claudeNearLimit reports whether cfg.Model is a Claude model whose plan is at
// or above the skip threshold, so the caller should pre-emptively fall back
// rather than launch a run that will die on the limit. It fails open: a
// non-Claude provider, an unresolvable model, or unavailable usage all return
// false so a best-effort pre-flight never becomes a new failure mode.
func (r agentRunner) claudeNearLimit(ctx context.Context, cfg StepConfig, stepName string, logger *slog.Logger) bool {
	if sessionLimitSkipPct <= 0 {
		return false
	}
	model, ok := agent.ResolveModel(cfg.Model)
	if !ok || model.Provider != agent.ProviderClaude {
		return false
	}
	if r.claudeUtilization == nil {
		return false
	}
	pct, ok := r.claudeUtilization(ctx, model)
	if !ok {
		logger.Debug("claude plan usage unavailable; proceeding without pre-flight skip",
			"step", stepName, "model", cfg.Model)
		return false
	}
	logger.Debug("claude plan pre-flight",
		"step", stepName, "model", cfg.Model, "utilization", pct, "threshold", sessionLimitSkipPct)
	return pct >= sessionLimitSkipPct
}

// runAgentForModel runs the transient-retry loop for a single model. It returns
// the step result, whether the terminal error was a workspace out-of-credits
// failure (so the caller can decide to fall back to a different model), and a
// terminal error (nil on success).
func (r agentRunner) runAgentForModel(ctx context.Context, stepName, prompt string, cfg StepConfig, workDir string, resumeSessionID string, renderer *render.Renderer, logger *slog.Logger) (StepAgentResult, bool, error) {
	model, ok := agent.ResolveModel(cfg.Model)
	if !ok {
		return StepAgentResult{}, false, fmt.Errorf("unknown model: %q", cfg.Model)
	}
	provider, err := r.newProviderForModel(model)
	if err != nil {
		return StepAgentResult{}, false, fmt.Errorf("create provider: %w", err)
	}
	// Close this model's provider before returning so a fallback doesn't leak it.
	defer provider.Close()

	logAttrs := []any{
		"step", stepName,
		"mode", cfg.PermissionMode,
		"model", cfg.Model,
		"work_dir", workDir,
	}
	if resumeSessionID != "" {
		logAttrs = append(logAttrs, "resume_session_id", resumeSessionID)
	}
	logger.Info("running agent", logAttrs...)
	logger.Debug("agent prompt", "step", stepName, "prompt", Truncate(prompt, 500))

	logHandler := newLogEventHandler(logger, stepName, provider.Name())
	var handler agent.EventHandler = logHandler
	if renderer != nil {
		handler = &compositeEventHandler{handlers: []agent.EventHandler{logHandler, &rendererEventHandler{r: renderer}}}
	}

	maxRetries := cfg.TransientRetries
	if maxRetries == 0 {
		maxRetries = 4
	}

	currentResume := resumeSessionID
	attempt := 0
	var result *agent.AgentResult
	for {
		logHandler.resetPerAttempt()
		opts, err := buildExecuteOpts(cfg, workDir, handler, currentResume, logger)
		if err != nil {
			return StepAgentResult{}, false, err
		}
		result, err = provider.Execute(ctx, prompt, nil, opts...)
		if err == nil {
			if result == nil || result.Success || result.Error == nil {
				break
			}
			if result.SessionID != "" {
				currentResume = result.SessionID
			}
			if agent.IsOutOfCredits(result.Error) {
				logHandler.flushText()
				return StepAgentResult{SessionID: currentResume}, true, fmt.Errorf("agent execution: %w", result.Error)
			}
			transient, reason := agent.ClassifyTransient(result.Error)
			if !transient || attempt >= maxRetries {
				break
			}
			attempt++
			backoff := r.retryBackoff(attempt)
			logger.Warn("agent retry on transient result error",
				"step", stepName,
				"attempt", attempt,
				"max", maxRetries,
				"reason", reason,
				"session_id", currentResume,
				"backoff", backoff,
			)
			if renderer != nil {
				renderer.Status(fmt.Sprintf("Transient error (%s); retrying in %s (attempt %d/%d)", reason, backoff, attempt, maxRetries))
			}
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				logHandler.flushText()
				return StepAgentResult{SessionID: currentResume}, false, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if result != nil && result.SessionID != "" {
			currentResume = result.SessionID
		}
		if agent.IsOutOfCredits(err) {
			logHandler.flushText()
			return StepAgentResult{SessionID: currentResume}, true, fmt.Errorf("agent execution: %w", err)
		}
		transient, reason := agent.ClassifyTransient(err)
		if !transient || attempt >= maxRetries {
			logHandler.flushText()
			return StepAgentResult{SessionID: currentResume}, false, fmt.Errorf("agent execution: %w", err)
		}
		attempt++
		backoff := r.retryBackoff(attempt)
		logger.Warn("agent retry on transient error",
			"step", stepName,
			"attempt", attempt,
			"max", maxRetries,
			"reason", reason,
			"session_id", currentResume,
			"backoff", backoff,
		)
		if renderer != nil {
			renderer.Status(fmt.Sprintf("Transient error (%s); retrying in %s (attempt %d/%d)", reason, backoff, attempt, maxRetries))
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			logHandler.flushText()
			return StepAgentResult{SessionID: currentResume}, false, ctx.Err()
		case <-timer.C:
		}
	}

	if result == nil {
		logHandler.flushText()
		return StepAgentResult{SessionID: currentResume}, false, fmt.Errorf("agent execution: provider returned no result")
	}

	if !result.Success {
		logHandler.flushText()
		failed := StepAgentResult{
			SessionID: result.SessionID,
		}
		// Not out-of-credits: any such error already returned early from the
		// retry loop above (the only way here is a non-transient break, which
		// the out-of-credits check precedes).
		if result.Error != nil {
			return failed, false, fmt.Errorf("agent execution: %w", result.Error)
		}
		return failed, false, fmt.Errorf("agent failed")
	}

	completedAttrs := []any{
		"step", stepName,
		"provider", provider.Name(),
		"session_id", result.SessionID,
		"duration_ms", result.DurationMs,
	}
	reportsTokens := providerReportsTokens(provider.Name())
	completedAttrs = append(completedAttrs, usageLogAttr(reportsTokens, "input_tokens", result.Usage.InputTokens)...)
	completedAttrs = append(completedAttrs, usageLogAttr(reportsTokens, "output_tokens", result.Usage.OutputTokens)...)
	completedAttrs = append(completedAttrs, usageLogAttr(providerReportsCost(provider.Name()), "cost_usd", result.Usage.CostUSD)...)
	logger.Info("agent completed", completedAttrs...)
	if result.Text != "" {
		logger.Debug("agent response", "step", stepName, "response", Truncate(result.Text, 100))
	}

	logHandler.flushText()
	output := resolveOutput(result.Text, logHandler, logger)
	// The provider already appended the marker to result.Text. If
	// resolveOutput returned the same string, the marker is still
	// present; otherwise plan-file substitution replaced it and we
	// need to re-append so the unresolved-error signal is not lost.
	if e := result.UnresolvedToolError; e != nil && output != result.Text {
		output = agent.AppendUnresolvedToolErrorMarker(output, *e)
	}
	return StepAgentResult{
		Output:    output,
		SessionID: result.SessionID,
	}, false, nil
}

func buildExecuteOpts(cfg StepConfig, workDir string, handler agent.EventHandler, resumeSessionID string, logger *slog.Logger) ([]agent.ExecuteOption, error) {
	var opts []agent.ExecuteOption
	opts = append(opts,
		agent.WithProviderWorkDir(workDir),
		agent.WithProviderPermissionMode(cfg.PermissionMode),
		agent.WithProviderModel(cfg.Model),
		agent.WithProviderKeepUserSettings(),
		agent.WithProviderEventHandler(handler),
	)
	if logger != nil {
		opts = append(opts, agent.WithProviderLogger(logger))
	}
	if cfg.SystemPrompt != "" {
		opts = append(opts, agent.WithProviderSystemPrompt(cfg.SystemPrompt))
	}
	if cfg.MaxTurns > 0 {
		opts = append(opts, agent.WithProviderMaxTurns(cfg.MaxTurns))
	}
	if cfg.MaxToolErrorRetries > 0 {
		opts = append(opts, agent.WithProviderMaxToolErrorRetries(cfg.MaxToolErrorRetries))
	}
	if cfg.MaxBudgetUSD > 0 {
		opts = append(opts, agent.WithProviderMaxBudgetUSD(cfg.MaxBudgetUSD))
	}
	if cfg.StreamTurnGracePeriod > 0 {
		opts = append(opts, agent.WithProviderStreamTurnGracePeriod(cfg.StreamTurnGracePeriod))
	}
	if cfg.Effort != "" {
		level, err := agent.ParseEffort(cfg.Effort)
		if err != nil {
			return nil, fmt.Errorf("effort: %w", err)
		}
		opts = append(opts, agent.WithProviderEffort(level))
	}
	if resumeSessionID != "" {
		opts = append(opts, agent.WithProviderResumeSessionID(resumeSessionID))
	}
	if cfg.LLMEndpoint != nil {
		ep := cfg.LLMEndpoint.ToEndpoint()
		if !ep.IsZero() {
			if err := ep.Validate(); err != nil {
				return nil, fmt.Errorf("llm_endpoint: %w", err)
			}
			opts = append(opts, agent.WithProviderLLMEndpoint(ep))
		}
	}
	return opts, nil
}

func (r agentRunner) retryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	if len(r.retryBackoffs) == 0 {
		return 0
	}
	idx := attempt - 1
	if idx >= len(r.retryBackoffs) {
		idx = len(r.retryBackoffs) - 1
	}
	return r.retryBackoffs[idx]
}

// resolveOutput returns the plan file content if one was detected, otherwise the agent's text output.
func resolveOutput(agentText string, handler *logEventHandler, logger *slog.Logger) string {
	return resolveOutputFromPath(agentText, handler.planFilePath, logger)
}

// resolveOutputFromPath reads a plan file if the path is non-empty, falling back to agentText.
func resolveOutputFromPath(agentText, planFilePath string, logger *slog.Logger) string {
	if planFilePath == "" {
		return agentText
	}
	planContent, err := os.ReadFile(planFilePath)
	if err != nil {
		logger.Warn("could not read plan file, using agent text output", "path", planFilePath, "error", err)
		return agentText
	}
	logger.Debug("using plan file content", "path", planFilePath)
	return string(planContent)
}

// JoinRoundOutputs filters empty outputs and joins non-empty ones with a separator.
func JoinRoundOutputs(outputs []string) string {
	var nonEmpty []string
	for _, o := range outputs {
		if strings.TrimSpace(o) != "" {
			nonEmpty = append(nonEmpty, o)
		}
	}
	return strings.Join(nonEmpty, "\n\n---\n\n")
}

func NewPromptData(issue *tracker.Issue, baseBranch string) PromptData {
	// Strip jiradozer's phase bookkeeping labels (exact-match allowlist, see
	// isJiradozerLabel) so agent prompts see only user-facing labels.
	// Callers that need the full set (e.g. phase skip logic in Workflow)
	// work off a separately-tracked copy.
	userLabels := make([]string, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		if !isJiradozerLabel(l) {
			userLabels = append(userLabels, l)
		}
	}
	d := PromptData{
		Identifier: issue.Identifier,
		Title:      issue.Title,
		Labels:     strings.Join(userLabels, ", "),
		BaseBranch: baseBranch,
	}
	if issue.Description != nil {
		d.Description = *issue.Description
	}
	if issue.URL != nil {
		d.URL = *issue.URL
	}
	return d
}

// PlanFilePath returns .jiradozer/plan.md within workDir, enabling plan
// reuse across separate process invocations (e.g. plan step then build step).
func PlanFilePath(workDir string) string {
	return filepath.Join(workDir, ".jiradozer", "plan.md")
}

// LoadPersistedPlan reads plan.md from workDir. Returns "" and no error when
// the file does not exist (a missing plan is normal, not a failure). The
// returned content is trimmed.
func LoadPersistedPlan(workDir string) (string, error) {
	planPath := PlanFilePath(workDir)
	content, err := os.ReadFile(planPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read persisted plan %s: %w", planPath, err)
	}
	return strings.TrimSpace(string(content)), nil
}

// PersistPlan writes plan output to PlanFilePath so --run-step=build can load it.
// Empty output is skipped (with a warning log) to avoid overwriting a previously valid plan.
func PersistPlan(workDir, output string, logger *slog.Logger) {
	if strings.TrimSpace(output) == "" {
		logger.Warn("skipping plan persistence — output is empty")
		return
	}
	planPath := PlanFilePath(workDir)
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		logger.Warn("failed to create plan directory", "error", err)
	} else if err := os.WriteFile(planPath, []byte(output), 0o644); err != nil {
		logger.Warn("failed to persist plan", "error", err)
	} else {
		logger.Debug("persisted plan to disk", "path", planPath)
	}
}

// GenerateTitle creates a short title from the first words of a description,
// truncating at word boundaries to fit within 80 characters.
func GenerateTitle(description string) string {
	const maxLen = 80
	words := strings.Fields(description)
	var b strings.Builder
	for _, w := range words {
		if b.Len()+len(w)+1 > maxLen {
			break
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	if b.Len() == 0 && description != "" {
		if len(description) > maxLen-3 {
			return description[:maxLen-3] + "..."
		}
		return description
	}
	return b.String()
}

func renderPrompt(tmplStr string, data PromptData) (string, error) {
	return renderTemplate("prompt", tmplStr, data)
}

func renderTemplate(name, tmplStr string, data any) (string, error) {
	t, err := template.New(name).Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}
