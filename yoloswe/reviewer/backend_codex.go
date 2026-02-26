package reviewer

import (
	"context"
	"fmt"
	"strings"

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
	// Create a new thread if none exists, or reuse for follow-ups
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
	}

	_, err := b.thread.SendMessage(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	result := &ReviewResult{}
	var responseText strings.Builder

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-b.client.Events():
			if !ok {
				return nil, fmt.Errorf("event channel closed unexpectedly")
			}

			switch e := event.(type) {
			case codex.TextDeltaEvent:
				if e.ThreadID == b.thread.ID() {
					if handler != nil {
						handler.OnText(e.Delta)
					}
					responseText.WriteString(e.Delta)
				}
			case codex.ReasoningDeltaEvent:
				if e.ThreadID == b.thread.ID() {
					if handler != nil {
						handler.OnReasoning(e.Delta)
					}
				}
			case codex.CommandStartEvent:
				if e.ThreadID == b.thread.ID() {
					if handler != nil {
						handler.OnToolStart(e.CallID, e.ParsedCmd, "")
					}
				}
			case codex.CommandOutputEvent:
				if e.ThreadID == b.thread.ID() {
					if handler != nil {
						handler.OnToolOutput(e.CallID, e.Chunk)
					}
				}
			case codex.CommandEndEvent:
				if e.ThreadID == b.thread.ID() {
					if handler != nil {
						handler.OnToolEnd(e.CallID, e.ExitCode, e.DurationMs)
					}
				}
			case codex.TurnCompletedEvent:
				if e.ThreadID == b.thread.ID() {
					result.ResponseText = responseText.String()
					result.Success = e.Success
					result.DurationMs = e.DurationMs
					result.InputTokens = e.Usage.InputTokens
					result.OutputTokens = e.Usage.OutputTokens
					return result, nil
				}
			case codex.ErrorEvent:
				if handler != nil {
					handler.OnError(e.Error, e.Context)
				}
				return nil, fmt.Errorf("error: %v", e.Error)
			}
		}
	}
}
