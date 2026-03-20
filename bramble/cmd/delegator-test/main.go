// Command delegator-test is an interactive CLI harness for testing the
// delegator agent's system prompt with mock or real tools.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/bramble/session"
)

func main() {
	mode := flag.String("mode", "mock", "Mode: mock (scripted children) or real (Manager-based with real sessions)")
	model := flag.String("model", "sonnet", "Claude model for the delegator")
	autoAdvance := flag.Bool("auto-advance", true, "Auto-send child state notifications (mock mode only)")
	behaviorFlag := flag.String("behavior", "", "State progressions, e.g. planner=running,completed;builder=running,completed (mock mode only)")
	systemPromptFile := flag.String("system-prompt", "", "Override system prompt from file (mock mode only)")
	prompt := flag.String("prompt", "", "Non-interactive: run a single conversation with this prompt")
	workDir := flag.String("work-dir", ".", "Working directory for the Claude session")
	verbose := flag.Bool("verbose", false, "Show non-error tool results / child session output")
	logDir := flag.String("log-dir", "", "Directory for JSONL session recording")
	timeout := flag.Duration("timeout", 5*time.Minute, "Timeout for non-interactive mode (real mode only)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	renderer := render.NewRenderer(os.Stdout, *verbose)

	switch *mode {
	case "mock":
		runMock(ctx, renderer, *model, *workDir, *prompt, *logDir, *autoAdvance, *behaviorFlag, *systemPromptFile)
	case "real":
		info, err := os.Stat(*workDir)
		if err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "--work-dir must be a valid directory in real mode\n")
			os.Exit(1)
		}
		runReal(ctx, *model, *workDir, *prompt, *logDir, *timeout)
	default:
		fmt.Fprintf(os.Stderr, "Unknown mode %q: use mock or real\n", *mode)
		os.Exit(1)
	}
}

