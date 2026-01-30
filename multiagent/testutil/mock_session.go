// Package testutil provides mock implementations for testing the multi-agent system.
package testutil

import (
	"context"
	"fmt"
	"sync"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// MockResponse represents a pre-configured response for mock sessions.
type MockResponse struct {
	Error   error
	Text    string
	Cost    float64
	Success bool
}

// MockLongRunningSession simulates a LongRunningSession for testing.
// It returns pre-configured responses without making actual API calls.
type MockLongRunningSession struct {
	sessionDir    string
	config        MockSessionConfig
	responses     []MockResponse
	messages      []string
	responseIndex int
	totalCost     float64
	turnCount     int
	mu            sync.Mutex
	started       bool
	stopped       bool
}

// MockSessionConfig configures mock session behavior.
type MockSessionConfig struct {
	SessionDir string
	Responses  []MockResponse
}

// NewMockLongRunningSession creates a new mock long-running session.
func NewMockLongRunningSession(config MockSessionConfig) *MockLongRunningSession {
	return &MockLongRunningSession{
		config:     config,
		sessionDir: config.SessionDir,
		responses:  config.Responses,
	}
}

// Start simulates starting the session.
func (m *MockLongRunningSession) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return fmt.Errorf("session already started")
	}
	m.started = true
	return nil
}

// Stop simulates stopping the session.
func (m *MockLongRunningSession) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return nil
	}
	m.stopped = true
	m.started = false
	return nil
}

// SendMessage simulates sending a message and returns the next pre-configured response.
func (m *MockLongRunningSession) SendMessage(ctx context.Context, message string) (*claude.TurnResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return nil, fmt.Errorf("session not started")
	}

	// Record the message
	m.messages = append(m.messages, message)

	// Get the next response
	if m.responseIndex >= len(m.responses) {
		return nil, fmt.Errorf("no more mock responses configured")
	}

	resp := m.responses[m.responseIndex]
	m.responseIndex++

	if resp.Error != nil {
		return nil, resp.Error
	}

	// Update metrics
	m.totalCost += resp.Cost
	m.turnCount++

	return &claude.TurnResult{
		TurnNumber: m.turnCount,
		Success:    resp.Success,
		Usage: claude.TurnUsage{
			CostUSD: resp.Cost,
		},
	}, nil
}

// SessionDir returns the session directory.
func (m *MockLongRunningSession) SessionDir() string {
	return m.sessionDir
}

// TotalCost returns the accumulated cost.
func (m *MockLongRunningSession) TotalCost() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totalCost
}

// TurnCount returns the number of turns.
func (m *MockLongRunningSession) TurnCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turnCount
}

// Messages returns all messages sent to this session (for test verification).
func (m *MockLongRunningSession) Messages() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]string, len(m.messages))
	copy(result, m.messages)
	return result
}

// IsStarted returns whether the session was started.
func (m *MockLongRunningSession) IsStarted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.started
}

// IsStopped returns whether the session was stopped.
func (m *MockLongRunningSession) IsStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopped
}

// MockEphemeralSession simulates an EphemeralSession for testing.
type MockEphemeralSession struct {
	baseSessionDir string
	config         MockSessionConfig
	responses      []MockResponse
	prompts        []string
	responseIndex  int
	totalCost      float64
	taskCount      int
	mu             sync.Mutex
}

// NewMockEphemeralSession creates a new mock ephemeral session.
func NewMockEphemeralSession(config MockSessionConfig) *MockEphemeralSession {
	return &MockEphemeralSession{
		config:         config,
		baseSessionDir: config.SessionDir,
		responses:      config.Responses,
	}
}

// Execute simulates executing a task and returns the next pre-configured response.
func (m *MockEphemeralSession) Execute(ctx context.Context, prompt string) (*claude.TurnResult, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Record the prompt
	m.prompts = append(m.prompts, prompt)

	// Generate task ID
	m.taskCount++
	taskID := fmt.Sprintf("task-%03d", m.taskCount)

	// Get the next response
	if m.responseIndex >= len(m.responses) {
		return nil, taskID, fmt.Errorf("no more mock responses configured")
	}

	resp := m.responses[m.responseIndex]
	m.responseIndex++

	if resp.Error != nil {
		return nil, taskID, resp.Error
	}

	// Update metrics
	m.totalCost += resp.Cost

	return &claude.TurnResult{
		TurnNumber: 1, // Ephemeral sessions always have turn 1
		Success:    resp.Success,
		Usage: claude.TurnUsage{
			CostUSD: resp.Cost,
		},
	}, taskID, nil
}

// BaseSessionDir returns the base session directory.
func (m *MockEphemeralSession) BaseSessionDir() string {
	return m.baseSessionDir
}

// TotalCost returns the accumulated cost.
func (m *MockEphemeralSession) TotalCost() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totalCost
}

// TaskCount returns the number of tasks executed.
func (m *MockEphemeralSession) TaskCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.taskCount
}

// Prompts returns all prompts executed (for test verification).
func (m *MockEphemeralSession) Prompts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]string, len(m.prompts))
	copy(result, m.prompts)
	return result
}

// AddCost adds cost to the session (used for testing cost aggregation).
func (m *MockEphemeralSession) AddCost(cost float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.totalCost += cost
}
