// Package delegator provides a cobra subcommand for testing the delegator
// agent's system prompt with mock or real tools.
package delegator

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

var (
	mode             string
	model            string
	childModel       string
	autoAdvance      bool
	behaviorFlag     string
	systemPromptFile string
	prompt           string
	workDir          string
	verbose          bool
	logDir           string
	timeout          time.Duration
	statusFD         int
	// Voice reporting flags.
	enableVoiceReports bool
	elevenLabsAPIKey   string
	ttsVoice           string
	voiceReportMode    string
	voiceSaveDir       string
)

// Cmd is the cobra command for the delegator test harness.
var Cmd = &cobra.Command{
	Use:   "delegator",
	Short: "Interactive CLI harness for testing the delegator agent",
	Long: `An interactive CLI harness for testing the delegator agent's system prompt
with mock or real tools.

Mock mode uses scripted child session behaviors for fast iteration on the
delegator's system prompt. Real mode uses the Manager with real Claude sessions.`,
	RunE: runDelegator,
}

func init() {
	Cmd.Flags().StringVar(&mode, "mode", "real", "Mode: real (Manager-based with real sessions) or mock (scripted children)")
	Cmd.Flags().StringVar(&model, "model", "sonnet", "Claude model for the delegator")
	Cmd.Flags().StringVar(&childModel, "child-model", "", "Model for child sessions (default: same as delegator)")
	Cmd.Flags().BoolVar(&autoAdvance, "auto-advance", true, "Auto-send child state notifications (mock mode only)")
	Cmd.Flags().StringVar(&behaviorFlag, "behavior", "", "State progressions, e.g. planner=running,completed;builder=running,completed (mock mode only)")
	Cmd.Flags().StringVar(&systemPromptFile, "system-prompt", "", "Override system prompt from file (mock mode only)")
	Cmd.Flags().StringVar(&prompt, "prompt", "", "Non-interactive: run a single conversation with this prompt")
	Cmd.Flags().StringVar(&workDir, "work-dir", ".", "Working directory for the Claude session")
	Cmd.Flags().BoolVar(&verbose, "verbose", false, "Show non-error tool results / child session output")
	Cmd.Flags().StringVar(&logDir, "log-dir", "", "Directory for JSONL session recording")
	Cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Timeout for non-interactive mode (real mode only)")
	Cmd.Flags().IntVar(&statusFD, "status-fd", 0, "File descriptor to write status events (idle/done) for programmatic control")
	Cmd.Flags().BoolVar(&enableVoiceReports, "enable-voice-reports", false, "Enable voice reporting on session completion (requires ELEVENLABS_API_KEY)")
	Cmd.Flags().StringVar(&elevenLabsAPIKey, "elevenlabs-api-key", "", "ElevenLabs API key (or set ELEVENLABS_API_KEY env var)")
	Cmd.Flags().StringVar(&ttsVoice, "tts-voice", "", "ElevenLabs voice ID for TTS synthesis")
	Cmd.Flags().StringVar(&voiceReportMode, "voice-report-mode", "auto", "Voice report playback mode: auto, direct, file, redirect (local is deprecated alias for direct)")
	Cmd.Flags().StringVar(&voiceSaveDir, "voice-save-dir", "", "Directory for file-mode voice reports (default: ~/.bramble/voice-reports)")
}

func runDelegator(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	renderer := render.NewRenderer(os.Stdout, verbose)

	switch mode {
	case "mock":
		if err := runMock(ctx, renderer, model, workDir, prompt, logDir, autoAdvance, behaviorFlag, systemPromptFile); err != nil {
			return err
		}
	case "real":
		info, err := os.Stat(workDir)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("--work-dir must be a valid directory in real mode")
		}
		if err := runReal(ctx, model, childModel, workDir, prompt, logDir, timeout, statusFD); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown mode %q: use mock or real", mode)
	}
	return nil
}

