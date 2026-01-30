package codex

import (
	"errors"
	"testing"
	"time"
)

func TestEventType_Values(t *testing.T) {
	// Verify event types have distinct values
	types := []EventType{
		EventTypeClientReady,
		EventTypeThreadStarted,
		EventTypeThreadReady,
		EventTypeTurnStarted,
		EventTypeTurnCompleted,
		EventTypeTextDelta,
		EventTypeItemStarted,
		EventTypeItemCompleted,
		EventTypeTokenUsage,
		EventTypeError,
		EventTypeStateChange,
	}

	seen := make(map[EventType]bool)
	for _, et := range types {
		if seen[et] {
			t.Errorf("duplicate event type: %d", et)
		}
		seen[et] = true
	}
}

func TestClientReadyEvent(t *testing.T) {
	e := ClientReadyEvent{UserAgent: "codex/1.0"}

	if e.Type() != EventTypeClientReady {
		t.Errorf("expected EventTypeClientReady, got %v", e.Type())
	}
	if e.UserAgent != "codex/1.0" {
		t.Errorf("unexpected UserAgent: %q", e.UserAgent)
	}
}

func TestThreadStartedEvent(t *testing.T) {
	e := ThreadStartedEvent{
		ThreadID:      "thread-123",
		Model:         "gpt-4o",
		ModelProvider: "openai",
		WorkDir:       "/home/user",
	}

	if e.Type() != EventTypeThreadStarted {
		t.Errorf("expected EventTypeThreadStarted, got %v", e.Type())
	}
	if e.ThreadID != "thread-123" {
		t.Errorf("unexpected ThreadID: %q", e.ThreadID)
	}
	if e.Model != "gpt-4o" {
		t.Errorf("unexpected Model: %q", e.Model)
	}
}

func TestThreadReadyEvent(t *testing.T) {
	e := ThreadReadyEvent{ThreadID: "thread-123"}

	if e.Type() != EventTypeThreadReady {
		t.Errorf("expected EventTypeThreadReady, got %v", e.Type())
	}
	if e.ThreadID != "thread-123" {
		t.Errorf("unexpected ThreadID: %q", e.ThreadID)
	}
}

func TestTurnStartedEvent(t *testing.T) {
	e := TurnStartedEvent{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
	}

	if e.Type() != EventTypeTurnStarted {
		t.Errorf("expected EventTypeTurnStarted, got %v", e.Type())
	}
	if e.ThreadID != "thread-123" {
		t.Errorf("unexpected ThreadID: %q", e.ThreadID)
	}
	if e.TurnID != "turn-456" {
		t.Errorf("unexpected TurnID: %q", e.TurnID)
	}
}

func TestTurnCompletedEvent(t *testing.T) {
	e := TurnCompletedEvent{
		ThreadID:   "thread-123",
		TurnID:     "turn-456",
		Success:    true,
		FullText:   "Response text",
		DurationMs: 1234,
		Usage: TurnUsage{
			InputTokens:  100,
			OutputTokens: 50,
		},
	}

	if e.Type() != EventTypeTurnCompleted {
		t.Errorf("expected EventTypeTurnCompleted, got %v", e.Type())
	}
	if !e.Success {
		t.Error("expected Success to be true")
	}
	if e.FullText != "Response text" {
		t.Errorf("unexpected FullText: %q", e.FullText)
	}
	if e.DurationMs != 1234 {
		t.Errorf("unexpected DurationMs: %d", e.DurationMs)
	}
}

func TestTurnCompletedEvent_WithError(t *testing.T) {
	testErr := errors.New("test error")
	e := TurnCompletedEvent{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
		Success:  false,
		Error:    testErr,
	}

	if e.Success {
		t.Error("expected Success to be false")
	}
	if e.Error != testErr {
		t.Errorf("unexpected Error: %v", e.Error)
	}
}

func TestTextDeltaEvent(t *testing.T) {
	e := TextDeltaEvent{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
		ItemID:   "item-789",
		Delta:    "Hello ",
		FullText: "Hello World",
	}

	if e.Type() != EventTypeTextDelta {
		t.Errorf("expected EventTypeTextDelta, got %v", e.Type())
	}
	if e.Delta != "Hello " {
		t.Errorf("unexpected Delta: %q", e.Delta)
	}
	if e.FullText != "Hello World" {
		t.Errorf("unexpected FullText: %q", e.FullText)
	}
}

