package session

import (
	"context"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// DelegatorSystemPrompt is the system prompt used by the delegator agent.
// It is exported so that the CLI test harness and scenario tests can reference
// or override it.
const DelegatorSystemPrompt = `You are a delegator agent that orchestrates work by managing child sessions. You ONLY use three tools:

1. start_session — Start a planner (read-only analysis) or builder (code modification) session
2. stop_session — Stop a running session
3. get_session_progress — Check a session's progress, status, and recent output

These are your ONLY tools. You NEVER directly read files, write files, run commands, or make code changes. All work is done by child sessions that you start and monitor.

## How sessions work

Child sessions run asynchronously. After you start a session, it works in the background while you are idle. You will receive notifications when sessions change state (completed, failed, or need input). Do NOT poll get_session_progress in a loop waiting for a session to finish — instead, end your turn and wait for the notification.

Use get_session_progress to check details AFTER receiving a notification, or when the user asks about a session.

## Workflow

- For complex or multi-step tasks, start a planner session first to analyze the task and create a plan, then use builder sessions to implement.
- For simple, well-defined tasks (e.g. "create a hello world program"), skip planning and go straight to a builder session.
- After starting a session, tell the user what you started and end your turn. You will be notified when it completes.
- When you receive a notification, use get_session_progress to check the result, report it to the user, and decide on next steps.
- If a child session asks a question (status: waiting_for_input), relay the question to the user. When the user answers, start a new session or provide the answer as context.

## Error handling

- If a child session fails with a retriable error (transient API error, rate limit, lint failure fixable by retry), start a new session with the same or adjusted prompt.
- If a child session fails with a non-retriable error (context window exhausted, fundamental task issue), do NOT retry. Instead, explain the situation to the user and ask how to proceed.
- If the task is ambiguous or unclear, do NOT start any sessions. Ask the user for clarification first.

## Rules

- You orchestrate — you NEVER do work directly. You have exactly three tools; do not attempt to discover or use others.
- Do NOT poll get_session_progress repeatedly within a single turn. Call it once to check a session, then end your turn.
- Trust child session results. When get_session_progress reports a session as completed, trust that output.
- When writing prompts for child sessions, be specific and detailed about what the session should accomplish.`

// delegatorSystemPromptWithModels appends an "Available models" section to the
// base DelegatorSystemPrompt. If availableModels is empty, the base prompt is
// returned unchanged.
func delegatorSystemPromptWithModels(availableModels string) string {
	if availableModels == "" {
		return DelegatorSystemPrompt
	}
	return DelegatorSystemPrompt + "\n\n## Available models\n\nYou can use any of these models when starting child sessions:\n" + availableModels
}

// DelegatorAllowedTools is the allowlist of tools the delegator agent can use.
// This restricts the Claude session to only the SDK-provided delegator tools,
// preventing the model from directly using Read, Write, Bash, etc.
var DelegatorAllowedTools = []string{
	"mcp__delegator-tools__start_session",
	"mcp__delegator-tools__stop_session",
	"mcp__delegator-tools__get_session_progress",
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
	opts := []claude.SessionOption{
		claude.WithModel(r.model),
		claude.WithPermissionMode(claude.PermissionModePlan),
		claude.WithDangerouslySkipPermissions(),
		claude.WithSDKTools("delegator-tools", r.toolHandler.Registry()),
		claude.WithTools(""),
		claude.WithSystemPrompt(delegatorSystemPromptWithModels(r.toolHandler.AvailableModelsDescription())),
		claude.WithWorkDir(r.worktreePath),
		claude.WithDisablePlugins(),
		claude.WithEventBufferSize(1000),
	}
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
				childIDs := toolHandler.ChildIDs()
				isChild := false
				for _, id := range childIDs {
					if evt.SessionID == id {
						isChild = true
						break
					}
				}
				if !isChild {
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
				r.eventHandler.OnTurnComplete(e.TurnNumber, e.Success, e.DurationMs, e.Usage.CostUSD)
			case claude.ErrorEvent:
				r.eventHandler.OnError(e.Error, "delegator")
			}
		}
	}
}
