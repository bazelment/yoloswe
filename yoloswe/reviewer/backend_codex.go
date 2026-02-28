package reviewer

import (
	"context"
	"fmt"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
)

// codexBackend wraps the Codex SDK as a Backend.
type codexBackend struct {
	client *codex.Client
	thread *codex.Thread
	config Config
}

func newCodexBackend(config Config) *codexBackend {
	return &codexBackend{config: config}
}

func (b *codexBackend) Start(ctx context.Context) error {
	opts := []codex.ClientOption{
		codex.WithClientName("codex-review"),
		codex.WithClientVersion("1.0.0"),
	}
	if b.config.SessionLogPath != "" {
		opts = append(opts, codex.WithSessionLogPath(b.config.SessionLogPath))
	}
	b.client = codex.NewClient(opts...)
	return b.client.Start(ctx)
}

func (b *codexBackend) Stop() error {
	if b.client != nil {
		return b.client.Stop()
	}
	return nil
}

func (b *codexBackend) RunPrompt(ctx context.Context, prompt string, handler EventHandler) (*ReviewResult, error) {
	// Create a new thread if none exists, or reuse for follow-ups.
	if b.thread == nil {
		thread, err := b.client.CreateThread(ctx,
			codex.WithModel(b.config.Model),
			codex.WithWorkDir(b.config.WorkDir),
			codex.WithApprovalPolicy(b.config.ApprovalPolicy),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create thread: %w", err)
		}
		if err := thread.WaitReady(ctx); err != nil {
			return nil, fmt.Errorf("thread not ready: %w", err)
		}
		b.thread = thread
		if handler != nil {
			handler.OnSessionInfo(thread.ID(), b.config.Model)
		}
	}

	var turnOpts []codex.TurnOption
	if b.config.Effort != "" {
		turnOpts = append(turnOpts, codex.WithEffort(b.config.Effort))
	}
	_, err := b.thread.SendMessage(ctx, prompt, turnOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	bridged, err := bridgeStreamEvents(ctx, b.client.Events(), handler, b.thread.ID())
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}

	result := &ReviewResult{
		ResponseText: bridged.responseText,
		Success:      bridged.success,
		DurationMs:   bridged.durationMs,
	}

	// Extract codex-specific token usage from the raw turn event.
	if tc, ok := bridged.turnEvent.(codex.TurnCompletedEvent); ok {
		result.InputTokens = tc.Usage.InputTokens
		result.OutputTokens = tc.Usage.OutputTokens
	}

	return result, nil
}
