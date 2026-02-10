package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

func TestNextTaskID(t *testing.T) {
	// Reset counter for test
	atomic.StoreUint64(&taskCounter, 0)

	id1 := nextTaskID()
	id2 := nextTaskID()
	id3 := nextTaskID()

	if id1 != "task-001" {
		t.Errorf("expected task-001, got %s", id1)
	}

	if id2 != "task-002" {
		t.Errorf("expected task-002, got %s", id2)
	}

	if id3 != "task-003" {
		t.Errorf("expected task-003, got %s", id3)
	}
}

func TestNewLongRunningSession(t *testing.T) {
	config := AgentConfig{
		Role:       RoleOrchestrator,
		Model:      "sonnet",
		WorkDir:    "/tmp/test",
		SessionDir: "/tmp/sessions",
	}

	session := NewLongRunningSession(config, "test-session-123")

	expectedDir := "/tmp/sessions/test-session-123/orchestrator"
	if session.SessionDir() != expectedDir {
		t.Errorf("SessionDir() = %s, want %s", session.SessionDir(), expectedDir)
	}

	if session.TotalCost() != 0 {
		t.Errorf("TotalCost() = %v, want 0", session.TotalCost())
	}

	if session.TurnCount() != 0 {
		t.Errorf("TurnCount() = %v, want 0", session.TurnCount())
	}
}

func TestNewEphemeralSession(t *testing.T) {
	config := AgentConfig{
		Role:       RoleDesigner,
		Model:      "haiku",
		WorkDir:    "/tmp/test",
		SessionDir: "/tmp/sessions",
	}

	session := NewEphemeralSession(config, "test-session-456")

	expectedDir := "/tmp/sessions/test-session-456/designer"
	if session.BaseSessionDir() != expectedDir {
		t.Errorf("BaseSessionDir() = %s, want %s", session.BaseSessionDir(), expectedDir)
	}

	if session.TotalCost() != 0 {
		t.Errorf("TotalCost() = %v, want 0", session.TotalCost())
	}

	if session.TaskCount() != 0 {
		t.Errorf("TaskCount() = %v, want 0", session.TaskCount())
	}
}

func TestLongRunningSessionNotStarted(t *testing.T) {
	config := AgentConfig{
		Role:       RolePlanner,
		Model:      "sonnet",
		WorkDir:    ".",
		SessionDir: "/tmp/sessions",
	}

	session := NewLongRunningSession(config, "test")

	// Recording should be nil before start
	if session.Recording() != nil {
		t.Error("Recording() should be nil before start")
	}

	// Events should be nil before start
	if session.Events() != nil {
		t.Error("Events() should be nil before start")
	}
}

func TestExecuteResult(t *testing.T) {
	result := &ExecuteResult{
		FilesCreated:  []string{"/tmp/a.go", "/tmp/b.go"},
		FilesModified: []string{"/tmp/c.go"},
	}

	if len(result.FilesCreated) != 2 {
		t.Errorf("expected 2 files created, got %d", len(result.FilesCreated))
	}
	if len(result.FilesModified) != 1 {
		t.Errorf("expected 1 file modified, got %d", len(result.FilesModified))
	}
}

// mockSessionRunner is a mock session that simulates the real session behavior
// where Events() channel only closes when Stop() is called.
type mockSessionRunner struct {
	askErr      error
	events      chan claude.Event
	stoppedChan chan struct{}
	askResult   *claude.TurnResult
	stopped     bool
}

func newMockSessionRunner() *mockSessionRunner {
	return &mockSessionRunner{
		events:      make(chan claude.Event, 10),
		stoppedChan: make(chan struct{}),
		askResult: &claude.TurnResult{
			Success: true,
			Usage:   claude.TurnUsage{CostUSD: 0.01},
		},
	}
}

func (m *mockSessionRunner) Start(ctx context.Context) error {
	return nil
}

func (m *mockSessionRunner) Stop() error {
	if !m.stopped {
		m.stopped = true
		close(m.stoppedChan)
		close(m.events)
	}
	return nil
}

func (m *mockSessionRunner) Ask(ctx context.Context, prompt string) (*claude.TurnResult, error) {
	// Simulate sending some events before returning
	m.events <- claude.ToolCompleteEvent{
		Name:  "Write",
		Input: map[string]interface{}{"file_path": "/tmp/test.go"},
	}
	return m.askResult, m.askErr
}

func (m *mockSessionRunner) Events() <-chan claude.Event {
	return m.events
}

