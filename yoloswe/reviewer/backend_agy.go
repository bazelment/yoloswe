package reviewer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agy"
)

// agyBackend wraps the agy CLI (print mode) as a Backend.
// Each RunPrompt call is a one-shot execution (no persistent session) — agy
// has no concept of a long-running process to reuse across turns; conversation
// continuity is requested via --conversation on every invocation instead.
//
// # Conversation threading
//
// Continuity across turns is the backend's own job, because each RunPrompt
// spawns a fresh agy process. The id a completed turn reports is stored on the
// backend and preferred over Config.ResumeSessionID on the next call, so
// Reviewer.FollowUp — whose prompt is purely context-dependent ("the code has
// been updated based on your previous feedback") — continues the conversation
// that saw the diff instead of starting an empty one. This mirrors
// providerRunner.RunTurn, which threads result.SessionID back the same way for
// ephemeral providers (bramble/session/manager.go).
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
// # Stall protection
//
// agy is a print-mode CLI: it emits nothing until the turn ends. There is
// therefore no activity to reset an inactivity deadline, so Config.IdleTimeout
// — explicitly "NOT a total-wall cap" — is deliberately NOT honored here.
// Applying it as a wall-clock bound would kill a healthy long review at the
// value calibrated for detecting a stalled *streaming* backend.
//
// The bound agy honors is Config.TurnTimeout, a total wall-clock cap: passed
// down as --print-timeout (agy's own flag, whose default is 5m) so the CLI
// enforces it, and backed by a local timer so a subprocess that ignores its own
// flag still returns. Zero leaves agy's default in force. Callers that want a
// hard cap set it; bramble code-review sets it from --timeout, the same value
// that bounds the context.
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

	// convID carries the last completed turn's conversation id across RunPrompt
	// calls, guarded by mu. Config is copied by value at New(), so the thread
	// state cannot live there.
	convID string

	config Config

	mu sync.Mutex
}

// resumeID returns the conversation to continue: the id threaded from a prior
// completed turn when there is one, else the caller-supplied Config value.
func (b *agyBackend) resumeID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.convID != "" {
		return b.convID
	}
	return b.config.ResumeSessionID
}

func (b *agyBackend) setResumeID(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.convID = id
}

func newAgyBackend(config Config) *agyBackend {
	return &agyBackend{config: config}
}

