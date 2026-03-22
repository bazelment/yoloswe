package session

import (
	"context"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// DelegatorSystemPrompt is the system prompt used by the delegator agent.
// It is exported so that the CLI test harness and scenario tests can reference
// or override it.
const DelegatorSystemPrompt = `You are a delegator agent that orchestrates work by managing child sessions. You have these tools:

1. start_session — Start a child session: planner (read-only analysis), builder (code modification), or codetalk (read-only code exploration and understanding)
2. stop_session — Stop a running session
3. get_session_progress — Check a session's progress, status, and recent output
4. send_followup — Send a follow-up message to an idle child session, resuming it with its existing conversation context
5. Read — Read files directly (use this to read research files produced by codetalk sessions)

You NEVER write files, run commands, or make code changes directly. All code work is done by child sessions.

## Session types

- **codetalk**: Read-only code exploration and understanding. Use when the goal is to understand how code works — exploring, explaining, tracing code paths. When a codetalk session goes idle, get_session_progress returns a research_file path containing its full analysis. Use Read to read this file.
- **planner**: Read-only analysis that produces an implementation plan file. Use when the goal is to plan a code change.
- **builder**: Read-write implementation. Use for making code changes. When a planner completes and reports a plan file path, start a builder with: "Implement the plan in <plan-file-path>".

## How sessions work

Child sessions run asynchronously. After you start a session, it works in the background while you are idle. You will receive notifications when sessions change state (completed, failed, idle, or need input). Do NOT poll get_session_progress in a loop — end your turn and wait for the notification.

Use get_session_progress to check details AFTER receiving a notification, or when the user asks about a session.

## Follow-ups to child sessions

When a child session goes idle and you need it to do more work on a related topic, use send_followup instead of starting a new session. This preserves the session's conversation context.

Start a new session only when:
- The topic is completely unrelated to any existing idle session
- An existing session's context is getting full (over 70% used, as shown by get_session_progress)
- An existing session's context is exhausted (failed with context window error)
- You need a different session type (e.g., switching from codetalk to builder)

## Workflow

- For code understanding tasks, start a codetalk session. When it completes, read its research file and synthesize a concise answer for the user.
- For follow-up questions about code already explored by a codetalk session, first try to answer from the research you already read. Only use send_followup if you need the codewalker to explore further.
- For complex tasks that require both understanding and modification, start codetalk first, then planner, then builder.
- For simple, well-defined implementation tasks, skip understanding and go straight to planner or builder.
- After starting a session, tell the user what you started and end your turn.
- If a child session asks a question (status: waiting_for_input), relay the question to the user.

## Response style — inverted pyramid

Structure every answer like a news article, not a tutorial:

1. **Lead sentence** — the single most important takeaway, in one sentence. If someone reads only this, they should get the answer.
2. **Key mechanism** — the 2-3 sentence explanation of how it works. Name the critical files/functions but don't exhaustively list them.
3. **Supporting detail** — only if the question asks "how" or "trace through". Even then, use prose paragraphs, not numbered step-by-step walkthroughs or section headers.

Anti-patterns to avoid:
- Do NOT use markdown headers (##), horizontal rules (---), or numbered step lists to organize the answer. Use paragraph breaks and natural transitions instead.
- Do NOT open with "Here's how X works:" followed by a structured doc. Open with the answer itself.
- Do NOT include code blocks, tables, or bullet-point file listings unless the user specifically asked for code or a list.
- Do NOT reproduce the research file's structure, headers, or full code blocks verbatim.

The goal: someone hearing this answer read aloud should follow it easily. Write prose, not documentation.

## Error handling

- If a child session fails with a retriable error (transient API error, rate limit, lint failure fixable by retry), start a new session with the same or adjusted prompt.
- If a child session fails with a non-retriable error (context window exhausted, fundamental task issue), do NOT retry. Explain the situation to the user.
- If the task is ambiguous or unclear, do NOT start any sessions. Ask the user for clarification first.

## Rules

- You orchestrate — you NEVER do work directly except reading files.
- Do NOT poll get_session_progress repeatedly within a single turn. Call it once to check a session, then end your turn.
- Trust child session results.
- When writing prompts for child sessions, be specific and detailed about what the session should accomplish.`

// delegatorSystemPromptWithModels appends an "Available models" section to the
// base DelegatorSystemPrompt. If availableModels is empty, the base prompt is
// returned unchanged.
func delegatorSystemPromptWithModels(availableModels, defaultChildModel string) string {
	prompt := DelegatorSystemPrompt
	if availableModels != "" {
		prompt += "\n\n## Available models\n\nYou can use any of these models when starting child sessions:\n" + availableModels
	}
	if defaultChildModel != "" {
		prompt += "\nYour default model for child sessions is: " + defaultChildModel + ". You do not need to specify a model when starting sessions unless you want to override this default."
	}
	return prompt
}

// DelegatorBaseSessionOpts returns the security-critical Claude session options
// that are common to all delegator session instantiations (production runner,
// scenario tests, and CLI mock mode). These options ensure the delegator can
// only access its three SDK tools and cannot directly use file or command tools.
// Callers may append additional options (e.g. WithWorkDir, WithRecording) after
// calling this function.
func DelegatorBaseSessionOpts(model string, registry *claude.TypedToolRegistry, systemPrompt string) []claude.SessionOption {
	return []claude.SessionOption{
		claude.WithModel(model),
		claude.WithPermissionMode(claude.PermissionModePlan),
		claude.WithDangerouslySkipPermissions(),
		claude.WithSDKTools("delegator-tools", registry),
		claude.WithTools("Read"),
		claude.WithSystemPrompt(systemPrompt),
		claude.WithDisablePlugins(),
		claude.WithEventBufferSize(1000),
	}
}

// delegatorRunner implements sessionRunner for delegator sessions.
// It wraps a Claude SDK session with tools for managing child sessions.
type delegatorRunner struct {
	claudeSession *claude.Session
	toolHandler   *DelegatorToolHandler
	eventHandler  *sessionEventHandler
	worktreePath  string
	model         string
	recordingDir  string
}

func (r *delegatorRunner) Start(ctx context.Context) error {
	opts := DelegatorBaseSessionOpts(
		r.model,
		r.toolHandler.Registry(),
		delegatorSystemPromptWithModels(r.toolHandler.AvailableModelsDescription(), r.toolHandler.DefaultModel()),
	)
	opts = append(opts, claude.WithWorkDir(r.worktreePath))
	if r.recordingDir != "" {
		opts = append(opts, claude.WithRecording(r.recordingDir))
	}

	r.claudeSession = claude.NewSession(opts...)

	if err := r.claudeSession.Start(ctx); err != nil {
		return err
	}

	// Wire event forwarding from the Claude session to the Manager's output system
	go r.forwardEvents(ctx)

	return nil
}

func (r *delegatorRunner) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	result, err := r.claudeSession.Ask(ctx, message)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return &result.Usage, nil
}