func runMock(ctx context.Context, renderer *render.Renderer, model, workDir, prompt, logDir string, autoAdvance bool, behaviorFlag, systemPromptFile string) error {
	behaviors := parseBehaviors(behaviorFlag)

	systemPrompt := session.DelegatorSystemPrompt
	if systemPromptFile != "" {
		data, err := os.ReadFile(systemPromptFile)
		if err != nil {
			return fmt.Errorf("error reading system prompt file: %w", err)
		}
		systemPrompt = string(data)
	}

	mock := session.NewMockDelegatorToolHandler(behaviors)

	opts := session.DelegatorBaseSessionOpts(model, mock.Registry(), systemPrompt)
	opts = append(opts, claude.WithWorkDir(workDir))
	if logDir != "" {
		opts = append(opts, claude.WithRecording(logDir))
	}

	s := claude.NewSession(opts...)

	if err := s.Start(ctx); err != nil {
		return fmt.Errorf("failed to start session: %w", err)
	}
	defer s.Stop()

	fmt.Printf("Delegator Test Harness (mock) | Model: %s | Auto-advance: %v\n\n", model, autoAdvance)

	if prompt != "" {
		runMockConversation(ctx, s, mock, renderer, prompt, autoAdvance)
		if logDir != "" {
			fmt.Printf("\nSession JSONL logs saved to: %s\n", logDir)
		}
		return nil
	}

	// Interactive mode (mock).
	// Note: mock mode uses a simple bufio.Scanner rather than InputReader/Spinner.
	// This is intentional — mock mode is a fast-iteration harness for the system
	// prompt, not a polished UX, and adding readline/spinner there would complicate
	// the mock execution path without meaningful benefit.
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print(">>> ")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print(">>> ")
			continue
		}
		if line == "quit" || line == "exit" {
			break
		}

		runMockConversation(ctx, s, mock, renderer, line, autoAdvance)
		fmt.Print("\n>>> ")
	}
	if logDir != "" {
		fmt.Printf("\nSession JSONL logs saved to: %s\n", logDir)
	}
	return nil
}

func runMockConversation(ctx context.Context, s *claude.Session, mock *session.MockDelegatorToolHandler, r *render.Renderer, msg string, autoAdvance bool) {
	if _, err := s.SendMessage(ctx, msg); err != nil {
		fmt.Fprintf(os.Stderr, "SendMessage error: %v\n", err)
		return
	}

	for {
		result, events, err := s.CollectResponse(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error collecting response: %v\n", err)
			return
		}

		hadToolCalls := false
		for _, evt := range events {
			switch e := evt.(type) {
			case claude.TextEvent:
				r.Text(e.Text)
			case claude.ThinkingEvent:
				r.Thinking(e.Thinking)
			case claude.ToolStartEvent:
				hadToolCalls = true
				r.ToolStart(e.Name, e.ID)
			case claude.ToolCompleteEvent:
				r.ToolComplete(e.Name, e.Input)
			case claude.CLIToolResultEvent:
				r.ToolResult(e.Content, e.IsError)
			}
		}

		if result != nil {
			r.TurnSummary(result.TurnNumber, result.Success, result.DurationMs, result.Usage.CostUSD)
		}

		if !hadToolCalls {
			break
		}

		// Auto-advance: step sessions forward until a notifiable state is reached.
		// Each AdvanceAll() call moves sessions one step, simulating time passing.
		if autoAdvance {
			notification := mock.AdvanceUntilNotification()
			if notification == "" {
				break
			}
			r.Status(notification)
			if _, err := s.SendMessage(ctx, notification); err != nil {
				fmt.Fprintf(os.Stderr, "Auto-notify error: %v\n", err)
				return
			}
		} else {
			break
		}
	}
}

