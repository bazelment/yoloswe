// Package testutil provides shared test utilities for yoloswe integration tests.
package testutil

import (
	"context"
	"os/exec"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// InitGitRepo initializes a git repo in the given directory with an initial commit.
// This is needed because the reviewer (Codex) runs git commands like `git log`.
func InitGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	// Configure git user for commits
	cmd = exec.Command("git", "config", "user.email", "test@test.com")
	cmd.Dir = dir
	cmd.Run()
	cmd = exec.Command("git", "config", "user.name", "Test")
	cmd.Dir = dir
	cmd.Run()
	// Create initial commit so git log works
	cmd = exec.Command("git", "commit", "--allow-empty", "-m", "Initial commit")
	cmd.Dir = dir
	cmd.Run()
}

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
