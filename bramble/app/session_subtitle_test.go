package app

import (
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/bramble/session"
)

func TestFormatSessionSubtitle_PendingSession(t *testing.T) {
	sess := &session.SessionInfo{
		CreatedAt: time.Now().Add(-5 * time.Minute),
		Prompt:    "Fix authentication bug in login flow",
		Progress: session.SessionProgressSnapshot{
			TurnCount:    0,
			TotalCostUSD: 0.0,
		},
	}
	subtitle := formatSessionSubtitle(sess)

	// Should contain elapsed time
	if !contains(subtitle, "ago") {
		t.Errorf("expected elapsed time in subtitle, got: %s", subtitle)
	}
	// Should NOT contain T: prefix when both turns and cost are zero
	if contains(subtitle, "T:0") {
		t.Errorf("expected no T: prefix for pending session, got: %s", subtitle)
	}
	// Should contain prompt
	if !contains(subtitle, "Fix") {
		t.Errorf("expected prompt excerpt in subtitle, got: %s", subtitle)
	}
}

func TestFormatSessionSubtitle_ActiveSession(t *testing.T) {
	sess := &session.SessionInfo{
		CreatedAt: time.Now().Add(-3 * time.Minute),
		Prompt:    "Fix auth bug in login flow",
		Progress: session.SessionProgressSnapshot{
			TurnCount:    5,
			TotalCostUSD: 0.0312,
		},
	}
	subtitle := formatSessionSubtitle(sess)

	// Should contain turn count
	if !contains(subtitle, "T:5") {
		t.Errorf("expected T:5 in subtitle, got: %s", subtitle)
	}
	// Should contain cost
	if !contains(subtitle, "$0.0312") {
		t.Errorf("expected $0.0312 in subtitle, got: %s", subtitle)
	}
	// Should contain elapsed time
	if !contains(subtitle, "ago") {
		t.Errorf("expected elapsed time in subtitle, got: %s", subtitle)
	}
	// Should contain prompt
	if !contains(subtitle, "Fix") {
		t.Errorf("expected prompt excerpt in subtitle, got: %s", subtitle)
	}
}

func TestFormatSessionSubtitle_LongPrompt(t *testing.T) {
	longPrompt := strings.Repeat("abcdefghij", 10) // 100 chars
	sess := &session.SessionInfo{
		CreatedAt: time.Now().Add(-1 * time.Minute),
		Prompt:    longPrompt,
		Progress: session.SessionProgressSnapshot{
			TurnCount:    2,
			TotalCostUSD: 0.0150,
		},
	}
	subtitle := formatSessionSubtitle(sess)

	// Subtitle should be truncated (prefix + truncated prompt)
	// The spec says max 40 chars, but with prefix it will be longer
	// Just verify it ends with "..." to show truncation
	if !contains(subtitle, "...") {
		t.Errorf("expected truncation in subtitle for long prompt, got: %s", subtitle)
	}
}

func TestFormatSessionSubtitle_ZeroCreatedAt(t *testing.T) {
	sess := &session.SessionInfo{
		// CreatedAt intentionally zero
		Prompt: "Hello world",
		Progress: session.SessionProgressSnapshot{
			TurnCount:    1,
			TotalCostUSD: 0.0010,
		},
	}
	subtitle := formatSessionSubtitle(sess)

	// Should NOT contain a nonsensical "ago" timestamp for the zero time
	if contains(subtitle, "d ago") {
		t.Errorf("expected no stale elapsed time for zero CreatedAt, got: %s", subtitle)
	}
	// Should still contain turn/cost info
	if !contains(subtitle, "T:1") {
		t.Errorf("expected T:1 in subtitle, got: %s", subtitle)
	}
}

func TestFormatSessionSubtitle_ZeroCostZeroTurns(t *testing.T) {
	sess := &session.SessionInfo{
		CreatedAt: time.Now().Add(-2 * time.Minute),
		Prompt:    "Test prompt",
		Progress: session.SessionProgressSnapshot{
			TurnCount:    0,
			TotalCostUSD: 0.0,
		},
	}
	subtitle := formatSessionSubtitle(sess)

	// Should NOT contain T: or $ prefix
	if contains(subtitle, "T:") || contains(subtitle, "$") {
		t.Errorf("expected no progress prefix for zero cost/turns, got: %s", subtitle)
	}
	// Should contain elapsed time and prompt
	if !contains(subtitle, "ago") {
		t.Errorf("expected elapsed time in subtitle, got: %s", subtitle)
	}
	if !contains(subtitle, "Test") {
		t.Errorf("expected prompt in subtitle, got: %s", subtitle)
	}
}
