package reviewer

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
)

// geminiBackend wraps the Gemini ACP client as a Backend.
// Each RunPrompt call is a one-shot execution (no persistent session).
type geminiBackend struct {
	config Config
}

func newGeminiBackend(config Config) *geminiBackend {
	return &geminiBackend{config: config}
}

// Start is a no-op for Gemini (one-shot per prompt via ACP).
func (b *geminiBackend) Start(_ context.Context) error {
	return nil
}

// Stop is a no-op for Gemini (one-shot per prompt via ACP).
func (b *geminiBackend) Stop() error {
	return nil
}

func (b *geminiBackend) RunPrompt(ctx context.Context, prompt string, handler EventHandler) (*ReviewResult, error) {
	// Build ACP client options. The default binary is "gemini" with
	// "--experimental-acp"; we override args to also set --model if specified.
	binaryArgs := []string{"--experimental-acp"}
	if b.config.Model != "" {
		binaryArgs = append(binaryArgs, "--model", b.config.Model)
	}

	opts := []acp.ClientOption{
		acp.WithClientName("gemini-review"),
		acp.WithClientVersion("1.0.0"),
		acp.WithBinaryArgs(binaryArgs...),
		acp.WithStderrHandler(func(data []byte) {
			s := string(data)
			if s != "" && s[len(s)-1] != '\n' {
				s += "\n"
			}
			fmt.Fprintf(os.Stderr, "[gemini stderr] %s", s)
		}),
	}

	client := acp.NewClient(opts...)
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("gemini: failed to start ACP client: %w", err)
	}
	defer client.Stop()

	var sessionOpts []acp.SessionOption
	if b.config.WorkDir != "" {
		sessionOpts = append(sessionOpts, acp.WithSessionCWD(b.config.WorkDir))
	}

	session, err := client.NewSession(ctx, sessionOpts...)
	if err != nil {
		return nil, fmt.Errorf("gemini: failed to create session: %w", err)
	}

	// Extract agent info from ClientReadyEvent for OnSessionInfo.
	// The session ID comes from SessionCreatedEvent; we capture it from
	// the events channel before prompting, since Prompt blocks until done.
	sessionID := session.ID()

	// Notify the handler with session info (model may be empty when Gemini picks its own default).
	if handler != nil {
		handler.OnSessionInfo(sessionID, b.config.Model)
	}

	// Use a derived context so the adapter goroutine is unblocked on early return.
	adapterCtx, adapterCancel := context.WithCancel(ctx)
	defer adapterCancel()

	// Start the event adapter goroutine before Prompt (which blocks until turn complete).
	adapter := &geminiEventAdapter{handler: handler, events: client.Events()}
	bridged, bridgeErr := make(chan *bridgeResult, 1), make(chan error, 1)
	go func() {
		r, err := bridgeStreamEvents(adapterCtx, adapter.filtered(adapterCtx), handler, "")
		if err != nil {
			bridgeErr <- err
		} else {
			bridged <- r
		}
	}()

	result, promptErr := session.Prompt(ctx, prompt)
	if promptErr != nil {
		return nil, fmt.Errorf("gemini: prompt failed: %w", promptErr)
	}
	_ = result // bridgeStreamEvents handles TurnComplete

	// Wait for the bridge goroutine to finish processing events.
	select {
	case r := <-bridged:
		return &ReviewResult{
			ResponseText: r.responseText,
			Success:      r.success,
			DurationMs:   r.durationMs,
		}, nil
	case err := <-bridgeErr:
		return nil, fmt.Errorf("gemini: %w", err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// geminiEventAdapter filters ACP events before they reach bridgeStreamEvents.
// It handles ClientReadyEvent and SessionCreatedEvent out-of-band and
// normalizes Gemini tool names for display.
type geminiEventAdapter struct {
	handler EventHandler
	events  <-chan acp.Event
}

// filtered returns a channel that re-emits ACP events as acp.Event,
// handling infrastructure events separately and normalizing tool names.
func (a *geminiEventAdapter) filtered(ctx context.Context) <-chan acp.Event {
	out := make(chan acp.Event)
	go func() {
		defer close(out)
		for ev := range a.events {
			switch e := ev.(type) {
			case acp.ClientReadyEvent, acp.SessionCreatedEvent:
				// Infrastructure events: not part of agentstream; drop silently.
				_ = e
			case acp.ToolCallStartEvent:
				// Normalize Gemini tool names before bridge sees them.
				e.ToolName = formatGeminiToolDisplay(e.ToolName, e.Input)
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			default:
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

// geminiToolNames maps Gemini CLI tool names to short display names.
var geminiToolNames = map[string]string{
	"read_file":   "read",
	"write_file":  "write",
	"run_shell":   "shell",
	"list_dir":    "ls",
	"glob":        "glob",
	"grep":        "grep",
	"edit":        "edit",
	"search":      "search",
	"web_fetch":   "fetch",
	"web_search":  "search",
	"view_file":   "read",
	"create_file": "write",
	"delete_file": "delete",
	"rename_file": "rename",
	"list_files":  "ls",
	"bash":        "shell",
	"python":      "python",
}

// geminiToolArgKeys maps Gemini tool names to the most informative input key.
var geminiToolArgKeys = map[string]string{
	"read_file":   "path",
	"write_file":  "path",
	"view_file":   "path",
	"create_file": "path",
	"delete_file": "path",
	"rename_file": "old_path",
	"run_shell":   "command",
	"bash":        "command",
	"python":      "code",
	"glob":        "pattern",
	"grep":        "pattern",
	"search":      "query",
	"web_fetch":   "url",
	"web_search":  "query",
	"list_dir":    "path",
	"list_files":  "path",
	"edit":        "path",
}

// formatGeminiToolDisplay formats a Gemini tool call into a human-readable string.
// e.g. "read_file" + {path: "/foo/bar/baz.go"} → "read .../bar/baz.go"
func formatGeminiToolDisplay(name string, input map[string]interface{}) string {
	displayName, ok := geminiToolNames[name]
	if !ok {
		// Convert snake_case to camelCase-ish short form for unknown tools.
		displayName = geminiShortName(name)
	}

	argKey := geminiToolArgKeys[name]
	if argKey == "" || input == nil {
		return displayName
	}

	argVal, ok := input[argKey]
	if !ok {
		return displayName
	}

	argStr, ok := argVal.(string)
	if !ok || argStr == "" {
		return displayName
	}

	switch argKey {
	case "path", "old_path":
		argStr = shortPath(argStr)
	case "command", "code":
		if len(argStr) > 50 {
			argStr = argStr[:47] + "..."
		}
	case "url", "query":
		if len(argStr) > 60 {
			argStr = argStr[:57] + "..."
		}
	}

	if argKey == "command" || argKey == "code" {
		return displayName + ": " + argStr
	}
	return displayName + " " + argStr
}

// geminiShortName converts a snake_case tool name to a compact display name.
func geminiShortName(name string) string {
	// Strip trailing _file, _text, _dir suffixes for brevity
	for _, suffix := range []string{"_file", "_text", "_dir"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}
