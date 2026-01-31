// Package yoloplanner provides a simple wrapper over claude-cli for planner mode.
//
// # Permission Mode and Setup
//
// This package demonstrates a specific permission handling approach:
//
//  1. Start with PermissionModeDefault - The CLI starts without --permission-mode plan
//     or --dangerously-skip-permissions flags, allowing fine-grained permission control.
//
//  2. Use AllowAllPermissionHandler - Instead of --dangerously-skip-permissions, we
//     register a permission handler that auto-approves all tool execution requests.
//     This gives us programmatic control over permissions (could be extended to
//     selectively approve/deny based on tool name, input, etc.).
//
//  3. Switch to plan mode via control message - After the session starts, we send a
//     set_permission_mode control request to switch to plan mode. This approach was
//     validated in experiments/plan_mode_analysis/ANALYSIS.md (Experiment C).
//
// # Limitations
//
//   - The AllowAllPermissionHandler approves everything - for production use, you may
//     want a more selective handler that reviews dangerous operations.
//   - Control message responses are not currently awaited - we fire-and-forget the
//     mode switch. The CLI processes it before the first user message.
package planner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
)

// readLineWithContext reads a line from stdin with context cancellation support.
//
// Design notes:
//   - We read directly from os.Stdin byte-by-byte instead of using bufio.Reader
//     to avoid buffering issues where input typed during one prompt leaks into
//     the next prompt.
//   - We use SetReadDeadline with short timeouts to make the read interruptible,
//     allowing clean cancellation when the context is cancelled (e.g., Ctrl+C).
//   - If SetReadDeadline isn't supported (e.g., pipes), we fall back to a goroutine
//     approach with potential leak on cancellation.
//   - This approach trades some efficiency (byte-by-byte vs buffered) for
//     correctness (no input leakage) and clean shutdown (no goroutine leaks).
func (p *PlannerWrapper) readLineWithContext(ctx context.Context) (string, error) {
	// Try to set deadline to test if it's supported
	testErr := os.Stdin.SetReadDeadline(time.Now().Add(time.Millisecond))
	os.Stdin.SetReadDeadline(time.Time{}) // Clear test deadline

	if testErr != nil {
		// Deadlines not supported - fall back to goroutine approach
		return p.readLineWithGoroutine(ctx)
	}

	var line strings.Builder
	buf := make([]byte, 1)

	for {
		// Short deadline allows periodic context checking for clean cancellation
		os.Stdin.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		n, err := os.Stdin.Read(buf)
		if err != nil {
			if os.IsTimeout(err) {
				// Timeout - check if we should stop
				select {
				case <-ctx.Done():
					os.Stdin.SetReadDeadline(time.Time{}) // Clear deadline
					return "", ctx.Err()
				default:
					continue // Keep waiting for input
				}
			}
			os.Stdin.SetReadDeadline(time.Time{}) // Clear deadline
			return "", err
		}

		if n > 0 {
			if buf[0] == '\n' {
				os.Stdin.SetReadDeadline(time.Time{}) // Clear deadline
				return strings.TrimSpace(line.String()), nil
			}
			line.WriteByte(buf[0])
		}
	}
}

// readLineWithGoroutine reads a line using a goroutine when deadlines aren't supported.
// Note: If context is cancelled, the goroutine may leak until stdin receives input.
func (p *PlannerWrapper) readLineWithGoroutine(ctx context.Context) (string, error) {
	type result struct {
		err  error
		line string
	}
	ch := make(chan result, 1)

	go func() {
		var line strings.Builder
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				ch <- result{err, ""}
				return
			}
			if n > 0 {
				if buf[0] == '\n' {
					ch <- result{nil, strings.TrimSpace(line.String())}
					return
				}
				line.WriteByte(buf[0])
			}
		}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		return r.line, r.err
	}
}

// BuildMode controls what happens after planning completes.
type BuildMode string

const (
	// BuildModeNone prompts interactively (default).
	BuildModeNone BuildMode = ""
	// BuildModeCurrent executes in current session with full context.
	BuildModeCurrent BuildMode = "current"
	// BuildModeNewSession starts a fresh session to implement the plan.
	BuildModeNewSession BuildMode = "new"
)