func runReal(ctx context.Context, model, childModel, workDir, initialPrompt, logDir string, timeout time.Duration, statusFD int) error {
	// Probe installed providers so the delegator knows which models are available.
	pa := agent.NewProviderAvailability()
	registry := agent.NewModelRegistry(pa, nil)

	cfg := session.ManagerConfig{
		SessionMode:   session.SessionModeTUI,
		ModelRegistry: registry,
		ChildModel:    childModel,
	}
	if logDir != "" {
		cfg.RecordingDir = logDir
		cfg.ProtocolLogDir = logDir
	}
	var voiceReporter *app.VoiceReporter
	if enableVoiceReports {
		cfg.VoiceReporting = &session.VoiceReportingConfig{
			Enabled: true,
			Mode:    voiceReportMode,
			Voice:   ttsVoice,
			SaveDir: voiceSaveDir,
		}
		voiceReporter = app.BuildVoiceReporter(elevenLabsAPIKey, ttsVoice, voiceReportMode, voiceSaveDir)
		if voiceReporter == nil {
			cfg.VoiceReporting = nil
		}
	}

	// voiceWG tracks in-flight voice report goroutines so runReal can wait for
	// them to finish before returning (preventing premature process exit).
	var voiceWG sync.WaitGroup

	m := session.NewManagerWithConfig(cfg)
	defer m.Close()

	prompt := initialPrompt
	interactive := false
	if prompt == "" {
		if render.IsTerminal(os.Stdin) {
			// Real terminal — interactive mode.
			interactive = true
		} else {
			// Piped stdin — read all input as the prompt.
			scanner := bufio.NewScanner(os.Stdin)
			if scanner.Scan() {
				prompt = strings.TrimSpace(scanner.Text())
			}
			if prompt == "" {
				return nil
			}
		}
	}

	// statusFile is an optional pipe for programmatic status events.
	// When set via --status-fd, the delegator writes "idle\n" when ready
	// for input and "done\n" on exit, enabling reliable IPC without
	// parsing stderr prompts.
	var statusFile *os.File
	if statusFD > 0 {
		statusFile = os.NewFile(uintptr(statusFD), "status-fd")
	}
	writeStatus := func(msg string) {
		if statusFile != nil {
			fmt.Fprintln(statusFile, msg)
		}
	}

	effectiveChild := childModel
	if effectiveChild == "" {
		effectiveChild = model
	}
	fmt.Fprintf(os.Stderr, "Delegator Test Harness (real) | Model: %s | Child: %s | Work dir: %s\n\n", model, effectiveChild, workDir)

	// Create a single InputReader for the entire interactive session.
	// This avoids creating multiple readline instances on the same terminal.
	var inputReader *InputReader
	if interactive {
		inputReader = NewInputReader(">>> ")
		defer inputReader.Close()

		writeStatus("idle")
		line, ok := <-inputReader.Lines()
		if !ok {
			return nil
		}
		prompt = strings.TrimSpace(line)
		if prompt == "" || prompt == "quit" || prompt == "exit" {
			return nil
		}
	}

	delegatorID, err := m.StartSession(session.SessionTypeDelegator, workDir, prompt, model)
	if err != nil {
		return fmt.Errorf("failed to start delegator session: %w", err)
	}

	// doneCh signals when the delegator reaches a terminal state.
	doneCh := make(chan struct{})
	// idleCh signals when the delegator goes idle (for interactive follow-ups).
	idleCh := make(chan struct{}, 1)

	// Turn-based rendering: instead of printing streaming event fragments,
	// we wait for turn-end / state-change events and read the full accumulated
	// output from the Manager. This gives complete text and clean formatting.
	//
	// linesRendered tracks how many OutputLines we've already printed for the
	// delegator so we can render only new lines on each turn boundary.
	linesRendered := 0

	// renderNewOutput reads the delegator's accumulated output from the Manager
	// and prints any lines added since the last render.
	// Only user-facing text goes to stdout; everything else goes to stderr
	// (some items gated behind --verbose).
	renderNewOutput := func() {
		lines := m.GetSessionOutput(delegatorID)
		for i := linesRendered; i < len(lines); i++ {
			line := lines[i]
			switch line.Type {
			case session.OutputTypeText:
				if line.IsUserPrompt {
					continue
				}
				fmt.Println(strings.TrimRight(line.Content, "\n"))
			case session.OutputTypeThinking:
				if verbose {
					fmt.Fprintf(os.Stderr, "%s%s%s%s\n",
						render.ColorDim, render.ColorItalic,
						strings.TrimRight(line.Content, "\n"),
						render.ColorReset)
				}
			case session.OutputTypeToolStart:
				if verbose {
					name := shortToolName(line.ToolName)
					if !isDelegatorTool(name) {
						continue
					}
					input := formatToolSummary(name, line.ToolInput)
					if input != "" {
						fmt.Fprintf(os.Stderr, "  %s%s%s %s\n", render.ColorCyan, name, render.ColorReset, input)
					} else {
						fmt.Fprintf(os.Stderr, "  %s%s%s\n", render.ColorCyan, name, render.ColorReset)
					}
				}
			case session.OutputTypeTurnEnd:
				if verbose {
					status, color := "✓", render.ColorGreen
					if line.IsError {
						status, color = "✗", render.ColorRed
					}
					fmt.Fprintf(os.Stderr, "%s%s Turn %d (%.1fs, $%.4f)%s\n\n",
						color, status, line.TurnNumber,
						float64(line.DurationMs)/1000, line.CostUSD, render.ColorReset)
				}
			case session.OutputTypeError:
				fmt.Fprintf(os.Stderr, "%sError: %s%s\n",
					render.ColorRed, line.Content, render.ColorReset)
			}
		}
		linesRendered = len(lines)
	}

	// renderChildStatus prints a summary line for a child session state change.
	// Always goes to stderr — lifecycle events are not user-facing output.
	renderChildStatus := func(childID session.SessionID, newStatus session.SessionStatus) {
		info, ok := m.GetSessionInfo(childID)
		if !ok {
			return
		}
		label := fmt.Sprintf("%s (%s)", childID, info.Type)
		switch newStatus {
		case session.StatusRunning:
			fmt.Fprintf(os.Stderr, "  %s▶ %s%s\n", render.ColorCyan, label, render.ColorReset)
		case session.StatusCompleted:
			summary := ""
			if len(info.Progress.RecentOutput) > 0 {
				summary = " — " + info.Progress.RecentOutput[len(info.Progress.RecentOutput)-1]
			}
			fmt.Fprintf(os.Stderr, "  %s✓ %s completed (%s)%s%s\n",
				render.ColorGreen, label,
				formatProgressDetail(info.Progress),
				summary, render.ColorReset)
		case session.StatusIdle:
			summary := ""
			if len(info.Progress.RecentOutput) > 0 {
				summary = " — " + info.Progress.RecentOutput[len(info.Progress.RecentOutput)-1]
			}
			detail := ""
			if info.Progress.TurnCount > 0 || info.Progress.TotalCostUSD > 0 || info.Progress.InputTokens > 0 {
				detail = fmt.Sprintf(" (%s)", formatProgressDetail(info.Progress))
			}
			fmt.Fprintf(os.Stderr, "  %s✓ %s done%s%s%s\n",
				render.ColorGreen, label,
				detail, summary, render.ColorReset)
		case session.StatusFailed:
			errMsg := info.ErrorMsg
			if errMsg == "" {
				errMsg = "unknown error"
			}
			fmt.Fprintf(os.Stderr, "  %s✗ %s failed: %s%s\n",
				render.ColorRed, label, errMsg, render.ColorReset)
		}
	}

	// stopCh is closed by the main goroutine when it exits the event wait,
	// signaling the event loop goroutine to stop writing to stdout.
	stopCh := make(chan struct{})
	var eventLoopDone sync.WaitGroup
	eventLoopDone.Add(1)

	// Event loop goroutine — drains Manager events and signals turn/state
	// boundaries. Rendering happens in response to these signals rather
	// than per-event, so we always see fully accumulated text.
	go func() {
		defer eventLoopDone.Done()
		progressTicker := time.NewTicker(30 * time.Second)
		defer progressTicker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-progressTicker.C:
				if !verbose {
					continue
				}
				// Show progress for active child sessions (verbose only, stderr).
				sessions := m.GetAllSessions()
				for i := range sessions {
					if sessions[i].ID == delegatorID {
						continue
					}
					if sessions[i].Status != session.StatusRunning {
						continue
					}
					elapsed := ""
					if sessions[i].StartedAt != nil {
						elapsed = fmt.Sprintf(" %s", time.Since(*sessions[i].StartedAt).Round(time.Second))
					}
					detail := ""
					if sessions[i].Progress.TurnCount > 0 {
						detail = ", " + formatProgressDetail(sessions[i].Progress)
					}
					fmt.Fprintf(os.Stderr, "  %s⏳ %s (%s)%s%s%s\n",
						render.ColorDim, sessions[i].ID, sessions[i].Type, elapsed, detail, render.ColorReset)
				}
			case evt, ok := <-m.Events():
				if !ok {
					return
				}
				switch e := evt.(type) {
				case session.SessionOutputEvent:
					// We only care about delegator turn-end events to trigger rendering.
					if e.SessionID != delegatorID || e.Line.Type != session.OutputTypeTurnEnd {
						continue
					}
					renderNewOutput()

				case session.SessionStateChangeEvent:
					if e.SessionID != delegatorID {
						renderChildStatus(e.SessionID, e.NewStatus)
					}

					// Trigger voice report for terminal session states.
					if voiceReporter != nil {
						switch e.NewStatus {
						case session.StatusCompleted, session.StatusFailed, session.StatusStopped:
							if info, ok := m.GetSessionInfo(e.SessionID); ok {
								voiceWG.Add(1)
								go func(i session.SessionInfo) {
									defer voiceWG.Done()
									ctx, cancel := context.WithTimeout(context.Background(), app.SynthesisTimeout)
									defer cancel()
									voiceReporter.Report(ctx, i)
								}(info)
							}
						}
					}

					if e.SessionID == delegatorID {
						switch e.NewStatus {
						case session.StatusCompleted, session.StatusFailed, session.StatusStopped:
							// Render any final output before signaling done.
							renderNewOutput()
							close(doneCh)
							return
						case session.StatusIdle:
							if !interactive && !hasActiveChildren(m, delegatorID) {
								renderNewOutput()
								// Give child completion events time to arrive
								// before we close the event loop.
								time.Sleep(300 * time.Millisecond)
								// Drain any remaining child events.
							drainLoop:
								for {
									select {
									case evt2, ok2 := <-m.Events():
										if !ok2 {
											break drainLoop
										}
										if e2, ok3 := evt2.(session.SessionStateChangeEvent); ok3 && e2.SessionID != delegatorID {
											renderChildStatus(e2.SessionID, e2.NewStatus)
										}
									default:
										break drainLoop
									}
								}
								close(doneCh)
								return
							}
							// Drain any stale signal so the latest idle
							// is never lost. Without this, rapid
							// Idle→Running→Idle cycles (from child
							// notification processing) drop the final
							// idle when the buffer-1 channel is full.
							select {
							case <-idleCh:
							default:
							}
							idleCh <- struct{}{}
						}
					}
				}
			}
		}
	}()

	if interactive {
		// Interactive loop: wait for delegator to go idle, then prompt for input.
		spinner := NewSpinner(os.Stderr)
		// Ensure spinner is always stopped on exit, even if onIdle is never called
		// (e.g. when doneCh closes or ctx is cancelled).
		defer spinner.Stop()
		// Start spinner immediately — the initial prompt is already being processed.
		spinner.Start("Thinking...")

		runInteractiveLoop(interactiveLoopConfig{
			hasActiveChildren: func() bool { return hasActiveChildren(m, delegatorID) },
			sendFollowUp:      func(msg string) error { return m.SendFollowUp(delegatorID, msg) },
			idleCh:            idleCh,
			doneCh:            doneCh,
			stdinCh:           inputReader.Lines(),
			writeStatus:       writeStatus,
			ctx:               ctx,
			onInputSent:       func() { spinner.Start("Thinking...") },
			onIdle:            func() { spinner.Stop() },
			setPrompt: func(prompt string) {
				inputReader.SetPrompt(prompt)
				inputReader.RefreshPrompt()
			},
		})
	} else {
		// Non-interactive: wait for completion or timeout.
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-doneCh:
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\nInterrupted\n")
		case <-timer.C:
			fmt.Fprintf(os.Stderr, "\nTimeout after %v\n", timeout)
		}
	}

	writeStatus("done")
	if statusFile != nil {
		statusFile.Close()
	}
	// Signal event loop goroutine to stop and wait for it to finish
	// before reading final state, preventing concurrent stdout writes.
	close(stopCh)
	eventLoopDone.Wait()

	// Print total summary across all sessions.
	lines := m.GetSessionOutput(delegatorID)
	var delegatorCost float64
	var delegatorTurns int
	for i := range lines {
		if lines[i].Type == session.OutputTypeTurnEnd {
			delegatorCost += lines[i].CostUSD
			delegatorTurns++
		}
	}
	totalCost := delegatorCost
	// Count child sessions by type and aggregate their costs and tokens.
	type childStats struct {
		count        int
		cost         float64
		inputTokens  int
		outputTokens int
	}
	childByType := make(map[session.SessionType]*childStats)
	allSessions := m.GetAllSessions()
	for i := range allSessions {
		if allSessions[i].ID != delegatorID {
			t := allSessions[i].Type
			s, ok := childByType[t]
			if !ok {
				s = &childStats{}
				childByType[t] = s
			}
			s.count++
			s.cost += allSessions[i].Progress.TotalCostUSD
			s.inputTokens += allSessions[i].Progress.InputTokens
			s.outputTokens += allSessions[i].Progress.OutputTokens
			totalCost += allSessions[i].Progress.TotalCostUSD
		}
	}
	// Build summary: "delegator: N turns | planner: M sessions | builder: K sessions"
	parts := []string{fmt.Sprintf("delegator: %d turns, $%.4f", delegatorTurns, delegatorCost)}
	for _, t := range []session.SessionType{session.SessionTypePlanner, session.SessionTypeBuilder, session.SessionTypeCodeTalk} {
		if s, ok := childByType[t]; ok {
			noun := "sessions"
			if s.count == 1 {
				noun = "session"
			}
			tokenInfo := ""
			if s.inputTokens > 0 || s.outputTokens > 0 {
				tokenInfo = fmt.Sprintf(", %din/%dout tokens", s.inputTokens, s.outputTokens)
			}
			parts = append(parts, fmt.Sprintf("%s: %d %s, $%.4f%s", t, s.count, noun, s.cost, tokenInfo))
		} else {
			parts = append(parts, fmt.Sprintf("%s: 0 sessions", t))
		}
	}
	fmt.Fprintf(os.Stderr, "Total: $%.4f (%s)\n", totalCost, strings.Join(parts, " | "))

	if logDir != "" {
		fmt.Fprintf(os.Stderr, "\nSession JSONL logs saved to: %s\n", logDir)
	}

	// Wait for any in-flight voice report goroutines to finish before exiting.
	voiceWG.Wait()

	return nil
}

