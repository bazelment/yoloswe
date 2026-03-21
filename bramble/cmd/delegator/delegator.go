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
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

var (
	mode             string
	model            string
	autoAdvance      bool
	behaviorFlag     string
	systemPromptFile string
	prompt           string
	workDir          string
	verbose          bool
	logDir           string
	timeout          time.Duration
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
	Cmd.Flags().StringVar(&mode, "mode", "mock", "Mode: mock (scripted children) or real (Manager-based with real sessions)")
	Cmd.Flags().StringVar(&model, "model", "sonnet", "Claude model for the delegator")
	Cmd.Flags().BoolVar(&autoAdvance, "auto-advance", true, "Auto-send child state notifications (mock mode only)")
	Cmd.Flags().StringVar(&behaviorFlag, "behavior", "", "State progressions, e.g. planner=running,completed;builder=running,completed (mock mode only)")
	Cmd.Flags().StringVar(&systemPromptFile, "system-prompt", "", "Override system prompt from file (mock mode only)")
	Cmd.Flags().StringVar(&prompt, "prompt", "", "Non-interactive: run a single conversation with this prompt")
	Cmd.Flags().StringVar(&workDir, "work-dir", ".", "Working directory for the Claude session")
	Cmd.Flags().BoolVar(&verbose, "verbose", false, "Show non-error tool results / child session output")
	Cmd.Flags().StringVar(&logDir, "log-dir", "", "Directory for JSONL session recording")
	Cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Timeout for non-interactive mode (real mode only)")
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
		if err := runReal(ctx, model, workDir, prompt, logDir, timeout); err != nil {
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

	// Interactive mode.
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("You> ")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("You> ")
			continue
		}
		if line == "quit" || line == "exit" {
			break
		}

		runMockConversation(ctx, s, mock, renderer, line, autoAdvance)
		fmt.Print("\nYou> ")
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

func runReal(ctx context.Context, model, workDir, initialPrompt, logDir string, timeout time.Duration) error {
	// Probe installed providers so the delegator knows which models are available.
	pa := agent.NewProviderAvailability()
	registry := agent.NewModelRegistry(pa, nil)

	cfg := session.ManagerConfig{
		SessionMode:   session.SessionModeTUI,
		ModelRegistry: registry,
	}
	if logDir != "" {
		cfg.RecordingDir = logDir
		cfg.ProtocolLogDir = logDir
	}

	m := session.NewManagerWithConfig(cfg)
	defer m.Close()

	prompt := initialPrompt
	interactive := false
	if prompt == "" {
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) != 0 {
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

	fmt.Printf("Delegator Test Harness (real) | Model: %s | Work dir: %s\n\n", model, workDir)

	if interactive {
		fmt.Print("You> ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return nil
		}
		prompt = strings.TrimSpace(scanner.Text())
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
	var prevLineType session.OutputLineType

	// renderNewOutput reads the delegator's accumulated output from the Manager
	// and prints any lines added since the last render.
	renderNewOutput := func() {
		lines := m.GetSessionOutput(delegatorID)
		for i := linesRendered; i < len(lines); i++ {
			line := lines[i]
			// Add separator between thinking and text blocks.
			if (line.Type == session.OutputTypeText && prevLineType == session.OutputTypeThinking) ||
				(line.Type == session.OutputTypeThinking && prevLineType == session.OutputTypeText) {
				fmt.Println()
			}
			switch line.Type {
			case session.OutputTypeText:
				fmt.Println(strings.TrimRight(line.Content, "\n"))
			case session.OutputTypeThinking:
				fmt.Fprintf(os.Stdout, "%s%s%s%s\n",
					render.ColorDim, render.ColorItalic,
					strings.TrimRight(line.Content, "\n"),
					render.ColorReset)
			case session.OutputTypeToolStart:
				name := shortToolName(line.ToolName)
				if !isDelegatorTool(name) {
					continue
				}
				input := formatToolSummary(name, line.ToolInput)
				if input != "" {
					fmt.Fprintf(os.Stdout, "  %s%s%s %s\n", render.ColorCyan, name, render.ColorReset, input)
				} else {
					fmt.Fprintf(os.Stdout, "  %s%s%s\n", render.ColorCyan, name, render.ColorReset)
				}
			case session.OutputTypeTurnEnd:
				status, color := "✓", render.ColorGreen
				if line.IsError {
					status, color = "✗", render.ColorRed
				}
				fmt.Fprintf(os.Stdout, "%s%s Turn %d (%.1fs, $%.4f)%s\n\n",
					color, status, line.TurnNumber,
					float64(line.DurationMs)/1000, line.CostUSD, render.ColorReset)
			case session.OutputTypeError:
				fmt.Fprintf(os.Stderr, "%sError: %s%s\n",
					render.ColorRed, line.Content, render.ColorReset)
			}
			if line.Type == session.OutputTypeText || line.Type == session.OutputTypeThinking {
				prevLineType = line.Type
			}
		}
		linesRendered = len(lines)
	}

	// renderChildStatus prints a summary line for a child session state change.
	renderChildStatus := func(childID session.SessionID, newStatus session.SessionStatus) {
		info, ok := m.GetSessionInfo(childID)
		if !ok {
			return
		}
		label := fmt.Sprintf("%s (%s)", childID, info.Type)
		switch newStatus {
		case session.StatusRunning:
			fmt.Fprintf(os.Stdout, "  %s▶ %s%s\n", render.ColorCyan, label, render.ColorReset)
		case session.StatusCompleted:
			summary := ""
			if len(info.Progress.RecentOutput) > 0 {
				summary = " — " + info.Progress.RecentOutput[len(info.Progress.RecentOutput)-1]
			}
			fmt.Fprintf(os.Stdout, "  %s✓ %s completed (%s)%s%s\n",
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
			fmt.Fprintf(os.Stdout, "  %s✓ %s done%s%s%s\n",
				render.ColorGreen, label,
				detail, summary, render.ColorReset)
		case session.StatusFailed:
			errMsg := info.ErrorMsg
			if errMsg == "" {
				errMsg = "unknown error"
			}
			fmt.Fprintf(os.Stdout, "  %s✗ %s failed: %s%s\n",
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
				// Show progress for active child sessions.
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
					fmt.Fprintf(os.Stdout, "  %s⏳ %s (%s)%s%s%s\n",
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
							select {
							case idleCh <- struct{}{}:
							default:
							}
						}
					}
				}
			}
		}
	}()

	if interactive {
		// Interactive loop: wait for delegator to go idle, then prompt for input.
		// Read stdin in a goroutine so we can select on doneCh without blocking.
		stdinCh := make(chan string)
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				stdinCh <- scanner.Text()
			}
			close(stdinCh)
		}()
		for {
			select {
			case <-doneCh:
				goto done
			case <-ctx.Done():
				goto done
			case <-idleCh:
				if hasActiveChildren(m, delegatorID) {
					fmt.Print("\nYou (children active)> ")
				} else {
					fmt.Print("\nYou> ")
				}
				// Wait for stdin input or completion.
				select {
				case <-doneCh:
					goto done
				case <-ctx.Done():
					goto done
				case text, ok := <-stdinCh:
					if !ok {
						goto done
					}
					line := strings.TrimSpace(text)
					if line == "" {
						continue
					}
					if line == "quit" || line == "exit" {
						goto done
					}
					if err := m.SendFollowUp(delegatorID, line); err != nil {
						fmt.Fprintf(os.Stderr, "SendFollowUp error: %v (delegator may be processing a notification, try again)\n", err)
					}
				}
			}
		}
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

done:
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
	for _, t := range []session.SessionType{session.SessionTypePlanner, session.SessionTypeBuilder} {
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
	fmt.Fprintf(os.Stdout, "Total: $%.4f (%s)\n", totalCost, strings.Join(parts, " | "))

	if logDir != "" {
		fmt.Printf("\nSession JSONL logs saved to: %s\n", logDir)
	}
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
	case "start_session", "stop_session", "get_session_progress":
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