// IsValid returns true if the BuildMode is a recognized value.
func (m BuildMode) IsValid() bool {
	switch m {
	case BuildModeNone, BuildModeCurrent, BuildModeNewSession:
		return true
	default:
		return false
	}
}

// Config holds planner configuration.
type Config struct {
	// Model to use: "haiku", "sonnet", "opus"
	Model string

	// WorkDir is the working directory for file operations.
	WorkDir string

	// RecordingDir is the directory for session recordings.
	RecordingDir string

	// SystemPrompt overrides the default system prompt.
	SystemPrompt string

	// Prompt is the initial prompt, used for generating output filenames in simple mode.
	Prompt string

	// BuildMode controls what happens after planning completes in simple mode.
	// "current" = execute in current session, "new" = execute in new session.
	BuildMode BuildMode

	// ExternalBuilderPath is the path to an external builder executable.
	// When set with BuildMode "new", the external builder is launched instead
	// of starting a fresh claude session.
	ExternalBuilderPath string

	// BuildModel is the model to use for build phase.
	// If empty, uses Model for both planning and building.
	// When set, planning uses Model and building uses BuildModel.
	BuildModel string

	// Verbose enables detailed tool result output. When false, only errors are shown.
	Verbose bool

	// Simple enables non-interactive mode: auto-answers questions with first option
	// and exports plan to markdown on completion.
	Simple bool
}

// SessionStats tracks cumulative token usage and cost for a session phase.
type SessionStats struct {
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	CostUSD         float64
	TurnCount       int
}

// Add accumulates stats from a turn.
func (s *SessionStats) Add(usage claude.TurnUsage) {
	s.InputTokens += usage.InputTokens
	s.OutputTokens += usage.OutputTokens
	s.CacheReadTokens += usage.CacheReadTokens
	s.CostUSD += usage.CostUSD
	s.TurnCount++
}

// PlannerWrapper wraps a claude.Session with planner-specific logic.
type PlannerWrapper struct {
	session             *claude.Session
	renderer            *Renderer
	planFilePath        string
	config              Config
	planningStats       SessionStats
	buildingStats       SessionStats
	waitingForUserInput bool
	inBuildPhase        bool
	pendingBuildStart   bool
}

// NewPlannerWrapper creates a new planner wrapper with the given configuration.
func NewPlannerWrapper(config Config) *PlannerWrapper {
	// Apply defaults
	if config.Model == "" {
		config.Model = "sonnet"
	}
	if config.RecordingDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			config.RecordingDir = filepath.Join(home, ".yoloswe")
		} else {
			config.RecordingDir = ".planner-sessions"
		}
	}

	p := &PlannerWrapper{
		config:   config,
		renderer: NewRenderer(os.Stdout, config.Verbose),
	}

	return p
}

// plannerInteractiveHandler implements claude.InteractiveToolHandler for the planner.
type plannerInteractiveHandler struct {
	p *PlannerWrapper
}

// HandleAskUserQuestion collects user responses for AskUserQuestion.
func (h *plannerInteractiveHandler) HandleAskUserQuestion(ctx context.Context, questions []claude.Question) (map[string]string, error) {
	answers := make(map[string]string)

	for i, q := range questions {
		// Convert to render options
		options := make([]render.QuestionOption, len(q.Options))
		for j, opt := range q.Options {
			options[j] = render.QuestionOption{Label: opt.Label}
		}

		// In simple mode, auto-select first option but show full question
		if h.p.config.Simple && len(options) > 0 {
			response := options[0].Label
			h.p.renderer.QuestionAutoAnswer(q.Text, "", options, 0)
			answers[q.Text] = response
			continue
		}

		h.p.renderer.QuestionWithOptions(q.Text, "", options)

		fmt.Printf("\nYour answer: ")
		response, err := h.p.readLineWithContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to read user input: %w", err)
		}

		// Handle numeric selection
		if len(options) > 0 {
			if idx := parseOptionIndex(response, len(options)); idx >= 0 {
				response = options[idx].Label
			}
		}

		answers[q.Text] = response
		fmt.Printf("  [Q%d] Selected: %s\n", i+1, response)
	}

	return answers, nil
}