// formatProgressDetail formats a progress summary showing turns and either cost
// or token counts (preferring cost when non-zero, falling back to tokens).
func formatProgressDetail(p session.SessionProgressSnapshot) string {
	if p.TotalCostUSD > 0 {
		return fmt.Sprintf("turns: %d, $%.4f", p.TurnCount, p.TotalCostUSD)
	}
	if p.InputTokens > 0 || p.OutputTokens > 0 {
		return fmt.Sprintf("turns: %d, %din/%dout tokens", p.TurnCount, p.InputTokens, p.OutputTokens)
	}
	return fmt.Sprintf("turns: %d, $%.4f", p.TurnCount, p.TotalCostUSD)
}

// formatToolSummary returns a short description of a tool invocation's input.
func formatToolSummary(name string, input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	switch name {
	case "start_session":
		typ, _ := input["type"].(string)
		p, _ := input["prompt"].(string)
		if len(p) > 80 {
			p = p[:77] + "..."
		}
		if typ != "" {
			return fmt.Sprintf("(%s) %s", typ, p)
		}
		return p
	case "get_session_progress":
		id, _ := input["session_id"].(string)
		return id
	case "stop_session":
		id, _ := input["session_id"].(string)
		return id
	case "send_followup":
		id, _ := input["session_id"].(string)
		msg, _ := input["prompt"].(string)
		if len(msg) > 60 {
			msg = msg[:57] + "..."
		}
		return fmt.Sprintf("%s: %s", id, msg)
	}
	return ""
}

