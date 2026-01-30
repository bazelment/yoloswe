// Package sessionplayer provides playback of recorded Claude and Codex sessions.
package sessionplayer

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	codexrender "github.com/bazelment/yoloswe/agent-cli-wrapper/codex/render"
)

// Player plays back recorded session messages (Claude or Codex format).
type Player struct {
	claudeRenderer *render.Renderer
	codexRenderer  *codexrender.Renderer
	codexPlayer    *CodexPlayer
	out            io.Writer
	verbose        bool
	noColor        bool
}

// NewPlayer creates a new session player.
func NewPlayer(out io.Writer, verbose bool) *Player {
	noColor := !isTerminal(out)
	codexR := codexrender.NewRenderer(out, verbose, noColor)
	return &Player{
		claudeRenderer: render.NewRenderer(out, verbose),
		codexRenderer:  codexR,
		codexPlayer:    NewCodexPlayer(codexR, verbose),
		out:            out,
		verbose:        verbose,
		noColor:        noColor,
	}
}

// NewPlayerWithOptions creates a new session player with explicit options.
func NewPlayerWithOptions(out io.Writer, verbose, noColor bool) *Player {
	codexR := codexrender.NewRenderer(out, verbose, noColor)
	return &Player{
		claudeRenderer: render.NewRendererWithOptions(out, verbose, noColor),
		codexRenderer:  codexR,
		codexPlayer:    NewCodexPlayer(codexR, verbose),
		out:            out,
		verbose:        verbose,
		noColor:        noColor,
	}
}

// isTerminal checks if the writer is a terminal.
func isTerminal(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		stat, err := f.Stat()
		if err != nil {
			return false
		}
		return (stat.Mode() & os.ModeCharDevice) != 0
	}
	return false
}

// color returns the color code if colors are enabled, empty string otherwise.
func (p *Player) color(c string) string {
	if p.noColor {
		return ""
	}
	return c
}

// Play auto-detects format and plays back the session.
// Accepts either a directory (Claude format) or a file (Codex format).
func (p *Player) Play(path string) error {
	format, err := DetectFormat(path)
	if err != nil {
		return fmt.Errorf("failed to detect format: %w", err)
	}

	switch format {
	case FormatClaude:
		return p.playClaude(path)
	case FormatCodex:
		return p.playCodex(path)
	default:
		return fmt.Errorf("unknown format: %s", format)
	}
}

// playClaude plays back a Claude session recording directory.
func (p *Player) playClaude(dirPath string) error {
	// Load metadata (optional - continue without it if missing)
	recording, metaErr := claude.LoadRecording(dirPath)
	if metaErr == nil {
		p.printHeader(recording)
	}

	// Load and play messages
	messages, err := claude.LoadMessages(dirPath)
	if err != nil {
		return fmt.Errorf("failed to load messages: %w", err)
	}

	for _, msg := range messages {
		p.renderMessage(msg)
	}

	// Print session footer only if we have metadata
	if metaErr == nil {
		p.printFooter(recording)
	}

	return nil
}

// playCodex plays back a Codex session log file.
func (p *Player) playCodex(filePath string) error {
	return p.codexPlayer.PlayFile(filePath)
}

// printHeader prints session metadata header.
func (p *Player) printHeader(rec *claude.SessionRecording) {
	fmt.Fprintf(p.out, "%s%s%s\n", p.color(render.ColorCyan), "═══════════════════════════════════════════════════════════", p.color(render.ColorReset))
	fmt.Fprintf(p.out, "%sSession Recording%s\n", p.color(render.ColorCyan), p.color(render.ColorReset))
	fmt.Fprintf(p.out, "%s%s%s\n", p.color(render.ColorCyan), "═══════════════════════════════════════════════════════════", p.color(render.ColorReset))
	fmt.Fprintf(p.out, "  Session ID: %s\n", rec.Metadata.SessionID)
	fmt.Fprintf(p.out, "  Model:      %s\n", rec.Metadata.Model)
	fmt.Fprintf(p.out, "  Work Dir:   %s\n", rec.Metadata.WorkDir)
	fmt.Fprintf(p.out, "  Mode:       %s\n", rec.Metadata.PermissionMode)
	fmt.Fprintf(p.out, "  Started:    %s\n", rec.Metadata.StartTime)
	fmt.Fprintf(p.out, "  Turns:      %d\n", rec.Metadata.TotalTurns)
	fmt.Fprintf(p.out, "%s%s%s\n\n", p.color(render.ColorCyan), "───────────────────────────────────────────────────────────", p.color(render.ColorReset))
}