// HandleExitPlanMode is not used by yoloplanner - ExitPlanMode is handled
// in the event loop via handleExitPlanMode method for more complex logic.
// This handler just returns approval to allow the tool to proceed.
func (h *plannerInteractiveHandler) HandleExitPlanMode(ctx context.Context, plan claude.PlanInfo) (string, error) {
	// Return empty string - yoloplanner handles ExitPlanMode in the event loop
	// via ToolCompleteEvent where it has access to toolUseID for refinement feedback.
	return "", nil
}

// Start initializes and starts the underlying claude session.
//
// The session is configured with:
//   - PermissionModeDefault: Start without plan mode flag, allowing control message switch
//   - Custom permission handler: Auto-approves most tools, but handles AskUserQuestion
//     specially by collecting user input and embedding answers in updatedInput
//   - Recording enabled: All messages are recorded for debugging/replay
//
// After starting, we switch to plan mode via a control message. This approach
// (vs starting directly with --permission-mode plan) demonstrates dynamic
// permission mode changes through the protocol.
func (p *PlannerWrapper) Start(ctx context.Context) error {
	opts := []claude.SessionOption{
		claude.WithModel(p.config.Model),
		// Start in default mode - we'll switch to plan mode via control message
		claude.WithPermissionMode(claude.PermissionModeDefault),
		// Route all permission prompts through stdio protocol
		claude.WithPermissionPromptToolStdio(),
		// Auto-approve all permission requests
		claude.WithPermissionHandler(claude.AllowAllPermissionHandler()),
		// Use interactive tool handler for AskUserQuestion
		claude.WithInteractiveToolHandler(&plannerInteractiveHandler{p}),
		claude.WithRecording(p.config.RecordingDir),
	}

	if p.config.WorkDir != "" {
		opts = append(opts, claude.WithWorkDir(p.config.WorkDir))
	}

	if p.config.SystemPrompt != "" {
		opts = append(opts, claude.WithSystemPrompt(p.config.SystemPrompt))
	}

	p.session = claude.NewSession(opts...)

	// Print CLI flags that will be used
	if args, err := p.session.CLIArgs(); err == nil {
		fmt.Fprintf(os.Stderr, "Starting claude with flags: %s\n", strings.Join(args, " "))
	}

	if err := p.session.Start(ctx); err != nil {
		return err
	}

	// Switch to plan mode via control message (per experiments/plan_mode_analysis/ANALYSIS.md)
	return p.session.SetPermissionMode(ctx, claude.PermissionModePlan)
}

// Stop gracefully shuts down the session.
func (p *PlannerWrapper) Stop() error {
	if p.session != nil {
		return p.session.Stop()
	}
	return nil
}