// shortToolName strips the MCP prefix from delegator tool names for readability.
// e.g. "mcp__delegator-tools__start_session" → "start_session"
func shortToolName(name string) string {
	const prefix = "mcp__delegator-tools__"
	if strings.HasPrefix(name, prefix) {
		return name[len(prefix):]
	}
	return name
}

// isDelegatorTool returns true if the (short) tool name is one of the known
// delegator tools. Built-in SDK tools like ToolSearch bypass --allowed-tools
// and may appear in the event stream; this filter keeps them out of the display.
func isDelegatorTool(name string) bool {
	switch name {
	case "start_session", "stop_session", "get_session_progress", "send_followup":
		return true
	}
	return false
}

// hasActiveChildren checks if any non-delegator session is in a non-terminal state.
func hasActiveChildren(m *session.Manager, delegatorID session.SessionID) bool {
	sessions := m.GetAllSessions()
	for i := range sessions {
		if sessions[i].ID == delegatorID {
			continue
		}
		switch sessions[i].Status {
		case session.StatusCompleted, session.StatusFailed, session.StatusStopped, session.StatusIdle:
			continue
		default:
			return true
		}
	}
	return false
}

// interactiveLoopConfig holds dependencies for the interactive event loop,
// enabling unit testing without real API calls or sessions.
type interactiveLoopConfig struct {
	hasActiveChildren func() bool
	sendFollowUp      func(msg string) error
	idleCh            <-chan struct{}
	doneCh            <-chan struct{}
	stdinCh           <-chan string
	writeStatus       func(string)
	ctx               context.Context

	// Optional callbacks for spinner and prompt management.
	// Nil-safe: callers that don't set these get no-op behavior.
	onInputSent func() // called after sendFollowUp succeeds
	onIdle      func() // called when delegator goes idle
	setPrompt   func(string)
}

