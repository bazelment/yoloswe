package session

import (
	"context"
	"fmt"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// DelegatorScenarioConfig defines a test scenario for the delegator agent.
type DelegatorScenarioConfig struct { //nolint:govet // fieldalignment: keep related fields grouped
	// InitialPrompt is the first user message sent to the delegator.
	InitialPrompt string

	// Behaviors maps session type to a queue of MockSessionBehavior scripts.
	Behaviors map[string][]*MockSessionBehavior

	// MaxTurns limits the number of delegator turns (default: 10).
	MaxTurns int

	// TurnTimeout is the per-turn timeout (default: 120s).
	TurnTimeout time.Duration

	// AutoNotify sends child state notifications automatically after each turn.
	AutoNotify bool

	// FollowUps maps turn number to an explicit user message to send instead
	// of auto-notification.
	FollowUps map[int]string

	// Model is the Claude model to use (default: "haiku").
	Model string

	// SystemPrompt overrides the default DelegatorSystemPrompt.
	SystemPrompt string

	// SessionOpts are extra claude.SessionOption values appended to the session
	// (e.g. for future WithAgents testing).
	SessionOpts []claude.SessionOption
}

// DelegatorScenarioResult captures the conversation outcome.
type DelegatorScenarioResult struct {
	Mock      *MockDelegatorToolHandler
	Turns     []DelegatorTurnResult
	TotalCost float64
}

// TurnCount returns the number of turns executed.
func (r *DelegatorScenarioResult) TurnCount() int {
	return len(r.Turns)
}

// DelegatorTurnResult captures events from a single delegator turn.
type DelegatorTurnResult struct {
	TextOutput string   // concatenated text events
	ToolCalls  []string // tool names called in this turn
	Success    bool
	CostUSD    float64
}

// RunDelegatorScenario executes a delegator scenario using a real Claude
// session with mock tools and returns the results.
func RunDelegatorScenario(ctx context.Context, cfg DelegatorScenarioConfig) (*DelegatorScenarioResult, error) {
	// Apply defaults.
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 10
	}
	if cfg.TurnTimeout <= 0 {
		cfg.TurnTimeout = 120 * time.Second
	}
	if cfg.Model == "" {
		cfg.Model = "haiku"
	}
	systemPrompt := cfg.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = DelegatorSystemPrompt
	}

	mock := NewMockDelegatorToolHandler(cfg.Behaviors)

	opts := DelegatorBaseSessionOpts(cfg.Model, mock.Registry(), systemPrompt)
	opts = append(opts, cfg.SessionOpts...)

	s := claude.NewSession(opts...)
	if err := s.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start session: %w", err)
	}
	defer s.Stop()

	result := &DelegatorScenarioResult{Mock: mock}

	// Send initial prompt.
	if _, err := s.SendMessage(ctx, cfg.InitialPrompt); err != nil {
		return nil, fmt.Errorf("failed to send initial prompt: %w", err)
	}

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		turnCtx, cancel := context.WithTimeout(ctx, cfg.TurnTimeout)
		turnResult, events, err := s.CollectResponse(turnCtx)
		cancel()
		if err != nil {
			return result, fmt.Errorf("turn %d collect failed: %w", turn, err)
		}
		if turnResult == nil {
			return result, fmt.Errorf("turn %d: nil result with no error", turn)
		}

		tr := DelegatorTurnResult{
			Success: turnResult.Success,
			CostUSD: turnResult.Usage.CostUSD,
		}

		// Extract text and tool calls from events.
		for _, evt := range events {
			switch e := evt.(type) {
			case claude.TextEvent:
				tr.TextOutput += e.Text
			case claude.ToolStartEvent:
				tr.ToolCalls = append(tr.ToolCalls, e.Name)
			}
		}

		result.Turns = append(result.Turns, tr)
		result.TotalCost += tr.CostUSD

		// If no tool calls this turn, the delegator is done talking.
		if len(tr.ToolCalls) == 0 {
			break
		}

		// Determine next message.
		var nextMsg string

		// Check for explicit follow-up.
		if cfg.FollowUps != nil {
			if msg, ok := cfg.FollowUps[turn]; ok {
				nextMsg = msg
			}
		}

		// Auto-notify: advance sessions step by step until a notifiable state
		// is reached (completed, failed, waiting_for_input). Each step simulates
		// async child progress between delegator turns.
		if nextMsg == "" && cfg.AutoNotify {
			nextMsg = mock.AdvanceUntilNotification()
		}

		if nextMsg == "" {
			// Nothing to send; conversation is over.
			break
		}

		if _, err := s.SendMessage(ctx, nextMsg); err != nil {
			return result, fmt.Errorf("turn %d send follow-up failed: %w", turn, err)
		}
	}

	return result, nil
}
