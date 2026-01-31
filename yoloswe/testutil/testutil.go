// Package testutil provides shared test utilities for yoloswe integration tests.
package testutil

import (
	"context"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// TurnEvents collects events from a single turn.
type TurnEvents struct {
	Ready        *claude.ReadyEvent
	TextEvents   []claude.TextEvent
	ToolStarts   []claude.ToolStartEvent
	ToolComplete []claude.ToolCompleteEvent
	ToolResults  []claude.CLIToolResultEvent
	TurnComplete *claude.TurnCompleteEvent
	Errors       []claude.ErrorEvent
}

// CollectTurnEvents collects all events until TurnCompleteEvent or context cancellation.
func CollectTurnEvents(ctx context.Context, events <-chan claude.Event) (*TurnEvents, error) {
	result := &TurnEvents{}

	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case event, ok := <-events:
			if !ok {
				return result, context.Canceled
			}

			switch e := event.(type) {
			case claude.ReadyEvent:
				result.Ready = &e
			case claude.TextEvent:
				result.TextEvents = append(result.TextEvents, e)
			case claude.ToolStartEvent:
				result.ToolStarts = append(result.ToolStarts, e)
			case claude.ToolCompleteEvent:
				result.ToolComplete = append(result.ToolComplete, e)
			case claude.CLIToolResultEvent:
				result.ToolResults = append(result.ToolResults, e)
			case claude.TurnCompleteEvent:
				result.TurnComplete = &e
				return result, nil
			case claude.ErrorEvent:
				result.Errors = append(result.Errors, e)
			}
		}
	}
}

// HasToolNamed checks if any tool with the given name was started.
func (te *TurnEvents) HasToolNamed(name string) bool {
	for _, t := range te.ToolStarts {
		if t.Name == name {
			return true
		}
	}
	return false
}

// ValidateRecording validates session recording structure.
func ValidateRecording(t *testing.T, recording *claude.SessionRecording, minTurns int) {
	t.Helper()

	if recording == nil {
		t.Fatal("recording is nil")
	}

	if len(recording.Turns) < minTurns {
		t.Errorf("expected at least %d turns, got %d", minTurns, len(recording.Turns))
	}
}