// runInteractiveLoop drives the interactive prompt loop. It waits for idle
// signals, emits status, and dispatches stdin input as follow-ups. Returns
// when doneCh closes, context is cancelled, or stdin sends "quit"/"exit".
//
// The loop uses a single select (not nested) so that idleCh signals from
// child-notification turn cycles are never missed. A promptReady flag
// prevents duplicate stderr prompts.
func runInteractiveLoop(cfg interactiveLoopConfig) {
	// Normalize optional callbacks to no-ops so the body never needs nil checks.
	if cfg.onInputSent == nil {
		cfg.onInputSent = func() {}
	}
	if cfg.onIdle == nil {
		cfg.onIdle = func() {}
	}
	if cfg.setPrompt == nil {
		cfg.setPrompt = func(p string) { fmt.Fprint(os.Stderr, p) }
	}

	promptReady := false
	for {
		select {
		case <-cfg.doneCh:
			return
		case <-cfg.ctx.Done():
			return
		case <-cfg.idleCh:
			// Delegator went idle — either after processing user input or
			// after a child-notification turn cycle. Emit status every time
			// so the eval script can track children-active vs all-done.
			cfg.onIdle()
			if cfg.hasActiveChildren() {
				cfg.writeStatus("idle-children-active")
			} else {
				cfg.writeStatus("idle")
			}
			if !promptReady {
				fmt.Fprintln(os.Stderr)
				inputPrompt := ">>> "
				if cfg.hasActiveChildren() {
					inputPrompt = "(children active) >>> "
				}
				cfg.setPrompt(inputPrompt)
				promptReady = true
			}
		case text, ok := <-cfg.stdinCh:
			if !ok {
				return
			}
			line := strings.TrimSpace(text)
			if line == "" {
				continue
			}
			if line == "quit" || line == "exit" {
				return
			}
			promptReady = false
			// Retry with backoff — the delegator may be momentarily
			// non-idle while processing a child notification.
			// Use ctx-aware sleeps so Ctrl-C or doneCh can interrupt.
			sent := false
			for attempt := 0; attempt < 5; attempt++ {
				if err := cfg.sendFollowUp(line); err == nil {
					sent = true
					break
				} else if attempt == 4 {
					fmt.Fprintf(os.Stderr, "SendFollowUp error after retries: %v\n", err)
				} else {
					delay := time.Duration(500*(attempt+1)) * time.Millisecond
					select {
					case <-cfg.ctx.Done():
						return
					case <-cfg.doneCh:
						return
					case <-time.After(delay):
					}
				}
			}
			if sent {
				cfg.onInputSent()
			}
		}
	}
}

