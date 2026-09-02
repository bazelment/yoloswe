package reviewer

import (
	"context"
	"fmt"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agy"
)

// agyBackend wraps the agy CLI (print mode) as a Backend.
// Each RunPrompt call is a one-shot execution (no persistent session) — agy
// has no concept of a long-running process to reuse across turns; resume is
// requested via --conversation on every invocation instead.
//
// # No tool-call events
//
// agy's print-mode wire format only exposes TextEvent, TurnCompleteEvent, and
// ErrorEvent (see agent-cli-wrapper/agy/events.go) — there is no tool-start or
// tool-complete signal to bridge, unlike the gemini/ACP backend this replaces.
// RunPrompt therefore never calls handler.OnToolStart/OnToolComplete. Callers
// that need to know what files an agy review touched must fall back to
// git-diff-based detection (see multiagent/agent/session.go's
// detectFileChangesGit, the same workaround already used for codex).
//
// # Resume verification
//
// agy's --output-format json result carries conversation_id, so a requested
// resume is verified by comparing it against the id we asked for: a match
// promotes ResumeStatusUnverified to ResumeStatusOK, a mismatch (or a turn
// that errors before completion) leaves it Unverified. This is a real check,
// not an optimistic promotion on success alone — an unrecognized
// --conversation id would otherwise surface only as agy's own turn failure,
// indistinguishable from any other error.
type agyBackend struct {
	// cliPath overrides the agy binary path. Empty means "agy" (agy's own
	// default, resolved via $PATH) — only tests set this, to point RunPrompt
	// at a fake binary instead of a live agy install.
	cliPath string
	config  Config
}

func newAgyBackend(config Config) *agyBackend {
	return &agyBackend{config: config}
}

// Start is a no-op for agy (one-shot per prompt).
func (b *agyBackend) Start(_ context.Context) error {
	return nil
}

// Stop is a no-op for agy (one-shot per prompt).
func (b *agyBackend) Stop() error {
	return nil
}

func (b *agyBackend) RunPrompt(ctx context.Context, prompt string, handler EventHandler) (*ReviewResult, error) {
	var sessionOpts []agy.SessionOption
	if b.config.Model != "" {
		sessionOpts = append(sessionOpts, agy.WithModel(b.config.Model))
	}
	if b.config.Effort != "" {
		sessionOpts = append(sessionOpts, agy.WithEffort(b.config.Effort))
	}
	if b.config.WorkDir != "" {
		sessionOpts = append(sessionOpts, agy.WithWorkDir(b.config.WorkDir))
	}

	var resumeStatus ResumeStatus
	if b.config.ResumeSessionID != "" {
		// Start at Unverified so a session that errors out before turn
		// completion still surfaces "resume was attempted" in the envelope,
		// instead of letting omitempty erase the signal. Promoted to OK
		// below once the turn actually completes — see the type doc for why
		// agy gives us no stronger signal than that.
		resumeStatus = ResumeStatusUnverified
		sessionOpts = append(sessionOpts, agy.WithConversation(b.config.ResumeSessionID))
	}

	sessionOpts = append(sessionOpts, agy.WithStderrHandler(stderrPrefixHandler("agy")))
	if b.cliPath != "" {
		sessionOpts = append(sessionOpts, agy.WithCLIPath(b.cliPath))
	}

	session := agy.NewSession(prompt, sessionOpts...)
	if err := session.Start(ctx); err != nil {
		return reviewErrorResult(resumeStatus, fmt.Errorf("agy: failed to start session: %w", err))
	}

	if handler != nil {
		// agy emits no Ready-equivalent event with a session id, so report
		// what we configured (empty session id) just like the model.
		handler.OnSessionInfo("", b.config.Model)
	}

	var responseText string
	var durationMs int64
	var success bool
	var turnErr error
	var eventErr error
	var conversationID string
	sawTurnComplete := false

loop:
	for {
		select {
		case <-ctx.Done():
			_ = session.Stop()
			return reviewPartialResult(resumeStatus, &bridgeResult{responseText: responseText, durationMs: durationMs}, ctx.Err())
		case evt, ok := <-session.Events():
			if !ok {
				break loop
			}
			switch e := evt.(type) {
			case agy.TextEvent:
				responseText += e.Text
				if handler != nil {
					handler.OnText(e.Text)
				}
			case agy.TurnCompleteEvent:
				sawTurnComplete = true
				durationMs = e.DurationMs
				success = e.Success
				conversationID = e.ConversationID
				if e.Error != nil {
					turnErr = e.Error
				} else {
					turnErr = eventErr
				}
				if handler != nil {
					handler.OnTurnComplete(success, durationMs)
				}
			case agy.ErrorEvent:
				eventErr = e.Error
				if handler != nil {
					handler.OnError(e.Error, e.Context)
				}
			}
		}
	}

	if !sawTurnComplete {
		if eventErr != nil {
			return reviewPartialResult(resumeStatus, &bridgeResult{responseText: responseText, durationMs: durationMs}, fmt.Errorf("agy: %w", eventErr))
		}
		return reviewPartialResult(resumeStatus, &bridgeResult{responseText: responseText, durationMs: durationMs}, fmt.Errorf("agy: session ended without result"))
	}

	if resumeStatus == ResumeStatusUnverified && turnErr == nil && conversationID == b.config.ResumeSessionID {
		// The turn completed with no error and agy echoed back the exact
		// conversation id we asked to resume — a real check, not an
		// optimistic promotion on success alone. See the type doc.
		resumeStatus = ResumeStatusOK
	}

	if turnErr != nil {
		return &ReviewResult{
			ResponseText: responseText,
			Success:      false,
			DurationMs:   durationMs,
			ErrorMessage: turnErr.Error(),
			ResumeStatus: resumeStatus,
		}, fmt.Errorf("agy: turn failed: %w", turnErr)
	}

	return &ReviewResult{
		ResponseText: responseText,
		Success:      success,
		DurationMs:   durationMs,
		ResumeStatus: resumeStatus,
	}, nil
}