// printFooter prints session summary footer.
func (p *Player) printFooter(rec *claude.SessionRecording) {
	fmt.Fprintf(p.out, "\n%s%s%s\n", p.color(render.ColorCyan), "═══════════════════════════════════════════════════════════", p.color(render.ColorReset))
	fmt.Fprintf(p.out, "%sSession Summary%s\n", p.color(render.ColorCyan), p.color(render.ColorReset))
	fmt.Fprintf(p.out, "%s%s%s\n", p.color(render.ColorCyan), "═══════════════════════════════════════════════════════════", p.color(render.ColorReset))

	totalCost := rec.TotalCost()
	var totalDuration int64
	for _, turn := range rec.Turns {
		totalDuration += turn.DurationMs
	}

	fmt.Fprintf(p.out, "  Total Turns:    %d\n", len(rec.Turns))
	fmt.Fprintf(p.out, "  Total Duration: %.1fs\n", float64(totalDuration)/1000)
	fmt.Fprintf(p.out, "  Total Cost:     $%.4f\n", totalCost)
	fmt.Fprintf(p.out, "%s%s%s\n", p.color(render.ColorCyan), "═══════════════════════════════════════════════════════════", p.color(render.ColorReset))
}

// renderMessage renders a single recorded message.
func (p *Player) renderMessage(msg claude.RecordedMessage) {
	// Parse the raw message to determine its type
	rawMsg, ok := msg.Message.(json.RawMessage)
	if !ok {
		// Already parsed (shouldn't happen with current implementation)
		return
	}

	// First, determine the message type
	var typeOnly struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rawMsg, &typeOnly); err != nil {
		return
	}

	switch typeOnly.Type {
	case "system":
		p.renderSystemMessage(rawMsg)
	case "stream_event":
		p.renderStreamEvent(rawMsg)
	case "assistant":
		// Complete assistant message - we already rendered via stream events
		// Could optionally show a summary here
	case "user":
		p.renderUserMessage(rawMsg)
	case "result":
		p.renderResultMessage(rawMsg)
	}
}

// renderSystemMessage renders a system message.
func (p *Player) renderSystemMessage(raw json.RawMessage) {
	var msg struct {
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		Model     string `json:"model"`
		CWD       string `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}

	switch msg.Subtype {
	case "init":
		p.claudeRenderer.Status(fmt.Sprintf("Session initialized: %s (model: %s)", msg.SessionID, msg.Model))
	case "hook_started", "hook_response":
		// Skip hook messages in playback
	}
}

// renderStreamEvent renders a streaming event.
func (p *Player) renderStreamEvent(raw json.RawMessage) {
	var msg struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}

	var event struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type         string `json:"type"`
			Text         string `json:"text"`
			Thinking     string `json:"thinking"`
			PartialJSON  string `json:"partial_json"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(msg.Event, &event); err != nil {
		return
	}

	switch event.Type {
	case "content_block_start":
		if event.ContentBlock.Type == "tool_use" {
			p.claudeRenderer.ToolStart(event.ContentBlock.Name, event.ContentBlock.ID)
		}
	case "content_block_delta":
		switch event.Delta.Type {
		case "text_delta":
			p.claudeRenderer.Text(event.Delta.Text)
		case "thinking_delta":
			p.claudeRenderer.Thinking(event.Delta.Thinking)
		case "input_json_delta":
			// Tool input streaming - skip for cleaner output
			// p.claudeRenderer.ToolProgress(event.Delta.PartialJSON)
		}
	case "content_block_stop":
		// Block completed
	}
}

// renderUserMessage renders a user message (typically tool results).
func (p *Player) renderUserMessage(raw json.RawMessage) {
	var msg struct {
		Message struct {
			Role    string `json:"role"`
			Content interface{} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}

	// Check if this is a tool result
	if content, ok := msg.Message.Content.([]interface{}); ok {
		for _, block := range content {
			if bMap, ok := block.(map[string]interface{}); ok {
				if bMap["type"] == "tool_result" {
					isError, _ := bMap["is_error"].(bool)
					p.claudeRenderer.ToolResult(bMap["content"], isError)
				}
			}
		}
	}
}

// renderResultMessage renders a result/completion message.
func (p *Player) renderResultMessage(raw json.RawMessage) {
	var msg struct {
		Subtype      string  `json:"subtype"`
		IsError      bool    `json:"is_error"`
		DurationMs   int64   `json:"duration_ms"`
		NumTurns     int     `json:"num_turns"`
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}

	if msg.Subtype == "success" || msg.Subtype == "error_max_turns" {
		p.claudeRenderer.TurnSummary(msg.NumTurns, !msg.IsError, msg.DurationMs, msg.TotalCostUSD)
	}
}