// parseBehaviors parses the --behavior flag value.
// Format: "planner=running,completed;builder=running,failed"
func parseBehaviors(s string) map[string][]*session.MockSessionBehavior {
	if s == "" {
		return defaultBehaviors()
	}

	behaviors := make(map[string][]*session.MockSessionBehavior)
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		typ := strings.TrimSpace(kv[0])
		statuses := strings.Split(strings.TrimSpace(kv[1]), ",")

		states := make([]session.MockSessionState, len(statuses))
		for i, status := range statuses {
			status = strings.TrimSpace(status)
			states[i] = session.MockSessionState{
				Status:    status,
				TurnCount: i + 1,
			}
			if status == "failed" {
				states[i].ErrorMsg = "simulated failure"
			}
		}
		behaviors[typ] = append(behaviors[typ], &session.MockSessionBehavior{States: states})
	}
	return behaviors
}

func defaultBehaviors() map[string][]*session.MockSessionBehavior {
	return map[string][]*session.MockSessionBehavior{
		"planner": {
			{States: []session.MockSessionState{
				{Status: "running", TurnCount: 1, TotalCostUSD: 0.01, InputTokens: 1200, OutputTokens: 400,
					RecentOutput: []string{"Analyzing codebase structure...", "Reading existing files..."}},
				{Status: "running", TurnCount: 2, TotalCostUSD: 0.03, InputTokens: 3500, OutputTokens: 1200,
					RecentOutput: []string{"Identified key components.", "Drafting implementation plan..."}},
				{Status: "completed", TurnCount: 3, TotalCostUSD: 0.05, InputTokens: 5000, OutputTokens: 2000,
					RecentOutput: []string{
						"Plan: 1) Add auth middleware with JWT validation",
						"2) Create login/register handlers",
						"3) Update OpenAPI spec with new endpoints",
					}},
			}},
		},
		"builder": {
			// First builder: does the main implementation.
			{States: []session.MockSessionState{
				{Status: "running", TurnCount: 1, TotalCostUSD: 0.02, InputTokens: 2000, OutputTokens: 800,
					RecentOutput: []string{"Setting up auth package structure..."}},
				{Status: "running", TurnCount: 3, TotalCostUSD: 0.06, InputTokens: 8000, OutputTokens: 3500,
					RecentOutput: []string{"Created auth/jwt.go", "Created handlers/auth.go", "Writing tests..."}},
				{Status: "waiting_for_input", TurnCount: 4, TotalCostUSD: 0.08, InputTokens: 10000, OutputTokens: 4200,
					Question:     "Should I use bcrypt or argon2 for password hashing? bcrypt is simpler but argon2 is more resistant to GPU attacks.",
					RecentOutput: []string{"Auth handlers implemented.", "Need decision on password hashing algorithm."}},
				{Status: "running", TurnCount: 5, TotalCostUSD: 0.10, InputTokens: 12000, OutputTokens: 5000,
					RecentOutput: []string{"Using bcrypt for password hashing.", "Running tests..."}},
				{Status: "completed", TurnCount: 6, TotalCostUSD: 0.12, InputTokens: 14000, OutputTokens: 5800,
					RecentOutput: []string{"All tests passing.", "Auth implementation complete."}},
			}},
			// Second builder: docs update.
			{States: []session.MockSessionState{
				{Status: "running", TurnCount: 1, TotalCostUSD: 0.01, InputTokens: 1500, OutputTokens: 600,
					RecentOutput: []string{"Reading existing OpenAPI spec..."}},
				{Status: "completed", TurnCount: 3, TotalCostUSD: 0.04, InputTokens: 4000, OutputTokens: 1800,
					RecentOutput: []string{"Added /auth/register and /auth/login endpoints.", "Added JWT security scheme.", "Docs update complete."}},
			}},
		},
	}
}