// Stop stops the delegator's Claude session. Child sessions spawned by the
// delegator are parented to the Manager's context, not the delegator session
// context, so they continue running after the delegator stops. Callers that
// want to terminate children should call Manager.StopSession on each child ID
// returned by DelegatorToolHandler.ChildIDs() before stopping the delegator.
func (r *delegatorRunner) Stop() error {
	if r.claudeSession != nil {
		return r.claudeSession.Stop()
	}
	return nil
}

func (r *delegatorRunner) CLISessionID() string {
	if r.claudeSession == nil {
		return ""
	}
	info := r.claudeSession.Info()
	if info == nil {
		return ""
	}
	return info.SessionID
}

// watchChildSessionChanges subscribes to Manager state change events and
// forwards relevant child session notifications to the notify channel.
// It runs until ctx is canceled. The caller must call the returned unsubscribe
// function to clean up the subscription.
func watchChildSessionChanges(ctx context.Context, manager *Manager, toolHandler *DelegatorToolHandler, notify chan<- SessionStateChangeEvent) func() {
	stateCh := make(chan SessionStateChangeEvent, 100)
	unsub := manager.SubscribeStateChanges(stateCh)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-stateCh:
				if !ok {
					// stateCh was closed by the unsubscribe wrapper below,
					// meaning the delegator session has ended. Exit cleanly.
					return
				}
				// Only forward if this is a child session we're tracking
				if !toolHandler.IsChild(evt.SessionID) {
					continue
				}
				// Only notify on meaningful state transitions
				if evt.NewStatus == StatusIdle ||
					evt.NewStatus == StatusCompleted ||
					evt.NewStatus == StatusFailed ||
					evt.NewStatus == StatusStopped {
					select {
					case notify <- evt:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	return func() {
		unsub()
		close(stateCh)
	}
}

// forwardEvents reads events from the Claude session and forwards them to the
// session's event handler so they appear in the Manager's output system.
func (r *delegatorRunner) forwardEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-r.claudeSession.Events():
			if !ok {
				return
			}
			switch e := evt.(type) {
			case claude.TextEvent:
				r.eventHandler.OnText(e.Text)
			case claude.ThinkingEvent:
				r.eventHandler.OnThinking(e.Thinking)
			case claude.ToolStartEvent:
				r.eventHandler.OnToolStart(e.Name, e.ID, nil)
			case claude.ToolCompleteEvent:
				r.eventHandler.OnToolComplete(e.Name, e.ID, e.Input, nil, false)
			case claude.CLIToolResultEvent:
				r.eventHandler.OnToolComplete(e.ToolName, e.ToolUseID, nil, e.Content, e.IsError)
			case claude.TurnCompleteEvent:
				// TurnEnd is emitted synchronously by the manager after
				// RunTurn returns, so we skip it here to avoid duplicates.
				_ = e
			case claude.ErrorEvent:
				r.eventHandler.OnError(e.Error, "delegator")
			}
		}
	}
}
