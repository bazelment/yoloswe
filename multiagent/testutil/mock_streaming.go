package testutil

import (
	"context"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/multiagent/subagent"
)

// MockStreamingSubAgent is a mock implementation of StreamingSubAgent for testing.
type MockStreamingSubAgent struct {
	err          error
	events       chan interface{}
	result       *subagent.Result
	prompt       string
	eventsToEmit []interface{}
	executeDelay time.Duration
	mu           sync.Mutex
	cancelled    bool
	executed     bool
}

// MockStreamingConfig configures the mock streaming sub-agent.
type MockStreamingConfig struct {
	Result       *subagent.Result
	Error        error
	Events       []interface{}
	ExecuteDelay time.Duration
}

// NewMockStreamingSubAgent creates a new mock streaming sub-agent.
func NewMockStreamingSubAgent(config MockStreamingConfig) *MockStreamingSubAgent {
	return &MockStreamingSubAgent{
		events:       make(chan interface{}, 100),
		result:       config.Result,
		err:          config.Error,
		eventsToEmit: config.Events,
		executeDelay: config.ExecuteDelay,
	}
}

// Events returns the event channel.
func (m *MockStreamingSubAgent) Events() <-chan interface{} {
	return m.events
}

// Cancel marks the sub-agent as cancelled.
func (m *MockStreamingSubAgent) Cancel(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancelled = true

	// Emit cancellation error
	errEvent := subagent.NewError("mock-req", subagent.AgentTypeDesigner, "cancelled", reason, false)
	select {
	case m.events <- errEvent:
	default:
	}
}

// IsCancelled returns whether the sub-agent has been cancelled.
func (m *MockStreamingSubAgent) IsCancelled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelled
}

// Execute simulates execution with pre-configured result and events.
func (m *MockStreamingSubAgent) Execute(ctx context.Context, prompt string) (*subagent.Result, error) {
	m.mu.Lock()
	m.executed = true
	m.prompt = prompt
	m.mu.Unlock()

	// Simulate execution delay
	if m.executeDelay > 0 {
		select {
		case <-time.After(m.executeDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Emit configured events
	for _, event := range m.eventsToEmit {
		select {
		case m.events <- event:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Close events channel
	close(m.events)

	if m.err != nil {
		return nil, m.err
	}

	return m.result, nil
}

// WasExecuted returns whether Execute was called.
func (m *MockStreamingSubAgent) WasExecuted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.executed
}

// LastPrompt returns the last prompt passed to Execute.
func (m *MockStreamingSubAgent) LastPrompt() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prompt
}

// MockClaudeSession is a mock Claude session for testing event transformation.
type MockClaudeSession struct {
	events       chan claude.Event
	eventsToEmit []claude.Event
	started      bool
	stopped      bool
}

// MockClaudeSessionConfig configures the mock Claude session.
type MockClaudeSessionConfig struct {
	Events []claude.Event
}

// NewMockClaudeSession creates a new mock Claude session.
func NewMockClaudeSession(config MockClaudeSessionConfig) *MockClaudeSession {
	return &MockClaudeSession{
		events:       make(chan claude.Event, 100),
		eventsToEmit: config.Events,
	}
}

// Start simulates starting the session.
func (m *MockClaudeSession) Start(ctx context.Context) error {
	m.started = true
	return nil
}

// Stop simulates stopping the session.
func (m *MockClaudeSession) Stop() error {
	m.stopped = true
	close(m.events)
	return nil
}

// Events returns the events channel.
func (m *MockClaudeSession) Events() <-chan claude.Event {
	return m.events
}

// EmitEvents sends the pre-configured events to the channel.
func (m *MockClaudeSession) EmitEvents() {
	for _, event := range m.eventsToEmit {
		m.events <- event
	}
}

// IsStarted returns whether the session was started.
func (m *MockClaudeSession) IsStarted() bool {
	return m.started
}

// IsStopped returns whether the session was stopped.
func (m *MockClaudeSession) IsStopped() bool {
	return m.stopped
}