// Run sends the initial prompt and processes events until completion.
func (p *PlannerWrapper) Run(ctx context.Context, prompt string) error {
	_, err := p.session.SendMessage(ctx, prompt)
	if err != nil {
		return fmt.Errorf("failed to send initial prompt: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-p.session.Events():
			if !ok {
				// Channel closed - session ended
				// In non-simple mode with build phase complete, prompt for follow-up
				if !p.config.Simple && p.inBuildPhase {
					done, err := p.promptForFollowUp(ctx)
					if err != nil {
						return err
					}
					if !done {
						// User provided follow-up input, restart event loop
						continue
					}
				}
				return nil
			}
			done, err := p.handleEvent(ctx, event)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// handleEvent processes a single event and returns (done, error).
func (p *PlannerWrapper) handleEvent(ctx context.Context, event claude.Event) (bool, error) {
	switch e := event.(type) {
	case claude.ReadyEvent:
		p.renderer.Status(fmt.Sprintf("Session started: %s (model: %s)", e.Info.SessionID, e.Info.Model))

	case claude.TextEvent:
		p.renderer.Text(e.Text)

	case claude.ThinkingEvent:
		p.renderer.Thinking(e.Thinking)

	case claude.ToolStartEvent:
		p.renderer.ToolStart(e.Name, e.ID)

	case claude.ToolProgressEvent:
		// Skip streaming tool progress - too noisy
		// p.renderer.ToolProgress(e.InputChunk)

	case claude.ToolCompleteEvent:
		p.renderer.ToolComplete(e.Name, e.Input)

		// Track plan file writes
		p.trackPlanFileWrite(e.Name, e.Input)

		// Handle special interactive tools
		// Note: AskUserQuestion is now handled in the permission handler
		// (createPermissionHandler) which collects user input and embeds answers
		// in updatedInput before the CLI executes the tool. No action needed here.
		if e.Name == "ExitPlanMode" {
			// Note: We set waitingForUserInput=true here, but if we're executing
			// (not exporting), handleExitPlanMode will set it back to false
			// since we're not waiting for user input during implementation.
			p.waitingForUserInput = true
			return p.handleExitPlanMode(ctx, e.ID, e.Input)
		}

	case claude.CLIToolResultEvent:
		p.renderer.ToolResult(e.Content, e.IsError)

	case claude.TurnCompleteEvent:
		p.renderer.TurnSummary(e)

		// Handle the plan→build transition:
		// When pendingBuildStart is true, this TurnComplete is still from the
		// ExitPlanMode planning turn. We need to count it as planning stats,
		// transition to build phase, and continue waiting for the build response.
		if p.pendingBuildStart {
			// Count as planning stats - this is still the ExitPlanMode turn
			p.planningStats.Add(e.Usage)
			// Transition to build phase for subsequent turns
			p.inBuildPhase = true
			p.pendingBuildStart = false
			// Keep waitingForUserInput=true so we don't exit yet
			// The actual build turn will clear this flag
			return false, nil
		}

		// Normal stats accumulation
		if p.inBuildPhase {
			p.buildingStats.Add(e.Usage)
		} else {
			p.planningStats.Add(e.Usage)
		}

		// If we're waiting for user input (AskUserQuestion/ExitPlanMode response),
		// clear the flag and continue/prompt as appropriate.
		if p.waitingForUserInput {
			p.waitingForUserInput = false
			// In non-simple mode with build phase complete, prompt for follow-up
			if p.inBuildPhase {
				// In simple mode, exit cleanly after build
				if p.config.Simple {
					return true, nil
				}
				// In non-simple mode, prompt for follow-up
				return p.promptForFollowUp(ctx)
			}
			// Otherwise continue the event loop (planning phase)
			return false, nil
		}

		// Only exit if build phase is complete or in simple mode.
		// During planning phase, continue waiting for ExitPlanMode/AskUserQuestion
		// (this handles the "Continue refining" case where model may respond
		// with text before calling ExitPlanMode again).
		if p.inBuildPhase || p.config.Simple {
			return true, nil
		}
		return false, nil

	case claude.ErrorEvent:
		p.renderer.Error(e.Error, e.Context)
		return false, e.Error
	}

	return false, nil
}

// handleExitPlanMode handles the ExitPlanMode tool call.
// Returns (done, error) where done=true means the session should end.
func (p *PlannerWrapper) handleExitPlanMode(ctx context.Context, toolUseID string, input map[string]interface{}) (bool, error) {
	p.renderer.PlanComplete(input)

	// In simple mode with build flag, auto-execute
	if p.config.Simple {
		switch p.config.BuildMode {
		case BuildModeCurrent:
			return p.executeInCurrentSession(ctx)
		case BuildModeNewSession:
			return p.executeInNewSession(ctx)
		default:
			// No build mode specified, export and exit (existing behavior)
			filename := p.generatePlanFilename()
			return p.exportPlanAndExit(ctx, filename)
		}
	}

	fmt.Println("\n" + strings.Repeat("─", 60))
	fmt.Println("What would you like to do?")
	fmt.Println("  1. Execute in current session (keeps context)")
	fmt.Println("  2. Execute in new session (fresh start)")
	fmt.Println("  3. Export plan to markdown")
	fmt.Println("  4. Continue refining")
	fmt.Println()
	fmt.Print("Enter choice (1-4) or feedback: ")

	choice, err := p.readLineWithContext(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to read user choice: %w", err)
	}

	switch choice {
	case "1":
		return p.executeInCurrentSession(ctx)

	case "2":
		return p.executeInNewSession(ctx)

	case "3":
		// Export to markdown
		filename := p.generatePlanFilename()
		return p.exportPlanAndExit(ctx, filename)

	default:
		// Continue refining - option "4" or any other input as feedback
		msg := "Please keep refining the plan."
		if choice != "4" {
			msg = fmt.Sprintf("Please refine the plan with this feedback: %s", choice)
		}
		_, err := p.session.SendToolResult(ctx, toolUseID, msg)
		return false, err
	}
}

// promptForFollowUp prompts the user for follow-up input after build completion.
// Returns (done, error). If user provides input, sends it and continues.
// If user signals EOF (Ctrl-D) or context is cancelled, exits gracefully.
func (p *PlannerWrapper) promptForFollowUp(ctx context.Context) (bool, error) {
	fmt.Println()
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println("Build complete. Enter follow-up message or Ctrl-D to exit:")
	fmt.Print("> ")

	input, err := p.readLineWithContext(ctx)
	if err != nil {
		// EOF (Ctrl-D) or context cancelled - exit gracefully
		if err == io.EOF || err == context.Canceled {
			fmt.Println("\nExiting.")
			return true, nil
		}
		return false, err
	}

	if strings.TrimSpace(input) == "" {
		// Empty input - prompt again
		return p.promptForFollowUp(ctx)
	}

	// Send follow-up message
	_, err = p.session.SendMessage(ctx, input)
	if err != nil {
		return false, fmt.Errorf("failed to send follow-up: %w", err)
	}

	return false, nil
}

// executeInCurrentSession switches to bypass mode and continues implementation.
func (p *PlannerWrapper) executeInCurrentSession(ctx context.Context) (bool, error) {
	fmt.Println("\n→ Executing plan in current session...")

	// Switch to bypass mode
	if err := p.session.SetPermissionMode(ctx, claude.PermissionModeBypass); err != nil {
		return false, fmt.Errorf("failed to switch permission mode: %w", err)
	}

	// Switch model if BuildModel is specified and different from current model
	if p.config.BuildModel != "" && p.config.BuildModel != p.config.Model {
		fmt.Printf("→ Switching to model: %s\n", p.config.BuildModel)
		if err := p.session.SetModel(ctx, p.config.BuildModel); err != nil {
			return false, fmt.Errorf("failed to switch model: %w", err)
		}
	}

	// Signal that we're starting build execution.
	// The next TurnComplete will be from the ExitPlanMode planning turn, which will:
	// 1. Count that turn's stats as planning (not build)
	// 2. Set inBuildPhase=true to transition to build phase
	// 3. Keep waitingForUserInput=true and continue waiting
	// Then the build turn will start processing "I approve this plan..." message.
	// When the build turn completes, waitingForUserInput will be cleared and we'll
	// prompt for follow-up (or exit in simple mode).
	p.pendingBuildStart = true
	_, err := p.session.SendMessage(ctx, "I approve this plan. Please proceed with implementation.")
	return false, err
}

// executeInNewSession stops current session and starts fresh to implement the plan.
// Requires a plan file to exist; falls back to current session if no plan file.
func (p *PlannerWrapper) executeInNewSession(ctx context.Context) (bool, error) {
	// If external builder is configured, use it instead
	if p.config.ExternalBuilderPath != "" {
		return p.executeWithExternalBuilder(ctx)
	}

	// Check if plan file exists - new session needs a file to reference
	if p.planFilePath == "" {
		fmt.Println("\n⚠ No plan file detected. Falling back to current session execution...")
		return p.executeInCurrentSession(ctx)
	}

	// Verify the plan file actually exists on disk
	if _, err := os.Stat(p.planFilePath); os.IsNotExist(err) {
		fmt.Printf("\n⚠ Plan file %s not found. Falling back to current session execution...\n", p.planFilePath)
		return p.executeInCurrentSession(ctx)
	}

	fmt.Println("\n→ Starting new session to implement plan...")

	// Log the planning session's recording path before stopping
	if oldRecordingPath := p.session.RecordingPath(); oldRecordingPath != "" {
		p.renderer.Status(fmt.Sprintf("Planning session recorded to: %s", oldRecordingPath))
	}

	// Note: There's no control message for "/clear" in the protocol.
	// We must stop the current session and start a fresh claude process.
	p.session.Stop()

	// Start new session with bypass permissions (no plan mode)
	// Use BuildModel if specified, otherwise fall back to Model
	modelToUse := p.config.BuildModel
	if modelToUse == "" {
		modelToUse = p.config.Model
	}
	newOpts := []claude.SessionOption{
		claude.WithModel(modelToUse),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithPermissionPromptToolStdio(),
		// Auto-approve all permission requests
		claude.WithPermissionHandler(claude.AllowAllPermissionHandler()),
		// Use interactive tool handler for AskUserQuestion
		claude.WithInteractiveToolHandler(&plannerInteractiveHandler{p}),
		claude.WithRecording(p.config.RecordingDir),
	}
	if p.config.WorkDir != "" {
		newOpts = append(newOpts, claude.WithWorkDir(p.config.WorkDir))
	}
	if p.config.SystemPrompt != "" {
		newOpts = append(newOpts, claude.WithSystemPrompt(p.config.SystemPrompt))
	}

	p.session = claude.NewSession(newOpts...)
	if err := p.session.Start(ctx); err != nil {
		return false, fmt.Errorf("failed to start new session: %w", err)
	}

	// Log that we're starting implementation in a new session
	p.renderer.Status(fmt.Sprintf("Implementation session started, using plan: %s", p.planFilePath))

	// For new session, we transition to build phase immediately since this is a fresh session.
	// Note: The final planning turn's TurnComplete event (with stats) won't arrive because
	// we stopped the old session. This means the final planning turn's stats are lost.
	// This is an acceptable tradeoff for getting a fresh context in the new session.
	p.inBuildPhase = true
	// Keep waitingForUserInput=true (set by handleExitPlanMode) so the event loop continues.
	// It will be set to false by the TurnComplete handler when the build turn completes.

	// Don't switch to plan mode - we want to execute directly
	msg := fmt.Sprintf("Implement the plan in %s", p.planFilePath)
	_, err := p.session.SendMessage(ctx, msg)
	return false, err
}

// executeWithExternalBuilder launches an external builder tool with the plan file.
// Returns (done=true, error) to signal session should exit.
func (p *PlannerWrapper) executeWithExternalBuilder(ctx context.Context) (bool, error) {
	// 1. Validate external builder path
	builderPath, err := validateExecutablePath(p.config.ExternalBuilderPath)
	if err != nil {
		return false, fmt.Errorf("invalid external builder: %w", err)
	}

	// 2. Check plan file exists
	if p.planFilePath == "" {
		return false, fmt.Errorf("no plan file available for external builder")
	}
	if _, err := os.Stat(p.planFilePath); os.IsNotExist(err) {
		return false, fmt.Errorf("plan file not found: %s", p.planFilePath)
	}

	// 3. Build command arguments
	args := p.buildExternalBuilderArgs()

	// 4. Create command with context (for cancellation)
	cmd := exec.CommandContext(ctx, builderPath, args...)
	cmd.Dir = p.config.WorkDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// 5. Log what we're doing
	fmt.Printf("\n→ Launching external builder: %s\n", builderPath)
	if p.config.Verbose {
		fmt.Printf("  Command: %s %s\n", builderPath, strings.Join(args, " "))
	}

	// 6. Run and wait for completion
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			// Context was cancelled (e.g., Ctrl+C) - exit gracefully
			return true, nil
		}
		return false, fmt.Errorf("external builder failed: %w", err)
	}

	// 7. Success - signal to exit session
	return true, nil
}

// validateExecutablePath checks if the path points to an executable file.
// Returns absolute path if valid, error otherwise.
// Supports both absolute/relative paths and bare executable names in PATH.
func validateExecutablePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}

	// If path contains a separator, treat as file path (absolute or relative)
	// Otherwise, try PATH lookup for bare executable name
	var absPath string
	if strings.ContainsRune(path, filepath.Separator) {
		// Resolve to absolute path
		var err error
		absPath, err = filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("failed to resolve path: %w", err)
		}
	} else {
		// Look up in PATH
		var err error
		absPath, err = exec.LookPath(path)
		if err != nil {
			return "", fmt.Errorf("executable not found in PATH: %s", path)
		}
	}

	// Check file exists
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s", absPath)
		}
		return "", err
	}

	// Check is regular file
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", absPath)
	}

	// Check is executable (Unix-like systems)
	if info.Mode()&0111 == 0 {
		return "", fmt.Errorf("file is not executable: %s (try: chmod +x)", absPath)
	}

	return absPath, nil
}

