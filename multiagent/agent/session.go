package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// fmtCost formats a USD cost for log output (e.g. "$0.003").
func fmtCost(usd float64) string {
	return fmt.Sprintf("$%.4f", math.Round(usd*1e4)/1e4)
}

// nopHandler is a slog.Handler that discards all output.
type nopHandler struct{}

func (nopHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (nopHandler) Handle(context.Context, slog.Record) error { return nil }
func (h nopHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h nopHandler) WithGroup(string) slog.Handler           { return h }

// nopLogger is a shared no-op logger instance.
var nopLogger = slog.New(nopHandler{})

// taskCounter is used to generate unique task IDs for ephemeral sessions.
var taskCounter uint64

// nextTaskID returns a unique task ID.
func nextTaskID() string {
	id := atomic.AddUint64(&taskCounter, 1)
	return fmt.Sprintf("task-%03d", id)
}

// LongRunningSession wraps a claude.Session for long-running agents (Orchestrator, Planner).
// It maintains a persistent session across multiple turns.
type LongRunningSession struct {
	asyncSendTime time.Time
	session       *claude.Session
	sessionDir    string
	extraOptions  []claude.SessionOption
	config        AgentConfig
	totalCost     float64
	turnCount     int
	mu            sync.Mutex
	started       bool
}

// NewLongRunningSession creates a new long-running session.
func NewLongRunningSession(config AgentConfig, swarmSessionID string) *LongRunningSession {
	sessionDir := filepath.Join(config.SessionDir, swarmSessionID, config.Role.String())
	return &LongRunningSession{
		config:     config,
		sessionDir: sessionDir,
	}
}

func (s *LongRunningSession) logger() *slog.Logger {
	if s.config.Logger != nil {
		return s.config.Logger.With("role", s.config.Role)
	}
	return nopLogger
}

// SetSessionOptions sets additional Claude session options.
// Must be called before Start().
func (s *LongRunningSession) SetSessionOptions(opts ...claude.SessionOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extraOptions = append(s.extraOptions, opts...)
}

// Start marks the session as ready to be used.
// The actual Claude session is started lazily on first message to avoid
// creating empty session directories for agents that don't send messages.
func (s *LongRunningSession) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return fmt.Errorf("session already started")
	}

	s.started = true
	return nil
}

// ensureSession starts the underlying Claude session if not already started.
// This implements lazy initialization to avoid creating empty directories.
func (s *LongRunningSession) ensureSession(ctx context.Context) error {
	if s.session != nil {
		return nil
	}

	s.logger().Info("session starting",
		"model", s.config.Model,
		"workdir", s.config.WorkDir,
	)

	// Ensure session directory exists
	if err := os.MkdirAll(s.sessionDir, 0755); err != nil {
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Build session options
	opts := []claude.SessionOption{
		claude.WithModel(s.config.Model),
		claude.WithWorkDir(s.config.WorkDir),
		claude.WithRecording(s.sessionDir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
	}

	if len(s.config.AllowedTools) > 0 {
		opts = append(opts, claude.WithAllowedTools(s.config.AllowedTools...))
	}

	// Add any extra options (e.g., MCP config)
	opts = append(opts, s.extraOptions...)

	// Create the Claude session with recording enabled
	s.session = claude.NewSession(opts...)

	if err := s.session.Start(ctx); err != nil {
		return fmt.Errorf("failed to start session: %w", err)
	}

	return nil
}

// Stop gracefully shuts down the session.
func (s *LongRunningSession) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started || s.session == nil {
		return nil
	}

	s.logger().Info("session stopped",
		"totalCost", fmtCost(s.totalCost),
		"turns", s.turnCount,
	)
	s.started = false
	return s.session.Stop()
}

// SendMessage sends a message and waits for the turn to complete.
// This is the primary way to interact with long-running agents.
func (s *LongRunningSession) SendMessage(ctx context.Context, message string) (*AgentResult, error) {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil, fmt.Errorf("session not started")
	}

	// Ensure the Claude session is started (lazy initialization)
	if err := s.ensureSession(ctx); err != nil {
		s.mu.Unlock()
		return nil, err
	}

	session := s.session
	s.mu.Unlock()

	log := s.logger()
	log.Info("sending message",
		"prompt", message,
	)

	// Send the message
	start := time.Now()
	result, err := session.Ask(ctx, message)
	if err != nil {
		log.Info("message failed", "error", err)
		return nil, err
	}

	// Update metrics
	s.mu.Lock()
	s.totalCost += result.Usage.CostUSD
	s.turnCount++
	s.mu.Unlock()

	log.Info("turn complete",
		"cost", fmtCost(result.Usage.CostUSD),
		"duration", time.Since(start).Round(time.Millisecond),
	)

	return ClaudeResultToAgentResult(result), nil
}

