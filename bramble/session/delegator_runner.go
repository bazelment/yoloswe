package session

import (
	"context"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

const delegatorSystemPrompt = `You are a delegator agent that orchestrates work on a single git worktree branch.

You have three tools to manage child sessions:

1. start_session — Start a planner (read-only analysis) or builder (code modification) session
2. stop_session — Stop a running session
3. get_session_progress — Check a session's progress, status, and recent output

Your workflow:
- Start with a planner session to analyze the task and create a plan
- Use builder sessions to implement changes
- Monitor progress with get_session_progress
- If a child session fails with a retriable error (transient API error, lint failure fixable by retry), start a new session with the same or adjusted prompt
- If a child session needs genuine human input or the task is ambiguous, end your turn with a clear summary of the situation and a specific question for the user

Important:
- You do NOT make code changes yourself — you orchestrate child sessions that do the work
- Always check session progress before starting new sessions
- When a child session completes, check its output to verify the work was done correctly
- Keep the user informed of progress at natural milestones`

// delegatorRunner implements sessionRunner for delegator sessions.
// It wraps a Claude SDK session with tools for managing child sessions.
type delegatorRunner struct {
	claudeSession *claude.Session
	toolHandler   *DelegatorToolHandler
	eventHandler  *sessionEventHandler
	worktreePath  string
	model         string
}

func (r *delegatorRunner) Start(ctx context.Context) error {
	opts := []claude.SessionOption{
		claude.WithModel(r.model),
		claude.WithPermissionMode(claude.PermissionModePlan),
		claude.WithDangerouslySkipPermissions(),
		claude.WithSDKTools("delegator-tools", r.toolHandler.Registry()),
		claude.WithSystemPrompt(delegatorSystemPrompt),
		claude.WithWorkDir(r.worktreePath),
		claude.WithDisablePlugins(),
		claude.WithEventBufferSize(1000),
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