func TestItemStartedEvent(t *testing.T) {
	e := ItemStartedEvent{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
		ItemID:   "item-789",
		ItemType: "message",
	}

	if e.Type() != EventTypeItemStarted {
		t.Errorf("expected EventTypeItemStarted, got %v", e.Type())
	}
	if e.ItemType != "message" {
		t.Errorf("unexpected ItemType: %q", e.ItemType)
	}
}

func TestItemCompletedEvent(t *testing.T) {
	e := ItemCompletedEvent{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
		ItemID:   "item-789",
		ItemType: "message",
		Text:     "Complete message",
	}

	if e.Type() != EventTypeItemCompleted {
		t.Errorf("expected EventTypeItemCompleted, got %v", e.Type())
	}
	if e.Text != "Complete message" {
		t.Errorf("unexpected Text: %q", e.Text)
	}
}

func TestTokenUsageEvent(t *testing.T) {
	e := TokenUsageEvent{
		ThreadID: "thread-123",
		TotalUsage: &TokenUsage{
			InputTokens:  1000,
			OutputTokens: 500,
			TotalTokens:  1500,
		},
		LastUsage: &TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
		},
	}

	if e.Type() != EventTypeTokenUsage {
		t.Errorf("expected EventTypeTokenUsage, got %v", e.Type())
	}
	if e.TotalUsage.TotalTokens != 1500 {
		t.Errorf("unexpected TotalTokens: %d", e.TotalUsage.TotalTokens)
	}
	if e.LastUsage.TotalTokens != 150 {
		t.Errorf("unexpected LastUsage TotalTokens: %d", e.LastUsage.TotalTokens)
	}
}

func TestTurnUsage_Fields(t *testing.T) {
	u := TurnUsage{
		InputTokens:           100,
		CachedInputTokens:     50,
		OutputTokens:          75,
		ReasoningOutputTokens: 25,
		TotalTokens:           250,
	}

	if u.InputTokens != 100 {
		t.Errorf("unexpected InputTokens: %d", u.InputTokens)
	}
	if u.CachedInputTokens != 50 {
		t.Errorf("unexpected CachedInputTokens: %d", u.CachedInputTokens)
	}
	if u.OutputTokens != 75 {
		t.Errorf("unexpected OutputTokens: %d", u.OutputTokens)
	}
	if u.ReasoningOutputTokens != 25 {
		t.Errorf("unexpected ReasoningOutputTokens: %d", u.ReasoningOutputTokens)
	}
	if u.TotalTokens != 250 {
		t.Errorf("unexpected TotalTokens: %d", u.TotalTokens)
	}
}

func TestErrorEvent(t *testing.T) {
	testErr := errors.New("test error")
	now := time.Now()

	e := ErrorEvent{
		ThreadID:  "thread-123",
		TurnID:    "turn-456",
		Error:     testErr,
		Context:   "send_message",
		Timestamp: now,
	}

	if e.Type() != EventTypeError {
		t.Errorf("expected EventTypeError, got %v", e.Type())
	}
	if e.Error != testErr {
		t.Errorf("unexpected Error: %v", e.Error)
	}
	if e.Context != "send_message" {
		t.Errorf("unexpected Context: %q", e.Context)
	}
	if !e.Timestamp.Equal(now) {
		t.Errorf("unexpected Timestamp")
	}
}

func TestStateChangeEvent(t *testing.T) {
	e := StateChangeEvent{
		ThreadID: "thread-123",
		From:     "ready",
		To:       "processing",
	}

	if e.Type() != EventTypeStateChange {
		t.Errorf("expected EventTypeStateChange, got %v", e.Type())
	}
	if e.From != "ready" {
		t.Errorf("unexpected From: %q", e.From)
	}
	if e.To != "processing" {
		t.Errorf("unexpected To: %q", e.To)
	}
}

func TestStateChangeEvent_ClientLevel(t *testing.T) {
	e := StateChangeEvent{
		ThreadID: "", // Empty for client-level
		From:     "starting",
		To:       "ready",
	}

	if e.ThreadID != "" {
		t.Error("client-level state change should have empty ThreadID")
	}
}

// Test that all event types implement the Event interface
func TestEventInterface(t *testing.T) {
	events := []Event{
		ClientReadyEvent{},
		ThreadStartedEvent{},
		ThreadReadyEvent{},
		TurnStartedEvent{},
		TurnCompletedEvent{},
		TextDeltaEvent{},
		ItemStartedEvent{},
		ItemCompletedEvent{},
		TokenUsageEvent{},
		ErrorEvent{},
		StateChangeEvent{},
	}

	for _, e := range events {
		// Just verify Type() can be called (interface is satisfied)
		_ = e.Type()
	}
}
