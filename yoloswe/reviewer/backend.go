package reviewer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/framelog"
)

// heartbeatInterval bounds how often bridgeStreamEvents emits a liveness line
// to heartbeatOut while a review is in progress. A review can sit silent for
// minutes while a backend "thinks"; without a periodic pulse a healthy long
// review is indistinguishable from a hung one in the logs. Overridable in
// tests via a sub-second value. heartbeatOut defaults to os.Stderr so the line
// lands in the same stream pr-polish captures per backend (…-stderr.txt) and
// the klogfmt run log; the envelope on stdout/--envelope-file is untouched.
var (
	heartbeatInterval           = 20 * time.Second
	heartbeatOut      io.Writer = os.Stderr
)

// heartbeatWindow accumulates per-interval activity so each heartbeat reports
// what the agent actually did since the last tick (tools, streamed text)
// rather than a bare timer. It is reset every tick; toolsInFlight is tracked
// separately because it spans windows.
type heartbeatWindow struct {
	toolsCompleted []string
	textChars      int
	reasoningChars int
	events         int
}

// formatHeartbeat renders one liveness line. On a window with activity it
// summarizes the tools that completed, how many are still in flight, and the
// volume of streamed text/reasoning; on a silent window it degrades to a bare
// idle pulse so the reader still sees a pulse without false "progress".
func formatHeartbeat(elapsed time.Duration, w heartbeatWindow, toolsInFlight int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[code-review] heartbeat %s", elapsed.Round(time.Second))
	if w.events == 0 {
		b.WriteString(" · idle (awaiting backend)")
		if toolsInFlight > 0 {
			fmt.Fprintf(&b, " · %d tool(s) in flight", toolsInFlight)
		}
		return b.String()
	}
	if done := summarizeTools(w.toolsCompleted); done != "" {
		fmt.Fprintf(&b, " · done: %s", done)
	}
	if toolsInFlight > 0 {
		fmt.Fprintf(&b, " · in flight: %d", toolsInFlight)
	}
	if chars := formatCharCount(w.textChars); w.textChars > 0 {
		fmt.Fprintf(&b, " · +%s chars", chars)
	}
	if w.reasoningChars > 0 {
		fmt.Fprintf(&b, " · +%s reasoning", formatCharCount(w.reasoningChars))
	}
	return b.String()
}

// summarizeTools renders completed tool names with per-name counts, e.g.
// "Read(2),Grep". Names are sorted for deterministic output.
func summarizeTools(names []string) string {
	if len(names) == 0 {
		return ""
	}
	counts := make(map[string]int, len(names))
	order := make([]string, 0, len(names))
	for _, n := range names {
		if _, seen := counts[n]; !seen {
			order = append(order, n)
		}
		counts[n]++
	}
	sort.Strings(order)
	parts := make([]string, 0, len(order))
	for _, n := range order {
		if counts[n] > 1 {
			parts = append(parts, fmt.Sprintf("%s(%d)", n, counts[n]))
		} else {
			parts = append(parts, n)
		}
	}
	return strings.Join(parts, ",")
}

// formatCharCount renders a char tally with a k-suffix above 1000 to keep the
// heartbeat line short (e.g. 1234 -> "1.2k").
func formatCharCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// stderrPrefixHandler returns a stderr chunk handler that writes each chunk to
// os.Stderr tagged with "[prefix stderr] ", ensuring a trailing newline.
func stderrPrefixHandler(prefix string) func([]byte) {
	tag := "[" + prefix + " stderr] "
	return func(data []byte) {
		os.Stderr.WriteString(tag)
		os.Stderr.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			os.Stderr.WriteString("\n")
		}
	}
}

// Backend abstracts the agent lifecycle for different providers.
type Backend interface {
	Start(ctx context.Context) error
	Stop() error
	RunPrompt(ctx context.Context, prompt string, handler EventHandler) (*ReviewResult, error)
}

// EventHandler receives streaming events from the agent backend.
type EventHandler interface {
	OnSessionInfo(sessionID, model string)
	OnText(delta string)
	OnReasoning(delta string)
	OnToolStart(name, callID string, input map[string]interface{})
	OnToolComplete(name, callID string, input map[string]interface{}, result interface{}, isError bool)
	OnTurnComplete(success bool, durationMs int64)
	OnError(err error, context string)
}

