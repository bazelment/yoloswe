package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

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
	session      *claude.Session
	sessionDir   string
	extraOptions []claude.SessionOption
	config       AgentConfig
	totalCost    float64
	turnCount    int
	mu           sync.Mutex
	started      bool
}

// NewLongRunningSession creates a new long-running session.
func NewLongRunningSession(config AgentConfig, swarmSessionID string) *LongRunningSession {
	sessionDir := filepath.Join(config.SessionDir, swarmSessionID, config.Role.String())
	return &LongRunningSession{
		config:     config,
		sessionDir: sessionDir,
	}
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

	s.started = false
	return s.session.Stop()
}

// SendMessage sends a message and waits for the turn to complete.
// This is the primary way to interact with long-running agents.
func (s *LongRunningSession) SendMessage(ctx context.Context, message string) (*claude.TurnResult, error) {
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

	// Send the message
	result, err := session.Ask(ctx, message)
	if err != nil {
		return nil, err
	}

	// Update metrics
	s.mu.Lock()
	s.totalCost += result.Usage.CostUSD
	s.turnCount++
	s.mu.Unlock()

	return result, nil
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

	return session.SendMessage(ctx, message)
}

// WaitForTurn blocks until the current turn completes.
// If no turn is in progress, it returns immediately with nil.
func (s *LongRunningSession) WaitForTurn(ctx context.Context) (*claude.TurnResult, error) {
	s.mu.Lock()
	session := s.session
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

	return result, nil
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

// EphemeralSession creates fresh Claude sessions for each task (Designer, Builder, Reviewer).
// Each Execute() call creates a new session, runs one interaction, and stops the session.
type EphemeralSession struct {
	swarmSessionID string
	baseSessionDir string
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
	*claude.TurnResult
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

// Execute creates a fresh session, runs the prompt, and returns the result.
// Each call is independent - no conversation history is preserved.
func (e *EphemeralSession) Execute(ctx context.Context, prompt string) (*claude.TurnResult, string, error) {
	result, _, taskID, err := e.ExecuteWithFiles(ctx, prompt)
	return result, taskID, err
}

// ExecuteWithFiles creates a fresh session, runs the prompt, and returns the result with file tracking.
// This method tracks Write and Edit tool calls to report which files were created/modified.
func (e *EphemeralSession) ExecuteWithFiles(ctx context.Context, prompt string) (*claude.TurnResult, *ExecuteResult, string, error) {
	// Generate unique task directory
	taskID := nextTaskID()
	taskDir := filepath.Join(e.baseSessionDir, taskID)

	// Ensure task directory exists
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		return nil, nil, "", fmt.Errorf("failed to create task directory: %w", err)
	}

	// Create a fresh session for this task
	session := claude.NewSession(
		claude.WithModel(e.config.Model),
		claude.WithWorkDir(e.config.WorkDir),
		claude.WithRecording(taskDir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
	)

	runner := &claudeSessionRunner{session: session}
	result, execResult, err := runSessionWithFileTracking(ctx, runner, prompt)
	if err != nil {
		return nil, nil, taskID, err
	}

	// Update metrics
	e.mu.Lock()
	e.totalCost += result.Usage.CostUSD
	e.taskCount++
	e.mu.Unlock()

	return result, execResult, taskID, nil
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
func runSessionWithFileTracking(ctx context.Context, session sessionRunner, prompt string) (*claude.TurnResult, *ExecuteResult, error) {
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
			if toolEvent, ok := event.(claude.ToolCompleteEvent); ok {
				filePath, _ := toolEvent.Input["file_path"].(string)
				if filePath != "" {
					filesMu.Lock()
					if !fileSeen[filePath] {
						fileSeen[filePath] = true
						switch toolEvent.Name {
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
		TurnResult:    result,
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
