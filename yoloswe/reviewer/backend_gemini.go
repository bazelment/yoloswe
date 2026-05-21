package reviewer

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/agy"
)

// geminiBackend is a compatibility alias for agy. The agy CLI does not expose
// a model-selection flag, so BackendGemini reports agy's fixed default model.
type geminiBackend struct {
	config Config
	mu     sync.Mutex
	hasRun bool
}

func newGeminiBackend(config Config) *geminiBackend {
	return &geminiBackend{config: config}
}

func (b *geminiBackend) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (b *geminiBackend) Stop() error {
	return nil
}

func (b *geminiBackend) RunPrompt(ctx context.Context, prompt string, handler EventHandler) (*ReviewResult, error) {
	sessionOpts := []agy.SessionOption{
		agy.WithStderrHandler(stderrPrefixHandler("agy")),
	}
	if b.config.WorkDir != "" {
		sessionOpts = append(sessionOpts, agy.WithWorkDir(b.config.WorkDir))
	}
	var resumeStatus ResumeStatus
	if b.config.ResumeSessionID != "" {
		resumeStatus = ResumeStatusUnverified
		sessionOpts = append(sessionOpts, agy.WithConversation(b.config.ResumeSessionID))
	} else if b.hasRunPrompt() {
		sessionOpts = append(sessionOpts, agy.WithContinue())
	}
	if handler != nil {
		handler.OnSessionInfo("", DefaultGeminiModel)
	}

	session := agy.NewSession(prompt, sessionOpts...)
	if err := session.Start(ctx); err != nil {
		return reviewErrorResult(resumeStatus, fmt.Errorf("gemini/agy: failed to start session: %w", err))
	}
	defer session.Stop()

	var response strings.Builder
	for evt := range session.Events() {
		switch e := evt.(type) {
		case agy.TextEvent:
			response.WriteString(e.Text)
			if handler != nil {
				handler.OnText(e.Text)
			}
		case agy.TurnCompleteEvent:
			b.markRunPrompt()
			if handler != nil {
				handler.OnTurnComplete(e.Success, e.DurationMs)
			}
			result := &ReviewResult{
				ResponseText: response.String(),
				Success:      e.Success,
				DurationMs:   e.DurationMs,
				ResumeStatus: resumeStatus,
			}
			if e.Error != nil {
				result.ErrorMessage = e.Error.Error()
				if handler != nil {
					handler.OnError(e.Error, "turn_complete")
				}
				return result, fmt.Errorf("gemini/agy turn failed: %w", e.Error)
			}
			return result, nil
		case agy.ErrorEvent:
			if handler != nil {
				handler.OnError(e.Error, e.Context)
			}
			return reviewErrorResult(resumeStatus, fmt.Errorf("gemini/agy: %w", e.Error))
		}
	}
	return reviewErrorResult(resumeStatus, fmt.Errorf("gemini/agy: session ended without result"))
}

func (b *geminiBackend) hasRunPrompt() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hasRun
}

func (b *geminiBackend) markRunPrompt() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hasRun = true
}

// filterGeminiEvents re-emits ACP events, dropping infrastructure events
// (ClientReady/SessionCreated) and normalizing tool names on start/update
// events so the bridge and downstream renderer see the display name.
func filterGeminiEvents(ctx context.Context, events <-chan acp.Event) <-chan acp.Event {
	out := make(chan acp.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				switch e := ev.(type) {
				case acp.ClientReadyEvent, acp.SessionCreatedEvent:
					// agentstream.KindUnknown; bridge would drop anyway.
					continue
				case acp.ToolCallStartEvent:
					e.ToolName = formatGeminiToolDisplay(e.ToolName, e.Input)
					ev = e
				case acp.ToolCallUpdateEvent:
					e.ToolName = formatGeminiToolDisplay(e.ToolName, e.Input)
					ev = e
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// geminiToolDisplay renders Gemini's ACP tool calls for terminal output.
// Entries here reflect tools observed in live Gemini CLI runs; unknown names
// fall through to geminiFallbackName (strip _file/_text/_dir).
var geminiToolDisplay = toolDisplay{
	tools: map[string]toolInfo{
		"read_file":  {Display: "read", ArgKey: "path", ArgFormat: argFormatPath},
		"write_file": {Display: "write", ArgKey: "path", ArgFormat: argFormatPath},
		"edit":       {Display: "edit", ArgKey: "path", ArgFormat: argFormatPath},
		"list_dir":   {Display: "ls", ArgKey: "path", ArgFormat: argFormatPath},
		"run_shell":  {Display: "shell", ArgKey: "command", ArgFormat: argFormatCommand},
		"bash":       {Display: "shell", ArgKey: "command", ArgFormat: argFormatCommand},
		"glob":       {Display: "glob", ArgKey: "pattern", ArgFormat: argFormatPlain},
		"grep":       {Display: "grep", ArgKey: "pattern", ArgFormat: argFormatPlain},
		"web_fetch":  {Display: "fetch", ArgKey: "url", ArgFormat: argFormatLongIdentifier},
		"web_search": {Display: "search", ArgKey: "query", ArgFormat: argFormatQuery},
	},
	fallback: geminiFallbackName,
}

// geminiFallbackName trims common suffixes from snake_case tool names.
func geminiFallbackName(name string) string {
	for _, suffix := range []string{"_file", "_text", "_dir"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

func formatGeminiToolDisplay(name string, input map[string]interface{}) string {
	return geminiToolDisplay.format(name, input)
}