// bridgeResult holds the outcome of bridgeStreamEvents.
type bridgeResult struct {
	// turnEvent is the raw TurnComplete event for backend-specific extraction
	// (e.g., codex token usage).
	turnEvent    agentstream.TurnComplete
	responseText string
	durationMs   int64
	success      bool
}

// bridgeStreamEvents reads SDK events from a typed channel and dispatches them
// to an EventHandler. It accumulates text deltas and returns when a TurnComplete
// or Error event is received, or the channel closes.
//
// scopeID enables filtering for multiplexed channels (e.g., codex thread ID).
// Pass "" to disable scope filtering.
//
// idleTimeout bounds how long the loop waits with NO in-scope events before
// returning a "review idle" error — an inactivity deadline (every in-scope
// event resets it), NOT a total-wall cap. Zero disables the idle check, so a
// caller that doesn't opt in keeps the prior no-inactivity-kill behavior. It is
// a per-call parameter (sourced from reviewer.Config.IdleTimeout) rather than a
// package global so one caller's opt-in (the code-review CLI) can't silently
// impose a stall policy on every other reviewer caller (e.g. yoloswe/swe.go).
//
// "In-scope" is decided by scopeID ALONE. Every event this scope receives resets
// the clock, including ones the bridge cannot render (SDK types outside the
// agentstream subset, or a conditional event returning KindUnknown). The timer
// answers "is the backend alive", and an event we can't display is still proof
// that it is. See applyEvent for the incident this distinction comes from.
func bridgeStreamEvents[E any](
	ctx context.Context,
	events <-chan E,
	handler EventHandler,
	scopeID string,
	idleTimeout time.Duration,
) (*bridgeResult, error) {
	if events == nil {
		return nil, fmt.Errorf("nil event channel")
	}

	var responseText strings.Builder

	// Liveness telemetry: a periodic, event-aware heartbeat written to
	// heartbeatOut (stderr). window accumulates activity since the last tick;
	// toolsInFlight spans windows (a tool started in one window may finish in a
	// later one). This is operator/log telemetry only — it never touches the
	// handler, the response text, or the envelope.
	start := time.Now()
	lastEvent := start
	lastHeartbeat := start
	// One ticker drives both the heartbeat and the idle check. It ticks fast
	// enough to honor idleTimeout precisely (so a small --idle-timeout behaves
	// as documented rather than at heartbeat resolution), but a heartbeat LINE
	// is only emitted every heartbeatInterval.
	tick := heartbeatInterval
	if idleTimeout > 0 && idleTimeout < tick {
		tick = idleTimeout
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	var window heartbeatWindow
	toolsInFlight := 0

	// failed is the ONLY way this function reports a failure. Every terminal
	// error path routes through it, so "does this exit preserve partial work?"
	// has exactly one answer for all of them: yes, always. The exits are
	// ctx.Done, the idle timeout, channel-close (both arms), and the KindError
	// event; `nil event channel` returns before any text can accumulate.
	// TestBridgeStreamEvents_EveryFailurePathPreservesPartialWork covers each —
	// keep a subtest there when adding an exit, or this comment becomes a claim
	// nothing checks. Four consecutive review rounds each found a different exit
	// that dropped accumulated text (idle timeout, channel close, ctx
	// cancellation, KindError) because each was
	// deciding that question for itself — the fix is one rule, not a fourth
	// case. A caller that ignores the result is unaffected; the error is
	// unchanged.
	//
	// The ctx path is the one that matters most in production: SKILL Step 3.b
	// wraps every reviewer in `timeout 2400`, GNU timeout sends SIGTERM, and
	// codereview.go installs signal.NotifyContext(SIGINT, SIGTERM) on the
	// review context — so the absolute backstop cancels here, not at the idle
	// timeout.
	failed := func(err error) (*bridgeResult, error) {
		text := responseText.String()
		if text == "" {
			// Nothing accumulated: there is no partial work to preserve, and a
			// non-nil empty result would make BuildEnvelope try to parse "".
			return nil, err
		}
		// Carry elapsed time. Token counts are only on the TurnComplete event,
		// which by definition never arrived here, so they stay zero — but the
		// wall time is known, and reporting 0ms for a review that ran twelve
		// minutes before stalling is the misreading this whole change exists to
		// stop. A partial envelope should say how long it got.
		return &bridgeResult{
			responseText: text,
			durationMs:   time.Since(start).Milliseconds(),
		}, err
	}

	// applyEvent processes one received event. done reports a terminal result
	// (TurnComplete/Error or a closed/invalid stream); inScope reports whether
	// the event counts toward liveness (only in-scope events reset the idle
	// clock — an unrelated event on a shared multiplexed channel must not keep
	// a stalled thread alive). It mutates the enclosing accumulators directly.
	applyEvent := func(ev E, ok bool) (res *bridgeResult, done bool, inScope bool, err error) {
		if !ok {
			// Channel closed without TurnComplete. Both arms go through
			// `failed` — the second is empty by construction, but exempting it
			// by reasoning is how the rule stops being checkable.
			if text := responseText.String(); text != "" {
				res, ferr := failed(fmt.Errorf(
					"session ended unexpectedly (partial response: %d chars)", len(text)))
				return res, true, false, ferr
			}
			res, ferr := failed(fmt.Errorf("session ended without result"))
			return res, true, false, ferr
		}

		// Scope filtering for multiplexed channels: an out-of-scope event is
		// another thread's traffic and must not reset our idle clock. This runs
		// FIRST and is the only thing that can deny liveness — see below.
		if scopeID != "" {
			if scoped, ok := any(ev).(agentstream.Scoped); ok {
				if id := scoped.ScopeID(); id != "" && id != scopeID {
					return nil, false, false, nil
				}
			}
		}

		// Liveness is a TRANSPORT fact, not a semantic one: any event that
		// belongs to this scope proves the backend is alive, whether or not the
		// bridge knows how to render it. Everything below only decides what to
		// DO with the event; it must never decide whether the stream is alive.
		//
		// Why this is separate from the kind switch: agentstream is a deliberate
		// common SUBSET (see agentstream/doc.go) — SDK-specific events like
		// codex.ItemStartedEvent, ItemCompletedEvent, TokenUsageEvent and
		// CommandOutputEvent intentionally don't implement agentstream.Event, and
		// conditional events legitimately return KindUnknown. Treating either as
		// "not alive" conflated "I can't render this" with "nothing happened".
		//
		// Measured on kernel#8682 r2: codex sent item/started 17s after the last
		// renderable event, then went quiet. The bridge ignored it and tripped a
		// 300s idle timer at 309s on a stream whose real silence was 293s,
		// discarding a review holding ~2M input tokens. Round 3's 132s gap
		// carried item/started + item/completed and nothing else. Fleet-wide this
		// was the largest single failure bucket (143 of 1,734 envelopes).
		sev, isStreamEvent := any(ev).(agentstream.Event)
		if !isStreamEvent {
			return nil, false, true, nil
		}
		kind := sev.StreamEventKind()
		if kind == agentstream.KindUnknown {
			return nil, false, true, nil
		}

		switch kind {
		case agentstream.KindText:
			te := sev.(agentstream.Text)
			delta := te.StreamDelta()
			responseText.WriteString(delta)
			window.textChars += len(delta)
			window.events++
			if handler != nil {
				handler.OnText(delta)
			}

		case agentstream.KindThinking:
			te := sev.(agentstream.Text)
			delta := te.StreamDelta()
			window.reasoningChars += len(delta)
			window.events++
			if handler != nil {
				handler.OnReasoning(delta)
			}

		case agentstream.KindToolStart:
			ts := sev.(agentstream.ToolStart)
			toolsInFlight++
			window.events++
			if handler != nil {
				handler.OnToolStart(ts.StreamToolName(), ts.StreamToolCallID(), ts.StreamToolInput())
			}

		case agentstream.KindToolEnd:
			te := sev.(agentstream.ToolEnd)
			if toolsInFlight > 0 {
				toolsInFlight--
			}
			window.toolsCompleted = append(window.toolsCompleted, te.StreamToolName())
			window.events++
			if handler != nil {
				handler.OnToolComplete(
					te.StreamToolName(),
					te.StreamToolCallID(),
					te.StreamToolInput(),
					te.StreamToolResult(),
					te.StreamToolIsError(),
				)
			}

		case agentstream.KindTurnComplete:
			tc := sev.(agentstream.TurnComplete)
			success := tc.StreamIsSuccess()
			durationMs := tc.StreamDuration()
			if handler != nil {
				handler.OnTurnComplete(success, durationMs)
			}
			return &bridgeResult{
				responseText: responseText.String(),
				success:      success,
				durationMs:   durationMs,
				turnEvent:    tc,
			}, true, true, nil
		}
		// KindError and any in-scope event that isn't terminal: in-scope, alive.
		if kind == agentstream.KindError {
			ee := sev.(agentstream.Error)
			if handler != nil {
				handler.OnError(ee.StreamErr(), ee.StreamErrorContext())
			}
			// Backend error events are a terminal failure like any other, so
			// they go through `failed` too. This arm was the hole that made the
			// "every terminal path" claim false for one more round.
			res, ferr := failed(fmt.Errorf("error: %w", ee.StreamErr()))
			return res, true, true, ferr
		}
		return nil, false, true, nil
	}

	// drainQueued processes every event already sitting on the channel. Both
	// the cancellation and the idle arms need it: the SDK channels are buffered
	// (codex EventBufferSize defaults to 100), so a terminal condition can win
	// the select while a wave of already-emitted events is still queued —
	// including the TurnComplete that would have made this a success, and the
	// text a partial result is supposed to preserve. Returns a terminal result
	// when one is found in the queue.
	drainQueued := func() (*bridgeResult, bool, error) {
		for {
			select {
			case ev, ok := <-events:
				res, done, inScope, err := applyEvent(ev, ok)
				if done {
					return res, true, err
				}
				if inScope {
					lastEvent = time.Now()
				}
			default:
				return nil, false, nil
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			// Drain before reporting the cancellation: a review that finished
			// in the same instant the context died should be delivered, not
			// discarded, and its streamed text belongs to the partial result
			// either way.
			if res, done, err := drainQueued(); done {
				return res, err
			}
			return failed(ctx.Err())
		case <-ticker.C:
			// Before declaring the review stalled, drain every event already
			// queued: select can pick the ticker over a ready events case, so a
			// pending event (the wave's continuation, or just proof of life)
			// must be processed first. Only IN-SCOPE events reset lastEvent, so
			// the idle decision below depends solely on lastEvent staleness —
			// NOT on whether the drain saw anything. Draining out-of-scope
			// multiplex noise must never suppress the idle trip for a stalled
			// in-scope thread on a shared channel. "In-scope" here means the
			// scopeID check only — an unrenderable event still proves liveness.
			if res, done, err := drainQueued(); done {
				return res, err
			}
			if idleTimeout > 0 && time.Since(lastEvent) >= idleTimeout {
				return failed(fmt.Errorf("review idle: no events for %s (stalled backend)", idleTimeout))
			}
			// Emit a heartbeat line at most every heartbeatInterval even if the
			// ticker fires more often for idle-check precision.
			if time.Since(lastHeartbeat) >= heartbeatInterval {
				fmt.Fprintln(heartbeatOut, formatHeartbeat(time.Since(start), window, toolsInFlight))
				window = heartbeatWindow{}
				lastHeartbeat = time.Now()
			}
		case ev, ok := <-events:
			res, done, inScope, err := applyEvent(ev, ok)
			if done {
				return res, err
			}
			if inScope {
				// A real in-scope event proves the stream is alive — reset idle.
				lastEvent = time.Now()
			}
		}
	}
}

// rendererEventHandler adapts EventHandler to a render.Renderer and also
// emits a structured slog record for each boundary event (session info, tool
// start/end, turn complete, error). slog writes to both the log file and
// stderr (at ERROR level) via the tee handler installed by SetupRunLog.
type rendererEventHandler struct {
	r        *render.Renderer
	reviewer *Reviewer // optional; captures lastSessionID when set
}

func (r *Reviewer) newEventHandler() *rendererEventHandler {
	return &rendererEventHandler{r: r.renderer, reviewer: r}
}

func (h *rendererEventHandler) OnSessionInfo(sessionID, model string) {
	h.r.SessionInfo(sessionID, model)
	if h.reviewer != nil {
		h.reviewer.lastSessionID = sessionID
		if model != "" {
			h.reviewer.effectiveModel = model
		}
	}
	slog.Info("reviewer session started", "session_id", sessionID, "model", model)
}

func (h *rendererEventHandler) OnText(delta string) {
	h.r.Text(delta)
}

func (h *rendererEventHandler) OnReasoning(delta string) {
	h.r.Reasoning(delta)
}

func (h *rendererEventHandler) OnToolStart(name, callID string, input map[string]interface{}) {
	h.r.CommandStart(callID, name)
	slog.Debug("tool call start",
		"tool", name,
		"call_id", callID,
		"input_summary", summarizeToolInput(input))
}

func (h *rendererEventHandler) OnToolComplete(name string, callID string, _ map[string]interface{}, result interface{}, isError bool) {
	exitCode := 0
	if isError {
		exitCode = 1
	}
	h.r.CommandEnd(callID, exitCode, 0)
	resultLen := 0
	if s, ok := result.(string); ok {
		resultLen = len(s)
	}
	slog.Debug("tool call end",
		"tool", name,
		"call_id", callID,
		"is_error", isError,
		"result_len", resultLen)
}

func (h *rendererEventHandler) OnTurnComplete(success bool, durationMs int64) {
	// Renderer update is handled by reviewer.go after RunPrompt returns.
	slog.Info("reviewer turn complete",
		"success", success,
		"duration_ms", durationMs)
}

func (h *rendererEventHandler) OnError(err error, context string) {
	h.r.Error(err, context)
	attrs := []any{
		"context", context,
		"error", err.Error(),
	}
	// Preserve the frame that failed to parse. Without it the log records only
	// the Go decode error, which names a struct field but not what the backend
	// actually sent — the gap that forced the 102-failure cursor investigation
	// to infer the wire shape from a type error instead of reading it.
	if line, n, ok := protocolErrorLine(err); ok {
		attrs = append(attrs, "line", line, "line_len", n)
	}
	slog.Error("reviewer error", attrs...)
}

// protocolLiner is implemented by each backend's ProtocolError. It is declared
// here, at the consumer, because the backends are independent packages with no
// shared error type — a structural interface avoids one type assertion per
// backend and picks up any future backend for free.
type protocolLiner interface {
	ProtocolLine() string
}

// protocolErrorLine extracts the raw offending frame from err or anything it
// wraps, returning a bounded, redacted rendering and the untruncated length.
// The bound-and-redact rule itself lives in framelog, which is also what the
// SDK-side debug logs use — one rule, one implementation.
func protocolErrorLine(err error) (string, int, bool) {
	var pl protocolLiner
	if !errors.As(err, &pl) {
		return "", 0, false
	}
	line := pl.ProtocolLine()
	if line == "" {
		return "", 0, false
	}
	rendered, n := framelog.Render(line)
	return rendered, n, true
}

// sensitiveToolInputKeys names keys whose values may contain shell commands,
// file paths, edit payloads, or other content that should not be written to
// the per-run log verbatim. For these keys summarizeToolInput records only the
// value length, not the value itself.
var sensitiveToolInputKeys = map[string]bool{
	"command":          true,
	"content":          true,
	"cwd":              true,
	"file_text":        true,
	"globPattern":      true,
	"new_string":       true,
	"old_string":       true,
	"path":             true,
	"file_path":        true,
	"pattern":          true,
	"query":            true,
	"simpleCommands":   true,
	"parsingResult":    true,
	"args":             true,
	"url":              true,
	"workingDirectory": true,
}

// summarizeToolInput collapses a tool input map to a short preview for
// logging. Non-sensitive primitive values are truncated and included; values
// under sensitive keys (commands, paths, edit payloads — see
// sensitiveToolInputKeys) are replaced with a length marker so the per-run
// log never stores shell commands or file contents verbatim.
func summarizeToolInput(input map[string]interface{}) string {
	if len(input) == 0 {
		return ""
	}
	var b strings.Builder
	const maxLen = 200
	for k, v := range input {
		if b.Len() >= maxLen {
			b.WriteString("...")
			break
		}
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(k)
		b.WriteString("=")
		if sensitiveToolInputKeys[k] {
			fmt.Fprintf(&b, "<redacted:%d>", redactedLen(v))
			continue
		}
		s := fmt.Sprintf("%v", v)
		if len(s) > 80 {
			s = s[:77] + "..."
		}
		b.WriteString(s)
	}
	return b.String()
}

// redactedLen reports a byte length for a redacted tool-input value so the
// log retains "how big was it" without the content itself.
func redactedLen(v interface{}) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case nil:
		return 0
	default:
		return len(fmt.Sprintf("%v", x))
	}
}
