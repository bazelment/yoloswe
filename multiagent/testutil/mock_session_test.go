package testutil

import (
	"context"
	"errors"
	"testing"
)

func TestMockLongRunningSession_Lifecycle(t *testing.T) {
	config := MockSessionConfig{
		SessionDir: "/tmp/test-session",
		Responses: []MockResponse{
			{Text: "Hello", Cost: 0.01, Success: true},
		},
	}

	session := NewMockLongRunningSession(config)

	// Not started initially
	if session.IsStarted() {
		t.Error("expected session not to be started initially")
	}

	// Start should succeed
	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	if !session.IsStarted() {
		t.Error("expected session to be started")
	}

	// Double start should fail
	if err := session.Start(context.Background()); err == nil {
		t.Error("expected error on double start")
	}

	// Stop should succeed
	if err := session.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	if session.IsStarted() {
		t.Error("expected session to be stopped")
	}

	if !session.IsStopped() {
		t.Error("expected IsStopped() to return true")
	}
}

func TestMockLongRunningSession_SendMessage(t *testing.T) {
	config := MockSessionConfig{
		SessionDir: "/tmp/test",
		Responses: []MockResponse{
			{Text: "Response 1", Cost: 0.01, Success: true},
			{Text: "Response 2", Cost: 0.02, Success: true},
		},
	}

	session := NewMockLongRunningSession(config)

	// SendMessage before start should fail
	_, err := session.SendMessage(context.Background(), "test")
	if err == nil {
		t.Error("expected error when sending before start")
	}

	// Start the session
	session.Start(context.Background())

	// First message
	result, err := session.SendMessage(context.Background(), "message 1")
	if err != nil {
		t.Fatalf("SendMessage(1) error: %v", err)
	}
	if result.Usage.CostUSD != 0.01 {
		t.Errorf("expected cost 0.01, got %v", result.Usage.CostUSD)
	}
	if session.TurnCount() != 1 {
		t.Errorf("expected turn count 1, got %d", session.TurnCount())
	}

	// Second message
	result, err = session.SendMessage(context.Background(), "message 2")
	if err != nil {
		t.Fatalf("SendMessage(2) error: %v", err)
	}
	if result.Usage.CostUSD != 0.02 {
		t.Errorf("expected cost 0.02, got %v", result.Usage.CostUSD)
	}
	if session.TurnCount() != 2 {
		t.Errorf("expected turn count 2, got %d", session.TurnCount())
	}

	// Total cost
	if session.TotalCost() != 0.03 {
		t.Errorf("expected total cost 0.03, got %v", session.TotalCost())
	}

	// Verify messages recorded
	messages := session.Messages()
	if len(messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(messages))
	}
	if messages[0] != "message 1" {
		t.Errorf("expected first message 'message 1', got %q", messages[0])
	}
}

func TestMockLongRunningSession_ErrorResponse(t *testing.T) {
	expectedErr := errors.New("simulated error")
	config := MockSessionConfig{
		SessionDir: "/tmp/test",
		Responses: []MockResponse{
			{Error: expectedErr},
		},
	}

	session := NewMockLongRunningSession(config)
	session.Start(context.Background())

	_, err := session.SendMessage(context.Background(), "test")
	if err != expectedErr {
		t.Errorf("expected error %v, got %v", expectedErr, err)
	}
}

func TestMockEphemeralSession_Execute(t *testing.T) {
	config := MockSessionConfig{
		SessionDir: "/tmp/test-ephemeral",
		Responses: []MockResponse{
			{Text: "Design response", Cost: 0.01, Success: true},
			{Text: "Build response", Cost: 0.02, Success: true},
		},
	}

	session := NewMockEphemeralSession(config)

	// First execute
	result, taskID, err := session.Execute(context.Background(), "design this")
	if err != nil {
		t.Fatalf("Execute(1) error: %v", err)
	}
	if taskID != "task-001" {
		t.Errorf("expected task ID task-001, got %s", taskID)
	}
	if result.Usage.CostUSD != 0.01 {
		t.Errorf("expected cost 0.01, got %v", result.Usage.CostUSD)
	}

	// Second execute
	result, taskID, err = session.Execute(context.Background(), "build this")
	if err != nil {
		t.Fatalf("Execute(2) error: %v", err)
	}
	if taskID != "task-002" {
		t.Errorf("expected task ID task-002, got %s", taskID)
	}

	// Verify metrics
	if session.TaskCount() != 2 {
		t.Errorf("expected task count 2, got %d", session.TaskCount())
	}
	if session.TotalCost() != 0.03 {
		t.Errorf("expected total cost 0.03, got %v", session.TotalCost())
	}

	// Verify prompts recorded
	prompts := session.Prompts()
	if len(prompts) != 2 {
		t.Errorf("expected 2 prompts, got %d", len(prompts))
	}
}

func TestMockEphemeralSession_ErrorResponse(t *testing.T) {
	expectedErr := errors.New("build failed")
	config := MockSessionConfig{
		SessionDir: "/tmp/test",
		Responses: []MockResponse{
			{Error: expectedErr},
		},
	}

	session := NewMockEphemeralSession(config)

	_, taskID, err := session.Execute(context.Background(), "test")
	if err != expectedErr {
		t.Errorf("expected error %v, got %v", expectedErr, err)
	}
	if taskID != "task-001" {
		t.Errorf("expected task ID even on error, got %s", taskID)
	}
}