// buildExternalBuilderArgs constructs command arguments for the external builder.
// Maps yoloplanner config to external builder CLI flags (yoloswe format).
func (p *PlannerWrapper) buildExternalBuilderArgs() []string {
	args := []string{}

	// Map model to builder-model
	// Use BuildModel if specified, otherwise fall back to Model
	modelToUse := p.config.BuildModel
	if modelToUse == "" {
		modelToUse = p.config.Model
	}
	if modelToUse != "" {
		args = append(args, "--builder-model", modelToUse)
	}

	// Map working directory
	if p.config.WorkDir != "" {
		args = append(args, "--dir", p.config.WorkDir)
	}

	// Map recording directory
	if p.config.RecordingDir != "" {
		args = append(args, "--record", p.config.RecordingDir)
	}

	// Map system prompt
	if p.config.SystemPrompt != "" {
		args = append(args, "--system", p.config.SystemPrompt)
	}

	// Map verbose flag
	if p.config.Verbose {
		args = append(args, "--verbose")
	}

	// Add prompt: "Implement the plan in <plan-file>"
	prompt := fmt.Sprintf("Implement the plan in %s", p.planFilePath)
	args = append(args, prompt)

	return args
}

// parseOptionIndex parses a 1-based numeric option selection.
// Returns -1 if not a valid option number.
func parseOptionIndex(input string, numOptions int) int {
	var idx int
	if _, err := fmt.Sscanf(input, "%d", &idx); err != nil {
		return -1
	}
	if idx < 1 || idx > numOptions {
		return -1
	}
	return idx - 1
}

