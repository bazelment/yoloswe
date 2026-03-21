package yoloswe

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
)

// defaultRecordingDir returns the default recording directory (~/.yoloswe).
func defaultRecordingDir() string {
	if homeDir, err := os.UserHomeDir(); err == nil {
		return filepath.Join(homeDir, ".yoloswe")
	}
	return ".yoloswe"
}

// baseSession holds the shared state and implements common methods used by
// BuilderSession and CodeTalkSession. Callers must embed baseSession and set
// the session field before calling any methods.
//
// sessionLabel is used in ReadyEvent log lines (e.g. "Builder" or "CodeTalk").
// errorPrefix is used in ErrorEvent messages (e.g. "builder" or "codetalk").
type baseSession struct {
	output       io.Writer
	session      *claude.Session
	renderer     *render.Renderer
	sessionLabel string
	errorPrefix  string
	stats        SessionStats
	readyEmitted bool // suppress duplicate ReadyEvent on follow-up turns
}

// newBaseSession initialises a baseSession with a plain renderer.
func newBaseSession(output io.Writer, verbose bool, sessionLabel, errorPrefix string) baseSession {
	return baseSession{
		output:       output,
		renderer:     render.NewRenderer(output, verbose),
		sessionLabel: sessionLabel,
		errorPrefix:  errorPrefix,
	}
}

// newBaseSessionWithEvents initialises a baseSession whose renderer emits
// structured events to eventHandler.
func newBaseSessionWithEvents(output io.Writer, verbose bool, eventHandler render.EventHandler, sessionLabel, errorPrefix string) baseSession {
	return baseSession{
		output:       output,
		renderer:     render.NewRendererWithEvents(output, verbose, eventHandler),
		sessionLabel: sessionLabel,
		errorPrefix:  errorPrefix,
	}
}

// CLISessionID returns the CLI session ID from the underlying claude session.
// Available after Start() completes.
func (b *baseSession) CLISessionID() string {
	if b.session == nil {
		return ""
	}
	info := b.session.Info()
	if info == nil {
		return ""
	}
	return info.SessionID
}

// Stop gracefully shuts down the session. Safe to call before Start.
func (b *baseSession) Stop() error {
	if b.session == nil {
		return nil
	}
	return b.session.Stop()
}

// RecordingPath returns the path to the session recording directory.
// Returns empty string if session not started.
func (b *baseSession) RecordingPath() string {
	if b.session == nil {
		return ""
	}
	return b.session.RecordingPath()
}

// SessionStats tracks cumulative token usage and cost across turns.
type SessionStats struct {
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	CostUSD         float64
	TurnCount       int
}

// Stats returns the cumulative session statistics.
func (b *baseSession) Stats() SessionStats {
	return b.stats
}

// PrintUsageSummary prints cumulative token usage and cost to stderr.
func (b *baseSession) PrintUsageSummary() {
	fmt.Fprintln(os.Stderr, "\n"+strings.Repeat("═", 50))
	fmt.Fprintln(os.Stderr, "SESSION USAGE SUMMARY")
	fmt.Fprintln(os.Stderr, strings.Repeat("─", 50))
	fmt.Fprintf(os.Stderr, "  Turns:         %d\n", b.stats.TurnCount)
	fmt.Fprintf(os.Stderr, "  Input tokens:  %d\n", b.stats.InputTokens)
	fmt.Fprintf(os.Stderr, "  Output tokens: %d\n", b.stats.OutputTokens)
	if b.stats.CacheReadTokens > 0 {
		fmt.Fprintf(os.Stderr, "  Cache read:    %d\n", b.stats.CacheReadTokens)
	}
	fmt.Fprintf(os.Stderr, "  Total cost:    $%.4f\n", b.stats.CostUSD)
	fmt.Fprintln(os.Stderr, strings.Repeat("═", 50))
}

// RunTurn sends a message and drains events until TurnComplete or an error.
func (b *baseSession) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	if strings.TrimSpace(message) == "" {
		return nil, fmt.Errorf("message cannot be empty")
	}

	_, err := b.session.SendMessage(ctx, message)
	if err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-b.session.Events():
			if !ok {
				return nil, fmt.Errorf("session ended unexpectedly")
			}

			switch e := event.(type) {
			case claude.ReadyEvent:
				if !b.readyEmitted {
					b.readyEmitted = true
					b.renderer.Status(fmt.Sprintf("%s session started: %s (model: %s)", b.sessionLabel, e.Info.SessionID, e.Info.Model))
				}

			case claude.TextEvent:
				b.renderer.Text(e.Text)

			case claude.ThinkingEvent:
				b.renderer.Thinking(e.Thinking)

			case claude.ToolStartEvent:
				b.renderer.ToolStart(e.Name, e.ID)

			case claude.ToolCompleteEvent:
				b.renderer.ToolComplete(e.Name, e.Input)

			case claude.CLIToolResultEvent:
				b.renderer.ToolResult(e.Content, e.IsError)

			case claude.TurnCompleteEvent:
				b.stats.TurnCount++
				b.stats.InputTokens += e.Usage.InputTokens
				b.stats.OutputTokens += e.Usage.OutputTokens
				b.stats.CacheReadTokens += e.Usage.CacheReadTokens
				b.stats.CostUSD += e.Usage.CostUSD
				b.renderer.TurnSummary(e.TurnNumber, e.Success, e.DurationMs, e.Usage.CostUSD)
				if !e.Success {
					return &e.Usage, fmt.Errorf("turn completed with success=false")
				}
				return &e.Usage, nil

			case claude.ErrorEvent:
				b.renderer.Error(e.Error, e.Context)
				return nil, fmt.Errorf("%s error: %v (context: %s)", b.errorPrefix, e.Error, e.Context)
			}
		}
	}
}

// autoAnswerQuestions selects the first option for each question (or "yes" if
// there are no options). Used by both builderInteractiveHandler and
// codetalkInteractiveHandler to auto-answer AskUserQuestion calls.
func autoAnswerQuestions(renderer *render.Renderer, questions []claude.Question) (map[string]string, error) {
	answers := make(map[string]string)
	for _, q := range questions {
		var response string
		if len(q.Options) > 0 {
			response = q.Options[0].Label
			renderer.Status(fmt.Sprintf("Auto-answering: %s -> %s", q.Text, response))
		} else {
			response = "yes"
			renderer.Status(fmt.Sprintf("Auto-answering (no options): %s -> %s", q.Text, response))
		}
		answers[q.Text] = response
	}
	return answers, nil
}
