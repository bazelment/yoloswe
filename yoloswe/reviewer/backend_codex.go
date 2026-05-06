package reviewer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
		codex.WithStderrHandler(stderrPrefixHandler("codex")),
	}
	// Wire the read-only approval handler at the client level. This is
	// paired with ApprovalPolicyOnFailure on the thread (set in
	// reviewer.New) so that Codex sends approval requests to us instead
	// of auto-approving. The handler denies Write tool calls while
	// allowing Bash/read tools—a software-level guard since bwrap
	// sandboxing is unavailable on most hosts (see Config doc).
	if b.config.ReadOnly {
		opts = append(opts, codex.WithApprovalHandler(codex.ReadOnlyHandler()))
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
	var resumeStatus ResumeStatus
	if b.thread == nil {
		threadOpts := []codex.ThreadOption{
			codex.WithModel(b.config.Model),
			codex.WithWorkDir(b.config.WorkDir),
			codex.WithApprovalPolicy(b.config.ApprovalPolicy),
			codex.WithSandbox(b.config.Sandbox),
		}
		var thread *codex.Thread
		var err error
		if b.config.ResumeSessionID != "" {
			// Start at Unverified so a non-recognized error from
			// ResumeThread (e.g. transport failure before the response
			// arrives) still surfaces "resume was attempted" in the
			// envelope, instead of letting omitempty erase the signal.
			resumeStatus = ResumeStatusUnverified
			thread, err = b.client.ResumeThread(ctx, b.config.ResumeSessionID, threadOpts...)
			if err != nil && isCodexResumeNotFound(err) {
				slog.Warn("codex resume failed; falling back to fresh thread", "session_id", b.config.ResumeSessionID, "error", err.Error())
				resumeStatus = ResumeStatusFallback
				thread, err = b.client.CreateThread(ctx, threadOpts...)
			} else if err == nil {
				resumeStatus = ResumeStatusOK
			}
		} else {
			thread, err = b.client.CreateThread(ctx, threadOpts...)
		}
		if err != nil {
			return reviewErrorResult(resumeStatus, fmt.Errorf("failed to create thread: %w", err))
		}
		if err := thread.WaitReady(ctx); err != nil {
			return reviewErrorResult(resumeStatus, fmt.Errorf("thread not ready: %w", err))
		}
		resumeStatus = resumeStatusAfterSessionReady(resumeStatus, b.config.ResumeSessionID, thread.ID())
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
		return reviewErrorResult(resumeStatus, fmt.Errorf("failed to send message: %w", err))
	}

	bridged, err := bridgeStreamEvents(ctx, b.client.Events(), handler, b.thread.ID())
	if err != nil {
		return reviewErrorResult(resumeStatus, fmt.Errorf("codex: %w", err))
	}

	result := &ReviewResult{
		ResponseText: bridged.responseText,
		Success:      bridged.success,
		DurationMs:   bridged.durationMs,
		ResumeStatus: resumeStatus,
	}

	// Extract codex-specific token usage and error from the raw turn event.
	if tc, ok := bridged.turnEvent.(codex.TurnCompletedEvent); ok {
		result.InputTokens = tc.Usage.InputTokens
		result.OutputTokens = tc.Usage.OutputTokens
		if tc.Error != nil {
			result.ErrorMessage = tc.Error.Error()
		}
	}

	return result, nil
}

func isCodexResumeNotFound(err error) bool {
	if errors.Is(err, codex.ErrThreadNotFound) {
		return true
	}
	var rpcErr *codex.RPCError
	if errors.As(err, &rpcErr) {
		return isResumeUnavailableMessage(rpcErr.Message)
	}
	return false
}