// RecordingPath returns the path to the session recording directory.
func (p *PlannerWrapper) RecordingPath() string {
	if p.session != nil {
		return p.session.RecordingPath()
	}
	return ""
}

// trackPlanFileWrite detects when Claude writes to a plan file in .claude/plans/.
func (p *PlannerWrapper) trackPlanFileWrite(toolName string, input map[string]interface{}) {
	if toolName != "Write" {
		return
	}
	filePath, ok := input["file_path"].(string)
	if !ok {
		return
	}
	if strings.Contains(filePath, ".claude/plans/") && strings.HasSuffix(filePath, ".md") {
		p.planFilePath = filePath
	}
}

// exportPlanAndExit exports the plan to the given filename and returns done=true.
// If no plan file was detected, it asks Claude to write the plan first.
// Returns (done, error) where done=true means the session should end.
func (p *PlannerWrapper) exportPlanAndExit(ctx context.Context, filename string) (bool, error) {
	if p.planFilePath == "" {
		// No plan file detected - ask Claude to write it
		p.renderer.Status("No plan file detected, requesting export...")
		_, err := p.session.SendMessage(ctx, fmt.Sprintf("Please write your complete plan to %s", filename))
		return false, err
	}

	// Read and export the actual plan file written by Claude
	if err := p.exportPlanToFile(filename); err != nil {
		return false, err
	}
	fmt.Printf("\n✓ Plan exported to %s\n", filename)
	return true, nil
}