func runMock(ctx context.Context, renderer *render.Renderer, model, workDir, prompt, logDir string, autoAdvance bool, behaviorFlag, systemPromptFile string) {
	behaviors := parseBehaviors(behaviorFlag)

	systemPrompt := session.DelegatorSystemPrompt
	if systemPromptFile != "" {
		data, err := os.ReadFile(systemPromptFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading system prompt file: %v\n", err)
			os.Exit(1)
		}
		systemPrompt = string(data)
	}

	mock := session.NewMockDelegatorToolHandler(behaviors)

	opts := []claude.SessionOption{
		claude.WithModel(model),
		claude.WithPermissionMode(claude.PermissionModePlan),
		claude.WithDangerouslySkipPermissions(),
		claude.WithSDKTools("delegator-tools", mock.Registry()),
		claude.WithTools(""),
		claude.WithSystemPrompt(systemPrompt),
		claude.WithWorkDir(workDir),
		claude.WithDisablePlugins(),
		claude.WithEventBufferSize(1000),
	}
	if logDir != "" {
		opts = append(opts, claude.WithRecording(logDir))
	}

	s := claude.NewSession(opts...)

	if err := s.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start session: %v\n", err)
		os.Exit(1)
	}
	defer s.Stop()

	fmt.Printf("Delegator Test Harness (mock) | Model: %s | Auto-advance: %v\n\n", model, autoAdvance)

	if prompt != "" {
		runMockConversation(ctx, s, mock, renderer, prompt, autoAdvance)
		if logDir != "" {
			fmt.Printf("\nSession JSONL logs saved to: %s\n", logDir)
		}
		return
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

func runReal(ctx context.Context, model, workDir, initialPrompt, logDir string, timeout time.Duration) {
	cfg := session.ManagerConfig{
		SessionMode: session.SessionModeTUI,
	}
	if logDir != "" {
		cfg.RecordingDir = logDir
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
				return
			}
		}
	}

	fmt.Printf("Delegator Test Harness (real) | Model: %s | Work dir: %s\n\n", model, workDir)

	if interactive {
		fmt.Print("You> ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return
		}
		prompt = strings.TrimSpace(scanner.Text())
		if prompt == "" || prompt == "quit" || prompt == "exit" {
			return
		}
	}

	delegatorID, err := m.StartSession(session.SessionTypeDelegator, workDir, prompt, model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start delegator session: %v\n", err)
		os.Exit(1)
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
			fmt.Fprintf(os.Stdout, "  %s✓ %s completed (turns: %d, $%.4f)%s%s\n",
				render.ColorGreen, label,
				info.Progress.TurnCount, info.Progress.TotalCostUSD,
				summary, render.ColorReset)
		case session.StatusIdle:
			summary := ""
			if len(info.Progress.RecentOutput) > 0 {
				summary = " — " + info.Progress.RecentOutput[len(info.Progress.RecentOutput)-1]
			}
			detail := ""
			if info.Progress.TurnCount > 0 || info.Progress.TotalCostUSD > 0 {
				detail = fmt.Sprintf(" (turns: %d, $%.4f)", info.Progress.TurnCount, info.Progress.TotalCostUSD)
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

	// Event loop goroutine — drains Manager events and signals turn/state
	// boundaries. Rendering happens in response to these signals rather
	// than per-event, so we always see fully accumulated text.
	go func() {
		progressTicker := time.NewTicker(30 * time.Second)
		defer progressTicker.Stop()
		for {
			select {
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
						detail = fmt.Sprintf(", turns: %d, $%.4f", sessions[i].Progress.TurnCount, sessions[i].Progress.TotalCostUSD)
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
		scanner := bufio.NewScanner(os.Stdin)
		for {
			select {
			case <-doneCh:
				goto done
			case <-ctx.Done():
				goto done
			case <-idleCh:
				// Check if any children are still running.
				if hasActiveChildren(m, delegatorID) {
					continue
				}
				fmt.Print("\nYou> ")
				if !scanner.Scan() {
					goto done
				}
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				if line == "quit" || line == "exit" {
					goto done
				}
				if err := m.SendFollowUp(delegatorID, line); err != nil {
					fmt.Fprintf(os.Stderr, "SendFollowUp error: %v\n", err)
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
	// Let events settle before reading final state.
	time.Sleep(100 * time.Millisecond)

	// Print total summary across all sessions.
	// Compute delegator costs from rendered output (more reliable than progress fields)
	// and child costs from session info.
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
	// Count child sessions by type and aggregate their costs.
	childCounts := make(map[session.SessionType]int)
	childCosts := make(map[session.SessionType]float64)
	allSessions := m.GetAllSessions()
	for i := range allSessions {
		if allSessions[i].ID != delegatorID {
			t := allSessions[i].Type
			childCounts[t]++
			childCosts[t] += allSessions[i].Progress.TotalCostUSD
			totalCost += allSessions[i].Progress.TotalCostUSD
		}
	}
	// Build summary: "delegator: N turns | planner: M sessions | builder: K sessions"
	parts := []string{fmt.Sprintf("delegator: %d turns, $%.4f", delegatorTurns, delegatorCost)}
	for _, t := range []session.SessionType{session.SessionTypePlanner, session.SessionTypeBuilder} {
		if cnt, ok := childCounts[t]; ok {
			noun := "sessions"
			if cnt == 1 {
				noun = "session"
			}
			parts = append(parts, fmt.Sprintf("%s: %d %s, $%.4f", t, cnt, noun, childCosts[t]))
		} else {
			parts = append(parts, fmt.Sprintf("%s: 0 sessions", t))
		}
	}
	fmt.Fprintf(os.Stdout, "Total: $%.4f (%s)\n", totalCost, strings.Join(parts, " | "))

	if logDir != "" {
		fmt.Printf("\nSession JSONL logs saved to: %s\n", logDir)
	}
}

// formatToolSummary returns a short description of a tool invocation's input.
func formatToolSummary(name string, input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	switch name {
	case "start_session":
		typ, _ := input["session_type"].(string)
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