// agyEffortLevel maps the reviewer's free-form --effort string onto agy's
// vocabulary, which its own --effort flag documents as exactly low|medium|high.
//
// agy.WithEffort is a deliberately thin transport that "does not validate,
// normalize, or clamp the value in any way" and leaves that to the caller (see
// agent-cli-wrapper/agy/session_options.go), so this is the reviewer's half of
// that contract - the same shape claudeEffortLevel implements next door, and
// for the same reason: the --effort flag is shared across backends whose
// vocabularies differ, and losing a whole review round to a typo'd level is a
// worse outcome than running at the model default.
//
// max clamps to high rather than dropping: it is the shared flag's documented
// "most effort" value, and high is the most agy offers, so honoring the intent
// beats silently running at the model default. multiagent/agent's own
// agyEffortLevel makes the identical call.
func agyEffortLevel(effort string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "":
		return "", false
	case "auto":
		// Explicitly "let the model decide" - same wire effect as unset.
		return "", false
	case "low":
		return "low", true
	case "medium", "med":
		return "medium", true
	case "high":
		return "high", true
	case "max":
		return "high", true
	default:
		slog.Warn("unrecognized effort for agy backend; using model default",
			"effort", effort,
			"supported", "low, medium, high, max")
		return "", false
	}
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
	if effort, ok := agyEffortLevel(b.config.Effort); ok {
		sessionOpts = append(sessionOpts, agy.WithEffort(effort))
	}
	if b.config.WorkDir != "" {
		// --add-dir puts the worktree in agy's workspace so its file tools can
		// see it at all; agy resolves tools against a registered workspace, not
		// against the process cwd.
		sessionOpts = append(sessionOpts, agy.WithWorkDir(b.config.WorkDir), agy.WithAddDir(b.config.WorkDir))
	}
	// --add-dir grants reads but NOT the "command" permission, and the review
	// prompt mandates a shell `git diff` (see diffScopeClause in reviewer.go).
	// Verified live against agy 1.1.25: with --add-dir alone that command is
	// auto-denied and the turn returns CANCELED with an empty response, so a
	// read-only grant does not make an agy review runnable — it only makes the
	// failure loud. Bash is granted for the same reason every sibling backend
	// grants it (a reviewer needs git log/diff/show): backend_claude.go uses
	// PermissionModeBypass, backend_cursor.go uses --trust --force.
	//
	// Config.ReadOnly is deliberately NOT consulted, and reviewer.go's Config
	// doc records that agy ignores it. agy exposes no per-tool lever that would
	// let it be honoured, and both alternatives were tried live against 1.1.25:
	//
	//   --sandbox alone still auto-denies "command", so the review cannot run.
	//   --sandbox WITH the grant does not restore the boundary — asked to write
	//   a file, the model shelled around the sandbox and created it.
	//
	// The third lever agy's own stderr names, a permissions.allow rule, lives in
	// a single global settings.json shared by every concurrent agy process on
	// the host, so writing it per-invocation is a race, not a boundary.
	//
	// So this backend is honestly unable to offer a read-only review, and says
	// so rather than implying one: an agy review must be given a checkout it is
	// allowed to modify. The prompt is the only remaining constraint, exactly as
	// for cursor. Callers needing a real boundary must sandbox the process.
	sessionOpts = append(sessionOpts, agy.WithDangerouslySkipPermissions())
	if b.config.TurnTimeout > 0 {
		// agy's --print-timeout is a TOTAL wall-clock bound on print mode (its
		// own default is 5m), so it is driven by Config.TurnTimeout, never by
		// Config.IdleTimeout — those are different quantities, and using the
		// inactivity value here would kill a healthy long review. See the type doc.
		sessionOpts = append(sessionOpts, agy.WithPrintTimeout(b.config.TurnTimeout))
	}

	requestedResumeID := b.resumeID()
	var resumeStatus ResumeStatus
	if requestedResumeID != "" {
		// Start at Unverified so a session that errors out before turn
		// completion still surfaces "resume was attempted" in the envelope,
		// instead of letting omitempty erase the signal. Promoted to OK
		// below once the turn actually completes — see the type doc for why
		// agy gives us no stronger signal than that.
		resumeStatus = ResumeStatusUnverified
		sessionOpts = append(sessionOpts, agy.WithConversation(requestedResumeID))
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
		// agy has no Ready-equivalent event, so the conversation id is not
		// known yet — it arrives with TurnCompleteEvent. Report the model now
		// (callers render it while the turn streams) and re-report with the id
		// once the turn completes, below. The id reported here is the one we
		// asked to resume, not "": rendererEventHandler.OnSessionInfo assigns
		// lastSessionID unconditionally, so passing "" would erase the id a
		// prior turn published every time a new turn starts.
		handler.OnSessionInfo(requestedResumeID, b.config.Model)
	}

	var responseText string
	var durationMs int64
	var success bool
	var turnErr error
	var eventErr error
	var conversationID string
	var effectiveModel string
	var inputTokens, outputTokens int64
	sawTurnComplete := false

	// Local backstop for Config.TurnTimeout. agy already enforces the same bound
	// via --print-timeout (set above), so this only fires when the subprocess
	// ignores its own flag or wedges before parsing it. A nil channel blocks
	// forever, so TurnTimeout==0 leaves the select with exactly the two arms it
	// had before. Ordering: the timer starts before the first receive, so a
	// subprocess that never emits anything is still bounded; a turn that
	// completes first breaks the loop and the deferred Stop tears the timer down.
	var stallC <-chan time.Time
	if b.config.TurnTimeout > 0 {
		stallTimer := time.NewTimer(b.config.TurnTimeout)
		defer stallTimer.Stop()
		stallC = stallTimer.C
	}

loop:
	for {
		select {
		case <-ctx.Done():
			_ = session.Stop()
			return reviewPartialResult(resumeStatus, &bridgeResult{responseText: responseText, durationMs: durationMs}, ctx.Err())
		case <-stallC:
			_ = session.Stop()
			return reviewPartialResult(resumeStatus, &bridgeResult{responseText: responseText, durationMs: durationMs},
				fmt.Errorf("agy: no result after %s (turn timeout)", b.config.TurnTimeout))
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
				effectiveModel = e.Model
				inputTokens = int64(e.Usage.InputTokens)
				outputTokens = int64(e.Usage.OutputTokens)
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

	// Report the model agy actually ran, which is not always the configured
	// one: the wrapper retargets a model whose pinned level disagrees with an
	// explicit --effort. Reviewer.effectiveModel is set from here and published
	// as the envelope's model, and EffectiveModel's own doc promises "the model
	// actually used by the backend".
	reportedModel := b.config.Model
	if effectiveModel != "" {
		reportedModel = effectiveModel
	}

	if conversationID != "" {
		// Thread this turn's conversation onto the backend so the next
		// RunPrompt (Reviewer.FollowUp) continues it. See the type doc.
		b.setResumeID(conversationID)
	}

	if handler != nil && (conversationID != "" || reportedModel != b.config.Model) {
		// Report the id agy assigned this conversation. Reviewer.lastSessionID
		// is set from here (see rendererEventHandler.OnSessionInfo), and it is
		// what BuildEnvelope publishes as session_id — the only way a caller
		// obtains the id it must later pass back as Config.ResumeSessionID.
		// Every sibling backend reports its own (backend_codex.go's thread id,
		// backend_claude.go's and backend_cursor.go's session id); agy's simply
		// is not known until the turn ends.
		handler.OnSessionInfo(conversationID, reportedModel)
	}

	if resumeStatus == ResumeStatusUnverified && turnErr == nil && conversationID == requestedResumeID {
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
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
		}, fmt.Errorf("agy: turn failed: %w", turnErr)
	}

	return &ReviewResult{
		ResponseText: responseText,
		Success:      success,
		DurationMs:   durationMs,
		ResumeStatus: resumeStatus,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}, nil
}