// exportPlanToFile copies the plan file to the specified destination.
func (p *PlannerWrapper) exportPlanToFile(destPath string) error {
	if p.planFilePath == "" {
		return fmt.Errorf("no plan file path set")
	}
	data, err := os.ReadFile(p.planFilePath)
	if err != nil {
		return fmt.Errorf("failed to read plan file %s: %w", p.planFilePath, err)
	}
	if err := os.WriteFile(destPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write export file %s: %w", destPath, err)
	}
	return nil
}

// PlanFilePath returns the path to the detected plan file.
func (p *PlannerWrapper) PlanFilePath() string {
	return p.planFilePath
}

// PrintUsageSummary prints the cumulative token usage and cost for both phases.
func (p *PlannerWrapper) PrintUsageSummary() {
	fmt.Println("\n" + strings.Repeat("═", 60))
	fmt.Println("SESSION USAGE SUMMARY")
	fmt.Println(strings.Repeat("─", 60))

	// Planning phase stats
	fmt.Println("Planning Phase:")
	fmt.Printf("  Turns:        %d\n", p.planningStats.TurnCount)
	fmt.Printf("  Input tokens: %d\n", p.planningStats.InputTokens)
	fmt.Printf("  Output tokens: %d\n", p.planningStats.OutputTokens)
	if p.planningStats.CacheReadTokens > 0 {
		fmt.Printf("  Cache read:   %d\n", p.planningStats.CacheReadTokens)
	}
	fmt.Printf("  Cost:         $%.4f\n", p.planningStats.CostUSD)

	// Building phase stats (only if we entered build phase)
	if p.inBuildPhase {
		fmt.Println(strings.Repeat("─", 60))
		fmt.Println("Building Phase:")
		fmt.Printf("  Turns:        %d\n", p.buildingStats.TurnCount)
		fmt.Printf("  Input tokens: %d\n", p.buildingStats.InputTokens)
		fmt.Printf("  Output tokens: %d\n", p.buildingStats.OutputTokens)
		if p.buildingStats.CacheReadTokens > 0 {
			fmt.Printf("  Cache read:   %d\n", p.buildingStats.CacheReadTokens)
		}
		fmt.Printf("  Cost:         $%.4f\n", p.buildingStats.CostUSD)
	}

	// Total
	fmt.Println(strings.Repeat("─", 60))
	totalInput := p.planningStats.InputTokens + p.buildingStats.InputTokens
	totalOutput := p.planningStats.OutputTokens + p.buildingStats.OutputTokens
	totalCache := p.planningStats.CacheReadTokens + p.buildingStats.CacheReadTokens
	totalCost := p.planningStats.CostUSD + p.buildingStats.CostUSD
	totalTurns := p.planningStats.TurnCount + p.buildingStats.TurnCount

	fmt.Println("TOTAL:")
	fmt.Printf("  Turns:        %d\n", totalTurns)
	fmt.Printf("  Input tokens: %d\n", totalInput)
	fmt.Printf("  Output tokens: %d\n", totalOutput)
	if totalCache > 0 {
		fmt.Printf("  Cache read:   %d\n", totalCache)
	}
	fmt.Printf("  Cost:         $%.4f\n", totalCost)
	fmt.Println(strings.Repeat("═", 60))
}

// generatePlanFilename creates a filename from the prompt.
// Example: "Create a hello world program" -> "plan-create-a-hello-world.md"
func (p *PlannerWrapper) generatePlanFilename() string {
	prompt := p.config.Prompt
	// Lowercase and replace non-alphanumeric with hyphens
	prompt = strings.ToLower(prompt)
	var result strings.Builder
	result.WriteString("plan-")
	wordCount := 0
	inWord := false
	for _, r := range prompt {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			result.WriteRune(r)
			inWord = true
		} else if inWord {
			result.WriteRune('-')
			inWord = false
			wordCount++
			if wordCount >= 5 { // Limit to ~5 words
				break
			}
		}
	}
	s := strings.TrimSuffix(result.String(), "-")
	return s + ".md"
}