// Events returns the event channel for streaming responses.
func (s *LongRunningSession) Events() <-chan claude.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return nil
	}
	return s.session.Events()
}

// LoggingEvents returns a channel that forwards events from the session
// while logging tool start/complete and turn complete events.
// This is useful for streaming callers that want visibility without
// consuming events themselves for logging.
// The returned channel mirrors the underlying session's events channel
// (stays open for long-running sessions, closes when session stops).
//
// NOTE: LoggingEvents calls RecordUsage on TurnCompleteEvent. Callers
// must NOT also call WaitForTurn (which also records usage) for the
// same turn, or cost/turn accounting will be double-counted.
func (s *LongRunningSession) LoggingEvents() <-chan claude.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return nil
	}

	log := s.logger()
	src := s.session.Events()
	out := make(chan claude.Event, cap(src))

	go func() {
		defer close(out)
		for event := range src {
			switch e := event.(type) {
			case claude.ToolCompleteEvent:
				filePath, _ := e.Input["file_path"].(string)
				if filePath != "" {
					log.Info("tool", "name", e.Name, "file", filepath.Base(filePath))
				} else {
					log.Info("tool", "name", e.Name)
				}
			case claude.TurnCompleteEvent:
				// Record usage so TotalCost() is accurate in streaming mode.
				s.RecordUsage(e.Usage)

				s.mu.Lock()
				sendTime := s.asyncSendTime
				s.mu.Unlock()
				if !sendTime.IsZero() {
					log.Info("turn complete",
						"cost", fmtCost(e.Usage.CostUSD),
						"duration", time.Since(sendTime).Round(time.Millisecond),
					)
				} else {
					log.Info("turn complete",
						"cost", fmtCost(e.Usage.CostUSD),
					)
				}
			}
			out <- event
		}
	}()

	return out
}

// SessionDir returns the session recording directory.
func (s *LongRunningSession) SessionDir() string {
	return s.sessionDir
}

// TotalCost returns the accumulated cost.
func (s *LongRunningSession) TotalCost() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalCost
}

// TurnCount returns the number of turns completed.
func (s *LongRunningSession) TurnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnCount
}

// Recording returns the session recording if available.
func (s *LongRunningSession) Recording() *claude.SessionRecording {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return nil
	}
	return s.session.Recording()
}

// SendMessageAsync sends a message without waiting for completion.
// Use Events() to receive streaming updates and WaitForTurn() to get the result.
// Returns the turn number that was started.
func (s *LongRunningSession) SendMessageAsync(ctx context.Context, message string) (int, error) {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return 0, fmt.Errorf("session not started")
	}

	// Ensure the Claude session is started (lazy initialization)
	if err := s.ensureSession(ctx); err != nil {
		s.mu.Unlock()
		return 0, err
	}

	session := s.session
	s.mu.Unlock()

	s.logger().Info("sending message",
		"prompt", message,
	)

	s.mu.Lock()
	s.asyncSendTime = time.Now()
	s.mu.Unlock()

	return session.SendMessage(ctx, message)
}

// WaitForTurn blocks until the current turn completes.
// If no turn is in progress, it returns immediately with nil.
func (s *LongRunningSession) WaitForTurn(ctx context.Context) (*AgentResult, error) {
	s.mu.Lock()
	session := s.session
	sendTime := s.asyncSendTime
	s.mu.Unlock()

	if session == nil {
		return nil, fmt.Errorf("session not started")
	}

	result, err := session.WaitForTurn(ctx)
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, nil
	}

	// Update metrics
	s.mu.Lock()
	s.totalCost += result.Usage.CostUSD
	s.turnCount++
	s.mu.Unlock()

	log := s.logger()
	if !sendTime.IsZero() {
		log.Info("turn complete",
			"cost", fmtCost(result.Usage.CostUSD),
			"duration", time.Since(sendTime).Round(time.Millisecond),
		)
	} else {
		log.Info("turn complete",
			"cost", fmtCost(result.Usage.CostUSD),
		)
	}

	return ClaudeResultToAgentResult(result), nil
}