// TestRunSessionWithFileTracking_StopBeforeWait verifies that runSessionWithFileTracking
// stops the session before waiting for the event goroutine. This is a regression test
// for a deadlock where:
// 1. Event goroutine blocks on `for event := range session.Events()`
// 2. Events() channel only closes when session.Stop() is called
// 3. If we wait for the goroutine before calling Stop(), we deadlock
//
// This test uses the ACTUAL runSessionWithFileTracking function with a mock session,
// so any change to the ordering in production code will cause this test to fail.
func TestRunSessionWithFileTracking_StopBeforeWait(t *testing.T) {
	mock := newMockSessionRunner()
	ctx := context.Background()

	// Run in a goroutine with timeout to detect deadlock
	done := make(chan struct{})
	var result *claude.TurnResult
	var execResult *ExecuteResult
	var err error

	go func() {
		result, execResult, err = runSessionWithFileTracking(ctx, mock, "test prompt", nopLogger)
		close(done)
	}()

	// If the ordering is wrong (wait before stop), this will timeout
	select {
	case <-done:
		// Success - completed without deadlock
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if execResult == nil {
			t.Fatal("expected non-nil execResult")
		}
		// Verify file tracking worked
		if len(execResult.FilesCreated) != 1 || execResult.FilesCreated[0] != "/tmp/test.go" {
			t.Errorf("expected FilesCreated=[/tmp/test.go], got %v", execResult.FilesCreated)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock detected: runSessionWithFileTracking likely has wrong stop/wait ordering")
	}

	// Verify the session was stopped
	if !mock.stopped {
		t.Error("expected session to be stopped")
	}
}

// TestRunSessionWithFileTracking_TracksFiles verifies that file tracking works correctly.
func TestRunSessionWithFileTracking_TracksFiles(t *testing.T) {
	mock := newMockSessionRunner()
	ctx := context.Background()

	// Override Ask to send multiple file events
	originalAsk := mock.askResult
	mock.askResult = originalAsk
	oldAsk := mock.Ask
	_ = oldAsk // silence unused warning

	// Create a custom mock that sends multiple events
	customMock := &fileTrackingMock{
		mockSessionRunner: newMockSessionRunner(),
	}

	result, execResult, err := runSessionWithFileTracking(ctx, customMock, "test", nopLogger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Verify files were tracked
	if len(execResult.FilesCreated) != 2 {
		t.Errorf("expected 2 files created, got %d: %v", len(execResult.FilesCreated), execResult.FilesCreated)
	}
	if len(execResult.FilesModified) != 1 {
		t.Errorf("expected 1 file modified, got %d: %v", len(execResult.FilesModified), execResult.FilesModified)
	}
}

// fileTrackingMock sends multiple file events to test tracking.
type fileTrackingMock struct {
	*mockSessionRunner
}

func (m *fileTrackingMock) Ask(ctx context.Context, prompt string) (*claude.TurnResult, error) {
	// Send multiple file events
	m.events <- claude.ToolCompleteEvent{
		Name:  "Write",
		Input: map[string]interface{}{"file_path": "/tmp/new1.go"},
	}
	m.events <- claude.ToolCompleteEvent{
		Name:  "Write",
		Input: map[string]interface{}{"file_path": "/tmp/new2.go"},
	}
	m.events <- claude.ToolCompleteEvent{
		Name:  "Edit",
		Input: map[string]interface{}{"file_path": "/tmp/existing.go"},
	}
	// Duplicate should be ignored
	m.events <- claude.ToolCompleteEvent{
		Name:  "Write",
		Input: map[string]interface{}{"file_path": "/tmp/new1.go"},
	}
	return m.askResult, m.askErr
}

// TestRunSessionWithFileTracking_StopError verifies that Stop() errors are propagated.
func TestRunSessionWithFileTracking_StopError(t *testing.T) {
	mock := &stopErrorMock{
		mockSessionRunner: newMockSessionRunner(),
		stopErr:           errors.New("stop failed"),
	}
	ctx := context.Background()

	_, _, err := runSessionWithFileTracking(ctx, mock, "test", nopLogger)
	if err == nil {
		t.Fatal("expected error from Stop failure")
	}
	if !errors.Is(err, mock.stopErr) && err.Error() != "failed to stop session: stop failed" {
		t.Errorf("unexpected error: %v", err)
	}
}

// stopErrorMock simulates a session where Stop() returns an error but still closes events.
type stopErrorMock struct {
	*mockSessionRunner
	stopErr error
}

func (m *stopErrorMock) Stop() error {
	// Still close events (otherwise we'd timeout)
	if !m.stopped {
		m.stopped = true
		close(m.stoppedChan)
		close(m.events)
	}
	return m.stopErr
}

// TestRunSessionWithFileTracking_StopFailsToCloseEvents verifies bounded wait behavior
// when Stop() doesn't close the events channel.
func TestRunSessionWithFileTracking_StopFailsToCloseEvents(t *testing.T) {
	// Skip in short mode since this test waits for the timeout
	if testing.Short() {
		t.Skip("skipping timeout test in short mode")
	}

	mock := &brokenStopMock{
		mockSessionRunner: newMockSessionRunner(),
	}
	ctx := context.Background()

	start := time.Now()
	result, execResult, err := runSessionWithFileTracking(ctx, mock, "test", nopLogger)
	elapsed := time.Since(start)

	// Should complete (with timeout) rather than deadlock forever
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if execResult == nil {
		t.Fatal("expected non-nil execResult")
	}

	// Should have waited approximately the timeout duration
	if elapsed < eventGoroutineTimeout-100*time.Millisecond {
		t.Errorf("expected to wait ~%v, but only waited %v", eventGoroutineTimeout, elapsed)
	}
	if elapsed > eventGoroutineTimeout+1*time.Second {
		t.Errorf("waited too long: %v", elapsed)
	}
}

// brokenStopMock simulates a session where Stop() succeeds but fails to close events.
type brokenStopMock struct {
	*mockSessionRunner
}

func (m *brokenStopMock) Stop() error {
	// Intentionally don't close events - simulates a broken session
	m.stopped = true
	close(m.stoppedChan)
	// NOT closing m.events - this is the bug we're protecting against
	return nil
}