// SendToolResult sends a tool result for a specific tool use.
// This is used when the SDK handles a tool locally (like AskUserQuestion).
func (s *LongRunningSession) SendToolResult(ctx context.Context, toolUseID, content string) (int, error) {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return 0, fmt.Errorf("session not started")
	}

	if s.session == nil {
		s.mu.Unlock()
		return 0, fmt.Errorf("session not initialized")
	}

	session := s.session
	s.mu.Unlock()

	return session.SendToolResult(ctx, toolUseID, content)
}

// CurrentTurnNumber returns the current turn number.
func (s *LongRunningSession) CurrentTurnNumber() int {
	s.mu.Lock()
	session := s.session
	s.mu.Unlock()

	if session == nil {
		return 0
	}

	return session.CurrentTurnNumber()
}

// RecordUsage records turn usage for cost and turn count tracking.
// This should be called when processing TurnCompleteEvent in streaming mode.
func (s *LongRunningSession) RecordUsage(usage claude.TurnUsage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalCost += usage.CostUSD
	s.turnCount++
}

// EphemeralSession creates fresh sessions for each task (Designer, Builder, Reviewer).
// Each Execute() call creates a new session, runs one interaction, and stops the session.
// When ProviderName is set to a non-Claude provider, it uses NewProviderForModel
// instead of creating a claude.Session directly.
type EphemeralSession struct {
	swarmSessionID string
	baseSessionDir string
	ProviderName   string // "claude", "gemini", "codex"; empty defaults to "claude"
	config         AgentConfig
	totalCost      float64
	taskCount      int
	mu             sync.Mutex
}

// NewEphemeralSession creates a new ephemeral session factory.
func NewEphemeralSession(config AgentConfig, swarmSessionID string) *EphemeralSession {
	baseSessionDir := filepath.Join(config.SessionDir, swarmSessionID, config.Role.String())
	return &EphemeralSession{
		config:         config,
		swarmSessionID: swarmSessionID,
		baseSessionDir: baseSessionDir,
	}
}

// ExecuteResult contains the result of an ephemeral session execution.
type ExecuteResult struct {
	FilesCreated  []string
	FilesModified []string
}

// sessionRunner abstracts session operations for testing.
// This allows injecting mock sessions to test the stop-before-wait ordering.
type sessionRunner interface {
	Start(ctx context.Context) error
	Stop() error
	Ask(ctx context.Context, prompt string) (*claude.TurnResult, error)
	Events() <-chan claude.Event
}

// claudeSessionRunner wraps a real claude.Session.
type claudeSessionRunner struct {
	session *claude.Session
}

func (r *claudeSessionRunner) Start(ctx context.Context) error {
	return r.session.Start(ctx)
}

func (r *claudeSessionRunner) Stop() error {
	return r.session.Stop()
}

func (r *claudeSessionRunner) Ask(ctx context.Context, prompt string) (*claude.TurnResult, error) {
	return r.session.Ask(ctx, prompt)
}

func (r *claudeSessionRunner) Events() <-chan claude.Event {
	return r.session.Events()
}

func (e *EphemeralSession) logger() *slog.Logger {
	if e.config.Logger != nil {
		return e.config.Logger.With("role", e.config.Role)
	}
	return nopLogger
}

// Execute creates a fresh session, runs the prompt, and returns the result.
// Each call is independent - no conversation history is preserved.
func (e *EphemeralSession) Execute(ctx context.Context, prompt string) (*AgentResult, string, error) {
	result, _, taskID, err := e.ExecuteWithFiles(ctx, prompt)
	return result, taskID, err
}

// ExecuteWithFiles creates a fresh session, runs the prompt, and returns the result with file tracking.
// For Claude, it tracks Write and Edit tool calls via events.
// For non-Claude providers, it detects file changes via git diff.
func (e *EphemeralSession) ExecuteWithFiles(ctx context.Context, prompt string) (*AgentResult, *ExecuteResult, string, error) {
	providerName := e.ProviderName
	if providerName == "" {
		providerName = ProviderClaude
	}

	if providerName != ProviderClaude {
		return e.executeWithProvider(ctx, prompt, providerName)
	}

	return e.executeWithClaude(ctx, prompt)
}

// executeWithClaude is the original Claude-specific execution path.
func (e *EphemeralSession) executeWithClaude(ctx context.Context, prompt string) (*AgentResult, *ExecuteResult, string, error) {
	// Generate unique task directory
	taskID := nextTaskID()
	taskDir := filepath.Join(e.baseSessionDir, taskID)

	log := e.logger()
	log.Info("session starting",
		"taskID", taskID,
		"model", e.config.Model,
	)

	// Ensure task directory exists
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		return nil, nil, "", fmt.Errorf("failed to create task directory: %w", err)
	}

	// Create a fresh session for this task
	opts := []claude.SessionOption{
		claude.WithModel(e.config.Model),
		claude.WithWorkDir(e.config.WorkDir),
		claude.WithRecording(taskDir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
	}
	if len(e.config.AllowedTools) > 0 {
		opts = append(opts, claude.WithAllowedTools(e.config.AllowedTools...))
	}
	session := claude.NewSession(opts...)

	runner := &claudeSessionRunner{session: session}
	start := time.Now()
	result, execResult, err := runSessionWithFileTracking(ctx, runner, prompt, log)
	if err != nil {
		log.Info("task failed",
			"taskID", taskID,
			"error", err,
			"duration", time.Since(start).Round(time.Millisecond),
		)
		return nil, nil, taskID, err
	}

	// Update metrics
	e.mu.Lock()
	e.totalCost += result.Usage.CostUSD
	e.taskCount++
	e.mu.Unlock()

	log.Info("task complete",
		"taskID", taskID,
		"cost", fmtCost(result.Usage.CostUSD),
		"filesCreated", len(execResult.FilesCreated),
		"filesModified", len(execResult.FilesModified),
		"duration", time.Since(start).Round(time.Millisecond),
	)

	return ClaudeResultToAgentResult(result), execResult, taskID, nil
}

// executeWithProvider runs the prompt using a non-Claude provider
// and detects file changes via git diff.
func (e *EphemeralSession) executeWithProvider(ctx context.Context, prompt, providerName string) (*AgentResult, *ExecuteResult, string, error) {
	taskID := nextTaskID()

	log := e.logger()
	log.Info("session starting",
		"taskID", taskID,
		"model", e.config.Model,
		"provider", providerName,
	)

	m, ok := ModelByID(e.config.Model)
	if !ok {
		return nil, nil, "", fmt.Errorf("unknown model %q", e.config.Model)
	}

	provider, err := NewProviderForModel(m)
	if err != nil {
		return nil, nil, "", fmt.Errorf("create provider: %w", err)
	}
	defer provider.Close()

	log.Info("sending message",
		"prompt", prompt,
	)

	start := time.Now()
	result, err := provider.Execute(ctx, prompt, nil,
		WithProviderModel(e.config.Model),
		WithProviderWorkDir(e.config.WorkDir),
		WithProviderPermissionMode("bypass"),
	)
	if err != nil {
		log.Info("task failed",
			"taskID", taskID,
			"error", err,
			"duration", time.Since(start).Round(time.Millisecond),
		)
		return nil, nil, taskID, err
	}

	// Detect file changes via git diff since non-Claude providers
	// don't emit tool events.
	execResult := detectFileChangesGit(e.config.WorkDir, log)

	// Update metrics
	e.mu.Lock()
	e.totalCost += result.Usage.CostUSD
	e.taskCount++
	e.mu.Unlock()

	log.Info("task complete",
		"taskID", taskID,
		"provider", providerName,
		"cost", fmtCost(result.Usage.CostUSD),
		"filesCreated", len(execResult.FilesCreated),
		"filesModified", len(execResult.FilesModified),
		"duration", time.Since(start).Round(time.Millisecond),
	)

	return result, execResult, taskID, nil
}

// detectFileChangesGit runs `git diff --name-status HEAD` in the given directory
// to detect files created or modified by the agent.
func detectFileChangesGit(workDir string, log *slog.Logger) *ExecuteResult {
	result := &ExecuteResult{}

	cmd := exec.Command("git", "diff", "--name-status", "HEAD")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		log.Warn("git diff failed for file tracking", "error", err)
		return result
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		status, file := parts[0], parts[1]
		switch {
		case status == "A":
			result.FilesCreated = append(result.FilesCreated, file)
		case strings.HasPrefix(status, "M"):
			result.FilesModified = append(result.FilesModified, file)
		}
	}

	return result
}

// eventGoroutineTimeout is the maximum time to wait for the event processing
// goroutine to finish after stopping the session. This provides a safety bound
// in case Stop() fails to close the events channel.
const eventGoroutineTimeout = 5 * time.Second

// runSessionWithFileTracking executes a prompt on a session while tracking file operations.
// This is extracted to allow testing the stop-before-wait ordering with mock sessions.
//
// CRITICAL: The ordering here is important to avoid deadlock:
// 1. Start session and spawn event goroutine
// 2. Execute Ask()
// 3. Stop session (closes events channel)
// 4. Wait for event goroutine to finish (with timeout)
//
// If step 3 and 4 are reversed, the goroutine blocks forever on the events channel.
func runSessionWithFileTracking(ctx context.Context, session sessionRunner, prompt string, log *slog.Logger) (*claude.TurnResult, *ExecuteResult, error) {
	// Start the session
	if err := session.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("failed to start session: %w", err)
	}

	// Track files from tool events in background
	var filesMu sync.Mutex
	filesCreated := make([]string, 0)
	filesModified := make([]string, 0)
	fileSeen := make(map[string]bool)
	eventsDone := make(chan struct{})

	go func() {
		defer close(eventsDone)
		events := session.Events()
		if events == nil {
			return
		}
		for event := range events {
			if e, ok := event.(claude.ToolCompleteEvent); ok {
				filePath, _ := e.Input["file_path"].(string)
				if filePath != "" {
					log.Info("tool", "name", e.Name, "file", filepath.Base(filePath))
					filesMu.Lock()
					if !fileSeen[filePath] {
						fileSeen[filePath] = true
						switch e.Name {
						case "Write":
							filesCreated = append(filesCreated, filePath)
						case "Edit":
							filesModified = append(filesModified, filePath)
						}
					}
					filesMu.Unlock()
				}
			}
		}
	}()

	// Log the full prompt for comprehensive test debugging.
	log.Info("sending message",
		"prompt", prompt,
	)

	// Execute the single turn
	result, err := session.Ask(ctx, prompt)

	// CRITICAL: Stop the session first - this closes the events channel
	// If we wait before stopping, we deadlock because the goroutine
	// is blocked on `for event := range events` which never closes.
	stopErr := session.Stop()

	// Wait for event processing goroutine to finish with a timeout.
	// The timeout protects against edge cases where Stop() fails to close
	// the events channel (which would cause an unbounded wait).
	select {
	case <-eventsDone:
		// Goroutine finished normally
	case <-time.After(eventGoroutineTimeout):
		// Timeout - goroutine may be stuck, but we can't wait forever.
		// The file lists may be incomplete, but we avoid deadlock.
	}

	// Propagate stop error if Ask succeeded but Stop failed
	if err == nil && stopErr != nil {
		return nil, nil, fmt.Errorf("failed to stop session: %w", stopErr)
	}
	if err != nil {
		return nil, nil, err
	}

	// Safe to access file lists now - goroutine has finished (or timed out)
	filesMu.Lock()
	execResult := &ExecuteResult{
		FilesCreated:  append([]string(nil), filesCreated...),
		FilesModified: append([]string(nil), filesModified...),
	}
	filesMu.Unlock()

	return result, execResult, nil
}

// BaseSessionDir returns the base directory for task recordings.
func (e *EphemeralSession) BaseSessionDir() string {
	return e.baseSessionDir
}

// TotalCost returns the accumulated cost across all tasks.
func (e *EphemeralSession) TotalCost() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.totalCost
}

// TaskCount returns the number of tasks executed.
func (e *EphemeralSession) TaskCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.taskCount
}
